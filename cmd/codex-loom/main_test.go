package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStartupRejectsOldPiBeforeOpeningStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	server := filepath.Join(dir, "codex-loom")
	if output, err := exec.Command("go", "build", "-o", server, ".").CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, output)
	}
	piBin := filepath.Join(dir, "pi")
	if err := os.WriteFile(piBin, []byte("#!/bin/sh\nprintf '0.84.0\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "data")
	command := exec.Command(server, "-data", dataDir)
	command.Env = append(os.Environ(), "PI_BIN="+piBin)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "Pi 0.84.0 is too old; Pi 0.84.1 or newer is required") {
		t.Fatalf("startup error = %v, output = %q", err, output)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("startup opened data directory before Pi preflight: %v", err)
	}
}
