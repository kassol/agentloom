package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type piSessionEntry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  string          `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

type piSessionMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	Model        string          `json:"model"`
	ToolCallID   string          `json:"toolCallId"`
	ToolName     string          `json:"toolName"`
	IsError      bool            `json:"isError"`
	Usage        struct {
		Input       int64 `json:"input"`
		Output      int64 `json:"output"`
		CacheRead   int64 `json:"cacheRead"`
		CacheWrite  int64 `json:"cacheWrite"`
		TotalTokens int64 `json:"totalTokens"`
	} `json:"usage"`
}

type piContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	Thinking  string         `json:"thinking"`
	Name      string         `json:"name"`
	ID        string         `json:"id"`
	Arguments map[string]any `json:"arguments"`
}

type piEntriesResponse struct {
	Entries []piSessionEntry `json:"entries"`
	LeafID  string           `json:"leafId"`
}

func (r *piAgentRuntime) piSessionEntries(nativeRef string) ([]piSessionEntry, string, error) {
	r.mu.Lock()
	rpc := r.rpc
	r.mu.Unlock()
	if rpc == nil || !rpc.Alive() {
		return readPiSessionEntries(nativeRef)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	response, err := rpc.Request(ctx, "get_entries", nil)
	if err != nil {
		return nil, "", err
	}
	var data piEntriesResponse
	if err := json.Unmarshal(response.Data, &data); err != nil {
		return nil, "", fmt.Errorf("Pi get_entries returned unsupported entries: %w", err)
	}
	if len(data.Entries) > 0 && data.LeafID == "" {
		return nil, "", errors.New("Pi get_entries returned entries without leafId")
	}
	return data.Entries, data.LeafID, nil
}

func readPiSessionEntries(path string) ([]piSessionEntry, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()

	entries := []piSessionEntry{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<26)
	line := 0
	for scanner.Scan() {
		line++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		var entry piSessionEntry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			return nil, "", fmt.Errorf("parse Pi session %s line %d: %w", path, line, err)
		}
		if entry.Type == "session" {
			continue
		}
		if entry.ID == "" {
			return nil, "", fmt.Errorf("parse Pi session %s line %d: entry has no id", path, line)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("read Pi session %s: %w", path, err)
	}
	leafID := ""
	if len(entries) > 0 {
		leafID = entries[len(entries)-1].ID
	}
	return entries, leafID, nil
}

func piActiveBranch(entries []piSessionEntry, leafID string) ([]piSessionEntry, error) {
	if leafID == "" {
		return nil, nil
	}
	byID := make(map[string]piSessionEntry, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	branch := make([]piSessionEntry, 0, len(entries))
	seen := map[string]bool{}
	for id := leafID; id != ""; {
		if seen[id] {
			return nil, fmt.Errorf("Pi session branch contains a cycle at %s", id)
		}
		seen[id] = true
		entry, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("Pi session branch references missing entry %s", id)
		}
		branch = append(branch, entry)
		id = entry.ParentID
	}
	for left, right := 0, len(branch)-1; left < right; left, right = left+1, right-1 {
		branch[left], branch[right] = branch[right], branch[left]
	}
	return branch, nil
}

func latestPiUserEntryID(entries []piSessionEntry, leafID string) (string, error) {
	branch, err := piActiveBranch(entries, leafID)
	if err != nil {
		return "", err
	}
	for index := len(branch) - 1; index >= 0; index-- {
		entry := branch[index]
		if entry.Type != "message" {
			continue
		}
		var message struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(entry.Message, &message) == nil && message.Role == "user" {
			return entry.ID, nil
		}
	}
	return "", nil
}

func (r *piAgentRuntime) InspectInterruptedTurn(nativeRef, nativeTurnID string) (RuntimeInterruptionEvidence, error) {
	entries, leafID, err := r.piSessionEntries(nativeRef)
	if err != nil {
		return RuntimeInterruptionEvidence{}, err
	}
	return inspectPiInterruptedTurn(entries, leafID, nativeTurnID)
}

func inspectPiInterruptedTurn(entries []piSessionEntry, leafID, nativeTurnID string) (RuntimeInterruptionEvidence, error) {
	branch, err := piActiveBranch(entries, leafID)
	if err != nil {
		return RuntimeInterruptionEvidence{}, err
	}
	evidence := RuntimeInterruptionEvidence{Status: RuntimeInterruptionClean, LeafEntryID: leafID, UnfinishedTools: []RuntimeToolEvidence{}}
	start := -1
	for index, entry := range branch {
		if entry.ID != nativeTurnID {
			continue
		}
		if entry.Type != "message" {
			return RuntimeInterruptionEvidence{}, fmt.Errorf("Pi native Turn %s is not a message entry", nativeTurnID)
		}
		var message piSessionMessage
		if err := json.Unmarshal(entry.Message, &message); err != nil || message.Role != "user" {
			return RuntimeInterruptionEvidence{}, fmt.Errorf("Pi native Turn %s is not a user message", nativeTurnID)
		}
		start = index
		break
	}
	if start < 0 {
		return RuntimeInterruptionEvidence{}, fmt.Errorf("Pi active branch does not contain native Turn %s", nativeTurnID)
	}

	pending := map[string]RuntimeToolEvidence{}
	order := []string{}
	terminalStatus := ""
	for _, entry := range branch[start+1:] {
		if entry.Type != "message" {
			continue
		}
		var message piSessionMessage
		if err := json.Unmarshal(entry.Message, &message); err != nil {
			return RuntimeInterruptionEvidence{}, fmt.Errorf("parse Pi message entry %s: %w", entry.ID, err)
		}
		if message.Role == "user" {
			terminalStatus = "completed"
			break
		}
		switch message.Role {
		case "assistant":
			for _, block := range piMessageContent(message.Content) {
				if block.Type != "toolCall" {
					continue
				}
				if block.ID == "" || block.Name == "" {
					return RuntimeInterruptionEvidence{}, fmt.Errorf("Pi assistant entry %s has malformed toolCall evidence", entry.ID)
				}
				if _, exists := pending[block.ID]; !exists {
					order = append(order, block.ID)
				}
				pending[block.ID] = RuntimeToolEvidence{
					ID: block.ID, Name: block.Name, Command: piToolCommand(block.Name, block.Arguments),
					Arguments: block.Arguments, StartedAt: entry.Timestamp,
				}
			}
			switch message.StopReason {
			case "stop", "length":
				terminalStatus = "completed"
			case "error":
				terminalStatus = "failed"
			case "aborted":
				terminalStatus = "interrupted"
			}
		case "toolResult":
			if message.ToolCallID == "" {
				return RuntimeInterruptionEvidence{}, fmt.Errorf("Pi tool result entry %s has no toolCallId", entry.ID)
			}
			if _, exists := pending[message.ToolCallID]; !exists {
				return RuntimeInterruptionEvidence{}, fmt.Errorf("Pi tool result entry %s has no matching toolCall", entry.ID)
			}
			delete(pending, message.ToolCallID)
		}
	}
	for _, id := range order {
		if tool, exists := pending[id]; exists {
			evidence.UnfinishedTools = append(evidence.UnfinishedTools, tool)
		}
	}
	if len(evidence.UnfinishedTools) > 0 {
		evidence.Status = RuntimeInterruptionAmbiguous
		return evidence, nil
	}
	if terminalStatus != "" {
		evidence.Status = RuntimeInterruptionTerminal
		evidence.TerminalStatus = terminalStatus
	}
	return evidence, nil
}

func projectPiHistory(entries []piSessionEntry, leafID string) (RuntimeHistory, error) {
	branch, err := piActiveBranch(entries, leafID)
	if err != nil {
		return RuntimeHistory{}, err
	}
	history := RuntimeHistory{}
	var turn *RuntimeHistoryTurn
	commands := map[string]map[string]any{}
	for _, entry := range branch {
		if entry.Type != "message" {
			continue
		}
		var message piSessionMessage
		if err := json.Unmarshal(entry.Message, &message); err != nil {
			return RuntimeHistory{}, fmt.Errorf("parse Pi message entry %s: %w", entry.ID, err)
		}
		blocks := piMessageContent(message.Content)
		switch message.Role {
		case "user":
			if turn != nil && turn.Status == "running" {
				turn.Status = "completed"
			}
			visibleText := piVisibleUserText(piContentText(blocks))
			history.Turns = append(history.Turns, RuntimeHistoryTurn{
				ID: entry.ID, Status: "running", StartedAt: entry.Timestamp, UpdatedAt: entry.Timestamp,
				Task: visibleText, Items: []map[string]any{{"type": "user", "text": visibleText, "timestamp": entry.Timestamp}},
			})
			turn = &history.Turns[len(history.Turns)-1]
			commands = map[string]map[string]any{}
		case "assistant":
			if turn == nil {
				continue
			}
			for _, block := range blocks {
				switch block.Type {
				case "thinking":
					if block.Thinking != "" {
						turn.Items = append(turn.Items, map[string]any{"type": "thinking", "text": block.Thinking, "timestamp": entry.Timestamp})
					}
				case "text":
					if block.Text != "" {
						turn.Items = append(turn.Items, map[string]any{"type": "answer", "text": block.Text, "timestamp": entry.Timestamp})
					}
				case "toolCall":
					item := map[string]any{"type": "command", "command": piToolCommand(block.Name, block.Arguments), "status": "running", "timestamp": entry.Timestamp}
					turn.Items = append(turn.Items, item)
					commands[block.ID] = item
				}
			}
			turn.Model = message.Model
			turn.UpdatedAt = entry.Timestamp
			turn.Usage = addPiUsage(turn.Usage, message)
			switch message.StopReason {
			case "stop", "length":
				turn.Status, turn.CompletedAt = "completed", entry.Timestamp
			case "error":
				turn.Status, turn.CompletedAt = "failed", entry.Timestamp
			case "aborted":
				turn.Status, turn.CompletedAt = "interrupted", entry.Timestamp
			}
		case "toolResult":
			if turn == nil {
				continue
			}
			output := piContentText(blocks)
			item := commands[message.ToolCallID]
			if item == nil {
				item = map[string]any{"type": "command", "command": message.ToolName, "timestamp": entry.Timestamp}
				turn.Items = append(turn.Items, item)
			}
			item["status"] = "completed"
			if message.IsError {
				item["status"] = "failed"
			}
			item["output"] = output
			turn.UpdatedAt = entry.Timestamp
		}
	}
	history.Total = len(history.Turns)
	return history, nil
}

func piMessageContent(raw json.RawMessage) []piContentBlock {
	var blocks []piContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		return blocks
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && text != "" {
		return []piContentBlock{{Type: "text", Text: text}}
	}
	return nil
}

func piContentText(blocks []piContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "")
}

func piVisibleUserText(text string) string {
	text = strings.TrimSpace(text)
	for _, envelope := range []struct {
		open, close string
	}{
		{"<loom_developer_context", "</loom_developer_context>"},
		{"<loom_context", "</loom_context>"},
	} {
		if !strings.HasPrefix(text, envelope.open) {
			continue
		}
		if end := strings.Index(text, envelope.close); end >= 0 {
			text = strings.TrimSpace(text[end+len(envelope.close):])
		}
	}
	const loomContextOpen = "<loom_context"
	if start := strings.LastIndex(text, loomContextOpen); start >= 0 {
		context := text[start:]
		if strings.HasSuffix(strings.TrimSpace(context), "</loom_context>") {
			text = strings.TrimSpace(text[:start])
		}
	}
	return text
}

func addPiUsage(current *RuntimeTokenUsage, message piSessionMessage) *RuntimeTokenUsage {
	usage := message.Usage
	if usage.Input == 0 && usage.Output == 0 && usage.CacheRead == 0 && usage.TotalTokens == 0 {
		return current
	}
	if current == nil {
		current = &RuntimeTokenUsage{}
	}
	current.InputTokens += usage.Input
	current.CachedInputTokens += usage.CacheRead
	current.OutputTokens += usage.Output
	current.TotalTokens += usage.TotalTokens
	current.Calls++
	return current
}
