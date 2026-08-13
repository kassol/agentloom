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

func TestUsageReportRejectsUnsourcedOrFabricatedUnavailableMetrics(t *testing.T) {
	unavailable := runtimecontract.UsageMetric{Source: "runtime_unavailable"}
	usage := runtimecontract.Usage{
		InputTokens: unavailable, CachedInputTokens: unavailable, OutputTokens: unavailable,
		ReasoningOutputTokens: unavailable, TotalTokens: unavailable, Calls: unavailable, CostMicros: unavailable,
	}
	report := runtimecontract.UsageReport{
		Lifetime: usage, LatestCall: usage,
		LatestProvider:     runtimecontract.UsageText{Source: "runtime_unavailable"},
		LatestModel:        runtimecontract.UsageText{Source: "runtime_unavailable"},
		ContextInputTokens: unavailable, ModelContextWindow: unavailable,
		LastUpdatedAt: runtimecontract.UsageText{Source: "runtime_unavailable"},
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("truthful unavailable report: %v", err)
	}
	report.Lifetime.Calls.Value = 1
	if err := report.Validate(); err == nil {
		t.Fatal("unavailable calls accepted a fabricated value")
	}
	report.Lifetime.Calls.Value = 0
	report.Activity = []runtimecontract.TurnActivity{{
		TurnID: runtimecontract.UsageText{Source: "runtime_unavailable"}, StartedAt: runtimecontract.UsageText{Source: "runtime_unavailable"},
		EndedAt: runtimecontract.UsageText{Source: "runtime_unavailable"}, Status: runtimecontract.UsageText{Source: "runtime_unavailable"},
		InferredEnd: runtimecontract.UsageBool{Value: true, Source: "runtime_unavailable"},
	}}
	if err := report.Validate(); err == nil {
		t.Fatal("unavailable inferred end accepted a fabricated value")
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
	for _, invalid := range []runtimecontract.ContentBlock{
		{ID: "empty-image", Kind: runtimecontract.ContentImage, Image: &runtimecontract.Image{MIMEType: "image/png"}},
		{ID: "empty-attachment", Kind: runtimecontract.ContentAttachment, Attachment: &runtimecontract.Attachment{Name: "notes.txt"}},
		{ID: "long-description", Kind: runtimecontract.ContentToolCall, ToolCall: &runtimecontract.ToolCall{Name: "exec_command", Description: strings.Repeat("界", 251)}},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("payload without public reference validated: %#v", invalid)
		}
	}
	for _, outcome := range []runtimecontract.Outcome{
		{State: runtimecontract.LifecycleCompleted, Failure: &runtimecontract.Failure{Code: "unexpected"}},
		{State: runtimecontract.LifecycleInterrupted, Failure: &runtimecontract.Failure{Code: "unexpected"}},
		{State: runtimecontract.LifecycleFailed},
		{State: runtimecontract.LifecycleIndeterminate},
		{State: runtimecontract.LifecycleIndeterminate, Failure: &runtimecontract.Failure{Code: "timeout", Retryable: true}},
		{State: runtimecontract.LifecycleFailed, Failure: &runtimecontract.Failure{Phase: runtimecontract.FailurePhaseTurnStart, Message: "failed"}},
		{State: runtimecontract.LifecycleFailed, Failure: &runtimecontract.Failure{Code: "failed", Phase: runtimecontract.FailurePhaseTurnStart}},
		{State: runtimecontract.LifecycleFailed, Failure: &runtimecontract.Failure{Code: "failed", Phase: "unknown", Message: "failed"}},
	} {
		if err := outcome.Validate(); err == nil {
			t.Fatalf("invalid outcome validated: %#v", outcome)
		}
	}
}

func TestMandatoryCoreLeavesLegacyControlsToCapabilityMigration(t *testing.T) {
	legacy := map[string]bool{"Options": true, "Policy": true, "Sandbox": true, "ApprovalPolicy": true, "ProviderID": true, "Model": true, "Effort": true, "DisabledSkillPaths": true}
	seen := map[reflect.Type]bool{}
	var inspect func(reflect.Type)
	inspect = func(typeOf reflect.Type) {
		for typeOf.Kind() == reflect.Pointer || typeOf.Kind() == reflect.Slice || typeOf.Kind() == reflect.Array {
			typeOf = typeOf.Elem()
		}
		if typeOf.Kind() != reflect.Struct || seen[typeOf] {
			return
		}
		seen[typeOf] = true
		for index := 0; index < typeOf.NumField(); index++ {
			field := typeOf.Field(index)
			if legacy[field.Name] {
				t.Fatalf("mandatory request graph %s still carries legacy control %s", typeOf.Name(), field.Name)
			}
			inspect(field.Type)
		}
	}
	for _, request := range []any{runtimecontract.BindingRequest{}, runtimecontract.TurnRequest{}} {
		inspect(reflect.TypeOf(request))
	}
}

