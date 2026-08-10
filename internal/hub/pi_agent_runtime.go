package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yan5xu/codex-loom/internal/pi"
	"github.com/yan5xu/codex-loom/internal/rollout"
)

type piAgentRuntime struct {
	agentID string
	dataDir string

	mu               sync.Mutex
	rpc              *pi.RPC
	onEvent          func(RuntimeEvent)
	onFailure        func(error)
	developerContext string
	messageSequence  uint64
	currentMessage   uint64
	pendingTerminal  RuntimeEvent
	settled          chan struct{}
	abortRequested   bool
	imageInput       bool
}

func newPiAgentRuntime(agentID, dataDir string) *piAgentRuntime {
	return &piAgentRuntime{agentID: agentID, dataDir: dataDir}
}

func (r *piAgentRuntime) SetRuntimeEventHandlers(onEvent func(RuntimeEvent), onFailure func(error)) {
	r.mu.Lock()
	r.onEvent, r.onFailure = onEvent, onFailure
	r.mu.Unlock()
}

func (r *piAgentRuntime) Alive() bool {
	r.mu.Lock()
	rpc := r.rpc
	r.mu.Unlock()
	return rpc != nil && rpc.Alive()
}

func (r *piAgentRuntime) Create(request RuntimeBindingRequest) (string, error) {
	state, err := r.start(request, "")
	if err != nil {
		return "", err
	}
	if state.SessionFile == "" {
		return "", errors.New("Pi get_state returned no sessionFile")
	}
	return state.SessionFile, nil
}

func (r *piAgentRuntime) Resume(request RuntimeBindingRequest, timeout time.Duration) error {
	if r.Alive() {
		return nil
	}
	state, err := r.start(request, request.NativeRef)
	if err != nil {
		return err
	}
	if filepath.Clean(state.SessionFile) != filepath.Clean(request.NativeRef) {
		return fmt.Errorf("Pi resumed session %q, expected %q", state.SessionFile, request.NativeRef)
	}
	return nil
}

type piSessionState struct {
	SessionFile string `json:"sessionFile"`
	SessionID   string `json:"sessionId"`
	Model       *struct {
		Input []string `json:"input"`
	} `json:"model,omitempty"`
}

func (r *piAgentRuntime) start(request RuntimeBindingRequest, nativeRef string) (piSessionState, error) {
	sessionDir := filepath.Join(r.dataDir, "pi", r.agentID)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return piSessionState{}, fmt.Errorf("create Pi session directory: %w", err)
	}
	args := []string{"--session-dir", sessionDir, "--approve"}
	if nativeRef == "" {
		args = append(args, "--session-id", r.agentID)
		if request.Name != "" {
			args = append(args, "--name", request.Name)
		}
	} else {
		args = append(args, "--session", nativeRef)
	}
	rpc, err := pi.SpawnRPC(pi.RPCOptions{
		Cwd: request.Cwd, Args: args,
		OnEvent: r.handleEvent,
		OnFailure: func(err error) {
			r.mu.Lock()
			handler := r.onFailure
			r.mu.Unlock()
			if handler != nil {
				handler(err)
			}
		},
	})
	if err != nil {
		return piSessionState{}, err
	}
	r.mu.Lock()
	previous := r.rpc
	r.rpc = rpc
	r.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	response, err := rpc.Request(ctx, "get_state", nil)
	if err != nil {
		rpc.Close()
		return piSessionState{}, err
	}
	var state piSessionState
	if err := json.Unmarshal(response.Data, &state); err != nil || state.SessionFile == "" || state.SessionID == "" {
		rpc.Close()
		return piSessionState{}, fmt.Errorf("Pi get_state returned unsupported session state: %s", response.Data)
	}
	imageInput := false
	if state.Model != nil {
		for _, input := range state.Model.Input {
			if input == "image" {
				imageInput = true
				break
			}
		}
	}
	r.mu.Lock()
	r.imageInput = imageInput
	r.mu.Unlock()
	return state, nil
}

func (r *piAgentRuntime) InjectDeveloperContext(_ string, content string, _ time.Duration) error {
	r.mu.Lock()
	r.developerContext = strings.TrimSpace(content)
	r.mu.Unlock()
	return nil
}

