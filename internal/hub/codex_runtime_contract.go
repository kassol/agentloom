package hub

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yan5xu/codex-loom/internal/codex"
	"github.com/yan5xu/codex-loom/internal/rollout"
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

func (c *codexRuntimeContract) ContextDeliveryMode() runtimecontract.ContextDeliveryMode {
	return runtimecontract.ContextDeliveryEpochIncremental
}

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

func (c *codexRuntimeContract) UpdateBindingName(ctx context.Context, binding runtimecontract.Binding, name string) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseBindingName); failure != nil {
		return *failure
	}
	if err := setThreadName(c.native.client, binding.NativeRef, name); err != nil {
		return codexFailureOutcome(err, runtimecontract.FailurePhaseBindingName)
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}

func (c *codexRuntimeContract) ArchiveBinding(ctx context.Context, binding runtimecontract.Binding) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseBindingArchive); failure != nil {
		return *failure
	}
	_, err := c.native.client.Request("thread/archive", map[string]any{"threadId": binding.NativeRef}, contractTimeout(ctx, 10*time.Second))
	if err != nil {
		return codexFailureOutcome(err, runtimecontract.FailurePhaseBindingArchive)
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}

func (c *codexRuntimeContract) StartTurn(ctx context.Context, request runtimecontract.TurnRequest) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseTurnStart); failure != nil {
		return *failure
	}
	controls := c.turnControls()
	c.setPendingTurn(request.TurnID, "")
	if developerContext := contractDeveloperContext(request.Input); developerContext != "" {
		if err := c.native.InjectDeveloperContext(request.Binding.NativeRef, developerContext, contractTimeout(ctx, 30*time.Second)); err != nil {
			return codexFailureOutcome(err, runtimecontract.FailurePhaseContextDelivery)
		}
	}
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
	timeout := contractTimeout(ctx, 10*time.Second)
	target := request.RuntimeTurnRef
	err := c.native.Interrupt(request.Binding.NativeRef, target, timeout)
	if actual, mismatch := activeTurnInterruptMismatch(err); mismatch && actual != target {
		// The adapter, not Loom's control plane, owns native correlation repair.
		// Bind the authoritative native Turn to the same Loom Turn and retry the
		// interrupt once; native identities remain private to this boundary.
		c.bindTurn(request.TurnID, "", actual)
		target = actual
		err = c.native.Interrupt(request.Binding.NativeRef, target, timeout)
	}
	if err != nil {
		return codexFailureOutcome(err, runtimecontract.FailurePhaseTurnInterrupt)
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleInterrupted, RuntimeTurnRef: target}
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
		if errors.Is(err, rollout.ErrRolloutNotFound) || errors.Is(err, rollout.ErrTurnNotFound) {
			return runtimecontract.History{}, &runtimecontract.Failure{Code: "history_not_found", Phase: runtimecontract.FailurePhaseHistory, Message: "Runtime history not found"}
		}
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

func (c *codexRuntimeContract) InspectInterruptedTurn(ctx context.Context, target runtimecontract.TurnTarget) (RuntimeInterruptionEvidence, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeInterruptionEvidence{}, err
	}
	turn, err := c.native.ReadTurn(target.Binding.NativeRef, target.RuntimeTurnRef)
	if err != nil {
		return RuntimeInterruptionEvidence{}, err
	}
	return inspectCodexInterruptedTurn(turn), nil
}