func TestCapabilitySnapshotValidateRejectsMalformedDescriptors(t *testing.T) {
	valid := runtimecontract.CapabilitySnapshot{Revision: "driver-1", Capabilities: []runtimecontract.CapabilityDescriptor{{
		ID: "history", Availability: runtimecontract.CapabilityAvailable, Revision: "cap-1",
	}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid snapshot: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*runtimecontract.CapabilitySnapshot)
	}{
		{name: "snapshot revision", mutate: func(snapshot *runtimecontract.CapabilitySnapshot) { snapshot.Revision = "" }},
		{name: "descriptor id", mutate: func(snapshot *runtimecontract.CapabilitySnapshot) { snapshot.Capabilities[0].ID = "" }},
		{name: "descriptor revision", mutate: func(snapshot *runtimecontract.CapabilitySnapshot) { snapshot.Capabilities[0].Revision = "" }},
		{name: "availability", mutate: func(snapshot *runtimecontract.CapabilitySnapshot) { snapshot.Capabilities[0].Availability = "maybe" }},
		{name: "duplicate", mutate: func(snapshot *runtimecontract.CapabilitySnapshot) {
			snapshot.Capabilities = append(snapshot.Capabilities, snapshot.Capabilities[0])
		}},
		{name: "unavailable reason", mutate: func(snapshot *runtimecontract.CapabilitySnapshot) {
			snapshot.Capabilities[0].Availability = runtimecontract.CapabilityUnavailable
			snapshot.Capabilities[0].Alternative = "use history"
		}},
		{name: "unavailable alternative", mutate: func(snapshot *runtimecontract.CapabilitySnapshot) {
			snapshot.Capabilities[0].Availability = runtimecontract.CapabilityUnavailable
			snapshot.Capabilities[0].Reason = "not supported"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := valid
			snapshot.Capabilities = append([]runtimecontract.CapabilityDescriptor(nil), valid.Capabilities...)
			test.mutate(&snapshot)
			if err := snapshot.Validate(); err == nil {
				t.Fatal("malformed capability snapshot validated")
			}
		})
	}
}

func TestResourceInventoryValidateRejectsNativeResourceAmbiguity(t *testing.T) {
	valid := runtimecontract.ResourceInventory{
		Revision:  "native-1",
		Semantics: "Runtime-specific resources; no cross-Runtime portability",
		Resources: []runtimecontract.Resource{{
			ID: "skill:/tmp/review/SKILL.md", Name: "review", Kind: runtimecontract.ResourceSkill,
			Path: "/tmp/review/SKILL.md", Scope: "user", Enabled: true,
		}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	pathless := runtimecontract.ResourceInventory{
		Revision:  "claude-reload-1",
		Semantics: "Claude-native resources are read-only and their filesystem paths are intentionally withheld",
		Resources: []runtimecontract.Resource{{
			ID: "skill:review", Name: "review", Kind: runtimecontract.ResourceSkill,
			Source: "claude_agent_sdk_reload", Enabled: true,
		}},
	}
	if err := pathless.Validate(); err != nil {
		t.Fatalf("pathless native resource inventory: %v", err)
	}
	for name, mutate := range map[string]func(*runtimecontract.ResourceInventory){
		"revision":  func(value *runtimecontract.ResourceInventory) { value.Revision = "" },
		"semantics": func(value *runtimecontract.ResourceInventory) { value.Semantics = "" },
		"duplicate": func(value *runtimecontract.ResourceInventory) {
			value.Resources = append(value.Resources, value.Resources[0])
		},
		"kind": func(value *runtimecontract.ResourceInventory) { value.Resources[0].Kind = "builtin" },
	} {
		t.Run(name, func(t *testing.T) {
			value := valid
			value.Resources = append([]runtimecontract.Resource(nil), valid.Resources...)
			mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatalf("Validate accepted %#v", value)
			}
		})
	}
}

func TestResourcePolicyStateValidateRequiresEffectiveEvidence(t *testing.T) {
	state := runtimecontract.ResourcePolicyState{Revision: "policy-1", Effective: true}
	if err := state.Validate(); err == nil || !strings.Contains(err.Error(), "evidence") {
		t.Fatalf("effective policy without evidence error = %v", err)
	}
	state.Evidence = []runtimecontract.CapabilityEvidence{{Kind: "native_ack", Summary: "native lifecycle acknowledged exact policy"}}
	if err := state.Validate(); err != nil {
		t.Fatalf("valid resource policy: %v", err)
	}
}
