package hub

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestPiHostDriverAcquiresDistinctPerAgentProcessesAndClosesIndependently(t *testing.T) {
	configureFakePiHubRPC(t, "happy")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	driver := newPiRuntimeHostDriver(h)
	ctx := context.Background()
	first, err := driver.Acquire(ctx, AgentHostRequest{AgentID: "pi-one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := driver.Acquire(ctx, AgentHostRequest{AgentID: "pi-two"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first.Contract() == second.Contract() {
		t.Fatal("two Pi Agents shared one process handle or Contract")
	}
	if first.Alive() || second.Alive() {
		t.Fatal("Acquire must not start Pi before create/resume binds the process")
	}
	firstBinding, firstOutcome := first.Contract().CreateBinding(ctx, runtimecontract.BindingRequest{AgentID: "pi-one", Name: "one", Cwd: t.TempDir()})
	secondBinding, secondOutcome := second.Contract().CreateBinding(ctx, runtimecontract.BindingRequest{AgentID: "pi-two", Name: "two", Cwd: t.TempDir()})
	if firstOutcome.State != runtimecontract.LifecycleAccepted || secondOutcome.State != runtimecontract.LifecycleAccepted {
		t.Fatalf("Pi binding outcomes = %#v / %#v", firstOutcome, secondOutcome)
	}
	if firstBinding.NativeRef == secondBinding.NativeRef || !strings.Contains(firstBinding.NativeRef, filepath.Join("pi", "pi-one")) || !strings.Contains(secondBinding.NativeRef, filepath.Join("pi", "pi-two")) {
		t.Fatalf("Pi bindings did not use distinct fixed session dirs: %#v / %#v", firstBinding, secondBinding)
	}
	if !first.Alive() || !second.Alive() {
		t.Fatalf("distinct Pi processes not alive: first=%v second=%v", first.Alive(), second.Alive())
	}
	first.Close()
	if first.Alive() || !second.Alive() {
		t.Fatal("closing one Pi handle affected the other process")
	}
	if err := driver.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPiHostCloseWaitsForRPCProcessExit(t *testing.T) {
	configureFakePiHubRPC(t, "happy")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	host, err := newPiRuntimeHostDriver(testHub(st)).Acquire(context.Background(), AgentHostRequest{AgentID: "pi-reap"})
	if err != nil {
		t.Fatal(err)
	}
	_, outcome := host.Contract().CreateBinding(context.Background(), runtimecontract.BindingRequest{AgentID: "pi-reap", Name: "reap", Cwd: t.TempDir()})
	if outcome.State != runtimecontract.LifecycleAccepted {
		t.Fatalf("create binding outcome = %#v", outcome)
	}
	rawPID, err := os.ReadFile(os.Getenv("FAKE_PI_PID_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(rawPID))
	if err != nil {
		t.Fatal(err)
	}
	host.Close()
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("Pi RPC pid %d remained after Host close: %v", pid, err)
	}
}

func TestPiResumeMismatchClosesWrongProcessBeforeRetry(t *testing.T) {
	configureFakePiHubRPC(t, "resume-mismatch-once")
	runtime := newPiAgentRuntime("pi-resume", t.TempDir(), "")
	defer runtime.Close()
	requested := filepath.Join(runtime.dataDir, "pi", runtime.agentID, "session-pi-resume.jsonl")
	request := nativeBindingRequest{NativeRef: requested, Name: "resume", Cwd: t.TempDir()}
	if err := runtime.Resume(request, time.Second); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("first mismatched resume error = %v", err)
	}
	if runtime.Alive() {
		t.Fatal("mismatched Pi resume left wrong process alive")
	}
	if err := runtime.Resume(request, time.Second); err != nil {
		t.Fatalf("retry requested Pi session: %v", err)
	}
	if !runtime.Alive() {
		t.Fatal("correct Pi retry process is not alive")
	}
	starts, err := os.ReadFile(os.Getenv("FAKE_PI_STARTS_FILE"))
	if err != nil || strings.Count(string(starts), "start\n") != 2 {
		t.Fatalf("Pi resume starts = %q, err=%v", starts, err)
	}
}

func TestPiRuntimeContractV2MapsCanonicalEventsAndRestartSafeHistory(t *testing.T) {
	configureFakePiHubRPC(t, "settle-before-entries")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	driver := newPiRuntimeHostDriver(testHub(st))
	host, err := driver.Acquire(context.Background(), AgentHostRequest{AgentID: "pi-contract"})
	if err != nil {
		t.Fatal(err)
	}
	contract := host.Contract()
	if contract.ContractVersion() != runtimecontract.Version {
		t.Fatalf("Contract version = %d", contract.ContractVersion())
	}
	binding, outcome := contract.CreateBinding(context.Background(), runtimecontract.BindingRequest{AgentID: "pi-contract", Name: "Pi Contract", Cwd: t.TempDir()})
	if outcome.State != runtimecontract.LifecycleAccepted || binding.RuntimeKind != "pi" {
		t.Fatalf("create binding = %#v / %#v", binding, outcome)
	}
	events := make(chan runtimecontract.Event, 32)
	contract.SetEventHandler(func(event runtimecontract.Event) { events <- event })
	turnOutcome := contract.StartTurn(context.Background(), runtimecontract.TurnRequest{
		Binding: binding, TurnID: "turn-loom-pi", Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: "hello v2"}},
	})
	if turnOutcome.State != runtimecontract.LifecycleAccepted || turnOutcome.RuntimeTurnRef != "user-1" {
		t.Fatalf("start Turn outcome = %#v", turnOutcome)
	}
	deadline := time.After(3 * time.Second)
	terminal, started := 0, 0
	for terminal == 0 {
		select {
		case event := <-events:
			if event.TurnID != "turn-loom-pi" {
				t.Fatalf("canonical Pi event TurnID = %q: %#v", event.TurnID, event)
			}
			if event.Kind == runtimecontract.EventTurnStarted {
				started++
			}
			if event.Kind == runtimecontract.EventTerminal {
				terminal++
			}
		case <-deadline:
			t.Fatal("Pi Contract emitted no terminal event")
		}
	}
	if started != 1 || terminal != 1 {
		t.Fatalf("event-before-response lifecycle counts started=%d terminal=%d", started, terminal)
	}
	history, failure := contract.ReadHistory(context.Background(), runtimecontract.HistoryRequest{Binding: binding, Count: 10})
	if failure != nil || len(history.Turns) != 1 || history.Turns[0].TurnID != "turn-loom-pi" || history.Turns[0].RuntimeTurnRef != "user-1" {
		t.Fatalf("canonical Pi history = %#v, failure=%#v", history, failure)
	}
	encoded, _ := json.Marshal(history)
	if strings.Contains(string(encoded), "user-1") || strings.Contains(string(encoded), binding.NativeRef) {
		t.Fatalf("canonical Pi history leaked native identity: %s", encoded)
	}
	host.Close()
	if host.Alive() {
		t.Fatal("Pi Contract close left process alive")
	}
}

