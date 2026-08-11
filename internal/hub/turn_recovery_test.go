package hub

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/rollout"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestRecoveryCrashWindowsConvergeAcrossTwoStoreReopens(t *testing.T) {
	tests := []struct {
		name        string
		disposition string
		state       string
		target      bool
		completed   bool
		wantStarts  int
	}{
		{name: "recovery planned before target", disposition: "recovery_turn", state: TurnRecoveryPlanned, wantStarts: 1},
		{name: "recovery target durable before dispatched", disposition: "recovery_turn", state: TurnRecoveryPlanned, target: true},
		{name: "recovery dispatched", disposition: "recovery_turn", state: TurnRecoveryDispatched, target: true},
		{name: "recovery completed", disposition: "recovery_turn", state: TurnRecoveryCompleted, target: true, completed: true},
		{name: "needs you planned before target", disposition: "needs_you", state: TurnRecoveryPlanned},
		{name: "needs you target durable before dispatched", disposition: "needs_you", state: TurnRecoveryPlanned, target: true},
		{name: "needs you dispatched", disposition: "needs_you", state: TurnRecoveryDispatched, target: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			st, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			stamp := now()
			marker := TurnRecoveryMarker{
				PredecessorTurnID: "turn-before", NativeTurnID: "native-before-secret",
				Disposition: test.disposition, State: test.state, RuntimeKind: "pi", Cause: "process_exit",
				Summary: "Runtime process exited before the Turn outcome was confirmed", CreatedAt: stamp, UpdatedAt: stamp,
			}
			if test.disposition == "recovery_turn" {
				marker.RecoveryTurnID = "turn-recovery-reserved"
			} else {
				marker.HumanRequestID = "hrq-recovery-reserved"
			}
			agent := &Agent{
				ID: "agent-1", Name: "worker", Cwd: t.TempDir(), ThreadID: "thr-loom",
				RuntimeBinding:      RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "pi", NativeRef: "native-session-secret"},
				RuntimeTurnBindings: map[string]string{"turn-before": "native-before-secret"},
				TurnRecoveryMarkers: map[string]TurnRecoveryMarker{"turn-before": marker},
				Status:              "interrupted", LastTurn: &TurnSummary{TurnID: "turn-before", Task: "perform original side effect", Status: "interrupted", CompletedAt: stamp},
				CreatedAt: stamp, UpdatedAt: stamp,
			}
			if test.disposition == "recovery_turn" && test.target {
				agent.RuntimeTurnBindings[marker.RecoveryTurnID] = "native-recovery-secret"
				if test.completed {
					agent.Status = "idle"
					agent.LastTurn = &TurnSummary{TurnID: marker.RecoveryTurnID, Task: "Continue interrupted work", Status: "completed", CompletedAt: stamp}
				} else {
					agent.Status = "running"
					agent.CurrentTurnID = marker.RecoveryTurnID
					agent.CurrentTask = "Continue interrupted work"
				}
			}
			if err := st.SaveAgents(map[string]*Agent{"agent-1": agent}); err != nil {
				t.Fatal(err)
			}
			if test.disposition == "needs_you" && test.target {
				if err := st.AppendHumanRequest(HumanRequest{
					ID: marker.HumanRequestID, AgentID: agent.ID, AgentName: agent.Name, ThreadID: agent.ThreadID,
					SourceTurnID: marker.PredecessorTurnID, SourceTask: agent.LastTurn.Task, Question: "Confirm outcome",
					Expectation: HumanRequestRequired, State: "open", DeliveryStatus: "waiting", CreatedAt: stamp, UpdatedAt: stamp,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}

			totalStarts := 0
			previousEventCount := -1
			for reopen := 1; reopen <= 2; reopen++ {
				reopened, err := store.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				h := testHub(reopened)
				if err := reopened.LoadAgents(&h.agents); err != nil {
					t.Fatal(err)
				}
				if err := h.loadHumanRequests(); err != nil {
					t.Fatal(err)
				}
				for agentID := range h.agents {
					h.seqs[agentID] = reopened.LastSeq(agentID)
				}
				fake := &fakeRecoveryRuntime{
					fakeAgentRuntime: fakeAgentRuntime{binding: "native-session-secret"},
					evidence:         RuntimeInterruptionEvidence{Status: RuntimeInterruptionClean, LeafEntryID: "leaf-secret"},
				}
				ready := make(chan struct{})
				close(ready)
				h.runtimes[agent.ID] = &runtime{agentID: agent.ID, agentRuntime: fake, ready: ready, approvals: map[string]*approval{}}
				h.contextHistoryProbe = func(threadID string, _ rollout.ContextHistoryQuery) (rollout.ContextHistoryState, error) {
					return rollout.ContextHistoryState{EpochID: "initial:" + threadID}, nil
				}

				h.recoverInterruptedTurn(agent.ID, marker.PredecessorTurnID)
				totalStarts += fake.starts
				requests, err := h.ListHumanRequests(agent.ID, "all")
				if err != nil {
					t.Fatal(err)
				}
				h.mu.Lock()
				currentMarker := h.agents[agent.ID].TurnRecoveryMarkers[marker.PredecessorTurnID]
				h.mu.Unlock()
				if test.disposition == "needs_you" {
					if len(requests) != 1 || requests[0].ID != marker.HumanRequestID || currentMarker.HumanRequestID != marker.HumanRequestID {
						t.Fatalf("reopen %d Needs You cardinality requests=%#v marker=%#v", reopen, requests, currentMarker)
					}
				} else if len(requests) != 0 || currentMarker.RecoveryTurnID != marker.RecoveryTurnID {
					t.Fatalf("reopen %d Recovery Turn identity requests=%#v marker=%#v", reopen, requests, currentMarker)
				}
				if test.completed {
					if currentMarker.State != TurnRecoveryCompleted {
						t.Fatalf("reopen %d completed marker=%#v", reopen, currentMarker)
					}
				} else if currentMarker.State != TurnRecoveryDispatched {
					t.Fatalf("reopen %d marker state=%#v", reopen, currentMarker)
				}
				events, err := h.ReadEvents(agent.ID, 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				if previousEventCount >= 0 && len(events) != previousEventCount {
					t.Fatalf("second reopen duplicated public events: before=%d after=%d events=%#v", previousEventCount, len(events), events)
				}
				previousEventCount = len(events)
				view, err := h.GetAgent(agent.ID)
				if err != nil {
					t.Fatal(err)
				}
				public, _ := json.Marshal(struct {
					View     AgentView
					Events   []store.Event
					Requests []HumanRequest
				}{view, events, requests})
				for _, secret := range []string{"native-session-secret", "native-before-secret", "native-recovery-secret", "leaf-secret"} {
					if strings.Contains(string(public), secret) {
						t.Fatalf("reopen %d public projection leaked %q: %s", reopen, secret, public)
					}
				}
				if fake.starts > 0 {
					var prompt strings.Builder
					for _, input := range fake.lastTurnRequest.Input {
						if input.Kind == RuntimeInputText {
							prompt.WriteString(input.Text)
						}
					}
					if !strings.Contains(prompt.String(), "<loom_turn_recovery") {
						t.Fatalf("reopen %d replayed original prompt instead of recovery control: %s", reopen, prompt.String())
					}
				}
				h.mu.Lock()
				h.stopping = true
				for _, runtime := range h.runtimes {
					if runtime.activeTurn != nil && !runtime.activeTurn.finished {
						runtime.activeTurn.finished = true
						close(runtime.activeTurn.stopWatchdog)
						runtime.activeTurn = nil
					}
				}
				h.mu.Unlock()
				h.workers.Wait()
				if err := reopened.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if totalStarts != test.wantStarts {
				t.Fatalf("reserved target starts=%d want=%d", totalStarts, test.wantStarts)
			}
		})
	}
}

type fakeRecoveryRuntime struct {
	fakeAgentRuntime
	evidence            RuntimeInterruptionEvidence
	inspectStarted      chan struct{}
	inspectRelease      chan struct{}
	starts              int
	inspectCount        atomic.Int32
	aliveAtInspect      atomic.Bool
	hostClosed          func() bool
	hostClosedAtInspect atomic.Bool
	inspectErr          error
}

func (f *fakeRecoveryRuntime) InspectInterruptedTurn(_, _ string) (RuntimeInterruptionEvidence, error) {
	f.inspectCount.Add(1)
	f.aliveAtInspect.Store(f.Alive())
	if f.hostClosed != nil {
		f.hostClosedAtInspect.Store(f.hostClosed())
	}
	if f.inspectStarted != nil {
		close(f.inspectStarted)
	}
	if f.inspectRelease != nil {
		<-f.inspectRelease
	}
	return f.evidence, f.inspectErr
}

func (f *fakeRecoveryRuntime) StartTurn(request RuntimeTurnRequest) (string, error) {
	f.starts++
	f.lastTurnRequest = request
	return "native-recovery-1", nil
}

func TestCleanPiCrashCreatesOneReservedRecoveryTurn(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.contextHistoryProbe = func(threadID string, _ rollout.ContextHistoryQuery) (rollout.ContextHistoryState, error) {
		return rollout.ContextHistoryState{EpochID: "initial:" + threadID}, nil
	}
	fake := &fakeRecoveryRuntime{
		fakeAgentRuntime: fakeAgentRuntime{binding: "native-session"},
		evidence:         RuntimeInterruptionEvidence{Status: RuntimeInterruptionClean, LeafEntryID: "leaf-clean"},
	}
	ready := make(chan struct{})
	close(ready)
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", Cwd: t.TempDir(), ThreadID: "loom-thread-1",
		RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "native-session"}, RuntimeTurnBindings: map[string]string{"turn-before": "native-before"},
		Status: "interrupted", LastTurn: &TurnSummary{TurnID: "turn-before", Task: "publish release", Status: "interrupted", CompletedAt: "2026-08-10T01:00:00Z"},
		CreatedAt: now(), UpdatedAt: now(),
	}
	h.runtimes["agent-1"] = &runtime{agentID: "agent-1", agentRuntime: fake, ready: ready, approvals: map[string]*approval{}}

	h.recoverPiInterruptedTurn("agent-1", "turn-before")
	marker := h.agents["agent-1"].TurnRecoveryMarkers["turn-before"]
	if fake.starts != 1 || marker.RecoveryTurnID == "" || marker.State != TurnRecoveryDispatched {
		t.Fatalf("first recovery starts=%d marker=%#v", fake.starts, marker)
	}
	if h.agents["agent-1"].CurrentTurnID != marker.RecoveryTurnID || marker.RecoveryTurnID == "turn-before" {
		t.Fatalf("Recovery Turn identity current=%q marker=%#v", h.agents["agent-1"].CurrentTurnID, marker)
	}
	var prompt strings.Builder
	for _, input := range fake.lastTurnRequest.Input {
		if input.Kind == RuntimeInputText {
			prompt.WriteString(input.Text)
		}
	}
	if !strings.Contains(prompt.String(), `<loom_turn_recovery`) || !strings.Contains(prompt.String(), `previous_turn_id="turn-before"`) {
		t.Fatalf("Recovery prompt = %s", prompt.String())
	}

	h.recoverPiInterruptedTurn("agent-1", "turn-before")
	if fake.starts != 1 {
		t.Fatalf("duplicate recovery starts = %d", fake.starts)
	}
	h.onRuntimeEvent(h.runtimes["agent-1"], RuntimeEvent{Kind: RuntimeTurnCompleted, NativeTurnID: "native-recovery-1"})
	if got := h.agents["agent-1"].TurnRecoveryMarkers["turn-before"].State; got != TurnRecoveryCompleted {
		t.Fatalf("completed recovery marker state = %q", got)
	}
	h.mu.Lock()
	source, sourceErr := h.turnReferenceLocked("agent-1", marker.RecoveryTurnID)
	h.mu.Unlock()
	if sourceErr != "" || source == nil || source.Kind != "recovery" || source.ID != "turn-before" {
		t.Fatalf("Recovery Turn source = %#v, err=%q", source, sourceErr)
	}
}

