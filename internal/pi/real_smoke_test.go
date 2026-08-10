package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRealRPCSmoke is an explicit local smoke, not part of automated tests.
// Contract tests use the fake executable in rpc_test.go.
func TestRealRPCSmoke(t *testing.T) {
	bin := os.Getenv("PI_REAL_BIN")
	if bin == "" {
		t.Skip("set PI_REAL_BIN to run the real Pi RPC smoke")
	}
	settled := make(chan struct{}, 1)
	answer := make(chan string, 1)
	failures := make(chan error, 1)
	rpc, err := SpawnRPC(RPCOptions{
		Bin: bin, Cwd: t.TempDir(),
		Args: []string{
			"--session-dir", t.TempDir(), "--session-id", "loom-issue-5-smoke",
			"--no-tools", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files", "--approve",
		},
		OnFailure: func(err error) { failures <- err },
		OnEvent: func(raw json.RawMessage) {
			var event struct {
				Type    string `json:"type"`
				Message struct {
					Role    string `json:"role"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			}
			_ = json.Unmarshal(raw, &event)
			if event.Type == "message_end" && event.Message.Role == "assistant" {
				for _, content := range event.Message.Content {
					if content.Type == "text" {
						answer <- content.Text
					}
				}
			}
			if event.Type == "agent_settled" {
				settled <- struct{}{}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	state, err := rpc.Request(ctx, "get_state", nil)
	if err != nil || !state.Success {
		t.Fatalf("get_state = %#v, err=%v", state, err)
	}
	prompt, err := rpc.Request(ctx, "prompt", map[string]any{"message": "Reply exactly PI_SMOKE_OK and nothing else."})
	if err != nil || !prompt.Success {
		t.Fatalf("prompt = %#v, err=%v", prompt, err)
	}
	var text string
	select {
	case text = <-answer:
	case err := <-failures:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-settled:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if text != "PI_SMOKE_OK" {
		t.Fatalf("assistant = %q", text)
	}
}

func TestRealLoomExtensionLoads(t *testing.T) {
	bin := os.Getenv("PI_REAL_BIN")
	if bin == "" {
		t.Skip("set PI_REAL_BIN to run the real Pi Extension smoke")
	}
	dataDir := t.TempDir()
	extensionPath, err := MaterializeLoomExtension(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	errors := make(chan string, 1)
	rpc, err := SpawnRPC(RPCOptions{
		Bin: bin, Cwd: t.TempDir(),
		Args: []string{
			"--session-dir", filepath.Join(dataDir, "sessions"), "--session-id", "loom-issue-9-extension-smoke",
			"--extension", extensionPath, "--approve",
		},
		Env: map[string]string{"CODEX_LOOM_AGENT_ID": "agent-smoke", "CODEX_LOOM_API_URL": "http://127.0.0.1:1"},
		OnEvent: func(raw json.RawMessage) {
			var event struct {
				Type  string `json:"type"`
				Error string `json:"error"`
			}
			if json.Unmarshal(raw, &event) == nil && event.Type == "extension_error" {
				errors <- event.Error
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := rpc.Request(ctx, "get_state", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-errors:
		t.Fatalf("Loom Pi Extension failed to load: %s", message)
	case <-time.After(500 * time.Millisecond):
	}
}
