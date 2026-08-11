package hub

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yan5xu/codex-loom/internal/codex"
	"github.com/yan5xu/codex-loom/internal/rollout"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

// codexRuntimeContract translates Runtime Contract v2 values around the
// existing Codex-native primitive. Provider/model and policy controls remain
// adapter-private behind typed optional capability hooks.
type codexRuntimeContract struct {
	agentID string
	native  *codexAgentRuntime

	mu                      sync.Mutex
	sandbox                 string
	providerID              string
	model                   string
	disabledSkillPaths      []string
	approvalPolicy          string
	effort                  string
	developerContextTimeout time.Duration
	handler                 func(runtimecontract.Event)
	approvalHandler         func(runtimecontract.ApprovalProposal)
	approvalResponses       map[string]codexNativeApprovalResponse
	bindingRef              string
	release                 func()
	pendingTurn             runtimeTurnCorrelation
	turnsByNative           map[string]runtimeTurnCorrelation
}

type runtimeTurnCorrelation struct {
	turnID            string
	predecessorTurnID string
}

type codexNativeApprovalResponse struct {
	client *codex.Client
	id     json.RawMessage
}

func (c *codexRuntimeContract) ContractVersion() int { return runtimecontract.Version }

func (c *codexRuntimeContract) ContextDeliveryMode() runtimecontract.ContextDeliveryMode {
	return runtimecontract.ContextDeliveryEpochIncremental
}

func (c *codexRuntimeContract) RuntimeContextEvidence(_ context.Context, binding runtimecontract.Binding, query RuntimeContextEvidenceQuery) (RuntimeContextEvidence, error) {
	nativeQuery := rollout.ContextHistoryQuery{TurnID: query.TurnID, Deliveries: make([]rollout.ContextDeliveryProbe, 0, len(query.Deliveries))}
	for _, delivery := range query.Deliveries {
		nativeQuery.Deliveries = append(nativeQuery.Deliveries, rollout.ContextDeliveryProbe{Role: delivery.Role, Marker: delivery.Marker, Hash: delivery.Hash})
	}
	state, err := rollout.ContextHistory(binding.NativeRef, nativeQuery)
	if err != nil {
		return RuntimeContextEvidence{}, err
	}
	return RuntimeContextEvidence{
		EpochID: state.EpochID, WindowNumber: state.WindowNumber, CompactedAt: state.CompactedAt,
		DeliveriesPersisted: state.DeliveriesPersisted, PersistedDeliveryKeys: state.PersistedDeliveryKeys,
	}, nil
}

func (c *codexRuntimeContract) SetRuntimeSandbox(value string) {
	c.mu.Lock()
	c.sandbox = value
	c.mu.Unlock()
}

func (c *codexRuntimeContract) SetRuntimeProvider(providerID, model string) {
	c.mu.Lock()
	c.providerID, c.model = providerID, model
	c.mu.Unlock()
}

func (c *codexRuntimeContract) SetRuntimeModel(model string) {
	c.mu.Lock()
	c.model = model
	c.mu.Unlock()
}

func (c *codexRuntimeContract) SetRuntimeDisabledSkills(paths []string) {
	c.mu.Lock()
	c.disabledSkillPaths = append([]string(nil), paths...)
	c.mu.Unlock()
}

func (c *codexRuntimeContract) SetRuntimeApprovalPolicy(value string) {
	c.mu.Lock()
	c.approvalPolicy = value
	c.mu.Unlock()
}

func (c *codexRuntimeContract) SetRuntimeEffort(value string) {
	c.mu.Lock()
	c.effort = value
	c.mu.Unlock()
}

func (c *codexRuntimeContract) SetRuntimeDeveloperContextTimeout(value time.Duration) {
	c.mu.Lock()
	c.developerContextTimeout = value
	c.mu.Unlock()
}

func (c *codexRuntimeContract) nativeBindingRequest(name, cwd, nativeRef string) nativeBindingRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return nativeBindingRequest{NativeRef: nativeRef, Name: name, Cwd: cwd, Sandbox: c.sandbox, ProviderID: c.providerID, Model: c.model, DisabledSkillPaths: append([]string(nil), c.disabledSkillPaths...)}
}

