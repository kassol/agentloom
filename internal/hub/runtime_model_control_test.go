package hub

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

type recordingModelControlContract struct {
	*controlPlaneContract
	state       runtimecontract.ModelControlState
	inspectHook func()
	selectHook  func(runtimecontract.ModelSelection) (runtimecontract.ModelControlState, *runtimecontract.Failure)
	selections  []runtimecontract.ModelSelection
}

func TestCodexModelPersistenceFailureRestoresSanitizedRolloutBytes(t *testing.T) {
	const threadID = "model-compensation-thread"
	sessions := t.TempDir()
	day := filepath.Join(sessions, "2026", "08", "11")
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("{\"type\":\"response_item\",\"payload\":{\"type\":\"reasoning\",\"content\":[{\"type\":\"reasoning_text\",\"text\":\"exact private reasoning\"}]}}\n")
	rolloutPath := filepath.Join(day, "rollout-2026-08-11T00-00-00-"+threadID+".jsonl")
	if err := os.WriteFile(rolloutPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SESSIONS_DIR", sessions)

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	contract := &codexRuntimeContract{providerID: "deepseek", model: "deepseek-v4-flash", effort: "max", turnsByNative: map[string]runtimeTurnCorrelation{}, modelCatalog: func() ([]runtimecontract.Model, error) {
		return []runtimecontract.Model{
			{Provider: "deepseek", ID: "deepseek-v4-flash", ThinkingLevels: []string{"max"}, DefaultThinkingLevel: "max"},
			{Provider: "openai", ID: "gpt", ThinkingLevels: []string{runtimecontract.ThinkingLevelDefault}, DefaultThinkingLevel: runtimecontract.ThinkingLevelDefault, ImageInput: true},
		}, nil
	}}
	agent := &Agent{ID: "agent-codex-compensation", Name: "codex", ThreadID: "thread-loom", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "codex", NativeRef: threadID}, ProviderID: "deepseek", Model: "deepseek-v4-flash", Effort: "max", Status: "idle", CreatedAt: now(), UpdatedAt: now()}
	ready := make(chan struct{})
	close(ready)
	h.agents[agent.ID] = agent
	h.runtimes[agent.ID] = &runtime{agentID: agent.ID, runtimeContract: contract, binding: runtimeContractBinding(agent), ready: ready, approvals: map[string]*approval{}}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = h.SwitchRuntimeModel(agent.ID, RuntimeModelSelection{Provider: "openai", Model: "gpt", ThinkingLevel: runtimecontract.ThinkingLevelDefault})
	if err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("persistence compensation error = %v", err)
	}
	restored, readErr := os.ReadFile(rolloutPath)
	if readErr != nil || string(restored) != string(original) {
		t.Fatalf("rollout bytes were not restored exactly: err=%v\n%s", readErr, restored)
	}
}

func (c *recordingModelControlContract) InspectModelControl(context.Context, runtimecontract.Binding) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
	if c.inspectHook != nil {
		c.inspectHook()
	}
	return c.state, nil
}

func (c *recordingModelControlContract) SelectModel(_ context.Context, _ runtimecontract.Binding, selection runtimecontract.ModelSelection) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
	c.selections = append(c.selections, selection)
	if c.selectHook != nil {
		return c.selectHook(selection)
	}
	for _, model := range c.state.Models {
		if model.Provider == selection.Provider && model.ID == selection.Model {
			c.state.Current = model
			c.state.ThinkingLevel = selection.ThinkingLevel
		}
	}
	return c.state, nil
}

func modelControlFailureForTest(message string) *runtimecontract.Failure {
	return &runtimecontract.Failure{Code: "fixture_model_control", Phase: runtimecontract.FailurePhaseModelControl, Message: message}
}

