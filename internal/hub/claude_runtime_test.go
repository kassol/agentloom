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

func TestClaudeInterruptReceiptWaitsForCorrelatedTerminal(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "interrupt.sh")
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
      printf '{"kind":"response","requestId":"%s","turnId":"%s","operation":"%s","accepted":true,"data":{"runtimeTurnRef":"native-interrupt"}}\n' "$request_id" "$turn_id" "$operation"
      printf '{"kind":"event","class":"control","event":"turn_started","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-interrupt"}}\n' "$turn_id" "$operation" ;;
    interrupt_turn)
      printf '{"kind":"response","requestId":"%s","turnId":"%s","operation":"%s","accepted":true,"data":{}}\n' "$request_id" "$turn_id" "$operation"
      sleep 0.1
      printf '{"kind":"event","class":"control","event":"turn_interrupted","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-interrupt"}}\n' "$turn_id" "$start_op"
      printf '{"kind":"event","class":"control","event":"turn_interrupted","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-interrupt"}}\n' "$turn_id" "$operation" ;;
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
	t.Cleanup(func() { _ = driver.Shutdown(context.Background()); _ = st.Close() })
	host, err := driver.Acquire(context.Background(), AgentHostRequest{AgentID: "agent-interrupt", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	contract := host.Contract()
	binding := runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "claude", NativeRef: "11111111-1111-4111-8111-111111111111"}
	started := contract.StartTurn(context.Background(), runtimecontract.TurnRequest{Binding: binding, TurnID: "turn-interrupt", Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleUser, Text: "work"}}})
	if started.State != runtimecontract.LifecycleAccepted {
		t.Fatalf("Start = %#v", started)
	}
	receipt := contract.InterruptTurn(context.Background(), runtimecontract.TurnTarget{Binding: binding, TurnID: "turn-interrupt", RuntimeTurnRef: started.RuntimeTurnRef})
	if receipt.State != runtimecontract.LifecycleAccepted || receipt.RuntimeTurnRef != started.RuntimeTurnRef {
		t.Fatalf("interrupt receipt = %#v", receipt)
	}
	history, err := st.LoadCanonicalTurnLedger("agent-interrupt", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if history.Total != 1 || history.Turns[0].State != runtimecontract.LifecycleAccepted {
		t.Fatalf("receipt settled Turn before terminal evidence: %#v", history)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		history, err = st.LoadCanonicalTurnLedger("agent-interrupt", 10, 0)
		if err == nil && history.Total == 1 && history.Turns[0].State == runtimecontract.LifecycleInterrupted {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("correlated terminal did not settle Turn: %#v err=%v", history, err)
}

func TestClaudeCommittedInterruptTerminalBeatsReceiptGrace(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "interrupt-commit-race.sh")
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
      printf '{"kind":"event","class":"control","event":"turn_started","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-race"}}\n' "$turn_id" "$operation"
      printf '{"kind":"response","requestId":"%s","turnId":"%s","operation":"%s","accepted":true,"data":{"runtimeTurnRef":"native-race"}}\n' "$request_id" "$turn_id" "$operation" ;;
    interrupt_turn)
      printf '{"kind":"event","class":"control","event":"interrupt_receipt","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-race"}}\n' "$turn_id" "$operation"
      printf '{"kind":"response","requestId":"%s","turnId":"%s","operation":"%s","accepted":true,"data":{}}\n' "$request_id" "$turn_id" "$operation"
      printf '{"kind":"event","class":"control","event":"turn_interrupted","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-race"}}\n' "$turn_id" "$operation" ;;
    close) exit 0 ;;
  esac
