package runtimecontract

import (
	"strings"
	"testing"
)

func TestModelControlStateValidatesCatalogAndPendingSelection(t *testing.T) {
	state := ModelControlState{
		Current: Model{Provider: "fixture", ID: "vision", ThinkingLevels: []string{"low", "high"}, DefaultThinkingLevel: "low", ImageInput: true},
		Models: []Model{
			{Provider: "fixture", ID: "vision", ThinkingLevels: []string{"low", "high"}, DefaultThinkingLevel: "low", ImageInput: true},
			{Provider: "fixture", ID: "text", ThinkingLevels: []string{"off"}, DefaultThinkingLevel: "off"},
		},
		ThinkingLevel: "high",
	}
	if err := state.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := state.ValidateSelection(ModelSelection{Provider: "fixture", Model: "text", ThinkingLevel: "high"}); err == nil || !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("unsupported pending thinking selection error = %v", err)
	}
	if err := state.ValidateSelection(ModelSelection{Provider: "fixture", Model: "missing", ThinkingLevel: "off"}); err == nil || !strings.Contains(err.Error(), "catalog") {
		t.Fatalf("unknown pending model error = %v", err)
	}
	if err := state.ValidateSelection(ModelSelection{Provider: "fixture", Model: "text", ThinkingLevel: "off"}); err != nil {
		t.Fatalf("valid pending selection: %v", err)
	}
}

func TestModelControlStateRejectsAmbiguousCatalogAndInvalidActiveTruth(t *testing.T) {
	for _, test := range []struct {
		name  string
		state ModelControlState
	}{
		{name: "duplicate", state: ModelControlState{Current: Model{Provider: "p", ID: "m"}, Models: []Model{{Provider: "p", ID: "m"}, {Provider: "p", ID: "m"}}}},
		{name: "missing current", state: ModelControlState{Current: Model{Provider: "p", ID: "missing"}, Models: []Model{{Provider: "p", ID: "m"}}}},
		{name: "invalid active thinking", state: ModelControlState{Current: Model{Provider: "p", ID: "m", ThinkingLevels: []string{"off"}, DefaultThinkingLevel: "off"}, Models: []Model{{Provider: "p", ID: "m", ThinkingLevels: []string{"off"}, DefaultThinkingLevel: "off"}}, ThinkingLevel: "high"}},
		{name: "current descriptor mismatch", state: ModelControlState{Current: Model{Provider: "p", ID: "m", ThinkingLevels: []string{"off"}, DefaultThinkingLevel: "off", ImageInput: false}, Models: []Model{{Provider: "p", ID: "m", ThinkingLevels: []string{"off"}, DefaultThinkingLevel: "off", ImageInput: true}}, ThinkingLevel: "off"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.state.Validate(); err == nil {
				t.Fatalf("invalid model-control state accepted: %#v", test.state)
			}
		})
	}
}
