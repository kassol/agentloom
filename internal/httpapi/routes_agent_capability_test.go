package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestAgentDetailReportsTruthfulPiRuntimeCapabilities(t *testing.T) {
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
	capabilities := agent["runtimeCapabilities"].(map[string]any)
	for _, capability := range []string{"history", "causalSteer", "interrupt", "approval", "provider"} {
		if capabilities[capability] != true {
			t.Fatalf("Pi %s capability = %#v", capability, capabilities[capability])
		}
	}
	for _, capability := range []string{"goal", "remote", "usage", "compaction", "skills", "naming", "archive", "sandbox", "imageInput"} {
		if value, exists := capabilities[capability]; !exists || value != false {
			t.Fatalf("Pi %s capability = %#v, exists=%v", capability, value, exists)
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
		{name: "read Goal", method: http.MethodGet, path: "/api/agents/agent-pi/goal", capability: "Goal"},
		{name: "update Goal", method: http.MethodPut, path: "/api/agents/agent-pi/goal", body: `{"objective":"ship"}`, capability: "Goal"},
		{name: "clear Goal", method: http.MethodDelete, path: "/api/agents/agent-pi/goal", capability: "Goal"},
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
	request := httptest.NewRequest(http.MethodPost, "/api/agents/agent-pi/runtime/model", strings.NewReader(`{"provider":"xai"}`))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "provider and model are required") {
		t.Fatalf("invalid Pi Runtime model switch = %d %s", recorder.Code, recorder.Body.String())
	}
	response := topicRequest(t, server, http.MethodPatch, "/api/agents/agent-pi/config", map[string]any{"approvalPolicy": "on-request"}, http.StatusOK)
	agent := response["agent"].(map[string]any)
	if agent["approvalPolicy"] != "on-request" {
		t.Fatalf("Pi Approval policy = %#v", agent["approvalPolicy"])
	}
}