done
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	h := New(st)
	h.interruptTerminalGraceForTest = 25 * time.Millisecond
	h.runtimeHostDrivers["claude"] = newClaudeRuntimeHostDriver(st, claudebridge.DriverOptions{ResolveActive: func(context.Context) (claudebridge.LaunchSpec, error) {
		return claudebridge.LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Manifest: claudegen.CurrentManifest()}, nil
	}})
	t.Cleanup(func() { h.Shutdown(); _ = st.Close() })
	agent, err := h.CreateAgent(CreateParams{Name: "claude-interrupt-race", Cwd: t.TempDir(), RuntimeKind: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.SendTask(agent.ID, "work", time.Minute); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	contract := h.runtimes[agent.ID].runtimeContract.(*claudeRuntimeContract)
	h.mu.Unlock()
	committed, release := make(chan struct{}), make(chan struct{})
	contract.mu.Lock()
	contract.afterLedgerCommitForTest = func() { close(committed); <-release }
	contract.mu.Unlock()
	if _, err := h.Interrupt(agent.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	<-committed
	time.Sleep(2 * h.effectiveInterruptTerminalGrace())
	view, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Recovery != nil || view.LastError != "" {
		t.Fatalf("receipt timer fenced a durably committed terminal: %#v", view)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		view, _ = h.GetAgent(agent.ID)
		if view.LastTurn != nil && view.LastTurn.Status == "interrupted" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("committed interrupt terminal did not reach Hub: %#v", view)
}

func TestClaudeReceiptGraceClaimFencesLaterTerminalCommit(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	contract := newClaudeRuntimeContract("agent-grace-claim", st, nil)
	contract.handleBridgeEvent(claudebridge.Event{
		Kind: "turn_started", TurnID: "turn-grace-claim", Operation: "op-start",
		Data: json.RawMessage(`{"runtimeTurnRef":"native-grace-claim"}`),
	})
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	contract.mu.Lock()
	contract.beforeLedgerCommitForTest = func() { close(entered); <-release }
	contract.mu.Unlock()
	go func() {
		contract.handleBridgeEvent(claudebridge.Event{
			Kind: "turn_interrupted", TurnID: "turn-grace-claim", Operation: "op-start",
			Data: json.RawMessage(`{"runtimeTurnRef":"native-grace-claim"}`),
		})
		close(done)
	}()
	<-entered
	if !contract.claimTurnUnsettled("turn-grace-claim") {
		t.Fatal("receipt grace did not atomically claim the unsettled Turn")
	}
	close(release)
	<-done
	history, err := st.LoadCanonicalTurnLedger("agent-grace-claim", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if history.Total != 1 || history.Turns[0].State != runtimecontract.LifecycleAccepted {
		t.Fatalf("terminal mutated Ledger after grace claim: %#v", history)
	}
	if contract.claimTurnUnsettled("turn-grace-claim") {
		t.Fatal("same unsettled Turn was claimed twice")
	}
}

func TestClaudeTurnPersistenceFailureSendsNoNativeCommandOrExecutionEvent(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	h.runtimeHostDrivers["claude"] = fakeClaudeBridgeDriver(t, st)
	agent, err := h.CreateAgent(CreateParams{Name: "claude-prefence", Cwd: t.TempDir(), RuntimeKind: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(st.Dir(), "canonical-turn-ledger.json")
	if err := os.Mkdir(ledger, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := h.ReadEvents(agent.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.SendTask(agent.ID, "must not send", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "prepare Runtime Turn persistence") {
		t.Fatalf("SendTask error = %v", err)
	}
	after, err := h.ReadEvents(agent.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("pre-send failure published events: before=%#v after=%#v", before, after)
	}
	h.mu.Lock()
	rt := h.runtimes[agent.ID]
	meta := h.agents[agent.ID]
	h.mu.Unlock()
	if rt == nil || rt.activeTurn != nil || meta.Status != "idle" || meta.CurrentTurnID != "" {
		t.Fatalf("pre-send failure reserved execution state: rt=%#v meta=%#v", rt, meta)
	}
	if contract := rt.runtimeContract.(*claudeRuntimeContract); contract.ops.Load() != 0 {
		t.Fatalf("pre-send failure issued %d native bridge commands", contract.ops.Load())
	}
	h.Shutdown()
}

func TestClaudeIndeterminateRecoveryCreatesOneNeedsYouWithoutReplay(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := now()
	agentID, turnID := "agent-claude-recovery", "turn-uncertain"
	if err := st.SaveAgents(map[string]*Agent{agentID: {
		ID: agentID, Name: "claude-recovery", Cwd: t.TempDir(), ThreadID: "thread-claude",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "claude", NativeRef: "11111111-1111-4111-8111-111111111111"},
		Status:         "interrupted", LastTurn: &TurnSummary{TurnID: turnID, Task: "write once", Status: "interrupted", CompletedAt: stamp},
		TurnRecoveryMarkers: map[string]TurnRecoveryMarker{turnID: {PredecessorTurnID: turnID, RuntimeKind: "claude", Cause: "command_indeterminate", FailurePhase: string(runtimecontract.FailurePhaseTurnStart), FailureCode: "transport_indeterminate", State: TurnRecoveryObserved, Summary: "Runtime command outcome is indeterminate", CreatedAt: stamp, UpdatedAt: stamp}},
		CreatedAt:           stamp, UpdatedAt: stamp,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for reopen := 0; reopen < 2; reopen++ {
		st, err = store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		h, err := Open(st)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		var requests []HumanRequest
		for time.Now().Before(deadline) {
			requests, _ = h.ListHumanRequests(agentID, "all")
			if len(requests) == 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if len(requests) != 1 || requests[0].SourceTurnID != turnID {
			t.Fatalf("reopen %d Needs You = %#v", reopen, requests)
		}
		h.Shutdown()
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
	historyStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer historyStore.Close()
	history, err := historyStore.LoadCanonicalTurnLedger(agentID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if history.Total != 0 {
		t.Fatalf("recovery replayed Claude input: %#v", history)
	}
}

func TestClaudeContractProjectsApprovalAndNeedsYouWithoutNativeIdentity(t *testing.T) {
	c := newClaudeRuntimeContract("agent-claude", nil, nil)
	approvals := make(chan runtimecontract.ApprovalProposal, 1)
	needsYou := make(chan runtimecontract.NeedsYouProposal, 1)
	c.SetApprovalHandler(func(proposal runtimecontract.ApprovalProposal) { approvals <- proposal })
	c.SetNeedsYouHandler(func(proposal runtimecontract.NeedsYouProposal) error { needsYou <- proposal; return nil })
	c.handleBridgeEvent(claudebridge.Event{Kind: "approval", TurnID: "turn-1", Data: json.RawMessage(`{"callbackId":"native-callback-secret","toolCallId":"native-tool-secret","toolName":"Bash","input":{"command":"printf safe"}}`)})
	approval := <-approvals
	encoded, _ := json.Marshal(approval)
	if approval.ID == "" || approval.TurnID != "turn-1" || approval.ToolName != "Bash" || strings.Contains(string(encoded), "native-") {
		t.Fatalf("Approval proposal = %s", encoded)
	}
	c.handleBridgeEvent(claudebridge.Event{Kind: "needs_you", TurnID: "turn-1", Data: json.RawMessage(`{"callbackId":"native-question-secret","toolCallId":"native-question-tool","questions":[{"question":"Ship now?","options":[{"label":"Yes","description":"Ship it"},{"label":"No","description":"Wait"}]}]}`)})
	request := <-needsYou
	encoded, _ = json.Marshal(request)
	if !strings.HasPrefix(request.ID, "hrq_") || request.TurnID != "turn-1" || request.Question != "Ship now?" || len(request.Options) != 2 || strings.Contains(string(encoded), "native-") {
		t.Fatalf("Needs You proposal = %s", encoded)
	}
}

func TestClaudeNeedsYouTerminalIsInterruptedInLedgerAndColdHistory(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := newClaudeRuntimeContract("agent-claude", st, nil)
	c.SetNeedsYouHandler(func(runtimecontract.NeedsYouProposal) error { return nil })
	c.handleBridgeEvent(claudebridge.Event{Kind: "turn_started", TurnID: "turn-1", Data: json.RawMessage(`{"runtimeTurnRef":"native-turn"}`)})
	c.handleBridgeEvent(claudebridge.Event{Kind: "needs_you", TurnID: "turn-1", Data: json.RawMessage(`{"callbackId":"question-1","toolCallId":"tool-1","questions":[{"question":"Ship now?","options":[{"label":"Yes"},{"label":"No"}]}]}`)})
	c.handleBridgeEvent(claudebridge.Event{Kind: "turn_completed", TurnID: "turn-1", Data: json.RawMessage(`{"runtimeTurnRef":"native-turn"}`)})
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	history, err := reopened.LoadCanonicalTurnLedger("agent-claude", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Turns) != 1 || history.Turns[0].State != runtimecontract.LifecycleInterrupted {
		t.Fatalf("cold canonical history = %#v", history.Turns)
	}
}

func TestClaudeCapabilitySnapshotAdvertisesApprovalOnce(t *testing.T) {
	snapshot := claudeControlPlaneCapabilitySnapshot()
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, descriptor := range snapshot.Capabilities {
		if descriptor.ID == runtimecontract.CapabilityApprovalPolicy && descriptor.Availability == runtimecontract.CapabilityAvailable {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("available Approval descriptors = %d", count)
	}
}

func TestClaudeDuplicateApprovalReusesProposalAndMismatchFences(t *testing.T) {
	c := newClaudeRuntimeContract("agent-claude", nil, nil)
	proposals := make(chan runtimecontract.ApprovalProposal, 2)
	c.SetApprovalHandler(func(proposal runtimecontract.ApprovalProposal) { proposals <- proposal })
	event := claudebridge.Event{Kind: "approval", TurnID: "turn-1", Data: json.RawMessage(`{"callbackId":"callback-1","toolCallId":"tool-1","toolName":"Bash","input":{"command":"printf safe"}}`)}
	c.handleBridgeEvent(event)
	c.handleBridgeEvent(event)
	first, second := <-proposals, <-proposals
	if first.ID != second.ID {
		t.Fatalf("duplicate proposal IDs = %q, %q", first.ID, second.ID)
	}
	failures := make(chan error, 1)
	host := &claudeAgentHost{contract: c, failure: func(err error) { failures <- err }}
	c.host = host
	event.Data = json.RawMessage(`{"callbackId":"callback-1","toolCallId":"tool-1","toolName":"Bash","input":{"command":"printf changed"}}`)
	c.handleBridgeEvent(event)
	select {
	case err := <-failures:
		if !strings.Contains(err.Error(), "diverged") {
			t.Fatalf("mismatch error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("mismatched duplicate did not fence the Runtime")
	}
}

func TestClaudeApprovalProjectsSafeCustomToolAndRejectsUnsafePayload(t *testing.T) {
	proposal, safe := claudeApprovalProposal("proposal-1", "turn-1", "tool-1", "mcp__deploy", map[string]any{"command": "printf safe"})
	if !safe || proposal.Action != "tool/custom" || proposal.ToolName != "mcp__deploy" || len(proposal.Arguments) != 1 {
		t.Fatalf("safe custom proposal = %#v safe=%v", proposal, safe)
	}
	for _, input := range []map[string]any{
		{"payload": map[string]any{"mutation": "deploy"}},
		{"input": map[string]any{"secretNativeArgument": "deploy"}},
	} {
		if _, safe := claudeApprovalProposal("proposal-2", "turn-1", "tool-2", "mcp__deploy", input); safe {
			t.Fatalf("unsafe input was approvable: %#v", input)
		}
	}
	if _, safe := claudeApprovalProposal("proposal-3", "turn-1", "tool-3", strings.Repeat("x", 129), map[string]any{"command": "true"}); safe {
		t.Fatal("unbounded tool name was approvable")
	}
	for tool, input := range map[string]map[string]any{
		"Read":         {"file_path": "/tmp/a", "offset": 1, "limit": 2},
		"Write":        {"file_path": "/tmp/a", "content": "safe"},
		"Edit":         {"file_path": "/tmp/a", "old_string": "old", "new_string": "new", "replace_all": false},
		"Bash":         {"command": "true", "timeout": 1000, "run_in_background": false},
		"NotebookEdit": {"notebook_path": "/tmp/a.ipynb", "cell_id": "cell-1", "new_source": "1", "edit_mode": "replace"},
	} {
		if _, safe := claudeApprovalProposal("proposal-built-in", "turn-1", "tool-built-in", tool, input); !safe {
			t.Fatalf("built-in %s input was rejected: %#v", tool, input)
		}
	}
}

func TestClaudeUnsafeApprovalIsAbortedWithoutOwnerOrToolExecution(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "resolved.json")
	executed := filepath.Join(t.TempDir(), "executed")
	path := filepath.Join(t.TempDir(), "bridge.sh")
	hello := fmt.Sprintf(`{"kind":"hello","protocolVersion":1,"bridgeBuild":"claude-bridge-v1","nodeVersion":"24.19.0","sdkVersion":"0.3.228","claudeCodeVersion":"2.1.228","os":%q,"arch":%q,"capabilities":["interrupt","approval","hooks","mcp","session_resume"]}`, gort.GOOS, gort.GOARCH)
	script := `#!/bin/sh
set -eu
printf '%s\n' '` + hello + `'
IFS= read -r init
printf '%s\n' '{"kind":"ready","requestId":"init","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
while IFS= read -r line; do
  [ "$line" = '{"kind":"close"}' ] && exit 0
  printf '%s' "$line" > "$1"
	printf '%s' "$line" | grep -q '"decision":"approve"' && printf executed > "$2" || true
  request_id=$(printf '%s' "$line" | sed -n 's/.*"requestId":"\([^"]*\)".*/\1/p')
  operation=$(printf '%s' "$line" | sed -n 's/.*"operation":"\([^"]*\)".*/\1/p')
  turn_id=$(printf '%s' "$line" | sed -n 's/.*"turnId":"\([^"]*\)".*/\1/p')
  printf '{"kind":"response","requestId":"%s","turnId":"%s","operation":"%s","accepted":true,"data":{}}\n' "$request_id" "$turn_id" "$operation"
done
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := claudegen.CurrentManifest()
	driver := claudebridge.NewDriver(claudebridge.DriverOptions{
		ResolveActive: func(context.Context) (claudebridge.LaunchSpec, error) {
			return claudebridge.LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Args: []string{marker, executed}, Manifest: manifest}, nil
		},
		NextID: func() string { return "init" },
	})
	bridge, err := driver.Acquire(context.Background(), "agent-claude")
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	c := newClaudeRuntimeContract("agent-claude", nil, bridge)
	ownerPrompts := 0
	c.SetApprovalHandler(func(runtimecontract.ApprovalProposal) { ownerPrompts++ })
	c.handleBridgeEvent(claudebridge.Event{Kind: "approval", TurnID: "turn-1", Data: json.RawMessage(`{"callbackId":"callback-1","toolCallId":"tool-1","toolName":"mcp__deploy","input":{"payload":{"mutation":"deploy"}}}`)})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if raw, readErr := os.ReadFile(marker); readErr == nil {
			if !bytes.Contains(raw, []byte(`"command":"resolve_approval"`)) {
				time.Sleep(time.Millisecond)
				continue
			}
			if ownerPrompts != 0 || !bytes.Contains(raw, []byte(`"decision":"abort"`)) {
				t.Fatalf("ownerPrompts=%d command=%s", ownerPrompts, raw)
			}
			if _, err := os.Stat(executed); !os.IsNotExist(err) {
				t.Fatalf("unsafe tool executed: %v", err)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("unsafe callback was not aborted")
}

func TestClaudeParkedApprovalProcessExitAbortsWithoutReplay(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hello := fmt.Sprintf(`{"kind":"hello","protocolVersion":1,"bridgeBuild":"claude-bridge-v1","nodeVersion":"24.19.0","sdkVersion":"0.3.228","claudeCodeVersion":"2.1.228","os":%q,"arch":%q,"capabilities":["interrupt","approval","hooks","mcp","session_resume"]}`, gort.GOOS, gort.GOARCH)
	path := filepath.Join(t.TempDir(), "bridge.sh")
	script := `#!/bin/sh
set -eu
printf '%s\n' '` + hello + `'
IFS= read -r init
printf '%s\n' '{"kind":"ready","requestId":"init","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}'
IFS= read -r command
printf '%s\n' '{"kind":"response","requestId":"request","turnId":"turn-1","operation":"start","accepted":true,"data":{"runtimeTurnRef":"native-turn"}}'
printf '%s\n' '{"kind":"event","class":"control","event":"turn_started","turnId":"turn-1","operation":"start","data":{"runtimeTurnRef":"native-turn"}}'
printf '%s\n' '{"kind":"event","class":"control","event":"approval","turnId":"turn-1","operation":"start","data":{"callbackId":"callback-1","toolCallId":"tool-1","toolName":"Bash","input":{"command":"true"}}}'
sleep 0.05
exit 70
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var contract *claudeRuntimeContract
	nextID := 0
	driver := claudebridge.NewDriver(claudebridge.DriverOptions{
		ResolveActive: func(context.Context) (claudebridge.LaunchSpec, error) {
			return claudebridge.LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Manifest: claudegen.CurrentManifest()}, nil
		},
		NextID: func() string {
			nextID++
			if nextID == 1 {
				return "init"
			}
			return "request"
		},
		OnEvent:   func(event claudebridge.Event) { contract.handleBridgeEvent(event) },
		OnFailure: func(_ string, err error) { contract.fail(err) },
	})
	bridge, err := driver.Acquire(context.Background(), "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	contract = newClaudeRuntimeContract("agent-1", st, bridge)
	host := &claudeAgentHost{bridge: bridge, contract: contract}
	contract.host = host
	h := testHub(st)
	meta := &Agent{ID: "agent-1", Name: "claude", ThreadID: "thread-1", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "claude", NativeRef: "11111111-1111-4111-8111-111111111111"}, Status: "running", CurrentTurnID: "turn-1", CurrentTask: "work"}
	rt := &runtime{agentID: meta.ID, agentHost: host, runtimeContract: contract, approvals: map[string]*approval{}, approvalIDs: map[string]string{}, activeTurn: &turnState{turnID: "turn-1", task: "work", startedAt: time.Now(), stopWatchdog: make(chan struct{})}}
	h.agents[meta.ID], h.runtimes[meta.ID] = meta, rt
	contract.SetEventHandler(func(event runtimecontract.Event) { h.onCanonicalRuntimeEvent(rt, event) })
	contract.SetApprovalHandler(func(proposal runtimecontract.ApprovalProposal) { h.onRuntimeApprovalRequest(rt, proposal) })
	host.SetFailureHandler(func(err error) { h.onRuntimeFailure(rt, err) })
	_, _ = bridge.Request(context.Background(), claudebridge.Command{Kind: "start_turn", TurnID: "turn-1", Operation: "start"})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		finished := meta.LastTurn != nil && meta.LastTurn.Status == "interrupted"
		var approval ApprovalView
		if len(h.approvalOrder) == 1 {
			approval = *h.approvals[h.approvalOrder[0]]
		}
		approvalCount, markerCount := len(h.approvalOrder), len(meta.TurnRecoveryMarkers)
		h.mu.Unlock()
		if finished && approval.Decision == "abort" && approval.DeliveryStatus != "pending" {
			if approvalCount != 1 || markerCount != 1 || approval.Status != "aborted" || approval.DeliveryStatus == "delivered" {
				t.Fatalf("Approval=%#v markers=%d approvals=%d", approval, markerCount, approvalCount)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("parked exit did not converge")
}

func TestClaudeNeedsYouRejectsMultipleQuestions(t *testing.T) {
	c := newClaudeRuntimeContract("agent-claude", nil, nil)
	requests := 0
	c.SetNeedsYouHandler(func(runtimecontract.NeedsYouProposal) error { requests++; return nil })
	failures := make(chan error, 1)
	c.host = &claudeAgentHost{contract: c, failure: func(err error) { failures <- err }}
	c.handleBridgeEvent(claudebridge.Event{Kind: "needs_you", TurnID: "turn-1", Data: json.RawMessage(`{"callbackId":"question-1","toolCallId":"tool-1","questions":[{"question":"First?","options":[]},{"question":"Second?","options":[]}]}`)})
	select {
	case <-failures:
	case <-time.After(time.Second):
		t.Fatal("multiple questions did not fail closed")
	}
	if requests != 0 {
		t.Fatalf("misleading Human Requests = %d", requests)
	}
}

func TestClaudeNeedsYouRejectsMalformedCanonicalOptions(t *testing.T) {
	for name, options := range map[string][]runtimecontract.NeedsYouOption{
		"too many":    make([]runtimecontract.NeedsYouOption, 9),
		"blank label": {{Label: "   "}},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			h := testHub(st)
			meta := &Agent{ID: "agent-1", Name: "claude", ThreadID: "thread-1", RuntimeBinding: RuntimeBinding{Kind: "claude"}, Status: "running"}
			rt := &runtime{agentID: meta.ID, activeTurn: &turnState{turnID: "turn-1", startedAt: time.Now(), stopWatchdog: make(chan struct{})}}
			h.agents[meta.ID], h.runtimes[meta.ID] = meta, rt
			if err := h.onRuntimeNeedsYouProposal(rt, runtimecontract.NeedsYouProposal{ID: "hrq-invalid", TurnID: "turn-1", Question: "Continue?", Options: options}); err == nil {
				t.Fatal("malformed options persisted")
			}
			if len(h.humanRequestOrder) != 0 || len(meta.TurnRecoveryMarkers) != 0 {
				t.Fatalf("requests=%#v markers=%#v", h.humanRequestOrder, meta.TurnRecoveryMarkers)
			}
		})
	}
}

func TestClaudeNeedsYouInterruptsSourceAndAnswerResumesOnceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	meta := &Agent{ID: "agent-1", Name: "claude", ThreadID: "thread-1", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "claude", NativeRef: "11111111-1111-4111-8111-111111111111"}, Status: "running", CurrentTurnID: "turn-source", CurrentTask: "ship release"}
	rt := &runtime{agentID: meta.ID, approvals: map[string]*approval{}, activeTurn: &turnState{turnID: "turn-source", task: "ship release", startedAt: time.Now(), stopWatchdog: make(chan struct{})}}
	h.agents[meta.ID], h.runtimes[meta.ID] = meta, rt
	if err := h.onRuntimeNeedsYouProposal(rt, runtimecontract.NeedsYouProposal{ID: "hrq_claude_question", TurnID: "turn-source", Question: "Ship now?", Options: []runtimecontract.NeedsYouOption{{Label: "Yes"}, {Label: "No"}}}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.onRuntimeEventLocked(meta, rt, runtimecontract.Event{Kind: runtimecontract.EventTerminal, TurnID: "turn-source", Outcome: &runtimecontract.Outcome{State: runtimecontract.LifecycleInterrupted}})
	h.mu.Unlock()
	request, err := h.GetHumanRequest("hrq_claude_question")
	if err != nil || request.SourceTurnID != "turn-source" || request.BlockedWork != "ship release" || request.DeliveryStatus != "waiting" || meta.LastTurn == nil || meta.LastTurn.Status != "interrupted" {
		t.Fatalf("request=%#v lastTurn=%#v err=%v", request, meta.LastTurn, err)
	}
	deliveries := 0
	h.dispatchHumanAnswer = func(agentID, input string) (SendResult, error) {
		deliveries++
		return SendResult{AgentID: agentID, TurnID: "turn-recovery"}, nil
	}
	if _, err := h.AnswerHumanRequest(request.ID, AnswerHumanRequestParams{Answer: "Yes"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		request, _ = h.GetHumanRequest(request.ID)
		if request.DeliveryStatus == "delivered" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if deliveries != 1 || request.ResumedTurnID != "turn-recovery" {
		t.Fatalf("deliveries=%d request=%#v", deliveries, request)
	}
	if _, err := h.AnswerHumanRequest(request.ID, AnswerHumanRequestParams{Answer: "Again"}); err == nil {
		t.Fatal("duplicate answer succeeded")
	}
	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	h2, err := Open(reopened)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Shutdown()
	request, err = h2.GetHumanRequest(request.ID)
	if err != nil || request.ResumedTurnID != "turn-recovery" || request.DeliveryStatus != "delivered" {
		t.Fatalf("reopened request=%#v err=%v", request, err)
	}
}

func TestClaudeNeedsYouRacingTerminalNeverLeavesOrphanRequest(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	meta := &Agent{ID: "agent-1", Name: "claude", ThreadID: "thread-1", RuntimeBinding: RuntimeBinding{Kind: "claude"}, Status: "running", CurrentTurnID: "turn-1", CurrentTask: "work"}
	rt := &runtime{agentID: meta.ID, approvals: map[string]*approval{}, activeTurn: &turnState{turnID: "turn-1", task: "work", startedAt: time.Now(), stopWatchdog: make(chan struct{})}}
	h.agents[meta.ID], h.runtimes[meta.ID] = meta, rt
	start := make(chan struct{})
	done := make(chan struct{}, 2)
	var proposalErr error
	go func() {
		<-start
		proposalErr = h.onRuntimeNeedsYouProposal(rt, runtimecontract.NeedsYouProposal{ID: "hrq-race", TurnID: "turn-1", Question: "Continue?"})
		done <- struct{}{}
	}()
	go func() {
		<-start
		h.mu.Lock()
		h.onRuntimeEventLocked(meta, rt, runtimecontract.Event{Kind: runtimecontract.EventTerminal, TurnID: "turn-1", Outcome: &runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}})
		h.mu.Unlock()
		done <- struct{}{}
	}()
	close(start)
	<-done
	<-done
	h.mu.Lock()
	defer h.mu.Unlock()
	request := h.humanRequests["hrq-race"]
	marker := meta.TurnRecoveryMarkers["turn-1"]
	if proposalErr == nil {
		if request == nil || marker.HumanRequestID != request.ID || meta.LastTurn == nil || meta.LastTurn.Status != "interrupted" {
			t.Fatalf("successful proposal orphaned: request=%#v marker=%#v Agent=%#v", request, marker, meta)
		}
	} else if request != nil {
		t.Fatalf("failed proposal left orphan request=%#v marker=%#v", request, marker)
	}
}

func TestClaudeNeedsYouPersistenceFailureLeavesSourceTurnRunning(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	meta := &Agent{ID: "agent-1", Name: "claude", ThreadID: "thread-1", RuntimeBinding: RuntimeBinding{Kind: "claude"}, Status: "running"}
	rt := &runtime{agentID: meta.ID, activeTurn: &turnState{turnID: "turn-source", task: "work", startedAt: time.Now(), stopWatchdog: make(chan struct{})}}
	h.agents[meta.ID], h.runtimes[meta.ID] = meta, rt
	if err := os.Mkdir(filepath.Join(dir, "human-requests.ndjson"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := h.onRuntimeNeedsYouProposal(rt, runtimecontract.NeedsYouProposal{ID: "hrq_failed", TurnID: "turn-source", Question: "Continue?"}); err == nil {
		t.Fatal("Needs You unexpectedly persisted")
	}
	if rt.activeTurn == nil || rt.activeTurn.finished || meta.Status != "running" || len(h.humanRequestOrder) != 0 || len(meta.TurnRecoveryMarkers) != 0 {
		t.Fatalf("source=%#v meta=%#v requests=%#v", rt.activeTurn, meta, h.humanRequestOrder)
	}
}

func TestClaudeNeedsYouHumanRequestOnlyReopenRepairsWaitingMarker(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := now()
	agent := &Agent{
		ID: "agent-1", Name: "claude", ThreadID: "thread-1", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "claude", NativeRef: "11111111-1111-4111-8111-111111111111"},
		Status: "running", CurrentTurnID: "turn-source", CurrentTask: "ship release", CreatedAt: stamp, UpdatedAt: stamp,
	}
	if err := st.SaveAgents(map[string]*Agent{agent.ID: agent}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendHumanRequest(HumanRequest{
		ID: "hrq_exact", AgentID: agent.ID, AgentName: agent.Name, ThreadID: agent.ThreadID, SourceTurnID: "turn-source", SourceTask: "ship release",
		Question: "Ship now?", Context: "release is ready", BlockedWork: "ship release", Options: []HumanRequestOption{{Label: "Yes"}, {Label: "No"}},
		Expectation: HumanRequestRequired, State: "open", DeliveryStatus: "waiting", CreatedAt: stamp, UpdatedAt: stamp,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	h, err := Open(reopened)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown()
	request, err := h.GetHumanRequest("hrq_exact")
	if err != nil || request.Question != "Ship now?" || request.Context != "release is ready" || request.BlockedWork != "ship release" || len(request.Options) != 2 {
		t.Fatalf("restored request=%#v err=%v", request, err)
	}
	if len(h.humanRequestOrder) != 1 {
		t.Fatalf("Human Requests = %#v", h.humanRequestOrder)
	}
	repaired := h.agents[agent.ID]
	marker := repaired.TurnRecoveryMarkers["turn-source"]
	if repaired.LastTurn == nil || repaired.LastTurn.Status != "interrupted" || marker.HumanRequestID != request.ID || marker.Disposition != "needs_you" {
		t.Fatalf("repaired Agent=%#v marker=%#v", repaired, marker)
	}
}

func TestClaudeLedgerCommitFailureFencesHostAndCreatesOneNeedsYou(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "command-received")
	release := filepath.Join(t.TempDir(), "release")
	path := filepath.Join(t.TempDir(), "ledger-failure.sh")
	hello := fmt.Sprintf(`{"kind":"hello","protocolVersion":1,"bridgeBuild":"claude-bridge-v1","nodeVersion":"24.19.0","sdkVersion":"0.3.228","claudeCodeVersion":"2.1.228","os":%q,"arch":%q,"capabilities":["interrupt","approval","hooks","mcp","session_resume"]}`, gort.GOOS, gort.GOARCH)
	script := `#!/bin/sh
printf '%s\n' '` + hello + `'
IFS= read -r init
request_id=$(printf '%s' "$init" | sed -n 's/.*"requestId":"\([^"]*\)".*/\1/p')
printf '{"kind":"ready","requestId":"%s","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}\n' "$request_id"
IFS= read -r line
request_id=$(printf '%s' "$line" | sed -n 's/.*"requestId":"\([^"]*\)".*/\1/p')
operation=$(printf '%s' "$line" | sed -n 's/.*"operation":"\([^"]*\)".*/\1/p')
turn_id=$(printf '%s' "$line" | sed -n 's/.*"turnId":"\([^"]*\)".*/\1/p')
printf received > "$1"
while [ ! -f "$2" ]; do sleep 0.01; done
printf '{"kind":"event","class":"control","event":"turn_started","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-ledger-failure"}}\n' "$turn_id" "$operation"
printf '{"kind":"response","requestId":"%s","turnId":"%s","operation":"%s","accepted":true,"data":{"runtimeTurnRef":"native-ledger-failure"}}\n' "$request_id" "$turn_id" "$operation"
sleep 60
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	driver := newClaudeRuntimeHostDriver(st, claudebridge.DriverOptions{ResolveActive: func(context.Context) (claudebridge.LaunchSpec, error) {
		return claudebridge.LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Args: []string{marker, release}, Manifest: claudegen.CurrentManifest()}, nil
	}})
	h := New(st)
	h.runtimeHostDrivers["claude"] = driver
	agent, err := h.CreateAgent(CreateParams{Name: "claude-ledger-fence", Cwd: t.TempDir(), RuntimeKind: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := h.SendTask(agent.ID, "write once", time.Minute); result <- err }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("bridge did not receive StartTurn")
	}
	ledger := filepath.Join(st.Dir(), "canonical-turn-ledger.json")
	if err := os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ledger, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "indeterminate") {
		t.Fatalf("SendTask error = %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	var requests []HumanRequest
	for time.Now().Before(deadline) {
		requests, _ = h.ListHumanRequests(agent.ID, "all")
		view, _ := h.GetAgent(agent.ID)
		if len(requests) == 1 && view.Recovery != nil && !view.ProcessAlive {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	view, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.ProcessAlive || view.Recovery == nil || view.Recovery.FailureCode != "ledger_commit_indeterminate" || len(requests) != 1 {
		t.Fatalf("ledger fence view=%#v requests=%#v", view, requests)
	}
	h.Shutdown()
}

func TestClaudeCommittedTerminalSurvivesProcessExitBeforeResponse(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "terminal-before-response.sh")
	hello := fmt.Sprintf(`{"kind":"hello","protocolVersion":1,"bridgeBuild":"claude-bridge-v1","nodeVersion":"24.19.0","sdkVersion":"0.3.228","claudeCodeVersion":"2.1.228","os":%q,"arch":%q,"capabilities":["interrupt","approval","hooks","mcp","session_resume"]}`, gort.GOOS, gort.GOARCH)
	script := `#!/bin/sh
printf '%s\n' '` + hello + `'
IFS= read -r init
request_id=$(printf '%s' "$init" | sed -n 's/.*"requestId":"\([^"]*\)".*/\1/p')
printf '{"kind":"ready","requestId":"%s","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}\n' "$request_id"
IFS= read -r line
operation=$(printf '%s' "$line" | sed -n 's/.*"operation":"\([^"]*\)".*/\1/p')
turn_id=$(printf '%s' "$line" | sed -n 's/.*"turnId":"\([^"]*\)".*/\1/p')
printf '{"kind":"event","class":"control","event":"turn_started","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-terminal"}}\n' "$turn_id" "$operation"
printf '{"kind":"event","class":"control","event":"turn_completed","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-terminal"}}\n' "$turn_id" "$operation"
exit 70
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	driver := newClaudeRuntimeHostDriver(st, claudebridge.DriverOptions{ResolveActive: func(context.Context) (claudebridge.LaunchSpec, error) {
		return claudebridge.LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Manifest: claudegen.CurrentManifest()}, nil
	}})
	h := New(st)
	h.runtimeHostDrivers["claude"] = driver
	agent, err := h.CreateAgent(CreateParams{Name: "claude-terminal-response", Cwd: t.TempDir(), RuntimeKind: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.SendTask(agent.ID, "work", time.Minute)
	if err != nil {
		t.Fatalf("SendTask after committed terminal: %v", err)
	}
	history, err := st.LoadCanonicalTurnLedger(agent.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if history.Total != 1 || history.Turns[0].TurnID != result.TurnID || history.Turns[0].State != runtimecontract.LifecycleCompleted {
		t.Fatalf("committed terminal history = %#v", history)
	}
	time.Sleep(50 * time.Millisecond)
	view, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "idle" || view.LastError != "" || view.Recovery != nil || view.LastTurn == nil || view.LastTurn.Status != "completed" {
		t.Fatalf("committed terminal was resurrected as Runtime failure: %#v", view)
	}
	events, err := h.ReadEvents(agent.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "loom/runtime-failed" {
			t.Fatalf("committed terminal emitted false Runtime failure: %#v", event)
		}
	}
	h.mu.Lock()
	contract := h.runtimes[agent.ID].runtimeContract.(*claudeRuntimeContract)
	h.mu.Unlock()
	contract.mu.Lock()
	if len(contract.pendingOps) != 0 || len(contract.terminalOps) != 0 || len(contract.opPhases) != 0 || len(contract.opTurns) != 0 {
		contract.mu.Unlock()
		t.Fatalf("terminal-before-response retained operation metadata: pending=%#v terminals=%#v phases=%#v turns=%#v", contract.pendingOps, contract.terminalOps, contract.opPhases, contract.opTurns)
	}
	contract.mu.Unlock()
	h.Shutdown()
	_ = st.Close()
}

func TestClaudeTerminalForDifferentOperationDoesNotAcceptFailedCommand(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "terminal-different-operation.sh")
	hello := fmt.Sprintf(`{"kind":"hello","protocolVersion":1,"bridgeBuild":"claude-bridge-v1","nodeVersion":"24.19.0","sdkVersion":"0.3.228","claudeCodeVersion":"2.1.228","os":%q,"arch":%q,"capabilities":["interrupt","approval","hooks","mcp","session_resume"]}`, gort.GOOS, gort.GOARCH)
	script := `#!/bin/sh
printf '%s\n' '` + hello + `'
IFS= read -r init
request_id=$(printf '%s' "$init" | sed -n 's/.*"requestId":"\([^"]*\)".*/\1/p')
printf '{"kind":"ready","requestId":"%s","capabilities":["interrupt","approval","hooks","mcp","session_resume"]}\n' "$request_id"
IFS= read -r start
start_request=$(printf '%s' "$start" | sed -n 's/.*"requestId":"\([^"]*\)".*/\1/p')
start_op=$(printf '%s' "$start" | sed -n 's/.*"operation":"\([^"]*\)".*/\1/p')
turn_id=$(printf '%s' "$start" | sed -n 's/.*"turnId":"\([^"]*\)".*/\1/p')
printf '{"kind":"event","class":"control","event":"turn_started","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-exact"}}\n' "$turn_id" "$start_op"
printf '{"kind":"response","requestId":"%s","turnId":"%s","operation":"%s","accepted":true,"data":{"runtimeTurnRef":"native-exact"}}\n' "$start_request" "$turn_id" "$start_op"
IFS= read -r continue
printf '{"kind":"event","class":"control","event":"turn_completed","turnId":"%s","operation":"%s","data":{"runtimeTurnRef":"native-exact"}}\n' "$turn_id" "$start_op"
exit 70
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	driver := newClaudeRuntimeHostDriver(st, claudebridge.DriverOptions{ResolveActive: func(context.Context) (claudebridge.LaunchSpec, error) {
		return claudebridge.LaunchSpec{NodePath: "/bin/sh", BridgePath: path, Manifest: claudegen.CurrentManifest()}, nil
	}})
	t.Cleanup(func() { _ = driver.Shutdown(context.Background()); _ = st.Close() })
	host, err := driver.Acquire(context.Background(), AgentHostRequest{AgentID: "agent-exact-operation", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	contract := host.Contract()
	binding := runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "claude", NativeRef: "11111111-1111-4111-8111-111111111111"}
	started := contract.StartTurn(context.Background(), runtimecontract.TurnRequest{Binding: binding, TurnID: "turn-exact", Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: "work"}}})
	if started.State != runtimecontract.LifecycleAccepted {
		t.Fatalf("Start = %#v", started)
	}
	continued := contract.ContinueTurn(context.Background(), runtimecontract.CausalInput{Binding: binding, TurnID: "turn-exact", RuntimeTurnRef: started.RuntimeTurnRef, Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: "more"}}})
	if continued.State != runtimecontract.LifecycleIndeterminate || continued.Failure == nil || continued.Failure.Phase != runtimecontract.FailurePhaseTurnContinue {
		t.Fatalf("different-operation terminal accepted failed Continue = %#v", continued)
	}
	claude := contract.(*claudeRuntimeContract)
	claude.mu.Lock()
	defer claude.mu.Unlock()
	if len(claude.opPhases) != 0 || len(claude.opTurns) != 0 || len(claude.pendingOps) != 0 || len(claude.terminalOps) != 0 {
		t.Fatalf("settled Turn retained operation metadata: phases=%#v turns=%#v pending=%#v terminals=%#v", claude.opPhases, claude.opTurns, claude.pendingOps, claude.terminalOps)
	}
}
