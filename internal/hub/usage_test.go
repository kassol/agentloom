package hub

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/rollout"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestRuntimeUsageCoverageIsMetadataOnlyAndSourceOrderIsStable(t *testing.T) {
	usage := RuntimeTokenUsage{TotalTokens: 2, Metrics: map[string]RuntimeUsageMetricCoverage{
		"totalTokens": {Available: true, Complete: true, Sources: []string{"z-source"}},
	}}
	usage.Add(RuntimeTokenUsage{TotalTokens: 3, Metrics: map[string]RuntimeUsageMetricCoverage{
		"totalTokens": {Available: true, Complete: true, Sources: []string{"a-source"}},
	}})
	if usage.TotalTokens != 5 {
		t.Fatalf("top-level numeric total = %d, want 5", usage.TotalTokens)
	}
	if !reflect.DeepEqual(usage.Metrics["totalTokens"].Sources, []string{"a-source", "z-source"}) {
		t.Fatalf("coverage sources are unstable: %#v", usage.Metrics["totalTokens"])
	}
	encoded, err := json.Marshal(usage.Metrics["totalTokens"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"value"`) {
		t.Fatalf("coverage duplicated numeric total: %s", encoded)
	}
}

func writeUsageRollout(t *testing.T, threadID string) {
	t.Helper()
	dir := t.TempDir()
	day := filepath.Join(dir, "2026", "07", "13")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(day, "rollout-2026-07-13T09-00-00-"+threadID+".jsonl")
	data := `{"timestamp":"2026-07-12T01:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}
{"timestamp":"2026-07-12T01:00:01Z","type":"turn_context","payload":{"turn_id":"turn-1","model":"gpt-5.6"}}
{"timestamp":"2026-07-12T01:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10,"reasoning_output_tokens":3,"total_tokens":110},"last_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10,"reasoning_output_tokens":3,"total_tokens":110},"model_context_window":1000}}}
{"timestamp":"2026-07-13T01:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2"}}
{"timestamp":"2026-07-13T01:00:01Z","type":"turn_context","payload":{"turn_id":"turn-2","model":"gpt-5.6-sol"}}
{"timestamp":"2026-07-13T01:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":180,"cached_input_tokens":100,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":200},"last_token_usage":{"input_tokens":80,"cached_input_tokens":60,"output_tokens":10,"reasoning_output_tokens":2,"total_tokens":90},"model_context_window":1000}}}
{"timestamp":"2026-07-13T01:00:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":180,"cached_input_tokens":100,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":200},"last_token_usage":{"input_tokens":80,"cached_input_tokens":60,"output_tokens":10,"reasoning_output_tokens":2,"total_tokens":90},"model_context_window":1000}}}
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SESSIONS_DIR", dir)
}

func writeSameModelProviderUsageRollout(t *testing.T, threadID string) {
	t.Helper()
	dir := t.TempDir()
	day := filepath.Join(dir, "2026", "07", "13")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(day, "rollout-2026-07-13T09-00-00-"+threadID+".jsonl")
	data := `{"timestamp":"2026-07-12T01:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}
{"timestamp":"2026-07-12T01:00:01Z","type":"turn_context","payload":{"turn_id":"turn-1","model":"shared-model"}}
{"timestamp":"2026-07-12T01:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10,"reasoning_output_tokens":3,"total_tokens":110},"last_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10,"reasoning_output_tokens":3,"total_tokens":110},"model_context_window":1000}}}
{"timestamp":"2026-07-13T01:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2"}}
{"timestamp":"2026-07-13T01:00:01Z","type":"turn_context","payload":{"turn_id":"turn-2","model":"shared-model"}}
{"timestamp":"2026-07-13T01:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":180,"cached_input_tokens":100,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":200},"last_token_usage":{"input_tokens":80,"cached_input_tokens":60,"output_tokens":10,"reasoning_output_tokens":2,"total_tokens":90},"model_context_window":1000}}}
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SESSIONS_DIR", dir)
}

func readTestRuntimeUsage(t *testing.T, threadID string) *RuntimeUsageReport {
	t.Helper()
	report, err := rollout.ReadUsage(threadID)
	if err != nil {
		t.Fatal(err)
	}
	converted := codexRuntimeUsageReport(report)
	return &converted
}

func TestBuildAgentUsageSeparatesTodayPeriodAndLifetime(t *testing.T) {
	const threadID = "usage-thread"
	writeUsageRollout(t, threadID)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	agent := AgentView{Agent: Agent{ID: "agent-1", Name: "research", ThreadID: "loom-" + threadID, RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: threadID}, Status: "idle"}, nativeRuntimeRef: threadID}

	usage := buildAgentUsage(agent, 7, now, readTestRuntimeUsage(t, threadID))
	if !usage.Available {
		t.Fatal("usage should be available")
	}
	if usage.Lifetime.TotalTokens != 200 || usage.Period.TotalTokens != 200 {
		t.Fatalf("lifetime/period = %d/%d", usage.Lifetime.TotalTokens, usage.Period.TotalTokens)
	}
	if usage.Today.TotalTokens != 90 {
		t.Fatalf("today = %#v, want second call only", usage.Today)
	}
	if usage.CacheHitPercent < 55.5 || usage.CacheHitPercent > 55.6 {
		t.Fatalf("cache hit = %v, want about 55.56", usage.CacheHitPercent)
	}
	if usage.Context.UsedPercent != 8 {
		t.Fatalf("context = %#v, want 8%%", usage.Context)
	}
	if len(usage.Models) != 2 || usage.Models[0].Model != "gpt-5.6" {
		t.Fatalf("models = %#v", usage.Models)
	}
}

func TestCodexUsageAdapterPreservesGoldenValuesSourcesAndOrder(t *testing.T) {
	const threadID = "usage-codex-golden"
	writeUsageRollout(t, threadID)
	report := readTestRuntimeUsage(t, threadID)
	if err := report.Validate(); err != nil {
		t.Fatal(err)
	}
	if report.Lifetime.InputTokens.Value != 180 || report.Lifetime.CachedInputTokens.Value != 100 || report.Lifetime.OutputTokens.Value != 20 || report.Lifetime.TotalTokens.Value != 200 || report.Lifetime.Calls.Value != 2 {
		t.Fatalf("Codex golden lifetime = %#v", report.Lifetime)
	}
	if report.Lifetime.TotalTokens.Source != "codex_rollout" || report.Lifetime.CostMicros.Available || report.Lifetime.CostMicros.Source != "runtime_unavailable" {
		t.Fatalf("Codex golden availability/source = %#v", report.Lifetime)
	}
	if len(report.Events) != 2 || report.Events[0].Timestamp.Value != "2026-07-12T01:00:02Z" || report.Events[1].Timestamp.Value != "2026-07-13T01:00:02Z" {
		t.Fatalf("Codex golden event order = %#v", report.Events)
	}
	contract := &codexRuntimeContract{}
	inspected, failure := contract.InspectUsage(t.Context(), runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "codex", NativeRef: threadID})
	if failure != nil {
		t.Fatal(failure)
	}
	if inspected.LatestProvider.Available || inspected.LatestProvider.Source != "runtime_unavailable" {
		t.Fatalf("Codex passive report fabricated Provider: %#v", inspected.LatestProvider)
	}
}

func TestBuildAgentUsageSeparatesSameThreadByProviderTimeline(t *testing.T) {
	const threadID = "usage-provider-thread"
	writeUsageRollout(t, threadID)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	agent := AgentView{Agent: Agent{
		ID: "agent-1", Name: "research", ThreadID: "loom-" + threadID, RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: threadID}, Status: "idle",
		ProviderID: "deepseek", Model: deepSeekModel,
		ProviderHistory: []ProviderBindingChange{{
			PreviousProviderID: "openai", PreviousModel: "gpt-5.6",
			ProviderID: "deepseek", Model: deepSeekModel, SwitchedAt: "2026-07-13T00:00:00Z",
		}},
	}, nativeRuntimeRef: threadID}

	usage := buildAgentUsage(agent, 7, now, readTestRuntimeUsage(t, threadID))
	if len(usage.Models) != 2 {
		t.Fatalf("Provider/model groups = %#v", usage.Models)
	}
	groups := map[string]int64{}
	for _, model := range usage.Models {
		groups[model.ProviderID+"/"+model.Model] = model.Usage.TotalTokens
	}
	if groups["openai/gpt-5.6"] != 110 || groups["deepseek/gpt-5.6-sol"] != 90 {
		t.Fatalf("Provider/model usage = %#v", groups)
	}
	if usage.Lifetime.TotalTokens != 200 || usage.LatestProviderID != "deepseek" {
		t.Fatalf("total/latest Provider = %#v", usage)
	}
}

func TestBuildAgentUsageDoesNotMergeSameModelAcrossProviders(t *testing.T) {
	const threadID = "usage-same-model-provider-thread"
	writeSameModelProviderUsageRollout(t, threadID)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	agent := AgentView{Agent: Agent{
		ID: "agent-1", Name: "research", ThreadID: "loom-" + threadID, RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: threadID}, Status: "idle",
		ProviderID: "deepseek", Model: "shared-model",
		ProviderHistory: []ProviderBindingChange{{
			PreviousProviderID: "openai", PreviousModel: "shared-model",
			ProviderID: "deepseek", Model: "shared-model", SwitchedAt: "2026-07-13T00:00:00Z",
		}},
	}, nativeRuntimeRef: threadID}

	usage := buildAgentUsage(agent, 7, now, readTestRuntimeUsage(t, threadID))
	groups := map[string]int64{}
	for _, model := range usage.Models {
		groups[model.ProviderID+"/"+model.Model] = model.Usage.TotalTokens
	}
	if len(groups) != 2 || groups["openai/shared-model"] != 110 || groups["deepseek/shared-model"] != 90 {
		t.Fatalf("same-model Provider groups = %#v", groups)
	}
	if usage.Period.TotalTokens != 200 || usage.Lifetime.TotalTokens != 200 {
		t.Fatalf("Provider split changed totals: %#v", usage)
	}
}

func TestBuildAgentUsageWithoutThreadIsUnavailable(t *testing.T) {
	usage := buildAgentUsage(AgentView{Agent: Agent{ID: "agent-1", Name: "empty"}}, 7, time.Now(), nil)
	if usage.Available || len(usage.Daily) != 7 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestBuildAgentUsageRangeSelectsHistoricalDayAndPreviousDay(t *testing.T) {
	const threadID = "usage-range-thread"
	writeUsageRollout(t, threadID)
	location := time.FixedZone("UTC+8", 8*60*60)
	start := time.Date(2026, 7, 13, 0, 0, 0, 0, location)
	endExclusive := start.AddDate(0, 0, 1)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, location)
	agent := AgentView{Agent: Agent{ID: "agent-1", Name: "research", ThreadID: "loom-" + threadID, RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: threadID}, Status: "idle"}, nativeRuntimeRef: threadID}

	usage := buildAgentUsageRange(agent, start, endExclusive, now, readTestRuntimeUsage(t, threadID))
	if usage.Period.TotalTokens != 90 {
		t.Fatalf("period = %#v, want Jul 13 usage", usage.Period)
	}
	if usage.Previous.TotalTokens != 110 {
		t.Fatalf("previous = %#v, want Jul 12 usage", usage.Previous)
	}
	if len(usage.Daily) != 1 || usage.Daily[0].Date != "2026-07-13" || usage.Daily[0].Usage.TotalTokens != 90 {
		t.Fatalf("daily = %#v", usage.Daily)
	}
}

func TestPreviousUsageRangeMatchesElapsedClockForLiveWindow(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	start := time.Date(2026, 7, 10, 0, 0, 0, 0, location)
	endExclusive := time.Date(2026, 7, 17, 0, 0, 0, 0, location)
	now := time.Date(2026, 7, 16, 10, 30, 0, 0, location)

	previousStart, previousEnd := previousUsageRange(start, endExclusive, now)
	if got := previousStart.Format(time.RFC3339); got != "2026-07-03T00:00:00+08:00" {
		t.Fatalf("previous start = %s", got)
	}
	if got := previousEnd.Format(time.RFC3339); got != "2026-07-09T10:30:00+08:00" {
		t.Fatalf("previous end = %s", got)
	}
}

func TestAgentUsageCacheIsBoundedAndDropsStaleReports(t *testing.T) {
	agentUsageCache.Lock()
	previousEntries, previousClock := agentUsageCache.entries, agentUsageCache.clock
	agentUsageCache.entries = map[string]agentUsageCacheEntry{}
	agentUsageCache.clock = 0
	agentUsageCache.Unlock()
	defer func() {
		agentUsageCache.Lock()
		agentUsageCache.entries = previousEntries
		agentUsageCache.clock = previousClock
		agentUsageCache.Unlock()
	}()

	reports := make([]*RuntimeUsageReport, maxAgentUsageCacheEntries+1)
	for index := range reports {
		reports[index] = &RuntimeUsageReport{}
		cacheAgentUsage(fmt.Sprintf("key-%03d", index), reports[index], AgentUsage{AgentName: fmt.Sprintf("agent-%03d", index)})
	}
	agentUsageCache.Lock()
	cacheSize := len(agentUsageCache.entries)
	_, oldestExists := agentUsageCache.entries["key-000"]
	_, newestExists := agentUsageCache.entries[fmt.Sprintf("key-%03d", maxAgentUsageCacheEntries)]
	agentUsageCache.Unlock()
	if cacheSize != maxAgentUsageCacheEntries || oldestExists || !newestExists {
		t.Fatalf("bounded cache size=%d oldest=%v newest=%v", cacheSize, oldestExists, newestExists)
	}

	key := fmt.Sprintf("key-%03d", maxAgentUsageCacheEntries)
	if _, ok := cachedAgentUsage(key, &RuntimeUsageReport{}); ok {
		t.Fatal("cache accepted a stale UsageReport pointer")
	}
	agentUsageCache.Lock()
	_, staleExists := agentUsageCache.entries[key]
	agentUsageCache.Unlock()
	if staleExists {
		t.Fatal("stale UsageReport entry was not evicted")
	}
}

func TestMixedRuntimeUsageSurvivesStoreReopenAndKeepsOptionalCoverageTruthful(t *testing.T) {
	const codexRef = "usage-mixed-codex"
	writeUsageRollout(t, codexRef)
	piRef := filepath.Join(t.TempDir(), "session-pi-mixed.jsonl")
	piData := `{"type":"session","version":3,"id":"pi-mixed","timestamp":"2026-08-10T01:00:00Z"}
{"type":"message","id":"pi-user","timestamp":"2026-08-10T01:00:01Z","message":{"role":"user","content":"work"}}
{"type":"message","id":"pi-answer","parentId":"pi-user","timestamp":"2026-08-10T01:00:02Z","message":{"role":"assistant","provider":"pi-provider","model":"pi-model","content":"done","stopReason":"stop","usage":{"input":10,"output":4,"cacheRead":2,"cacheWrite":3,"totalTokens":19}}}
{"type":"message","id":"pi-user-open","parentId":"pi-answer","timestamp":"2026-08-10T01:00:03Z","message":{"role":"user","content":"still running"}}
`
	if err := os.WriteFile(piRef, []byte(piData), 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	open := func() (*store.Store, *Hub) {
		st, err := store.Open(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		return st, New(st)
	}
	st, h := open()
	for _, params := range []RestoreAgentParams{
		{ID: "mixed-codex", Name: "mixed-codex", Cwd: t.TempDir(), ThreadID: "loom-codex", RuntimeBinding: RuntimeBinding{SchemaVersion: 2, Kind: "codex", NativeRef: codexRef}, CreatedAt: "2026-07-12T00:00:00Z"},
		{ID: "mixed-pi", Name: "mixed-pi", Cwd: t.TempDir(), ThreadID: "loom-pi", RuntimeBinding: RuntimeBinding{SchemaVersion: 2, Kind: "pi", NativeRef: piRef}, CreatedAt: "2026-08-10T00:00:00Z"},
	} {
		if _, err := h.RestoreAgent(params); err != nil {
			t.Fatal(err)
		}
	}
	assertMixed := func(h *Hub) {
		start := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
		end := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
		if _, err := h.runtimeUsageReport("mixed-pi", runtimecontract.Binding{SchemaVersion: 2, RuntimeKind: "pi", NativeRef: piRef}); err != nil {
			t.Fatalf("Pi Runtime usage report: %v", err)
		}
		piUsage, err := h.AgentTokenUsageRange("mixed-pi", start, end)
		if err != nil || !piUsage.Available || piUsage.Period.TotalTokens != 19 || piUsage.Period.InputTokens != 15 || piUsage.Period.Calls != 0 || piUsage.LatestProviderID != "pi-provider" {
			t.Fatalf("Pi usage = %#v err=%v", piUsage, err)
		}
		if calls := piUsage.Period.Metrics["calls"]; calls.Available || calls.Complete {
			t.Fatalf("Pi calls were fabricated: %#v", calls)
		}
		overview := h.TokenUsageOverviewRange(start, end)
		if overview.TrackedAgents != 2 || overview.Period.TotalTokens != 219 || overview.Period.Calls != 2 {
			t.Fatalf("mixed overview = %#v", overview)
		}
		if calls := overview.Period.Metrics["calls"]; !calls.Available || calls.Complete {
			t.Fatalf("team optional coverage = %#v", calls)
		}
		activity := h.DailyActivity(time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC), 30)
		if activity.TrackedAgents != 2 || activity.ActiveAgents != 2 || activity.InactiveAgents != 0 || activity.Usage.TotalTokens != 19 {
			t.Fatalf("mixed daily activity = %#v", activity)
		}
		piActive := false
		for _, row := range activity.Agents {
			piActive = piActive || row.AgentID == "mixed-pi"
		}
		if !piActive {
			t.Fatal("Pi consumed work was classified inactive")
		}
		workload := h.WorkloadOverviewRange(start, end)
		for _, agent := range workload.Agents {
			if agent.AgentID == "mixed-pi" && (!agent.ActivityAvailable || agent.OpenTurns != 1) {
				t.Fatalf("open Pi Turn was not preserved across Store reopen: %#v", agent)
			}
		}
	}
	assertMixed(h)
	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, h = open()
	defer h.Shutdown()
	defer st.Close()
	assertMixed(h)
}
