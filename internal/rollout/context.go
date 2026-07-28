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
	EpochID               string          `json:"epochId"`
	WindowNumber          int             `json:"windowNumber,omitempty"`
	CompactedAt           string          `json:"compactedAt,omitempty"`
	DeliveriesPersisted   bool            `json:"deliveriesPersisted"`
	PersistedDeliveryKeys map[string]bool `json:"persistedDeliveryKeys,omitempty"`
}

// ContextHistory scans only canonical rollout records. A compaction marker
// starts a new epoch and invalidates matches found in an earlier window.
func ContextHistory(threadID string, query ContextHistoryQuery) (ContextHistoryState, error) {
	state := ContextHistoryState{EpochID: "initial:" + strings.TrimSpace(threadID)}
	state.PersistedDeliveryKeys = make(map[string]bool, len(query.Deliveries))
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
			}
		case "response_item":
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

func responseItemContainsContext(payload json.RawMessage, turnID string, delivery ContextDeliveryProbe) bool {
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
	if json.Unmarshal(payload, &message) != nil || message.Type != "message" ||
		message.Role != strings.TrimSpace(delivery.Role) {
		return false
	}
	if message.Role == "user" && strings.TrimSpace(turnID) != "" &&
		message.Metadata.TurnID != strings.TrimSpace(turnID) {
		return false
	}
	for _, content := range message.Content {
		if (content.Type == "input_text" || content.Type == "text") &&
			strings.Contains(content.Text, delivery.Marker) &&
			sha256Hex(content.Text) == delivery.Hash {
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