func TestAmbiguousPiCrashCreatesOneReservedNeedsYouAndNoRecoveryTurn(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	fake := &fakeRecoveryRuntime{
		fakeAgentRuntime: fakeAgentRuntime{binding: "native-session"},
		inspectErr:       fmt.Errorf("read /private/native/session-secret.jsonl: native-ref-secret"),
		evidence: RuntimeInterruptionEvidence{
			Status: RuntimeInterruptionAmbiguous, LeafEntryID: "assistant-tool",
			UnfinishedTools: []RuntimeToolEvidence{{ID: "call-deploy", Name: "bash", Command: "deploy --prod", StartedAt: "2026-08-10T01:00:02Z"}},
		},
	}
	ready := make(chan struct{})
	close(ready)
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", Cwd: t.TempDir(), ThreadID: "loom-thread-1",
		RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "native-session"}, RuntimeTurnBindings: map[string]string{"turn-before": "native-before"},
		Status: "interrupted", LastTurn: &TurnSummary{TurnID: "turn-before", Task: "publish release", Status: "interrupted", CompletedAt: "2026-08-10T01:00:00Z"},
		CreatedAt: now(), UpdatedAt: now(),
	}
	h.runtimes["agent-1"] = &runtime{agentID: "agent-1", agentRuntime: fake, ready: ready, approvals: map[string]*approval{}}

	h.recoverPiInterruptedTurn("agent-1", "turn-before")
	marker := h.agents["agent-1"].TurnRecoveryMarkers["turn-before"]
	request := h.humanRequests[marker.HumanRequestID]
	if marker.Disposition != "needs_you" || marker.State != TurnRecoveryDispatched || marker.HumanRequestID == "" || request == nil {
		t.Fatalf("ambiguous recovery marker=%#v request=%#v", marker, request)
	}
	if fake.starts != 0 || h.agents["agent-1"].Status != "idle" || request.AgentID != "agent-1" || request.ThreadID != "loom-thread-1" || request.SourceTurnID != "turn-before" {
		t.Fatalf("ambiguous recovery starts=%d Agent=%#v request=%#v", fake.starts, h.agents["agent-1"], request)
	}
	for _, want := range []string{"deploy --prod", "may have partially completed", "bash"} {
		if !strings.Contains(request.Context, want) {
			t.Fatalf("Needs You context missing %q: %s", want, request.Context)
		}
	}
	for _, secret := range []string{"assistant-tool", "call-deploy", "/private/native/session-secret.jsonl", "native-ref-secret"} {
		if strings.Contains(request.Context, secret) {
			t.Fatalf("Needs You context leaked native evidence %q: %s", secret, request.Context)
		}
	}

	h.recoverPiInterruptedTurn("agent-1", "turn-before")
	if len(h.humanRequestOrder) != 1 || fake.starts != 0 {
		t.Fatalf("duplicate ambiguous recovery requests=%v starts=%d", h.humanRequestOrder, fake.starts)
	}
}

