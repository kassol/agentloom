package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yan5xu/codex-loom/internal/codex"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

// codexRuntimeContract translates Runtime Contract v2 values around the
// existing Codex-native primitive. Provider/model and policy fields remain in
// the v1 compatibility facade until their capability tickets migrate them.
type codexRuntimeContract struct {
	agentID string
	native  *codexAgentRuntime

	mu             sync.Mutex
	bindingRequest RuntimeBindingRequest
	turnRequest    RuntimeTurnRequest
	handler        func(runtimecontract.Event)
	release        func()
	pendingTurn    runtimeTurnCorrelation
	turnsByNative  map[string]runtimeTurnCorrelation
}

type runtimeTurnCorrelation struct {
	turnID            string
	predecessorTurnID string
}

func (c *codexRuntimeContract) ContractVersion() int { return runtimecontract.Version }

func (c *codexRuntimeContract) setCompatibilityBinding(request RuntimeBindingRequest) {
	c.mu.Lock()
	c.bindingRequest = request
	c.mu.Unlock()
}

func (c *codexRuntimeContract) setCompatibilityTurn(request RuntimeTurnRequest) {
	c.mu.Lock()
	c.turnRequest = request
	c.mu.Unlock()
}

func (c *codexRuntimeContract) bindingControls() RuntimeBindingRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bindingRequest
}

func (c *codexRuntimeContract) turnControls() RuntimeTurnRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turnRequest
}

func (c *codexRuntimeContract) CreateBinding(ctx context.Context, request runtimecontract.BindingRequest) (runtimecontract.Binding, runtimecontract.Outcome) {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseBindingCreate); failure != nil {
		return runtimecontract.Binding{}, *failure
	}
	controls := c.bindingControls()
	controls.Name, controls.Cwd = request.Name, request.Cwd
	nativeRef, err := c.native.Create(controls)
	if err != nil {
		return runtimecontract.Binding{}, codexFailureOutcome(err, runtimecontract.FailurePhaseBindingCreate)
	}
	return runtimecontract.Binding{
		SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "codex", NativeRef: nativeRef,
	}, runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}
}

func (c *codexRuntimeContract) ResumeBinding(ctx context.Context, binding runtimecontract.Binding) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseBindingResume); failure != nil {
		return *failure
	}
	request := c.bindingControls()
	request.NativeRef = binding.NativeRef
	if err := c.native.Resume(request, contractTimeout(ctx, 60*time.Second)); err != nil {
		return codexFailureOutcome(err, runtimecontract.FailurePhaseBindingResume)
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}

func (c *codexRuntimeContract) StartTurn(ctx context.Context, request runtimecontract.TurnRequest) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseTurnStart); failure != nil {
		return *failure
	}
	controls := c.turnControls()
	c.setPendingTurn(request.TurnID, "")
	controls.NativeRef = request.Binding.NativeRef
	controls.Input = contractInputToV1(request.Input)
	controls.Timeout = contractTimeout(ctx, 4*time.Hour)
	nativeTurnID, err := c.native.StartTurn(controls)
	if err != nil {
		return codexFailureOutcome(err, runtimecontract.FailurePhaseTurnStart)
	}
	c.bindTurn(request.TurnID, "", nativeTurnID)
	return runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: nativeTurnID}
}

func (c *codexRuntimeContract) ContinueTurn(ctx context.Context, request runtimecontract.CausalInput) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseTurnContinue); failure != nil {
		return *failure
	}
	text := make([]string, 0, len(request.Input))
	for _, input := range request.Input {
		if input.Kind == runtimecontract.InputText && input.Text != "" {
			text = append(text, input.Text)
		}
	}
	c.setPendingTurn(request.TurnID, request.PredecessorTurnID)
	nativeTurnID, err := c.native.Steer(request.Binding.NativeRef, request.RuntimeTurnRef, strings.Join(text, "\n"), contractTimeout(ctx, 30*time.Second))
	if err != nil {
		return codexFailureOutcome(err, runtimecontract.FailurePhaseTurnContinue)
	}
	c.bindTurn(request.TurnID, request.PredecessorTurnID, nativeTurnID)
	return runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: nativeTurnID}
}

