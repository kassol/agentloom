package claudebridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/claudegen"
)

func TestBridgeStartPublishesReadyOnlyAfterExactHelloInitializeReady(t *testing.T) {
	t.Parallel()
	manifest := testManifest()
	logPath := filepath.Join(t.TempDir(), "bridge.log")
	bridgePath := writeFakeBridge(t, `
hello='{"kind":"hello","protocolVersion":1,"bridgeBuild":"claude-bridge-v1","nodeVersion":"24.19.0","sdkVersion":"0.3.228","claudeCodeVersion":"2.1.228","os":"`+runtime.GOOS+`","arch":"`+runtime.GOARCH+`","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
printf '%s\n' "$hello"
IFS= read -r init
printf '%s\n' "$init" >> "$1"
printf '%s\n' '{"kind":"ready","requestId":"loom-init","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
while IFS= read -r line; do [ "$line" = '{"kind":"close"}' ] && exit 0; done
`)
	driver := NewDriver(DriverOptions{
		ResolveActive: func(context.Context) (LaunchSpec, error) {
			return LaunchSpec{NodePath: "/bin/sh", BridgePath: bridgePath, Args: []string{logPath}, Manifest: manifest}, nil
		},
		NextID: func() string { return "loom-init" },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bridge, err := driver.Acquire(ctx, "agent-one")
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	if !bridge.Alive() {
		t.Fatal("published bridge is not alive")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var init struct {
		Kind      string `json:"kind"`
		RequestID string `json:"requestId"`
		AgentID   string `json:"agentId"`
	}
	if err := json.Unmarshal(data, &init); err != nil {
		t.Fatalf("initialize frame is not JSON: %s", data)
	}
	if init.Kind != "initialize" || init.RequestID != "loom-init" || init.AgentID != "agent-one" {
		t.Fatalf("unexpected initialize frame: %+v", init)
	}
}

func testManifest() claudegen.Manifest {
	return claudegen.Manifest{
		ID:                   "test-generation",
		BridgeProtocol:       1,
		BridgeBuild:          "claude-bridge-v1",
		NodeVersion:          "24.19.0",
		SDKVersion:           "0.3.228",
		ClaudeCodeVersion:    "2.1.228",
		RequiredCapabilities: []string{"interrupt", "approval", "hooks", "mcp", "session_resume"},
	}
}

func writeFakeBridge(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-bridge.sh")
	contents := fmt.Sprintf("#!/bin/sh\nset -eu\n%s", body)
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
