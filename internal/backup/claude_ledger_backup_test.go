package backup

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalTurnLedgerSurvivesBackup(t *testing.T) {
	dataDir := t.TempDir()
	ledger := []byte(`{"version":1,"agents":{"agent-stable":[{"turnId":"turn-stable","state":"completed","content":[]}]}}`)
	if err := os.WriteFile(filepath.Join(dataDir, "canonical-turn-ledger.json"), ledger, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "agents.json"), []byte(`{}`), 0o600); err != nil {
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
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != "codex-loom/canonical-turn-ledger.json" {
			continue
		}
		got, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(ledger) {
			t.Fatalf("ledger changed in backup: %s", got)
		}
		return
	}
	t.Fatal("Canonical Turn Ledger was not included in backup")
}
