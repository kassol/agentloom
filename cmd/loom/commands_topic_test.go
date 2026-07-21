package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTopicFileFlagsCountAsPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "purpose.md")
	if err := os.WriteFile(path, []byte("Long-lived delivery purpose.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := parseArgs([]string{"update", "tpc_1", "--purpose-file", path})
	if !topicFlagPresent(a, "purpose") {
		t.Fatal("--purpose-file did not count as a supplied purpose")
	}
	if got := topicFlagText(a, "purpose"); got != "Long-lived delivery purpose.\n" {
		t.Fatalf("topicFlagText = %q", got)
	}
}
