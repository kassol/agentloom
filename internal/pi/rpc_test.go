package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRPCProcessCorrelatesCommandsAndForwardsLFDelimitedEvents(t *testing.T) {
	bin := fakeRPCBin(t, "happy")
	events := make(chan json.RawMessage, 8)
	failures := make(chan error, 1)
	rpc, err := SpawnRPC(RPCOptions{
		Bin: bin, Cwd: t.TempDir(), Args: []string{"--session-dir", t.TempDir()},
		OnEvent:   func(event json.RawMessage) { events <- event },
		OnFailure: func(err error) { failures <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	state, err := rpc.Request(ctx, "get_state", nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.Command != "get_state" || !state.Success || !strings.Contains(string(state.Data), `"sessionFile"`) {
		t.Fatalf("get_state response = %#v", state)
	}
	prompt, err := rpc.Request(ctx, "prompt", map[string]any{"message": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if prompt.Command != "prompt" || !prompt.Success {
		t.Fatalf("prompt response = %#v", prompt)
	}

	for _, want := range []string{"agent_start", "message_end", "agent_end", "agent_settled"} {
		select {
		case raw := <-events:
			var event struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &event); err != nil || event.Type != want {
				t.Fatalf("event = %s (%v), want %s", raw, err, want)
			}
		case err := <-failures:
			t.Fatalf("unexpected Runtime failure: %v", err)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

func TestRPCProcessReceivesScopedEnvironment(t *testing.T) {
	t.Setenv("CODEX_LOOM_AGENT_ID", "")
	rpc, err := SpawnRPC(RPCOptions{
		Bin: fakeRPCBin(t, "environment"), Cwd: t.TempDir(),
		Env: map[string]string{"CODEX_LOOM_AGENT_ID": "agent-pi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	state, err := rpc.Request(ctx, "get_state", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state.Data), `"agent":"agent-pi"`) {
		t.Fatalf("child environment = %s", state.Data)
	}
}

func TestRPCProcessReportsProtocolDriftMalformedOutputAndExit(t *testing.T) {
	for _, scenario := range []string{"wrong-id", "malformed", "unterminated", "exit"} {
		t.Run(scenario, func(t *testing.T) {
			failures := make(chan error, 1)
			rpc, err := SpawnRPC(RPCOptions{
				Bin: fakeRPCBin(t, scenario), Cwd: t.TempDir(),
				OnFailure: func(err error) { failures <- err },
			})
			if err != nil {
				t.Fatal(err)
			}
			defer rpc.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, requestErr := rpc.Request(ctx, "get_state", nil)
			if requestErr == nil {
				t.Fatal("invalid Pi output did not fail the command")
			}
			select {
			case failure := <-failures:
				if failure == nil || !strings.Contains(strings.ToLower(failure.Error()), scenarioFailureWord(scenario)) {
					t.Fatalf("failure = %v", failure)
				}
			case <-ctx.Done():
				t.Fatal("Runtime failure callback was not invoked")
			}
		})
	}
}

func scenarioFailureWord(scenario string) string {
	switch scenario {
	case "wrong-id":
		return "id"
	case "malformed", "unterminated":
		return "protocol"
	default:
		return "exit"
	}
}

func fakeRPCBin(t *testing.T, scenario string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pi")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestFakePiRPCProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_PI_RPC_SCENARIO", scenario)
	return path
}

func TestFakePiRPCProcess(t *testing.T) {
	if os.Getenv("FAKE_PI_RPC_SCENARIO") == "" {
		return
	}
	args := os.Args
	separator := 0
	for i, arg := range args {
		if arg == "--" {
			separator = i + 1
			break
		}
	}
	if separator == 0 || !containsArgs(args[separator:], "--mode", "rpc") {
		os.Exit(20)
	}
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		os.Exit(21)
	}
	var command map[string]any
	if json.Unmarshal([]byte(line), &command) != nil || command["type"] != "get_state" {
		os.Exit(22)
	}
	id, _ := command["id"].(string)
	switch os.Getenv("FAKE_PI_RPC_SCENARIO") {
	case "wrong-id":
		fmt.Printf(`{"id":"other","type":"response","command":"get_state","success":true}` + "\n")
		return
	case "malformed":
		fmt.Print("not-json\n")
		return
	case "unterminated":
		fmt.Print(`{"type":"agent_start"}`)
		return
	case "exit":
		os.Exit(23)
	}
	if os.Getenv("FAKE_PI_RPC_SCENARIO") == "environment" {
		fmt.Printf(`{"id":%q,"type":"response","command":"get_state","success":true,"data":{"agent":%q}}`+"\n", id, os.Getenv("CODEX_LOOM_AGENT_ID"))
		_, _ = reader.ReadString('\n')
		return
	}
	fmt.Printf(`{"id":%q,"type":"response","command":"get_state","success":true,"data":{"sessionFile":"/loom/pi/agent/session.jsonl","sessionId":"session-1"}}`+"\n", id)
	line, err = reader.ReadString('\n')
	if err != nil || json.Unmarshal([]byte(line), &command) != nil || command["type"] != "prompt" || command["message"] != "hello" {
		os.Exit(24)
	}
	id, _ = command["id"].(string)
	fmt.Printf(`{"id":%q,"type":"response","command":"prompt","success":true}`+"\n", id)
	fmt.Print("{\"type\":\"agent_start\"}\n")
	fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"hello from Pi\"}]}}\n")
	fmt.Print("{\"type\":\"agent_end\"}\n")
	fmt.Print("{\"type\":\"agent_settled\"}\n")
	_, _ = reader.ReadString('\n')
}

func containsArgs(args []string, left, right string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == left && args[i+1] == right {
			return true
		}
	}
	return false
}