func modelControlTestHub(t *testing.T) (*Hub, *Agent, *recordingModelControlContract) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := testHub(st)
	base := &controlPlaneContract{snapshot: runtimecontract.CapabilitySnapshot{Revision: "model-test", Capabilities: []runtimecontract.CapabilityDescriptor{
		{ID: runtimecontract.CapabilityModelConfiguration, Availability: runtimecontract.CapabilityAvailable, Revision: "model-test"},
		{ID: runtimecontract.CapabilityImageInput, Availability: runtimecontract.CapabilityAvailable, Revision: "model-test"},
	}}}
	contract := &recordingModelControlContract{controlPlaneContract: base, state: runtimecontract.ModelControlState{
		Current: runtimecontract.Model{Provider: "fixture", ID: "vision", ThinkingLevels: []string{"low", "high"}, DefaultThinkingLevel: "low", ImageInput: true},
		Models: []runtimecontract.Model{
			{Provider: "fixture", ID: "vision", ThinkingLevels: []string{"low", "high"}, DefaultThinkingLevel: "low", ImageInput: true},
			{Provider: "fixture", ID: "text", ThinkingLevels: []string{"off"}, DefaultThinkingLevel: "off"},
		},
		ThinkingLevel: "low",
	}}
	agent := &Agent{ID: "agent-model", Name: "model-agent", ThreadID: "thread-model", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "native-model"}, ProviderID: "fixture", Model: "vision", Effort: "low", Status: "idle", CreatedAt: now(), UpdatedAt: now()}
	ready := make(chan struct{})
	close(ready)
	h.agents[agent.ID] = agent
	h.runtimes[agent.ID] = &runtime{agentID: agent.ID, runtimeContract: contract, binding: runtimeContractBinding(agent), ready: ready, approvals: map[string]*approval{}}
	return h, agent, contract
}

func TestRuntimeModelSelectionValidatesPendingTruthBeforeMutation(t *testing.T) {
	h, agent, contract := modelControlTestHub(t)
	for _, selection := range []runtimecontract.ModelSelection{
		{Provider: "fixture", Model: "missing", ThinkingLevel: "off"},
		{Provider: "fixture", Model: "text", ThinkingLevel: "high"},
	} {
		if _, err := h.SwitchRuntimeModel(agent.ID, selection); err == nil {
			t.Fatalf("invalid pending selection accepted: %#v", selection)
		}
	}
	if len(contract.selections) != 0 || agent.Model != "vision" || agent.Effort != "low" {
		t.Fatalf("invalid pending selection mutated Runtime or Agent: selections=%#v Agent=%#v", contract.selections, agent)
	}
}

func TestRuntimeModelSelectionRejectsStaleInspectorResult(t *testing.T) {
	h, agent, contract := modelControlTestHub(t)
	contract.inspectHook = func() {
		h.mu.Lock()
		agent.Model = "changed-concurrently"
		h.mu.Unlock()
		contract.inspectHook = nil
	}
	_, err := h.SwitchRuntimeModel(agent.ID, runtimecontract.ModelSelection{Provider: "fixture", Model: "text", ThinkingLevel: "off"})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("stale model-control preview error = %v", err)
	}
	if len(contract.selections) != 0 {
		t.Fatalf("stale preview reached Runtime selection: %#v", contract.selections)
	}
}

func TestRuntimeModelSelectionRequiresPausedGoalAndResolvedApproval(t *testing.T) {
	for _, test := range []struct {
		name  string
		guard func(*Hub, *Agent)
		want  string
	}{
		{name: "active goal", want: "pause the active Goal", guard: func(h *Hub, agent *Agent) {
			h.goals[agent.ID] = &ThreadGoal{ThreadID: agent.ThreadID, Status: GoalStatusActive}
		}},
		{name: "pending approval", want: "pending approval", guard: func(h *Hub, agent *Agent) {
			h.runtimes[agent.ID].approvals["approval-1"] = &approval{}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, agent, contract := modelControlTestHub(t)
			test.guard(h, agent)
			_, err := h.SwitchRuntimeModel(agent.ID, runtimecontract.ModelSelection{Provider: "fixture", Model: "text", ThinkingLevel: "off"})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("guard error = %v", err)
			}
			if len(contract.selections) != 0 {
				t.Fatalf("guard reached native selection: %#v", contract.selections)
			}
		})
	}
}

func TestRuntimeModelSelectionRevalidatesGoalAfterPreview(t *testing.T) {
	h, agent, contract := modelControlTestHub(t)
	contract.inspectHook = func() {
		h.mu.Lock()
		h.goals[agent.ID] = &ThreadGoal{ThreadID: agent.ThreadID, Status: GoalStatusActive}
		h.mu.Unlock()
		contract.inspectHook = nil
	}
	_, err := h.SwitchRuntimeModel(agent.ID, runtimecontract.ModelSelection{Provider: "fixture", Model: "text", ThinkingLevel: "off"})
	if err == nil || !strings.Contains(err.Error(), "pause the active Goal") {
		t.Fatalf("preview Goal race error = %v", err)
	}
	if len(contract.selections) != 0 {
		t.Fatalf("preview Goal race reached native selection: %#v", contract.selections)
	}
}

