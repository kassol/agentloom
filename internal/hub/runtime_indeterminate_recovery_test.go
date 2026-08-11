package hub

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestInterruptedRecoveryCheckpointFailurePublishesNothing(t *testing.T) {
	dir := t.TempDir()
	writable, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := store.OpenWithOptions(dir, store.OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	h := testHub(readOnly)
	turn := &turnState{
		turnID: "turn-uncertain", task: "publish once", startedAt: time.Now(), lastActivity: time.Now(),
		stopWatchdog: make(chan struct{}),
	}
	meta := &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "thr-1", RuntimeBinding: RuntimeBinding{Kind: "codex"},
		Status: "running", CurrentTurnID: turn.turnID, CurrentTask: turn.task, CreatedAt: now(), UpdatedAt: now(),
	}
	fake := &fakeAgentRuntime{binding: "native-thread"}
	rt := &runtime{agentID: meta.ID, agentRuntime: fake, activeTurn: turn, approvals: map[string]*approval{}}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = rt

	h.onRuntimeIndeterminate(rt, &runtimeIndeterminateError{failure: &runtimecontract.Failure{
		Code: "transport_timeout", Phase: runtimecontract.FailurePhaseTurnStart,
		Message: "timeout", Cause: errors.New("transport timeout"),
	}})

	view, err := h.GetAgent(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "running" || view.LastTurn != nil || view.Recovery != nil {
		t.Fatalf("failed checkpoint public view = %#v", view)
	}
	if rt.activeTurn != turn || turn.finished || !fake.closed.Load() || !rt.effectDomainInvalidated {
		t.Fatalf("failed checkpoint did not restore state and fence backend: active=%#v finished=%v closed=%v fenced=%v", rt.activeTurn, turn.finished, fake.closed.Load(), rt.effectDomainInvalidated)
	}
	if h.seqs[meta.ID] != 0 || len(h.turnRecoveryInFlight) != 0 || len(h.humanRequests) != 0 {
		t.Fatalf("failed checkpoint published recovery: seq=%d inFlight=%v requests=%v", h.seqs[meta.ID], h.turnRecoveryInFlight, h.humanRequests)
	}
	for _, event := range []RuntimeEvent{
		{Kind: RuntimeTurnStarted, LoomTurnID: turn.turnID, NativeTurnID: "native-late"},
		{Kind: RuntimeTextDelta, LoomTurnID: turn.turnID, NativeTurnID: "native-late", Text: "late"},
		{Kind: RuntimeTurnCompleted, LoomTurnID: turn.turnID, NativeTurnID: "native-late"},
	} {
		h.onRuntimeEvent(rt, event)
	}
	if rt.activeTurn != turn || turn.finished || meta.Status != "running" || h.seqs[meta.ID] != 0 {
		t.Fatalf("late events polluted failed checkpoint snapshot: Agent=%#v active=%#v seq=%d", meta, rt.activeTurn, h.seqs[meta.ID])
	}
}

func TestStoreReopenConvergesObservedRecoveryMarkerOnce(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := now()
	if err := st.SaveAgents(map[string]*Agent{
		"agent-1": {
			ID: "agent-1", Name: "worker", Cwd: t.TempDir(), ThreadID: "thr-1",
			RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "native-thread"}, Status: "interrupted",
			LastTurn: &TurnSummary{TurnID: "turn-before", Task: "publish once", Status: "interrupted", CompletedAt: stamp},
			TurnRecoveryMarkers: map[string]TurnRecoveryMarker{
				"turn-before": {PredecessorTurnID: "turn-before", RuntimeKind: "codex", Cause: "process_exit", State: TurnRecoveryObserved, Summary: "Runtime process exited before the Turn outcome was confirmed", CreatedAt: stamp, UpdatedAt: stamp},
			},
			CreatedAt: stamp, UpdatedAt: stamp,
		},
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

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		requests, _ := h.ListHumanRequests("agent-1", "all")
		h.mu.Lock()
		state := h.agents["agent-1"].TurnRecoveryMarkers["turn-before"].State
		h.mu.Unlock()
		if len(requests) == 1 && state == TurnRecoveryDispatched {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	requests, err := h.ListHumanRequests("agent-1", "all")
	if err != nil || len(requests) != 1 {
		t.Fatalf("observed recovery requests=%#v err=%v", requests, err)
	}
	h.mu.Lock()
	marker := h.agents["agent-1"].TurnRecoveryMarkers["turn-before"]
	h.mu.Unlock()
	if marker.State != TurnRecoveryDispatched || marker.HumanRequestID != requests[0].ID {
		t.Fatalf("observed recovery marker=%#v request=%#v", marker, requests[0])
	}
}

func TestLateCanonicalEventsCannotSupersedeOrRenderOnSuccessor(t *testing.T) {
	for _, event := range []RuntimeEvent{
		{Kind: RuntimeTurnStarted, LoomTurnID: "turn-old", NativeTurnID: "native-old"},
		{Kind: RuntimeTextDelta, LoomTurnID: "turn-old", NativeTurnID: "native-old", Text: "stale"},
		{Kind: RuntimeTurnCompleted, LoomTurnID: "turn-old", NativeTurnID: "native-old"},
	} {
		t.Run(string(event.Kind), func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			h := testHub(st)
			turn := &turnState{
				turnID: "turn-successor", nativeTurnID: "native-successor", startedConfirmed: true,
				task: "new work", startedAt: time.Now(), lastActivity: time.Now(), stopWatchdog: make(chan struct{}),
			}
			meta := &Agent{
				ID: "agent-1", Name: "worker", ThreadID: "thr-1", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thread-native"},
				Status: "running", CurrentTask: turn.task, CurrentTurnID: turn.turnID, CreatedAt: now(), UpdatedAt: now(),
			}
			rt := &runtime{agentID: meta.ID, activeTurn: turn, approvals: map[string]*approval{}}
			h.agents[meta.ID] = meta
			h.runtimes[meta.ID] = rt
			before := h.seqs[meta.ID]

			h.onRuntimeEvent(rt, event)

			if rt.activeTurn != turn || turn.finished || meta.CurrentTurnID != turn.turnID || meta.Status != "running" {
				t.Fatalf("late event changed successor: event=%#v Agent=%#v active=%#v", event, meta, rt.activeTurn)
			}
			if h.seqs[meta.ID] != before {
				t.Fatalf("late event rendered publicly: seq before=%d after=%d", before, h.seqs[meta.ID])
			}
		})
	}
}

func TestLateCodexCompatibilityEventDoesNotRenderOnSuccessor(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	turn := &turnState{
		turnID: "turn-successor", nativeTurnID: "native-successor", startedConfirmed: true,
		task: "new work", startedAt: time.Now(), lastActivity: time.Now(), stopWatchdog: make(chan struct{}),
	}
	meta := &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "thr-1", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thread-native"},
		Status: "running", CurrentTask: turn.task, CurrentTurnID: turn.turnID, CreatedAt: now(), UpdatedAt: now(),
	}
	contract := &codexRuntimeContract{turnsByNative: map[string]runtimeTurnCorrelation{}}
	contract.bindTurn("turn-old", "", "native-old")
	rt := &runtime{agentID: meta.ID, activeTurn: turn, runtimeContract: contract, approvals: map[string]*approval{}}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = rt
	before := h.seqs[meta.ID]

	h.onCodexCompatibilityNotification(rt, "item/agentMessage/delta", json.RawMessage(`{"threadId":"thread-native","turnId":"native-old","delta":"stale"}`), true)

	if h.seqs[meta.ID] != before || rt.activeTurn != turn || turn.finished {
		t.Fatalf("late compatibility event changed successor: seq=%d active=%#v", h.seqs[meta.ID], rt.activeTurn)
	}
}

func TestObservedRecoveryFencesAllRuntimeEventsWithoutActiveTurn(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	stamp := now()
	meta := &Agent{
		ID: "agent-1", Name: "worker", RuntimeBinding: RuntimeBinding{Kind: "pi"}, Status: "interrupted",
		LastTurn: &TurnSummary{TurnID: "turn-old", Status: "interrupted"},
		TurnRecoveryMarkers: map[string]TurnRecoveryMarker{
			"turn-old": {PredecessorTurnID: "turn-old", RuntimeKind: "pi", Cause: "process_exit", State: TurnRecoveryObserved, CreatedAt: stamp, UpdatedAt: stamp},
		},
	}
	rt := &runtime{agentID: meta.ID, approvals: map[string]*approval{}}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = rt

	for _, event := range []RuntimeEvent{
		{Kind: RuntimeTurnStarted, LoomTurnID: "turn-old", NativeTurnID: "native-old"},
		{Kind: RuntimeTextDelta, LoomTurnID: "turn-old", NativeTurnID: "native-old", Text: "stale"},
		{Kind: RuntimeTurnCompleted, LoomTurnID: "turn-old", NativeTurnID: "native-old"},
	} {
		h.onRuntimeEvent(rt, event)
	}
	if rt.activeTurn != nil || meta.Status != "interrupted" || h.seqs[meta.ID] != 0 {
		t.Fatalf("observed recovery accepted old events: Agent=%#v active=%#v seq=%d", meta, rt.activeTurn, h.seqs[meta.ID])
	}
}

func TestCodexInterruptedTurnInspectorDistinguishesCleanAmbiguousAndTerminal(t *testing.T) {
	for _, test := range []struct {
		name string
		turn RuntimeHistoryTurn
		want string
	}{
		{name: "clean", turn: RuntimeHistoryTurn{ID: "native-1", Status: "running", Items: []map[string]any{{"id": "assistant-1", "type": "agentMessage"}}}, want: RuntimeInterruptionClean},
		{name: "unfinished tool", turn: RuntimeHistoryTurn{ID: "native-1", Status: "running", Items: []map[string]any{{"id": "tool-1", "type": "commandExecution", "command": "deploy", "status": "inProgress"}}}, want: RuntimeInterruptionAmbiguous},
		{name: "projected completed tool is conservative", turn: RuntimeHistoryTurn{ID: "native-1", Status: "running", Items: []map[string]any{{"id": "tool-1", "type": "commandExecution", "command": "deploy", "status": "completed"}}}, want: RuntimeInterruptionAmbiguous},
		{name: "raw paired call is clean", turn: RuntimeHistoryTurn{ID: "native-1", Status: "running", Items: []map[string]any{{"type": "function_call", "call_id": "call-1", "name": "exec"}, {"type": "function_call_output", "call_id": "call-1"}}}, want: RuntimeInterruptionClean},
		{name: "raw unpaired call is ambiguous", turn: RuntimeHistoryTurn{ID: "native-1", Status: "running", Items: []map[string]any{{"type": "function_call", "call_id": "call-1", "name": "exec"}}}, want: RuntimeInterruptionAmbiguous},
		{name: "terminal", turn: RuntimeHistoryTurn{ID: "native-1", Status: "completed"}, want: RuntimeInterruptionTerminal},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence := inspectCodexInterruptedTurn(test.turn)
			if evidence.Status != test.want {
				t.Fatalf("evidence=%#v want status=%s", evidence, test.want)
			}
		})
	}
}

