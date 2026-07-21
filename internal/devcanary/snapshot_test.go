package devcanary

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateFilteredSnapshot(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "data")
	writeFixture(t, filepath.Join(source, "agents.json"), `{
  "a1":{"id":"a1","name":"alpha"},
  "a2":{"id":"a2","name":"beta"}
}`)
	writeFixture(t, filepath.Join(source, "profiles.json"), `{
  "a1":{"agentId":"a1","domain":"one"},
  "a2":{"agentId":"a2","domain":"two"}
}`)
	writeFixture(t, filepath.Join(source, "team-links.json"), `{
  "r1":{"id":"r1","fromAgentId":"a1","toAgentId":"a2","from":"alpha","to":"beta","description":"handoff"}
}`)
	writeFixture(t, filepath.Join(source, "collaboration-groups.json"), `{
	  "g1":{"id":"g1","name":"Shared","description":"shared group","status":"active","memberAgentIds":["a1","a2"],"relationshipIds":["r1"],"version":1},
	  "g2":{"id":"g2","name":"Beta only","description":"other group","status":"active","memberAgentIds":["a2"],"relationshipIds":[],"version":1},
	  "g3":{"id":"g3","name":"Archived","description":"historical group","status":"archived","memberAgentIds":["a1"],"relationshipIds":["missing"],"version":2}
	}`)
	writeFixture(t, filepath.Join(source, "integrations.json"), `{
  "connections":{"c1":{"id":"c1"},"c2":{"id":"c2"}},
  "addresses":{"d1":{"id":"d1","agentId":"a1","connectionId":"c1"},"d2":{"id":"d2","agentId":"a2","connectionId":"c2"}},
  "memberships":{"m1":{"id":"m1","addressId":"d1"},"m2":{"id":"m2","addressId":"d2"}},
  "conversationCandidates":{}
}`)
	writeFixture(t, filepath.Join(source, "comms.ndjson"), "{\"message\":{\"id\":\"x1\",\"fromAgentId\":\"a1\"}}\n{\"message\":{\"id\":\"x2\",\"fromAgentId\":\"a2\"}}\n")
	if err := os.Mkdir(filepath.Join(source, "events"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(source, "events", "a1.ndjson"), "{\"secret\":true}\n")

	summary, err := CreateSnapshot(source, destination, Options{Agents: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.AgentIDs) != 1 || summary.AgentIDs[0] != "a1" || !summary.Filtered {
		t.Fatalf("summary = %#v", summary)
	}
	var agents map[string]any
	readFixtureJSON(t, filepath.Join(destination, "agents.json"), &agents)
	if len(agents) != 1 || agents["a1"] == nil {
		t.Fatalf("agents = %#v", agents)
	}
	var groups map[string]struct {
		MemberAgentIDs  []string `json:"memberAgentIds"`
		RelationshipIDs []string `json:"relationshipIds"`
	}
	readFixtureJSON(t, filepath.Join(destination, "collaboration-groups.json"), &groups)
	if len(groups) != 2 || len(groups["g1"].MemberAgentIDs) != 1 || groups["g1"].MemberAgentIDs[0] != "a1" || len(groups["g1"].RelationshipIDs) != 0 {
		t.Fatalf("collaboration groups = %#v", groups)
	}
	if len(groups["g3"].RelationshipIDs) != 1 || groups["g3"].RelationshipIDs[0] != "missing" {
		t.Fatalf("collaboration groups = %#v", groups)
	}
	var integrations struct {
		Connections map[string]any `json:"connections"`
		Addresses   map[string]any `json:"addresses"`
		Memberships map[string]any `json:"memberships"`
	}
	readFixtureJSON(t, filepath.Join(destination, "integrations.json"), &integrations)
	if len(integrations.Connections) != 1 || integrations.Connections["c1"] == nil || len(integrations.Memberships) != 1 {
		t.Fatalf("integrations = %#v", integrations)
	}
	comms, err := os.ReadFile(filepath.Join(destination, "comms.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(comms), `"x1"`) || strings.Contains(string(comms), `"x2"`) {
		t.Fatalf("comms = %s", comms)
	}
	entries, err := os.ReadDir(filepath.Join(destination, "events"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("events = %v, %v", entries, err)
	}
}

func TestCreateSnapshotCopiesCollaborationGroups(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "data")
	writeFixture(t, filepath.Join(source, "agents.json"), `{"a1":{"id":"a1","name":"alpha"}}`)
	writeFixture(t, filepath.Join(source, "collaboration-groups.json"), `{"g1":{"id":"g1","name":"One","status":"archived","memberAgentIds":["a1"],"relationshipIds":["missing"],"version":2}}`)
	if _, err := CreateSnapshot(source, destination, Options{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "collaboration-groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"missing"`) {
		t.Fatalf("collaboration groups = %s", data)
	}
}

func TestCreateSnapshotRejectsUnknownAgent(t *testing.T) {
	source := t.TempDir()
	writeFixture(t, filepath.Join(source, "agents.json"), `{"a1":{"id":"a1","name":"alpha"}}`)
	_, err := CreateSnapshot(source, filepath.Join(t.TempDir(), "data"), Options{Agents: []string{"missing"}})
	if err == nil || !strings.Contains(err.Error(), "Agent not found") {
		t.Fatalf("error = %v", err)
	}
}

func writeFixture(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFixtureJSON(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}