func (c *codexRuntimeContract) InterruptTurn(ctx context.Context, request runtimecontract.TurnTarget) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseTurnInterrupt); failure != nil {
		return *failure
	}
	if strings.TrimSpace(request.TurnID) == "" {
		return runtimecontract.Outcome{State: runtimecontract.LifecycleRejected, Failure: &runtimecontract.Failure{
			Code: "missing_turn_id", Phase: runtimecontract.FailurePhaseTurnInterrupt, Message: "canonical Loom Turn ID is required",
		}}
	}
	if err := c.native.Interrupt(request.Binding.NativeRef, request.RuntimeTurnRef, contractTimeout(ctx, 10*time.Second)); err != nil {
		return codexFailureOutcome(err, runtimecontract.FailurePhaseTurnInterrupt)
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleInterrupted, RuntimeTurnRef: request.RuntimeTurnRef}
}

func (c *codexRuntimeContract) SetEventHandler(handler func(runtimecontract.Event)) {
	c.mu.Lock()
	c.handler = handler
	c.mu.Unlock()
}

func (c *codexRuntimeContract) handleNativeEvent(method string, params json.RawMessage) int {
	events := c.native.NormalizeEvent(method, params)
	for _, event := range events {
		correlation := c.correlationForEvent(event)
		canonical := runtimeContractEvent(event, correlation)
		c.mu.Lock()
		handler := c.handler
		c.mu.Unlock()
		if handler != nil {
			handler(canonical)
		}
	}
	return len(events)
}

func (c *codexRuntimeContract) setPendingTurn(turnID, predecessorTurnID string) {
	c.mu.Lock()
	c.pendingTurn = runtimeTurnCorrelation{turnID: turnID, predecessorTurnID: predecessorTurnID}
	c.mu.Unlock()
}

func (c *codexRuntimeContract) bindTurn(turnID, predecessorTurnID, nativeTurnID string) {
	turnID = strings.TrimSpace(turnID)
	nativeTurnID = strings.TrimSpace(nativeTurnID)
	if turnID == "" || nativeTurnID == "" {
		return
	}
	c.mu.Lock()
	if c.turnsByNative == nil {
		c.turnsByNative = map[string]runtimeTurnCorrelation{}
	}
	c.turnsByNative[nativeTurnID] = runtimeTurnCorrelation{turnID: turnID, predecessorTurnID: predecessorTurnID}
	if c.pendingTurn.turnID == turnID {
		c.pendingTurn = runtimeTurnCorrelation{}
	}
	c.mu.Unlock()
}

func (c *codexRuntimeContract) correlationForEvent(event RuntimeEvent) runtimeTurnCorrelation {
	c.mu.Lock()
	defer c.mu.Unlock()
	correlation := c.turnsByNative[event.NativeTurnID]
	if correlation.turnID == "" {
		correlation = c.pendingTurn
		if correlation.turnID != "" && event.NativeTurnID != "" {
			if c.turnsByNative == nil {
				c.turnsByNative = map[string]runtimeTurnCorrelation{}
			}
			c.turnsByNative[event.NativeTurnID] = correlation
		}
	}
	return correlation
}

func (c *codexRuntimeContract) ReadHistory(ctx context.Context, request runtimecontract.HistoryRequest) (runtimecontract.History, *runtimecontract.Failure) {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseHistory); failure != nil {
		return runtimecontract.History{}, failure.Failure
	}
	native, err := c.native.ReadHistory(request.Binding.NativeRef, request.Count, request.Offset)
	if err != nil {
		outcome := codexFailureOutcome(err, runtimecontract.FailurePhaseHistory)
		return runtimecontract.History{}, outcome.Failure
	}
	history := runtimecontract.History{Total: native.Total}
	for _, turn := range native.Turns {
		diagnostic, _ := json.Marshal(turn)
		history.Turns = append(history.Turns, runtimecontract.HistoryTurn{
			TurnID:         c.turnIDForNative(turn.ID),
			RuntimeTurnRef: turn.ID,
			State:          codexHistoryState(turn.Status),
			Content:        codexHistoryContent(turn.Items),
			Usage:          codexContractUsage(turn.Usage),
			StartedAt:      turn.StartedAt,
			CompletedAt:    turn.CompletedAt,
			Diagnostic:     diagnostic,
		})
	}
	return history, nil
}

func (c *codexRuntimeContract) CapabilitySnapshot(context.Context, runtimecontract.Binding) runtimecontract.CapabilitySnapshot {
	return runtimecontract.CapabilitySnapshot{}
}

