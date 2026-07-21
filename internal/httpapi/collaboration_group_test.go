package httpapi

import (
	"net/http"
	"testing"
	"testing/fstest"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestCollaborationGroupHTTPRoundTrip(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", t.TempDir()+"/missing.json")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAgents(map[string]*hub.Agent{
		"one": {ID: "one", Name: "one", Cwd: t.TempDir(), ThreadID: "thread-one", Status: "idle"},
		"two": {ID: "two", Name: "two", Cwd: t.TempDir(), ThreadID: "thread-two", Status: "idle"},
	}); err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	server := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	relationship := topicRequest(t, server, http.MethodPost, "/api/team/relationships", map[string]any{
		"from": "one", "to": "two", "description": "A stable interface.",
	}, http.StatusCreated)["relationship"].(map[string]any)
	created := topicRequest(t, server, http.MethodPost, "/api/team/collaboration-groups", map[string]any{
		"name": "Shared", "description": "A named view.", "memberAgentIds": []string{"one", "two"},
		"relationshipIds": []string{relationship["id"].(string)},
	}, http.StatusCreated)
	group := created["group"].(map[string]any)
	groupID := group["id"].(string)

	listed := topicRequest(t, server, http.MethodGet, "/api/team/collaboration-groups?status=active", nil, http.StatusOK)
	if len(listed["groups"].([]any)) != 1 {
		t.Fatalf("groups = %#v", listed)
	}
	topicRequest(t, server, http.MethodPatch, "/api/team/collaboration-groups/"+groupID, map[string]any{
		"description": "Updated named view.", "expectedVersion": 1,
	}, http.StatusOK)
	topicRequest(t, server, http.MethodDelete, "/api/team/collaboration-groups/"+groupID+"?expectedVersion=1", nil, http.StatusConflict)
	deleted := topicRequest(t, server, http.MethodDelete, "/api/team/collaboration-groups/"+groupID+"?expectedVersion=2", nil, http.StatusOK)
	if deleted["group"].(map[string]any)["id"] != groupID {
		t.Fatalf("deleted group = %#v", deleted)
	}
	topicRequest(t, server, http.MethodGet, "/api/team/collaboration-groups/"+groupID, nil, http.StatusNotFound)
}