func TestRuntimeModelSelectionCompensatesWhenGoalActivatesDuringNativeSelect(t *testing.T) {
	h, agent, contract := modelControlTestHub(t)
	calls := 0
	contract.selectHook = func(selection runtimecontract.ModelSelection) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
		calls++
		for _, model := range contract.state.Models {
			if model.Provider == selection.Provider && model.ID == selection.Model {
				contract.state.Current = model
				contract.state.ThinkingLevel = selection.ThinkingLevel
			}
		}
		if calls == 1 {
			h.mu.Lock()
			h.goals[agent.ID] = &ThreadGoal{ThreadID: agent.ThreadID, Status: GoalStatusActive}
			h.mu.Unlock()
		}
		return contract.state, nil
	}
	_, err := h.SwitchRuntimeModel(agent.ID, runtimecontract.ModelSelection{Provider: "fixture", Model: "text", ThinkingLevel: "off"})
	if err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("final Goal race error = %v", err)
	}
	if len(contract.selections) != 2 || agent.Model != "vision" {
		t.Fatalf("final Goal race was not compensated: selections=%#v agent=%#v", contract.selections, agent)
	}
}

func TestRuntimeModelSelectionRestoresRuntimeWhenPersistenceFails(t *testing.T) {
	h, agent, contract := modelControlTestHub(t)
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := h.SwitchRuntimeModel(agent.ID, runtimecontract.ModelSelection{Provider: "fixture", Model: "text", ThinkingLevel: "off"})
	if err == nil || !strings.Contains(err.Error(), "save Runtime model selection") {
		t.Fatalf("persistence failure = %v", err)
	}
	if len(contract.selections) != 2 || contract.selections[0].Model != "text" || contract.selections[1].Model != "vision" {
		t.Fatalf("Runtime selection was not compensated: %#v", contract.selections)
	}
	if agent.ProviderID != "fixture" || agent.Model != "vision" || agent.Effort != "low" {
		t.Fatalf("failed persistence changed active Agent selection: %#v", agent)
	}
}

func TestRuntimeModelSelectionRestoresExactPreviousStateAfterInvalidNativeSuccess(t *testing.T) {
	h, agent, contract := modelControlTestHub(t)
	calls := 0
	contract.selectHook = func(selection runtimecontract.ModelSelection) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
		calls++
		if calls == 1 {
			return runtimecontract.ModelControlState{Current: runtimecontract.Model{Provider: "fixture", ID: "wrong"}}, nil
		}
		contract.state.Current = contract.state.Models[0]
		contract.state.ThinkingLevel = selection.ThinkingLevel
		return contract.state, nil
	}
	_, err := h.SwitchRuntimeModel(agent.ID, runtimecontract.ModelSelection{Provider: "fixture", Model: "text", ThinkingLevel: "off"})
	if err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("invalid native success error = %v", err)
	}
	if len(contract.selections) != 2 || contract.selections[1] != (runtimecontract.ModelSelection{Provider: "fixture", Model: "vision", ThinkingLevel: "low"}) {
		t.Fatalf("invalid native success was not rolled back exactly: %#v", contract.selections)
	}
	if h.runtimes[agent.ID].effectDomainInvalidated {
		t.Fatal("successful exact rollback fenced Runtime")
	}
}

func TestRuntimeModelSelectionRestoresExactPreviousStateAfterNativeSelectionMismatch(t *testing.T) {
	h, agent, contract := modelControlTestHub(t)
	calls := 0
	contract.selectHook = func(selection runtimecontract.ModelSelection) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
		calls++
		if calls == 1 {
			// Structurally valid, but the Runtime did not apply the requested selection.
			return contract.state, nil
		}
		contract.state.Current = contract.state.Models[0]
		contract.state.ThinkingLevel = selection.ThinkingLevel
		return contract.state, nil
	}
	_, err := h.SwitchRuntimeModel(agent.ID, runtimecontract.ModelSelection{Provider: "fixture", Model: "text", ThinkingLevel: "off"})
	if err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("native selection mismatch error = %v", err)
	}
	if len(contract.selections) != 2 || contract.selections[1] != (runtimecontract.ModelSelection{Provider: "fixture", Model: "vision", ThinkingLevel: "low"}) {
		t.Fatalf("native selection mismatch was not rolled back exactly: %#v", contract.selections)
	}
}

