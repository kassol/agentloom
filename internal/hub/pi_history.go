package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

type piSessionEntry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  string          `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
	Usage     *piNativeUsage  `json:"usage"`
}

type piNativeUsageCost struct {
	Total *float64 `json:"total"`
}

type piNativeUsage struct {
	Input       int64              `json:"input"`
	Output      int64              `json:"output"`
	CacheRead   int64              `json:"cacheRead"`
	CacheWrite  int64              `json:"cacheWrite"`
	TotalTokens *int64             `json:"totalTokens"`
	Reasoning   *int64             `json:"reasoning"`
	Cost        *piNativeUsageCost `json:"cost"`
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
	Provider     string          `json:"provider"`
	Usage        piNativeUsage   `json:"usage"`
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
	reader := bufio.NewReader(file)
	line := 0
	for {
		lineRaw, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, "", fmt.Errorf("read Pi session %s: %w", path, readErr)
		}
		if lineRaw == "" && errors.Is(readErr, io.EOF) {
			break
		}
		line++
		raw := strings.TrimSpace(lineRaw)
		if raw == "" {
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}
		var entry piSessionEntry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			if errors.Is(readErr, io.EOF) {
				break // an append interrupted mid-record is not consumed work yet
			}
			return nil, "", fmt.Errorf("parse Pi session %s line %d: %w", path, line, err)
		}
		if entry.Type == "session" {
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}
		if entry.ID == "" {
			return nil, "", fmt.Errorf("parse Pi session %s line %d: entry has no id", path, line)
		}
		entries = append(entries, entry)
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	leafID := ""
	if len(entries) > 0 {
		leafID = entries[len(entries)-1].ID
	}
	return entries, leafID, nil
}

type piUsageEnvelope struct {
	Role       string         `json:"role"`
	Provider   string         `json:"provider"`
	Model      string         `json:"model"`
	StopReason string         `json:"stopReason"`
	Usage      *piNativeUsage `json:"usage"`
}

func (c *piRuntimeContract) InspectUsage(ctx context.Context, binding runtimecontract.Binding) (runtimecontract.UsageReport, *runtimecontract.Failure) {
	if err := ctx.Err(); err != nil {
		return runtimecontract.UsageReport{}, piUsageFailure(err)
	}
	entries, _, err := readPiSessionEntries(binding.NativeRef)
	if err != nil {
		return runtimecontract.UsageReport{}, piUsageFailure(err)
	}
	return projectPiUsage(entries), nil
}

func piUsageFailure(err error) *runtimecontract.Failure {
	return &runtimecontract.Failure{Code: "usage_inspection_failed", Phase: runtimecontract.FailurePhaseUsageInspection, Message: "Pi usage could not be inspected", Diagnostic: err.Error(), Cause: err}
}

func projectPiUsage(entries []piSessionEntry) runtimecontract.UsageReport {
	const source = "pi_session"
	unavailableText := runtimecontract.UsageText{Source: "runtime_unavailable"}
	unavailableMetric := runtimecontract.UsageMetric{Source: "runtime_unavailable"}
	unavailableUsage := runtimecontract.Usage{
		InputTokens: unavailableMetric, CachedInputTokens: unavailableMetric, OutputTokens: unavailableMetric,
		ReasoningOutputTokens: unavailableMetric, TotalTokens: unavailableMetric, Calls: unavailableMetric, CostMicros: unavailableMetric,
	}
	report := runtimecontract.UsageReport{
		Events: []runtimecontract.UsageEvent{}, Turns: []runtimecontract.TurnUsage{}, Activity: []runtimecontract.TurnActivity{},
		LatestProvider: unavailableText, LatestModel: unavailableText,
		ContextInputTokens: unavailableMetric, ModelContextWindow: unavailableMetric,
		Lifetime: unavailableUsage, LatestCall: unavailableUsage, LastUpdatedAt: unavailableText,
	}
	byID := make(map[string]piSessionEntry, len(entries))
	envelopes := make(map[string]piUsageEnvelope, len(entries))
	roles := make(map[string]string, len(entries))
	activities := make(map[string]*runtimecontract.TurnActivity, len(entries))
	activityOrder := []string{}
	for _, entry := range entries {
		if _, exists := byID[entry.ID]; exists {
			continue
		}
		byID[entry.ID] = entry
		var envelope piUsageEnvelope
		if len(entry.Message) > 0 && json.Unmarshal(entry.Message, &envelope) != nil {
			continue
		}
		if envelope.Usage == nil {
			envelope.Usage = entry.Usage
		}
		envelopes[entry.ID] = envelope
		roles[entry.ID] = envelope.Role
		if envelope.Role == "user" {
			activities[entry.ID] = &runtimecontract.TurnActivity{
				TurnID: observedUsageText(entry.ID, source), StartedAt: observedUsageText(entry.Timestamp, source),
				EndedAt: unavailableText, Status: observedUsageText("running", source),
				InferredEnd: runtimecontract.UsageBool{Available: true, Source: source},
			}
			activityOrder = append(activityOrder, entry.ID)
		}
	}
	seen := map[string]bool{}
	turnStatuses := map[string]struct {
		status string
		ended  runtimecontract.UsageText
	}{}
	for _, entry := range entries {
		if seen[entry.ID] {
			continue
		}
		seen[entry.ID] = true
		envelope := envelopes[entry.ID]
		turnID := piUsageTurnID(entry, byID, roles)
		if status := piUsageStatus(envelope.StopReason); status != "" {
			current := turnStatuses[turnID]
			if piUsageStatusRank(status) >= piUsageStatusRank(current.status) {
				turnStatuses[turnID] = struct {
					status string
					ended  runtimecontract.UsageText
				}{status: status, ended: observedUsageText(entry.Timestamp, source)}
			}
		}
		if envelope.Usage == nil {
			continue
		}
		usage := piRuntimeUsage(*envelope.Usage)
		report.Lifetime.Add(usage)
		report.Events = append(report.Events, runtimecontract.UsageEvent{
			Timestamp: observedUsageText(entry.Timestamp, source), TurnID: observedUsageText(turnID, source),
			Provider: observedUsageText(envelope.Provider, source), Model: observedUsageText(envelope.Model, source), Usage: usage,
		})
	}
	sort.SliceStable(report.Events, func(i, j int) bool {
		return report.Events[i].Timestamp.Value < report.Events[j].Timestamp.Value
	})
	turns := map[string]*runtimecontract.TurnUsage{}
	turnOrder := []string{}
	for _, event := range report.Events {
		id := event.TurnID.Value
		turn := turns[id]
		if turn == nil {
			turn = &runtimecontract.TurnUsage{TurnID: event.TurnID, Provider: unavailableText}
			turns[id] = turn
			turnOrder = append(turnOrder, id)
			if activities[id] == nil {
				entry := byID[id]
				activities[id] = &runtimecontract.TurnActivity{
					TurnID: event.TurnID, StartedAt: observedUsageText(entry.Timestamp, source), EndedAt: unavailableText,
					Status: observedUsageText("running", source), InferredEnd: runtimecontract.UsageBool{Available: true, Source: source},
				}
				activityOrder = append(activityOrder, id)
			}
		}
		turn.Usage.Add(event.Usage)
		if event.Provider.Available {
			turn.Provider = event.Provider
		}
		if event.Model.Available {
			turn.Model = event.Model
		}
		turn.LastUpdatedAt = event.Timestamp
	}
	for _, id := range turnOrder {
		report.Turns = append(report.Turns, *turns[id])
	}
	for _, id := range activityOrder {
		if terminal := turnStatuses[id]; terminal.status != "" {
			activities[id].Status = observedUsageText(terminal.status, source)
			activities[id].EndedAt = terminal.ended
		}
		report.Activity = append(report.Activity, *activities[id])
	}
	if len(report.Events) > 0 {
		latest := report.Events[len(report.Events)-1]
		report.LatestCall, report.LatestProvider, report.LatestModel, report.LastUpdatedAt = latest.Usage, latest.Provider, latest.Model, latest.Timestamp
	}
	return report
}

func piUsageStatus(stopReason string) string {
	switch stopReason {
	case "stop", "length":
		return "completed"
	case "aborted":
		return "interrupted"
	case "error":
		return "failed"
	default:
		return ""
	}
}

func piUsageStatusRank(status string) int {
	switch status {
	case "completed":
		return 3
	case "failed":
		return 2
	case "interrupted":
		return 1
	default:
		return 0
	}
}

func piUsageTurnID(entry piSessionEntry, byID map[string]piSessionEntry, roles map[string]string) string {
	seen := map[string]bool{}
	for current := entry; current.ID != "" && !seen[current.ID]; current = byID[current.ParentID] {
		seen[current.ID] = true
		if roles[current.ID] == "user" {
			return current.ID
		}
		if current.ParentID == "" {
			break
		}
	}
	return entry.ID
}

func piRuntimeUsage(value piNativeUsage) runtimecontract.Usage {
	observed := func(amount int64) runtimecontract.UsageMetric {
		return runtimecontract.UsageMetric{Available: true, Value: amount, Source: "pi_session_usage"}
	}
	unavailable := runtimecontract.UsageMetric{Source: "runtime_unavailable"}
	result := runtimecontract.Usage{
		InputTokens: observed(value.Input + value.CacheRead + value.CacheWrite), CachedInputTokens: observed(value.CacheRead),
		OutputTokens: observed(value.Output), TotalTokens: observed(piNativeTotalTokens(value)),
		ReasoningOutputTokens: unavailable, Calls: unavailable, CostMicros: unavailable,
	}
	if value.Reasoning != nil {
		result.ReasoningOutputTokens = observed(*value.Reasoning)
	}
	if value.Cost != nil && value.Cost.Total != nil {
		result.CostMicros = observed(piNativeCostMicros(*value.Cost.Total))
	}
	return result
}

func piNativeTotalTokens(value piNativeUsage) int64 {
	if value.TotalTokens != nil {
		return *value.TotalTokens
	}
	return value.Input + value.Output + value.CacheRead + value.CacheWrite
}

func piNativeCostMicros(value float64) int64 {
	return int64(math.Round(value * 1_000_000))
}

func observedUsageText(value, source string) runtimecontract.UsageText {
	if value == "" {
		return runtimecontract.UsageText{Source: "runtime_unavailable"}
	}
	return runtimecontract.UsageText{Available: true, Value: value, Source: source}
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
					StartedAt: entry.Timestamp,
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

func projectPiHistory(entries []piSessionEntry, leafID string) (nativeHistory, error) {
	branch, err := piActiveBranch(entries, leafID)
	if err != nil {
		return nativeHistory{}, err
	}
	history := nativeHistory{}
	var turn *nativeHistoryTurn
	commands := map[string]map[string]any{}
	for _, entry := range branch {
		if entry.Type != "message" {
			continue
		}
		var message piSessionMessage
		if err := json.Unmarshal(entry.Message, &message); err != nil {
			return nativeHistory{}, fmt.Errorf("parse Pi message entry %s: %w", entry.ID, err)
		}
		blocks := piMessageContent(message.Content)
		switch message.Role {
		case "user":
			userText := piContentText(blocks)
			visibleText := piVisibleUserText(userText)
			item := map[string]any{"type": "user", "text": visibleText, "timestamp": entry.Timestamp}
			if attachments := piUserAttachments(userText); len(attachments) > 0 {
				item["attachments"] = attachments
			}
			if turn != nil && turn.Status == "running" && piCausalAgentMessage(userText) {
				turn.Items = append(turn.Items, item)
				turn.UpdatedAt = entry.Timestamp
				continue
			}
			if turn != nil && turn.Status == "running" {
				turn.Status = "completed"
			}
			history.Turns = append(history.Turns, nativeHistoryTurn{
				ID: entry.ID, Status: "running", StartedAt: entry.Timestamp, UpdatedAt: entry.Timestamp,
				Task: visibleText, Items: []map[string]any{item},
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

func projectPiCanonicalHistory(entries []piSessionEntry, leafID string) (runtimecontract.History, error) {
	branch, err := piActiveBranch(entries, leafID)
	if err != nil {
		return runtimecontract.History{}, err
	}
	history := runtimecontract.History{}
	var turn *runtimecontract.HistoryTurn
	for _, entry := range branch {
		if entry.Type != "message" {
			continue
		}
		var message piSessionMessage
		if err := json.Unmarshal(entry.Message, &message); err != nil {
			return runtimecontract.History{}, fmt.Errorf("parse Pi message entry %s: %w", entry.ID, err)
		}
		blocks := piMessageContent(message.Content)
		switch message.Role {
		case "user":
			userText := piContentText(blocks)
			visibleText := piVisibleUserText(userText)
			content := []runtimecontract.ContentBlock{{ID: piCanonicalContentID(entry.ID, "user"), Kind: runtimecontract.ContentUserText, Text: visibleText}}
			for index, attachment := range piUserCanonicalAttachments(userText) {
				blockID := piCanonicalContentID(entry.ID, fmt.Sprintf("attachment-%d", index+1))
				if strings.HasPrefix(strings.ToLower(attachment.MIMEType), "image/") {
					image := runtimecontract.Image(attachment)
					content = append(content, runtimecontract.ContentBlock{ID: blockID, Kind: runtimecontract.ContentImage, Image: &image})
				} else {
					copy := attachment
					content = append(content, runtimecontract.ContentBlock{ID: blockID, Kind: runtimecontract.ContentAttachment, Attachment: &copy})
				}
			}
			if turn != nil && turn.State == runtimecontract.LifecycleAccepted && piCausalAgentMessage(userText) {
				turn.Content = append(turn.Content, content...)
				continue
			}
			if turn != nil && turn.State == runtimecontract.LifecycleAccepted {
				turn.State = runtimecontract.LifecycleCompleted
			}
			history.Turns = append(history.Turns, runtimecontract.HistoryTurn{
				RuntimeTurnRef: entry.ID, State: runtimecontract.LifecycleAccepted, Content: content, StartedAt: entry.Timestamp,
			})
			turn = &history.Turns[len(history.Turns)-1]
		case "assistant":
			if turn == nil {
				continue
			}
			for index, block := range blocks {
				blockID := piCanonicalContentID(entry.ID, block.ID, fmt.Sprint(index+1))
				switch block.Type {
				case "thinking":
					if block.Thinking != "" {
						turn.Content = append(turn.Content, runtimecontract.ContentBlock{ID: blockID, Kind: runtimecontract.ContentReasoning, Text: block.Thinking})
					}
				case "text":
					if block.Text != "" {
						turn.Content = append(turn.Content, runtimecontract.ContentBlock{ID: blockID, Kind: runtimecontract.ContentAssistantText, Text: block.Text})
					}
				case "toolCall":
					arguments, _ := json.Marshal(block.Arguments)
					turn.Content = append(turn.Content, runtimecontract.ContentBlock{ID: piCanonicalContentID("tool-call", block.ID), Kind: runtimecontract.ContentToolCall, ToolCall: &runtimecontract.ToolCall{Name: block.Name, Arguments: arguments}})
				}
			}
			turn.Usage = addPiContractUsage(turn.Usage, message)
			switch message.StopReason {
			case "stop", "length":
				turn.State, turn.CompletedAt = runtimecontract.LifecycleCompleted, entry.Timestamp
			case "error":
				turn.State, turn.CompletedAt = runtimecontract.LifecycleFailed, entry.Timestamp
			case "aborted":
				turn.State, turn.CompletedAt = runtimecontract.LifecycleInterrupted, entry.Timestamp
			}
		case "toolResult":
			if turn == nil {
				continue
			}
			toolCallID := piCanonicalContentID("tool-call", message.ToolCallID)
			if message.ToolCallID == "" {
				toolCallID = piCanonicalContentID(entry.ID, "tool")
			}
			turn.Content = append(turn.Content, runtimecontract.ContentBlock{
				ID: piCanonicalContentID(entry.ID, "result"), Kind: runtimecontract.ContentToolResult,
				ToolResult: &runtimecontract.ToolResult{ToolCallID: toolCallID, Text: piContentText(blocks), Success: !message.IsError},
			})
		}
	}
	history.Total = len(history.Turns)
	return history, nil
}

func piCanonicalContentID(parts ...string) string {
	return "content_" + sha256Hex([]byte(strings.Join(parts, "\x00")))[:16]
}

func piUserCanonicalAttachments(text string) []runtimecontract.Attachment {
	raw := piUserAttachments(text)
	result := make([]runtimecontract.Attachment, 0, len(raw))
	for _, source := range raw {
		id, _ := source["id"].(string)
		name, _ := source["name"].(string)
		mimeType, _ := source["mimeType"].(string)
		ref, _ := source["url"].(string)
		if id != "" {
			ref = "artifact:" + id
		}
		if ref == "" {
			continue
		}
		size, _ := source["size"].(int64)
		result = append(result, runtimecontract.Attachment{ID: id, Name: name, MIMEType: mimeType, Size: size, Ref: ref})
	}
	return result
}

func addPiContractUsage(current *runtimecontract.Usage, message piSessionMessage) *runtimecontract.Usage {
	usage := message.Usage
	if !piNativeUsagePresent(usage) {
		return current
	}
	if current == nil {
		current = &runtimecontract.Usage{}
	}
	add := func(metric *runtimecontract.UsageMetric, value int64) {
		metric.Available, metric.Value, metric.Source = true, metric.Value+value, "native"
	}
	add(&current.InputTokens, usage.Input+usage.CacheRead+usage.CacheWrite)
	add(&current.CachedInputTokens, usage.CacheRead)
	add(&current.OutputTokens, usage.Output)
	add(&current.TotalTokens, piNativeTotalTokens(usage))
	if usage.Reasoning != nil {
		add(&current.ReasoningOutputTokens, *usage.Reasoning)
	} else if current.ReasoningOutputTokens.Source == "" {
		current.ReasoningOutputTokens.Source = "runtime_unavailable"
	}
	if usage.Cost != nil && usage.Cost.Total != nil {
		add(&current.CostMicros, piNativeCostMicros(*usage.Cost.Total))
	} else if current.CostMicros.Source == "" {
		current.CostMicros.Source = "runtime_unavailable"
	}
	current.Calls.Source = "runtime_unavailable"
	return current
}

func piNativeUsagePresent(usage piNativeUsage) bool {
	return usage.Input != 0 || usage.Output != 0 || usage.CacheRead != 0 || usage.CacheWrite != 0 ||
		usage.TotalTokens != nil || usage.Reasoning != nil || usage.Cost != nil
}

func piCausalAgentMessage(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "<agent_message ") && strings.HasSuffix(text, "</agent_message>")
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

func piUserAttachments(text string) []map[string]any {
	start := strings.LastIndex(text, "<loom_context")
	if start < 0 {
		return nil
	}
	const closeTag = "</loom_context>"
	end := strings.Index(text[start:], closeTag)
	if end < 0 {
		return nil
	}
	end += start + len(closeTag)
	var context struct {
		Turn struct {
			Attachments []struct {
				ID       string `xml:"id,attr"`
				Name     string `xml:"name,attr"`
				MimeType string `xml:"mime_type,attr"`
				Size     int64  `xml:"size,attr"`
				Path     string `xml:"path,attr"`
				URL      string `xml:"url,attr"`
			} `xml:"attachments>attachment"`
		} `xml:"loom_turn_context"`
	}
	if xml.Unmarshal([]byte(text[start:end]), &context) != nil {
		return nil
	}
	attachments := make([]map[string]any, 0, len(context.Turn.Attachments))
	for _, attachment := range context.Turn.Attachments {
		attachments = append(attachments, map[string]any{
			"id": attachment.ID, "name": attachment.Name, "mimeType": attachment.MimeType,
			"size": attachment.Size, "path": attachment.Path, "url": attachment.URL,
		})
	}
	return attachments
}

func addPiUsage(current *nativeTokenUsage, message piSessionMessage) *nativeTokenUsage {
	usage := message.Usage
	if !piNativeUsagePresent(usage) {
		return current
	}
	if current == nil {
		current = &nativeTokenUsage{}
	}
	current.InputTokens += usage.Input + usage.CacheRead + usage.CacheWrite
	current.CachedInputTokens += usage.CacheRead
	current.OutputTokens += usage.Output
	if usage.Reasoning != nil {
		current.ReasoningOutputTokens += *usage.Reasoning
	}
	current.TotalTokens += piNativeTotalTokens(usage)
	current.Calls++
	return current
}
