package hub

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

var errPiPromptAcceptanceIndeterminate = errors.New("Pi accepted prompt without a new native user entry")

// piRuntimeContract is Pi's authoritative v2 lifecycle boundary. Native model,
// Approval, and developer-context controls stay behind typed optional hooks.
type piRuntimeContract struct {
	agentID string
	native  *piAgentRuntime
	host    *piAgentHost

	mu                      sync.Mutex
	sandbox                 string
	providerID              string
	model                   string
	approvalPolicy          string
	effort                  string
	developerContextTimeout time.Duration
	handler                 func(runtimecontract.Event)
	release                 func()
	pendingTurn             runtimeTurnCorrelation
	turnsByNative           map[string]runtimeTurnCorrelation
}

var _ runtimecontract.Contract = (*piRuntimeContract)(nil)

func newPiRuntimeContract(agentID string, native *piAgentRuntime) *piRuntimeContract {
	return &piRuntimeContract{agentID: agentID, native: native, turnsByNative: map[string]runtimeTurnCorrelation{}}
}

func (c *piRuntimeContract) ContractVersion() int { return runtimecontract.Version }

func (c *piRuntimeContract) ContextDeliveryMode() runtimecontract.ContextDeliveryMode {
	return runtimecontract.ContextDeliveryFullPerTurn
}

func (c *piRuntimeContract) InspectResources(ctx context.Context, request runtimecontract.ResourceInventoryRequest) (runtimecontract.ResourceInventory, *runtimecontract.Failure) {
	if c.native == nil {
		return runtimecontract.ResourceInventory{}, &runtimecontract.Failure{Code: "resource_inventory_unavailable", Phase: runtimecontract.FailurePhaseResourceInventory, Message: "Pi resource inventory requires a running Runtime"}
	}
	inventory, err := c.native.resources(ctx)
	if err != nil {
		return runtimecontract.ResourceInventory{}, &runtimecontract.Failure{Code: "resource_inventory_failed", Phase: runtimecontract.FailurePhaseResourceInventory, Message: "Pi could not list its native resources", Diagnostic: err.Error(), Cause: err}
	}
	if err := inventory.Validate(); err != nil {
		return runtimecontract.ResourceInventory{}, &runtimecontract.Failure{Code: "resource_inventory_invalid", Phase: runtimecontract.FailurePhaseResourceInventory, Message: "Pi returned an invalid native resource inventory", Diagnostic: err.Error(), Cause: err}
	}
	return inventory, nil
}

func (c *piRuntimeContract) SetRuntimeSandbox(value string) {
	c.mu.Lock()
	c.sandbox = value
	c.mu.Unlock()
}

func (c *piRuntimeContract) SetRuntimeProvider(providerID, model string) {
	c.mu.Lock()
	c.providerID, c.model = providerID, model
	c.mu.Unlock()
}

func (c *piRuntimeContract) SetRuntimeModel(model string) {
	c.mu.Lock()
	c.model = model
	c.mu.Unlock()
}

func (c *piRuntimeContract) SetRuntimeApprovalPolicy(value string) {
	c.mu.Lock()
	c.approvalPolicy = value
	c.mu.Unlock()
}

func (c *piRuntimeContract) SetRuntimeEffort(value string) {
	c.mu.Lock()
	c.effort = value
	c.mu.Unlock()
}

func (c *piRuntimeContract) SetRuntimeDeveloperContextTimeout(value time.Duration) {
	c.mu.Lock()
	c.developerContextTimeout = value
	c.mu.Unlock()
}

func (c *piRuntimeContract) contextDeliveryTimeout() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.developerContextTimeout > 0 {
		return c.developerContextTimeout
	}
	return 30 * time.Second
}

func (c *piRuntimeContract) nativeBindingRequest(name, cwd, nativeRef string) nativeBindingRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return nativeBindingRequest{NativeRef: nativeRef, Name: name, Cwd: cwd, Sandbox: c.sandbox, ProviderID: c.providerID, Model: c.model}
}

func (c *piRuntimeContract) nativeTurnRequest(request runtimecontract.TurnRequest) nativeTurnRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return nativeTurnRequest{LoomTurnID: request.TurnID, NativeRef: request.Binding.NativeRef, Input: contractInputToV1(request.Input), ApprovalPolicy: c.approvalPolicy, Sandbox: c.sandbox, Model: c.model, Effort: c.effort}
}

