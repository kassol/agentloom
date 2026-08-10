package hub

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/store"
)

func TestPiAgentCompletesLiveLoomTurnOnlyAfterAgentSettled(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "happy")
	h := testHub(st)
	h.stop = make(chan struct{})

	agent, err := h.CreateAgent(CreateParams{Name: "pi-worker", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	if agent.RuntimeBinding.Kind != "pi" || agent.RuntimeBinding.NativeRef != "" || agent.RuntimeTurnBindings != nil {
		t.Fatalf("public Agent Runtime = %#v, turn bindings=%#v", agent.RuntimeBinding, agent.RuntimeTurnBindings)
	}
	if native := h.agents[agent.ID].RuntimeBinding.NativeRef; !strings.HasPrefix(native, filepath.Join(st.Dir(), "pi", agent.ID)+string(filepath.Separator)) {
		t.Fatalf("Pi session = %q, want under per-Agent Loom data directory", native)
	}

	result, err := h.SendTask(agent.ID, "hello Pi", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.TurnID == "" || !strings.HasPrefix(result.TurnID, "turn_") {
		t.Fatalf("Loom Turn result = %#v", result)
	}
	waitForPiFile(t, os.Getenv("FAKE_PI_AGENT_END_FILE"))
	view, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "running" || view.CurrentTurnID != result.TurnID || view.LastTurn != nil {
		t.Fatalf("Agent after agent_end = status %q current %q last %#v", view.Status, view.CurrentTurnID, view.LastTurn)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, _ = h.GetAgent(agent.ID)
		if view.LastTurn != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if view.Status != "idle" || view.LastTurn == nil || view.LastTurn.Status != "completed" || view.LastTurn.TurnID != result.TurnID {
		t.Fatalf("settled Agent = %#v", view.Agent)
	}
	events, err := st.ReadEvents(agent.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var answerSeen, completionSeen bool
	for _, event := range events {
		switch event.Type {
		case "loom/text-completed":
			answerSeen = strings.Contains(string(event.Data), "hello from Pi")
		case "loom/turn-completed":
			completionSeen = answerSeen && strings.Contains(string(event.Data), result.TurnID)
		}
	}
	if !answerSeen || !completionSeen {
		t.Fatalf("normalized Pi events = %#v", events)
	}
	prompt, err := os.ReadFile(os.Getenv("FAKE_PI_PROMPT_FILE"))
	if err != nil || !strings.Contains(string(prompt), "hello Pi") || !strings.Contains(string(prompt), "loom_agent_profile") {
		t.Fatalf("Pi prompt context = %q, err=%v", prompt, err)
	}
	starts, err := os.ReadFile(os.Getenv("FAKE_PI_STARTS_FILE"))
	if err != nil || strings.Count(string(starts), "start\n") != 1 {
		t.Fatalf("Pi process starts = %q, err=%v", starts, err)
	}
	h.Shutdown()
}

func TestPiProtocolFailureFailsActiveLoomTurn(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "malformed-after-prompt")
	h := testHub(st)
	h.stop = make(chan struct{})
	agent, err := h.CreateAgent(CreateParams{Name: "pi-failure", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.SendTask(agent.ID, "break protocol", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		view, _ := h.GetAgent(agent.ID)
		if view.LastTurn != nil {
			if view.LastTurn.TurnID != result.TurnID || view.LastTurn.Status != "failed" || !strings.Contains(view.LastError, "protocol") {
				t.Fatalf("failed Pi Turn = %#v", view.Agent)
			}
			h.Shutdown()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Pi protocol failure did not fail the active Loom Turn")
}

func configureFakePiHubRPC(t *testing.T, scenario string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "pi")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestFakePiHubRPCProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_BIN", bin)
	t.Setenv("FAKE_PI_HUB_SCENARIO", scenario)
	t.Setenv("FAKE_PI_AGENT_END_FILE", filepath.Join(dir, "agent-end"))
	t.Setenv("FAKE_PI_PROMPT_FILE", filepath.Join(dir, "prompt"))
	t.Setenv("FAKE_PI_STARTS_FILE", filepath.Join(dir, "starts"))
}

func TestFakePiHubRPCProcess(t *testing.T) {
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "" {
		return
	}
	starts, _ := os.OpenFile(os.Getenv("FAKE_PI_STARTS_FILE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if starts != nil {
		_, _ = starts.WriteString("start\n")
		_ = starts.Close()
	}
	args := os.Args
	sessionDir := argumentValue(args, "--session-dir")
	sessionID := argumentValue(args, "--session-id")
	if sessionDir == "" || sessionID == "" || !containsArgumentPair(args, "--mode", "rpc") {
		os.Exit(30)
	}
	reader := bufio.NewReader(os.Stdin)
	var command map[string]any
	readFakePiCommand(t, reader, &command)
	id, _ := command["id"].(string)
	sessionFile := filepath.Join(sessionDir, "session-"+sessionID+".jsonl")
	fmt.Printf(`{"id":%q,"type":"response","command":"get_state","success":true,"data":{"sessionFile":%q,"sessionId":%q}}`+"\n", id, sessionFile, sessionID)
	readFakePiCommand(t, reader, &command)
	id, _ = command["id"].(string)
	message, _ := command["message"].(string)
	_ = os.WriteFile(os.Getenv("FAKE_PI_PROMPT_FILE"), []byte(message), 0o600)
	fmt.Printf(`{"id":%q,"type":"response","command":"prompt","success":true}`+"\n", id)
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "malformed-after-prompt" {
		fmt.Print("not-json\n")
		return
	}
	fmt.Print("{\"type\":\"agent_start\"}\n")
	fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":\"hello from Pi\"}]}}\n")
	fmt.Print("{\"type\":\"agent_end\"}\n")
	_ = os.WriteFile(os.Getenv("FAKE_PI_AGENT_END_FILE"), []byte("done"), 0o600)
	time.Sleep(250 * time.Millisecond)
	fmt.Print("{\"type\":\"agent_settled\"}\n")
	_, _ = reader.ReadString('\n')
}

func readFakePiCommand(t *testing.T, reader *bufio.Reader, command *map[string]any) {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil || json.Unmarshal([]byte(line), command) != nil {
		os.Exit(31)
	}
}

func argumentValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func containsArgumentPair(args []string, left, right string) bool {
	return argumentValue(args, left) == right
}

func waitForPiFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
