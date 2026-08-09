package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yan5xu/codex-loom/internal/credentials"
)

func TestCBackupExcludesFixedCredentialDirectory(t *testing.T) {
	dataDir := t.TempDir()
	credDir := filepath.Join(dataDir, credentials.DirectoryName)
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := []byte("secret-must-not-be-backed-up")
	if err := os.WriteFile(filepath.Join(credDir, strings.Repeat("a", 64)), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "agents.json"), []byte(`{"agent":{"id":"a"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	agentsDir := t.TempDir()
	snapshot, err := Create(Options{Reason: "credential-exclusion", DataDir: dataDir, CodexSessionsDir: agentsDir})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(snapshot.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(archive, secret) {
		t.Fatal("backup archive contains managed credential material")
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
	tarReader := tar.NewReader(gz)
	found := false
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != "manifest.json" {
			continue
		}
		manifestBytes, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(manifestBytes), credentials.DirectoryName+"/**") {
			found = true
		}
	}
	if !found {
		t.Fatal("backup manifest does not declare the credential directory exclusion")
	}
}