func TestPiRuntimeContractReportsUnknownPromptAcceptanceAsIndeterminate(t *testing.T) {
	configureFakePiHubRPC(t, "accepted-no-entry")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	host, err := newPiRuntimeHostDriver(testHub(st)).Acquire(context.Background(), AgentHostRequest{AgentID: "pi-unknown"})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	binding, outcome := host.Contract().CreateBinding(context.Background(), runtimecontract.BindingRequest{AgentID: "pi-unknown", Name: "unknown", Cwd: t.TempDir()})
	if outcome.State != runtimecontract.LifecycleAccepted {
		t.Fatalf("create binding outcome = %#v", outcome)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	outcome = host.Contract().StartTurn(ctx, runtimecontract.TurnRequest{
		Binding: binding, TurnID: "turn-unknown", Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: "maybe accepted"}},
	})
	if outcome.State != runtimecontract.LifecycleIndeterminate || outcome.Failure == nil || outcome.Failure.Code != "prompt_acceptance_unknown" {
		t.Fatalf("unknown prompt acceptance outcome = %#v", outcome)
	}
}

func TestPiRuntimeContractV2ContinuesAndInterruptsExactTurn(t *testing.T) {
	t.Run("continue", func(t *testing.T) {
		configureFakePiHubRPC(t, "steer")
		contract, binding, closeHost := newTestPiContract(t, "pi-v2-steer")
		defer closeHost()
		started := contract.StartTurn(context.Background(), runtimecontract.TurnRequest{
			Binding: binding, TurnID: "turn-v2-steer", Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: "start"}},
		})
		if started.State != runtimecontract.LifecycleAccepted {
			t.Fatalf("start outcome = %#v", started)
		}
		continued := contract.ContinueTurn(context.Background(), runtimecontract.CausalInput{
			Binding: binding, TurnID: "turn-v2-steer", RuntimeTurnRef: started.RuntimeTurnRef,
			Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: "continue causally"}},
		})
		if continued.State != runtimecontract.LifecycleAccepted || continued.RuntimeTurnRef != started.RuntimeTurnRef {
			t.Fatalf("continue outcome = %#v", continued)
		}
	})
	t.Run("interrupt", func(t *testing.T) {
		configureFakePiHubRPC(t, "abort")
		contract, binding, closeHost := newTestPiContract(t, "pi-v2-abort")
		defer closeHost()
		started := contract.StartTurn(context.Background(), runtimecontract.TurnRequest{
			Binding: binding, TurnID: "turn-v2-abort", Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: "start"}},
		})
		if started.State != runtimecontract.LifecycleAccepted {
			t.Fatalf("start outcome = %#v", started)
		}
		interrupted := contract.InterruptTurn(context.Background(), runtimecontract.TurnTarget{
			Binding: binding, TurnID: "turn-v2-abort", RuntimeTurnRef: started.RuntimeTurnRef,
		})
		if interrupted.State != runtimecontract.LifecycleInterrupted || interrupted.RuntimeTurnRef != started.RuntimeTurnRef {
			t.Fatalf("interrupt outcome = %#v", interrupted)
		}
	})
}

