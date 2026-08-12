package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/claudebridge"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestClaudePassiveContextAndUsageReadCanonicalLedgerWithoutRuntime(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	developer := `<loom_developer_context version="1" prompt_revision="owner:3" prompt_hash="prompt-sha" profile_revision="profile:7" profile_hash="profile-sha"><loom_agent_profile_data /></loom_developer_context>`
	input := `<loom_context version="1"><loom_agent_relationships revision="relationships:abc" hash="relationships-sha"/><loom_agent_goal id="goal-1" revision="4" status="active"/><loom_turn_context kind="needs_you_answer" ref_id="hrq-1" topic_id="topic-1"/></loom_context>`
	source := "claude_agent_sdk"
	if err := st.SaveCanonicalTurnLedger("agent-claude", []runtimecontract.HistoryTurn{{
		TurnID: "turn-loom", State: runtimecontract.LifecycleCompleted,
		StartedAt: "2026-08-12T01:00:00Z", CompletedAt: "2026-08-12T01:00:02Z",
		Content: []runtimecontract.ContentBlock{},
		Usage: &runtimecontract.Usage{
			InputTokens:           runtimecontract.UsageMetric{Available: true, Value: 11, Source: source},
			CachedInputTokens:     runtimecontract.UsageMetric{Source: source},
			OutputTokens:          runtimecontract.UsageMetric{Available: true, Value: 5, Source: source},
			ReasoningOutputTokens: runtimecontract.UsageMetric{Source: source},
			TotalTokens:           runtimecontract.UsageMetric{Source: source}, Calls: runtimecontract.UsageMetric{Source: source},
			CostMicros: runtimecontract.UsageMetric{Available: true, Value: 42, Source: source},
		},
		UsageDetails: &runtimecontract.UsageDetails{
			ObservedAt: runtimecontract.UsageText{Available: true, Value: "2026-08-12T01:00:01Z", Source: "canonical_turn_ledger"},
			Models: []runtimecontract.ModelUsage{{Provider: runtimecontract.UsageText{Source: source}, Model: runtimecontract.UsageText{Available: true, Value: "claude-sonnet", Source: source}, Usage: runtimecontract.Usage{
				InputTokens: runtimecontract.UsageMetric{Available: true, Value: 11, Source: source}, CachedInputTokens: runtimecontract.UsageMetric{Source: source}, OutputTokens: runtimecontract.UsageMetric{Available: true, Value: 5, Source: source}, ReasoningOutputTokens: runtimecontract.UsageMetric{Source: source}, TotalTokens: runtimecontract.UsageMetric{Source: source}, Calls: runtimecontract.UsageMetric{Source: source}, CostMicros: runtimecontract.UsageMetric{Available: true, Value: 42, Source: source},
			}, ContextWindow: runtimecontract.UsageMetric{Available: true, Value: 200000, Source: source}}},
		},
		ContextEvidence: &runtimecontract.ContextEvidence{
			State: runtimecontract.ContextEvidenceProven, TurnID: "turn-loom", Mode: runtimecontract.ContextDeliveryFullPerTurn,
			Sources: []runtimecontract.ContextEvidenceSource{
				{Key: "loom_agent_prompt", Revision: "owner:3", Hash: "prompt-sha", Channel: "developer", State: "delivered"}, {Key: "loom_agent_profile", Revision: "profile:7", Hash: "profile-sha", Channel: "developer", State: "delivered"}, {Key: "loom_agent_relationships", Revision: "relationships:abc", Hash: "relationships-sha", Channel: "input", State: "delivered"},
				{Key: "loom_agent_goal", Revision: "goal:goal-1:4", Channel: "input", State: "delivered"}, {Key: "turn_source", Revision: "needs_you_answer:hrq-1", Channel: "input", State: "delivered"}, {Key: "needs_you", Revision: "hrq-1", Channel: "input", State: "delivered"}, {Key: "topic", Revision: "topic-1", Channel: "input", State: "delivered"},
			},
			Deliveries:            []runtimecontract.ContextEvidenceDelivery{{Channel: "developer", Role: "developer", Hash: sha256Hex([]byte(developer)), Content: developer}, {Channel: "input", Role: "user", Hash: sha256Hex([]byte(input)), Content: input}},
			UnsupportedDimensions: []string{"coverage", "epoch", "replay", "resend"},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resolved := 0
	driver := newClaudeRuntimeHostDriver(reopened, claudebridge.DriverOptions{ResolveActive: func(context.Context) (claudebridge.LaunchSpec, error) {
		resolved++
		return claudebridge.LaunchSpec{}, nil
	}})
	contract := driver.HistoryContract(AgentHostRequest{AgentID: "agent-claude"})
	binding := runtimecontract.Binding{SchemaVersion: 2, RuntimeKind: "claude", NativeRef: "private-session"}
	contextReport, failure := contract.(runtimecontract.ContextEvidenceCapability).InspectContextEvidence(context.Background(), binding, runtimecontract.ContextEvidenceQuery{TurnID: "turn-loom", RuntimeTurnRef: "private-turn"})
	if failure != nil || contextReport.State != runtimecontract.ContextEvidenceProven || len(contextReport.Sources) != 7 || len(contextReport.Deliveries) != 2 {
		t.Fatalf("cold Claude context = %#v, failure=%v", contextReport, failure)
	}
	usage, failure := contract.(runtimecontract.UsageInspectionCapability).InspectUsage(context.Background(), binding)
	if failure != nil || usage.Lifetime.InputTokens.Value != 11 || usage.Lifetime.CachedInputTokens.Available || usage.LatestModel.Value != "claude-sonnet" || usage.LatestProvider.Available || len(usage.Activity) != 1 {
		t.Fatalf("cold Claude usage = %#v, failure=%v", usage, failure)
	}
	encoded, _ := json.Marshal(struct {
		Context runtimecontract.ContextEvidence `json:"context"`
		Usage   runtimecontract.UsageReport     `json:"usage"`
	}{contextReport, usage})
	if strings.Contains(string(encoded), "private-") || resolved != 0 {
		t.Fatalf("passive evidence leaked native identity or started Runtime: resolved=%d payload=%s", resolved, encoded)
	}
}

func TestClaudeUsageEventPreservesUnavailableNativeFields(t *testing.T) {
	event, err := claudeContractEvent(claudebridge.Event{Kind: "usage", TurnID: "turn-1", Data: json.RawMessage(`{"runtimeTurnRef":"native-secret","usage":{"inputTokens":{"available":true,"value":3,"source":"claude_agent_sdk"},"cachedInputTokens":{"available":false,"source":"claude_agent_sdk"},"outputTokens":{"available":true,"value":1,"source":"claude_agent_sdk"},"reasoningOutputTokens":{"available":false,"source":"claude_agent_sdk"},"totalTokens":{"available":false,"source":"claude_agent_sdk"},"calls":{"available":false,"source":"claude_agent_sdk"},"costMicros":{"available":false,"source":"claude_agent_sdk"}},"details":{"observedAt":{"available":true,"value":"2026-08-12T01:00:00Z","source":"claude_agent_sdk"},"models":[{"provider":{"available":false,"source":"claude_agent_sdk"},"model":{"available":true,"value":"claude-sonnet","source":"claude_agent_sdk"},"usage":{"inputTokens":{"available":true,"value":3,"source":"claude_agent_sdk"},"cachedInputTokens":{"available":false,"source":"claude_agent_sdk"},"outputTokens":{"available":true,"value":1,"source":"claude_agent_sdk"},"reasoningOutputTokens":{"available":false,"source":"claude_agent_sdk"},"totalTokens":{"available":false,"source":"claude_agent_sdk"},"calls":{"available":false,"source":"claude_agent_sdk"},"costMicros":{"available":false,"source":"claude_agent_sdk"}},"contextWindow":{"available":false,"source":"claude_agent_sdk"}}]}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if event.Usage == nil || event.Usage.CachedInputTokens.Available || event.Usage.TotalTokens.Available || event.Usage.CostMicros.Available || event.UsageDetails == nil || len(event.UsageDetails.Models) != 1 || event.UsageDetails.Models[0].Provider.Available || event.UsageDetails.Models[0].Model.Value != "claude-sonnet" {
		t.Fatalf("Claude partial usage event = %#v", event)
	}
	encoded, _ := json.Marshal(event)
	if strings.Contains(string(encoded), "native-secret") {
		t.Fatalf("canonical SSE event leaked Runtime Turn ref: %s", encoded)
	}
}

func TestClaudeCumulativeUsageSnapshotsReplaceInsteadOfSum(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := newClaudeRuntimeContract("agent-claude", st, nil)
	c.handleBridgeEvent(claudebridge.Event{Kind: "turn_started", TurnID: "turn-1", Operation: "op-1", Data: json.RawMessage(`{"runtimeTurnRef":"native"}`)})
	frame := func(input int) claudebridge.Event {
		return claudebridge.Event{Kind: "usage", TurnID: "turn-1", Data: json.RawMessage(fmt.Sprintf(`{"runtimeTurnRef":"native","usage":{"inputTokens":{"available":true,"value":%d,"source":"claude_agent_sdk"},"cachedInputTokens":{"available":true,"value":0,"source":"claude_agent_sdk"},"outputTokens":{"available":true,"value":1,"source":"claude_agent_sdk"},"reasoningOutputTokens":{"available":false,"source":"claude_agent_sdk"},"totalTokens":{"available":false,"source":"claude_agent_sdk"},"calls":{"available":false,"source":"claude_agent_sdk"},"costMicros":{"available":true,"value":1,"source":"claude_agent_sdk"}}}`, input))}
	}
	c.handleBridgeEvent(frame(3))
	c.handleBridgeEvent(frame(7))
	report, failure := c.InspectUsage(context.Background(), runtimecontract.Binding{})
	if failure != nil || report.Lifetime.InputTokens.Value != 7 || len(report.Events) != 1 {
		t.Fatalf("cumulative usage was summed: %#v failure=%v", report, failure)
	}
}

func TestClaudeContextEvidenceRejectsStaleTurnStartCorrelation(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := newClaudeRuntimeContract("agent-claude", st, nil)
	developer := `<loom_developer_context prompt_revision="owner:1" prompt_hash="p" profile_revision="profile:1" profile_hash="q"></loom_developer_context>`
	input := `<loom_context><loom_agent_relationships revision="relationships:1" hash="r"/></loom_context>`
	c.pendingContext["turn-1"] = claudeContextEvidence("turn-1", []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleDeveloper, Text: developer}, {Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleUser, Text: input}}, "developer")
	c.opTurns["op-current"] = "turn-1"
	c.handleBridgeEvent(claudebridge.Event{Kind: "turn_started", TurnID: "turn-1", Operation: "op-stale", Data: json.RawMessage(`{"runtimeTurnRef":"native-stale"}`)})
	report, failure := c.InspectContextEvidence(context.Background(), runtimecontract.Binding{}, runtimecontract.ContextEvidenceQuery{TurnID: "turn-1"})
	if failure != nil || report.State != runtimecontract.ContextEvidenceUnknown || len(report.Deliveries) != 0 {
		t.Fatalf("stale Claude Turn correlation = %#v, failure=%v", report, failure)
	}
}

func TestClaudeAcceptedTurnPersistsContextEvidenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	h.runtimeHostDrivers["claude"] = fakeClaudeBridgeDriver(t, st)
	agent, err := h.CreateAgent(CreateParams{Name: "context-claude", Cwd: t.TempDir(), RuntimeKind: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.SendTask(agent.ID, "inspect context", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	view, err := h.ExplainTurnContext(agent.ID, result.TurnID)
	if err != nil || view.State != runtimecontract.ContextEvidenceProven || len(view.Deliveries) != 2 {
		t.Fatalf("live accepted context = %#v, err=%v", view, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		history, historyErr := h.CanonicalHistory(agent.ID, 10, 0)
		if historyErr == nil && len(history.Turns) == 1 && history.Turns[0].State == runtimecontract.LifecycleCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if view.Deliveries[0].Role != "developer" || view.Deliveries[1].Role != "user" {
		t.Fatalf("context delivery roles = %#v", view.Deliveries)
	}
	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	h2 := New(reopened)
	resolved := 0
	h2.runtimeHostDrivers["claude"] = newClaudeRuntimeHostDriver(reopened, claudebridge.DriverOptions{ResolveActive: func(context.Context) (claudebridge.LaunchSpec, error) {
		resolved++
		return claudebridge.LaunchSpec{}, nil
	}})
	reopenedView, err := h2.ExplainTurnContext(agent.ID, result.TurnID)
	if err != nil || reopenedView.State != runtimecontract.ContextEvidenceProven || resolved != 0 {
		t.Fatalf("reopened context = %#v, resolved=%d, err=%v", reopenedView, resolved, err)
	}
}

func TestUsageAggregateMarksMissingAgentCoveragePartial(t *testing.T) {
	observed := runtimecontract.UsageReport{Lifetime: runtimecontract.Usage{InputTokens: runtimecontract.UsageMetric{Available: true, Value: 3, Source: "observed"}}}
	for _, metric := range []*runtimecontract.UsageMetric{&observed.Lifetime.CachedInputTokens, &observed.Lifetime.OutputTokens, &observed.Lifetime.ReasoningOutputTokens, &observed.Lifetime.TotalTokens, &observed.Lifetime.Calls, &observed.Lifetime.CostMicros} {
		metric.Source = "runtime_unavailable"
	}
	start := mustUsageTime(t, "2026-08-12T00:00:00Z")
	end := mustUsageTime(t, "2026-08-13T00:00:00Z")
	first := buildAgentUsageRange(AgentView{Agent: Agent{ID: "observed", Name: "Observed"}}, start, end, start.Add(time.Hour), &observed)
	missing := buildAgentUsageRange(AgentView{Agent: Agent{ID: "missing", Name: "Missing"}}, start, end, start.Add(time.Hour), nil)
	total := RuntimeTokenUsage{}
	total.Add(first.Lifetime)
	total.Add(missing.Lifetime)
	coverage := total.Metrics["inputTokens"]
	if total.InputTokens != 3 || !coverage.Available || coverage.Complete || !containsUsageSource(coverage.Sources, "runtime_unavailable") {
		t.Fatalf("mixed observed/missing aggregate = %#v", total)
	}
}

func TestClaudeMultiModelLifetimePreservesObservedSubsetAsPartial(t *testing.T) {
	source := "claude_agent_sdk"
	unavailable := runtimecontract.UsageMetric{Source: source}
	usage := func(input runtimecontract.UsageMetric) runtimecontract.Usage {
		return runtimecontract.Usage{InputTokens: input, CachedInputTokens: unavailable, OutputTokens: unavailable, ReasoningOutputTokens: unavailable, TotalTokens: unavailable, Calls: unavailable, CostMicros: unavailable}
	}
	report := runtimecontract.UsageReport{
		Lifetime: usage(runtimecontract.UsageMetric{Available: true, Value: 3, Source: source}),
		Events: []runtimecontract.UsageEvent{
			{Timestamp: runtimecontract.UsageText{Available: true, Value: "2026-08-12T01:00:00Z", Source: "canonical_turn_ledger"}, TurnID: runtimecontract.UsageText{Available: true, Value: "turn-1", Source: "canonical_turn_ledger"}, Provider: runtimecontract.UsageText{Source: source}, Model: runtimecontract.UsageText{Available: true, Value: "claude-main", Source: source}, Usage: usage(runtimecontract.UsageMetric{Available: true, Value: 3, Source: source})},
			{Timestamp: runtimecontract.UsageText{Available: true, Value: "2026-08-12T01:00:00Z", Source: "canonical_turn_ledger"}, TurnID: runtimecontract.UsageText{Available: true, Value: "turn-1", Source: "canonical_turn_ledger"}, Provider: runtimecontract.UsageText{Source: source}, Model: runtimecontract.UsageText{Available: true, Value: "claude-side", Source: source}, Usage: usage(unavailable)},
		},
	}
	start := mustUsageTime(t, "2026-08-12T00:00:00Z")
	public := buildAgentUsageRange(AgentView{Agent: Agent{ID: "claude", Name: "Claude"}}, start, start.Add(24*time.Hour), start.Add(2*time.Hour), &report)
	coverage := public.Lifetime.Metrics["inputTokens"]
	if public.Lifetime.InputTokens != 3 || !coverage.Available || coverage.Complete || public.LatestModel != "" {
		t.Fatalf("multi-model partial lifetime = %#v", public)
	}
}

func TestClaudeMultiModelUsageLeavesSingularAttributionUnavailable(t *testing.T) {
	source := "claude_agent_sdk"
	unavailable := runtimecontract.UsageMetric{Source: source}
	usage := runtimecontract.Usage{InputTokens: runtimecontract.UsageMetric{Available: true, Value: 3, Source: source}, CachedInputTokens: unavailable, OutputTokens: unavailable, ReasoningOutputTokens: unavailable, TotalTokens: unavailable, Calls: unavailable, CostMicros: unavailable}
	report := projectClaudeUsage([]runtimecontract.HistoryTurn{{TurnID: "turn-1", State: runtimecontract.LifecycleCompleted, StartedAt: "2026-08-12T01:00:00Z", CompletedAt: "2026-08-12T01:00:01Z", Content: []runtimecontract.ContentBlock{}, Usage: &usage, UsageDetails: &runtimecontract.UsageDetails{
		ObservedAt: runtimecontract.UsageText{Available: true, Value: "2026-08-12T01:00:01Z", Source: "canonical_turn_ledger"},
		Models: []runtimecontract.ModelUsage{
			{Provider: runtimecontract.UsageText{Available: true, Value: "firstParty", Source: source}, Model: runtimecontract.UsageText{Available: true, Value: "claude-main", Source: source}, Usage: usage, ContextWindow: runtimecontract.UsageMetric{Available: true, Value: 200000, Source: source}},
			{Provider: runtimecontract.UsageText{Available: true, Value: "gateway", Source: source}, Model: runtimecontract.UsageText{Available: true, Value: "claude-side", Source: source}, Usage: usage, ContextWindow: runtimecontract.UsageMetric{Available: true, Value: 100000, Source: source}},
		},
	}}})
	if report.LatestModel.Available || report.LatestProvider.Available || report.Turns[0].Model.Available || len(report.Events) != 2 {
		t.Fatalf("multi-model singular attribution = %#v", report)
	}
}

func TestClaudeContextEvidencePreservesExactManagedDeliveryBytes(t *testing.T) {
	developer := "  " + `<loom_developer_context prompt_revision="owner:1" prompt_hash="p" profile_revision="profile:1" profile_hash="q"><loom_agent_profile_data>Owner-authored profile</loom_agent_profile_data></loom_developer_context>` + "\n"
	input := `<loom_context><loom_agent_relationships revision="relationships:1" hash="r"/></loom_context>`
	report := claudeContextEvidence("turn-redacted", []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleDeveloper, Text: developer}, {Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleUser, Text: input}}, "developer")
	encoded, _ := json.Marshal(report)
	if report == nil || report.Deliveries[0].Content != developer || report.Deliveries[0].Hash != sha256Hex([]byte(report.Deliveries[0].Content)) {
		t.Fatalf("exact Claude context evidence = %s", encoded)
	}
	continued := claudeContextEvidence("turn-redacted", []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleDeveloper, Text: developer}, {Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleUser, Text: input}}, "user")
	if continued == nil || continued.Deliveries[0].Role != "user" {
		t.Fatalf("causal Claude context evidence = %#v", continued)
	}
}

func TestClaudeCanonicalLedgerRejectsMalformedContextEvidence(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	err = st.SaveCanonicalTurnLedger("agent-claude", []runtimecontract.HistoryTurn{{
		TurnID: "turn-malformed", State: runtimecontract.LifecycleCompleted, Content: []runtimecontract.ContentBlock{},
		ContextEvidence: &runtimecontract.ContextEvidence{
			State: runtimecontract.ContextEvidenceProven, TurnID: "turn-malformed", Mode: runtimecontract.ContextDeliveryFullPerTurn,
			Sources: []runtimecontract.ContextEvidenceSource{}, Deliveries: []runtimecontract.ContextEvidenceDelivery{{Channel: "developer", Role: "developer", Hash: "wrong", Content: "accepted bytes"}},
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "hash does not match") {
		t.Fatalf("malformed context evidence save error = %v", err)
	}
}

func TestClaudeCorruptedContextEvidenceIsUnavailableAfterReopen(t *testing.T) {
	developer := `<loom_developer_context prompt_revision="owner:1" prompt_hash="p" profile_revision="profile:1" profile_hash="q"></loom_developer_context>`
	input := `<loom_context><loom_agent_relationships revision="relationships:1" hash="r"/></loom_context>`
	valid := claudeContextEvidence("turn-1", []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleDeveloper, Text: developer}, {Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleUser, Text: input}}, "developer")
	mutations := map[string]func(*runtimecontract.ContextEvidence){
		"blank required revision": func(value *runtimecontract.ContextEvidence) { value.Sources[0].Revision = "" },
		"wrong developer role":    func(value *runtimecontract.ContextEvidence) { value.Deliveries[0].Role = "assistant" },
		"wrong source state":      func(value *runtimecontract.ContextEvidence) { value.Sources[0].State = "observed" },
		"missing input channel": func(value *runtimecontract.ContextEvidence) {
			value.Deliveries = value.Deliveries[:1]
			value.Sources = value.Sources[:2]
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			st, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(valid)
			var corrupted runtimecontract.ContextEvidence
			if err := json.Unmarshal(encoded, &corrupted); err != nil {
				t.Fatal(err)
			}
			mutate(&corrupted)
			if err := st.SaveCanonicalTurnLedger("agent-claude", []runtimecontract.HistoryTurn{{TurnID: "turn-1", State: runtimecontract.LifecycleCompleted, Content: []runtimecontract.ContentBlock{}, ContextEvidence: &corrupted}}); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			contract := newClaudeRuntimeContract("agent-claude", reopened, nil)
			report, failure := contract.InspectContextEvidence(context.Background(), runtimecontract.Binding{}, runtimecontract.ContextEvidenceQuery{TurnID: "turn-1"})
			if failure != nil || report.State != runtimecontract.ContextEvidenceUnavailable {
				t.Fatalf("corrupted reopened evidence = %#v, failure=%v", report, failure)
			}
			history, historyFailure := contract.ReadHistory(context.Background(), runtimecontract.HistoryRequest{Count: 10})
			if historyFailure != nil || history.Turns[0].ContextEvidence == nil || history.Turns[0].ContextEvidence.State != runtimecontract.ContextEvidenceUnavailable {
				t.Fatalf("corrupted history projection = %#v, failure=%v", history, historyFailure)
			}
			if _, usageFailure := contract.InspectUsage(context.Background(), runtimecontract.Binding{}); usageFailure != nil {
				t.Fatalf("independent usage was poisoned by context: %v", usageFailure)
			}
		})
	}
}

func TestClaudeContextInspectionIgnoresUnrelatedCorruptTurn(t *testing.T) {
	developer := `<loom_developer_context prompt_revision="owner:1" prompt_hash="p" profile_revision="profile:1" profile_hash="q"></loom_developer_context>`
	input := `<loom_context><loom_agent_relationships revision="relationships:1" hash="r"/></loom_context>`
	valid := claudeContextEvidence("turn-valid", []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleDeveloper, Text: developer}, {Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleUser, Text: input}}, "developer")
	encoded, _ := json.Marshal(valid)
	var corrupt runtimecontract.ContextEvidence
	if err := json.Unmarshal(encoded, &corrupt); err != nil {
		t.Fatal(err)
	}
	corrupt.TurnID = "turn-corrupt"
	corrupt.Sources[0].Revision = ""
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveCanonicalTurnLedger("agent-claude", []runtimecontract.HistoryTurn{{TurnID: "turn-valid", State: runtimecontract.LifecycleCompleted, Content: []runtimecontract.ContentBlock{}, ContextEvidence: valid}, {TurnID: "turn-corrupt", State: runtimecontract.LifecycleCompleted, Content: []runtimecontract.ContentBlock{}, ContextEvidence: &corrupt}}); err != nil {
		t.Fatal(err)
	}
	contract := newClaudeRuntimeContract("agent-claude", st, nil)
	report, failure := contract.InspectContextEvidence(context.Background(), runtimecontract.Binding{}, runtimecontract.ContextEvidenceQuery{TurnID: "turn-valid"})
	if failure != nil || report.State != runtimecontract.ContextEvidenceProven {
		t.Fatalf("valid evidence poisoned by unrelated Turn = %#v, failure=%v", report, failure)
	}
}

func mustUsageTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