func (c *codexRuntimeContract) nativeTurnRequest(request runtimecontract.TurnRequest) nativeTurnRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return nativeTurnRequest{LoomTurnID: request.TurnID, NativeRef: request.Binding.NativeRef, Input: contractInputToV1(request.Input), ApprovalPolicy: c.approvalPolicy, Sandbox: c.sandbox, Model: c.model, Effort: c.effort}
}

func (c *codexRuntimeContract) contextDeliveryTimeout() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.developerContextTimeout > 0 {
		return c.developerContextTimeout
	}
	return 30 * time.Second
}

func (c *codexRuntimeContract) CreateBinding(ctx context.Context, request runtimecontract.BindingRequest) (runtimecontract.Binding, runtimecontract.Outcome) {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseBindingCreate); failure != nil {
		return runtimecontract.Binding{}, *failure
	}
	controls := c.nativeBindingRequest(request.Name, request.Cwd, "")
	nativeRef, err := c.native.Create(controls)
	if err != nil {
		return runtimecontract.Binding{}, codexFailureOutcome(err, runtimecontract.FailurePhaseBindingCreate)
	}
	c.mu.Lock()
	c.bindingRef = nativeRef
	c.mu.Unlock()
	return runtimecontract.Binding{
		SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "codex", NativeRef: nativeRef,
	}, runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}
}

func (c *codexRuntimeContract) ResumeBinding(ctx context.Context, binding runtimecontract.Binding) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseBindingResume); failure != nil {
		return *failure
	}
	request := c.nativeBindingRequest("", "", binding.NativeRef)
	if err := c.native.Resume(request, contractTimeout(ctx, 60*time.Second)); err != nil {
		if errors.Is(err, rollout.ErrRolloutNotFound) {
			return runtimecontract.Outcome{State: runtimecontract.LifecycleRejected, Failure: &runtimecontract.Failure{
				Code: runtimecontract.FailureCodeBindingNotFound, Phase: runtimecontract.FailurePhaseBindingResume,
				Message: "Runtime binding was not found", Diagnostic: err.Error(), Cause: err,
			}}
		}
		return codexFailureOutcome(err, runtimecontract.FailurePhaseBindingResume)
	}
	c.mu.Lock()
	c.bindingRef = binding.NativeRef
	c.mu.Unlock()
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}

func (c *codexRuntimeContract) SetApprovalHandler(handler func(runtimecontract.ApprovalProposal)) {
	c.mu.Lock()
	c.approvalHandler = handler
	c.mu.Unlock()
}

func (c *codexRuntimeContract) ResolveApproval(_ context.Context, proposalID string, decision runtimecontract.ApprovalDecision) error {
	c.mu.Lock()
	response, ok := c.approvalResponses[proposalID]
	if ok {
		delete(c.approvalResponses, proposalID)
	}
	c.mu.Unlock()
	if !ok || response.client == nil {
		return fmt.Errorf("Runtime Approval proposal %s is unavailable", proposalID)
	}
	wireDecision := "cancel"
	if decision == runtimecontract.ApprovalApprove {
		wireDecision = "accept"
	}
	return response.client.Respond(response.id, map[string]any{"decision": wireDecision})
}

func (c *codexRuntimeContract) handlesNativeThread(threadID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return threadID != "" && c.bindingRef == threadID
}

func (c *codexRuntimeContract) canHandleApproval() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.approvalHandler != nil
}

