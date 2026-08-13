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

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

type goalSyncContract struct {
	*controlPlaneContract
	err       error
	updateErr error
	clearErr  error
	initial   *ThreadGoal
	getCalls  int
	updates   []GoalUpdateParams
	clears    int
	onUpdate  func()
}

type goalConformanceHost struct {
	contract runtimecontract.Contract
	alive    bool
}

func (h *goalConformanceHost) Alive() bool                        { return h.alive }
func (h *goalConformanceHost) Contract() runtimecontract.Contract { return h.contract }
func (h *goalConformanceHost) SetFailureHandler(func(error))      {}
func (h *goalConformanceHost) Close()                             { h.alive = false }

type goalConformanceDriver struct{ host *goalConformanceHost }

func (d *goalConformanceDriver) Preflight(context.Context) error { return nil }
func (d *goalConformanceDriver) Acquire(context.Context, AgentHostRequest) (AgentHost, error) {
	return d.host, nil
}
func (d *goalConformanceDriver) Shutdown(context.Context) error {
	d.host.Close()
	return nil
}

func (c *goalSyncContract) RuntimeGoal(context.Context, runtimecontract.Binding) (*ThreadGoal, error) {
	c.getCalls++
	return cloneGoalRecord(c.initial), c.err
}

func (c *goalSyncContract) UpdateRuntimeGoal(_ context.Context, binding runtimecontract.Binding, update GoalUpdateParams) (*ThreadGoal, error) {
	c.updates = append(c.updates, update)
	if c.onUpdate != nil {
		c.onUpdate()
	}
	if c.updateErr != nil {
		return nil, c.updateErr
	}
	if c.err != nil {
		return nil, c.err
	}
	return &ThreadGoal{ThreadID: binding.NativeRef, Objective: *update.Objective, Status: *update.Status}, nil
}

func (c *goalSyncContract) ClearRuntimeGoal(context.Context, runtimecontract.Binding) (bool, error) {
	c.clears++
	if c.clearErr != nil {
		return false, c.clearErr
	}
	return c.err == nil, c.err
}

func TestGoalNotificationCannotMutateLoomOwnedGoal(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.stopping = true // terminal transitions must not launch background delivery in this unit test
	h.agents["agent-1"] = &Agent{ID: "agent-1", Name: "research", ThreadID: "thread-1", Status: "idle"}
	h.goals["agent-1"] = &ThreadGoal{ID: "goal-loom", Version: 7, ThreadID: "thread-1", Objective: "Loom truth", Status: GoalStatusPaused}
	runtime := &runtime{agentID: "agent-1", approvals: map[string]*approval{}}

	deliverTestNativeNotification(h, runtime, "thread/goal/updated", json.RawMessage(`{
		"threadId":"thread-1",
		"turnId":null,
		"goal":{"threadId":"thread-1","objective":"Complete the audit","status":"active","tokenBudget":120000,"tokensUsed":4300,"timeUsedSeconds":92,"createdAt":100,"updatedAt":200}
	}`))

	h.mu.Lock()
	view := h.viewLocked(h.agents["agent-1"])
	reserved := h.activeGoalReservesThreadLocked("agent-1")
	h.mu.Unlock()
	if view.Goal == nil || view.Goal.Objective != "Loom truth" || view.Goal.Status != GoalStatusPaused || view.Goal.Version != 7 {
		t.Fatalf("native notification mutated Loom Goal = %#v", view.Goal)
	}
	if reserved {
		t.Fatal("native active Goal reserved Loom Thread")
	}

	deliverTestNativeNotification(h, runtime, "thread/goal/cleared", json.RawMessage(`{"threadId":"thread-1"}`))
	h.mu.Lock()
	view = h.viewLocked(h.agents["agent-1"])
	reserved = h.activeGoalReservesThreadLocked("agent-1")
	h.mu.Unlock()
	if view.Goal == nil || view.Goal.Objective != "Loom truth" || reserved {
		t.Fatalf("native clear mutated Loom Goal: %#v, reserved=%v", view.Goal, reserved)
	}
}

