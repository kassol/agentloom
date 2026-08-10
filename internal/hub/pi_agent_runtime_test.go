package hub

import (
	"bufio"
	"bytes"
	"encoding/base64"
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
	var normalized []string
	toolIDs := map[string]bool{}
	for _, event := range events {
		switch event.Type {
		case "loom/reasoning-delta", "loom/reasoning-completed", "loom/text-delta", "loom/tool-started", "loom/tool-updated", "loom/tool-completed":
			normalized = append(normalized, event.Type)
			if strings.HasPrefix(event.Type, "loom/tool-") {
				var payload struct {
					ItemID string `json:"itemId"`
				}
				_ = json.Unmarshal(event.Data, &payload)
				toolIDs[payload.ItemID] = true
			}
		case "loom/text-completed":
			normalized = append(normalized, event.Type)
			answerSeen = strings.Contains(string(event.Data), "hello from Pi")
		case "loom/turn-completed":
			normalized = append(normalized, event.Type)
			completionSeen = answerSeen && strings.Contains(string(event.Data), result.TurnID)
		}
	}
	if !answerSeen || !completionSeen {
		t.Fatalf("normalized Pi events = %#v", events)
	}
	wantNormalized := []string{
		"loom/reasoning-delta", "loom/reasoning-completed", "loom/tool-started", "loom/tool-updated",
		"loom/tool-completed", "loom/text-delta", "loom/text-completed", "loom/turn-completed",
	}
	if strings.Join(normalized, ",") != strings.Join(wantNormalized, ",") || len(toolIDs) != 1 || !toolIDs["call-1"] {
		t.Fatalf("streamed normalized order=%v toolIDs=%v", normalized, toolIDs)
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

func TestPiAgentSendsSupportedImagesAsNativeRPCContent(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "image")
	h := testHub(st)
	h.stop = make(chan struct{})
	defer h.Shutdown()

	agent, err := h.CreateAgent(CreateParams{Name: "pi-vision", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	if !agent.RuntimeCapabilities.ImageInput {
		t.Fatalf("Pi image capability = %#v", agent.RuntimeCapabilities)
	}
	imageBytes := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x5a}, 64)...)
	image, err := h.StageThreadArtifact(agent.ID, "diagram.png", "image/png", bytes.NewReader(imageBytes))
	if err != nil {
		t.Fatal(err)
	}
	document, err := h.StageThreadArtifact(agent.ID, "brief.pdf", "application/pdf", strings.NewReader("%PDF-1.4\nloom"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.SendTaskWithArtifacts(agent.ID, "Review both files", []string{image.ID, document.ID}, time.Second); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(os.Getenv("FAKE_PI_PROMPT_JSON_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	var prompt struct {
		Message string `json:"message"`
		Images  []struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"images"`
	}
	if err := json.Unmarshal(data, &prompt); err != nil {
		t.Fatal(err)
	}
	if len(prompt.Images) != 1 || prompt.Images[0].Type != "image" || prompt.Images[0].MimeType != "image/png" ||
		prompt.Images[0].Data != base64.StdEncoding.EncodeToString(imageBytes) {
		t.Fatalf("Pi native images = %#v", prompt.Images)
	}
	if !strings.Contains(prompt.Message, document.Path) {
		t.Fatalf("generic file was not path-only guidance: %s", data)
	}
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

func TestPiRuntimeNormalizesStreamingTextReasoningAndToolLifecycle(t *testing.T) {
	runtime := newPiAgentRuntime("agent-1", t.TempDir())
	rawEvents := []string{
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","contentIndex":0,"delta":"checking"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":1,"delta":"hello"}}`,
		`{"type":"tool_execution_start","toolCallId":"call-1","toolName":"bash","args":{"command":"pwd"}}`,
		`{"type":"tool_execution_update","toolCallId":"call-1","toolName":"bash","args":{"command":"pwd"},"partialResult":{"content":[{"type":"text","text":"/tmp"}]}}`,
		`{"type":"tool_execution_end","toolCallId":"call-1","toolName":"bash","result":{"content":[{"type":"text","text":"/tmp/project"}]},"isError":false}`,
		`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"thinking","thinking":"checking files"},{"type":"text","text":"hello world"}]}}`,
	}
	var events []RuntimeEvent
	for _, raw := range rawEvents {
		events = append(events, runtime.NormalizeEvent("", json.RawMessage(raw))...)
	}
	if len(events) != 7 {
		t.Fatalf("normalized events = %#v, want 7", events)
	}
	wantKinds := []RuntimeEventKind{
		RuntimeReasoningDelta, RuntimeTextDelta, RuntimeToolStarted, RuntimeToolUpdated,
		RuntimeToolCompleted, RuntimeReasoningCompleted, RuntimeTextCompleted,
	}
	for i, want := range wantKinds {
		if events[i].Kind != want {
			t.Fatalf("event %d kind = %q, want %q (all=%#v)", i, events[i].Kind, want, events)
		}
	}
	if events[0].ItemID == "" || events[0].ItemID == events[1].ItemID || events[0].Text != "checking" || events[1].Text != "hello" {
		t.Fatalf("stream correlation = %#v", events[:2])
	}
	for _, index := range []int{2, 3, 4} {
		if events[index].ItemID != "call-1" {
			t.Fatalf("tool event %d correlation = %#v", index, events[index])
		}
	}
	if events[3].Item["aggregatedOutput"] != "/tmp" || events[4].Item["aggregatedOutput"] != "/tmp/project" || events[4].Item["status"] != "completed" {
		t.Fatalf("tool lifecycle = %#v", events[2:5])
	}
	if events[5].ItemID != events[0].ItemID || events[5].Text != "checking files" || events[6].ItemID != events[1].ItemID || events[6].Text != "hello world" {
		t.Fatalf("completed message correlation = %#v", events[5:])
	}
}

func TestPiAbortKeepsLoomTurnRunningUntilAbortedAgentSettles(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "abort")
	h := testHub(st)
	h.stop = make(chan struct{})
	defer h.Shutdown()
	agent, err := h.CreateAgent(CreateParams{Name: "pi-abort", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.SendTask(agent.ID, "stream until stopped", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitForPiFile(t, os.Getenv("FAKE_PI_ABORT_READY_FILE"))

	type interruptResult struct {
		result InterruptResult
		err    error
	}
	interruptCh := make(chan interruptResult, 1)
	go func() {
		interrupted, err := h.Interrupt(agent.ID, "Owner stopped Pi")
		interruptCh <- interruptResult{result: interrupted, err: err}
	}()

	waitForPiFile(t, os.Getenv("FAKE_PI_AGENT_END_FILE"))
	view, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "running" || view.CurrentTurnID != result.TurnID || view.LastTurn != nil {
		t.Fatalf("Loom settled before Pi agent_settled: %#v", view.Agent)
	}

	select {
	case stopped := <-interruptCh:
		if stopped.err != nil || !stopped.result.Interrupted {
			t.Fatalf("Interrupt() = (%#v, %v)", stopped.result, stopped.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Interrupt did not return after Pi settled")
	}
	view, err = h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "idle" || view.LastTurn == nil || view.LastTurn.Status != "interrupted" || view.LastTurn.TurnID != result.TurnID {
		t.Fatalf("settled aborted Pi Turn = %#v", view.Agent)
	}
	events, err := st.ReadEvents(agent.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	terminal := ""
	for _, event := range events {
		switch event.Type {
		case "loom/text-delta":
			var payload struct {
				Delta string `json:"delta"`
			}
			_ = json.Unmarshal(event.Data, &payload)
			text += payload.Delta
		case "loom/turn-completed", "loom/turn-failed", "loom/turn-interrupted":
			terminal = event.Type
		}
	}
	if text != "before abort after abort" || terminal != "loom/turn-interrupted" {
		t.Fatalf("post-abort event stream text=%q terminal=%q events=%#v", text, terminal, events)
	}
}

func TestPiRetryAndCompactionContinuationSettleOnlyFinalAssistantState(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "retry-compaction")
	h := testHub(st)
	h.stop = make(chan struct{})
	defer h.Shutdown()
	agent, err := h.CreateAgent(CreateParams{Name: "pi-retry", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.SendTask(agent.ID, "recover transparently", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitForPiFile(t, os.Getenv("FAKE_PI_AGENT_END_FILE"))
	view, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "running" || view.LastTurn != nil {
		t.Fatalf("Loom settled during retry/compaction continuation: %#v", view.Agent)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, _ = h.GetAgent(agent.ID)
		if view.LastTurn != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if view.LastTurn == nil || view.LastTurn.TurnID != result.TurnID || view.LastTurn.Status != "completed" {
		t.Fatalf("settled retry Turn = %#v", view.Agent)
	}
	events, err := st.ReadEvents(agent.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var deltas string
	terminals := 0
	for _, event := range events {
		if event.Type == "loom/text-delta" {
			var payload struct {
				Delta string `json:"delta"`
			}
			_ = json.Unmarshal(event.Data, &payload)
			deltas += payload.Delta
		}
		if event.Type == "loom/turn-completed" || event.Type == "loom/turn-failed" || event.Type == "loom/turn-interrupted" {
			terminals++
		}
	}
	if deltas != "first attemptrecovered" || terminals != 1 {
		t.Fatalf("retry stream deltas=%q terminals=%d events=%#v", deltas, terminals, events)
	}
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
	t.Setenv("FAKE_PI_PROMPT_JSON_FILE", filepath.Join(dir, "prompt.json"))
	t.Setenv("FAKE_PI_STARTS_FILE", filepath.Join(dir, "starts"))
	t.Setenv("FAKE_PI_ABORT_READY_FILE", filepath.Join(dir, "abort-ready"))
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
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "image" {
		fmt.Printf(`{"id":%q,"type":"response","command":"get_state","success":true,"data":{"sessionFile":%q,"sessionId":%q,"model":{"id":"vision","input":["text","image"]}}}`+"\n", id, sessionFile, sessionID)
	} else {
		fmt.Printf(`{"id":%q,"type":"response","command":"get_state","success":true,"data":{"sessionFile":%q,"sessionId":%q}}`+"\n", id, sessionFile, sessionID)
	}
	readFakePiCommand(t, reader, &command)
	id, _ = command["id"].(string)
	message, _ := command["message"].(string)
	_ = os.WriteFile(os.Getenv("FAKE_PI_PROMPT_FILE"), []byte(message), 0o600)
	if encoded, err := json.Marshal(command); err == nil {
		_ = os.WriteFile(os.Getenv("FAKE_PI_PROMPT_JSON_FILE"), encoded, 0o600)
	}
	fmt.Printf(`{"id":%q,"type":"response","command":"prompt","success":true}`+"\n", id)
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "abort" {
		fmt.Print("{\"type\":\"agent_start\"}\n")
		fmt.Print("{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n")
		fmt.Print("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"before abort \"}}\n")
		_ = os.WriteFile(os.Getenv("FAKE_PI_ABORT_READY_FILE"), []byte("ready"), 0o600)
		readFakePiCommand(t, reader, &command)
		id, _ = command["id"].(string)
		if command["type"] != "abort" {
			os.Exit(32)
		}
		fmt.Printf(`{"id":%q,"type":"response","command":"abort","success":true}`+"\n", id)
		fmt.Print("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"after abort\"}}\n")
		fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"aborted\",\"content\":[{\"type\":\"text\",\"text\":\"before abort after abort\"}]}}\n")
		fmt.Print("{\"type\":\"agent_end\",\"willRetry\":false}\n")
		_ = os.WriteFile(os.Getenv("FAKE_PI_AGENT_END_FILE"), []byte("done"), 0o600)
		time.Sleep(250 * time.Millisecond)
		fmt.Print("{\"type\":\"agent_settled\"}\n")
		_, _ = reader.ReadString('\n')
		return
	}
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "retry-compaction" {
		fmt.Print("{\"type\":\"agent_start\"}\n")
		fmt.Print("{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n")
		fmt.Print("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"first attempt\"}}\n")
		fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"error\",\"errorMessage\":\"transient overload\",\"content\":[]}}\n")
		fmt.Print("{\"type\":\"agent_end\",\"willRetry\":true}\n")
		fmt.Print("{\"type\":\"auto_retry_start\",\"attempt\":1,\"maxAttempts\":3}\n")
		_ = os.WriteFile(os.Getenv("FAKE_PI_AGENT_END_FILE"), []byte("done"), 0o600)
		time.Sleep(250 * time.Millisecond)
		fmt.Print("{\"type\":\"auto_retry_end\",\"success\":true,\"attempt\":1}\n")
		fmt.Print("{\"type\":\"compaction_start\",\"reason\":\"overflow\"}\n")
		fmt.Print("{\"type\":\"compaction_end\",\"reason\":\"overflow\",\"aborted\":false,\"willRetry\":true}\n")
		fmt.Print("{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n")
		fmt.Print("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"recovered\"}}\n")
		fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":\"recovered\"}]}}\n")
		fmt.Print("{\"type\":\"agent_end\",\"willRetry\":false}\n")
		time.Sleep(100 * time.Millisecond)
		fmt.Print("{\"type\":\"agent_settled\"}\n")
		_, _ = reader.ReadString('\n')
		return
	}
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "malformed-after-prompt" {
		fmt.Print("not-json\n")
		return
	}
	fmt.Print("{\"type\":\"agent_start\"}\n")
	fmt.Print("{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n")
	fmt.Print("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"thinking_delta\",\"contentIndex\":0,\"delta\":\"checking\"}}\n")
	fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"toolUse\",\"content\":[{\"type\":\"thinking\",\"thinking\":\"checking\"}]}}\n")
	fmt.Print("{\"type\":\"tool_execution_start\",\"toolCallId\":\"call-1\",\"toolName\":\"bash\",\"args\":{\"command\":\"pwd\"}}\n")
	fmt.Print("{\"type\":\"tool_execution_update\",\"toolCallId\":\"call-1\",\"toolName\":\"bash\",\"args\":{\"command\":\"pwd\"},\"partialResult\":{\"content\":[{\"type\":\"text\",\"text\":\"/tmp\"}]}}\n")
	fmt.Print("{\"type\":\"tool_execution_end\",\"toolCallId\":\"call-1\",\"toolName\":\"bash\",\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"/tmp/project\"}]},\"isError\":false}\n")
	fmt.Print("{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n")
	fmt.Print("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"hello from Pi\"}}\n")
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