func TestStoreRestartConvergesPlannedNeedsYouMarkerWithoutDuplicate(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := now()
	marker := TurnRecoveryMarker{
		PredecessorTurnID: "turn-before", NativeTurnID: "native-before", Disposition: "needs_you", State: TurnRecoveryPlanned,
		HumanRequestID: "hrq-recovery", CreatedAt: stamp, UpdatedAt: stamp,
	}
	if err := st.SaveAgents(map[string]*Agent{
		"agent-1": {
			ID: "agent-1", Name: "worker", Cwd: t.TempDir(), ThreadID: "loom-thread-1",
			RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: t.TempDir() + "/session.jsonl"}, RuntimeTurnBindings: map[string]string{"turn-before": "native-before"},
			TurnRecoveryMarkers: map[string]TurnRecoveryMarker{"turn-before": marker}, Status: "interrupted",
			LastTurn: &TurnSummary{TurnID: "turn-before", Task: "publish release", Status: "interrupted", CompletedAt: stamp}, CreatedAt: stamp, UpdatedAt: stamp,
		},
	}); err != nil {
		t.Fatal(err)
	}
	request := HumanRequest{
		ID: marker.HumanRequestID, AgentID: "agent-1", AgentName: "worker", ThreadID: "loom-thread-1", SourceTurnID: "turn-before",
		SourceTask: "publish release", BlockedWork: "publish release", Expectation: HumanRequestRequired,
		Question: "Confirm the outcome", State: "open", DeliveryStatus: "waiting", CreatedAt: stamp, UpdatedAt: stamp,
	}
	if err := st.AppendHumanRequest(request); err != nil {
		t.Fatal(err)
	}

	h, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		state := h.agents["agent-1"].TurnRecoveryMarkers["turn-before"].State
		h.mu.Unlock()
		if state == TurnRecoveryDispatched {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	requests, err := h.ListHumanRequests("agent-1", "all")
	if err != nil || len(requests) != 1 || requests[0].ID != marker.HumanRequestID {
		t.Fatalf("restarted Needs You = %#v, err=%v", requests, err)
	}
	h.mu.Lock()
	state := h.agents["agent-1"].TurnRecoveryMarkers["turn-before"].State
	h.mu.Unlock()
	if state != TurnRecoveryDispatched {
		t.Fatalf("restarted marker state = %q", state)
	}
	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := Open(reopened)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Shutdown()
	requests, err = restarted.ListHumanRequests("agent-1", "all")
	if err != nil || len(requests) != 1 || requests[0].ID != marker.HumanRequestID {
		t.Fatalf("second restart Needs You = %#v, err=%v", requests, err)
	}
}