func (c *codexRuntimeContract) handleNativeServerRequest(client *codex.Client, id json.RawMessage, method string, params json.RawMessage) bool {
	if !strings.Contains(strings.ToLower(method), "approval") {
		return false
	}
	c.mu.Lock()
	handler := c.approvalHandler
	if handler == nil {
		c.mu.Unlock()
		return false
	}
	proposalID := "runtime-approval-" + strings.TrimPrefix(newIntegrationID("proposal"), "proposal_")
	if c.approvalResponses == nil {
		c.approvalResponses = map[string]codexNativeApprovalResponse{}
	}
	c.approvalResponses[proposalID] = codexNativeApprovalResponse{client: client, id: append(json.RawMessage(nil), id...)}
	turnID := c.pendingTurn.turnID
	c.mu.Unlock()
	toolName, action := codexApprovalTool(method, params)
	handler(runtimecontract.ApprovalProposal{
		ID: proposalID, TurnID: turnID, ToolName: toolName,
		Action: action, Arguments: codexApprovalArguments(params), Timeout: 5 * time.Minute,
	})
	return true
}

func codexApprovalTool(method string, params json.RawMessage) (string, string) {
	var input map[string]any
	_ = json.Unmarshal(params, &input)
	name := ""
	if input != nil {
		for _, key := range []string{"toolName", "name", "tool"} {
			if value, ok := input[key].(string); ok && strings.TrimSpace(value) != "" {
				name = strings.TrimSpace(value)
				break
			}
		}
	}
	lowerMethod := strings.ToLower(method)
	action := "tool"
	if _, ok := input["command"]; ok || strings.Contains(lowerMethod, "command") {
		action = "command"
		if name == "" {
			name = "command"
		}
	} else if strings.Contains(lowerMethod, "filechange") || strings.Contains(lowerMethod, "edit") || input["path"] != nil || input["filePath"] != nil || input["patch"] != nil || input["diff"] != nil {
		action = "edit"
		if name == "" {
			name = "edit"
		}
	}
	if name == "" {
		name = "runtime"
	}
	name = strings.TrimPrefix(name, "tool/")
	return "tool/" + name, action
}

func codexApprovalArguments(params json.RawMessage) []runtimecontract.ApprovalArgument {
	var input map[string]any
	if json.Unmarshal(params, &input) != nil {
		return nil
	}
	arguments := make([]runtimecontract.ApprovalArgument, 0, len(input))
	for key, value := range input {
		if !approvalActionKey(key) {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			continue
		}
		arguments = append(arguments, runtimecontract.ApprovalArgument{Name: key, Value: strings.Trim(string(encoded), `"`)})
	}
	sort.Slice(arguments, func(i, j int) bool { return arguments[i].Name < arguments[j].Name })
	return arguments
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
	controls := c.nativeTurnRequest(request)
	c.setPendingTurn(request.TurnID, "")
	if developerContext := contractDeveloperContext(request.Input); developerContext != "" {
		if err := c.native.InjectDeveloperContext(request.Binding.NativeRef, developerContext, contractTimeout(ctx, c.contextDeliveryTimeout())); err != nil {
			return codexFailureOutcome(err, runtimecontract.FailurePhaseContextDelivery)
		}
	}
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
	emitted := 0
	for _, event := range events {
		correlation := c.correlationForEvent(event)
		if correlation.turnID == "" {
			// A stale or unrelated native event has no Loom-owned causal target.
			// Never guess that it belongs to the currently active Turn.
			continue
		}
		canonical := runtimeContractEvent(event, correlation)
		c.mu.Lock()
		handler := c.handler
		c.mu.Unlock()
		if handler != nil {
			handler(canonical)
			emitted++
		}
	}
	return emitted
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

func (c *codexRuntimeContract) turnIDForNative(nativeTurnID string) string {
	return c.correlationForNative(nativeTurnID).turnID
}

func (c *codexRuntimeContract) correlationForNative(nativeTurnID string) runtimeTurnCorrelation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turnsByNative[nativeTurnID]
}

func (c *codexRuntimeContract) seedTurnBindings(bindings map[string]string) {
	for loomTurnID, nativeTurnID := range bindings {
		c.bindTurn(loomTurnID, "", nativeTurnID)
	}
}

