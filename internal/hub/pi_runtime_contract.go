package hub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

var errPiPromptAcceptanceIndeterminate = errors.New("Pi accepted prompt without a new native user entry")

// piRuntimeContract is Pi's authoritative v2 lifecycle boundary. Native model,
// Approval, and developer-context controls remain compatibility extras.
type piRuntimeContract struct {
	agentID string
	native  *piAgentRuntime
	host    *piAgentHost

	mu             sync.Mutex
	bindingRequest RuntimeBindingRequest
	turnRequest    RuntimeTurnRequest
	handler        func(runtimecontract.Event)
	release        func()
	pendingTurn    runtimeTurnCorrelation
	turnsByNative  map[string]runtimeTurnCorrelation
}

var _ runtimecontract.Contract = (*piRuntimeContract)(nil)

func newPiRuntimeContract(agentID string, native *piAgentRuntime) *piRuntimeContract {
	return &piRuntimeContract{agentID: agentID, native: native, turnsByNative: map[string]runtimeTurnCorrelation{}}
}

func (c *piRuntimeContract) ContractVersion() int { return runtimecontract.Version }

func (c *piRuntimeContract) ContextDeliveryMode() runtimecontract.ContextDeliveryMode {
	return runtimecontract.ContextDeliveryFullPerTurn
}

func (c *piRuntimeContract) setCompatibilityBinding(request RuntimeBindingRequest) {
	c.mu.Lock()
	c.bindingRequest = request
	c.mu.Unlock()
}

func (c *piRuntimeContract) setCompatibilityTurn(request RuntimeTurnRequest) {
	c.mu.Lock()
	c.turnRequest = request
	c.mu.Unlock()
}

func (c *piRuntimeContract) CreateBinding(ctx context.Context, request runtimecontract.BindingRequest) (runtimecontract.Binding, runtimecontract.Outcome) {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseBindingCreate); failure != nil {
		return runtimecontract.Binding{}, *failure
	}
	c.mu.Lock()
	controls := c.bindingRequest
	c.mu.Unlock()
	controls.Name, controls.Cwd = request.Name, request.Cwd
	nativeRef, err := c.host.createBinding(controls)
	if err != nil {
		return runtimecontract.Binding{}, piFailureOutcome(err, runtimecontract.FailurePhaseBindingCreate)
	}
	return piContractBinding(nativeRef), runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}
}

func (c *piRuntimeContract) ResumeBinding(ctx context.Context, binding runtimecontract.Binding) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseBindingResume); failure != nil {
		return *failure
	}
	c.mu.Lock()
	request := c.bindingRequest
	c.mu.Unlock()
	request.NativeRef = binding.NativeRef
	if err := c.host.resumeBinding(request, contractTimeout(ctx, 60*time.Second)); err != nil {
		return piFailureOutcome(err, runtimecontract.FailurePhaseBindingResume)
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}

func (c *piRuntimeContract) StartTurn(ctx context.Context, request runtimecontract.TurnRequest) runtimecontract.Outcome {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseTurnStart); failure != nil {
		return *failure
	}
	c.mu.Lock()
	controls := c.turnRequest
	c.pendingTurn = runtimeTurnCorrelation{turnID: request.TurnID}
	c.mu.Unlock()
	if developerContext := contractDeveloperContext(request.Input); developerContext != "" {
		if err := c.native.InjectDeveloperContext(request.Binding.NativeRef, developerContext, contractTimeout(ctx, 30*time.Second)); err != nil {
			return piFailureOutcome(err, runtimecontract.FailurePhaseContextDelivery)
		}
	}
	controls.LoomTurnID = request.TurnID
	controls.NativeRef = request.Binding.NativeRef
	controls.Input = contractInputToV1(request.Input)
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

func (c *piRuntimeContract) handleNativeEvent(event RuntimeEvent) {
	c.mu.Lock()
	correlation := c.turnsByNative[event.NativeTurnID]
	if correlation.turnID == "" {
		correlation = c.pendingTurn
		if correlation.turnID != "" && event.NativeTurnID != "" {
			c.turnsByNative[event.NativeTurnID] = correlation
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
	if c.pendingTurn.turnID == turnID {
		c.pendingTurn = runtimeTurnCorrelation{}
	}
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
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turnsByNative[nativeTurnID].turnID
}

func (c *piRuntimeContract) ReadHistory(ctx context.Context, request runtimecontract.HistoryRequest) (runtimecontract.History, *runtimecontract.Failure) {
	if failure := contextFailure(ctx, runtimecontract.FailurePhaseHistory); failure != nil {
		return runtimecontract.History{}, failure.Failure
	}
	native, err := c.native.ReadHistory(request.Binding.NativeRef, request.Count, request.Offset)
	if err != nil {
		outcome := piFailureOutcome(err, runtimecontract.FailurePhaseHistory)
		return runtimecontract.History{}, outcome.Failure
	}
	history := runtimecontract.History{Total: native.Total}
	for _, turn := range native.Turns {
		diagnostic, _ := json.Marshal(turn)
		history.Turns = append(history.Turns, runtimecontract.HistoryTurn{
			TurnID: c.turnIDForNative(turn.ID), RuntimeTurnRef: turn.ID,
			State: codexHistoryState(turn.Status), Content: codexHistoryContent(turn.Items),
			Usage: codexContractUsage(turn.Usage), StartedAt: turn.StartedAt, CompletedAt: turn.CompletedAt,
			Diagnostic: diagnostic,
		})
	}
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
	return piControlPlaneCapabilitySnapshot()
}

func (c *piRuntimeContract) CloseBinding(context.Context, runtimecontract.Binding) runtimecontract.Outcome {
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