func TestAutomaticRecoveryMarkerFencesManualInterruptedTurnActions(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", Status: "interrupted",
		LastTurn: &TurnSummary{TurnID: "turn-before", Task: "publish release", Status: "interrupted", CompletedAt: now()},
		TurnRecoveryMarkers: map[string]TurnRecoveryMarker{
			"turn-before": {PredecessorTurnID: "turn-before", Disposition: "recovery_turn", State: TurnRecoveryPlanned, RecoveryTurnID: "turn-recovery"},
		},
		CreatedAt: now(), UpdatedAt: now(),
	}

	if _, err := h.ContinueInterruptedTurn("agent-1", time.Second); err == nil || !strings.Contains(err.Error(), "automatic recovery") {
		t.Fatalf("manual Continue error = %v", err)
	}
	if _, err := h.DismissInterruptedTurn("agent-1"); err == nil || !strings.Contains(err.Error(), "automatic recovery") {
		t.Fatalf("manual Dismiss error = %v", err)
	}
}

func TestRecoveryDispatchPersistenceFailureKeepsInterruptedMarker(t *testing.T) {
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
	marker := TurnRecoveryMarker{
		PredecessorTurnID: "turn-before", Disposition: "needs_you", State: TurnRecoveryPlanned,
		HumanRequestID: "hrq-recovery", CreatedAt: now(), UpdatedAt: now(),
	}
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", Status: "interrupted", LastError: "runtime failed",
		LastTurn:            &TurnSummary{TurnID: "turn-before", Status: "interrupted"},
		TurnRecoveryMarkers: map[string]TurnRecoveryMarker{"turn-before": marker},
	}

	h.markRecoveryDispatched("agent-1", "turn-before", marker, true)
	if got := h.agents["agent-1"]; got.Status != "interrupted" || got.LastError != "runtime failed" || got.TurnRecoveryMarkers["turn-before"].State != TurnRecoveryPlanned {
		t.Fatalf("failed persistence changed recovery truth: %#v", got)
	}
}