func TestRecoveryViewRedactsNativeEvidence(t *testing.T) {
	meta := &Agent{
		ID: "agent-1", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "native-thread"},
		LastTurn: &TurnSummary{TurnID: "turn-loom", Status: "interrupted"},
		TurnRecoveryMarkers: map[string]TurnRecoveryMarker{
			"turn-loom": {
				PredecessorTurnID: "turn-loom", NativeTurnID: "native-secret", EvidenceLeafID: "leaf-secret",
				RuntimeKind: "codex", Cause: "command_indeterminate", FailurePhase: "turn_start", FailureCode: "transport_timeout",
				Summary: "Runtime command outcome is indeterminate", State: TurnRecoveryObserved,
			},
		},
	}
	view := recoveryView(meta)
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, "native-secret") || strings.Contains(text, "leaf-secret") || !strings.Contains(text, "command_indeterminate") {
		t.Fatalf("public recovery view=%s", text)
	}
}

func TestCleanCodexEvidenceCreatesOneReservedRecoveryTurn(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	fake := &fakeRecoveryRuntime{
		fakeAgentRuntime: fakeAgentRuntime{binding: "native-thread"},
		evidence:         RuntimeInterruptionEvidence{Status: RuntimeInterruptionClean, LeafEntryID: "leaf-clean"},
	}
	ready := make(chan struct{})
	close(ready)
	stamp := now()
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", Cwd: t.TempDir(), ThreadID: "thr-1",
		RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "native-thread"}, RuntimeTurnBindings: map[string]string{"turn-before": "native-before"},
		Status: "interrupted", LastTurn: &TurnSummary{TurnID: "turn-before", Task: "continue safely", Status: "interrupted", CompletedAt: stamp},
		TurnRecoveryMarkers: map[string]TurnRecoveryMarker{
			"turn-before": {PredecessorTurnID: "turn-before", NativeTurnID: "native-before", RuntimeKind: "codex", Cause: "process_exit", State: TurnRecoveryObserved, Summary: "Runtime process exited before the Turn outcome was confirmed", CreatedAt: stamp, UpdatedAt: stamp},
		},
		CreatedAt: stamp, UpdatedAt: stamp,
	}
	h.runtimes["agent-1"] = &runtime{agentID: "agent-1", agentRuntime: fake, ready: ready, approvals: map[string]*approval{}}

	h.recoverInterruptedTurn("agent-1", "turn-before")
	h.recoverInterruptedTurn("agent-1", "turn-before")

	marker := h.agents["agent-1"].TurnRecoveryMarkers["turn-before"]
	if fake.starts != 1 || marker.RecoveryTurnID == "" || marker.State != TurnRecoveryDispatched {
		t.Fatalf("Codex clean recovery starts=%d marker=%#v", fake.starts, marker)
	}
}