func TestRuntimeModelSelectionFencesIndeterminateRollback(t *testing.T) {
	for _, test := range []struct {
		name     string
		rollback func(runtimecontract.ModelControlState) (runtimecontract.ModelControlState, *runtimecontract.Failure)
	}{
		{name: "failure", rollback: func(runtimecontract.ModelControlState) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
			return runtimecontract.ModelControlState{}, modelControlFailureForTest("rollback failed")
		}},
		{name: "mismatch", rollback: func(state runtimecontract.ModelControlState) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
			state.Current = state.Models[1]
			state.ThinkingLevel = "off"
			return state, nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, agent, contract := modelControlTestHub(t)
			calls := 0
			contract.selectHook = func(selection runtimecontract.ModelSelection) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
				calls++
				if calls == 1 {
					return runtimecontract.ModelControlState{Current: runtimecontract.Model{Provider: "fixture", ID: "wrong"}}, nil
				}
				return test.rollback(contract.state)
			}
			_, err := h.SwitchRuntimeModel(agent.ID, runtimecontract.ModelSelection{Provider: "fixture", Model: "text", ThinkingLevel: "off"})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "indeterminate") || !strings.Contains(strings.ToLower(err.Error()), "fenced") {
				t.Fatalf("rollback %s error = %v", test.name, err)
			}
			if !h.runtimes[agent.ID].effectDomainInvalidated || contract.closeCalls != 1 {
				t.Fatalf("rollback %s did not fence/close Runtime: fenced=%v close=%d", test.name, h.runtimes[agent.ID].effectDomainInvalidated, contract.closeCalls)
			}
		})
	}
}

func TestRuntimeModelSelectionFailureFencesRuntimeBeforePersist(t *testing.T) {
	h, agent, contract := modelControlTestHub(t)
	contract.selectHook = func(runtimecontract.ModelSelection) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
		return runtimecontract.ModelControlState{}, &runtimecontract.Failure{
			Code: "model_selection_indeterminate", Phase: runtimecontract.FailurePhaseModelControl,
			Message: "native write completion unknown",
		}
	}
	_, err := h.SwitchRuntimeModel(agent.ID, runtimecontract.ModelSelection{Provider: "fixture", Model: "text", ThinkingLevel: "off"})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "indeterminate") {
		t.Fatalf("indeterminate native failure = %v", err)
	}
	if !h.runtimes[agent.ID].effectDomainInvalidated || contract.closeCalls != 1 {
		t.Fatalf("indeterminate native failure did not fence Runtime: fenced=%v close=%d", h.runtimes[agent.ID].effectDomainInvalidated, contract.closeCalls)
	}
	if agent.Model != "vision" || agent.Effort != "low" {
		t.Fatalf("indeterminate native failure mutated durable Agent: %#v", agent)
	}
}

func TestRuntimeModelProviderChangeAppendsHistoryAndClearsUsageCache(t *testing.T) {
	h, agent, contract := modelControlTestHub(t)
	contract.state.Models = append(contract.state.Models, runtimecontract.Model{Provider: "other", ID: "vision-2", ThinkingLevels: []string{"low"}, DefaultThinkingLevel: "low", ImageInput: true})
	agentUsageCache.Lock()
	agentUsageCache.entries["stale"] = agentUsageCacheEntry{}
	agentUsageCache.Unlock()
	if _, err := h.SwitchRuntimeModel(agent.ID, runtimecontract.ModelSelection{Provider: "fixture", Model: "vision", ThinkingLevel: "high"}); err != nil {
		t.Fatal(err)
	}
	if len(agent.ProviderHistory) != 0 {
		t.Fatalf("thinking-only selection appended Provider history: %#v", agent.ProviderHistory)
	}
	if _, err := h.SwitchRuntimeModel(agent.ID, runtimecontract.ModelSelection{Provider: "other", Model: "vision-2", ThinkingLevel: "low"}); err != nil {
		t.Fatal(err)
	}
	if len(agent.ProviderHistory) != 1 || agent.ProviderHistory[0].PreviousProviderID != "fixture" || agent.ProviderHistory[0].ProviderID != "other" {
		t.Fatalf("Provider history = %#v", agent.ProviderHistory)
	}
	agentUsageCache.Lock()
	cacheLen := len(agentUsageCache.entries)
	agentUsageCache.Unlock()
	if cacheLen != 0 {
		t.Fatalf("usage cache retained %d stale entries after Provider change", cacheLen)
	}
}
