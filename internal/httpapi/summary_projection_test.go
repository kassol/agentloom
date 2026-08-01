package httpapi

import (
	"net/http"
	"testing"
	"testing/fstest"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestWorkspaceSummaryEndpointsKeepDetailOutOfListResponses(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", t.TempDir()+"/missing.json")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	managedContext := `<loom_context version="1"><payload>` + string(make([]byte, 2_048)) + `</payload></loom_context>`
	if err := st.SaveAgents(map[string]*hub.Agent{
		"lead": {
			ID: "lead", Name: "summary-lead", Cwd: t.TempDir(), ThreadID: "thread-lead", Status: "idle",
			CurrentTask:     "Review the candidate\n\n" + managedContext,
			LastTurn:        &hub.TurnSummary{TurnID: "turn-1", Task: "Completed work\n\n" + managedContext, Status: "completed"},
			ProviderHistory: []hub.ProviderBindingChange{{PreviousProviderID: "deepseek", ProviderID: "", SwitchedAt: "2026-08-01T00:00:00Z"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	server := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	agentSummary := topicRequest(t, server, http.MethodGet, "/api/agents?view=summary", nil, http.StatusOK)["agents"].([]any)[0].(map[string]any)
	if agentSummary["currentTask"] != "Review the candidate" {
		t.Fatalf("summary currentTask = %#v", agentSummary["currentTask"])
	}
	if _, exists := agentSummary["providerHistory"]; exists {
		t.Fatalf("summary leaked providerHistory: %#v", agentSummary)
	}
	fullAgent := topicRequest(t, server, http.MethodGet, "/api/agents/lead", nil, http.StatusOK)["agent"].(map[string]any)
	if _, exists := fullAgent["providerHistory"]; !exists {
		t.Fatalf("full Agent lost providerHistory: %#v", fullAgent)
	}

	created := topicRequest(t, server, http.MethodPost, "/api/topics", map[string]any{
		"title": "Summary boundary", "purpose": "Keep list payload bounded", "completionBoundary": "Detail remains available",
		"responsibleAgent": "summary-lead", "createdBy": "owner",
		"initialBrief": map[string]any{
			"summary": "Current state", "currentState": "State detail", "nextStep": "Review detail",
			"evidence": []map[string]any{{"type": "artifact", "id": "art_private"}},
		},
	}, http.StatusCreated)
	topicID := created["topic"].(map[string]any)["id"].(string)
	topicRequest(t, server, http.MethodPost, "/api/topics/"+topicID+"/links", map[string]any{
		"actor": "summary-lead", "type": "artifact", "id": "art_private", "relation": "evidence",
	}, http.StatusOK)

	topicSummary := topicRequest(t, server, http.MethodGet, "/api/topics?view=summary", nil, http.StatusOK)["topics"].([]any)[0].(map[string]any)
	for _, field := range []string{"events", "briefHistory", "links", "participants", "completionBoundary"} {
		if _, exists := topicSummary[field]; exists {
			t.Fatalf("Topic summary includes %s: %#v", field, topicSummary[field])
		}
	}
	brief := topicSummary["currentBrief"].(map[string]any)
	if _, exists := brief["evidence"]; exists {
		t.Fatalf("Topic summary includes brief evidence: %#v", brief)
	}
	for _, field := range []string{"currentState", "limitations"} {
		if _, exists := brief[field]; exists {
			t.Fatalf("Topic summary includes %s: %#v", field, brief[field])
		}
	}
	fullTopic := topicRequest(t, server, http.MethodGet, "/api/topics/"+topicID, nil, http.StatusOK)["topic"].(map[string]any)
	if _, exists := fullTopic["events"]; !exists {
		t.Fatalf("full Topic lost events: %#v", fullTopic)
	}
	if _, exists := fullTopic["links"]; !exists {
		t.Fatalf("full Topic lost links: %#v", fullTopic)
	}

	createdRequest := topicRequest(t, server, http.MethodPost, "/api/human-requests", map[string]any{
		"agent": "summary-lead", "expectation": "required", "question": "Approve this boundary?",
		"context": "private decision context", "blockedWork": "private blocked work",
	}, http.StatusCreated)
	requestID := createdRequest["request"].(map[string]any)["id"].(string)
	requestSummary := topicRequest(t, server, http.MethodGet, "/api/human-requests?view=summary", nil, http.StatusOK)["requests"].([]any)[0].(map[string]any)
	for _, field := range []string{"sourceTask", "context", "blockedWork", "options", "answer", "lastError", "threadId"} {
		if _, exists := requestSummary[field]; exists {
			t.Fatalf("Human Request summary includes %s: %#v", field, requestSummary[field])
		}
	}
	fullRequest := topicRequest(t, server, http.MethodGet, "/api/human-requests/"+requestID, nil, http.StatusOK)["request"].(map[string]any)
	if fullRequest["context"] != "private decision context" || fullRequest["blockedWork"] != "private blocked work" {
		t.Fatalf("full Human Request lost detail: %#v", fullRequest)
	}
}