func newTestPiContract(t *testing.T, agentID string) (runtimecontract.Contract, runtimecontract.Binding, func()) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	driver := newPiRuntimeHostDriver(testHub(st))
	host, err := driver.Acquire(context.Background(), AgentHostRequest{AgentID: agentID})
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	binding, outcome := host.Contract().CreateBinding(context.Background(), runtimecontract.BindingRequest{AgentID: agentID, Name: agentID, Cwd: t.TempDir()})
	if outcome.State != runtimecontract.LifecycleAccepted {
		host.Close()
		_ = st.Close()
		t.Fatalf("create binding outcome = %#v", outcome)
	}
	return host.Contract(), binding, func() { host.Close(); _ = st.Close() }
}

func TestPiDriverPreflightFailsBeforeCreatingAgentSessionDirectory(t *testing.T) {
	dataDir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_BIN", bin)
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	_, err = h.CreateAgent(CreateParams{Name: "pi-preflight", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("Pi preflight error = %v", err)
	}
	h.mu.Lock()
	agents, runtimes, seqs := len(h.agents), len(h.runtimes), len(h.seqs)
	h.mu.Unlock()
	if agents != 0 || runtimes != 0 || seqs != 0 {
		t.Fatalf("preflight failure remained in memory: agents=%d runtimes=%d seqs=%d", agents, runtimes, seqs)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "pi")); !os.IsNotExist(statErr) {
		t.Fatalf("preflight created Pi session directory: %v", statErr)
	}
}