func (c *piRuntimeContract) CreateBinding(ctx context.Context, request runtimecontract.BindingRequest) (runtimecontract.Binding, runtimecontract.Outcome) {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseBindingCreate); failure != nil {
		return runtimecontract.Binding{}, *failure
	}
	controls := c.nativeBindingRequest(request.Name, request.Cwd, "")
	nativeRef, err := c.host.createBinding(controls)
	if err != nil {
		return runtimecontract.Binding{}, piFailureOutcome(err, runtimecontract.FailurePhaseBindingCreate)
	}
	if err := c.reconcileConfiguredModel(ctx); err != nil {
		return runtimecontract.Binding{}, piModelReconcileOutcome(err, runtimecontract.FailurePhaseBindingCreate)
	}
	return piContractBinding(nativeRef), runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}
}

func (c *piRuntimeContract) ResumeBinding(ctx context.Context, binding runtimecontract.Binding) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseBindingResume); failure != nil {
		return *failure
	}
	request := c.nativeBindingRequest("", "", binding.NativeRef)
	if err := c.host.resumeBinding(request, contractTimeout(ctx, 60*time.Second)); err != nil {
		if os.IsNotExist(err) {
			return runtimecontract.Outcome{State: runtimecontract.LifecycleRejected, Failure: &runtimecontract.Failure{
				Code: runtimecontract.FailureCodeBindingNotFound, Phase: runtimecontract.FailurePhaseBindingResume,
				Message: "Runtime binding was not found", Diagnostic: err.Error(), Cause: err,
			}}
		}
		return piFailureOutcome(err, runtimecontract.FailurePhaseBindingResume)
	}
	if err := c.reconcileConfiguredModel(ctx); err != nil {
		return piModelReconcileOutcome(err, runtimecontract.FailurePhaseBindingResume)
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}

func (c *piRuntimeContract) reconcileConfiguredModel(ctx context.Context) error {
	c.mu.Lock()
	provider, model, effort := c.providerID, c.model, c.effort
	c.mu.Unlock()
	if provider == "" || model == "" || c.native == nil {
		return nil
	}
	state, err := c.native.models(contractTimeout(ctx, 15*time.Second))
	if err != nil {
		return err
	}
	if effort == "" {
		effort = state.ThinkingLevel
	}
	if state.Current.Provider == provider && state.Current.ID == model && state.ThinkingLevel == effort {
		return nil
	}
	_, err = c.native.switchModel(runtimecontract.ModelSelection{Provider: provider, Model: model, ThinkingLevel: effort}, contractTimeout(ctx, 15*time.Second))
	return err
}

func piModelReconcileOutcome(err error, phase runtimecontract.FailurePhase) runtimecontract.Outcome {
	var indeterminate *runtimeIndeterminateError
	if errors.As(err, &indeterminate) {
		return runtimecontract.Outcome{State: runtimecontract.LifecycleIndeterminate, Failure: indeterminate.RuntimeFailure()}
	}
	return piFailureOutcome(err, phase)
}

func (c *piRuntimeContract) StartTurn(ctx context.Context, request runtimecontract.TurnRequest) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseTurnStart); failure != nil {
		return *failure
	}
	controls := c.nativeTurnRequest(request)
	c.mu.Lock()
	c.pendingTurn = runtimeTurnCorrelation{turnID: request.TurnID}
	c.mu.Unlock()
	if developerContext := contractDeveloperContext(request.Input); developerContext != "" {
		if err := c.native.InjectDeveloperContext(request.Binding.NativeRef, developerContext, contractTimeout(ctx, c.contextDeliveryTimeout())); err != nil {
			return piFailureOutcome(err, runtimecontract.FailurePhaseContextDelivery)
		}
	}
	controls.Timeout = contractTimeout(ctx, 4*time.Hour)
	nativeTurnID, err := c.native.StartTurn(controls)
	if err != nil {
		return piFailureOutcome(err, runtimecontract.FailurePhaseTurnStart)
	}
	c.bindTurn(request.TurnID, "", nativeTurnID)
	return runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: nativeTurnID}
}

func (c *piRuntimeContract) ContinueTurn(ctx context.Context, request runtimecontract.CausalInput) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseTurnContinue); failure != nil {
		return *failure
	}
	texts := make([]string, 0, len(request.Input))
	for _, input := range request.Input {
		if input.Kind == runtimecontract.InputText && input.Text != "" {
			texts = append(texts, input.Text)
		}
	}
	c.mu.Lock()
	c.pendingTurn = runtimeTurnCorrelation{turnID: request.TurnID, predecessorTurnID: request.PredecessorTurnID}
	c.mu.Unlock()
	nativeTurnID, err := c.native.Steer(request.Binding.NativeRef, request.RuntimeTurnRef, strings.Join(texts, "\n"), contractTimeout(ctx, 30*time.Second))
	if err != nil {
		return piFailureOutcome(err, runtimecontract.FailurePhaseTurnContinue)
	}
	c.bindTurn(request.TurnID, request.PredecessorTurnID, nativeTurnID)
	return runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: nativeTurnID}
}

