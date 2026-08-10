package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestPiResponsiblePublishesIntegratedTopicResultAcrossHTTPSSERestart(t *testing.T) {
	t.Setenv("PI_BIN", t.TempDir()+"/missing-pi")
	t.Setenv("CODEX_BIN", t.TempDir()+"/missing-codex")
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	const piSession = "/loom/private/pi-session-native.jsonl"
	const piNativeTurn = "pi-user-entry-native"
	if err := st.SaveAgents(map[string]*hub.Agent{
		"pi-lead": {
			ID: "pi-lead", Name: "pi-topic-lead", ThreadID: "loom-thread-pi", Status: "running",
			RuntimeBinding:      hub.RuntimeBinding{Kind: "pi", NativeRef: piSession},
			RuntimeTurnBindings: map[string]string{"turn_public": piNativeTurn},
		},
		"codex-edge": {
			ID: "codex-edge", Name: "codex-participant", ThreadID: "loom-thread-codex", Status: "running",
			RuntimeBinding: hub.RuntimeBinding{Kind: "codex", NativeRef: "codex-thread-native"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	web := fstest.MapFS{"index.html": {Data: []byte("ok")}}
	handler := New(h, st, web).Handler()

	created := topicRequest(t, handler, http.MethodPost, "/api/topics", map[string]any{
		"title": "Integrated mixed Runtime result", "purpose": "Integrate bounded participant evidence",
		"completionBoundary": "The Responsible Agent publishes the reconciled result",
		"responsibleAgent":   "pi-lead", "createdBy": "pi-lead",
		"participants": []map[string]any{{"agent": "codex-edge", "responsibility": "Return bounded client evidence"}},
		"initialBrief": map[string]any{"summary": "Participant evidence is pending"},
	}, http.StatusCreated)["topic"].(map[string]any)
	topicID := created["id"].(string)

	request := topicRequest(t, handler, http.MethodPost, "/api/comms/messages", map[string]any{
		"from": "pi-lead", "to": "codex-edge", "topicId": topicID,
		"subject": "Verify client boundary", "body": "Return the bounded evidence.", "response": "required",
	}, http.StatusAccepted)["message"].(map[string]any)
	reply := topicRequest(t, handler, http.MethodPost, "/api/comms/messages", map[string]any{
		"from": "codex-edge", "replyTo": request["id"], "body": "Client evidence is verified.",
	}, http.StatusAccepted)["message"].(map[string]any)
	topicRequest(t, handler, http.MethodPatch, "/api/topics/"+topicID, map[string]any{
		"actor": "pi-lead", "expectedVersion": 1,
		"waitingOn": map[string]any{
			"kind": "agent-message", "refId": reply["id"], "summary": "Waiting to reconcile participant evidence",
			"resumeAction": "Integrate the verified reply",
		},
	}, http.StatusOK)
	topicRequest(t, handler, http.MethodPatch, "/api/topics/"+topicID, map[string]any{
		"actor": "pi-lead", "expectedVersion": 2, "clearWaiting": true,
	}, http.StatusOK)

	topicRequest(t, handler, http.MethodPatch, "/api/topics/"+topicID, map[string]any{
		"actor": "codex-edge", "expectedVersion": 3,
		"brief": map[string]any{"summary": "Participant tried to bypass integration"}, "publishResult": true,
	}, http.StatusForbidden)
	intermediate := topicRequest(t, handler, http.MethodGet, "/api/topics/"+topicID, nil, http.StatusOK)["topic"].(map[string]any)
	if intermediate["resultsReady"] == true || intermediate["currentBrief"].(map[string]any)["summary"] != "Participant evidence is pending" {
		t.Fatalf("Participant reply crossed result boundary: %#v", intermediate)
	}

	cursor := h.LastGlobalSeq()
	progress := topicRequest(t, handler, http.MethodPatch, "/api/topics/"+topicID, map[string]any{
		"actor": "pi-lead", "expectedVersion": 3,
		"brief": map[string]any{
			"summary":      "Codex participant evidence has been integrated into progress.",
			"currentState": "Client boundary verified", "nextStep": "Reconcile final limitations",
			"evidence": []map[string]any{{"type": "message", "id": reply["id"], "label": "Participant evidence"}},
		},
	}, http.StatusOK)["topic"].(map[string]any)
	if progress["resultsReady"] == true || progress["version"] != float64(4) {
		t.Fatalf("progress crossed final result boundary: %#v", progress)
	}
	final := topicRequest(t, handler, http.MethodPatch, "/api/topics/"+topicID, map[string]any{
		"actor": "pi-lead", "expectedVersion": 4, "publishResult": true,
		"brief": map[string]any{
			"summary":      "Final integrated result: the client boundary is verified.",
			"currentState": "Ready for Owner review", "limitations": "Server rollout remains outside this Topic.",
			"evidence": []map[string]any{{"type": "message", "id": reply["id"], "label": "Participant evidence"}},
		},
	}, http.StatusOK)["topic"].(map[string]any)
	if final["resultsReady"] != true || final["responsibleAgentId"] != "pi-lead" || final["resultReadyVersion"] != float64(3) {
		t.Fatalf("final Pi Topic result = %#v", final)
	}

	events := readGlobalSSE(t, h, "/api/events?since="+jsonNumber(cursor), 4)
	canonical := make([]store.Event, 0, 2)
	for index, event := range events {
		if event.Type != "loom/topic-updated" && event.Type != "hub/topic-updated" {
			t.Fatalf("publication SSE[%d] = %#v", index, event)
		}
		encoded := string(event.Data)
		if containsAny(encoded, piSession, piNativeTurn, "codex-thread-native") {
			t.Fatalf("publication SSE leaked Runtime-native identity: %s", encoded)
		}
		if event.Type == "loom/topic-updated" {
			canonical = append(canonical, event)
		}
	}
	if len(canonical) != 2 || !containsAny(string(canonical[0].Data), "brief_updated") || !containsAny(string(canonical[1].Data), "result_published") {
		t.Fatalf("publication SSE lost progress/result audit: %#v", canonical)
	}

	view := topicRequest(t, handler, http.MethodGet, "/api/topics/"+topicID, nil, http.StatusOK)["topic"].(map[string]any)
	assertCompleteTopicAudit(t, view, request["id"].(string), reply["id"].(string))
	encodedView, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if containsAny(string(encodedView), piSession, piNativeTurn, "codex-thread-native") {
		t.Fatalf("public Topic leaked Runtime-native identity: %s", encodedView)
	}

	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := hub.New(reopened)
	defer restarted.Shutdown()
	restartHandler := New(restarted, reopened, web).Handler()
	restored := topicRequest(t, restartHandler, http.MethodGet, "/api/topics/"+topicID, nil, http.StatusOK)["topic"].(map[string]any)
	if restored["resultsReady"] != true || restored["responsibleAgentId"] != "pi-lead" || restored["resultReadyVersion"] != float64(3) {
		t.Fatalf("restarted Pi Topic result = %#v", restored)
	}
	assertCompleteTopicAudit(t, restored, request["id"].(string), reply["id"].(string))
	restoredJSON, err := json.Marshal(restored)
	if err != nil {
		t.Fatal(err)
	}
	if containsAny(string(restoredJSON), piSession, piNativeTurn, "codex-thread-native") {
		t.Fatalf("restarted public Topic leaked Runtime-native identity: %s", restoredJSON)
	}
}

func assertCompleteTopicAudit(t *testing.T, topic map[string]any, requestID, replyID string) {
	t.Helper()
	wantTypes := map[string]bool{
		"created": false, "message_created": false, "message_replied": false,
		"waiting": false, "waiting_cleared": false, "brief_updated": false, "result_published": false,
	}
	refs := map[string]bool{}
	for _, raw := range topic["events"].([]any) {
		event := raw.(map[string]any)
		if eventType, ok := event["type"].(string); ok {
			if _, wanted := wantTypes[eventType]; wanted {
				wantTypes[eventType] = true
			}
		}
		if ref, ok := event["ref"].(map[string]any); ok {
			refs[ref["id"].(string)] = true
		}
	}
	for eventType, found := range wantTypes {
		if !found {
			t.Fatalf("Topic audit missing %s: %#v", eventType, topic["events"])
		}
	}
	if !refs[requestID] || !refs[replyID] {
		t.Fatalf("Topic audit lost participant Message evidence: refs=%v", refs)
	}
}

func jsonNumber(value int64) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if candidate != "" && strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
