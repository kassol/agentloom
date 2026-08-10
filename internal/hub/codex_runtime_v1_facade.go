package hub

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

// codexRuntimeV1Facade keeps existing Hub consumers green while making the v2
// Contract the authoritative Codex lifecycle path. Concrete compatibility
// extras stay on the native primitive until their capability tickets land.
type codexRuntimeV1Facade struct {
	host     *codexAgentHost
	contract *codexRuntimeContract
	native   *codexAgentRuntime
}

func (f *codexRuntimeV1Facade) Alive() bool { return f != nil && f.host != nil && f.host.Alive() }

func (f *codexRuntimeV1Facade) Create(request RuntimeBindingRequest) (string, error) {
	f.contract.setCompatibilityBinding(request)
	binding, outcome := f.contract.CreateBinding(context.Background(), runtimecontract.BindingRequest{
		AgentID: f.contract.agentID, Name: request.Name, Cwd: request.Cwd,
	})
	return binding.NativeRef, compatibilityOutcomeError(outcome)
}

func (f *codexRuntimeV1Facade) Resume(request RuntimeBindingRequest, timeout time.Duration) error {
	f.contract.setCompatibilityBinding(request)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return compatibilityOutcomeError(f.contract.ResumeBinding(ctx, contractBinding(request.NativeRef)))
}

func (f *codexRuntimeV1Facade) InjectDeveloperContext(nativeRef, content string, timeout time.Duration) error {
	return f.native.InjectDeveloperContext(nativeRef, content, timeout)
}

func (f *codexRuntimeV1Facade) StartTurn(request RuntimeTurnRequest) (string, error) {
	f.contract.setCompatibilityTurn(request)
	ctx, cancel := context.WithTimeout(context.Background(), request.Timeout)
	defer cancel()
	outcome := f.contract.StartTurn(ctx, runtimecontract.TurnRequest{
		Binding: contractBinding(request.NativeRef), TurnID: request.LoomTurnID, Input: v1InputToContract(request.Input),
	})
	return outcome.RuntimeTurnRef, compatibilityOutcomeError(outcome)
}

func (f *codexRuntimeV1Facade) Steer(nativeRef, expectedNativeTurnID, input string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	outcome := f.contract.ContinueTurn(ctx, runtimecontract.CausalInput{
		Binding: contractBinding(nativeRef), TurnID: f.contract.turnIDForNative(expectedNativeTurnID), RuntimeTurnRef: expectedNativeTurnID,
		Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: input}},
	})
	return outcome.RuntimeTurnRef, compatibilityOutcomeError(outcome)
}

func (c *codexRuntimeContract) turnIDForNative(nativeTurnID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turnsByNative[nativeTurnID].turnID
}

func (f *codexRuntimeV1Facade) Interrupt(nativeRef, nativeTurnID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return compatibilityOutcomeError(f.contract.InterruptTurn(ctx, runtimecontract.TurnTarget{
		Binding: contractBinding(nativeRef), TurnID: f.contract.turnIDForNative(nativeTurnID), RuntimeTurnRef: nativeTurnID,
	}))
}

func (f *codexRuntimeV1Facade) NormalizeEvent(method string, params json.RawMessage) []RuntimeEvent {
	// Raw compatibility normalization remains concrete until the canonical
	// product-stream ticket removes it from ordinary Hub consumers.
	return f.native.NormalizeEvent(method, params)
}

func (f *codexRuntimeV1Facade) ReadHistory(nativeRef string, count, offset int) (RuntimeHistory, error) {
	history, failure := f.contract.ReadHistory(context.Background(), runtimecontract.HistoryRequest{
		Binding: contractBinding(nativeRef), Count: count, Offset: offset,
	})
	if failure != nil {
		return RuntimeHistory{}, compatibilityFailureError(failure)
	}
	result := RuntimeHistory{Total: history.Total}
	for _, turn := range history.Turns {
		var legacy RuntimeHistoryTurn
		if len(turn.Diagnostic) == 0 || json.Unmarshal(turn.Diagnostic, &legacy) != nil {
			legacy = RuntimeHistoryTurn{ID: turn.RuntimeTurnRef, Status: compatibilityLifecycleStatus(turn.State)}
		}
		result.Turns = append(result.Turns, legacy)
	}
	return result, nil
}

func (f *codexRuntimeV1Facade) ReadTurn(nativeRef, nativeTurnID string) (RuntimeHistoryTurn, error) {
	return f.native.ReadTurn(nativeRef, nativeTurnID)
}

func (f *codexRuntimeV1Facade) LatestTurn(nativeRef string) (*RuntimeHistoryTurn, error) {
	return f.native.LatestTurn(nativeRef)
}

func (f *codexRuntimeV1Facade) Capabilities() RuntimeCapabilities { return f.native.Capabilities() }

func (f *codexRuntimeV1Facade) Close() {
	f.contract.CloseBinding(context.Background(), runtimecontract.Binding{})
}

func contractBinding(nativeRef string) runtimecontract.Binding {
	return runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "codex", NativeRef: nativeRef}
}

func v1InputToContract(input []RuntimeInput) []runtimecontract.InputBlock {
	result := make([]runtimecontract.InputBlock, 0, len(input))
	for _, item := range input {
		switch item.Kind {
		case RuntimeInputText:
			result = append(result, runtimecontract.InputBlock{Kind: runtimecontract.InputText, Text: item.Text})
		case RuntimeInputLocalImage:
			result = append(result, runtimecontract.InputBlock{Kind: runtimecontract.InputImage, Ref: item.Path, MIMEType: item.MimeType})
		}
	}
	return result
}

func compatibilityOutcomeError(outcome runtimecontract.Outcome) error {
	if outcome.Failure == nil {
		return nil
	}
	if outcome.Failure.Cause != nil {
		return outcome.Failure.Cause
	}
	if outcome.Failure.Diagnostic != "" {
		return errors.New(outcome.Failure.Diagnostic)
	}
	return errors.New(outcome.Failure.Message)
}

func compatibilityFailureError(failure *runtimecontract.Failure) error {
	if failure == nil {
		return nil
	}
	if failure.Cause != nil {
		return failure.Cause
	}
	if failure.Diagnostic != "" {
		return errors.New(failure.Diagnostic)
	}
	return errors.New(failure.Message)
}

func compatibilityLifecycleStatus(state runtimecontract.LifecycleState) string {
	switch state {
	case runtimecontract.LifecycleCompleted:
		return "completed"
	case runtimecontract.LifecycleInterrupted:
		return "interrupted"
	case runtimecontract.LifecycleFailed, runtimecontract.LifecycleRejected, runtimecontract.LifecycleIndeterminate:
		return "failed"
	default:
		return "running"
	}
}
