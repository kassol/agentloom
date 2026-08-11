package rollout

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

type ContextDeliveryProbe struct {
	Role   string `json:"role"`
	Marker string `json:"marker"`
	Hash   string `json:"hash"`
}

type ContextHistoryQuery struct {
	TurnID     string                 `json:"turnId,omitempty"`
	Deliveries []ContextDeliveryProbe `json:"deliveries,omitempty"`
}

// ContextHistoryState identifies the latest replayable context window and
// whether every requested Loom context delivery is present in that window.
type ContextHistoryState struct {
	EpochID               string                `json:"epochId"`
	WindowNumber          int                   `json:"windowNumber,omitempty"`
	CompactedAt           string                `json:"compactedAt,omitempty"`
	TurnObserved          bool                  `json:"turnObserved,omitempty"`
	TurnEpochID           string                `json:"turnEpochId,omitempty"`
	TurnWindowNumber      int                   `json:"turnWindowNumber,omitempty"`
	TurnCompactedAt       string                `json:"turnCompactedAt,omitempty"`
	TurnDeliveries        []ContextTurnDelivery `json:"turnDeliveries,omitempty"`
	DeliveriesPersisted   bool                  `json:"deliveriesPersisted"`
	PersistedDeliveryKeys map[string]bool       `json:"persistedDeliveryKeys,omitempty"`
}

type ContextTurnDelivery struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ContextHistory scans only canonical rollout records. A compaction marker
// starts a new epoch and invalidates matches found in an earlier window.
func ContextHistory(threadID string, query ContextHistoryQuery) (ContextHistoryState, error) {
	state := ContextHistoryState{EpochID: "initial:" + strings.TrimSpace(threadID)}
	state.PersistedDeliveryKeys = make(map[string]bool, len(query.Deliveries))
	pendingDeveloper := []ContextTurnDelivery{}
	path, err := FindRollout(threadID)
	if err != nil {
		if errors.Is(err, ErrRolloutNotFound) {
			return state, nil
		}
		return state, err
	}
	file, err := os.Open(path)
	if err != nil {
		return state, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<26)
	for scanner.Scan() {
		var record line
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		switch record.Type {
		case "compacted":
			var payload struct {
				WindowID     string `json:"window_id"`
				WindowNumber int    `json:"window_number"`
			}
			if json.Unmarshal(record.Payload, &payload) == nil {
				if strings.TrimSpace(payload.WindowID) != "" {
					state.EpochID = "window:" + strings.TrimSpace(payload.WindowID)
				} else {
					state.EpochID = "compacted:" + record.Timestamp
				}
				state.WindowNumber = payload.WindowNumber
				state.CompactedAt = record.Timestamp
				clear(state.PersistedDeliveryKeys)
				pendingDeveloper = pendingDeveloper[:0]
			}
		case "response_item":
			message, validMessage := contextResponseMessage(record.Payload)
			if validMessage {
				switch message.Role {
				case "developer":
					for _, content := range message.Content {
						if containsLoomRoot(content, "loom_developer_context") {
							pendingDeveloper = append(pendingDeveloper, ContextTurnDelivery{Role: "developer", Content: content})
						}
					}
				case "user":
					if strings.TrimSpace(query.TurnID) != "" && message.TurnID == strings.TrimSpace(query.TurnID) {
						if !state.TurnObserved {
							state.TurnObserved = true
							state.TurnEpochID = state.EpochID
							state.TurnWindowNumber = state.WindowNumber
							state.TurnCompactedAt = state.CompactedAt
						}
						state.TurnDeliveries = append(state.TurnDeliveries, pendingDeveloper...)
						for _, content := range message.Content {
							if containsLoomRoot(content, "loom_context") {
								state.TurnDeliveries = append(state.TurnDeliveries, ContextTurnDelivery{Role: "user", Content: content})
							}
						}
					}
					pendingDeveloper = pendingDeveloper[:0]
				}
			}
			for _, delivery := range query.Deliveries {
				key := contextDeliveryProbeKey(delivery)
				if !state.PersistedDeliveryKeys[key] &&
					responseItemContainsContext(record.Payload, query.TurnID, delivery) {
					state.PersistedDeliveryKeys[key] = true
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return state, err
	}
	state.DeliveriesPersisted = len(query.Deliveries) > 0
	for _, delivery := range query.Deliveries {
		if !state.PersistedDeliveryKeys[contextDeliveryProbeKey(delivery)] {
			state.DeliveriesPersisted = false
			break
		}
	}
	return state, nil
}

type contextMessage struct {
	Role    string
	TurnID  string
	Content []string
}

func contextResponseMessage(payload json.RawMessage) (contextMessage, bool) {
	var message struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Metadata struct {
			TurnID string `json:"turn_id"`
		} `json:"internal_chat_message_metadata_passthrough"`
	}
	if json.Unmarshal(payload, &message) != nil || message.Type != "message" {
		return contextMessage{}, false
	}
	result := contextMessage{Role: strings.TrimSpace(message.Role), TurnID: strings.TrimSpace(message.Metadata.TurnID)}
	for _, content := range message.Content {
		if content.Type == "input_text" || content.Type == "text" {
			result.Content = append(result.Content, content.Text)
		}
	}
	return result, true
}

func containsLoomRoot(content, root string) bool {
	content = strings.TrimSpace(content)
	return strings.HasPrefix(content, "<"+root) && (strings.HasSuffix(content, "</"+root+">") || strings.HasSuffix(content, "/>"))
}

func responseItemContainsContext(payload json.RawMessage, turnID string, delivery ContextDeliveryProbe) bool {
	message, ok := contextResponseMessage(payload)
	if !ok || message.Role != strings.TrimSpace(delivery.Role) {
		return false
	}
	if message.Role == "user" && strings.TrimSpace(turnID) != "" &&
		message.TurnID != strings.TrimSpace(turnID) {
		return false
	}
	for _, content := range message.Content {
		if strings.Contains(content, delivery.Marker) && sha256Hex(content) == delivery.Hash {
			return true
		}
	}
	return false
}

func contextDeliveryProbeKey(delivery ContextDeliveryProbe) string {
	return strings.TrimSpace(delivery.Role) + ":" + strings.TrimSpace(delivery.Marker)
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
