package runtimecontract_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

type fakeContract struct{}

func (fakeContract) ContractVersion() int { return runtimecontract.Version }
func (fakeContract) CreateBinding(context.Context, runtimecontract.BindingRequest) (runtimecontract.Binding, runtimecontract.Outcome) {
	return runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "fake", NativeRef: "opaque"}, runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}
}
func (fakeContract) ResumeBinding(context.Context, runtimecontract.Binding) runtimecontract.Outcome {
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}
func (fakeContract) StartTurn(context.Context, runtimecontract.TurnRequest) runtimecontract.Outcome {
	return runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: "opaque-turn"}
}
func (fakeContract) ContinueTurn(context.Context, runtimecontract.CausalInput) runtimecontract.Outcome {
	return runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}
}
func (fakeContract) InterruptTurn(context.Context, runtimecontract.TurnTarget) runtimecontract.Outcome {
	return runtimecontract.Outcome{State: runtimecontract.LifecycleInterrupted}
}
func (fakeContract) SetEventHandler(func(runtimecontract.Event)) {}
func (fakeContract) ReadHistory(context.Context, runtimecontract.HistoryRequest) (runtimecontract.History, *runtimecontract.Failure) {
	return runtimecontract.History{}, nil
}
func (fakeContract) CapabilitySnapshot(context.Context, runtimecontract.Binding) runtimecontract.CapabilitySnapshot {
	return runtimecontract.CapabilitySnapshot{}
}
func (fakeContract) CloseBinding(context.Context, runtimecontract.Binding) runtimecontract.Outcome {
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}

var _ runtimecontract.Contract = fakeContract{}

func TestContractV2RepresentsTypedCapabilityLifecycleContentUsageAndFailure(t *testing.T) {
	descriptor := runtimecontract.CapabilityDescriptor{
		ID:           "image_input",
		Availability: runtimecontract.CapabilityUnavailable,
		Scope: runtimecontract.CapabilityScope{
			RuntimeKind: "fake", Model: "text-only", ConfigurationRevision: "config-7",
		},
		Reason:      "the selected model accepts text only",
		Alternative: "select a vision model",
		Evidence: []runtimecontract.CapabilityEvidence{{
			Kind: "model_catalog", Summary: "image input is false", ObservedAt: "2026-08-10T00:00:00Z",
		}},
		Revision: "cap-9",
	}
	outcome := runtimecontract.Outcome{
		State: runtimecontract.LifecycleIndeterminate,
		Failure: &runtimecontract.Failure{
			Code: "transport_timeout", Phase: runtimecontract.FailurePhaseTurnStart,
			Message: "acceptance is unknown", Retryable: false,
		},
	}
	history := runtimecontract.History{Turns: []runtimecontract.HistoryTurn{{
		TurnID: "turn-loom", RuntimeTurnRef: "opaque-native-turn", State: runtimecontract.LifecycleCompleted,
		Content: []runtimecontract.ContentBlock{
			{ID: "user-1", Kind: runtimecontract.ContentUserText, Text: "inspect this"},
			{ID: "reason-1", Kind: runtimecontract.ContentReasoning, Text: "checking"},
			{ID: "tool-1", Kind: runtimecontract.ContentToolCall, ToolCall: &runtimecontract.ToolCall{Name: "read", Arguments: json.RawMessage(`{"path":"README.md"}`)}},
			{ID: "result-1", Kind: runtimecontract.ContentToolResult, ToolResult: &runtimecontract.ToolResult{ToolCallID: "tool-1", Text: "ok", Success: true}},
			{ID: "image-1", Kind: runtimecontract.ContentImage, Image: &runtimecontract.Image{MIMEType: "image/png", Ref: "attachment-1"}},
			{ID: "assistant-1", Kind: runtimecontract.ContentAssistantText, Text: "done"},
		},
		Usage: &runtimecontract.Usage{
			InputTokens: runtimecontract.UsageMetric{Available: true, Value: 10, Source: "native"},
			CostMicros:  runtimecontract.UsageMetric{Available: false, Source: "runtime_unavailable"},
		},
	}}}

	payload, err := json.Marshal(struct {
		Descriptor runtimecontract.CapabilityDescriptor `json:"descriptor"`
		Outcome    runtimecontract.Outcome              `json:"outcome"`
		History    runtimecontract.History              `json:"history"`
	}{descriptor, outcome, history})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["descriptor"].(map[string]any)["alternative"] != "select a vision model" {
		t.Fatalf("Capability descriptor JSON = %s", payload)
	}
	if decoded["outcome"].(map[string]any)["state"] != "indeterminate" {
		t.Fatalf("Lifecycle outcome JSON = %s", payload)
	}
	if strings.Contains(string(payload), "opaque-native-turn") {
		t.Fatalf("canonical JSON leaked native Turn ref: %s", payload)
	}
	turns := decoded["history"].(map[string]any)["turns"].([]any)
	if got := len(turns[0].(map[string]any)["content"].([]any)); got != 6 {
		t.Fatalf("canonical content block count = %d, payload = %s", got, payload)
	}
}

func TestContractV2RejectsContradictoryContentAndLifecycleStates(t *testing.T) {
	valid := runtimecontract.ContentBlock{ID: "tool-1", Kind: runtimecontract.ContentToolCall, ToolCall: &runtimecontract.ToolCall{Name: "read"}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid content: %v", err)
	}
	invalid := runtimecontract.ContentBlock{ID: "bad", Kind: runtimecontract.ContentToolCall, Image: &runtimecontract.Image{MIMEType: "image/png", Ref: "image-1"}}
	if err := invalid.Validate(); err == nil {
		t.Fatal("contradictory content validated")
	}
	for _, outcome := range []runtimecontract.Outcome{
		{State: runtimecontract.LifecycleCompleted, Failure: &runtimecontract.Failure{Code: "unexpected"}},
		{State: runtimecontract.LifecycleFailed},
		{State: runtimecontract.LifecycleIndeterminate, Failure: &runtimecontract.Failure{Code: "timeout", Retryable: true}},
	} {
		if err := outcome.Validate(); err == nil {
			t.Fatalf("invalid outcome validated: %#v", outcome)
		}
	}
}

func TestMandatoryCoreLeavesLegacyControlsToCapabilityMigration(t *testing.T) {
	legacy := []string{"Sandbox", "ApprovalPolicy", "ProviderID", "Model", "Effort", "DisabledSkillPaths"}
	for _, request := range []any{runtimecontract.BindingRequest{}, runtimecontract.TurnRequest{}} {
		typeOf := reflect.TypeOf(request)
		for _, field := range legacy {
			if _, exists := typeOf.FieldByName(field); exists {
				t.Fatalf("mandatory %s still carries legacy control %s", typeOf.Name(), field)
			}
		}
	}
}