func (c *codexRuntimeContract) correlationForEvent(event nativeEvent) runtimeTurnCorrelation {
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
		correlation := c.correlationForNative(turn.ID)
		history.Turns = append(history.Turns, runtimecontract.HistoryTurn{
			TurnID:            correlation.turnID,
			PredecessorTurnID: correlation.predecessorTurnID,
			RuntimeTurnRef:    turn.ID,
			State:             codexHistoryState(turn.Status),
			Content:           codexHistoryContent(turn.Items),
			Usage:             codexContractUsage(turn.Usage),
			StartedAt:         turn.StartedAt,
			CompletedAt:       turn.CompletedAt,
			Diagnostic:        diagnostic,
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
	c.mu.Lock()
	imageInput := c.providerID != deepSeekProviderID
	c.mu.Unlock()
	return codexControlPlaneCapabilitySnapshot(imageInput)
}

func (c *codexRuntimeContract) ValidateRuntimeInput(_ context.Context, _ runtimecontract.Binding, input []runtimecontract.InputBlock) *runtimecontract.Failure {
	c.mu.Lock()
	imageInput := c.providerID != deepSeekProviderID
	c.mu.Unlock()
	if imageInput {
		return nil
	}
	for _, block := range input {
		if block.Kind == runtimecontract.InputImage {
			return &runtimecontract.Failure{Code: "image_input_unavailable", Phase: runtimecontract.FailurePhaseTurnStart, Message: "the active Runtime model does not accept image input"}
		}
	}
	return nil
}

func (c *codexRuntimeContract) RuntimeGoal(ctx context.Context, binding runtimecontract.Binding) (*ThreadGoal, error) {
	raw, err := c.native.client.Request("thread/goal/get", map[string]any{"threadId": binding.NativeRef}, contractTimeout(ctx, 15*time.Second))
	if err != nil {
		return nil, err
	}
	var response threadGoalGetResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	return response.Goal, nil
}

func (c *codexRuntimeContract) UpdateRuntimeGoal(ctx context.Context, binding runtimecontract.Binding, update GoalUpdateParams) (*ThreadGoal, error) {
	params := goalUpdateRuntimeParams(update)
	params["threadId"] = binding.NativeRef
	raw, err := c.native.client.Request("thread/goal/set", params, contractTimeout(ctx, 20*time.Second))
	if err != nil {
		return nil, err
	}
	var response threadGoalSetResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	return &response.Goal, nil
}

func (c *codexRuntimeContract) ClearRuntimeGoal(ctx context.Context, binding runtimecontract.Binding) (bool, error) {
	raw, err := c.native.client.Request("thread/goal/clear", map[string]any{"threadId": binding.NativeRef}, contractTimeout(ctx, 20*time.Second))
	if err != nil {
		return false, err
	}
	var response struct {
		Cleared bool `json:"cleared"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return false, err
	}
	return response.Cleared, nil
}

func (c *codexRuntimeContract) RuntimeUsage(ctx context.Context, binding runtimecontract.Binding) (*RuntimeUsageReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	report, err := rollout.ReadUsage(binding.NativeRef)
	if err != nil {
		return nil, err
	}
	return codexRuntimeUsageReport(report), nil
}

func codexRuntimeUsageReport(report *rollout.UsageReport) *RuntimeUsageReport {
	if report == nil {
		return nil
	}
	tokens := func(value rollout.TokenUsage) RuntimeTokenUsage {
		return RuntimeTokenUsage{
			InputTokens: value.InputTokens, CachedInputTokens: value.CachedInputTokens,
			OutputTokens: value.OutputTokens, ReasoningOutputTokens: value.ReasoningOutputTokens,
			TotalTokens: value.TotalTokens, Calls: value.Calls,
		}
	}
	result := &RuntimeUsageReport{
		Lifetime: tokens(report.Lifetime), LatestCall: tokens(report.LatestCall), LatestModel: report.LatestModel,
		ContextInputTokens: report.ContextInputTokens, ModelContextWindow: report.ModelContextWindow,
		LastUpdatedAt: report.LastUpdatedAt,
		Events:        make([]RuntimeUsageEvent, 0, len(report.Events)),
		Turns:         make([]RuntimeTurnUsage, 0, len(report.Turns)),
		Activity:      make([]RuntimeTurnActivity, 0, len(report.Activity)),
	}
	for _, event := range report.Events {
		result.Events = append(result.Events, RuntimeUsageEvent{Timestamp: event.Timestamp, TurnID: event.TurnID, Model: event.Model, Usage: tokens(event.Usage)})
	}
	for _, turn := range report.Turns {
		result.Turns = append(result.Turns, RuntimeTurnUsage{TurnID: turn.TurnID, Model: turn.Model, Usage: tokens(turn.Usage), LastUpdatedAt: turn.LastUpdatedAt})
	}
	for _, activity := range report.Activity {
		result.Activity = append(result.Activity, RuntimeTurnActivity{TurnID: activity.TurnID, StartedAt: activity.StartedAt, EndedAt: activity.EndedAt, Status: activity.Status, InferredEnd: activity.InferredEnd})
	}
	return result
}

func (c *codexRuntimeContract) CompactRuntimeBinding(ctx context.Context, binding runtimecontract.Binding) error {
	_, err := c.native.client.Request("thread/compact/start", map[string]any{"threadId": binding.NativeRef}, contractTimeout(ctx, 60*time.Second))
	return err
}

func (c *codexRuntimeContract) CloseBinding(ctx context.Context, _ runtimecontract.Binding) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseClose); failure != nil {
		return *failure
	}
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

func contractInputToV1(input []runtimecontract.InputBlock) []nativeInput {
	result := make([]nativeInput, 0, len(input))
	for _, block := range input {
		if block.Role == runtimecontract.InputRoleDeveloper {
			continue
		}
		switch block.Kind {
		case runtimecontract.InputText:
			result = append(result, nativeInput{Kind: nativeInputText, Text: block.Text})
		case runtimecontract.InputImage:
			result = append(result, nativeInput{Kind: nativeInputLocalImage, Path: block.Ref, MimeType: block.MIMEType})
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

func codexContractUsage(usage *nativeTokenUsage) *runtimecontract.Usage {
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

func runtimeContractEvent(event nativeEvent, correlation runtimeTurnCorrelation) runtimecontract.Event {
	canonical := runtimecontract.Event{
		TurnID: correlation.turnID, PredecessorTurnID: correlation.predecessorTurnID, RuntimeTurnRef: event.NativeTurnID,
	}
	switch event.Kind {
	case nativeTurnStarted:
		canonical.Kind = runtimecontract.EventTurnStarted
	case nativeTurnCompleted, nativeTurnFailed, nativeTurnInterrupted:
		canonical.Kind = runtimecontract.EventTerminal
		state := runtimecontract.LifecycleCompleted
		if event.Kind == nativeTurnFailed {
			state = runtimecontract.LifecycleFailed
		}
		if event.Kind == nativeTurnInterrupted {
			state = runtimecontract.LifecycleInterrupted
		}
		canonical.Outcome = &runtimecontract.Outcome{State: state, RuntimeTurnRef: event.NativeTurnID}
		if event.Kind == nativeTurnFailed {
			message := event.Error
			if message == "" {
				message = "Runtime Turn failed"
			}
			canonical.Outcome.Failure = &runtimecontract.Failure{Code: "runtime_error", Phase: runtimecontract.FailurePhaseTurnStart, Message: message, Diagnostic: event.Error}
		}
	default:
		canonical.Kind = runtimecontract.EventContent
		switch event.Kind {
		case nativeTextDelta, nativeReasoningDelta:
			canonical.ContentPhase = runtimecontract.ContentPhaseDelta
		case nativeToolStarted:
			canonical.ContentPhase = runtimecontract.ContentPhaseStarted
		case nativeToolUpdated:
			canonical.ContentPhase = runtimecontract.ContentPhaseUpdated
		case nativeToolCompleted:
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
		case nativeUserInput:
			content.Kind = runtimecontract.ContentUserText
		case nativeTextDelta, nativeTextCompleted:
			content.Kind = runtimecontract.ContentAssistantText
		case nativeReasoningDelta, nativeReasoningCompleted:
			content.Kind = runtimecontract.ContentReasoning
		case nativeToolCompleted:
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
