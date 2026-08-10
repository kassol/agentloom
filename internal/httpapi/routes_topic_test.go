package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestTopicHTTPRoundTripAndResultBoundary(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", t.TempDir()+"/missing.json")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAgents(map[string]*hub.Agent{
		"lead": {ID: "lead", Name: "parall-dev-lead", Cwd: t.TempDir(), ThreadID: "loom-thread-lead", RuntimeBinding: hub.RuntimeBinding{Kind: "codex", NativeRef: "thread-lead"}, Status: "idle"},
		"edge": {ID: "edge", Name: "parall-edge-dev", Cwd: t.TempDir(), ThreadID: "loom-thread-edge", RuntimeBinding: hub.RuntimeBinding{Kind: "codex", NativeRef: "thread-edge"}, Status: "idle"},
	}); err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	server := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	created := topicRequest(t, server, http.MethodPost, "/api/topics", map[string]any{
		"title": "Parall Clip 0.2.0", "purpose": "Keep delivery state recoverable", "completionBoundary": "Shared staging passes",
		"responsibleAgent": "parall-dev-lead", "createdBy": "owner",
		"participants": []map[string]any{{"agent": "parall-edge-dev", "responsibility": "Own packaged smoke"}},
		"initialBrief": map[string]any{"summary": "Candidate frozen"},
	}, http.StatusCreated)
	topic := created["topic"].(map[string]any)
	topicID := topic["id"].(string)
	if topic["responsibleAgent"] != "parall-dev-lead" || len(topic["participants"].([]any)) != 1 {
		t.Fatalf("created Topic = %#v", topic)
	}

	listed := topicRequest(t, server, http.MethodGet, "/api/topics?agent=parall-edge-dev", nil, http.StatusOK)
	if len(listed["topics"].([]any)) != 1 {
		t.Fatalf("Topic list = %#v", listed)
	}

	topicRequest(t, server, http.MethodPatch, "/api/topics/"+topicID, map[string]any{
		"actor": "owner", "expectedVersion": 1, "brief": map[string]any{"summary": "Owner correction"}, "publishResult": true,
	}, http.StatusForbidden)
	topicRequest(t, server, http.MethodPatch, "/api/topics/"+topicID, map[string]any{
		"actor": "owner", "expectedVersion": 1, "brief": map[string]any{"summary": "Owner bypass"},
	}, http.StatusForbidden)
	topicRequest(t, server, http.MethodPost, "/api/topics/"+topicID+"/participants", map[string]any{
		"actor": "owner", "agent": "parall-edge-dev", "responsibility": "Owner bypass",
	}, http.StatusForbidden)

	updated := topicRequest(t, server, http.MethodPatch, "/api/topics/"+topicID, map[string]any{
		"actor": "parall-dev-lead", "expectedVersion": 1, "brief": map[string]any{"summary": "Stage ready for Owner"}, "publishResult": true,
	}, http.StatusOK)
	if ready, _ := updated["topic"].(map[string]any)["resultsReady"].(bool); !ready {
		t.Fatalf("Responsible result was not ready: %#v", updated)
	}
}

func TestPiResponsibleTopicWaitingAndResumeStayRuntimeNeutralOverHTTP(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", t.TempDir()+"/missing.json")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAgents(map[string]*hub.Agent{
		"pi-lead": {
			ID: "pi-lead", Name: "pi-topic-lead", Cwd: t.TempDir(), ThreadID: "loom-thread-pi-lead",
			RuntimeBinding: hub.RuntimeBinding{Kind: "pi", NativeRef: "/loom/pi/session.jsonl"}, Status: "idle",
		},
		"codex-edge": {
			ID: "codex-edge", Name: "codex-edge", Cwd: t.TempDir(), ThreadID: "loom-thread-codex-edge",
			RuntimeBinding: hub.RuntimeBinding{Kind: "codex", NativeRef: "codex-thread"}, Status: "idle",
		},
	}); err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	server := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	created := topicRequest(t, server, http.MethodPost, "/api/topics", map[string]any{
		"title": "Mixed Runtime coordination", "purpose": "Let Pi integrate bounded Codex evidence", "completionBoundary": "Evidence is integrated",
		"responsibleAgent": "pi-lead", "createdBy": "pi-lead",
		"participants": []map[string]any{{"agent": "codex-edge", "responsibility": "Return client evidence only"}},
		"initialBrief": map[string]any{"summary": "Pi owns integration"},
	}, http.StatusCreated)["topic"].(map[string]any)
	topicID := created["id"].(string)
	if created["responsibleAgentId"] != "pi-lead" || created["status"] != hub.TopicStatusActive {
		t.Fatalf("created Pi Topic = %#v", created)
	}

	waiting := topicRequest(t, server, http.MethodPatch, "/api/topics/"+topicID, map[string]any{
		"actor": "pi-lead", "expectedVersion": 1,
		"waitingOn": map[string]any{"kind": "agent-message", "refId": "msg_codex", "summary": "Waiting for Codex evidence", "resumeAction": "Integrate the reply"},
	}, http.StatusOK)["topic"].(map[string]any)
	if waiting["status"] != hub.TopicStatusWaiting || waiting["waitingOn"].(map[string]any)["refId"] != "msg_codex" {
		t.Fatalf("waiting Pi Topic = %#v", waiting)
	}

	resumed := topicRequest(t, server, http.MethodPatch, "/api/topics/"+topicID, map[string]any{
		"actor": "pi-lead", "expectedVersion": 2, "clearWaiting": true,
	}, http.StatusOK)["topic"].(map[string]any)
	if resumed["status"] != hub.TopicStatusActive || resumed["waitingOn"] != nil || resumed["responsibleAgentId"] != "pi-lead" {
		t.Fatalf("resumed Pi Topic = %#v", resumed)
	}
}

func topicRequest(t *testing.T, handler http.Handler, method, path string, body any, wantStatus int) map[string]any {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, &payload)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != wantStatus {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, response.Code, wantStatus, response.Body.String())
	}
	result := map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode %s %s: %v (%s)", method, path, err, response.Body.String())
	}
	return result
}
