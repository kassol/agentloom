package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	gort "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/claudebridge"
	"github.com/yan5xu/codex-loom/internal/claudegen"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func fakeClaudeBridgeDriver(t *testing.T, st *store.Store) *claudeRuntimeHostDriver {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.sh")
	hello := fmt.Sprintf(`{"kind":"hello","protocolVersion":1,"bridgeBuild":"claude-bridge-v1","nodeVersion":"24.19.0","sdkVersion":"0.3.228","claudeCodeVersion":"2.1.228","os":%q,"arch":%q,"capabilities":["interrupt","approval","hooks","mcp","session_resume"]}`, gort.GOOS, gort.GOARCH)
	script := `#!/bin/sh
printf '%s\n' '` + hello + `'
IFS= read -r init
request_id=$(printf '%s' "$init" | sed -n 's/.*"requestId":"\([^"]*\)".*/\1/p')
printf '{"kind":"ready","requestId":"%s","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}\n' "$request_id"
while IFS= read -r line; do
  [ "$(printf '%s' "$line" | sed -n 's/.*"kind":"\([^"]*\)".*/\1/p')" = close ] && exit 0
  request_id=$(printf '%s' "$line" | sed -n 's/.*"requestId":"\([^"]*\)".*/\1/p')
  operation=$(printf '%s' "$line" | sed -n 's/.*"operation":"\([^"]*\)".*/\1/p')
  turn_id=$(printf '%s' "$line" | sed -n 's/.*"turnId":"\([^"]*\)".*/\1/p')
  command=$(printf '%s' "$line" | sed -n 's/.*"command":"\([^"]*\)".*/\1/p')
  case "$command" in
    resume_binding)
	  data='{}'
	  printf '{"kind":"response","requestId":"%s","turnId":"%s","operation":"%s","accepted":true,"data":%s}\n' "$request_id" "$turn_id" "$operation" "$data"
	  printf '{"kind":"event","class":"control","event":"binding_resumed","turnId":"%s","operation":"%s","data":{}}\n' "$turn_id" "$operation"
	  continue ;;
    start_turn)
	  session_ref=$(printf '%s' "$line" | sed -n 's/.*"sessionRef":"\([^"]*\)".*/\1/p')
	  [ "${#session_ref}" -eq 36 ] || { printf '{"kind":"response","requestId":"%s","turnId":"%s","operation":"%s","accepted":false,"error":"bad reserved session"}\n' "$request_id" "$turn_id" "$operation"; continue; }
      data='{"runtimeTurnRef":"native-turn"}'
      printf '{"kind":"response","requestId":"%s","turnId":"%s","operation":"%s","accepted":true,"data":%s}\n' "$request_id" "$turn_id" "$operation" "$data"
      printf '{"kind":"event","class":"control","event":"turn_started","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-turn"}}\n' "$turn_id" "$operation"
	  printf '{"kind":"event","class":"control","event":"content","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-turn","phase":"completed","content":{"id":"native-user","kind":"user_text","text":"work"}}}\n' "$turn_id" "$operation"
      printf '{"kind":"event","class":"control","event":"content","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-turn","phase":"completed","content":{"id":"native-answer","kind":"assistant_text","text":"done"}}}\n' "$turn_id" "$operation"
	  printf '{"kind":"event","class":"control","event":"usage","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-turn","usage":{"inputTokens":{"available":true,"value":3,"source":"claude_agent_sdk"},"cachedInputTokens":{"available":true,"value":0,"source":"claude_agent_sdk"},"outputTokens":{"available":true,"value":1,"source":"claude_agent_sdk"},"reasoningOutputTokens":{"available":false,"source":"claude_agent_sdk"},"totalTokens":{"available":true,"value":4,"source":"claude_agent_sdk"},"calls":{"available":true,"value":1,"source":"claude_agent_sdk"},"costMicros":{"available":true,"value":5,"source":"claude_agent_sdk"}}}}\n' "$turn_id" "$operation"
      printf '{"kind":"event","class":"control","event":"turn_completed","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-turn"}}\n' "$turn_id" "$operation"
	  printf '{"kind":"event","class":"control","event":"turn_completed","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-turn"}}\n' "$turn_id" "$operation"
      continue ;;
    continue_turn|interrupt_turn) data='{}' ;;
    *) printf '{"kind":"response","requestId":"%s","turnId":"%s","operation":"%s","accepted":false,"error":"unsupported"}\n' "$request_id" "$turn_id" "$operation"; continue ;;
  esac
  printf '{"kind":"response","requestId":"%s","turnId":"%s","operation":"%s","accepted":true,"data":%s}\n' "$request_id" "$turn_id" "$operation" "$data"
done
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := claudegen.CurrentManifest()
	return newClaudeRuntimeHostDriver(st, claudebridge.DriverOptions{ResolveActive: func(context.Context) (claudebridge.LaunchSpec, error) {
		return claudebridge.LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Manifest: manifest}, nil
	}})
}

func TestClaudeCanonicalHistoryIsColdAndLedgerOnly(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	if _, err := h.RestoreAgent(RestoreAgentParams{
		ID: "agent-claude", Name: "claude", Cwd: t.TempDir(), ThreadID: "thread-loom",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "claude", NativeRef: "session-private"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveCanonicalTurnLedger("agent-claude", []runtimecontract.HistoryTurn{{
		TurnID: "turn-loom", State: runtimecontract.LifecycleCompleted,
		Content: []runtimecontract.ContentBlock{{ID: "item-loom", Kind: runtimecontract.ContentAssistantText, Text: "cold"}},
	}}); err != nil {
		t.Fatal(err)
	}
	resolved := 0
	h.runtimeHostDrivers["claude"] = newClaudeRuntimeHostDriver(st, claudebridge.DriverOptions{ResolveActive: func(context.Context) (claudebridge.LaunchSpec, error) {
		resolved++
		return claudebridge.LaunchSpec{}, nil
	}})

	history, err := h.CanonicalHistory("agent-claude", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != 0 {
		t.Fatalf("cold History resolved or started the Claude generation %d times", resolved)
	}
	if history.Total != 1 || len(history.Turns) != 1 || history.Turns[0].TurnID != "turn-loom" || history.Turns[0].Content[0].Text != "cold" {
		t.Fatalf("history = %#v", history)
	}
}

func TestClaudeRuntimeRunsAndReopensFromCanonicalTurnLedger(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	h.runtimeHostDrivers["claude"] = fakeClaudeBridgeDriver(t, st)
	agent, err := h.CreateAgent(CreateParams{Name: "claude-worker", Cwd: t.TempDir(), RuntimeKind: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if nativeRef := h.agents[agent.ID].RuntimeBinding.NativeRef; nativeRef == "11111111-1111-4111-8111-111111111111" || len(nativeRef) != 36 {
		t.Fatalf("CreateBinding did not reserve a Loom-generated Claude session UUID: %q", nativeRef)
	}
	result, err := h.SendTask(agent.ID, "work", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var history CanonicalHistory
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		history, err = h.CanonicalHistory(agent.ID, 10, 0)
		if err == nil && history.Total == 1 && history.Turns[0].State == runtimecontract.LifecycleCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if history.Total != 1 || history.Turns[0].TurnID != result.TurnID || history.Turns[0].State != runtimecontract.LifecycleCompleted || len(history.Turns[0].Content) != 2 || history.Turns[0].Content[0].Text != "work" || history.Turns[0].Content[1].Text != "done" || history.Turns[0].Usage == nil || history.Turns[0].Usage.TotalTokens.Value != 4 {
		t.Fatalf("history = %#v, result = %#v", history, result)
	}
	if encoded := fmt.Sprintf("%#v", history); strings.Contains(encoded, "native-turn") || strings.Contains(encoded, "native-answer") {
		t.Fatalf("public History leaked native identity: %s", encoded)
	}

	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(reopenedStore)
	if err != nil {
		t.Fatal(err)
	}
	reopened.runtimeHostDrivers["claude"] = fakeClaudeBridgeDriver(t, reopenedStore)
	t.Cleanup(func() { reopened.Shutdown(); _ = reopenedStore.Close() })
	cold, err := reopened.CanonicalHistory(agent.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cold.Total != 1 || cold.Turns[0].TurnID != result.TurnID || cold.Turns[0].Content[1].Text != "done" {
		t.Fatalf("reopened History = %#v", cold)
	}
	before, _ := reopened.GetRuntimeDiagnostics(agent.ID)
	second, err := reopened.SendTask(agent.ID, "work", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	wantCwd := reopened.agents[agent.ID].Cwd
	if reopened.runtimes[agent.ID].runtimeContract.(*claudeRuntimeContract).cwd != wantCwd {
		t.Fatalf("reopened Runtime cwd = %q, want %q", reopened.runtimes[agent.ID].runtimeContract.(*claudeRuntimeContract).cwd, wantCwd)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cold, err = reopened.CanonicalHistory(agent.ID, 10, 0)
		if err == nil && cold.Total == 2 && cold.Turns[1].TurnID == second.TurnID && cold.Turns[1].State == runtimecontract.LifecycleCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	after, _ := reopened.GetRuntimeDiagnostics(agent.ID)
	if before.NativeRef != after.NativeRef || after.NativeRef == "" || cold.Total != 2 {
		t.Fatalf("reopen replaced session or replayed History: before=%#v after=%#v history=%#v", before, after, cold)
	}
}

func TestClaudeLedgerSanitizesBeforeWritingDisk(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := newClaudeRuntimeContract("agent-safe", st, nil)
	c.handleBridgeEvent(claudebridge.Event{Kind: "turn_started", TurnID: "turn-safe", Data: json.RawMessage(`{"runtimeTurnRef":"native-turn"}`)})
	c.handleBridgeEvent(claudebridge.Event{Kind: "content", TurnID: "turn-safe", Data: json.RawMessage(`{"runtimeTurnRef":"native-turn","phase":"completed","content":{"id":"native-item","kind":"tool_call","toolCall":{"name":"Read","arguments":{"nativeSession":"session-secret","path":"/Users/owner/private","authorization":"Bearer super-secret"}}}}`)})
	c.handleBridgeEvent(claudebridge.Event{Kind: "content", TurnID: "turn-safe", Data: json.RawMessage(`{"runtimeTurnRef":"native-turn","phase":"completed","content":{"id":"native-result","kind":"tool_result","toolResult":{"toolCallId":"native-item","text":"Bearer super-secret at /Users/owner/private","success":false}}}`)})
	c.handleBridgeEvent(claudebridge.Event{Kind: "turn_completed", TurnID: "turn-safe", Data: json.RawMessage(`{"runtimeTurnRef":"native-turn"}`)})
	disk, err := os.ReadFile(filepath.Join(st.Dir(), "canonical-turn-ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"native-turn", "native-item", "native-result", "session-secret", "/Users/owner/private", "super-secret"} {
		if bytes.Contains(disk, []byte(secret)) {
			t.Fatalf("ledger contains private value %q: %s", secret, disk)
		}
	}
}

func TestClaudeLedgerSaveFailureDoesNotMutateCommittedReducerState(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := newClaudeRuntimeContract("agent-atomic", st, nil)
	c.handleBridgeEvent(claudebridge.Event{Kind: "turn_started", TurnID: "turn-atomic", Data: json.RawMessage(`{"runtimeTurnRef":"native-turn"}`)})
	c.handleBridgeEvent(claudebridge.Event{Kind: "content", TurnID: "turn-atomic", Data: json.RawMessage(`{"runtimeTurnRef":"native-turn","phase":"completed","content":{"id":"item","kind":"assistant_text","text":"committed"}}`)})
	ledger := filepath.Join(st.Dir(), "canonical-turn-ledger.json")
	if err := os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ledger, 0o700); err != nil {
		t.Fatal(err)
	}
	c.handleBridgeEvent(claudebridge.Event{Kind: "content", TurnID: "turn-atomic", Data: json.RawMessage(`{"runtimeTurnRef":"native-turn","phase":"completed","content":{"id":"item","kind":"assistant_text","text":"must-not-commit"}}`)})
	if len(c.turns) != 1 || len(c.turns[0].Content) != 1 || c.turns[0].Content[0].Text != "committed" {
		t.Fatalf("failed Save mutated committed reducer state: %#v", c.turns)
	}
}

func TestClaudeStartupRepairsCommittedTerminalBeforeInterruptedProjection(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	meta := &Agent{ID: "agent-crash", Name: "claude-crash", Cwd: t.TempDir(), ThreadID: "loom-thread", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "claude", NativeRef: "11111111-1111-4111-8111-111111111111"}, Sandbox: "danger-full-access", ApprovalPolicy: "never", Status: "running", CurrentTurnID: "turn-crash", CurrentTask: "work", CreatedAt: stamp, UpdatedAt: stamp}
	if err := st.SaveAgents(map[string]*Agent{meta.ID: meta}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveCanonicalTurnLedger(meta.ID, []runtimecontract.HistoryTurn{{TurnID: "turn-crash", State: runtimecontract.LifecycleCompleted, Content: []runtimecontract.ContentBlock{}, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano)}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(reopenedStore)
	if err != nil {
		t.Fatal(err)
	}
	view, err := reopened.GetAgent(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "idle" || view.LastTurn == nil || view.LastTurn.Status != "completed" || view.CurrentTurnID != "" {
		t.Fatalf("startup projection = %#v", view.Agent)
	}
}

func TestClaudeTerminalRepairFinishesInternalMessageHandling(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	t.Cleanup(func() { h.Shutdown(); _ = st.Close() })
	meta := &Agent{ID: "agent-message", Name: "claude-message", Cwd: t.TempDir(), ThreadID: "loom-thread", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "claude", NativeRef: "11111111-1111-4111-8111-111111111111"}, Status: "running", CurrentTurnID: "turn-message", CurrentTask: "handle message"}
	h.mu.Lock()
	h.agents[meta.ID] = meta
	h.comms["message-1"] = &AgentMessage{ID: "message-1", ToAgentID: meta.ID, To: meta.Name, DeliveryStatus: "delivered", DeliveryMode: "turn_start", DeliveredTurnID: "turn-message", HandlingStatus: "running", ActiveHandlingID: "attempt-1", HandlingAttempts: []AgentMessageHandlingAttempt{{ID: "attempt-1", TurnID: "turn-message", Status: "running", StartedAt: now()}}}
	h.commOrder = append(h.commOrder, "message-1")
	h.mu.Unlock()
	if err := st.SaveCanonicalTurnLedger(meta.ID, []runtimecontract.HistoryTurn{{TurnID: "turn-message", State: runtimecontract.LifecycleCompleted, Content: []runtimecontract.ContentBlock{{ID: "answer", Kind: runtimecontract.ContentAssistantText, Text: "handled"}}}}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	repaired, status, err := h.repairClaudeTerminalFromLedgerLocked(meta)
	if err == nil && repaired != nil {
		h.finishAgentMessageTurnLocked(repaired, status, "")
	}
	message := *h.comms["message-1"]
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if repaired == nil || status != "completed" || message.HandlingStatus != "completed" || len(message.HandlingAttempts) != 1 || message.HandlingAttempts[0].Status != "completed" {
		t.Fatalf("repair=%#v status=%q message=%#v", repaired, status, message)
	}
}

func TestClaudeTerminalRepairTopicEventIsIdempotentAcrossRetry(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	t.Cleanup(func() { h.Shutdown(); _ = st.Close() })
	topic := &Topic{ID: "topic-repair", Title: "Repair", Purpose: "test", CompletionBoundary: "done", Status: TopicStatusActive, ResponsibleAgentID: "agent", ResponsibleAgent: "claude", Version: 1, CreatedAt: now(), UpdatedAt: now()}
	h.mu.Lock()
	h.topics[topic.ID] = topic
	event := TopicEvent{Type: "turn_completed", Actor: "claude", AgentID: "agent", Agent: "claude", Summary: "completed", Ref: &TopicRef{Type: "turn", ID: "turn-stable", Label: "claude"}}
	h.recordTopicWorkEventOnceLocked(topic.ID, event)
	h.recordTopicWorkEventOnceLocked(topic.ID, event)
	count := len(topic.Events)
	h.mu.Unlock()
	if count != 1 || topic.Events[0].Ref == nil || topic.Events[0].Ref.ID != "turn-stable" {
		t.Fatalf("Topic repair events = %#v", topic.Events)
	}
}

func TestClaudeConcurrentAcquireDoesNotCloseSharedBridge(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	driver := fakeClaudeBridgeDriver(t, st)
	var wg sync.WaitGroup
	handles := make(chan AgentHost, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			host, err := driver.Acquire(context.Background(), AgentHostRequest{AgentID: "agent-concurrent", Cwd: t.TempDir()})
			handles <- host
			errs <- err
		}()
	}
	wg.Wait()
	close(handles)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first AgentHost
	for host := range handles {
		if first == nil {
			first = host
		}
		if host != first {
			t.Fatalf("Acquire returned distinct handles: %p %p", first, host)
		}
	}
	if first == nil || !first.Alive() {
		t.Fatal("concurrent loser closed the shared Claude Bridge")
	}
	if err := driver.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeConversationCatalogIsTruthful(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	h.runtimeHostDrivers["claude"] = newClaudeRuntimeHostDriver(nil, claudebridge.DriverOptions{ResolveActive: func(context.Context) (claudebridge.LaunchSpec, error) { return claudebridge.LaunchSpec{}, nil }})
	catalogs := h.RuntimeConversationCatalogs()
	for _, catalog := range catalogs {
		if catalog.RuntimeKind != "claude" {
			continue
		}
		if len(catalog.Capabilities) != 4 || catalog.Capabilities[0].Available || !catalog.Capabilities[3].Available {
			t.Fatalf("Claude catalog = %#v", catalog)
		}
		return
	}
	t.Fatal("Claude Runtime missing from catalog")
}

func TestClaudeArchiveRestorePreservesLedgerAndAgentIdentity(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	h.runtimeHostDrivers["claude"] = newClaudeRuntimeHostDriver(st, claudebridge.DriverOptions{ResolveActive: func(context.Context) (claudebridge.LaunchSpec, error) { return claudebridge.LaunchSpec{}, nil }})
	params := RestoreAgentParams{ID: "agent-archive", Name: "claude-archive", Cwd: t.TempDir(), ThreadID: "loom-thread", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "claude", NativeRef: "11111111-1111-4111-8111-111111111111"}}
	if _, err := h.RestoreAgent(params); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveCanonicalTurnLedger(params.ID, []runtimecontract.HistoryTurn{{TurnID: "turn-stable", State: runtimecontract.LifecycleCompleted, Content: []runtimecontract.ContentBlock{{ID: "item-stable", Kind: runtimecontract.ContentAssistantText, Text: "preserved"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ArchiveAgent(params.ID); err != nil {
		t.Fatal(err)
	}
	restored, err := h.RestoreAgent(params)
	if err != nil {
		t.Fatal(err)
	}
	history, err := h.CanonicalHistory(restored.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != params.ID || restored.ThreadID != params.ThreadID || history.Total != 1 || history.Turns[0].TurnID != "turn-stable" || history.Turns[0].Content[0].Text != "preserved" {
		t.Fatalf("restore lost stable identity or ledger: agent=%#v history=%#v", restored.Agent, history)
	}
}

func TestClaudeLedgerFailureSuppressesCanonicalRuntimeEvent(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	h.runtimeHostDrivers["claude"] = fakeClaudeBridgeDriver(t, st)
	agent, err := h.CreateAgent(CreateParams{Name: "claude-failure", Cwd: t.TempDir(), RuntimeKind: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(st.Dir(), "canonical-turn-ledger.json")
	if err := os.Mkdir(ledger, 0o700); err != nil {
		t.Fatal(err)
	}
	beforeGlobal := h.LastGlobalSeq()
	_, _ = h.SendTask(agent.ID, "work", time.Minute)
	time.Sleep(100 * time.Millisecond)
	events, err := h.ReadEvents(agent.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "loom/runtime-event" || event.Type == "loom/turn-completed" || event.Type == "loom/turn-failed" || event.Type == "loom/turn-interrupted" || bytes.Contains(event.Data, []byte(`"kind":"terminal"`)) {
			t.Fatalf("published canonical Runtime event before ledger commit: %#v", event)
		}
	}
	global, err := h.ReadGlobalEvents(beforeGlobal, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range global {
		terminalIdle := bytes.Contains(event.Data, []byte(`"status":"idle"`)) && bytes.Contains(event.Data, []byte(`"lastTurn":{`))
		if event.Type == "loom/agent-status" && (terminalIdle || bytes.Contains(event.Data, []byte(`"status":"interrupted"`))) {
			t.Fatalf("published terminal Agent status before ledger commit: %#v", event)
		}
	}
}

func TestClaudeCausalContinueKeepsOneTurnAndOnePublicTerminal(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "continue.sh")
	hello := fmt.Sprintf(`{"kind":"hello","protocolVersion":1,"bridgeBuild":"claude-bridge-v1","nodeVersion":"24.19.0","sdkVersion":"0.3.228","claudeCodeVersion":"2.1.228","os":%q,"arch":%q,"capabilities":["interrupt","approval","hooks","mcp","session_resume"]}`, gort.GOOS, gort.GOARCH)
	script := `#!/bin/sh
printf '%s\n' '` + hello + `'
IFS= read -r init
request_id=$(printf '%s' "$init" | sed -n 's/.*"requestId":"\([^"]*\)".*/\1/p')
printf '{"kind":"ready","requestId":"%s","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}\n' "$request_id"
while IFS= read -r line; do
  request_id=$(printf '%s' "$line" | sed -n 's/.*"requestId":"\([^"]*\)".*/\1/p')
  operation=$(printf '%s' "$line" | sed -n 's/.*"operation":"\([^"]*\)".*/\1/p')
  turn_id=$(printf '%s' "$line" | sed -n 's/.*"turnId":"\([^"]*\)".*/\1/p')
  command=$(printf '%s' "$line" | sed -n 's/.*"command":"\([^"]*\)".*/\1/p')
  case "$command" in
    start_turn)
      start_op=$operation
      printf '{"kind":"response","requestId":"%s","turnId":"%s","operation":"%s","accepted":true,"data":{"runtimeTurnRef":"native-causal"}}\n' "$request_id" "$turn_id" "$operation"
      printf '{"kind":"event","class":"control","event":"turn_started","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-causal"}}\n' "$turn_id" "$operation" ;;
    continue_turn)
      printf '{"kind":"response","requestId":"%s","turnId":"%s","operation":"%s","accepted":true,"data":{}}\n' "$request_id" "$turn_id" "$operation"
      printf '{"kind":"event","class":"control","event":"content","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-causal","phase":"completed","content":{"id":"native-user-continue","kind":"user_text","text":"more"}}}\n' "$turn_id" "$operation"
      printf '{"kind":"event","class":"control","event":"content","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-causal","phase":"completed","content":{"id":"native-answer","kind":"assistant_text","text":"continued"}}}\n' "$turn_id" "$operation"
      printf '{"kind":"event","class":"control","event":"turn_completed","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-causal"}}\n' "$turn_id" "$start_op"
      printf '{"kind":"event","class":"control","event":"turn_completed","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-causal"}}\n' "$turn_id" "$operation" ;;
    close) exit 0 ;;
  esac
