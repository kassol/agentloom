package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"testing/fstest"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestHumanRequestHTTPAndSSECausalitySurvivesRestart(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", t.TempDir()+"/missing.json")
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAgents(map[string]*hub.Agent{
		"release": {
			ID: "release", Name: "release-agent", Cwd: t.TempDir(), ThreadID: "loom-thread-release",
			RuntimeBinding: hub.RuntimeBinding{Kind: "codex", NativeRef: "native-thread-release"}, Status: "idle",
			CurrentTurnID: "turn-upload", CurrentTask: "Upload the release", CreatedAt: nowForTest(), UpdatedAt: nowForTest(),
		},
	}); err != nil {
		t.Fatal(err)
	}
	h, err := hub.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	web := fstest.MapFS{"index.html": {Data: []byte("ok")}}
	handler := New(h, st, web).Handler()
	cursor := h.LastGlobalSeq()
	created := topicRequest(t, handler, http.MethodPost, "/api/human-requests", map[string]any{
		"agent": "release-agent", "question": "Did the upload complete?",
		"context": "The Runtime stopped after starting the upload.",
	}, http.StatusCreated)["request"].(map[string]any)
	requestID := created["id"].(string)
	if created["threadId"] != "loom-thread-release" || created["sourceTurnId"] != "turn-upload" || created["blockedWork"] != "Upload the release" {
		t.Fatalf("created request causality = %#v", created)
	}
	events := readGlobalSSE(t, h, "/api/events?since="+strconv.FormatInt(cursor, 10), 1)
	assertHumanRequestEvent(t, events[0], requestID, "open", "waiting")

	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := hub.Open(reopened)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Shutdown()
	restartHandler := New(restarted, reopened, web).Handler()
	detail := topicRequest(t, restartHandler, http.MethodGet, "/api/human-requests/"+requestID, nil, http.StatusOK)["request"].(map[string]any)
	if detail["agentId"] != "release" || detail["threadId"] != "loom-thread-release" || detail["sourceTurnId"] != "turn-upload" {
		t.Fatalf("restarted request = %#v", detail)
	}

	cursor = restarted.LastGlobalSeq()
	answered := topicRequest(t, restartHandler, http.MethodPost, "/api/human-requests/"+requestID+"/answer", map[string]any{
		"answer": "The upload is visible; continue verification.",
	}, http.StatusAccepted)["request"].(map[string]any)
	if answered["state"] != "answered" || answered["deliveryStatus"] != "queued" {
		t.Fatalf("answered request = %#v", answered)
	}
	events = readGlobalSSE(t, restarted, "/api/events?since="+strconv.FormatInt(cursor, 10), 1)
	assertHumanRequestEvent(t, events[0], requestID, "answered", "queued")
}

func assertHumanRequestEvent(t *testing.T, event store.Event, id, state, delivery string) {
	t.Helper()
	if event.Type != "loom/human-request" {
		t.Fatalf("Human Request SSE type = %q", event.Type)
	}
	var payload struct {
		Request hub.HumanRequest `json:"request"`
	}
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Request.ID != id || payload.Request.State != state || payload.Request.DeliveryStatus != delivery {
		t.Fatalf("Human Request SSE payload = %#v", payload.Request)
	}
}