func TestOnlyActiveGoalReservesThreadButCausalReplyRemainsEligible(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-1"] = &Agent{ID: "agent-1", Name: "research", ThreadID: "thread-1", Status: "idle"}
	h.goals["agent-1"] = &ThreadGoal{ThreadID: "thread-1", Objective: "Audit", Status: GoalStatusActive}
	if !h.activeGoalReservesThreadLocked("agent-1") {
		t.Fatal("active Goal did not reserve Thread")
	}
	for _, status := range []string{GoalStatusPaused, GoalStatusBlocked, GoalStatusUsageLimited, GoalStatusBudgetLimited, GoalStatusComplete} {
		h.goals["agent-1"] = &ThreadGoal{ThreadID: "thread-1", Objective: "Audit", Status: status}
		if h.activeGoalReservesThreadLocked("agent-1") {
			t.Fatalf("non-running Goal status %s reserved Thread", status)
		}
	}

	root := &AgentMessage{ID: "root", FromAgentID: "agent-1", ToAgentID: "agent-2", SourceTurnID: "turn-goal"}
	reply := &AgentMessage{ID: "reply", FromAgentID: "agent-2", ToAgentID: "agent-1", ReplyTo: "root"}
	unrelated := &AgentMessage{ID: "unrelated", FromAgentID: "agent-2", ToAgentID: "agent-1"}
	h.comms[root.ID] = root
	if !h.isCausalReplyForAgentLocked(reply, "agent-1") {
		t.Fatal("causal reply was not eligible for unfinished Goal")
	}
	if h.isCausalReplyForAgentLocked(unrelated, "agent-1") {
		t.Fatal("unrelated message was treated as causal Goal input")
	}
}

func TestActiveGoalAgentIDsAreStableAndSorted(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-z"] = &Agent{ID: "agent-z"}
	h.agents["agent-paused"] = &Agent{ID: "agent-paused"}
	h.agents["agent-a"] = &Agent{ID: "agent-a"}
	h.goals["agent-z"] = &ThreadGoal{ThreadID: "thread-z", Objective: "Z", Status: GoalStatusActive}
	h.goals["agent-paused"] = &ThreadGoal{ThreadID: "thread-p", Objective: "P", Status: GoalStatusPaused}
	h.goals["agent-a"] = &ThreadGoal{ThreadID: "thread-a", Objective: "A", Status: GoalStatusActive}
	h.goals["agent-archived"] = &ThreadGoal{ThreadID: "thread-orphan", Objective: "orphan", Status: GoalStatusActive}

	got := h.ActiveGoalAgentIDs()
	want := []string{"agent-a", "agent-z"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("active Goal Agent IDs = %#v, want %#v", got, want)
	}
}

func TestActiveGoalKeepsExternalInboxQueued(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-1"] = &Agent{ID: "agent-1", Name: "research", ThreadID: "thread-1", Status: "idle"}
	h.goals["agent-1"] = &ThreadGoal{ThreadID: "thread-1", Objective: "Audit", Status: GoalStatusActive}
	h.inbox = map[string]*InboxItem{"inbox-1": {ID: "inbox-1", AgentID: "agent-1", State: "queued"}}
	h.inboxOrder = []string{"inbox-1"}

	h.deliverNextInboxForAgent("agent-1")
	if h.inbox["inbox-1"].State != "queued" {
		t.Fatalf("Inbox state = %q, want queued", h.inbox["inbox-1"].State)
	}
}

