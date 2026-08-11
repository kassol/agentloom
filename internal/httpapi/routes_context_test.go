package httpapi

import (
	"net/http"
	"testing"
	"testing/fstest"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestContextHTTPPromptAndCoverageRoundTrip(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", t.TempDir()+"/missing.json")
	t.Setenv("CODEX_SESSIONS_DIR", t.TempDir())
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAgents(map[string]*hub.Agent{
		"one": {ID: "one", Name: "one", Cwd: t.TempDir(), ThreadID: "loom-thread-one", RuntimeBinding: hub.RuntimeBinding{Kind: "codex", NativeRef: "thread-one"}, Status: "idle"},
	}); err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	server := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	builtin := topicRequest(t, server, http.MethodGet, "/api/context/agent-prompt", nil, http.StatusOK)["prompt"].(map[string]any)
	if builtin["source"] != "builtin" || builtin["version"] != float64(2) {
		t.Fatalf("builtin prompt = %#v", builtin)
	}
	owner := topicRequest(t, server, http.MethodPut, "/api/context/agent-prompt", map[string]any{
		"content":         httpTestLoomPromptTemplate("Stable Owner-managed Loom semantics."),
		"expectedVersion": 2,
	}, http.StatusOK)["prompt"].(map[string]any)
	if owner["source"] != "owner" || owner["version"] != float64(3) {
		t.Fatalf("owner prompt = %#v", owner)
	}
	explain := topicRequest(t, server, http.MethodGet, "/api/agents/one/context/explain", nil, http.StatusOK)["context"].(map[string]any)
	if len(explain["sources"].([]any)) != 3 {
		t.Fatalf("context explain = %#v", explain)
	}
	missing := topicRequest(t, server, http.MethodGet, "/api/agents/one/context/explain?turnId=turn-missing", nil, http.StatusOK)["context"].(map[string]any)
	if missing["state"] != "unknown" || missing["turnId"] != "turn-missing" {
		t.Fatalf("missing Turn context evidence = %#v", missing)
	}
	coverage := topicRequest(t, server, http.MethodGet, "/api/agents/one/context/coverage", nil, http.StatusOK)["coverage"].(map[string]any)
	if coverage["threadId"] != "loom-thread-one" || coverage["epoch"].(map[string]any)["id"] != "initial:thread-one" {
		t.Fatalf("coverage = %#v", coverage)
	}
	cleared := topicRequest(t, server, http.MethodDelete, "/api/context/agent-prompt?expectedVersion=3", nil, http.StatusOK)["prompt"].(map[string]any)
	if cleared["source"] != "builtin" || cleared["version"] != float64(2) {
		t.Fatalf("cleared prompt = %#v", cleared)
	}
}

func httpTestLoomPromptTemplate(body string) string {
	return body + `
<loom_agent_profile_data version="1"
  revision="{{ .AgentProfileRevisionXMLAttr }}"
  agent_id="{{ .AgentIDXMLAttr }}"
  name="{{ .AgentNameXMLAttr }}"
  complete="true"
  supersedes_previous="true"
  declarative_not_authorization="true">
  <identity><![CDATA[{{ .ProfileIdentityCDATA }}]]></identity>
  <domain><![CDATA[{{ .ProfileDomainCDATA }}]]></domain>
  <scope><![CDATA[{{ .ProfileScopeCDATA }}]]></scope>
</loom_agent_profile_data>`
}