func TestAutomaticRecoveryClaimFencesManualActionsBeforeMarkerIsPersisted(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	started := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeRecoveryRuntime{
		fakeAgentRuntime: fakeAgentRuntime{binding: "native-session"},
		evidence: RuntimeInterruptionEvidence{
			Status:          RuntimeInterruptionAmbiguous,
			UnfinishedTools: []RuntimeToolEvidence{{ID: "call-deploy", Name: "bash"}},
		},
		inspectStarted: started,
		inspectRelease: release,
	}
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "loom-thread-1", Status: "interrupted",
		RuntimeBinding:      RuntimeBinding{Kind: "pi", NativeRef: "native-session"},
		RuntimeTurnBindings: map[string]string{"turn-before": "native-before"},
		LastTurn:            &TurnSummary{TurnID: "turn-before", Task: "publish release", Status: "interrupted", CompletedAt: now()},
		CreatedAt:           now(), UpdatedAt: now(),
	}
	h.runtimes["agent-1"] = &runtime{agentID: "agent-1", agentRuntime: fake}

	h.mu.Lock()
	if !h.schedulePiRecoveryLocked("agent-1", "turn-before") {
		h.mu.Unlock()
		t.Fatal("automatic recovery was not scheduled")
	}
	h.mu.Unlock()
	<-started

	if _, err := h.ContinueInterruptedTurn("agent-1", time.Second); err == nil || !strings.Contains(err.Error(), "automatic recovery") {
		t.Fatalf("manual Continue error = %v", err)
	}
	if _, err := h.DismissInterruptedTurn("agent-1"); err == nil || !strings.Contains(err.Error(), "automatic recovery") {
		t.Fatalf("manual Dismiss error = %v", err)
	}

	close(release)
	h.workers.Wait()
}

func TestUnknownPiCrashEvidenceCreatesNeedsYouInsteadOfRecoveryTurn(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", Cwd: t.TempDir(), ThreadID: "loom-thread-1",
		RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "missing-session"}, Status: "interrupted",
		LastTurn:  &TurnSummary{TurnID: "turn-before", Task: "publish release", Status: "interrupted", CompletedAt: now()},
		CreatedAt: now(), UpdatedAt: now(),
	}

	h.recoverPiInterruptedTurn("agent-1", "turn-before")
	marker := h.agents["agent-1"].TurnRecoveryMarkers["turn-before"]
	request := h.humanRequests[marker.HumanRequestID]
	if marker.Disposition != "needs_you" || marker.RecoveryTurnID != "" || request == nil || !strings.Contains(request.Context, "could not safely confirm the durable Runtime outcome") || strings.Contains(request.Context, "no native Turn binding") {
		t.Fatalf("unknown recovery marker=%#v request=%#v", marker, request)
	}
}

var _ AgentRuntime = (*fakeRecoveryRuntime)(nil)
var _ RuntimeInterruptedTurnInspector = (*fakeRecoveryRuntime)(nil)