func TestPiGoalCRUDPersistsLogicalIdentityAndRejectsStaleRevision(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.stopping = true
	h.agents["agent-pi"] = &Agent{
		ID: "agent-pi", Name: "pi-worker", ThreadID: "loom-thread-pi", Status: "idle",
		RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "/private/native-session.jsonl"},
	}
	objective := "Ship the Runtime-neutral Goal"
	zero := int64(0)
	goal, err := h.UpdateGoal("agent-pi", GoalUpdateParams{Objective: &objective, ExpectedVersion: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if goal.ID == "" || goal.Version != 1 || goal.ThreadID != "loom-thread-pi" || goal.NativeSyncState != goalNativeSyncNotApplicable {
		t.Fatalf("created Pi Goal = %#v", goal)
	}
	if strings.Contains(goal.ThreadID, "native-session") {
		t.Fatalf("Goal leaked native binding: %#v", goal)
	}
	info, err := os.Stat(filepath.Join(dir, "goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("Goal store mode = %v", info.Mode().Perm())
	}
	one := goal.Version
	cleared, err := h.ClearGoalVersion("agent-pi", &one)
	if err != nil || !cleared {
		t.Fatalf("clear Goal = %v, %v", cleared, err)
	}
	if _, err := h.UpdateGoal("agent-pi", GoalUpdateParams{Objective: &objective, ExpectedVersion: &zero}); err == nil || !strings.Contains(err.Error(), "version changed") {
		t.Fatalf("stale create after clear = %v", err)
	}

	var persisted map[string]*ThreadGoal
	if err := st.LoadGoals(&persisted); err != nil {
		t.Fatal(err)
	}
	tombstone := persisted["agent-pi"]
	if tombstone == nil || tombstone.Version != 2 || tombstone.ClearedAt == 0 {
		t.Fatalf("persisted Goal tombstone = %#v", tombstone)
	}
	restarted := testHub(st)
	restarted.stopping = true
	restarted.agents["agent-pi"] = h.agents["agent-pi"]
	if err := st.LoadGoals(&restarted.goals); err != nil {
		t.Fatal(err)
	}
	if reopened, revision, err := restarted.GetGoalState("agent-pi"); err != nil || reopened != nil || revision != 2 {
		t.Fatalf("reopened cleared Goal = %#v revision=%d err=%v", reopened, revision, err)
	}
	two := int64(2)
	if recreated, err := restarted.UpdateGoal("agent-pi", GoalUpdateParams{Objective: &objective, ExpectedVersion: &two}); err != nil || recreated.Version != 3 || recreated.ID == goal.ID {
		t.Fatalf("recreated Goal = %#v err=%v", recreated, err)
	}
}

func TestArchiveMakesDurableGoalInertAndStableIDRestoreReconnectsIt(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	agent := &Agent{ID: "agent-archive", Name: "archive", ThreadID: "thread-archive", Status: "idle", RuntimeBinding: RuntimeBinding{Kind: "pi"}}
	goal := &ThreadGoal{ID: "goal-archive", Version: 4, ThreadID: agent.ThreadID, Objective: "Resume after restore", Status: GoalStatusActive, NativeSyncState: goalNativeSyncNotApplicable}
	h.agents[agent.ID], h.goals[agent.ID] = agent, goal
	if err := st.SaveAgents(h.agents); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveGoals(h.goals); err != nil {
		t.Fatal(err)
	}

	if _, err := h.ArchiveAgent(agent.ID); err != nil {
		t.Fatal(err)
	}
	persisted := map[string]*ThreadGoal{}
	if err := st.LoadGoals(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted[agent.ID] == nil || persisted[agent.ID].ID != goal.ID {
		t.Fatalf("archive discarded stable-ID Goal slot: %#v", persisted[agent.ID])
	}
	if _, _, err := h.GetGoalState(agent.ID); hubErrorStatus(err) != 404 {
		t.Fatalf("archived Agent exposed inert Goal: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened := testHub(reopenedStore)
	defer reopenedStore.Close()
	if err := reopenedStore.LoadGoals(&reopened.goals); err != nil {
		t.Fatal(err)
	}
	if ids := reopened.ActiveGoalAgentIDs(); len(ids) != 0 {
		t.Fatalf("archived Goal started without its Agent after reopen: %v", ids)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	close(release)
	contract := &controlPlaneContract{
		contextMode: runtimecontract.ContextDeliveryFullPerTurn, snapshot: piControlPlaneCapabilitySnapshot(),
		startOutcome: runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: "native-restored"}, startStarted: started, startRelease: release,
	}
	reopened.runtimeHostDrivers["pi"] = &controlPlaneDriver{acquireHost: &controlPlaneHost{contract: contract, alive: true}}
	reopened.contextHistoryProbe = func(threadID string, _ RuntimeContextEvidenceQuery) (RuntimeContextEvidence, error) {
		return RuntimeContextEvidence{EpochID: "initial:" + threadID}, nil
	}
	view, err := reopened.RestoreAgent(RestoreAgentParams{
		ID: agent.ID, Name: agent.Name, Cwd: t.TempDir(), ThreadID: agent.ThreadID,
		RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "session.jsonl"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.Goal == nil || view.Goal.ID != goal.ID || view.Goal.Version != goal.Version {
		t.Fatalf("stable-ID restore did not reconnect Goal: %#v", view.Goal)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("active restored Goal did not resume")
	}
	time.Sleep(20 * time.Millisecond)
	if contract.startCalls != 1 {
		t.Fatalf("active restored Goal continuations = %d, want 1", contract.startCalls)
	}
	reopened.Shutdown()
}

func TestCodexGoalCommitsBeforeOptionalPausedShadowSync(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.stopping = true
	agent := &Agent{ID: "agent-codex", Name: "codex-worker", ThreadID: "loom-thread", Status: "idle", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "codex", NativeRef: "native-thread"}}
	h.agents[agent.ID] = agent
	contract := &goalSyncContract{controlPlaneContract: &controlPlaneContract{version: runtimecontract.Version}}
	ready := make(chan struct{})
	close(ready)
	h.runtimes[agent.ID] = &runtime{agentID: agent.ID, ready: ready, runtimeContract: contract, binding: runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "codex", NativeRef: "native-thread"}}

	objective := "Own continuation in Loom"
	zero := int64(0)
	goal, err := h.UpdateGoal(agent.ID, GoalUpdateParams{Objective: &objective, ExpectedVersion: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if len(contract.updates) != 1 || contract.updates[0].Status == nil || *contract.updates[0].Status != GoalStatusPaused {
		t.Fatalf("native shadow updates = %#v", contract.updates)
	}
	if goal.NativeSyncState != goalNativeSyncSynced {
		t.Fatalf("sync evidence = %#v", goal)
	}

	contract.err = errors.New("token=secret native sync failed")
	active := GoalStatusActive
	version := goal.Version
	goal, err = h.UpdateGoal(agent.ID, GoalUpdateParams{Status: &active, ExpectedVersion: &version})
	if err != nil {
		t.Fatalf("optional sync rolled back Loom update: %v", err)
	}
	if goal.Status != GoalStatusActive || goal.NativeSyncState != goalNativeSyncFailed || strings.Contains(goal.NativeSyncError, "secret") || !strings.Contains(goal.NativeSyncError, "redacted") {
		t.Fatalf("failed sync evidence = %#v", goal)
	}
	h.mu.Lock()
	continuationReady := h.goalContinuationReadyLocked(agent.ID)
	h.mu.Unlock()
	if !continuationReady {
		t.Fatal("optional native shadow failure blocked Loom-owned continuation")
	}
	var persisted map[string]*ThreadGoal
	if err := st.LoadGoals(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted[agent.ID].Version != goal.Version || persisted[agent.ID].Status != GoalStatusActive {
		t.Fatalf("persisted Loom-first Goal = %#v", persisted[agent.ID])
	}
}

func TestOnlyUnneutralizedLegacyGoalBlocksLoomContinuation(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-codex"] = &Agent{ID: "agent-codex", ThreadID: "thread", Status: "idle", RuntimeBinding: RuntimeBinding{Kind: "codex"}}
	h.goals["agent-codex"] = &ThreadGoal{
		ID: "goal-imported", Version: 1, ThreadID: "thread", Objective: "legacy", Status: GoalStatusActive,
		NativeSyncState: goalNativeSyncFailed, NativeMigrationBlocked: true,
	}
	if h.goalContinuationReadyLocked("agent-codex") {
		t.Fatal("unneutralized legacy native continuation was allowed to race Loom")
	}
	h.goals["agent-codex"].NativeMigrationBlocked = false
	if !h.goalContinuationReadyLocked("agent-codex") {
		t.Fatal("ordinary optional native sync evidence controlled Loom continuation")
	}
}

func TestGoalStartupUsesOneSharedCodexHydrationStarter(t *testing.T) {
	h := testHub(nil)
	h.agents["codex-a"] = &Agent{ID: "codex-a", RuntimeBinding: RuntimeBinding{Kind: "codex"}}
	h.agents["codex-b"] = &Agent{ID: "codex-b", RuntimeBinding: RuntimeBinding{Kind: "codex"}}
	h.agents["pi-active"] = &Agent{ID: "pi-active", RuntimeBinding: RuntimeBinding{Kind: "pi"}}
	h.goals["pi-active"] = &ThreadGoal{ID: "goal-pi", Version: 1, Status: GoalStatusActive}
	if got := h.goalStartupAgentIDs(); len(got) != 2 || got[0] != "codex-a" || got[1] != "pi-active" {
		t.Fatalf("startup IDs = %v, want one Codex hydration starter plus active Pi", got)
	}
	h.goals["codex-b"] = &ThreadGoal{ID: "goal-codex", Version: 1, Status: GoalStatusActive}
	if got := h.goalStartupAgentIDs(); len(got) != 2 || got[0] != "codex-b" || got[1] != "pi-active" {
		t.Fatalf("startup IDs with active Codex = %v, want active Goals only", got)
	}
}

func TestLegacyCodexGoalImportsOnceBeforeNeutralizationAndClearNeverReimports(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.stopping = true
	agent := &Agent{ID: "agent-codex", Name: "worker", ThreadID: "loom-thread", Status: "idle", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "codex", NativeRef: "native-thread"}}
	h.agents[agent.ID] = agent
	target := goalHydrationTarget{agentID: agent.ID, threadID: agent.RuntimeBinding.NativeRef, loomThreadID: agent.ThreadID}
	contract := &goalSyncContract{
		controlPlaneContract: &controlPlaneContract{},
		initial:              &ThreadGoal{ThreadID: "native-thread", Objective: "Legacy active work", Status: GoalStatusActive},
	}
	persistedBeforePause := false
	contract.onUpdate = func() {
		records := map[string]*ThreadGoal{}
		if loadErr := st.LoadGoals(&records); loadErr == nil {
			goal := records[agent.ID]
			persistedBeforePause = goal != nil && goal.ThreadID == agent.ThreadID && goal.NativeMigrationBlocked && goal.NativeSyncState == goalNativeSyncPending
		}
	}

	h.hydrateGoal(target, contract)
	goal, revision, err := h.GetGoalState(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persistedBeforePause || contract.getCalls != 1 || len(contract.updates) != 1 || contract.updates[0].Status == nil || *contract.updates[0].Status != GoalStatusPaused {
		t.Fatalf("import ordering get=%d updates=%#v persistedBeforePause=%v", contract.getCalls, contract.updates, persistedBeforePause)
	}
	if goal == nil || goal.ThreadID != agent.ThreadID || goal.Version != 1 || goal.NativeMigrationBlocked || goal.NativeSyncState != goalNativeSyncSynced {
		t.Fatalf("imported Loom Goal = %#v", goal)
	}

	ready := make(chan struct{})
	close(ready)
	h.runtimes[agent.ID] = &runtime{agentID: agent.ID, ready: ready, runtimeContract: contract, binding: runtimeContractBinding(agent)}
	if cleared, err := h.ClearGoalVersion(agent.ID, &revision); err != nil || !cleared {
		t.Fatalf("clear imported Goal = %v, %v", cleared, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedStore.Close()
	reopened := testHub(reopenedStore)
	reopened.stopping = true
	reopened.agents[agent.ID] = agent
	if err := reopenedStore.LoadGoals(&reopened.goals); err != nil {
		t.Fatal(err)
	}
	afterReopen := &goalSyncContract{controlPlaneContract: &controlPlaneContract{}, initial: contract.initial}
	reopened.hydrateGoal(target, afterReopen)
	if afterReopen.getCalls != 0 || afterReopen.clears != 1 {
		t.Fatalf("cleared import fence queried native again: gets=%d clears=%d", afterReopen.getCalls, afterReopen.clears)
	}
	if reopenedGoal, reopenedRevision, err := reopened.GetGoalState(agent.ID); err != nil || reopenedGoal != nil || reopenedRevision != revision+1 {
		t.Fatalf("reopened cleared Goal=%#v revision=%d err=%v", reopenedGoal, reopenedRevision, err)
	}
}

func TestLegacyMigrationFenceSurvivesFailedClearRecreateAndReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := now()
	agent := &Agent{
		ID: "agent-codex", Name: "worker", ThreadID: "loom-thread", Status: "idle",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "codex", NativeRef: "native-thread"},
		CreatedAt:      stamp, UpdatedAt: stamp,
	}
	if err := st.SaveAgents(map[string]*Agent{agent.ID: agent}); err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.stopping = true
	h.agents[agent.ID] = agent
	target := goalHydrationTarget{agentID: agent.ID, threadID: agent.RuntimeBinding.NativeRef, loomThreadID: agent.ThreadID}
	contract := &goalSyncContract{
		controlPlaneContract: &controlPlaneContract{},
		initial:              &ThreadGoal{ThreadID: agent.RuntimeBinding.NativeRef, Objective: "Legacy active work", Status: GoalStatusActive},
		updateErr:            errors.New("native pause failed"),
	}
	h.hydrateGoal(target, contract)
	imported := h.goals[agent.ID]
	wantBinding := goalBindingRevision(runtimeContractBinding(agent))
	if imported == nil || !imported.NativeMigrationBlocked || imported.NativeMigrationBindingRevision != wantBinding {
		t.Fatalf("failed import did not retain scoped fence: %#v", imported)
	}
	ready := make(chan struct{})
	close(ready)
	h.runtimes[agent.ID] = &runtime{agentID: agent.ID, ready: ready, runtimeContract: contract, binding: runtimeContractBinding(agent)}
	contract.clearErr = errors.New("native clear failed")
	revision := imported.Version
	if cleared, err := h.ClearGoalVersion(agent.ID, &revision); err != nil || !cleared {
		t.Fatalf("Loom clear = %v, %v", cleared, err)
	}
	tombstone := h.goals[agent.ID]
	if !tombstone.NativeMigrationBlocked || tombstone.NativeMigrationBindingRevision != wantBinding || tombstone.NativeSyncState != goalNativeSyncFailed {
		t.Fatalf("failed native clear dropped fence: %#v", tombstone)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedStore.Close()
	reopened := testHub(reopenedStore)
	reopened.stopping = true
	if err := reopenedStore.LoadAgents(&reopened.agents); err != nil {
		t.Fatal(err)
	}
	if err := reopenedStore.LoadGoals(&reopened.goals); err != nil {
		t.Fatal(err)
	}
	reopened.runtimes[agent.ID] = &runtime{agentID: agent.ID, ready: ready, runtimeContract: contract, binding: runtimeContractBinding(agent)}
	contract.clearErr = nil
	revision = tombstone.Version
	objective := "Recreated work"
	recreated, err := reopened.UpdateGoal(agent.ID, GoalUpdateParams{Objective: &objective, ExpectedVersion: &revision})
	if err != nil {
		t.Fatal(err)
	}
	if !recreated.NativeMigrationBlocked || recreated.NativeMigrationBindingRevision != wantBinding || recreated.NativeSyncState != goalNativeSyncFailed {
		t.Fatalf("recreate bypassed legacy fence: %#v", recreated)
	}
	reopened.mu.Lock()
	continuationReady := reopened.goalContinuationReadyLocked(agent.ID)
	reopened.mu.Unlock()
	if continuationReady {
		t.Fatal("Loom continuation raced an unneutralized legacy native loop")
	}

	contract.updateErr = nil
	revision = recreated.Version
	active := GoalStatusActive
	neutralized, err := reopened.UpdateGoal(agent.ID, GoalUpdateParams{Status: &active, ExpectedVersion: &revision})
	if err != nil {
		t.Fatal(err)
	}
	if neutralized.NativeMigrationBlocked || neutralized.NativeSyncState != goalNativeSyncSynced || neutralized.NativeSyncBindingRevision != wantBinding {
		t.Fatalf("successful neutralization did not release fence: %#v", neutralized)
	}
	reopened.mu.Lock()
	continuationReady = reopened.goalContinuationReadyLocked(agent.ID)
	reopened.mu.Unlock()
	if !continuationReady {
		t.Fatal("successful neutralization did not release Loom continuation")
	}
}

func TestLegacyMigrationEvidenceDoesNotCrossRuntimeBinding(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.stopping = true
	agent := &Agent{
		ID: "agent-codex", ThreadID: "loom-thread", Status: "idle",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "codex", NativeRef: "native-new"},
	}
	h.agents[agent.ID] = agent
	oldBinding := runtimecontract.Binding{SchemaVersion: RuntimeBindingSchemaVersion, RuntimeKind: "codex", NativeRef: "native-old"}
	h.goals[agent.ID] = &ThreadGoal{
		ID: "goal-old", Version: 5, ThreadID: agent.ThreadID, Objective: "Old work", Status: GoalStatusActive,
		NativeMigrationBlocked: true, NativeMigrationBindingRevision: goalBindingRevision(oldBinding),
		NativeSyncState: goalNativeSyncSynced, NativeSyncBindingRevision: goalBindingRevision(oldBinding),
	}
	contract := &goalSyncContract{controlPlaneContract: &controlPlaneContract{}, updateErr: errors.New("new optional shadow failed")}
	ready := make(chan struct{})
	close(ready)
	h.runtimes[agent.ID] = &runtime{agentID: agent.ID, ready: ready, runtimeContract: contract, binding: runtimeContractBinding(agent)}
	revision := int64(5)
	active := GoalStatusActive
	updated, err := h.UpdateGoal(agent.ID, GoalUpdateParams{Status: &active, ExpectedVersion: &revision})
	if err != nil {
		t.Fatal(err)
	}
	if updated.NativeMigrationBlocked || updated.NativeSyncState != goalNativeSyncFailed || updated.NativeSyncBindingRevision != goalBindingRevision(runtimeContractBinding(agent)) {
		t.Fatalf("old binding evidence controlled replacement binding: %#v", updated)
	}
	h.mu.Lock()
	readyForLoom := h.goalContinuationReadyLocked(agent.ID)
	h.mu.Unlock()
	if !readyForLoom {
		t.Fatal("irrelevant old binding fence blocked Loom continuation")
	}
}

func TestLoomGoalConformanceAcrossCodexPiAndClaudeStoreDriverRestart(t *testing.T) {
	for _, kind := range []string{"codex", "pi", "claude"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			st, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			stamp := now()
			agent := &Agent{
				ID: "agent-" + kind, Name: "worker-" + kind, Cwd: t.TempDir(), ThreadID: "loom-thread-" + kind,
				RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: kind, NativeRef: "native-" + kind},
				Status:         "idle", CreatedAt: stamp, UpdatedAt: stamp,
			}
			if kind == "claude" {
				agent.RuntimeConfiguration = testClaudeRuntimeConfiguration()
			}
			if err := st.SaveAgents(map[string]*Agent{agent.ID: agent}); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveGoals(map[string]*ThreadGoal{agent.ID: {
				ID: "goal-fence", Version: 1, ThreadID: agent.ThreadID, ClearedAt: time.Now().UnixMilli(), NativeSyncState: goalNativeSyncNotApplicable,
			}}); err != nil {
				t.Fatal(err)
			}

			h, err := Open(st)
			if err != nil {
				t.Fatal(err)
			}
			contract, driver, started := newGoalConformanceRuntime(kind, false)
			h.mu.Lock()
			h.runtimeHostDrivers[kind] = driver
			h.contextHistoryProbe = func(threadID string, _ RuntimeContextEvidenceQuery) (RuntimeContextEvidence, error) {
				return RuntimeContextEvidence{EpochID: "initial:" + threadID}, nil
			}
			h.mu.Unlock()

			objective := "Ship across " + kind
			revision := int64(1)
			created, err := h.UpdateGoal(agent.ID, GoalUpdateParams{Objective: &objective, ExpectedVersion: &revision})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("Goal continuation did not start")
			}
			var delivered strings.Builder
			for _, input := range contract.startRequest.Input {
				delivered.WriteString(input.Text)
			}
			if !strings.Contains(delivered.String(), "loom_agent_goal") || !strings.Contains(delivered.String(), objective) {
				t.Fatalf("Goal context was not delivered: %s", delivered.String())
			}
			paused := GoalStatusPaused
			revision = created.Version
			pausedGoal, err := h.UpdateGoal(agent.ID, GoalUpdateParams{Status: &paused, ExpectedVersion: &revision})
			if err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			if rt := h.runtimes[agent.ID]; rt != nil && rt.activeTurn != nil {
				h.finishTurnLocked(h.agents[agent.ID], rt, "completed", "")
			}
			h.mu.Unlock()
			complete := GoalStatusComplete
			revision = pausedGoal.Version
			completedGoal, err := h.UpdateGoal(agent.ID, GoalUpdateParams{Status: &complete, ExpectedVersion: &revision})
			if err != nil {
				t.Fatal(err)
			}
			revision = completedGoal.Version
			if cleared, err := h.ClearGoalVersion(agent.ID, &revision); err != nil || !cleared {
				t.Fatalf("clear = %v, %v", cleared, err)
			}
			fragments, _, _, _, _, _, err := h.currentContextFragments(agent.ID, now())
			if err != nil {
				t.Fatal(err)
			}
			if fragment := goalContextFragment(fragments); fragment == nil || !strings.Contains(fragment.XML, `cleared="true"`) {
				t.Fatalf("clear tombstone context = %#v", fragment)
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
			reopenedContract, reopenedDriver, reopenedStarted := newGoalConformanceRuntime(kind, kind == "codex")
			reopened.mu.Lock()
			reopened.runtimeHostDrivers[kind] = reopenedDriver
			reopened.contextHistoryProbe = func(threadID string, _ RuntimeContextEvidenceQuery) (RuntimeContextEvidence, error) {
				return RuntimeContextEvidence{EpochID: "initial:" + threadID}, nil
			}
			reopened.mu.Unlock()
			if goal, gotRevision, err := reopened.GetGoalState(agent.ID); err != nil || goal != nil || gotRevision != 5 {
				t.Fatalf("reopened tombstone goal=%#v revision=%d err=%v", goal, gotRevision, err)
			}
			revision = 5
			recreated, err := reopened.UpdateGoal(agent.ID, GoalUpdateParams{Objective: &objective, ExpectedVersion: &revision})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-reopenedStarted:
			case <-time.After(time.Second):
				t.Fatal("recreated Goal continuation did not start")
			}
			if recreated.Version != 6 || (kind == "codex" && (recreated.NativeSyncState != goalNativeSyncFailed || recreated.NativeMigrationBlocked)) || (kind != "codex" && recreated.NativeSyncState != goalNativeSyncNotApplicable) {
				t.Fatalf("recreated Goal = %#v", recreated)
			}
			if reopenedContract.startCalls != 1 {
				t.Fatalf("reopened continuations = %d", reopenedContract.startCalls)
			}
			reopened.Shutdown()
			if err := reopenedStore.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func newGoalConformanceRuntime(kind string, failNativeSync bool) (*controlPlaneContract, *goalConformanceDriver, <-chan struct{}) {
	started := make(chan struct{})
	release := make(chan struct{})
	close(release)
	contract := &controlPlaneContract{
		contextMode:  runtimecontract.ContextDeliveryFullPerTurn,
		startOutcome: runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: "native-turn"},
		startStarted: started, startRelease: release,
	}
	if kind == "codex" {
		contract.contextMode = runtimecontract.ContextDeliveryEpochIncremental
		contract.snapshot = codexControlPlaneCapabilitySnapshot()
		syncContract := &goalSyncContract{controlPlaneContract: contract}
		if failNativeSync {
			syncContract.err = errors.New("optional shadow failed")
		}
		return contract, &goalConformanceDriver{host: &goalConformanceHost{contract: syncContract, alive: true}}, started
	}
	if kind == "claude" {
		contract.snapshot = controlPlaneCapabilitySnapshot("claude")
		for index := range contract.snapshot.Capabilities {
			id := contract.snapshot.Capabilities[index].ID
			if id == runtimecontract.CapabilityContextDelivery || id == runtimecontract.CapabilityGoal {
				contract.snapshot.Capabilities[index] = runtimeCapabilityDescriptor("claude", id, true)
			}
		}
	} else {
		contract.snapshot = piControlPlaneCapabilitySnapshot()
	}
	return contract, &goalConformanceDriver{host: &goalConformanceHost{contract: contract, alive: true}}, started
}

func goalContextFragment(fragments []contextFragment) *contextFragment {
	for index := range fragments {
		if fragments[index].Key == "loom_agent_goal" {
			return &fragments[index]
		}
	}
	return nil
}

func TestGoalContinuationUsesOneOrdinaryTurnAndFencesGoalRevision(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	contract := &controlPlaneContract{
		contextMode:  runtimecontract.ContextDeliveryFullPerTurn,
		startOutcome: runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: "native-turn"},
	}
	agent := &Agent{
		ID: "agent-goal", Name: "goal-worker", Cwd: t.TempDir(), ThreadID: "loom-thread", Status: "idle",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "pi", NativeRef: "pi-session"},
	}
	ready := make(chan struct{})
	close(ready)
	h.agents[agent.ID] = agent
	h.goals[agent.ID] = &ThreadGoal{ID: "goal-1", Version: 1, ThreadID: agent.ThreadID, Objective: "Ship", Status: GoalStatusActive}
	h.runtimes[agent.ID] = &runtime{agentID: agent.ID, ready: ready, runtimeContract: contract, binding: runtimeContractBinding(agent), approvals: map[string]*approval{}}

	h.continueGoal(agent.ID)
	h.continueGoal(agent.ID)
	if contract.startCalls != 1 || !strings.Contains(contract.startRequest.Input[len(contract.startRequest.Input)-1].Text, "loom_agent_goal") {
		t.Fatalf("Goal continuation starts=%d request=%#v", contract.startCalls, contract.startRequest)
	}

	h.mu.Lock()
	h.goals[agent.ID] = &ThreadGoal{ID: "goal-1", Version: 2, ThreadID: agent.ThreadID, Objective: "Ship", Status: GoalStatusPaused}
	h.finishTurnLocked(agent, h.runtimes[agent.ID], "completed", "")
	h.mu.Unlock()
	stale := internalBusinessContext("loom_goal", "goal_continuation", "goal-1", "", "Ship")
	stale.GoalID, stale.GoalVersion, stale.GoalActive = "goal-1", 1, true
	if _, err := h.sendTaskWithContext(agent.ID, "continue", nil, time.Minute, "", "", "", "", "", stale); err == nil || !strings.Contains(err.Error(), "Goal changed") {
		t.Fatalf("stale Goal continuation = %v", err)
	}
	if contract.startCalls != 1 {
		t.Fatalf("stale Goal revision started another Turn: %d", contract.startCalls)
	}
	h.Shutdown()
}

func TestGoalSSEUsesSameRevisionAndHidesClearTombstone(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.stopping = true
	h.agents["agent-pi"] = &Agent{ID: "agent-pi", Name: "pi", ThreadID: "loom-thread", Status: "idle", RuntimeBinding: RuntimeBinding{Kind: "pi"}}
	events, cancel := h.SubscribeGlobal()
	defer cancel()
	objective := "Ship"
	zero := int64(0)
	goal, err := h.UpdateGoal("agent-pi", GoalUpdateParams{Objective: &objective, ExpectedVersion: &zero})
	if err != nil {
		t.Fatal(err)
	}
	created := <-events
	var createdData map[string]any
	if err := json.Unmarshal(created.Data, &createdData); err != nil {
		t.Fatal(err)
	}
	if created.Type != "loom/agent-status" || int64(createdData["goalRevision"].(float64)) != goal.Version || createdData["goal"].(map[string]any)["id"] != goal.ID {
		t.Fatalf("created Goal SSE = %#v %#v", created, createdData)
	}
	version := goal.Version
	if _, err := h.ClearGoalVersion("agent-pi", &version); err != nil {
		t.Fatal(err)
	}
	cleared := <-events
	var clearedData map[string]any
	if err := json.Unmarshal(cleared.Data, &clearedData); err != nil {
		t.Fatal(err)
	}
	if int64(clearedData["goalRevision"].(float64)) != version+1 || clearedData["goal"] != nil {
		t.Fatalf("cleared Goal SSE leaked tombstone = %#v", clearedData)
	}
}