func TestPiCreatePersistenceFailureClosesStartedProcessWithoutDeletingSessionEvidence(t *testing.T) {
	configureFakePiHubRPC(t, "happy")
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// SaveAgents writes the compatibility mirror first. A directory at that
	// exact path deterministically fails after Pi has created its native binding.
	if err := os.Mkdir(filepath.Join(dataDir, "sessions.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.stop = make(chan struct{})
	defer h.Shutdown()

	_, err = h.CreateAgent(CreateParams{Name: "pi-rollback", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err == nil || !strings.Contains(err.Error(), "persist started Runtime binding") {
		t.Fatalf("CreateAgent error = %v, want binding persistence failure", err)
	}
	h.mu.Lock()
	if len(h.agents) != 0 || len(h.runtimes) != 0 || len(h.seqs) != 0 {
		t.Fatalf("failed Pi create remained live: agents=%d runtimes=%d seqs=%d", len(h.agents), len(h.runtimes), len(h.seqs))
	}
	driver := h.piHostDriver
	h.mu.Unlock()
	if driver == nil || driver.aliveCount() != 0 {
		t.Fatalf("Pi process survived failed binding persistence: driver=%#v", driver)
	}
	rawRolledBackPID, readPIDErr := os.ReadFile(os.Getenv("FAKE_PI_PID_FILE"))
	rolledBackPID, parsePIDErr := strconv.Atoi(string(rawRolledBackPID))
	if readPIDErr != nil || parsePIDErr != nil {
		t.Fatalf("read rolled-back Pi pid: raw=%q read=%v parse=%v", rawRolledBackPID, readPIDErr, parsePIDErr)
	}
	if err := syscall.Kill(rolledBackPID, 0); err != syscall.ESRCH {
		t.Fatalf("rolled-back Pi pid %d was not reaped: %v", rolledBackPID, err)
	}
	entries, globErr := filepath.Glob(filepath.Join(dataDir, "pi", "*", "session-*.jsonl"))
	if globErr != nil || len(entries) != 1 {
		t.Fatalf("recoverable Pi session evidence = %v, err=%v", entries, globErr)
	}
	if err := os.Remove(filepath.Join(dataDir, "sessions.json")); err != nil {
		t.Fatal(err)
	}
	retried, err := h.CreateAgent(CreateParams{Name: "pi-rollback", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	h.mu.Lock()
	agents, runtimes, seqs := len(h.agents), len(h.runtimes), len(h.seqs)
	h.mu.Unlock()
	if agents != 1 || runtimes != 1 || seqs != 1 || driver.aliveCount() != 1 || retried.ID == "" {
		t.Fatalf("retry state agents=%d runtimes=%d seqs=%d alive=%d agent=%#v", agents, runtimes, seqs, driver.aliveCount(), retried)
	}
	starts, readErr := os.ReadFile(os.Getenv("FAKE_PI_STARTS_FILE"))
	if readErr != nil || strings.Count(string(starts), "start\n") != 2 {
		t.Fatalf("Pi retry starts = %q, err=%v", starts, readErr)
	}
}

func TestPiArchiveReleasesOnlyThatAgentProcessAndShutdownClosesTheRest(t *testing.T) {
	configureFakePiHubRPC(t, "happy")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	h.stop = make(chan struct{})
	first, err := h.CreateAgent(CreateParams{Name: "pi-archive-one", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.CreateAgent(CreateParams{Name: "pi-archive-two", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	firstHost := h.runtimes[first.ID].agentHost
	secondHost := h.runtimes[second.ID].agentHost
	driver := h.piHostDriver
	h.mu.Unlock()
	if driver.aliveCount() != 2 {
		t.Fatalf("Pi processes before archive = %d", driver.aliveCount())
	}
	if _, err := h.ArchiveAgent(first.ID); err != nil {
		t.Fatal(err)
	}
	if firstHost.Alive() || !secondHost.Alive() || driver.aliveCount() != 1 {
		t.Fatalf("archive isolation first=%v second=%v alive=%d", firstHost.Alive(), secondHost.Alive(), driver.aliveCount())
	}
	h.Shutdown()
	h.Shutdown()
	if secondHost.Alive() || driver.aliveCount() != 0 {
		t.Fatalf("idempotent shutdown left Pi process alive: second=%v alive=%d", secondHost.Alive(), driver.aliveCount())
	}
}
