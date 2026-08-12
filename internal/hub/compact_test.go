package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestCompactAgentThreadStartsCodexCompaction(t *testing.T) {
	logPath := installFakeSharedCodexHost(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	defer h.Shutdown()
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", Cwd: "/tmp/stale", ThreadID: "loom-thr-stale", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thr-compact"},
		Sandbox: "danger-full-access", ApprovalPolicy: "never", Status: "idle",
		CreatedAt: now(), UpdatedAt: now(),
	}
	if err := h.persistAgentsLocked(); err != nil {
		t.Fatal(err)
	}

	result, err := h.CompactAgentThread("agent-1")
	if err != nil {
		t.Fatalf("CompactAgentThread: %v", err)
	}
	if !result.Started || result.ThreadID != "loom-thr-stale" || result.AgentName != "worker" {
		t.Fatalf("compact result = %#v", result)
	}
	h.workers.Wait()
	view, err := h.GetAgent("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if operation := view.ContextMaintenance; operation == nil || operation.ID != result.Operation.ID || operation.State != "completed" || operation.Origin != "owner" {
		t.Fatalf("context maintenance = %#v, result = %#v", operation, result)
	}
	if view.LastTurn != nil || view.CurrentTurnID != "" {
		t.Fatalf("synthetic Codex compaction Turn became public: %#v", view)
	}
	events, err := h.ReadEvents("agent-1", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	states := []string{}
	for _, event := range events {
		if event.Type != "loom/context-maintenance" {
			continue
		}
		var operation ContextMaintenanceOperation
		if json.Unmarshal(event.Data, &operation) != nil {
			t.Fatalf("context maintenance event = %s", event.Data)
		}
		states = append(states, operation.State)
		if strings.Contains(string(event.Data), "thr-compact") {
			t.Fatalf("native Runtime identity leaked: %s", event.Data)
		}
	}
	if strings.Join(states, ",") != "started,completed" {
		t.Fatalf("context maintenance states = %v", states)
	}
	compact := lastRequestParams(t, logPath, "thread/compact/start")
	if compact["threadId"] != "thr-compact" {
		t.Fatalf("thread/compact/start params = %#v", compact)
	}
	if got := countRequestMethod(t, logPath, "thread/compact/start"); got != 1 {
		t.Fatalf("thread/compact/start requests = %d, want 1", got)
	}
}

func TestCodexContextMaintenanceIgnoresOrdinaryRemoteTurn(t *testing.T) {
	waiter := &codexContextMaintenanceWaiter{bindingRef: "thr-ordinary", result: make(chan runtimecontract.Outcome, 1)}
	contract := &codexRuntimeContract{contextMaintenance: waiter}
	contract.handleContextMaintenanceEvent("turn/started", json.RawMessage(`{"threadId":"thr-ordinary","turn":{"id":"turn-ordinary","status":"inProgress"}}`))
	contract.handleContextMaintenanceEvent("turn/completed", json.RawMessage(`{"threadId":"thr-ordinary","turn":{"id":"turn-ordinary","status":"completed"}}`))
	if waiter.nativeTurnID != "" {
		t.Fatalf("ordinary Remote Turn was correlated as context maintenance: %q", waiter.nativeTurnID)
	}
	select {
	case outcome := <-waiter.result:
		t.Fatalf("ordinary Remote Turn completed context maintenance: %#v", outcome)
	default:
	}

	contract.handleContextMaintenanceEvent("item/started", json.RawMessage(`{"threadId":"thr-ordinary","turnId":"turn-compact","item":{"id":"item-compact","type":"contextCompaction"}}`))
	contract.handleContextMaintenanceEvent("turn/completed", json.RawMessage(`{"threadId":"thr-ordinary","turn":{"id":"turn-compact","status":"completed"}}`))
	select {
	case outcome := <-waiter.result:
		if outcome.State != runtimecontract.LifecycleCompleted {
			t.Fatalf("context compaction outcome = %#v", outcome)
		}
	default:
		t.Fatal("contextCompaction item did not correlate its terminal Turn")
	}
}

func writePiMaintenanceFixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "pi-maintenance.jsonl")
	content := `{"type":"session","version":3,"id":"pi-maintenance","timestamp":"2026-08-10T01:00:00Z","cwd":"/tmp/work"}` + "\n" +
		`{"type":"message","id":"user-1","parentId":null,"timestamp":"2026-08-10T01:00:01Z","message":{"role":"user","content":[{"type":"text","text":"preserve me"}]}}` + "\n" +
		`{"type":"message","id":"assistant-1","parentId":"user-1","timestamp":"2026-08-10T01:00:02Z","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"kept"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompactAgentThreadUsesPiNativeMaintenance(t *testing.T) {
	configureFakePiHubRPC(t, "happy")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	defer h.Shutdown()
	session := writePiMaintenanceFixture(t, st.Dir())
	h.agents["agent-pi"] = &Agent{
		ID: "agent-pi", Name: "pi-worker", Cwd: "/tmp/work", ThreadID: "loom-pi",
		RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: session}, Status: "idle", CreatedAt: now(), UpdatedAt: now(),
	}
	if err := h.persistAgentsLocked(); err != nil {
		t.Fatal(err)
	}

	result, err := h.CompactAgentThread("agent-pi")
	if err != nil {
		t.Fatalf("CompactAgentThread: %v", err)
	}
	if !result.Started || result.Operation.State != "started" {
		t.Fatalf("compact result = %#v", result)
	}
	h.workers.Wait()
	view, err := h.GetAgent("agent-pi")
	if err != nil {
		t.Fatal(err)
	}
	if view.ContextMaintenance == nil || view.ContextMaintenance.State != "completed" {
		t.Fatalf("Pi context maintenance = %#v", view.ContextMaintenance)
	}
	history, err := h.CanonicalHistory("agent-pi", 20, 0)
	if err != nil || history.Total != 1 {
		t.Fatalf("Pi history after maintenance = %#v, err=%v", history, err)
	}
}

func TestContextMaintenanceReopenInspectsWithoutRepeatingNativeMutation(t *testing.T) {
	for _, test := range []struct {
		name, wantState string
		changedEvidence bool
	}{
		{name: "unchanged evidence remains indeterminate", wantState: "indeterminate"},
		{name: "changed evidence proves completion", wantState: "completed", changedEvidence: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			logPath := installFakeSharedCodexHost(t)
			rolloutDir := t.TempDir()
			writeTestRollout(t, rolloutDir, "thr-reconcile", now())
			if test.changedEvidence {
				paths, err := filepath.Glob(filepath.Join(rolloutDir, "*", "*", "*", "*.jsonl"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("find rollout: paths=%v err=%v", paths, err)
				}
				file, err := os.OpenFile(paths[0], os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString(`{"timestamp":"2026-08-12T00:01:00Z","type":"compacted","payload":{"window_number":2,"window_id":"after-maintenance"}}` + "\n")
				if closeErr := file.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CODEX_SESSIONS_DIR", rolloutDir)
			dataDir := t.TempDir()
			st, err := store.Open(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			agent := &Agent{
				ID: "agent-reconcile", Name: "reconcile", Cwd: "/tmp/stale", ThreadID: "loom-reconcile",
				RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "codex", NativeRef: "thr-reconcile"},
				Status:         "idle", CreatedAt: now(), UpdatedAt: now(),
				ContextMaintenance: &ContextMaintenanceOperation{
					ID: "cmop-reconcile", AgentID: "agent-reconcile", ThreadID: "loom-reconcile", Origin: "owner", State: "started",
					StartedAt: now(), BaselineRevision: contextMaintenanceRevisionHash("initial:thr-reconcile\x000\x00"), BindingRevision: "binding:test",
				},
			}
			if err := st.SaveAgents(map[string]*Agent{agent.ID: agent}); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = store.Open(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			h, err := Open(st)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Shutdown()
			h.workers.Wait()
			view, err := h.GetAgent(agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if view.ContextMaintenance == nil || view.ContextMaintenance.State != test.wantState {
				t.Fatalf("reconciled context maintenance = %#v", view.ContextMaintenance)
			}
			if _, statErr := os.Stat(logPath); statErr == nil {
				if got := countRequestMethod(t, logPath, "thread/compact/start"); got != 0 {
					t.Fatalf("reopen repeated native context maintenance %d times", got)
				}
				if got := countRequestMethod(t, logPath, "thread/resume"); got != 0 {
					t.Fatalf("reopen used active Runtime control instead of passive evidence %d times", got)
				}
			} else if !os.IsNotExist(statErr) {
				t.Fatal(statErr)
			}
		})
	}
}

func TestCompactAgentThreadRejectsActiveTurnAndGoal(t *testing.T) {
	installFakeSharedCodexHost(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	defer h.Shutdown()
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "loom-thr-stale", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thr-stale"}, Status: "idle",
	}

	h.runtimes["agent-1"] = &runtime{agentID: "agent-1", activeTurn: &turnState{finished: false}}
	_, err = h.CompactAgentThread("agent-1")
	if err == nil || !strings.Contains(err.Error(), "active Turn") {
		t.Fatalf("active Turn compact error = %v", err)
	}
	h.runtimes["agent-1"].activeTurn = nil
	h.runtimes["agent-1"].approvals = map[string]*approval{"ap-1": {}}
	_, err = h.CompactAgentThread("agent-1")
	if err == nil || !strings.Contains(err.Error(), "pending approval") {
		t.Fatalf("pending approval compact error = %v", err)
	}
	h.runtimes["agent-1"].approvals = nil

	h.goals["agent-1"] = &ThreadGoal{ThreadID: "thr-stale", Status: GoalStatusActive}
	_, err = h.CompactAgentThread("agent-1")
	if err == nil || !strings.Contains(err.Error(), "active Goal") {
		t.Fatalf("active Goal compact error = %v", err)
	}
}

func TestContextMaintenanceFencesRuntimeMutationsButNotReads(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	defer h.Shutdown()
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", Cwd: "/tmp/work", ThreadID: "loom-thread",
		RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "native-thread"}, Status: "idle",
		CreatedAt: now(), UpdatedAt: now(), ContextMaintenance: &ContextMaintenanceOperation{State: contextMaintenanceStarted},
	}

	if _, err := h.GetAgent("agent-1"); err != nil {
		t.Fatalf("GetAgent while maintenance runs: %v", err)
	}
	newName := "renamed"
	objective := "keep working"
	mutations := map[string]func() error{
		"Turn start": func() error {
			_, err := h.SendTask("agent-1", "do work", time.Second)
			return err
		},
		"Agent config": func() error {
			_, err := h.UpdateAgentConfig("agent-1", ConfigParams{Name: &newName})
			return err
		},
		"Goal update": func() error {
			_, err := h.UpdateGoal("agent-1", GoalUpdateParams{Objective: &objective})
			return err
		},
		"Goal clear": func() error {
			_, err := h.ClearGoal("agent-1")
			return err
		},
		"archive": func() error {
			_, err := h.ArchiveAgent("agent-1")
			return err
		},
	}
	for name, mutate := range mutations {
		if err := mutate(); err == nil || !strings.Contains(err.Error(), "context maintenance") {
			t.Errorf("%s error = %v", name, err)
		}
	}
}

func TestArchiveRechecksContextMaintenanceAfterRuntimeSerialization(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	defer h.Shutdown()
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", Cwd: "/tmp/work", ThreadID: "loom-thread",
		RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "native-agent-1"}, Status: "idle", CreatedAt: now(), UpdatedAt: now(),
	}
	h.runtimes["agent-1"] = testRuntime("agent-1", "pi", &controlPlaneContract{}, nil)
	locked := make(chan struct{})
	release := make(chan struct{})
	h.archiveStartLockForTest = func(string) {
		close(locked)
		<-release
	}
	result := make(chan error, 1)
	go func() {
		_, err := h.ArchiveAgent("agent-1")
		result <- err
	}()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("archive did not reach the serialized Runtime boundary")
	}
	h.mu.Lock()
	h.agents["agent-1"].ContextMaintenance = &ContextMaintenanceOperation{State: contextMaintenanceStarted}
	h.mu.Unlock()
	close(release)
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "context maintenance") {
			t.Fatalf("ArchiveAgent error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ArchiveAgent did not leave the serialized Runtime boundary")
	}
	if _, err := h.GetAgent("agent-1"); err != nil {
		t.Fatalf("Agent was archived despite started context maintenance: %v", err)
	}
}

func TestSendRechecksContextMaintenanceAfterRuntimeSerialization(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	defer h.Shutdown()
	contract := &controlPlaneContract{resumeStarted: make(chan struct{}), resumeRelease: make(chan struct{})}
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", Cwd: "/tmp/work", ThreadID: "loom-thread",
		RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "native-agent-1"}, Status: "idle", CreatedAt: now(), UpdatedAt: now(),
	}
	h.runtimes["agent-1"] = testRuntime("agent-1", "pi", contract, nil)
	result := make(chan error, 1)
	go func() {
		_, err := h.SendTask("agent-1", "do work", time.Second)
		result <- err
	}()
	select {
	case <-contract.resumeStarted:
	case <-time.After(time.Second):
		t.Fatal("SendTask did not reach serialized Runtime resume")
	}
	h.mu.Lock()
	h.agents["agent-1"].ContextMaintenance = &ContextMaintenanceOperation{State: contextMaintenanceStarted}
	h.mu.Unlock()
	close(contract.resumeRelease)
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "context maintenance") {
			t.Fatalf("SendTask error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SendTask did not leave serialized Runtime resume")
	}
	if contract.startCalls != 0 {
		t.Fatalf("StartTurn calls = %d, want zero", contract.startCalls)
	}
}

func TestContextMaintenancePersistsCanonicalTerminalOutcomes(t *testing.T) {
	failed := func(state runtimecontract.LifecycleState) runtimecontract.Outcome {
		return runtimecontract.Outcome{State: state, Failure: &runtimecontract.Failure{
			Code: "fixture_failure", Phase: runtimecontract.FailurePhaseContextMaintenance, Message: "safe failure",
		}}
	}
	for _, outcome := range []runtimecontract.Outcome{
		{State: runtimecontract.LifecycleInterrupted},
		failed(runtimecontract.LifecycleFailed),
		failed(runtimecontract.LifecycleIndeterminate),
	} {
		t.Run(string(outcome.State), func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			h := testHub(st)
			defer h.Shutdown()
			contract := &controlPlaneContract{maintenanceOutcome: outcome}
			h.agents["agent-1"] = &Agent{
				ID: "agent-1", Name: "worker", Cwd: "/tmp/work", ThreadID: "loom-thread",
				RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "native-agent-1"}, Status: "idle", CreatedAt: now(), UpdatedAt: now(),
			}
			h.runtimes["agent-1"] = testRuntime("agent-1", "pi", contract, nil)
			if err := h.persistAgentsLocked(); err != nil {
				t.Fatal(err)
			}

			result, err := h.CompactAgentThread("agent-1")
			if err != nil {
				t.Fatal(err)
			}
			h.workers.Wait()
			view, err := h.GetAgent("agent-1")
			if err != nil {
				t.Fatal(err)
			}
			if view.ContextMaintenance == nil || view.ContextMaintenance.ID != result.Operation.ID || view.ContextMaintenance.State != string(outcome.State) {
				t.Fatalf("context maintenance = %#v", view.ContextMaintenance)
			}
			if (outcome.Failure != nil) != (view.ContextMaintenance.Error != "") {
				t.Fatalf("safe failure projection = %#v", view.ContextMaintenance)
			}
		})
	}
}
