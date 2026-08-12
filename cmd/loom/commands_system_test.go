package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClaudeRuntimeCLIUsesCanonicalGenerationAPI(t *testing.T) {
	requests := 0
	acceptances := []bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/runtime-generations/claude" && r.URL.Path != "/api/runtime-generations/claude/install" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			acceptances = append(acceptances, body["acceptTerms"] == true)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"generation": map[string]any{
			"state": "staged", "developerPreview": true, "productionReady": false,
			"required": map[string]any{"id": "claude-v1", "nodeVersion": "24.19.0", "sdkVersion": "0.3.228", "claudeCodeVersion": "2.1.228"},
			"staged":   map[string]any{"id": "claude-v1"},
			"platform": map[string]any{"os": "darwin", "arch": "arm64", "supported": true},
		}})
	}))
	defer server.Close()
	previousBase, previousColor := base, useColor
	base, useColor = server.URL, false
	defer func() { base, useColor = previousBase, previousColor }()

	output := captureStdout(t, func() { cmdRuntimeGeneration(args{positional: []string{"status"}, flags: map[string]string{}}) })
	output += captureStdout(t, func() {
		cmdRuntimeGeneration(args{positional: []string{"install"}, flags: map[string]string{"accept-terms": "true"}})
	})
	output += captureStdout(t, func() {
		cmdRuntimeGeneration(args{positional: []string{"install"}, flags: map[string]string{"accept-terms": "false"}})
	})
	for _, want := range []string{"developer preview", "staged", "Node 24.19.0", "SDK 0.3.228"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %s", want, output)
		}
	}
	if requests != 3 {
		t.Fatalf("requests = %d", requests)
	}
	if len(acceptances) != 2 || !acceptances[0] || acceptances[1] {
		t.Fatalf("terms acceptances = %#v", acceptances)
	}
}

func TestFormatBuildIncludesRuntimeIdentity(t *testing.T) {
	text := formatBuild("running", map[string]any{
		"product": "CodexLoom", "version": "1.2.3", "commit": "abc123", "builtAt": "2026-07-15T01:00:00Z",
		"goVersion": "go1.25", "os": "darwin", "arch": "arm64", "pid": 42.0,
		"startedAt": "2026-07-15T02:00:00Z", "mode": "canary", "readOnly": true,
		"dataDir": "/tmp/canary", "webAsset": "assets/index-test.js",
	})
	for _, want := range []string{"CodexLoom 1.2.3 (abc123)", "pid 42", "mode canary", "read-only true", "assets/index-test.js"} {
		if !strings.Contains(text, want) {
			t.Fatalf("formatBuild missing %q:\n%s", want, text)
		}
	}
}

func TestBuildMismatchDetectsDifferentCommit(t *testing.T) {
	got := buildMismatch(map[string]any{"commit": "new"}, map[string]any{"commit": "old"})
	if !strings.Contains(got, "restart required") {
		t.Fatalf("mismatch = %q", got)
	}
}
