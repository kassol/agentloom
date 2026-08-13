package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestAgentDetailUsesCapabilitySnapshotAsOnlyRuntimeCapabilitySurface(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	if _, err := h.RestoreAgent(hub.RestoreAgentParams{
		ID: "agent-pi", Name: "pi-worker", Cwd: t.TempDir(), ThreadID: "loom-thread-pi",
		RuntimeBinding: hub.RuntimeBinding{Kind: "pi", NativeRef: "/tmp/pi-session.jsonl"},
	}); err != nil {
		t.Fatal(err)
	}
	server := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	response := topicRequest(t, server, http.MethodGet, "/api/agents/agent-pi", nil, http.StatusOK)
	agent := response["agent"].(map[string]any)
	if _, exists := agent["runtimeCapabilities"]; exists {
		t.Fatalf("Agent detail retained obsolete flat Runtime capabilities: %#v", agent)
	}
	snapshot, ok := agent["capabilitySnapshot"].(map[string]any)
	if !ok || snapshot["revision"] == "" || len(snapshot["capabilities"].([]any)) == 0 {
		t.Fatalf("Agent detail omitted scoped capability snapshot: %#v", agent)
	}
	for _, raw := range snapshot["capabilities"].([]any) {
		capability := raw.(map[string]any)
		if capability["id"] == "provider_configuration" {
			t.Fatalf("Agent detail exposed Host Provider administration as a per-Agent Runtime capability: %#v", snapshot)
		}
	}
}

func TestNativeConversationDivergenceRecoveryRouteAdvancesPublicBoundary(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent := &hub.Agent{
		ID: "agent-claude-diverged", Name: "claude-diverged", Cwd: t.TempDir(), ThreadID: "loom-thread-claude",
		RuntimeBinding:  hub.RuntimeBinding{SchemaVersion: hub.RuntimeBindingSchemaVersion, Kind: "claude", NativeRef: "private-session"},
		HistoryBoundary: &hub.HistoryBoundary{Kind: "history_boundary", CreatedAt: nowForTest(), ImportedTurns: 0, Disclosure: "Existing native content remains outside Loom history.", NativeRevision: "private-old"},
		NativeConversationDivergence: &hub.NativeConversationDivergence{
			Code: "native_conversation_divergence", DetectedAt: nowForTest(), Summary: "Native context changed.", Recovery: "Accept current context.", NativeRevision: "private-new",
		},
		RuntimeConfiguration: hub.RuntimeConfiguration{Configured: true, SettingSources: []string{"project"}, Authentication: hub.RuntimeAuthentication{Category: hub.RuntimeAuthConsole, Source: "api_key"}},
		RuntimeTurnBindings:  map[string]string{}, Status: "fenced", CreatedAt: nowForTest(), UpdatedAt: nowForTest(),
	}
	if err := st.SaveAgents(map[string]*hub.Agent{agent.ID: agent}); err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	server := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	response := topicRequest(t, server, http.MethodPost, "/api/agents/"+agent.ID+"/runtime/conversation/recover", map[string]any{}, http.StatusOK)
	public := response["agent"].(map[string]any)
	if public["status"] != "idle" || public["nativeConversationDivergence"] != nil {
		t.Fatalf("recovered Agent = %#v", public)
	}
	boundary := public["historyBoundary"].(map[string]any)
	if boundary["nativeRevision"] != nil {
		t.Fatalf("public boundary leaked private revision: %#v", boundary)
	}
}

func TestUnsupportedAgentOperationsReturnConflict(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	if _, err := h.RestoreAgent(hub.RestoreAgentParams{
		ID: "agent-pi", Name: "pi-worker", Cwd: t.TempDir(), ThreadID: "loom-thread-pi",
		RuntimeBinding: hub.RuntimeBinding{Kind: "pi", NativeRef: "/tmp/pi-session.jsonl"},
	}); err != nil {
		t.Fatal(err)
	}
	server := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	tests := []struct {
		name, method, path, body, capability string
	}{
		{name: "sandbox config", method: http.MethodPatch, path: "/api/agents/agent-pi/config", body: `{"sandbox":"read-only"}`, capability: "sandbox"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusConflict || !strings.Contains(strings.ToLower(response.Body.String()), strings.ToLower(tt.capability)) {
				t.Fatalf("%s %s = %d %s, want explicit unsupported %s conflict", tt.method, tt.path, response.Code, response.Body.String(), tt.capability)
			}
		})
	}
	response := topicRequest(t, server, http.MethodGet, "/api/agents/agent-pi/goal", nil, http.StatusOK)
	if response["goal"] != nil || response["revision"].(float64) != 0 {
		t.Fatalf("initial Pi Goal = %#v", response)
	}
	topicRequest(t, server, http.MethodPut, "/api/agents/agent-pi/goal", map[string]any{"objective": "missing revision"}, http.StatusBadRequest)
	topicRequest(t, server, http.MethodDelete, "/api/agents/agent-pi/goal", nil, http.StatusBadRequest)
	response = topicRequest(t, server, http.MethodPut, "/api/agents/agent-pi/goal", map[string]any{"objective": "ship", "expectedVersion": 0}, http.StatusOK)
	goal := response["goal"].(map[string]any)
	if goal["objective"] != "ship" || goal["nativeSyncState"] != "notApplicable" {
		t.Fatalf("Pi Goal = %#v", goal)
	}
	revision := int64(goal["version"].(float64))
	response = topicRequest(t, server, http.MethodDelete, "/api/agents/agent-pi/goal?expectedVersion="+strconv.FormatInt(revision, 10), nil, http.StatusOK)
	if response["cleared"] != true || int64(response["revision"].(float64)) != revision+1 {
		t.Fatalf("cleared Pi Goal = %#v", response)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/agents/agent-pi/provider", strings.NewReader(`{"providerId":"xai","model":"grok"}`))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code == http.StatusOK {
		t.Fatalf("obsolete per-Agent Provider authority is still reachable: %d %s", recorder.Code, recorder.Body.String())
	}
	response = topicRequest(t, server, http.MethodPatch, "/api/agents/agent-pi/config", map[string]any{"approvalPolicy": "on-request"}, http.StatusOK)
	agent := response["agent"].(map[string]any)
	if agent["approvalPolicy"] != "on-request" {
		t.Fatalf("Pi Approval policy = %#v", agent["approvalPolicy"])
	}
}

func TestRuntimeConfigurationRoutesPreserveCapabilityAndRevisionFailures(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	if _, err := h.RestoreAgent(hub.RestoreAgentParams{
		ID: "agent-pi-config", Name: "pi-config", Cwd: t.TempDir(), ThreadID: "loom-thread-pi-config",
		RuntimeBinding: hub.RuntimeBinding{Kind: "pi", NativeRef: "/tmp/pi-session.jsonl"},
	}); err != nil {
		t.Fatal(err)
	}
	server := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	request := httptest.NewRequest(http.MethodGet, "/api/agents/agent-pi-config/runtime/configuration", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "owner configuration") {
		t.Fatalf("GET Runtime configuration = %d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPatch, "/api/agents/agent-pi-config/runtime/configuration", strings.NewReader(`{"configuration":{}}`))
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "expectedRevision") {
		t.Fatalf("PATCH Runtime configuration without revision = %d %s", response.Code, response.Body.String())
	}
}