func (r *piAgentRuntime) StartTurn(request RuntimeTurnRequest) (string, error) {
	parts := make([]string, 0, len(request.Input)+1)
	type piImage struct {
		Type     string `json:"type"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	}
	images := make([]piImage, 0)
	r.mu.Lock()
	if r.developerContext != "" {
		parts = append(parts, r.developerContext)
		r.developerContext = ""
	}
	rpc := r.rpc
	imageInput := r.imageInput
	r.pendingTerminal = RuntimeEvent{}
	r.currentMessage = 0
	r.settled = make(chan struct{})
	r.abortRequested = false
	r.mu.Unlock()
	for _, input := range request.Input {
		switch input.Kind {
		case RuntimeInputText:
			if text := strings.TrimSpace(input.Text); text != "" {
				parts = append(parts, text)
			}
		case RuntimeInputLocalImage:
			if !imageInput {
				return "", errors.New("Pi Runtime active model does not support image input")
			}
			if !strings.HasPrefix(strings.ToLower(input.MimeType), "image/") {
				return "", fmt.Errorf("Pi image input %q has invalid MIME type %q", input.Path, input.MimeType)
			}
			data, err := os.ReadFile(input.Path)
			if err != nil {
				return "", fmt.Errorf("read Pi image input %q: %w", input.Path, err)
			}
			images = append(images, piImage{Type: "image", Data: base64.StdEncoding.EncodeToString(data), MimeType: input.MimeType})
		default:
			return "", fmt.Errorf("Pi Runtime does not support input kind %q", input.Kind)
		}
	}
	if rpc == nil || !rpc.Alive() {
		return "", errors.New("Pi RPC process is unavailable")
	}
	message := strings.Join(parts, "\n\n")
	if message == "" {
		return "", errors.New("Pi prompt text is required")
	}
	entriesBefore, leafBefore, err := r.piSessionEntries(request.NativeRef)
	if err != nil {
		return "", fmt.Errorf("snapshot Pi prompt entry: %w", err)
	}
	previousUserEntryID, err := latestPiUserEntryID(entriesBefore, leafBefore)
	if err != nil {
		return "", fmt.Errorf("snapshot Pi prompt entry: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), request.Timeout)
	defer cancel()
	prompt := map[string]any{"message": message}
	if len(images) > 0 {
		prompt["images"] = images
	}
	if _, err := rpc.Request(ctx, "prompt", prompt); err != nil {
		return "", err
	}
	entries, leafID, err := r.piSessionEntries(request.NativeRef)
	if err != nil {
		return "", fmt.Errorf("read Pi prompt entry: %w", err)
	}
	nativeUserEntryID, err := latestPiUserEntryID(entries, leafID)
	if err != nil {
		return "", fmt.Errorf("read Pi prompt entry: %w", err)
	}
	if nativeUserEntryID == "" || nativeUserEntryID == previousUserEntryID {
		return "", errors.New("Pi accepted prompt without a new native user entry")
	}
	return nativeUserEntryID, nil
}

func (r *piAgentRuntime) Steer(string, string, string, time.Duration) (string, error) {
	return "", errors.New("Pi Runtime does not support causal steering")
}

func (r *piAgentRuntime) Interrupt(_ string, _ string, timeout time.Duration) error {
	r.mu.Lock()
	rpc := r.rpc
	settled := r.settled
	if rpc != nil && settled != nil {
		r.abortRequested = true
	}
	r.mu.Unlock()
	if rpc == nil {
		return errors.New("Pi RPC process is unavailable")
	}
	if settled == nil {
		return errors.New("Pi Turn is not active")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := rpc.Request(ctx, "abort", nil); err != nil {
		return err
	}
	select {
	case <-settled:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for Pi aborted Turn settlement: %w", ctx.Err())
	}
}

func (r *piAgentRuntime) NormalizeEvent(_ string, raw json.RawMessage) []RuntimeEvent {
	r.mu.Lock()
	events, _ := r.normalizeEventLocked(raw)
	r.mu.Unlock()
	return events
}

func (r *piAgentRuntime) handleEvent(raw json.RawMessage) {
	r.mu.Lock()
	events, terminal := r.normalizeEventLocked(raw)
	if terminal.Kind != "" {
		r.pendingTerminal = terminal
	}
	var typeOnly struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &typeOnly)
	var settled chan struct{}
	if typeOnly.Type == "agent_settled" {
		terminal = r.pendingTerminal
		r.pendingTerminal = RuntimeEvent{}
		if r.abortRequested && terminal.Kind != RuntimeTurnInterrupted {
			terminal = RuntimeEvent{Kind: RuntimeTurnFailed, Error: "Pi settled an aborted Turn without a final aborted assistant state"}
		} else if terminal.Kind == "" {
			terminal.Kind = RuntimeTurnCompleted
		}
		events = append(events, terminal)
		settled = r.settled
		r.settled = nil
		r.abortRequested = false
	}
	handler := r.onEvent
	r.mu.Unlock()
	if handler != nil {
		for _, event := range events {
			handler(event)
		}
	}
	if settled != nil {
		close(settled)
	}
}

func (r *piAgentRuntime) normalizeEventLocked(raw json.RawMessage) ([]RuntimeEvent, RuntimeEvent) {
	var envelope struct {
		Type    string `json:"type"`
		Message struct {
			Role         string `json:"role"`
			StopReason   string `json:"stopReason"`
			ErrorMessage string `json:"errorMessage"`
			Content      []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"content"`
		} `json:"message"`
		AssistantMessageEvent struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		} `json:"assistantMessageEvent"`
		ToolCallID    string         `json:"toolCallId"`
		ToolName      string         `json:"toolName"`
		Args          map[string]any `json:"args"`
		PartialResult map[string]any `json:"partialResult"`
		Result        map[string]any `json:"result"`
		IsError       bool           `json:"isError"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, RuntimeEvent{Kind: RuntimeTurnFailed, Error: "Pi emitted malformed event"}
	}
	switch envelope.Type {
	case "agent_start":
		return []RuntimeEvent{{Kind: RuntimeTurnStarted}}, RuntimeEvent{}
	case "message_start":
		if envelope.Message.Role == "assistant" {
			r.messageSequence++
			r.currentMessage = r.messageSequence
		}
		return nil, RuntimeEvent{}
	case "message_update":
		sequence := r.ensureCurrentMessageLocked()
		switch envelope.AssistantMessageEvent.Type {
		case "text_delta":
			return []RuntimeEvent{{Kind: RuntimeTextDelta, ItemID: piMessageItemID(sequence), Text: envelope.AssistantMessageEvent.Delta}}, RuntimeEvent{}
		case "thinking_delta":
			return []RuntimeEvent{{Kind: RuntimeReasoningDelta, ItemID: piReasoningItemID(sequence), Text: envelope.AssistantMessageEvent.Delta}}, RuntimeEvent{}
		default:
			return nil, RuntimeEvent{}
		}
	case "tool_execution_start", "tool_execution_update", "tool_execution_end":
		if envelope.ToolCallID == "" || envelope.ToolName == "" {
			return nil, RuntimeEvent{}
		}
		kind, status, result := RuntimeToolStarted, "running", map[string]any(nil)
		switch envelope.Type {
		case "tool_execution_update":
			kind, result = RuntimeToolUpdated, envelope.PartialResult
		case "tool_execution_end":
			kind, result = RuntimeToolCompleted, envelope.Result
			status = "completed"
			if envelope.IsError {
				status = "failed"
			}
		}
		output := piResultText(result)
		item := map[string]any{
			"id": envelope.ToolCallID, "type": "commandExecution", "command": piToolCommand(envelope.ToolName, envelope.Args),
			"toolName": envelope.ToolName, "args": envelope.Args, "status": status, "aggregatedOutput": output,
		}
		if envelope.IsError {
			item["error"] = output
		}
		return []RuntimeEvent{{Kind: kind, ItemID: envelope.ToolCallID, Item: item, Status: status, Error: func() string {
			if envelope.IsError {
				return output
			}
			return ""
		}()}}, RuntimeEvent{}
	case "message_end":
		if envelope.Message.Role != "assistant" {
			return nil, RuntimeEvent{}
		}
		sequence := r.ensureCurrentMessageLocked()
		r.currentMessage = 0
		var text, reasoning []string
		for _, content := range envelope.Message.Content {
			switch content.Type {
			case "text":
				text = append(text, content.Text)
			case "thinking":
				reasoning = append(reasoning, content.Thinking)
			}
		}
		answer := strings.Join(text, "")
		events := []RuntimeEvent{}
		if thought := strings.Join(reasoning, ""); thought != "" {
			itemID := piReasoningItemID(sequence)
			events = append(events, RuntimeEvent{
				Kind: RuntimeReasoningCompleted, ItemID: itemID, Text: thought,
				Item: map[string]any{"id": itemID, "type": "reasoning", "text": thought},
			})
		}
		if answer != "" {
			itemID := piMessageItemID(sequence)
			events = append(events, RuntimeEvent{
				Kind: RuntimeTextCompleted, ItemID: itemID, Text: answer,
				Item: map[string]any{"id": itemID, "type": "agentMessage", "text": answer},
			})
		}
		switch envelope.Message.StopReason {
		case "error":
			message := strings.TrimSpace(envelope.Message.ErrorMessage)
			if message == "" {
				message = "Pi assistant message ended with an error"
			}
			return events, RuntimeEvent{Kind: RuntimeTurnFailed, Error: message}
		case "aborted":
			return events, RuntimeEvent{Kind: RuntimeTurnInterrupted, Error: "Pi Turn aborted"}
		default:
			return events, RuntimeEvent{Kind: RuntimeTurnCompleted}
		}
	default:
		return nil, RuntimeEvent{}
	}
}

func (r *piAgentRuntime) ensureCurrentMessageLocked() uint64 {
	if r.currentMessage == 0 {
		r.messageSequence++
		r.currentMessage = r.messageSequence
	}
	return r.currentMessage
}

func piMessageItemID(sequence uint64) string { return fmt.Sprintf("pi-message-%d", sequence) }

func piReasoningItemID(sequence uint64) string { return fmt.Sprintf("pi-reasoning-%d", sequence) }

func piToolCommand(name string, args map[string]any) string {
	if name == "bash" {
		if command, _ := args["command"].(string); command != "" {
			return command
		}
	}
	raw, _ := json.Marshal(args)
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return name
	}
	return name + " " + string(raw)
}

func piResultText(result map[string]any) string {
	content, _ := result["content"].([]any)
	var parts []string
	for _, value := range content {
		item, _ := value.(map[string]any)
		if item["type"] == "text" {
			if text, _ := item["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "")
}

func (r *piAgentRuntime) ReadHistory(nativeRef string, count, offset int) (RuntimeHistory, error) {
	entries, leafID, err := r.piSessionEntries(nativeRef)
	if err != nil {
		return RuntimeHistory{}, err
	}
	history, err := projectPiHistory(entries, leafID)
	if err != nil {
		return RuntimeHistory{}, err
	}
	if count <= 0 {
		count = 10
	}
	if offset < 0 {
		offset = 0
	}
	end := history.Total - offset
	if end < 0 {
		end = 0
	}
	start := end - count
	if start < 0 {
		start = 0
	}
	history.Turns = history.Turns[start:end]
	return history, nil
}

func (r *piAgentRuntime) ReadTurn(nativeRef, nativeTurnID string) (RuntimeHistoryTurn, error) {
	entries, leafID, err := r.piSessionEntries(nativeRef)
	if err != nil {
		return RuntimeHistoryTurn{}, err
	}
	history, err := projectPiHistory(entries, leafID)
	if err != nil {
		return RuntimeHistoryTurn{}, err
	}
	for _, turn := range history.Turns {
		if turn.ID == nativeTurnID {
			return turn, nil
		}
	}
	return RuntimeHistoryTurn{}, fmt.Errorf("%w: %s", rollout.ErrTurnNotFound, nativeTurnID)
}

func (r *piAgentRuntime) LatestTurn(nativeRef string) (*RuntimeHistoryTurn, error) {
	history, err := r.ReadHistory(nativeRef, 1, 0)
	if err != nil || len(history.Turns) == 0 {
		return nil, err
	}
	turn := history.Turns[0]
	return &turn, nil
}

func (r *piAgentRuntime) Capabilities() RuntimeCapabilities {
	r.mu.Lock()
	imageInput := r.imageInput
	r.mu.Unlock()
	return RuntimeCapabilities{History: true, Interrupt: true, ImageInput: imageInput}
}

func (r *piAgentRuntime) Close() {
	r.mu.Lock()
	rpc := r.rpc
	r.rpc = nil
	r.mu.Unlock()
	if rpc != nil {
		rpc.Close()
	}
}