func TestCodexDisconnectAfterTurnStartWriteIsInterruptedAndReconciledOnce(t *testing.T) {
	logPath, _ := installFakeIndeterminateThreadHost(t, "turn/start-exit")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	h.stop = make(chan struct{})
	defer h.Shutdown()

	agent, err := h.CreateAgent(CreateParams{Name: "codex-uncertain", Cwd: t.TempDir(), RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.SendTask(agent.ID, "perform this side effect once", time.Second)
	if err == nil {
		t.Fatal("turn/start transport loss returned success")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		view, _ := h.GetAgent(agent.ID)
		requests, _ := h.ListHumanRequests(agent.ID, "all")
		if view.LastTurn != nil && view.LastTurn.Status == "interrupted" && len(requests) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	view, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := h.ListHumanRequests(agent.ID, "all")
	if err != nil {
		t.Fatal(err)
	}
	if view.LastTurn == nil || view.LastTurn.Status != "interrupted" || len(requests) != 1 {
		t.Fatalf("uncertain Codex Turn truth = Agent %#v requests=%#v", view.Agent, requests)
	}
	if view.Recovery == nil || view.Recovery.Cause != "process_exit" || view.Recovery.RuntimeKind != "codex" {
		t.Fatalf("Codex process exit recovery = %#v", view.Recovery)
	}
	if !strings.Contains(requests[0].Context, "may have partially completed") {
		t.Fatalf("Needs You lacks actionable uncertainty: %#v", requests[0])
	}
	if got := countMethod(readRequestMethods(t, logPath), "turn/start"); got != 1 {
		t.Fatalf("uncertain original prompt was replayed: turn/start count=%d", got)
	}
}

func TestTypedIndeterminatePersistsCommandCauseAndFailureIdentity(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	turn := &turnState{turnID: "turn-1", task: "write once", startedAt: time.Now(), lastActivity: time.Now(), stopWatchdog: make(chan struct{})}
	meta := &Agent{
		ID: "agent-1", Name: "worker", RuntimeBinding: RuntimeBinding{Kind: "codex"}, Status: "running",
		CurrentTask: turn.task, CurrentTurnID: turn.turnID, CreatedAt: now(), UpdatedAt: now(),
	}
	rt := &runtime{agentID: meta.ID, activeTurn: turn, approvals: map[string]*approval{}}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = rt
	failure := &runtimecontract.Failure{Code: "transport_timeout", Phase: runtimecontract.FailurePhaseTurnStart, Message: "timeout", Cause: errors.New("timeout")}

	h.onRuntimeFailure(rt, &runtimeIndeterminateError{failure: failure})

	h.workers.Wait()
	h.mu.Lock()
	marker := meta.TurnRecoveryMarkers[turn.turnID]
	h.mu.Unlock()
	if marker.Cause != "command_indeterminate" || marker.FailurePhase != string(runtimecontract.FailurePhaseTurnStart) || marker.FailureCode != "transport_timeout" {
		t.Fatalf("typed failure marker=%#v", marker)
	}
}

func TestGracefulShutdownPersistsDistinctRecoveryCause(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	h.stop = make(chan struct{})
	turn := &turnState{turnID: "turn-1", task: "write once", startedAt: time.Now(), lastActivity: time.Now(), stopWatchdog: make(chan struct{})}
	meta := &Agent{
		ID: "agent-1", Name: "worker", RuntimeBinding: RuntimeBinding{Kind: "codex"}, Status: "running",
		CurrentTask: turn.task, CurrentTurnID: turn.turnID, CreatedAt: now(), UpdatedAt: now(),
	}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = &runtime{agentID: meta.ID, activeTurn: turn, approvals: map[string]*approval{}}

	h.Shutdown()

	marker := meta.TurnRecoveryMarkers[turn.turnID]
	if meta.LastTurn == nil || meta.LastTurn.Status != "interrupted" || marker.State != TurnRecoveryObserved || marker.Cause != "hub_shutdown" {
		t.Fatalf("shutdown recovery Agent=%#v marker=%#v", meta, marker)
	}
}

func TestShutdownCheckpointFailureDoesNotExposeRecovery(t *testing.T) {
	dir := t.TempDir()
	writable, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := store.OpenWithOptions(dir, store.OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	h := testHub(readOnly)
	h.stop = make(chan struct{})
	turn := &turnState{turnID: "turn-1", task: "write once", startedAt: time.Now(), lastActivity: time.Now(), stopWatchdog: make(chan struct{})}
	meta := &Agent{
		ID: "agent-1", Name: "worker", RuntimeBinding: RuntimeBinding{Kind: "pi"}, Status: "running",
		CurrentTask: turn.task, CurrentTurnID: turn.turnID, CreatedAt: now(), UpdatedAt: now(),
	}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = &runtime{agentID: meta.ID, activeTurn: turn, approvals: map[string]*approval{}}

	h.Shutdown()

	if meta.Status != "running" || meta.LastTurn != nil || recoveryView(meta) != nil {
		t.Fatalf("failed shutdown checkpoint became public: %#v recovery=%#v", meta, recoveryView(meta))
	}
}

func TestRecoveryTurnCompletionPersistenceFailurePublishesNothing(t *testing.T) {
	dir := t.TempDir()
	writable, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := now()
	marker := TurnRecoveryMarker{
		PredecessorTurnID: "turn-before", RuntimeKind: "pi", Cause: "process_exit", State: TurnRecoveryDispatched,
		Disposition: "recovery_turn", RecoveryTurnID: "turn-recovery", CreatedAt: stamp, UpdatedAt: stamp,
	}
	if err := writable.SaveAgents(map[string]*Agent{
		"agent-1": {
			ID: "agent-1", Name: "worker", ThreadID: "thr-1", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "pi", NativeRef: "session.jsonl"}, Status: "running",
			CurrentTask: "continue", CurrentTurnID: "turn-recovery",
			LastTurn:            &TurnSummary{TurnID: "turn-before", Status: "interrupted"},
			TurnRecoveryMarkers: map[string]TurnRecoveryMarker{"turn-before": marker}, CreatedAt: stamp, UpdatedAt: stamp,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := store.OpenWithOptions(dir, store.OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	h, err := OpenWithOptions(readOnly, OpenOptions{Passive: true})
	if err != nil {
		t.Fatal(err)
	}
	turn := &turnState{
		turnID: "turn-recovery", nativeTurnID: "native-recovery", task: "continue", startedAt: time.Now(), lastActivity: time.Now(), stopWatchdog: make(chan struct{}),
	}
	rt := &runtime{agentID: "agent-1", activeTurn: turn, approvals: map[string]*approval{}}
	h.runtimes["agent-1"] = rt

	h.onRuntimeEvent(rt, RuntimeEvent{Kind: RuntimeTurnCompleted, LoomTurnID: turn.turnID, NativeTurnID: turn.nativeTurnID})
	view, err := h.GetAgent("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "running" || view.LastTurn == nil || view.LastTurn.TurnID != "turn-before" || view.Recovery == nil || view.Recovery.State != TurnRecoveryDispatched || h.seqs["agent-1"] != 0 {
		t.Fatalf("failed completion checkpoint public view=%#v seq=%d", view, h.seqs["agent-1"])
	}
	h.Shutdown()
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var persisted map[string]*Agent
	if err := reopened.LoadAgents(&persisted); err != nil {
		t.Fatal(err)
	}
	if got := persisted["agent-1"]; got == nil || got.Status != "running" || got.TurnRecoveryMarkers["turn-before"].State != TurnRecoveryDispatched {
		t.Fatalf("reopened completion truth=%#v", got)
	}
}

func TestIndeterminateSteerAndInterruptUseSameDurableRecovery(t *testing.T) {
	for _, operation := range []string{"steer", "interrupt"} {
		t.Run(operation, func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			h := testHub(st)
			phase := runtimecontract.FailurePhaseTurnContinue
			if operation == "interrupt" {
				phase = runtimecontract.FailurePhaseTurnInterrupt
			}
			failure := &runtimeIndeterminateError{failure: &runtimecontract.Failure{
				Code: "transport_timeout", Phase: phase, Message: "timeout", Cause: errors.New("timeout"),
			}}
			fake := &fakeAgentRuntime{binding: "native-thread", steerErr: failure, interruptErr: failure}
			turn := &turnState{
				turnID: "turn-1", nativeTurnID: "native-turn", task: "write once", startedAt: time.Now(), lastActivity: time.Now(),
				stopWatchdog: make(chan struct{}),
			}
			meta := &Agent{
				ID: "agent-1", Name: "worker", RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "native-thread"},
				Status: "running", CurrentTask: turn.task, CurrentTurnID: turn.turnID, CreatedAt: now(), UpdatedAt: now(),
			}
			rt := &runtime{agentID: meta.ID, agentRuntime: fake, activeTurn: turn, approvals: map[string]*approval{}}
			h.agents[meta.ID] = meta
			h.runtimes[meta.ID] = rt

			if operation == "steer" {
				_, err = h.requestTurnSteer(rt, "native-thread", turn.turnID, "continue", time.Second)
			} else {
				_, err = h.Interrupt(meta.ID, "Owner abort")
			}
			if err == nil {
				t.Fatal("indeterminate operation returned success")
			}
			h.workers.Wait()
			h.mu.Lock()
			marker := meta.TurnRecoveryMarkers[turn.turnID]
			lastTurn := meta.LastTurn
			h.mu.Unlock()
			if lastTurn == nil || lastTurn.Status != "interrupted" || marker.Cause != "command_indeterminate" || marker.State == "" {
				t.Fatalf("%s recovery Agent=%#v marker=%#v", operation, meta, marker)
			}
		})
	}
}

func TestIndeterminateCheckpointFencesRuntimeBeforeEvidenceInspection(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	failure := &runtimeIndeterminateError{failure: &runtimecontract.Failure{
		Code: "transport_timeout", Phase: runtimecontract.FailurePhaseTurnContinue, Message: "timeout", Cause: errors.New("timeout"),
	}}
	fake := &fakeRecoveryRuntime{
		fakeAgentRuntime: fakeAgentRuntime{binding: "session.jsonl", steerErr: failure},
		evidence:         RuntimeInterruptionEvidence{Status: RuntimeInterruptionAmbiguous},
	}
	h := testHub(st)
	turn := &turnState{
		turnID: "turn-1", nativeTurnID: "native-turn", task: "write once", startedAt: time.Now(), lastActivity: time.Now(), stopWatchdog: make(chan struct{}),
	}
	meta := &Agent{
		ID: "agent-1", Name: "worker", RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "session.jsonl"},
		RuntimeTurnBindings: map[string]string{"turn-1": "native-turn"}, Status: "running", CurrentTask: turn.task, CurrentTurnID: turn.turnID,
		CreatedAt: now(), UpdatedAt: now(),
	}
	rt := &runtime{agentID: meta.ID, agentRuntime: fake, activeTurn: turn, approvals: map[string]*approval{}}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = rt

	if _, err := h.requestTurnSteer(rt, "session.jsonl", turn.turnID, "continue", time.Second); err == nil {
		t.Fatal("indeterminate steer returned success")
	}
	h.workers.Wait()
	if fake.inspectCount.Load() != 1 || fake.aliveAtInspect.Load() {
		t.Fatalf("evidence inspection count=%d alive=%v", fake.inspectCount.Load(), fake.aliveAtInspect.Load())
	}
}

func TestSharedCodexIndeterminateClosesHostBeforeAnyRecoveryInspection(t *testing.T) {
	installFakeSharedCodexHost(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	host, err := h.ensureCodexHost()
	if err != nil {
		t.Fatal(err)
	}
	failure := &runtimeIndeterminateError{failure: &runtimecontract.Failure{
		Code: "transport_timeout", Phase: runtimecontract.FailurePhaseTurnStart,
		Message: "timeout", Cause: errors.New("transport timeout"),
	}}
	fakes := []*fakeRecoveryRuntime{
		{evidence: RuntimeInterruptionEvidence{Status: RuntimeInterruptionAmbiguous}},
		{evidence: RuntimeInterruptionEvidence{Status: RuntimeInterruptionAmbiguous}},
	}
	for i, fake := range fakes {
		agentID := "agent-" + string(rune('1'+i))
		turnID := "turn-" + string(rune('1'+i))
		nativeTurnID := "native-" + turnID
		fake.hostClosed = host.client.Closed
		turn := &turnState{
			turnID: turnID, nativeTurnID: nativeTurnID, task: "write once", startedAt: time.Now(), lastActivity: time.Now(),
			stopWatchdog: make(chan struct{}),
		}
		meta := &Agent{
			ID: agentID, Name: agentID, RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thread-" + agentID},
			RuntimeTurnBindings: map[string]string{turnID: nativeTurnID}, Status: "running", CurrentTask: turn.task, CurrentTurnID: turnID,
			CreatedAt: now(), UpdatedAt: now(),
		}
		h.agents[agentID] = meta
		h.runtimes[agentID] = &runtime{
			agentID: agentID, agentRuntime: fake, client: host.client, hostGeneration: host.generation,
			activeTurn: turn, approvals: map[string]*approval{},
		}
	}

	h.onRuntimeIndeterminate(h.runtimes["agent-1"], failure)
	h.workers.Wait()

	if !host.client.Closed() {
		t.Fatal("shared Codex host remained alive after indeterminate command")
	}
	for i, fake := range fakes {
		if fake.inspectCount.Load() != 1 || !fake.hostClosedAtInspect.Load() {
			t.Fatalf("Agent %d inspection count=%d hostClosed=%v", i+1, fake.inspectCount.Load(), fake.hostClosedAtInspect.Load())
		}
	}
}
