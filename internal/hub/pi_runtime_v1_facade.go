package hub

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

type piRuntimeV1Facade struct {
	host     *piAgentHost
	contract *piRuntimeContract
	native   *piAgentRuntime
}

var _ AgentRuntime = (*piRuntimeV1Facade)(nil)
var _ RuntimeApprovalSource = (*piRuntimeV1Facade)(nil)
var _ RuntimeInterruptedTurnInspector = (*piRuntimeV1Facade)(nil)

type runtimeIndeterminateError struct {
	failure *runtimecontract.Failure
}

func (e *runtimeIndeterminateError) Error() string {
	if e == nil || e.failure == nil {
		return "Runtime outcome is indeterminate"
	}
	return e.failure.Message
}

func (e *runtimeIndeterminateError) Unwrap() error {
	if e == nil || e.failure == nil {
		return nil
	}
	return e.failure.Cause
}

func piCompatibilityOutcomeError(outcome runtimecontract.Outcome) error {
	if outcome.State == runtimecontract.LifecycleIndeterminate && outcome.Failure != nil {
		return &runtimeIndeterminateError{failure: outcome.Failure}
	}
	return compatibilityOutcomeError(outcome)
}

func (f *piRuntimeV1Facade) Alive() bool { return f != nil && f.host != nil && f.host.Alive() }

func (f *piRuntimeV1Facade) Create(request RuntimeBindingRequest) (string, error) {
	f.contract.setCompatibilityBinding(request)
	binding, outcome := f.contract.CreateBinding(context.Background(), runtimecontract.BindingRequest{
		AgentID: f.contract.agentID, Name: request.Name, Cwd: request.Cwd,
	})
	return binding.NativeRef, piCompatibilityOutcomeError(outcome)
}

func (f *piRuntimeV1Facade) Resume(request RuntimeBindingRequest, timeout time.Duration) error {
	f.contract.setCompatibilityBinding(request)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return piCompatibilityOutcomeError(f.contract.ResumeBinding(ctx, piContractBinding(request.NativeRef)))
}

func (f *piRuntimeV1Facade) InjectDeveloperContext(nativeRef, content string, timeout time.Duration) error {
	return f.native.InjectDeveloperContext(nativeRef, content, timeout)
}

func (f *piRuntimeV1Facade) StartTurn(request RuntimeTurnRequest) (string, error) {
	f.contract.setCompatibilityTurn(request)
	ctx, cancel := context.WithTimeout(context.Background(), request.Timeout)
	defer cancel()
	outcome := f.contract.StartTurn(ctx, runtimecontract.TurnRequest{
		Binding: piContractBinding(request.NativeRef), TurnID: request.LoomTurnID, Input: v1InputToContract(request.Input),
	})
	return outcome.RuntimeTurnRef, piCompatibilityOutcomeError(outcome)
}

func (f *piRuntimeV1Facade) Steer(nativeRef, expectedNativeTurnID, input string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	outcome := f.contract.ContinueTurn(ctx, runtimecontract.CausalInput{
		Binding: piContractBinding(nativeRef), TurnID: f.contract.turnIDForNative(expectedNativeTurnID), RuntimeTurnRef: expectedNativeTurnID,
		Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: input}},
	})
	return outcome.RuntimeTurnRef, piCompatibilityOutcomeError(outcome)
}

func (f *piRuntimeV1Facade) Interrupt(nativeRef, nativeTurnID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return piCompatibilityOutcomeError(f.contract.InterruptTurn(ctx, runtimecontract.TurnTarget{
		Binding: piContractBinding(nativeRef), TurnID: f.contract.turnIDForNative(nativeTurnID), RuntimeTurnRef: nativeTurnID,
	}))
}

var _ error = (*runtimeIndeterminateError)(nil)
var _ interface{ Unwrap() error } = (*runtimeIndeterminateError)(nil)

func isRuntimeIndeterminate(err error) bool {
	var target *runtimeIndeterminateError
	return errors.As(err, &target)
}

func (f *piRuntimeV1Facade) NormalizeEvent(_ string, _ json.RawMessage) []RuntimeEvent { return nil }

func (f *piRuntimeV1Facade) ReadHistory(nativeRef string, count, offset int) (RuntimeHistory, error) {
	history, failure := f.contract.ReadHistory(context.Background(), runtimecontract.HistoryRequest{Binding: piContractBinding(nativeRef), Count: count, Offset: offset})
	if failure != nil {
		return RuntimeHistory{}, compatibilityFailureError(failure)
	}
	result := RuntimeHistory{Total: history.Total}
	for _, turn := range history.Turns {
		var legacy RuntimeHistoryTurn
		if len(turn.Diagnostic) == 0 || json.Unmarshal(turn.Diagnostic, &legacy) != nil {
			legacy = RuntimeHistoryTurn{ID: turn.RuntimeTurnRef, Status: compatibilityLifecycleStatus(turn.State)}
		}
		for _, item := range legacy.Items {
			values, ok := item["attachments"].([]any)
			if !ok {
				continue
			}
			attachments := make([]map[string]any, 0, len(values))
			for _, value := range values {
				if attachment, ok := value.(map[string]any); ok {
					attachments = append(attachments, attachment)
				}
			}
			item["attachments"] = attachments
		}
		result.Turns = append(result.Turns, legacy)
	}
	return result, nil
}

func (f *piRuntimeV1Facade) ReadTurn(nativeRef, nativeTurnID string) (RuntimeHistoryTurn, error) {
	return f.native.ReadTurn(nativeRef, nativeTurnID)
}
func (f *piRuntimeV1Facade) LatestTurn(nativeRef string) (*RuntimeHistoryTurn, error) {
	return f.native.LatestTurn(nativeRef)
}
func (f *piRuntimeV1Facade) Capabilities() RuntimeCapabilities { return f.native.Capabilities() }
func (f *piRuntimeV1Facade) SetRuntimeApprovalHandler(handler func(RuntimeApprovalRequest)) {
	f.native.SetRuntimeApprovalHandler(handler)
}
func (f *piRuntimeV1Facade) InspectInterruptedTurn(nativeRef, nativeTurnID string) (RuntimeInterruptionEvidence, error) {
	return f.native.InspectInterruptedTurn(nativeRef, nativeTurnID)
}
func (f *piRuntimeV1Facade) Close() {
	f.contract.CloseBinding(context.Background(), runtimecontract.Binding{})
}