func (c *piRuntimeContract) InterruptTurn(ctx context.Context, request runtimecontract.TurnTarget) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseTurnInterrupt); failure != nil {
		return *failure
	}
	if request.TurnID == "" {
		return runtimecontract.Outcome{State: runtimecontract.LifecycleRejected, Failure: &runtimecontract.Failure{
			Code: "missing_turn_id", Phase: runtimecontract.FailurePhaseTurnInterrupt, Message: "canonical Loom Turn ID is required",
		}}
	}
	if err := c.native.Interrupt(request.Binding.NativeRef, request.RuntimeTurnRef, contractTimeout(ctx, 10*time.Second)); err != nil {
		return piFailureOutcome(err, runtimecontract.FailurePhaseTurnInterrupt)
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleInterrupted, RuntimeTurnRef: request.RuntimeTurnRef}
}

func (c *piRuntimeContract) SetEventHandler(handler func(runtimecontract.Event)) {
	c.mu.Lock()
	c.handler = handler
	c.mu.Unlock()
}

func (c *piRuntimeContract) handleNativeEvent(event nativeEvent) {
	c.mu.Lock()
	correlation := c.turnsByNative[event.NativeTurnID]
	if correlation.turnID == "" {
		correlation = c.pendingTurn
		if correlation.turnID != "" && event.NativeTurnID != "" {
			c.turnsByNative[event.NativeTurnID] = correlation
		}
	}
	if event.Kind == nativeTurnCompleted || event.Kind == nativeTurnFailed || event.Kind == nativeTurnInterrupted {
		if c.pendingTurn.turnID == correlation.turnID {
			c.pendingTurn = runtimeTurnCorrelation{}
		}
	}
	handler := c.handler
	c.mu.Unlock()
	if handler != nil {
		handler(runtimeContractEvent(event, correlation))
	}
}

func (c *piRuntimeContract) bindTurn(turnID, predecessorTurnID, nativeTurnID string) {
	if strings.TrimSpace(turnID) == "" || strings.TrimSpace(nativeTurnID) == "" {
		return
	}
	c.mu.Lock()
	c.turnsByNative[nativeTurnID] = runtimeTurnCorrelation{turnID: turnID, predecessorTurnID: predecessorTurnID}
	c.mu.Unlock()
}

func (c *piRuntimeContract) seedTurnBindings(bindings map[string]string) {
	c.mu.Lock()
	for loomTurnID, nativeTurnID := range bindings {
		if loomTurnID != "" && nativeTurnID != "" {
			c.turnsByNative[nativeTurnID] = runtimeTurnCorrelation{turnID: loomTurnID}
		}
	}
	c.mu.Unlock()
}

func (c *piRuntimeContract) turnIDForNative(nativeTurnID string) string {
	return c.correlationForNative(nativeTurnID).turnID
}

func (c *piRuntimeContract) correlationForNative(nativeTurnID string) runtimeTurnCorrelation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turnsByNative[nativeTurnID]
}

func (c *piRuntimeContract) ReadHistory(ctx context.Context, request runtimecontract.HistoryRequest) (runtimecontract.History, *runtimecontract.Failure) {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseHistory); failure != nil {
		return runtimecontract.History{}, failure.Failure
	}
	entries, leafID, err := c.native.piSessionEntries(request.Binding.NativeRef)
	if err != nil {
		if os.IsNotExist(err) {
			return runtimecontract.History{}, &runtimecontract.Failure{Code: "history_not_found", Phase: runtimecontract.FailurePhaseHistory, Message: "Runtime history not found"}
		}
		outcome := piFailureOutcome(err, runtimecontract.FailurePhaseHistory)
		return runtimecontract.History{}, outcome.Failure
	}
	history, err := projectPiCanonicalHistory(entries, leafID)
	if err != nil {
		outcome := piFailureOutcome(err, runtimecontract.FailurePhaseHistory)
		return runtimecontract.History{}, outcome.Failure
	}
	for index := range history.Turns {
		correlation := c.correlationForNative(history.Turns[index].RuntimeTurnRef)
		history.Turns[index].TurnID = correlation.turnID
		history.Turns[index].PredecessorTurnID = correlation.predecessorTurnID
	}
	count, offset := request.Count, request.Offset
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

