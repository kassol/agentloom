package codex

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVersionUsesSelectedBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'codex-cli 0.144.1\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	version, err := Version(path)
	if err != nil {
		t.Fatal(err)
	}
	if version != "codex-cli 0.144.1" {
		t.Fatalf("version = %q", version)
	}
}
