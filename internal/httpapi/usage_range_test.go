package httpapi

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestCalendarWindowFromRequestUsesExplicitTimezoneAndInclusiveDates(t *testing.T) {
	request := httptest.NewRequest("GET", "/api/usage?from=2026-07-10&to=2026-07-16&tz=Asia%2FShanghai", nil)
	now := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	start, endExclusive, explicit, err := calendarWindowFromRequest(request, now)
	if err != nil {
		t.Fatal(err)
	}
	if !explicit || start.Format(time.RFC3339) != "2026-07-10T00:00:00+08:00" || endExclusive.Format(time.RFC3339) != "2026-07-17T00:00:00+08:00" {
		t.Fatalf("window = %s to %s explicit=%v", start.Format(time.RFC3339), endExclusive.Format(time.RFC3339), explicit)
	}
}

func TestUsageAPIReportsPiConsumedWorkAndUnavailableCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-pi-api.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"session","version":3,"id":"pi-api","timestamp":"2026-08-10T01:00:00Z"}
{"type":"message","id":"user-1","timestamp":"2026-08-10T01:00:01Z","message":{"role":"user","content":"work"}}
{"type":"message","id":"answer-1","parentId":"user-1","timestamp":"2026-08-10T01:00:02Z","message":{"role":"assistant","provider":"pi-provider","model":"pi-model","content":"done","stopReason":"stop","usage":{"input":10,"output":4,"cacheRead":2,"cacheWrite":3,"totalTokens":19}}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	if _, err := h.RestoreAgent(hub.RestoreAgentParams{
		ID: "pi-api", Name: "pi-api", Cwd: t.TempDir(), ThreadID: "loom-pi-api",
		RuntimeBinding: hub.RuntimeBinding{SchemaVersion: 2, Kind: "pi", NativeRef: path}, CreatedAt: "2026-08-10T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	handler := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()
	for _, target := range []string{
		"/api/usage?from=2026-08-10&to=2026-08-10&tz=UTC",
		"/api/agents/pi-api/usage?from=2026-08-10&to=2026-08-10&tz=UTC",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", target, response.Code, response.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body["usage"]) == 0 {
			t.Fatalf("GET %s did not return usage JSON: %s err=%v", target, response.Body.String(), err)
		}
		if text := string(body["usage"]); !containsAll(text, `"totalTokens":19`, `"inputTokens":15`, `"calls":0`, `"available":false`, `"runtime_unavailable"`) {
			t.Fatalf("GET %s usage is not truthful: %s", target, text)
		}
		if strings.Contains(response.Body.String(), path) {
			t.Fatalf("GET %s leaked the private Runtime reference: %s", target, response.Body.String())
		}
	}
}

func TestClaudeObservabilityAPIPreservesPartialTruthWithoutNativeRefs(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	developer := `<loom_developer_context prompt_revision="owner:2" prompt_hash="p" profile_revision="profile:3" profile_hash="q"></loom_developer_context>`
	input := `<loom_context><loom_agent_relationships revision="relationships:4" hash="r"></loom_agent_relationships></loom_context>`
	hash := func(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }
	source := "claude_agent_sdk"
	unavailable := runtimecontract.UsageMetric{Source: "runtime_unavailable"}
	usage := &runtimecontract.Usage{
		InputTokens: runtimecontract.UsageMetric{Available: true, Value: 6, Source: source}, CachedInputTokens: runtimecontract.UsageMetric{Available: true, Source: source}, OutputTokens: runtimecontract.UsageMetric{Available: true, Value: 2, Source: source},
		ReasoningOutputTokens: unavailable, TotalTokens: unavailable, Calls: unavailable, CostMicros: runtimecontract.UsageMetric{Available: true, Value: 9, Source: source},
	}
	if err := st.SaveCanonicalTurnLedger("claude-api", []runtimecontract.HistoryTurn{{
		TurnID: "turn-claude-api", State: runtimecontract.LifecycleCompleted, StartedAt: "2026-08-12T01:00:00Z", CompletedAt: "2026-08-12T01:00:01Z", Content: []runtimecontract.ContentBlock{}, Usage: usage,
		ContextEvidence: &runtimecontract.ContextEvidence{State: runtimecontract.ContextEvidenceProven, TurnID: "turn-claude-api", Mode: runtimecontract.ContextDeliveryFullPerTurn,
			Sources:    []runtimecontract.ContextEvidenceSource{{Key: "loom_agent_prompt", Revision: "owner:2", Hash: "p", Channel: "developer", State: "delivered"}, {Key: "loom_agent_profile", Revision: "profile:3", Hash: "q", Channel: "developer", State: "delivered"}, {Key: "loom_agent_relationships", Revision: "relationships:4", Hash: "r", Channel: "input", State: "delivered"}},
			Deliveries: []runtimecontract.ContextEvidenceDelivery{{Channel: "developer", Role: "developer", Hash: hash(developer), Content: developer}, {Channel: "input", Role: "user", Hash: hash(input), Content: input}}, UnsupportedDimensions: []string{"coverage", "epoch", "replay", "resend"}},
	}}); err != nil {
		t.Fatal(err)
	}
	privateSession := "/Users/owner/.claude/projects/private-session.jsonl"
	if err := st.SaveAgents(map[string]*hub.Agent{"claude-api": {ID: "claude-api", Name: "claude-api", Cwd: t.TempDir(), ThreadID: "loom-claude-api", RuntimeBinding: hub.RuntimeBinding{SchemaVersion: 2, Kind: "claude", NativeRef: privateSession}, RuntimeTurnBindings: map[string]string{"turn-claude-api": "native-secret"}, RuntimeConfiguration: hub.RuntimeConfiguration{Configured: true, SettingSources: []string{"project"}, Authentication: hub.RuntimeAuthentication{Category: hub.RuntimeAuthConsole, Source: "api_key"}}, Status: "idle", CreatedAt: nowForTest(), UpdatedAt: nowForTest()}}); err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	handler := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()
	for _, target := range []string{
		"/api/usage?from=2026-08-12&to=2026-08-12&tz=UTC",
		"/api/agents/claude-api/usage?from=2026-08-12&to=2026-08-12&tz=UTC",
		"/api/agents/claude-api/context/explain?turnId=turn-claude-api",
		"/api/workload?from=2026-08-12&to=2026-08-12&tz=UTC",
		"/api/activity/daily?date=2026-08-12&tz=UTC&bucket=30",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", target, response.Code, response.Body.String())
		}
		text := response.Body.String()
		if strings.Contains(text, privateSession) || strings.Contains(text, "native-secret") {
			t.Fatalf("GET %s leaked native correlation: %s", target, text)
		}
		switch {
		case strings.Contains(target, "context/explain"):
			if !containsAll(text, `"state":"proven"`, `"revision":"owner:2"`, `"revision":"profile:3"`, `"revision":"relationships:4"`) {
				t.Fatalf("GET %s context is incomplete: %s", target, text)
			}
		case strings.Contains(target, "/workload"):
			if !containsAll(text, `"activityAvailable":true`, `"turnCount":1`, `"trackedActivityAgents":1`) {
				t.Fatalf("GET %s omitted Claude activity: %s", target, text)
			}
		case strings.Contains(target, "/activity/daily"):
			if !containsAll(text, `"trackedAgents":1`, `"turnCount":1`, `"inputTokens":6`, `"totalTokens":{"available":false`) {
				t.Fatalf("GET %s daily activity is not partial: %s", target, text)
			}
		default:
			if !containsAll(text, `"inputTokens":6`, `"totalTokens":0`, `"totalTokens":{"available":false`, `"calls":{"available":false`) {
				t.Fatalf("GET %s usage is not partial: %s", target, text)
			}
		}
	}
}

func containsAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}

func TestCalendarWindowFromRequestRejectsIncompleteFutureAndOversizedRanges(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	for _, target := range []string{
		"/api/usage?from=2026-07-10",
		"/api/usage?from=2026-07-17&to=2026-07-17&tz=Asia%2FShanghai",
		"/api/usage?from=2025-01-01&to=2026-07-16&tz=Asia%2FShanghai",
		"/api/usage?from=2026-07-10&to=2026-07-09",
	} {
		request := httptest.NewRequest("GET", target, nil)
		if _, _, _, err := calendarWindowFromRequest(request, now); err == nil {
			t.Fatalf("calendarWindowFromRequest accepted %s", target)
		}
	}
}

func TestDailyActivityWindowUsesDateTimezoneAndBucket(t *testing.T) {
	now := time.Date(2026, 7, 21, 3, 0, 0, 0, time.UTC)
	request := httptest.NewRequest("GET", "/api/activity/daily?date=2026-07-20&tz=Asia%2FShanghai&bucket=30", nil)
	start, endExclusive, bucket, err := dailyActivityWindowFromRequest(request, now)
	if err != nil {
		t.Fatal(err)
	}
	if start.Format(time.RFC3339) != "2026-07-20T00:00:00+08:00" || endExclusive.Format(time.RFC3339) != "2026-07-21T00:00:00+08:00" || bucket != 30 {
		t.Fatalf("daily window = %s to %s bucket=%d", start.Format(time.RFC3339), endExclusive.Format(time.RFC3339), bucket)
	}
}

func TestDailyActivityWindowRejectsFutureAndUnsupportedBucket(t *testing.T) {
	now := time.Date(2026, 7, 21, 3, 0, 0, 0, time.UTC)
	for _, target := range []string{
		"/api/activity/daily?date=2026-07-22&tz=Asia%2FShanghai",
		"/api/activity/daily?date=2026-07-21&tz=Asia%2FShanghai&bucket=20",
		"/api/activity/daily?date=bad&tz=Asia%2FShanghai",
	} {
		request := httptest.NewRequest("GET", target, nil)
		if _, _, _, err := dailyActivityWindowFromRequest(request, now); err == nil {
			t.Fatalf("dailyActivityWindowFromRequest accepted %s", target)
		}
	}
}
