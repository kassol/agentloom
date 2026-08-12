package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func restoreCodexAgentParams(id, name, cwd, nativeRef string) hub.RestoreAgentParams {
	return hub.RestoreAgentParams{ID: id, Name: name, Cwd: cwd, ThreadID: "loom-" + nativeRef, RuntimeBinding: hub.RuntimeBinding{Kind: "codex", NativeRef: nativeRef}}
}

func TestConnectorCommandStreamIsExclusivePerConnection(t *testing.T) {
	s := &Server{activeConnectors: map[string]struct{}{}}
	if !s.acquireConnector("conn-a") {
		t.Fatal("first connector lease was rejected")
	}
	if s.acquireConnector("conn-a") {
		t.Fatal("second connector lease was accepted")
	}
	if !s.acquireConnector("conn-b") {
		t.Fatal("different connection was rejected")
	}
	s.releaseConnector("conn-a")
	if !s.acquireConnector("conn-a") {
		t.Fatal("released connector lease was not reusable")
	}
}

func TestThreadSSEProjectsOnlyCanonicalNamespace(t *testing.T) {
	event := store.Event{Seq: 7, Type: "hub/session-created", Data: json.RawMessage(`{"id":"agent-1"}`)}
	var canonical bytes.Buffer
	writeThreadSSE(&canonical, event)
	if got := canonical.String(); !strings.Contains(got, `"type":"loom/agent-created"`) || strings.Contains(got, `"type":"hub/session-created"`) {
		t.Fatalf("canonical SSE = %q", got)
	}
}

func TestThreadSSEPreservesUsageAvailabilityWithoutNativeRefs(t *testing.T) {
	usage := runtimecontract.Usage{
		InputTokens:       runtimecontract.UsageMetric{Available: true, Value: 0, Source: "claude_agent_sdk"},
		CachedInputTokens: runtimecontract.UsageMetric{Source: "runtime_unavailable"}, OutputTokens: runtimecontract.UsageMetric{Source: "runtime_unavailable"}, ReasoningOutputTokens: runtimecontract.UsageMetric{Source: "runtime_unavailable"}, TotalTokens: runtimecontract.UsageMetric{Source: "runtime_unavailable"}, Calls: runtimecontract.UsageMetric{Source: "runtime_unavailable"}, CostMicros: runtimecontract.UsageMetric{Source: "runtime_unavailable"},
	}
	payload, err := json.Marshal(runtimecontract.Event{Kind: runtimecontract.EventUsage, TurnID: "turn-1", RuntimeTurnRef: "native-secret", Usage: &usage})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	writeThreadSSE(&output, store.Event{Type: "loom/runtime-event", Data: payload})
	got := output.String()
	if !containsAll(got, `"inputTokens":{"available":true`, `"totalTokens":{"available":false`) || strings.Contains(got, "native-secret") {
		t.Fatalf("partial Claude usage SSE = %s", got)
	}
}

func TestGlobalSSEIsCanonical(t *testing.T) {
	var output bytes.Buffer
	writeGlobalSSE(&output, store.Event{Type: "hub/comms-message", Data: json.RawMessage(`{}`)})
	got := output.String()
	if !strings.Contains(got, `"type":"loom/comms-message"`) || strings.Contains(got, `"type":"hub/comms-message"`) {
		t.Fatalf("ordinary global SSE = %q", got)
	}
}

func TestGlobalThreadEventHasNoLegacyDuplicate(t *testing.T) {
	var output bytes.Buffer
	writeGlobalSSE(&output, store.Event{Type: "loom/thread-event", Data: json.RawMessage(`{"agentId":"agent-1"}`)})
	got := output.String()
	if strings.Count(got, `"type":"loom/thread-event"`) != 1 || strings.Contains(got, `"type":"hub/thread-event"`) {
		t.Fatalf("multiplexed global SSE = %q", got)
	}
}

