package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/codex"
	"github.com/yan5xu/codex-loom/internal/rollout"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

type codexAgentRuntime struct {
	client *codex.Client
}

func (r *codexAgentRuntime) Alive() bool { return r != nil && r.client != nil && !r.client.Closed() }

func (r *codexAgentRuntime) Create(request nativeBindingRequest) (string, error) {
	result, err := r.client.Request("thread/start", threadBindingParams(request.Sandbox, request.Cwd, request.ProviderID, request.Model, request.DisabledSkillPaths), 30*time.Second)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil || parsed.Thread.ID == "" {
		return "", fmt.Errorf("thread/start returned no thread id")
	}
	return parsed.Thread.ID, nil
}

func (r *codexAgentRuntime) Resume(request nativeBindingRequest, timeout time.Duration) error {
	params := threadBindingParams(request.Sandbox, request.Cwd, request.ProviderID, request.Model, request.DisabledSkillPaths)
	params["threadId"] = request.NativeRef
	_, err := r.client.Request("thread/resume", params, timeout)
	return err
}

func (r *codexAgentRuntime) Resources(ctx context.Context, cwd string) (runtimecontract.ResourceInventory, error) {
	if r == nil || r.client == nil {
		return runtimecontract.ResourceInventory{}, errors.New("Codex Runtime is not running")
	}
	params := map[string]any{"forceReload": true}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		params["cwds"] = []string{cwd}
	}
	raw, err := r.client.Request("skills/list", params, contractTimeout(ctx, 30*time.Second))
	if err != nil {
		return runtimecontract.ResourceInventory{}, err
	}
	var native struct {
		Data []SkillInventoryEntry `json:"data"`
	}
	if err := json.Unmarshal(raw, &native); err != nil {
		return runtimecontract.ResourceInventory{}, fmt.Errorf("decode Codex skills/list: %w", err)
	}
	resources := make([]runtimecontract.Resource, 0)
	for _, entry := range native.Data {
		if cwd != "" && filepath.Clean(entry.Cwd) != filepath.Clean(cwd) {
			continue
		}
		for _, skill := range entry.Skills {
			if strings.TrimSpace(skill.Name) == "" || strings.TrimSpace(skill.Path) == "" {
				continue
			}
			resources = append(resources, runtimecontract.Resource{
				ID: "skill:" + skill.Path, Name: skill.Name, Description: skill.Description,
				Kind: runtimecontract.ResourceSkill, Path: skill.Path, Scope: skill.Scope, Source: "codex", Enabled: skill.Enabled,
			})
		}
	}
	encoded, _ := json.Marshal(resources)
	return runtimecontract.ResourceInventory{
		Revision:  "codex:" + sha256Hex(encoded)[:16],
		Semantics: "Codex-native Skills; paths and enablement follow Codex discovery and SessionFlags policy",
		Resources: resources,
	}, nil
}

func (r *codexAgentRuntime) InjectDeveloperContext(nativeRef, content string, timeout time.Duration) error {
	_, err := r.client.Request("thread/inject_items", map[string]any{
		"threadId": nativeRef,
		"items": []map[string]any{{
			"type": "message", "role": "developer",
			"content": []map[string]any{{"type": "input_text", "text": content}},
		}},
	}, timeout)
	return err
}

func (r *codexAgentRuntime) StartTurn(request nativeTurnRequest) (string, error) {
	input := make([]map[string]any, 0, len(request.Input))
	for _, item := range request.Input {
		switch item.Kind {
		case nativeInputText:
			input = append(input, map[string]any{"type": "text", "text": item.Text})
		case nativeInputLocalImage:
			input = append(input, map[string]any{"type": "localImage", "path": item.Path})
		}
	}
	params := map[string]any{
		"threadId": request.NativeRef, "input": input,
		"approvalPolicy": request.ApprovalPolicy, "sandboxPolicy": codexSandboxPolicy(request.Sandbox),
	}
	if request.Model != "" {
		params["model"] = request.Model
	}
	if request.Effort != "" {
		params["effort"] = request.Effort
	}
	result, err := r.client.Request("turn/start", params, request.Timeout)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
		TurnID string `json:"turnId"`
		ID     string `json:"id"`
	}
	_ = json.Unmarshal(result, &parsed)
	for _, id := range []string{parsed.Turn.ID, parsed.TurnID, parsed.ID} {
		if id != "" {
			return id, nil
		}
	}
	return "", nil
}