done
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	driver := newClaudeRuntimeHostDriver(st, claudebridge.DriverOptions{ResolveActive: func(context.Context) (claudebridge.LaunchSpec, error) {
		return claudebridge.LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Manifest: claudegen.CurrentManifest()}, nil
	}})
	host, err := driver.Acquire(context.Background(), AgentHostRequest{AgentID: "agent-causal", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	contract := host.Contract()
	binding := runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "claude", NativeRef: "11111111-1111-4111-8111-111111111111"}
	var eventsMu sync.Mutex
	var events []runtimecontract.Event
	contract.SetEventHandler(func(event runtimecontract.Event) { eventsMu.Lock(); events = append(events, event); eventsMu.Unlock() })
	started := contract.StartTurn(context.Background(), runtimecontract.TurnRequest{Binding: binding, TurnID: "turn-causal", Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleUser, Text: "start"}}})
	if started.State != runtimecontract.LifecycleAccepted || started.RuntimeTurnRef != "native-causal" {
		t.Fatalf("Start = %#v", started)
	}
	continued := contract.ContinueTurn(context.Background(), runtimecontract.CausalInput{Binding: binding, TurnID: "turn-causal", RuntimeTurnRef: started.RuntimeTurnRef, Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleUser, Text: "more"}}})
	if continued.State != runtimecontract.LifecycleAccepted || continued.RuntimeTurnRef != started.RuntimeTurnRef {
		t.Fatalf("Continue = %#v", continued)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		history, _ := st.LoadCanonicalTurnLedger("agent-causal", 10, 0)
		if history.Total == 1 && history.Turns[0].State == runtimecontract.LifecycleCompleted {
			if len(history.Turns[0].Content) != 2 || history.Turns[0].Content[0].Text != "more" || history.Turns[0].Content[1].Text != "continued" {
				t.Fatalf("History = %#v", history)
			}
			eventsMu.Lock()
			observed := append([]runtimecontract.Event(nil), events...)
			eventsMu.Unlock()
			terminals := 0
			for _, event := range observed {
				if event.Kind == runtimecontract.EventTerminal {
					terminals++
				}
			}
			if terminals != 1 {
				t.Fatalf("terminal events = %d, events=%#v", terminals, observed)
			}
			if err := driver.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	t.Fatalf("Continue did not close one canonical Turn: %#v", events)
}