func TestCanonicalRuntimeEventFilterCoversPersistedRawAndNestedGlobalRows(t *testing.T) {
	cases := []store.Event{
		{Type: "item/agentMessage/delta", Data: json.RawMessage(`{"delta":"old"}`)},
		{Type: "loom/text-delta", Data: json.RawMessage(`{"delta":"old-normalized"}`)},
		{Type: "hub/reasoning-delta", Data: json.RawMessage(`{"delta":"old-alias"}`)},
		{Type: "loom/tool-completed", Data: json.RawMessage(`{"toolCallId":"old-tool"}`)},
		{Type: "loom/runtime-event", Data: json.RawMessage(`{"compatibility":true}`)},
		{Type: "loom/thread-event", Data: json.RawMessage(`{"event":{"type":"item/agentMessage/delta","data":{"delta":"old"}}}`)},
		{Type: "loom/thread-event", Data: json.RawMessage(`{"event":{"type":"hub/text-delta","data":{"delta":"old-nested-alias"}}}`)},
	}
	for _, event := range cases {
		if !isHistoricalRawRuntimeEvent(event) {
			t.Fatalf("event was not filtered: %#v", event)
		}
	}
	if isHistoricalRawRuntimeEvent(store.Event{Type: "loom/runtime-event", Data: json.RawMessage(`{"kind":"content"}`)}) {
		t.Fatal("typed canonical event was filtered")
	}
}

func TestCanonicalGlobalRouteFiltersPersistedRawRowsAndLegacyRouteIsGone(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	h.EmitGlobal("loom/thread-event", map[string]any{
		"agentId": "agent-1", "event": store.Event{Seq: 1, Type: "loom/runtime-event", Data: json.RawMessage(`{"kind":"content","turnId":"turn-1"}`)},
	})
	h.EmitGlobal("loom/thread-event", map[string]any{
		"agentId": "agent-1", "event": store.Event{Seq: 2, Type: "item/agentMessage/delta", Data: json.RawMessage(`{"compatibility":true,"delta":"duplicate"}`)},
	})
	server := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	requestStream := func(path string) *httptest.ResponseRecorder {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	canonical := requestStream("/api/agents/events?since=0")
	if canonical.Code != http.StatusOK || !strings.Contains(canonical.Body.String(), `"type":"loom/runtime-event"`) || strings.Contains(canonical.Body.String(), "duplicate") {
		t.Fatalf("canonical global stream = %d %s", canonical.Code, canonical.Body.String())
	}
	if canonical.Header().Get("Deprecation") != "" {
		t.Fatalf("canonical stream marked deprecated: %#v", canonical.Header())
	}
	legacy := requestStream("/api/events?since=0")
	if legacy.Code != http.StatusNotFound {
		t.Fatalf("legacy global stream = %d body=%s", legacy.Code, legacy.Body.String())
	}
}

func TestCodexLoomAgentAPIHasNoLegacySessionAlias(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", t.TempDir()+"/missing.json")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	server := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	for _, path := range []string{"/api/agents", "/api/usage", "/api/health"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		res := httptest.NewRecorder()
		server.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, res.Code, res.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s JSON: %v", path, err)
		}
	}
	for _, path := range []string{"/api/sessions", "/api/events", "/api/images"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		res := httptest.NewRecorder()
		server.ServeHTTP(res, req)
		if res.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d: %s", path, res.Code, res.Body.String())
		}
	}
}