func (c *codexRuntimeContract) CloseBinding(context.Context, runtimecontract.Binding) runtimecontract.Outcome {
	if c.release != nil {
		c.release()
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}

func contextFailure(ctx context.Context, phase runtimecontract.FailurePhase) *runtimecontract.Outcome {
	if err := ctx.Err(); err != nil {
		outcome := runtimecontract.Outcome{State: runtimecontract.LifecycleRejected, Failure: &runtimecontract.Failure{
			Code: "context_done", Phase: phase, Message: err.Error(), Diagnostic: err.Error(),
		}}
		return &outcome
	}
	return nil
}

func codexFailureOutcome(err error, phase runtimecontract.FailurePhase) runtimecontract.Outcome {
	state, code := runtimecontract.LifecycleFailed, "runtime_error"
	if phase == runtimecontract.FailurePhaseTurnStart || phase == runtimecontract.FailurePhaseTurnContinue || phase == runtimecontract.FailurePhaseTurnInterrupt {
		switch {
		case codex.IsRequestTimeout(err):
			state, code = runtimecontract.LifecycleIndeterminate, "transport_timeout"
		case errors.Is(err, codex.ErrClosed):
			state, code = runtimecontract.LifecycleIndeterminate, "transport_closed"
		}
	}
	return runtimecontract.Outcome{State: state, Failure: &runtimecontract.Failure{
		Code: code, Phase: phase, Message: err.Error(), Diagnostic: err.Error(), Cause: err,
	}}
}

func contractTimeout(ctx context.Context, fallback time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < fallback {
			return remaining
		}
	}
	return fallback
}

func contractInputToV1(input []runtimecontract.InputBlock) []RuntimeInput {
	result := make([]RuntimeInput, 0, len(input))
	for _, block := range input {
		switch block.Kind {
		case runtimecontract.InputText:
			result = append(result, RuntimeInput{Kind: RuntimeInputText, Text: block.Text})
		case runtimecontract.InputImage:
			result = append(result, RuntimeInput{Kind: RuntimeInputLocalImage, Path: block.Ref, MimeType: block.MIMEType})
		}
	}
	return result
}

func codexHistoryState(status string) runtimecontract.LifecycleState {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed":
		return runtimecontract.LifecycleCompleted
	case "interrupted", "aborted", "cancelled", "canceled":
		return runtimecontract.LifecycleInterrupted
	case "failed":
		return runtimecontract.LifecycleFailed
	default:
		return runtimecontract.LifecycleAccepted
	}
}

func codexHistoryContent(items []map[string]any) []runtimecontract.ContentBlock {
	content := make([]runtimecontract.ContentBlock, 0, len(items)+1)
	for index, item := range items {
		itemType, _ := item["type"].(string)
		id, _ := item["id"].(string)
		if id == "" {
			id = fmt.Sprintf("content-%d", index+1)
		}
		text := codexHistoryText(item)
		block := runtimecontract.ContentBlock{ID: id, Text: text}
		switch strings.ToLower(itemType) {
		case "usermessage", "user_message", "input_text", "user":
			block.Kind = runtimecontract.ContentUserText
		case "agentmessage", "assistantmessage", "agent_message", "assistant_text", "answer":
			block.Kind = runtimecontract.ContentAssistantText
		case "reasoning", "thinking":
			block.Kind = runtimecontract.ContentReasoning
		case "command", "commandexecution":
			arguments, _ := json.Marshal(map[string]any{"command": item["command"], "cwd": item["cwd"]})
			block.Kind = runtimecontract.ContentToolCall
			block.Text = ""
			block.ToolCall = &runtimecontract.ToolCall{Name: "exec_command", Arguments: arguments}
			content = append(content, block, runtimecontract.ContentBlock{
				ID: id + "-result", Kind: runtimecontract.ContentToolResult,
				ToolResult: codexToolResult(id, item),
			})
			continue
		default:
			arguments, _ := json.Marshal(item)
			block.Kind = runtimecontract.ContentToolCall
			block.Text = ""
			block.ToolCall = &runtimecontract.ToolCall{Name: itemType, Arguments: arguments}
		}
		content = append(content, block)
	}
	return content
}

func codexToolResult(toolCallID string, item map[string]any) *runtimecontract.ToolResult {
	text := ""
	for _, key := range []string{"aggregatedOutput", "output", "text"} {
		if value, _ := item[key].(string); value != "" {
			text = value
			break
		}
	}
	success := true
	if status, _ := item["status"].(string); strings.EqualFold(status, "failed") || strings.EqualFold(status, "error") {
		success = false
	}
	switch exitCode := item["exitCode"].(type) {
	case float64:
		success = success && exitCode == 0
	case int:
		success = success && exitCode == 0
	case int64:
		success = success && exitCode == 0
	}
	return &runtimecontract.ToolResult{ToolCallID: toolCallID, Text: text, Success: success}
}