func (c *codexRuntimeContract) CapabilitySnapshot(context.Context, runtimecontract.Binding) runtimecontract.CapabilitySnapshot {
	return codexControlPlaneCapabilitySnapshot()
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
	if codexMutatingFailurePhase(phase) {
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

func codexMutatingFailurePhase(phase runtimecontract.FailurePhase) bool {
	switch phase {
	case runtimecontract.FailurePhaseBindingCreate, runtimecontract.FailurePhaseBindingResume,
		runtimecontract.FailurePhaseContextDelivery, runtimecontract.FailurePhaseTurnStart,
		runtimecontract.FailurePhaseTurnContinue, runtimecontract.FailurePhaseTurnInterrupt:
		return true
	default:
		return false
	}
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
		if block.Role == runtimecontract.InputRoleDeveloper {
			continue
		}
		switch block.Kind {
		case runtimecontract.InputText:
			result = append(result, RuntimeInput{Kind: RuntimeInputText, Text: block.Text})
		case runtimecontract.InputImage:
			result = append(result, RuntimeInput{Kind: RuntimeInputLocalImage, Path: block.Ref, MimeType: block.MIMEType})
		}
	}
	return result
}

func contractDeveloperContext(input []runtimecontract.InputBlock) string {
	parts := make([]string, 0, 1)
	for _, block := range input {
		if block.Kind == runtimecontract.InputText && block.Role == runtimecontract.InputRoleDeveloper && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
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
			visibleText, attachments := historyManagedAttachments(text, item)
			block.Text = visibleText
			content = append(content, block)
			for attachmentIndex, attachment := range attachments {
				attachmentID := id + "-attachment-" + fmt.Sprint(attachmentIndex+1)
				if strings.HasPrefix(strings.ToLower(attachment.MIMEType), "image/") {
					image := runtimecontract.Image(attachment)
					content = append(content, runtimecontract.ContentBlock{ID: attachmentID, Kind: runtimecontract.ContentImage, Image: &image})
				} else {
					copy := attachment
					content = append(content, runtimecontract.ContentBlock{ID: attachmentID, Kind: runtimecontract.ContentAttachment, Attachment: &copy})
				}
			}
			continue
		case "agentmessage", "assistantmessage", "agent_message", "assistant_text", "answer":
			block.Kind = runtimecontract.ContentAssistantText
		case "reasoning", "thinking":
			block.Kind = runtimecontract.ContentReasoning
		case "image":
			ref, _ := item["data"].(string)
			if ref == "" {
				ref, _ = item["url"].(string)
			}
			if ref == "" {
				ref, _ = item["path"].(string)
			}
			if ref == "" {
				continue
			}
			mimeType, _ := item["mimeType"].(string)
			block.Kind, block.Text = runtimecontract.ContentImage, ""
			block.Image = &runtimecontract.Image{MIMEType: mimeType, Ref: ref}
		case "command", "commandexecution":
			arguments, _ := json.Marshal(map[string]any{"command": item["command"], "cwd": item["cwd"]})
			block.Kind = runtimecontract.ContentToolCall
			block.Text = ""
			block.ToolCall = &runtimecontract.ToolCall{Name: "exec_command", Arguments: arguments}
			content = append(content, block)
			if runtimeHistoryToolSettled(item) {
				content = append(content, runtimecontract.ContentBlock{
					ID: id + "-result", Kind: runtimecontract.ContentToolResult,
					ToolResult: codexToolResult(id, item),
				})
			}
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

func runtimeHistoryToolSettled(item map[string]any) bool {
	status, _ := item["status"].(string)
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "failed", "error", "interrupted", "aborted", "cancelled", "canceled":
		return true
	}
	for _, key := range []string{"aggregatedOutput", "output", "exitCode", "error"} {
		if value, ok := item[key]; ok && value != nil && value != "" {
			return true
		}
	}
	return false
}

type historyAttachmentXML struct {
	ID       string `xml:"id,attr"`
	Name     string `xml:"name,attr"`
	MIMEType string `xml:"mime_type,attr"`
	Size     int64  `xml:"size,attr"`
	Path     string `xml:"path,attr"`
	URL      string `xml:"url,attr"`
}

type historyAttachmentManifest struct {
	Attachments []historyAttachmentXML `xml:"attachment"`
}

func historyManagedAttachments(text string, item map[string]any) (string, []runtimecontract.Attachment) {
	visible := text
	manifest := ""
	if index := strings.Index(text, "<loom_attachments"); index >= 0 {
		visible, manifest = strings.TrimSpace(text[:index]), strings.TrimSpace(text[index:])
	}
	attachments := make([]runtimecontract.Attachment, 0)
	if manifest != "" {
		var parsed historyAttachmentManifest
		if xml.Unmarshal([]byte(manifest), &parsed) == nil {
			for _, source := range parsed.Attachments {
				ref := source.URL
				if source.ID != "" {
					ref = "artifact:" + source.ID
				}
				attachments = append(attachments, runtimecontract.Attachment{ID: source.ID, Name: source.Name, Size: source.Size, MIMEType: source.MIMEType, Ref: ref})
			}
		}
	}
	if raw, ok := item["attachments"].([]map[string]any); ok {
		for _, source := range raw {
			id, _ := source["id"].(string)
			name, _ := source["name"].(string)
			mimeType, _ := source["mimeType"].(string)
			ref, _ := source["url"].(string)
			if id != "" {
				ref = "artifact:" + id
			}
			var size int64
			switch value := source["size"].(type) {
			case int64:
				size = value
			case int:
				size = int64(value)
			case float64:
				size = int64(value)
			}
			attachments = append(attachments, runtimecontract.Attachment{ID: id, Name: name, Size: size, MIMEType: mimeType, Ref: ref})
		}
	}
	seen := map[string]bool{}
	deduplicated := attachments[:0]
	for _, attachment := range attachments {
		if attachment.ID == "" && !strings.HasPrefix(attachment.Ref, "/api/agents/") {
			continue
		}
		key := attachment.ID
		if key == "" {
			key = attachment.Ref + "\x00" + attachment.Name
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		deduplicated = append(deduplicated, attachment)
	}
	return visible, deduplicated
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
