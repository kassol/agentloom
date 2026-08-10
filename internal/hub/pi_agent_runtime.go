package hub

import (
	"context"
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
	pendingTerminal  RuntimeEvent
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
	r.mu.Lock()
	if r.developerContext != "" {
		parts = append(parts, r.developerContext)
		r.developerContext = ""
	}
	rpc := r.rpc
	r.pendingTerminal = RuntimeEvent{}
	r.mu.Unlock()
	for _, input := range request.Input {
		if input.Kind != RuntimeInputText {
			return "", fmt.Errorf("Pi core Turn supports direct text only")
		}
		if text := strings.TrimSpace(input.Text); text != "" {
			parts = append(parts, text)
		}
	}
	if rpc == nil || !rpc.Alive() {
		return "", errors.New("Pi RPC process is unavailable")
	}
	message := strings.Join(parts, "\n\n")
	if message == "" {
		return "", errors.New("Pi prompt text is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), request.Timeout)
	defer cancel()
	_, err := rpc.Request(ctx, "prompt", map[string]any{"message": message})
	return "", err
}

func (r *piAgentRuntime) Steer(string, string, string, time.Duration) (string, error) {
	return "", errors.New("Pi Runtime does not support causal steering")
}

func (r *piAgentRuntime) Interrupt(string, string, time.Duration) error {
	r.mu.Lock()
	rpc := r.rpc
	r.mu.Unlock()
	if rpc == nil {
		return errors.New("Pi RPC process is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := rpc.Request(ctx, "abort", nil)
	return err
}

func (r *piAgentRuntime) NormalizeEvent(_ string, raw json.RawMessage) []RuntimeEvent {
	events, _ := normalizePiEvent(raw, 0)
	return events
}

func (r *piAgentRuntime) handleEvent(raw json.RawMessage) {
	r.mu.Lock()
	r.messageSequence++
	sequence := r.messageSequence
	r.mu.Unlock()
	events, terminal := normalizePiEvent(raw, sequence)
	if terminal.Kind != "" {
		r.mu.Lock()
		r.pendingTerminal = terminal
		r.mu.Unlock()
	}
	var typeOnly struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &typeOnly)
	if typeOnly.Type == "agent_settled" {
		r.mu.Lock()
		settled := r.pendingTerminal
		r.pendingTerminal = RuntimeEvent{}
		r.mu.Unlock()
		if settled.Kind == "" {
			settled.Kind = RuntimeTurnCompleted
		}
		events = append(events, settled)
	}
	r.mu.Lock()
	handler := r.onEvent
	r.mu.Unlock()
	if handler != nil {
		for _, event := range events {
			handler(event)
		}
	}
}

func normalizePiEvent(raw json.RawMessage, sequence uint64) ([]RuntimeEvent, RuntimeEvent) {
	var envelope struct {
		Type    string `json:"type"`
		Message struct {
			Role         string `json:"role"`
			StopReason   string `json:"stopReason"`
			ErrorMessage string `json:"errorMessage"`
			Content      []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, RuntimeEvent{Kind: RuntimeTurnFailed, Error: "Pi emitted malformed event"}
	}
	switch envelope.Type {
	case "agent_start":
		return []RuntimeEvent{{Kind: RuntimeTurnStarted}}, RuntimeEvent{}
	case "message_end":
		if envelope.Message.Role != "assistant" {
			return nil, RuntimeEvent{}
		}
		var text []string
		for _, content := range envelope.Message.Content {
			if content.Type == "text" && content.Text != "" {
				text = append(text, content.Text)
			}
		}
		answer := strings.Join(text, "")
		itemID := fmt.Sprintf("pi-message-%d", sequence)
		events := []RuntimeEvent{}
		if answer != "" {
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
			return events, RuntimeEvent{}
		}
	default:
		return nil, RuntimeEvent{}
	}
}

func (r *piAgentRuntime) ReadHistory(string, int, int) (RuntimeHistory, error) {
	return RuntimeHistory{}, nil
}

func (r *piAgentRuntime) ReadTurn(string, string) (RuntimeHistoryTurn, error) {
	return RuntimeHistoryTurn{}, rollout.ErrTurnNotFound
}

func (r *piAgentRuntime) LatestTurn(string) (*RuntimeHistoryTurn, error) { return nil, nil }

func (r *piAgentRuntime) Capabilities() RuntimeCapabilities {
	return RuntimeCapabilities{Interrupt: true}
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