func codexHistoryText(item map[string]any) string {
	for _, key := range []string{"text", "message"} {
		if text, _ := item[key].(string); text != "" {
			return text
		}
	}
	parts, _ := item["content"].([]any)
	var text []string
	for _, part := range parts {
		if value, ok := part.(map[string]any); ok {
			if piece, _ := value["text"].(string); piece != "" {
				text = append(text, piece)
			}
		}
	}
	return strings.Join(text, "")
}

func codexContractUsage(usage *RuntimeTokenUsage) *runtimecontract.Usage {
	if usage == nil {
		return nil
	}
	metric := func(value int64) runtimecontract.UsageMetric {
		return runtimecontract.UsageMetric{Available: true, Value: value, Source: "native"}
	}
	return &runtimecontract.Usage{
		InputTokens: metric(usage.InputTokens), CachedInputTokens: metric(usage.CachedInputTokens),
		OutputTokens: metric(usage.OutputTokens), ReasoningOutputTokens: metric(usage.ReasoningOutputTokens),
		TotalTokens: metric(usage.TotalTokens), Calls: metric(usage.Calls),
		CostMicros: runtimecontract.UsageMetric{Available: false, Source: "runtime_unavailable"},
	}
}

func runtimeContractEvent(event RuntimeEvent, correlation runtimeTurnCorrelation) runtimecontract.Event {
	canonical := runtimecontract.Event{
		TurnID: correlation.turnID, PredecessorTurnID: correlation.predecessorTurnID, RuntimeTurnRef: event.NativeTurnID,
	}
	switch event.Kind {
	case RuntimeTurnStarted:
		canonical.Kind = runtimecontract.EventTurnStarted
	case RuntimeTurnCompleted, RuntimeTurnFailed, RuntimeTurnInterrupted:
		canonical.Kind = runtimecontract.EventTerminal
		state := runtimecontract.LifecycleCompleted
		if event.Kind == RuntimeTurnFailed {
			state = runtimecontract.LifecycleFailed
		}
		if event.Kind == RuntimeTurnInterrupted {
			state = runtimecontract.LifecycleInterrupted
		}
		canonical.Outcome = &runtimecontract.Outcome{State: state, RuntimeTurnRef: event.NativeTurnID}
		if event.Kind == RuntimeTurnFailed {
			message := event.Error
			if message == "" {
				message = "Runtime Turn failed"
			}
			canonical.Outcome.Failure = &runtimecontract.Failure{Code: "runtime_error", Phase: runtimecontract.FailurePhaseTurnStart, Message: message, Diagnostic: event.Error}
		}
	default:
		canonical.Kind = runtimecontract.EventContent
		switch event.Kind {
		case RuntimeTextDelta, RuntimeReasoningDelta:
			canonical.ContentPhase = runtimecontract.ContentPhaseDelta
		case RuntimeToolStarted:
			canonical.ContentPhase = runtimecontract.ContentPhaseStarted
		case RuntimeToolUpdated:
			canonical.ContentPhase = runtimecontract.ContentPhaseUpdated
		case RuntimeToolCompleted:
			canonical.ContentPhase = runtimecontract.ContentPhaseCompleted
		default:
			canonical.ContentPhase = runtimecontract.ContentPhaseCompleted
		}
		id := event.ItemID
		if id == "" {
			id = "content"
		}
		content := runtimecontract.ContentBlock{ID: id, Text: event.Text}
		content.Diagnostic, _ = json.Marshal(event.Item)
		switch event.Kind {
		case RuntimeUserInput:
			content.Kind = runtimecontract.ContentUserText
		case RuntimeTextDelta, RuntimeTextCompleted:
			content.Kind = runtimecontract.ContentAssistantText
		case RuntimeReasoningDelta, RuntimeReasoningCompleted:
			content.Kind = runtimecontract.ContentReasoning
		case RuntimeToolCompleted:
			content.Kind = runtimecontract.ContentToolResult
			content.Text = ""
			content.ToolResult = codexToolResult(id, event.Item)
		default:
			arguments, _ := json.Marshal(event.Item)
			name, _ := event.Item["type"].(string)
			content.Kind = runtimecontract.ContentToolCall
			content.Text = ""
			content.ToolCall = &runtimecontract.ToolCall{Name: name, Arguments: arguments}
		}
		canonical.Content = &content
	}
	return canonical
}
