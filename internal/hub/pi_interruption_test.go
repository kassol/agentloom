package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPiInterruptedTurnInspectorReportsUnfinishedToolOnActiveBranch(t *testing.T) {
	sessionFile := filepath.Join(t.TempDir(), "session.jsonl")
	contents := strings.Join([]string{
		`{"type":"session","version":3,"id":"session-1","timestamp":"2026-08-10T01:00:00Z","cwd":"/tmp/work"}`,
		`{"type":"message","id":"user-1","parentId":null,"timestamp":"2026-08-10T01:00:01Z","message":{"role":"user","content":[{"type":"text","text":"publish release"}]}}`,
		`{"type":"message","id":"assistant-1","parentId":"user-1","timestamp":"2026-08-10T01:00:02Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"bash","arguments":{"command":"deploy --prod"}}],"stopReason":"toolUse"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(sessionFile, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	runtime := newPiAgentRuntime("agent-1", t.TempDir(), "")
	evidence, err := runtime.InspectInterruptedTurn(sessionFile, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Status != RuntimeInterruptionAmbiguous || evidence.LeafEntryID != "assistant-1" || len(evidence.UnfinishedTools) != 1 {
		t.Fatalf("interruption evidence = %#v", evidence)
	}
	tool := evidence.UnfinishedTools[0]
	if tool.ID != "call-1" || tool.Name != "bash" || tool.Command != "deploy --prod" || tool.StartedAt != "2026-08-10T01:00:02Z" {
		t.Fatalf("unfinished tool = %#v", tool)
	}
}

func TestPiInterruptedTurnInspectorTreatsMatchingToolResultAsCleanEvidence(t *testing.T) {
	entries := []piSessionEntry{
		{Type: "message", ID: "user-1", Timestamp: "2026-08-10T01:00:01Z", Message: []byte(`{"role":"user","content":[{"type":"text","text":"publish release"}]}`)},
		{Type: "message", ID: "assistant-1", ParentID: "user-1", Timestamp: "2026-08-10T01:00:02Z", Message: []byte(`{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"bash","arguments":{"command":"deploy --prod"}}],"stopReason":"toolUse"}`)},
		{Type: "message", ID: "result-1", ParentID: "assistant-1", Timestamp: "2026-08-10T01:00:03Z", Message: []byte(`{"role":"toolResult","toolCallId":"call-1","toolName":"bash","content":[{"type":"text","text":"deployed"}],"isError":false}`)},
	}

	evidence, err := inspectPiInterruptedTurn(entries, "result-1", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Status != RuntimeInterruptionClean || evidence.LeafEntryID != "result-1" || len(evidence.UnfinishedTools) != 0 {
		t.Fatalf("clean interruption evidence = %#v", evidence)
	}
}

func TestPiInterruptedTurnInspectorIgnoresUnfinishedToolOnAbandonedBranch(t *testing.T) {
	entries := []piSessionEntry{
		{Type: "message", ID: "user-1", Timestamp: "2026-08-10T01:00:01Z", Message: []byte(`{"role":"user","content":[{"type":"text","text":"publish release"}]}`)},
		{Type: "message", ID: "abandoned-tool", ParentID: "user-1", Timestamp: "2026-08-10T01:00:02Z", Message: []byte(`{"role":"assistant","content":[{"type":"toolCall","id":"call-abandoned","name":"bash","arguments":{"command":"deploy --old"}}],"stopReason":"toolUse"}`)},
		{Type: "message", ID: "active-answer", ParentID: "user-1", Timestamp: "2026-08-10T01:00:03Z", Message: []byte(`{"role":"assistant","content":[{"type":"text","text":"current branch"}],"stopReason":"stop"}`)},
	}

	evidence, err := inspectPiInterruptedTurn(entries, "active-answer", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Status != RuntimeInterruptionTerminal || evidence.TerminalStatus != "completed" || len(evidence.UnfinishedTools) != 0 {
		t.Fatalf("active-branch interruption evidence = %#v", evidence)
	}
}
