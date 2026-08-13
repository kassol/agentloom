package backup

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeIdentityBindingBoundaryAndCanonicalLedgerSurviveBackup(t *testing.T) {
	dataDir := t.TempDir()
	ledger := []byte(`{"version":1,"agents":{"agent-stable":[{"turnId":"turn-stable","state":"completed","content":[]}]}}`)
	agents := []byte(`{"agent-stable":{"id":"agent-stable","name":"claude-stable","cwd":"/tmp/work","threadId":"loom-thread-stable","runtimeBinding":{"schemaVersion":2,"kind":"claude","nativeRef":"native-session-private"},"historyBoundary":{"kind":"history_boundary","createdAt":"2026-08-13T00:00:00Z","importedTurns":0,"disclosure":"Native content is not imported.","nativeRevision":"native-revision-private"},"runtimeTurnBindings":{"turn-stable":"native-turn-private"},"sandbox":"danger-full-access","approvalPolicy":"never","status":"idle","currentTask":"","currentTurnId":"","lastError":"","lastTurn":null,"createdAt":"2026-08-13T00:00:00Z","updatedAt":"2026-08-13T00:00:00Z"}}`)
	if err := os.WriteFile(filepath.Join(dataDir, "canonical-turn-ledger.json"), ledger, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "agents.json"), agents, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Create(Options{Reason: "claude-ledger", DataDir: dataDir, CodexSessionsDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(snapshot.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(gz)
	expected := map[string][]byte{
		"codex-loom/agents.json":                agents,
		"codex-loom/canonical-turn-ledger.json": ledger,
	}
	for len(expected) > 0 {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		want, ok := expected[header.Name]
		if !ok {
			continue
		}
		got, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s changed in backup: %s", header.Name, got)
		}
		delete(expected, header.Name)
	}
	if len(expected) != 0 {
		t.Fatalf("Claude durable state was not included in backup: %v", expected)
	}
}
