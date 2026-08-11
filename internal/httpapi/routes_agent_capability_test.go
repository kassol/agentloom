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

func TestPiUnsupportedAgentOperationsReturnConflict(t *testing.T) {
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
		{name: "usage", method: http.MethodGet, path: "/api/agents/agent-pi/usage?days=7", capability: "usage"},
		{name: "manual compaction", method: http.MethodPost, path: "/api/agents/agent-pi/compact", capability: "compaction"},
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