func TestAgentRuntimeKindIsRequiredAndImmutable(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", t.TempDir()+"/missing.json")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	server := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	create := httptest.NewRequest(http.MethodPost, "/api/agents", strings.NewReader(`{"name":"worker","cwd":"/tmp"}`))
	created := httptest.NewRecorder()
	server.ServeHTTP(created, create)
	if created.Code != http.StatusBadRequest || !strings.Contains(created.Body.String(), "runtime") {
		t.Fatalf("POST /api/agents = %d %s, want required Runtime error", created.Code, created.Body.String())
	}

	if _, err := h.RestoreAgent(hub.RestoreAgentParams{
		ID: "agent-1", Name: "worker", Cwd: "/tmp", ThreadID: "thread-1",
		RuntimeBinding: hub.RuntimeBinding{Kind: "codex", NativeRef: "codex-thread-1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent("__runtime-diagnostic-agent-1", store.Event{Seq: 1, TS: nowForTest(), Type: "turn/native", Data: json.RawMessage(`{"threadId":"codex-thread-1","credential":"native-only"}`)}); err != nil {
		t.Fatal(err)
	}
	diagnosticsRequest := httptest.NewRequest(http.MethodGet, "/api/agents/agent-1/runtime/diagnostics", nil)
	diagnosticsResponse := httptest.NewRecorder()
	server.ServeHTTP(diagnosticsResponse, diagnosticsRequest)
	if diagnosticsResponse.Code != http.StatusOK || !strings.Contains(diagnosticsResponse.Body.String(), `"nativeRef":"codex-thread-1"`) {
		t.Fatalf("Runtime diagnostics = %d %s", diagnosticsResponse.Code, diagnosticsResponse.Body.String())
	}
	diagnosticEventsRequest := httptest.NewRequest(http.MethodGet, "/api/agents/agent-1/runtime/diagnostics/events", nil)
	diagnosticEventsResponse := httptest.NewRecorder()
	server.ServeHTTP(diagnosticEventsResponse, diagnosticEventsRequest)
	if diagnosticEventsResponse.Code != http.StatusOK || !strings.Contains(diagnosticEventsResponse.Body.String(), `"credential":"[redacted]"`) || strings.Contains(diagnosticEventsResponse.Body.String(), "native-only") {
		t.Fatalf("Runtime diagnostic events = %d %s", diagnosticEventsResponse.Code, diagnosticEventsResponse.Body.String())
	}
	ordinaryRequest := httptest.NewRequest(http.MethodGet, "/api/agents/agent-1", nil)
	ordinaryResponse := httptest.NewRecorder()
	server.ServeHTTP(ordinaryResponse, ordinaryRequest)
	if strings.Contains(ordinaryResponse.Body.String(), "codex-thread-1") {
		t.Fatalf("ordinary Agent API leaked native Runtime identity: %s", ordinaryResponse.Body.String())
	}
	update := httptest.NewRequest(http.MethodPatch, "/api/agents/agent-1/config", strings.NewReader(`{"runtimeKind":"pi"}`))
	updated := httptest.NewRecorder()
	server.ServeHTTP(updated, update)
	if updated.Code != http.StatusConflict || !strings.Contains(updated.Body.String(), "immutable") {
		t.Fatalf("PATCH Runtime kind = %d %s, want immutable error", updated.Code, updated.Body.String())
	}

	detail := httptest.NewRecorder()
	server.ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/agents/agent-1", nil))
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"runtimeBinding":{"kind":"codex"}`) || strings.Contains(detail.Body.String(), "codex-thread-1") {
		t.Fatalf("GET Agent Runtime projection = %d %s", detail.Code, detail.Body.String())
	}
}

func TestWebAssetCachingAndSPAFallback(t *testing.T) {
	server := &Server{web: fstest.MapFS{
		"index.html":          {Data: []byte("app-shell")},
		"assets/index-abc.js": {Data: []byte("export const ready = true")},
	}}
	tests := []struct {
		name    string
		path    string
		status  int
		body    string
		cache   string
		content string
	}{
		{name: "index is never cached", path: "/", status: http.StatusOK, body: "app-shell", cache: "no-store", content: "text/html"},
		{name: "spa route uses current index", path: "/team", status: http.StatusOK, body: "app-shell", cache: "no-store", content: "text/html"},
		{name: "hashed asset is immutable", path: "/assets/index-abc.js", status: http.StatusOK, body: "export const ready = true", cache: "public, max-age=31536000, immutable", content: "text/javascript"},
		{name: "missing asset is not html", path: "/assets/missing-old.js", status: http.StatusNotFound, body: "404 page not found", cache: "no-store", content: "text/plain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)
			response := httptest.NewRecorder()
			server.serveWeb(response, request)
			if response.Code != tt.status {
				t.Fatalf("GET %s = %d, want %d", tt.path, response.Code, tt.status)
			}
			if body := strings.TrimSpace(response.Body.String()); body != tt.body {
				t.Fatalf("GET %s body = %q, want %q", tt.path, body, tt.body)
			}
			if cache := response.Header().Get("Cache-Control"); cache != tt.cache {
				t.Fatalf("GET %s Cache-Control = %q, want %q", tt.path, cache, tt.cache)
			}
			if content := response.Header().Get("Content-Type"); !strings.Contains(content, tt.content) {
				t.Fatalf("GET %s Content-Type = %q, want %q", tt.path, content, tt.content)
			}
		})
	}
}

func TestCanonicalEventType(t *testing.T) {
	tests := map[string]string{
		"hub/live":            "loom/live",
		"hub/session-created": "loom/agent-created",
		"hub/session-status":  "loom/agent-status",
		"hub/session-killed":  "loom/agent-archived",
		"hub/turn-started":    "loom/turn-started",
		"item/completed":      "item/completed",
	}
	for input, want := range tests {
		if got := canonicalEventType(input); got != want {
			t.Errorf("canonicalEventType(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRestartDrainAllowsCausalReplyButRejectsNewRootWork(t *testing.T) {
	if !isDrainCompletionMessage(hub.CommParams{ReplyTo: "msg_required"}) {
		t.Fatal("causal reply was not treated as drain completion")
	}
	if isDrainCompletionMessage(hub.CommParams{To: "other-agent", Subject: "new work"}) {
		t.Fatal("new root message was treated as drain completion")
	}
}
