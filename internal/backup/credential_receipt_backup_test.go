package backup

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialReceiptAndExplicitMarkerSurviveOrdinaryBackup(t *testing.T) {
	dataDir := t.TempDir()
	receiptDir := filepath.Join(dataDir, "credential-receipts")
	if err := os.MkdirAll(receiptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	receipt := []byte(`{"version":1,"id":"creceipt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","connectionId":"conn_1","previousRef":"keychain:legacy","managedRef":"managed:opaque","status":"completed"}`)
	if err := os.WriteFile(filepath.Join(receiptDir, "creceipt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json"), receipt, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Create(Options{Reason: "credential-receipt", DataDir: dataDir, CodexSessionsDir: t.TempDir()})
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
	archive := tar.NewReader(gz)
	foundReceipt, foundMarker := false, false
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "codex-loom/credential-receipts/creceipt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json" {
			foundReceipt = string(content) == string(receipt)
		}
		if header.Name == "manifest.json" {
			foundMarker = strings.Contains(string(content), `"credentialReceipts"`) &&
				strings.Contains(string(content), "non-secret migration receipts included")
		}
	}
	if !foundReceipt || !foundMarker {
		t.Fatalf("receipt=%v marker=%v", foundReceipt, foundMarker)
	}
}
