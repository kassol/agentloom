package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/rollout"
	"github.com/yan5xu/codex-loom/internal/store"
)

type fakeRecoveryRuntime struct {
	fakeAgentRuntime
	evidence       RuntimeInterruptionEvidence
	inspectStarted chan struct{}
	inspectRelease chan struct{}
	starts         int
}

func (f *fakeRecoveryRuntime) InspectInterruptedTurn(_, _ string) (RuntimeInterruptionEvidence, error) {
	if f.inspectStarted != nil {
		close(f.inspectStarted)
	}
	if f.inspectRelease != nil {
		<-f.inspectRelease
	}
	return f.evidence, nil
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
	for _, want := range []string{"deploy --prod", "may have partially completed", "assistant-tool"} {
		if !strings.Contains(request.Context, want) {
			t.Fatalf("Needs You context missing %q: %s", want, request.Context)
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
	if marker.Disposition != "needs_you" || marker.RecoveryTurnID != "" || request == nil || !strings.Contains(request.Context, "no native Turn binding") {
		t.Fatalf("unknown recovery marker=%#v request=%#v", marker, request)
	}
}

var _ AgentRuntime = (*fakeRecoveryRuntime)(nil)
var _ RuntimeInterruptedTurnInspector = (*fakeRecoveryRuntime)(nil)
