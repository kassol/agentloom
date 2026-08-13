package hub

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/codex"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func contractBinding(nativeRef string) runtimecontract.Binding {
	return runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "codex", NativeRef: nativeRef}
}

func TestCodexHostDriverAcquiresDistinctAgentContractsOnOneSharedHost(t *testing.T) {
	logPath := installFakeSharedCodexHost(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	driver := newCodexRuntimeHostDriver(h)
	ctx := context.Background()
	if err := driver.Preflight(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := driver.Acquire(ctx, AgentHostRequest{AgentID: "agent-1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := driver.Acquire(ctx, AgentHostRequest{AgentID: "agent-2"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first.Contract() == second.Contract() {
		t.Fatal("two Agents shared one AgentHost or Runtime Contract handler")
	}
	if !first.Alive() || !second.Alive() {
		t.Fatalf("acquired handles are not alive: first=%v second=%v", first.Alive(), second.Alive())
	}
	if got := countRequestMethod(t, logPath, "initialize"); got != 1 {
		t.Fatalf("initialize requests = %d, want one shared Codex host", got)
	}
	first.Close()
	if first.Alive() || !second.Alive() {
		t.Fatalf("per-Agent close affected shared host: first=%v second=%v", first.Alive(), second.Alive())
	}
	if err := driver.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := driver.Shutdown(ctx); err != nil {
		t.Fatalf("second shutdown was not idempotent: %v", err)
	}
	if second.Alive() {
		t.Fatal("shared Host remained alive after shutdown")
	}
}

func TestCodexHostDriverPreflightRequiresReadableVersion(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n[ \"$1\" = \"--version\" ] && exit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_REMOTE_BIN", binPath)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = newCodexRuntimeHostDriver(testHub(st)).Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("Preflight error = %v, want unreadable version", err)
	}
}

func TestCodexAgentReopensSameBindingWithoutPublicNativeIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PINIX_EDGE_NAMES", filepath.Join(t.TempDir(), "missing.json"))
	installFakeSharedCodexHost(t)
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	agent, err := h.CreateAgent(CreateParams{Name: "one", Cwd: "/tmp/one", RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := h.GetRuntimeDiagnostics(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.agents[agent.ID].RuntimeTurnBindings = map[string]string{"turn-loom-before-restart": "turn-native-before-restart"}
	if err := h.persistAgentsLocked(); err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	h.mu.Unlock()
	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	logPath := installFakeSharedCodexHost(t)
	st, err = store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h, err = Open(st)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown()
	reopened, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	rt, err := h.getRuntimeLocked(h.agents[agent.ID])
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(rt); err != nil {
		t.Fatal(err)
	}
	if reopened.ID != agent.ID || reopened.ThreadID != agent.ThreadID || reopened.RuntimeBinding.NativeRef != "" {
		t.Fatalf("reopened public Agent = %#v", reopened.Agent)
	}
	encoded, _ := json.Marshal(reopened)
	if strings.Contains(string(encoded), diagnostics.NativeRef) {
		t.Fatalf("public Agent leaked native ref: %s", encoded)
	}
	after, err := h.GetRuntimeDiagnostics(agent.ID)
	if err != nil || after.NativeRef != diagnostics.NativeRef {
		t.Fatalf("reopened diagnostics = %#v, err=%v; want native ref %q", after, err, diagnostics.NativeRef)
	}
	if got := countRequestMethod(t, logPath, "thread/resume"); got != 1 {
		t.Fatalf("restart resume requests = %d, want one", got)
	}
	h.onHostNotification(rt.hostGeneration, "item/agentMessage/delta", json.RawMessage(`{"threadId":"thr-one","turnId":"turn-native-before-restart","itemId":"answer-after-restart","delta":"still mapped"}`))
	events, err := st.ReadEvents(agent.ID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	foundMappedDelta := false
	for _, event := range events {
		if event.Type != "loom/runtime-event" {
			continue
		}
		var payload struct {
			Kind    runtimecontract.EventKind     `json:"kind"`
			TurnID  string                        `json:"turnId"`
			Content *runtimecontract.ContentBlock `json:"content"`
		}
		_ = json.Unmarshal(event.Data, &payload)
		foundMappedDelta = payload.Kind == runtimecontract.EventContent && payload.TurnID == "turn-loom-before-restart" && payload.Content != nil && payload.Content.Text == "still mapped"
	}
	if !foundMappedDelta {
		t.Fatalf("restart canonical mapping missing from events: %#v", events)
	}
}

func TestCodexSharedHostFailureInterruptsEachActiveTurnOnce(t *testing.T) {
	installFakeSharedCodexHost(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	defer h.Shutdown()
	agent, err := h.CreateAgent(CreateParams{Name: "one", Cwd: "/tmp/one", RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	rt := h.runtimes[agent.ID]
	turn := &turnState{turnID: "turn-loom", nativeTurnID: "native-turn", task: "work", startedAt: time.Now(), stopWatchdog: make(chan struct{})}
	rt.activeTurn = turn
	h.agents[agent.ID].Status = "running"
	h.agents[agent.ID].CurrentTurnID = turn.turnID
	h.agents[agent.ID].CurrentTask = turn.task
	generation := rt.hostGeneration
	driver := h.codexHostDriver
	h.mu.Unlock()
	driver.fanoutHostFailure(generation, errors.New("shared host exited"))
	driver.fanoutHostFailure(generation, errors.New("duplicate callback"))
	view, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.LastTurn == nil || view.LastTurn.TurnID != turn.turnID || view.LastTurn.Status != "interrupted" {
		t.Fatalf("terminal Agent projection = %#v", view.Agent)
	}
	events, err := st.ReadEvents(agent.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	terminal := 0
	for _, event := range events {
		if event.Type == "loom/turn-interrupted" {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("terminal event count = %d, events = %#v", terminal, events)
	}
}

func TestCodexArchiveClosesOnlyOneAgentHandleAndHubShutdownUsesDriverOnce(t *testing.T) {
	installFakeSharedCodexHost(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	first, err := h.CreateAgent(CreateParams{Name: "one", Cwd: "/tmp/one", RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.CreateAgent(CreateParams{Name: "two", Cwd: "/tmp/two", RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	firstHandle := h.runtimes[first.ID].agentHost
	secondHandle := h.runtimes[second.ID].agentHost
	driver := h.codexHostDriver
	h.mu.Unlock()
	if _, err := h.ArchiveAgent(first.ID); err != nil {
		t.Fatal(err)
	}
	if firstHandle.Alive() || !secondHandle.Alive() {
		t.Fatalf("archive handle state: first=%v second=%v", firstHandle.Alive(), secondHandle.Alive())
	}
	h.Shutdown()
	h.Shutdown()
	driver.mu.Lock()
	shutdown := driver.shutdown
	driver.mu.Unlock()
	if !shutdown || secondHandle.Alive() {
		t.Fatalf("Hub shutdown did not close Driver once: shutdown=%v second=%v", shutdown, secondHandle.Alive())
	}
}

func TestCodexHostDriverRoutesEventsAndFailuresToEachAgentHandleOnce(t *testing.T) {
	installFakeSharedCodexHost(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	driver := newCodexRuntimeHostDriver(h)
	ctx := context.Background()
	first, err := driver.Acquire(ctx, AgentHostRequest{AgentID: "agent-1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := driver.Acquire(ctx, AgentHostRequest{AgentID: "agent-2"})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.codexHostDriver = driver
	firstAgent := &Agent{ID: "agent-1", Name: "one", ThreadID: "loom-a", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "native-a"}, Status: "idle"}
	secondAgent := &Agent{ID: "agent-2", Name: "two", ThreadID: "loom-b", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "native-b"}, Status: "idle"}
	h.agents["agent-1"], h.agents["agent-2"] = firstAgent, secondAgent
	firstHandle, secondHandle := first.(*codexAgentHost), second.(*codexAgentHost)
	ready := make(chan struct{})
	close(ready)
	firstRuntime := &runtime{agentID: "agent-1", agentHost: first, runtimeContract: first.Contract(), hostGeneration: firstHandle.host.generation, ready: ready, approvals: map[string]*approval{}}
	secondRuntime := &runtime{agentID: "agent-2", agentHost: second, runtimeContract: second.Contract(), hostGeneration: secondHandle.host.generation, ready: ready, approvals: map[string]*approval{}}
	h.bindCodexContract(firstAgent, firstRuntime, firstHandle.contract)
	h.bindCodexContract(secondAgent, secondRuntime, secondHandle.contract)
	first.SetFailureHandler(func(err error) { h.onCodexHostFailure(firstRuntime, err) })
	second.SetFailureHandler(func(err error) { h.onCodexHostFailure(secondRuntime, err) })
	h.runtimes["agent-1"], h.runtimes["agent-2"] = firstRuntime, secondRuntime
	generation := firstHandle.host.generation
	h.mu.Unlock()
	h.onHostNotification(generation, "turn/started", json.RawMessage(`{"threadId":"native-a","turn":{"id":"native-turn-a"}}`))
	h.mu.Lock()
	if firstRuntime.activeTurn == nil || firstRuntime.activeTurn.nativeTurnID != "native-turn-a" || secondRuntime.activeTurn != nil {
		h.mu.Unlock()
		t.Fatalf("production event routing = first %#v, second %#v", firstRuntime.activeTurn, secondRuntime.activeTurn)
	}
	secondTurn := &turnState{turnID: "turn-loom-b", nativeTurnID: "native-turn-b", startedConfirmed: true, startedAt: time.Now(), stopWatchdog: make(chan struct{})}
	secondRuntime.activeTurn = secondTurn
	secondAgent.Status, secondAgent.CurrentTurnID = "running", secondTurn.turnID
	secondHandle.contract.bindTurn(secondTurn.turnID, "", secondTurn.nativeTurnID)
	h.mu.Unlock()
	driver.fanoutHostFailure(generation, errors.New("shared host exited"))
	driver.fanoutHostFailure(generation, errors.New("duplicate close callback"))
	for _, agentID := range []string{"agent-1", "agent-2"} {
		view, err := h.GetAgent(agentID)
		if err != nil || view.LastTurn == nil || view.LastTurn.Status != "interrupted" {
			t.Fatalf("failure projection for %s = %#v, err=%v", agentID, view.Agent, err)
		}
		events, err := st.ReadEvents(agentID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		terminals := 0
		for _, event := range events {
			if event.Type == "loom/turn-interrupted" {
				terminals++
			}
		}
		if terminals != 1 {
			t.Fatalf("terminal count for %s = %d", agentID, terminals)
		}
	}
	// Host failure recovery is finite Hub-owned work. Shut the Hub down so the
	// recovery workers finish before TempDir removes their Store.
	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexRuntimeContractV2ExecutesCoreLifecycle(t *testing.T) {
	installFakeSharedCodexHost(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	driver := newCodexRuntimeHostDriver(h)
	ctx := context.Background()
	handle, err := driver.Acquire(ctx, AgentHostRequest{AgentID: "agent-1"})
	if err != nil {
		t.Fatal(err)
	}
	contract := handle.Contract()
	other, err := driver.Acquire(ctx, AgentHostRequest{AgentID: "agent-2"})
	if err != nil {
		t.Fatal(err)
	}
	binding, outcome := contract.CreateBinding(ctx, runtimecontract.BindingRequest{AgentID: "agent-1", Name: "worker", Cwd: "/tmp/one"})
	if outcome.State != runtimecontract.LifecycleAccepted || binding.NativeRef != "thr-one" {
		t.Fatalf("create binding = %#v, outcome = %#v", binding, outcome)
	}
	binding.NativeRef = "thr-stale"
	if outcome := contract.ResumeBinding(ctx, binding); outcome.State != runtimecontract.LifecycleCompleted {
		t.Fatalf("resume outcome = %#v", outcome)
	}
	started := contract.StartTurn(ctx, runtimecontract.TurnRequest{
		Binding: binding, TurnID: "turn-loom", Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: "hello"}},
	})
	if started.State != runtimecontract.LifecycleAccepted || started.RuntimeTurnRef != "turn-stale" {
		t.Fatalf("start outcome = %#v", started)
	}
	continued := contract.ContinueTurn(ctx, runtimecontract.CausalInput{
		Binding: binding, TurnID: "turn-loom", RuntimeTurnRef: started.RuntimeTurnRef,
		Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: "more"}},
	})
	if continued.State != runtimecontract.LifecycleAccepted || continued.RuntimeTurnRef != started.RuntimeTurnRef {
		t.Fatalf("continue outcome = %#v", continued)
	}
	interrupted := contract.InterruptTurn(ctx, runtimecontract.TurnTarget{
		Binding: binding, TurnID: "turn-loom", RuntimeTurnRef: started.RuntimeTurnRef,
	})
	if interrupted.State != runtimecontract.LifecycleInterrupted {
		t.Fatalf("interrupt outcome = %#v", interrupted)
	}
	eventCount, failureCount := 0, 0
	contract.SetEventHandler(func(runtimecontract.Event) { eventCount++ })
	handle.SetFailureHandler(func(error) { failureCount++ })
	if outcome := contract.CloseBinding(ctx, binding); outcome.State != runtimecontract.LifecycleCompleted || handle.Alive() || !other.Alive() {
		t.Fatalf("per-binding close = %#v, handle alive = %v, other alive = %v", outcome, handle.Alive(), other.Alive())
	}
	driver.dispatchNativeEvent("agent-1", "turn/started", json.RawMessage(`{"threadId":"thr-stale","turn":{"id":"after-close"}}`))
	driver.fanoutHostFailure(handle.(*codexAgentHost).host.generation, errors.New("after close"))
	if eventCount != 0 || failureCount != 0 || !other.Alive() {
		t.Fatalf("closed handle deliveries = events %d failures %d other alive %v", eventCount, failureCount, other.Alive())
	}
	if err := driver.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCodexV2MutatingBindingAndContextTransportLossIsIndeterminate(t *testing.T) {
	for _, phase := range []runtimecontract.FailurePhase{
		runtimecontract.FailurePhaseBindingCreate,
		runtimecontract.FailurePhaseBindingResume,
		runtimecontract.FailurePhaseContextDelivery,
	} {
		for name, transportErr := range map[string]error{
			"timeout": &codex.RequestTimeoutError{Method: string(phase), Timeout: time.Millisecond},
			"closed":  codex.ErrClosed,
		} {
			t.Run(string(phase)+"/"+name, func(t *testing.T) {
				outcome := codexFailureOutcome(transportErr, phase)
				if outcome.State != runtimecontract.LifecycleIndeterminate || outcome.Failure == nil || outcome.Failure.Phase != phase {
					t.Fatalf("outcome = %#v", outcome)
				}
				if err := outcome.Validate(); err != nil {
					t.Fatalf("Validate: %v", err)
				}
			})
		}
	}
}

func TestCodexContractRejectsInterruptWithoutCanonicalTurnID(t *testing.T) {
	contract := &codexRuntimeContract{native: &codexAgentRuntime{}}
	outcome := contract.InterruptTurn(context.Background(), runtimecontract.TurnTarget{
		Binding: contractBinding("thread-native"), RuntimeTurnRef: "turn-native",
	})
	if outcome.State != runtimecontract.LifecycleRejected || outcome.Failure == nil || outcome.Failure.Code != "missing_turn_id" {
		t.Fatalf("interrupt without Loom TurnID = %#v", outcome)
	}
}

func TestCodexRuntimeContractCorrelatesEventBeforeStartResponse(t *testing.T) {
	installFakeSharedCodexHost(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	driver := newCodexRuntimeHostDriver(h)
	h.codexHostDriver = driver
	handle, err := driver.Acquire(context.Background(), AgentHostRequest{AgentID: "agent-early"})
	if err != nil {
		t.Fatal(err)
	}
	contract := handle.Contract()
	events := make(chan runtimecontract.Event, 2)
	contract.SetEventHandler(func(event runtimecontract.Event) { events <- event })
	h.mu.Lock()
	h.agents["agent-early"] = &Agent{ID: "agent-early", Name: "early", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thr-stale"}, Status: "running", CurrentTurnID: "turn-loom-early"}
	h.runtimes["agent-early"] = &runtime{
		agentID: "agent-early", agentHost: handle, runtimeContract: contract, hostGeneration: handle.(*codexAgentHost).host.generation,
		activeTurn: &turnState{turnID: "turn-loom-early", startedAt: time.Now(), stopWatchdog: make(chan struct{})},
	}
	h.mu.Unlock()

	binding := contractBinding("thr-stale")
	if outcome := contract.ResumeBinding(context.Background(), binding); outcome.State != runtimecontract.LifecycleCompleted {
		t.Fatalf("ResumeBinding = %#v", outcome)
	}
	outcome := contract.StartTurn(context.Background(), runtimecontract.TurnRequest{
		Binding: binding, TurnID: "turn-loom-early", Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: "early-event"}},
	})
	if outcome.State != runtimecontract.LifecycleAccepted || outcome.RuntimeTurnRef != "turn-native-early" {
		t.Fatalf("StartTurn = %#v", outcome)
	}
	for index := 0; index < 2; index++ {
		select {
		case event := <-events:
			if event.TurnID != "turn-loom-early" || event.RuntimeTurnRef != "turn-native-early" {
				t.Fatalf("correlated event = %#v", event)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for early canonical event")
		}
	}
	if err := driver.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCodexHubConsumesCanonicalContractEventWithoutRawNormalization(t *testing.T) {
	installFakeSharedCodexHost(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	defer h.Shutdown()
	agent, err := h.CreateAgent(CreateParams{Name: "canonical", Cwd: "/tmp/one", RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	rt := h.runtimes[agent.ID]
	turn := &turnState{turnID: "turn-loom-canonical", nativeTurnID: "turn-native-canonical", startedConfirmed: true, startedAt: time.Now(), stopWatchdog: make(chan struct{})}
	rt.activeTurn = turn
	h.agents[agent.ID].Status = "running"
	h.agents[agent.ID].CurrentTurnID = turn.turnID
	h.agents[agent.ID].RuntimeTurnBindings = map[string]string{turn.turnID: turn.nativeTurnID}
	rt.runtimeContract.(*codexRuntimeContract).bindTurn(turn.turnID, "", turn.nativeTurnID)
	generation := rt.hostGeneration
	h.mu.Unlock()

	h.onHostNotification(generation, "turn/completed", json.RawMessage(`{"threadId":"thr-one","turn":{"id":"turn-native-canonical","status":"completed"}}`))
	view, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "idle" || view.LastTurn == nil || view.LastTurn.TurnID != turn.turnID || view.LastTurn.Status != "completed" {
		t.Fatalf("canonical terminal projection = %#v", view.Agent)
	}
	events, err := st.ReadEvents(agent.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	rawCount := 0
	for _, event := range events {
		if event.Type == "turn/completed" {
			rawCount++
		}
	}
	if rawCount != 0 {
		t.Fatalf("canonical stream retained raw Runtime event: %#v", events)
	}
}

func TestCodexContractPreservesStreamingAndToolLifecycle(t *testing.T) {
	contract := &codexRuntimeContract{native: &codexAgentRuntime{}}
	contract.bindTurn("turn-loom", "turn-before", "turn-native")
	var events []runtimecontract.Event
	contract.SetEventHandler(func(event runtimecontract.Event) { events = append(events, event) })
	fixtures := []struct {
		method string
		params string
	}{
		{"item/agentMessage/delta", `{"threadId":"thread-native","turnId":"turn-native","itemId":"answer","delta":"hi"}`},
		{"item/completed", `{"threadId":"thread-native","turnId":"turn-native","item":{"id":"answer","type":"agentMessage","content":[{"type":"output_text","text":"done"}]}}`},
		{"item/completed", `{"threadId":"thread-native","turnId":"turn-native","item":{"id":"reasoning","type":"reasoning","summary":[{"type":"summary_text","text":"checked"}]}}`},
		{"item/started", `{"threadId":"thread-native","turnId":"turn-native","item":{"id":"tool-1","type":"commandExecution","command":"pwd","description":"Locate the workspace root"}}`},
		{"item/updated", `{"threadId":"thread-native","turnId":"turn-native","item":{"id":"tool-1","type":"commandExecution","command":"pwd","description":"Locate the workspace root","aggregatedOutput":"/repo"}}`},
		{"item/completed", `{"threadId":"thread-native","turnId":"turn-native","item":{"id":"tool-1","type":"commandExecution","command":"pwd","description":"Locate the workspace root","output":"/repo\n","status":"completed","exitCode":0,"durationMs":12}}`},
	}
	for _, fixture := range fixtures {
		contract.handleNativeEvent(fixture.method, json.RawMessage(fixture.params))
	}
	if len(events) != 6 {
		t.Fatalf("canonical events = %#v", events)
	}
	wantPhases := []runtimecontract.ContentPhase{
		runtimecontract.ContentPhaseDelta, runtimecontract.ContentPhaseCompleted, runtimecontract.ContentPhaseCompleted,
		runtimecontract.ContentPhaseStarted, runtimecontract.ContentPhaseUpdated, runtimecontract.ContentPhaseCompleted,
	}
	for index, event := range events {
		if event.TurnID != "turn-loom" || event.PredecessorTurnID != "turn-before" || event.ContentPhase != wantPhases[index] {
			t.Fatalf("event %d = %#v", index, event)
		}
	}
	if events[1].Content == nil || events[1].Content.Kind != runtimecontract.ContentAssistantText ||
		events[2].Content == nil || events[2].Content.Kind != runtimecontract.ContentReasoning {
		t.Fatalf("completed text/reasoning content was lost: %#v", events)
	}
	if events[0].Content == nil || events[0].Content.Kind != runtimecontract.ContentAssistantText || events[0].Content.Text != "hi" {
		t.Fatalf("assistant delta = %#v", events[0])
	}
	if events[3].Content == nil || events[3].Content.Kind != runtimecontract.ContentToolCall || events[3].Content.ToolCall == nil || events[3].Content.ToolCall.Name != "commandExecution" || events[3].Content.ToolCall.Description != "Locate the workspace root" {
		t.Fatalf("tool call = %#v", events[3])
	}
	if events[5].Content == nil || events[5].Content.Kind != runtimecontract.ContentToolResult || events[5].Content.ToolResult == nil ||
		events[5].Content.ToolResult.ToolCallID != "tool-1" || events[5].Content.ToolResult.Text != "/repo\n" || !events[5].Content.ToolResult.Success {
		t.Fatalf("tool result = %#v", events[5])
	}
	completedItem := map[string]any{}
	if err := json.Unmarshal(events[5].Content.Diagnostic, &completedItem); err != nil {
		t.Fatalf("decode adapter diagnostic: %v", err)
	}
	for key, want := range map[string]any{"type": "commandExecution", "command": "pwd", "output": "/repo\n", "status": "completed", "exitCode": float64(0), "durationMs": float64(12)} {
		if completedItem[key] != want {
			t.Fatalf("completed tool item[%s] = %#v, want %#v; item=%#v", key, completedItem[key], want, completedItem)
		}
	}
}

func TestCodexRuntimeContractHistoryIsLazyAndCanonical(t *testing.T) {
	sessions := t.TempDir()
	threadID := "native-thread-lazy"
	day := filepath.Join(sessions, "2026", "08", "10")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(day, "rollout-2026-08-10T00-00-00-"+threadID+".jsonl")
	data := `{"timestamp":"2026-08-10T00:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"native-turn"}}
{"timestamp":"2026-08-10T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"inspect"}}
{"timestamp":"2026-08-10T00:00:01.500Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call-1","arguments":"{\"cmd\":\"pwd\",\"workdir\":\"/repo\"}"}}
{"timestamp":"2026-08-10T00:00:01.750Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"/repo\nProcess exited with code 0\n"}}
{"timestamp":"2026-08-10T00:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"done"}}
{"timestamp":"2026-08-10T00:00:03Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"native-turn"}}
`
	if err := os.WriteFile(rolloutPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SESSIONS_DIR", sessions)
	contract := &codexRuntimeContract{native: &codexAgentRuntime{}}
	contract.bindTurn("turn-loom-history", "", "native-turn")
	history, failure := contract.ReadHistory(context.Background(), runtimecontract.HistoryRequest{
		Binding: runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "codex", NativeRef: threadID}, Count: 10,
	})
	if failure != nil || history.Total != 1 || len(history.Turns) != 1 {
		t.Fatalf("history = %#v, failure = %#v", history, failure)
	}
	turn := history.Turns[0]
	if turn.RuntimeTurnRef != "native-turn" || turn.TurnID != "turn-loom-history" || turn.State != runtimecontract.LifecycleCompleted || len(turn.Content) < 2 {
		t.Fatalf("canonical history Turn = %#v", turn)
	}
	publicHistory, _ := json.Marshal(history)
	if strings.Contains(string(publicHistory), "native-turn") {
		t.Fatalf("canonical history leaked Runtime Turn ref: %s", publicHistory)
	}
	var call, result *runtimecontract.ContentBlock
	for index := range turn.Content {
		switch turn.Content[index].Kind {
		case runtimecontract.ContentToolCall:
			call = &turn.Content[index]
		case runtimecontract.ContentToolResult:
			result = &turn.Content[index]
		}
	}
	if call == nil || call.ToolCall == nil || call.ToolCall.Name != "exec_command" || result == nil || result.ToolResult == nil ||
		result.ToolResult.ToolCallID != call.ID || result.ToolResult.Text == "" || !result.ToolResult.Success {
		t.Fatalf("canonical command history = %#v", turn.Content)
	}
}

func TestCodexInspectorTreatsRealRolloutUnpairedFunctionCallAsAmbiguous(t *testing.T) {
	sessions := t.TempDir()
	threadID := "native-thread-interrupted"
	day := filepath.Join(sessions, "2026", "08", "10")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(day, "rollout-2026-08-10T00-00-00-"+threadID+".jsonl")
	data := `{"timestamp":"2026-08-10T00:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"native-turn"}}
{"timestamp":"2026-08-10T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"deploy"}}
{"timestamp":"2026-08-10T00:00:02Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"call-unfinished","arguments":"{\"cmd\":\"deploy\"}"}}
`
	if err := os.WriteFile(rolloutPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SESSIONS_DIR", sessions)
	contract := &codexRuntimeContract{native: &codexAgentRuntime{}}
	evidence, err := contract.InspectInterruptedTurn(context.Background(), runtimecontract.TurnTarget{
		Binding: contractBinding(threadID), TurnID: "turn-loom", RuntimeTurnRef: "native-turn",
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Status != RuntimeInterruptionAmbiguous || len(evidence.UnfinishedTools) != 1 {
		t.Fatalf("real rollout interruption evidence=%#v", evidence)
	}
}