func (c *piRuntimeContract) InspectInterruptedTurn(ctx context.Context, target runtimecontract.TurnTarget) (RuntimeInterruptionEvidence, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeInterruptionEvidence{}, err
	}
	// Recovery evidence is durable session history; reading it must not share
	// the live Pi RPC command stream with a concurrently resumed Turn.
	entries, leafID, err := readPiSessionEntries(target.Binding.NativeRef)
	if err != nil {
		return RuntimeInterruptionEvidence{}, err
	}
	return inspectPiInterruptedTurn(entries, leafID, target.RuntimeTurnRef)
}

func (c *piRuntimeContract) CapabilitySnapshot(context.Context, runtimecontract.Binding) runtimecontract.CapabilitySnapshot {
	if c.native == nil {
		return piControlPlaneCapabilitySnapshot()
	}
	c.native.mu.Lock()
	imageInput := c.native.imageInput
	c.native.mu.Unlock()
	return piControlPlaneCapabilitySnapshot(imageInput)
}

func (c *piRuntimeContract) ValidateInput(_ context.Context, _ runtimecontract.Binding, input []runtimecontract.InputBlock) *runtimecontract.Failure {
	imageInput := false
	if c.native != nil {
		c.native.mu.Lock()
		imageInput = c.native.imageInput
		c.native.mu.Unlock()
	}
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

func (c *piRuntimeContract) InspectModelControl(ctx context.Context, _ runtimecontract.Binding) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
	if err := ctx.Err(); err != nil {
		return runtimecontract.ModelControlState{}, modelControlFailure(err)
	}
	if c.native == nil {
		return runtimecontract.ModelControlState{}, modelControlFailure(errors.New("Pi Runtime model catalog is unavailable"))
	}
	state, err := c.native.models(contractTimeout(ctx, 15*time.Second))
	if err != nil {
		return runtimecontract.ModelControlState{}, modelControlFailure(err)
	}
	return state, nil
}

func (c *piRuntimeContract) SelectModel(ctx context.Context, _ runtimecontract.Binding, selection runtimecontract.ModelSelection) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
	if err := ctx.Err(); err != nil {
		return runtimecontract.ModelControlState{}, modelControlFailure(err)
	}
	if c.native == nil {
		return runtimecontract.ModelControlState{}, modelControlFailure(errors.New("Pi Runtime model switching is unavailable"))
	}
	state, err := c.native.switchModel(selection, contractTimeout(ctx, 15*time.Second))
	if err != nil {
		var indeterminate *runtimeIndeterminateError
		if errors.As(err, &indeterminate) {
			return runtimecontract.ModelControlState{}, indeterminate.RuntimeFailure()
		}
		return runtimecontract.ModelControlState{}, modelControlFailure(err)
	}
	return state, nil
}

func modelControlFailure(err error) *runtimecontract.Failure {
	return &runtimecontract.Failure{Code: "model_control_failed", Phase: runtimecontract.FailurePhaseModelControl, Message: err.Error(), Cause: err}
}

func (c *piRuntimeContract) SetApprovalHandler(handler func(runtimecontract.ApprovalProposal)) {
	if c.native != nil {
		c.native.SetApprovalHandler(handler)
	}
}

func (c *piRuntimeContract) ResolveApproval(ctx context.Context, proposalID string, decision runtimecontract.ApprovalDecision) error {
	if c.native == nil {
		return errors.New("Pi Runtime Approval is unavailable")
	}
	return c.native.ResolveApproval(ctx, proposalID, decision)
}

func (c *piRuntimeContract) CloseBinding(ctx context.Context, _ runtimecontract.Binding) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseClose); failure != nil {
		return *failure
	}
	if c.release != nil {
		c.release()
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}

func piFailureOutcome(err error, phase runtimecontract.FailurePhase) runtimecontract.Outcome {
	state, code := runtimecontract.LifecycleFailed, "runtime_error"
	if errors.Is(err, errPiPromptAcceptanceIndeterminate) {
		state, code = runtimecontract.LifecycleIndeterminate, "prompt_acceptance_unknown"
	} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		state, code = runtimecontract.LifecycleIndeterminate, "transport_timeout"
	}
	return runtimecontract.Outcome{State: state, Failure: &runtimecontract.Failure{
		Code: code, Phase: phase, Message: err.Error(), Diagnostic: err.Error(), Cause: err,
	}}
}

func piContractBinding(nativeRef string) runtimecontract.Binding {
	return runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "pi", NativeRef: nativeRef}
}