func (r *codexAgentRuntime) Steer(nativeRef, expectedNativeTurnID, input string, timeout time.Duration) (string, error) {
	result, err := r.client.Request("turn/steer", map[string]any{
		"threadId": nativeRef, "expectedTurnId": expectedNativeTurnID,
		"input": []map[string]any{{"type": "text", "text": input}},
	}, timeout)
	if err != nil {
		return "", err
	}
	var response struct {
		TurnID string `json:"turnId"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return "", fmt.Errorf("decode turn/steer response: %w", err)
	}
	if response.TurnID == "" {
		return "", errors.New("turn/steer returned no turnId")
	}
	if response.TurnID != expectedNativeTurnID {
		return "", fmt.Errorf("turn/steer accepted %s, expected %s", response.TurnID, expectedNativeTurnID)
	}
	return response.TurnID, nil
}

func (r *codexAgentRuntime) Interrupt(nativeRef, nativeTurnID string, timeout time.Duration) error {
	_, err := r.client.Request("turn/interrupt", map[string]any{"threadId": nativeRef, "turnId": nativeTurnID}, timeout)
	return err
}

func (r *codexAgentRuntime) NormalizeEvent(method string, params json.RawMessage) []nativeEvent {
	var envelope struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		ItemID   string `json:"itemId"`
		Delta    string `json:"delta"`
		Thread   struct {
			ID string `json:"id"`
		} `json:"thread"`
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
		Item  map[string]any `json:"item"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(params, &envelope) != nil {
		return nil
	}
	nativeRef := strings.TrimSpace(envelope.ThreadID)
	if nativeRef == "" {
		nativeRef = strings.TrimSpace(envelope.Thread.ID)
	}
	nativeTurnID := strings.TrimSpace(envelope.TurnID)
	if nativeTurnID == "" {
		nativeTurnID = strings.TrimSpace(envelope.Turn.ID)
	}
	event := nativeEvent{NativeRef: nativeRef, NativeTurnID: nativeTurnID, ItemID: envelope.ItemID, Text: envelope.Delta, Item: envelope.Item, Status: envelope.Turn.Status}
	switch method {
	case "turn/started":
		event.Kind = nativeTurnStarted
	case "item/agentMessage/delta":
		event.Kind = nativeTextDelta
	case "item/reasoning/delta":
		event.Kind = nativeReasoningDelta
	case "item/started", "item/updated", "item/completed":
		itemType, _ := envelope.Item["type"].(string)
		if id, _ := envelope.Item["id"].(string); event.ItemID == "" {
			event.ItemID = id
		}
		switch itemType {
		case "userMessage":
			event.Kind = nativeUserInput
			event.Text = notificationUserText(params)
		case "agentMessage":
			if method != "item/completed" {
				return nil
			}
			event.Kind = nativeTextCompleted
			event.Text = completedFinalAnswer(method, params)
		case "reasoning":
			if method != "item/completed" {
				return nil
			}
			event.Kind = nativeReasoningCompleted
		default:
			switch method {
			case "item/started":
				event.Kind = nativeToolStarted
			case "item/updated":
				event.Kind = nativeToolUpdated
			case "item/completed":
				event.Kind = nativeToolCompleted
			}
		}
	case "turn/completed", "turn/failed", "turn/aborted":
		event.Kind = nativeTurnCompleted
		if envelope.Turn.Error != nil {
			event.Error = envelope.Turn.Error.Message
		} else if envelope.Error != nil {
			event.Error = envelope.Error.Message
		}
		switch {
		case method == "turn/failed", envelope.Turn.Status == "failed":
			event.Kind = nativeTurnFailed
		case method == "turn/aborted", envelope.Turn.Status == "interrupted", envelope.Turn.Status == "aborted", envelope.Turn.Status == "cancelled", envelope.Turn.Status == "canceled":
			event.Kind = nativeTurnInterrupted
		}
	default:
		return nil
	}
	return []nativeEvent{event}
}

func (r *codexAgentRuntime) ReadHistory(nativeRef string, count, offset int) (nativeHistory, error) {
	window, total, err := rollout.ReadWindow(nativeRef, count, offset)
	if err != nil {
		return nativeHistory{}, err
	}
	usageByTurn := map[string]rollout.TurnUsage{}
	if report, usageErr := rollout.ReadUsage(nativeRef); usageErr == nil {
		for _, turn := range report.Turns {
			usageByTurn[turn.TurnID] = turn
		}
	}
	result := nativeHistory{Total: total}
	for _, turn := range window.Turns {
		item := nativeHistoryTurn{ID: turn.ID, Status: turn.Status, Items: turn.Items}
		if usage, ok := usageByTurn[turn.ID]; ok {
			item.Model = usage.Model
			item.Usage = runtimeTokenUsage(usage.Usage)
			item.UsageUpdatedAt = usage.LastUpdatedAt
		}
		result.Turns = append(result.Turns, item)
	}
	return result, nil
}

func (r *codexAgentRuntime) ReadTurn(nativeRef, nativeTurnID string) (nativeHistoryTurn, error) {
	turn, err := rollout.ReadTurn(nativeRef, nativeTurnID)
	if err != nil {
		return nativeHistoryTurn{}, err
	}
	result := nativeHistoryTurn{ID: turn.ID, Status: turn.Status, Items: turn.Items}
	if report, usageErr := rollout.ReadUsage(nativeRef); usageErr == nil {
		for _, activity := range report.Activity {
			if activity.TurnID == nativeTurnID {
				result.StartedAt, result.CompletedAt = activity.StartedAt, activity.EndedAt
				if result.Status == "running" && activity.Status != "running" {
					result.Status = activity.Status
				}
				break
			}
		}
		for _, usage := range report.Turns {
			if usage.TurnID == nativeTurnID {
				result.Model = usage.Model
				result.Usage = runtimeTokenUsage(usage.Usage)
				result.UsageUpdatedAt = usage.LastUpdatedAt
				break
			}
		}
	}
	if result.StartedAt == "" && len(result.Items) > 0 {
		result.StartedAt, _ = result.Items[0]["timestamp"].(string)
	}
	if result.CompletedAt == "" && result.Status != "running" && len(result.Items) > 0 {
		result.CompletedAt, _ = result.Items[len(result.Items)-1]["timestamp"].(string)
	}
	return result, nil
}

func (r *codexAgentRuntime) LatestTurn(nativeRef string) (*nativeHistoryTurn, error) {
	turn, err := rollout.LatestTurn(nativeRef)
	if err != nil || turn == nil {
		return nil, err
	}
	return &nativeHistoryTurn{ID: turn.ID, Status: turn.Status, UpdatedAt: turn.UpdatedAt, Task: turn.Task}, nil
}

// Close releases per-binding resources. Codex bindings share one host-owned
// app-server client, so the binding itself has nothing to close.
func (r *codexAgentRuntime) Close() {}

func runtimeTokenUsage(usage rollout.TokenUsage) *nativeTokenUsage {
	return &nativeTokenUsage{
		InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens,
		OutputTokens: usage.OutputTokens, ReasoningOutputTokens: usage.ReasoningOutputTokens,
		TotalTokens: usage.TotalTokens, Calls: usage.Calls,
	}
}
