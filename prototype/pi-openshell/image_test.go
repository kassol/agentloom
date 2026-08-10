package piopenshell_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPinnedImageLoadsPiAndExtensionsWithOrderedRPC(t *testing.T) {
	if os.Getenv("CODEX_LOOM_RUN_OPEN_SHELL_IMAGE_PROBE") != "1" {
		t.Skip("set CODEX_LOOM_RUN_OPEN_SHELL_IMAGE_PROBE=1 to build and run the prototype image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is unavailable")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("codexloom/pi-openshell-prototype:test-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", tag).Run() })

	build := exec.Command("docker", "build", "--tag", tag, "--file", "prototype/pi-openshell/Dockerfile", ".")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build pinned Pi image: %v\n%s", err, output)
	}

	version, err := exec.Command("docker", "run", "--rm", tag, "--version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(version)) != "0.84.1" {
		t.Fatalf("image Pi version = %q, err=%v", version, err)
	}

	const script = `set -eu
mkdir -p /sandbox/session
printf '%s\n' '{"id":"probe-1","type":"get_state"}' '{"id":"probe-2","type":"get_state"}' |
  pi --mode rpc --session-dir /sandbox/session --session-id image-probe --approve \
    --extension /opt/codex-loom/loom_extension.ts \
    --extension /opt/codex-loom/probe_extension.ts
test -f /sandbox/probe-extension-loaded
`
	command := exec.Command("docker", "run", "--rm", "-i", "--entrypoint", "/bin/sh", tag, "-c", script)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("run image RPC smoke: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "openshell-probe-extension-loaded") || !strings.Contains(stderr.String(), "openshell-probe-extension-loaded") {
		t.Fatalf("stderr separation failed\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}

	var ids []string
	scanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	for scanner.Scan() {
		var response struct {
			ID      string `json:"id"`
			Success bool   `json:"success"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatalf("stdout contains non-JSON RPC data %q: %v", scanner.Text(), err)
		}
		if response.ID != "" {
			if !response.Success {
				t.Fatalf("RPC response %s was unsuccessful: %s", response.ID, scanner.Text())
			}
			ids = append(ids, response.ID)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "probe-1,probe-2" {
		t.Fatalf("RPC response order = %v", ids)
	}
}
