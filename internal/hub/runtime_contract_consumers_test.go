package hub

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

type controlPlaneContract struct {
	createBinding    runtimecontract.Binding
	createOutcome    runtimecontract.Outcome
	createCalls      int
	version          int
	archiveOutcome   runtimecontract.Outcome
	archiveCalls     int
	archiveBinding   runtimecontract.Binding
	closeCalls       int
	closeBinding     runtimecontract.Binding
	continueOutcome  runtimecontract.Outcome
	continueSet      bool
	continueCalls    int
	continueRequest  runtimecontract.CausalInput
	interruptOutcome runtimecontract.Outcome
	interruptCalls   int
	interruptRequest runtimecontract.TurnTarget
	callOrder        []string
	contextMode      runtimecontract.ContextDeliveryMode
	startOutcome     runtimecontract.Outcome
	startCalls       int
	startRequest     runtimecontract.TurnRequest
	resumeStarted    chan struct{}
	resumeRelease    chan struct{}
	resumeOutcome    runtimecontract.Outcome
	startStarted     chan struct{}
	startRelease     chan struct{}
	closeOutcome     runtimecontract.Outcome
	nameOutcome      runtimecontract.Outcome
	nameCalls        int
	nameBinding      runtimecontract.Binding
	name             string
	snapshot         runtimecontract.CapabilitySnapshot
	snapshotHook     func()
	history          runtimecontract.History
	historyFailure   *runtimecontract.Failure
	eventHandler     func(runtimecontract.Event)
}

func (c *controlPlaneContract) SetApprovalHandler(func(runtimecontract.ApprovalProposal)) {}
func (c *controlPlaneContract) ResolveApproval(context.Context, string, runtimecontract.ApprovalDecision) error {
	return nil
}

type controlPlaneHost struct {
	contract *controlPlaneContract
	alive    bool
}

func (h *controlPlaneHost) Alive() bool                        { return h.alive }
func (h *controlPlaneHost) Contract() runtimecontract.Contract { return h.contract }
func (h *controlPlaneHost) SetFailureHandler(func(error))      {}
func (h *controlPlaneHost) Close()                             { h.alive = false }

type controlPlaneDriver struct {
	hosts                []*controlPlaneHost
	acquireHost          AgentHost
	shutdownCalls        int
	closedBeforeShutdown bool
	snapshot             runtimecontract.CapabilitySnapshot
	historyContract      runtimecontract.Contract
}

type blockingAcquireDriver struct {
	started chan struct{}
	release chan struct{}
	host    AgentHost
	start   sync.Once
	calls   atomic.Int32
}

func (d *blockingAcquireDriver) Preflight(context.Context) error { return nil }
func (d *blockingAcquireDriver) Acquire(context.Context, AgentHostRequest) (AgentHost, error) {
	d.calls.Add(1)
	d.start.Do(func() { close(d.started) })
	<-d.release
	return d.host, nil
}
func (d *blockingAcquireDriver) Shutdown(context.Context) error { return nil }

func (d *controlPlaneDriver) Preflight(context.Context) error { return nil }
func (d *controlPlaneDriver) Acquire(context.Context, AgentHostRequest) (AgentHost, error) {
	if d.acquireHost != nil {
		return d.acquireHost, nil
	}
	return nil, errors.New("unexpected acquire")
}
func (d *controlPlaneDriver) CapabilitySnapshot(context.Context, runtimecontract.Binding) runtimecontract.CapabilitySnapshot {
	return d.snapshot
}
func (d *controlPlaneDriver) HistoryContract(AgentHostRequest) runtimecontract.Contract {
	return d.historyContract
}
func (d *controlPlaneDriver) Shutdown(context.Context) error {
	d.shutdownCalls++
	d.closedBeforeShutdown = true
	for _, host := range d.hosts {
		if host.contract.closeCalls == 0 {
			d.closedBeforeShutdown = false
		}
		host.Close()
	}
	return nil
}

func TestRuntimeAcquireDoesNotFreezeAgentReads(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	contract := &controlPlaneContract{createBinding: runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "pi", NativeRef: "native-blocking"}}
	driver := &blockingAcquireDriver{
		started: make(chan struct{}), release: make(chan struct{}),
		host: &controlPlaneHost{contract: contract, alive: true},
	}
	h.runtimeHostDrivers["pi"] = driver
	created := make(chan error, 1)
	go func() {
		_, err := h.CreateAgent(CreateParams{Name: "blocked-acquire", Cwd: t.TempDir(), RuntimeKind: "pi"})
		created <- err
	}()
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("Runtime acquire did not start")
	}
	listed := make(chan struct{})
	go func() {
		h.ListAgents()
		close(listed)
	}()
	select {
	case <-listed:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("ListAgents blocked behind Runtime host acquisition")
	}
	close(driver.release)
	if err := <-created; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentRuntimeAcquireInstallsOneLiveHost(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	defer h.Shutdown()
	contract := &controlPlaneContract{createBinding: runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "pi", NativeRef: "native-concurrent"}}
	driver := &blockingAcquireDriver{
		started: make(chan struct{}), release: make(chan struct{}),
		host: &controlPlaneHost{contract: contract, alive: true},
	}
	h.runtimeHostDrivers["pi"] = driver
	meta := &Agent{ID: "agent-concurrent", Name: "concurrent", Cwd: t.TempDir(), ThreadID: "thread-concurrent", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "pi"}, Status: "idle"}
	h.agents[meta.ID] = meta
	h.seqs[meta.ID] = 0
	type result struct {
		rt  *runtime
		err error
	}
	acquire := func(ch chan<- result) {
		h.mu.Lock()
		rt, err := h.getRuntimeLocked(meta)
		h.mu.Unlock()
		ch <- result{rt: rt, err: err}
	}
	first := make(chan result, 1)
	go acquire(first)
	<-driver.started
	second := make(chan result, 1)
	go acquire(second)
	secondResult := <-second
	if secondResult.err != nil || secondResult.rt == nil {
		t.Fatalf("second acquire = %#v", secondResult)
	}
	close(driver.release)
	firstResult := <-first
	if firstResult.err != nil || firstResult.rt != secondResult.rt {
		t.Fatalf("concurrent acquires = first %#v second %#v", firstResult, secondResult)
	}
	if err := waitReady(firstResult.rt); err != nil {
		t.Fatal(err)
	}
	if driver.calls.Load() != 1 || !firstResult.rt.agentHost.Alive() {
		t.Fatalf("Driver calls=%d Host alive=%v", driver.calls.Load(), firstResult.rt.agentHost.Alive())
	}
}

func TestRuntimeContractRejectsWrongVersionOrBindingBeforePersistence(t *testing.T) {
	tests := []struct {
		name     string
		version  int
		binding  runtimecontract.Binding
		wantText string
	}{
		{name: "Contract version", version: runtimecontract.Version + 1, wantText: "Contract version"},
		{name: "binding schema", binding: runtimecontract.Binding{SchemaVersion: 1, RuntimeKind: "pi", NativeRef: "native"}, wantText: "schema version"},
		{name: "binding kind", binding: runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "codex", NativeRef: "native"}, wantText: "binding kind"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			h := testHub(st)
			contract := &controlPlaneContract{version: test.version, createBinding: test.binding}
			host := &controlPlaneHost{contract: contract, alive: true}
			h.runtimeHostDrivers["pi"] = &controlPlaneDriver{acquireHost: host}
			_, err = h.CreateAgent(CreateParams{Name: "invalid-contract", Cwd: t.TempDir(), RuntimeKind: "pi"})
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("CreateAgent error = %v, want %q", err, test.wantText)
			}
			if agents := h.ListAgents(); len(agents) != 0 {
				t.Fatalf("invalid Runtime Contract persisted Agent: %#v", agents)
			}
			persisted := map[string]*Agent{}
			loadErr := st.LoadAgents(&persisted)
			if loadErr != nil || len(persisted) != 0 {
				t.Fatalf("persisted invalid Runtime binding = %#v, err=%v", persisted, loadErr)
			}
			if host.Alive() {
				t.Fatal("rejected Runtime Contract Host remained alive")
			}
		})
	}
}

func TestRuntimeResumeCreatesOnlyForTypedBindingNotFound(t *testing.T) {
	for _, test := range []struct {
		name       string
		outcome    runtimecontract.Outcome
		wantCreate int
	}{
		{
			name: "typed binding missing",
			outcome: runtimecontract.Outcome{State: runtimecontract.LifecycleRejected, Failure: &runtimecontract.Failure{
				Code: runtimecontract.FailureCodeBindingNotFound, Phase: runtimecontract.FailurePhaseBindingResume, Message: "gone",
			}},
			wantCreate: 1,
		},
		{
			name: "unrelated not found text",
			outcome: runtimecontract.Outcome{State: runtimecontract.LifecycleFailed, Failure: &runtimecontract.Failure{
				Code: "model_error", Phase: runtimecontract.FailurePhaseBindingResume, Message: "model not found; bearer private-token",
			}},
		},
		{
			name: "indeterminate binding missing",
			outcome: runtimecontract.Outcome{State: runtimecontract.LifecycleIndeterminate, Failure: &runtimecontract.Failure{
				Code: runtimecontract.FailureCodeBindingNotFound, Phase: runtimecontract.FailurePhaseBindingResume, Message: "transport result unknown",
			}},
		},
		{
			name: "failed binding missing",
			outcome: runtimecontract.Outcome{State: runtimecontract.LifecycleFailed, Failure: &runtimecontract.Failure{
				Code: runtimecontract.FailureCodeBindingNotFound, Phase: runtimecontract.FailurePhaseBindingResume, Message: "failed",
			}},
		},
		{
			name: "wrong phase binding missing",
			outcome: runtimecontract.Outcome{State: runtimecontract.LifecycleRejected, Failure: &runtimecontract.Failure{
				Code: runtimecontract.FailureCodeBindingNotFound, Phase: runtimecontract.FailurePhaseTurnStart, Message: "wrong phase",
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			h := testHub(st)
			meta := &Agent{ID: "agent-1", Name: "worker", Cwd: t.TempDir(), ThreadID: "thread-loom", RuntimeBinding: RuntimeBinding{
				SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "native-old",
			}, Status: "idle"}
			h.agents[meta.ID] = meta
			contract := &controlPlaneContract{
				createBinding: runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "fake", NativeRef: "native-new"},
				resumeOutcome: test.outcome,
			}
			rt := &runtime{agentID: meta.ID, runtimeContract: contract, binding: runtimeContractBinding(meta), ready: make(chan struct{}), approvals: map[string]*approval{}}
			h.runtimes[meta.ID] = rt

			h.initRuntime(meta.ID, rt)

			if contract.createCalls != test.wantCreate {
				t.Fatalf("CreateBinding calls = %d, want %d", contract.createCalls, test.wantCreate)
			}
			if test.wantCreate == 0 && rt.initErr == nil {
				t.Fatal("resume failure was silently accepted")
			}
		})
	}
}

func TestMandatoryRuntimeOperationsRejectWrongSuccessfulLifecycleStates(t *testing.T) {
	for _, test := range []struct {
		name       string
		outcome    runtimecontract.Outcome
		expected   runtimecontract.LifecycleState
		requireRef bool
	}{
		{name: "create completed", outcome: runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}, expected: runtimecontract.LifecycleAccepted},
		{name: "resume accepted", outcome: runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}, expected: runtimecontract.LifecycleCompleted},
		{name: "start completed", outcome: runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted, RuntimeTurnRef: "native-turn"}, expected: runtimecontract.LifecycleAccepted, requireRef: true},
		{name: "start missing ref", outcome: runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}, expected: runtimecontract.LifecycleAccepted, requireRef: true},
		{name: "continue completed", outcome: runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}, expected: runtimecontract.LifecycleAccepted},
		{name: "interrupt accepted", outcome: runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}, expected: runtimecontract.LifecycleInterrupted},
		{name: "close interrupted", outcome: runtimecontract.Outcome{State: runtimecontract.LifecycleInterrupted}, expected: runtimecontract.LifecycleCompleted},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := runtimeLifecycleOutcomeError(test.outcome, test.expected, test.requireRef); err == nil {
				t.Fatalf("wrong lifecycle outcome accepted: %#v", test.outcome)
			}
		})
	}
}

func (c *controlPlaneContract) ContextDeliveryMode() runtimecontract.ContextDeliveryMode {
	return c.contextMode
}
func (c *controlPlaneContract) SetRuntimeSandbox(string)          {}
func (c *controlPlaneContract) SetRuntimeProvider(string, string) {}
func (c *controlPlaneContract) SetRuntimeModel(string)            {}
func (c *controlPlaneContract) SetRuntimeDisabledSkills([]string) {}
func (c *controlPlaneContract) SetRuntimeApprovalPolicy(string)   {}
func (c *controlPlaneContract) SetRuntimeEffort(string)           {}
func (c *controlPlaneContract) ValidateInput(context.Context, runtimecontract.Binding, []runtimecontract.InputBlock) *runtimecontract.Failure {
	return nil
}
func (c *controlPlaneContract) RuntimeGoal(context.Context, runtimecontract.Binding) (*ThreadGoal, error) {
	return nil, nil
}
func (c *controlPlaneContract) UpdateRuntimeGoal(context.Context, runtimecontract.Binding, GoalUpdateParams) (*ThreadGoal, error) {
	return nil, nil
}
func (c *controlPlaneContract) ClearRuntimeGoal(context.Context, runtimecontract.Binding) (bool, error) {
	return false, nil
}
func (c *controlPlaneContract) RuntimeUsage(context.Context, runtimecontract.Binding) (*RuntimeUsageReport, error) {
	return &RuntimeUsageReport{}, nil
}
func (c *controlPlaneContract) InspectModelControl(context.Context, runtimecontract.Binding) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
	return runtimecontract.ModelControlState{}, nil
}
func (c *controlPlaneContract) SelectModel(context.Context, runtimecontract.Binding, runtimecontract.ModelSelection) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
	return runtimecontract.ModelControlState{}, nil
}
func (c *controlPlaneContract) CompactRuntimeBinding(context.Context, runtimecontract.Binding) error {
	return nil
}
func (c *controlPlaneContract) ContractVersion() int {
	if c.version != 0 {
		return c.version
	}
	return runtimecontract.Version
}
func (c *controlPlaneContract) CreateBinding(context.Context, runtimecontract.BindingRequest) (runtimecontract.Binding, runtimecontract.Outcome) {
	c.createCalls++
	if c.createOutcome.State != "" {
		return c.createBinding, c.createOutcome
	}
	return c.createBinding, runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}
}
func (c *controlPlaneContract) ResumeBinding(context.Context, runtimecontract.Binding) runtimecontract.Outcome {
	if c.resumeStarted != nil {
		close(c.resumeStarted)
		<-c.resumeRelease
	}
	if c.resumeOutcome.State != "" {
		return c.resumeOutcome
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}
func (c *controlPlaneContract) UpdateBindingName(_ context.Context, binding runtimecontract.Binding, name string) runtimecontract.Outcome {
	c.nameCalls++
	c.nameBinding, c.name = binding, name
	if c.nameOutcome.State != "" || c.nameOutcome.Failure != nil {
		return c.nameOutcome
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}
func (c *controlPlaneContract) StartTurn(_ context.Context, request runtimecontract.TurnRequest) runtimecontract.Outcome {
	c.startCalls++
	c.startRequest = request
	if c.startStarted != nil {
		close(c.startStarted)
		<-c.startRelease
	}
	if c.startOutcome.State != "" {
		if c.eventHandler != nil && c.startOutcome.State == runtimecontract.LifecycleAccepted {
			c.eventHandler(runtimecontract.Event{Kind: runtimecontract.EventTurnStarted, TurnID: request.TurnID, RuntimeTurnRef: c.startOutcome.RuntimeTurnRef})
		}
		return c.startOutcome
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}
}
func (c *controlPlaneContract) ContinueTurn(_ context.Context, request runtimecontract.CausalInput) runtimecontract.Outcome {
	c.continueCalls++
	c.continueRequest = request
	if c.continueSet || c.continueOutcome.State != "" {
		return c.continueOutcome
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}
}
func (c *controlPlaneContract) InterruptTurn(_ context.Context, request runtimecontract.TurnTarget) runtimecontract.Outcome {
	c.interruptCalls++
	c.interruptRequest = request
	c.callOrder = append(c.callOrder, "interrupt")
	if c.interruptOutcome.State != "" {
		return c.interruptOutcome
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleInterrupted}
}
func (c *controlPlaneContract) SetEventHandler(handler func(runtimecontract.Event)) {
	c.eventHandler = handler
}
func (c *controlPlaneContract) ReadHistory(context.Context, runtimecontract.HistoryRequest) (runtimecontract.History, *runtimecontract.Failure) {
	return c.history, c.historyFailure
}
func (c *controlPlaneContract) CapabilitySnapshot(context.Context, runtimecontract.Binding) runtimecontract.CapabilitySnapshot {
	if c.snapshotHook != nil {
		c.snapshotHook()
	}
	if c.snapshot.Revision == "" && len(c.snapshot.Capabilities) == 0 {
		return runtimecontract.CapabilitySnapshot{Revision: "test-empty"}
	}
	return c.snapshot
}

func TestCanonicalHistoryUsesRegisteredDriverWhenAgentIsCold(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	contract := &controlPlaneContract{history: runtimecontract.History{
		Total: 1,
		Turns: []runtimecontract.HistoryTurn{{
			RuntimeTurnRef: "native-turn", State: runtimecontract.LifecycleCompleted,
			Content: []runtimecontract.ContentBlock{{ID: "native-content", Kind: runtimecontract.ContentAssistantText, Text: "hello"}},
		}},
	}}
	h.runtimeHostDrivers = map[string]RuntimeHostDriver{"fake": &controlPlaneDriver{historyContract: contract}}
	h.agents["agent-1"] = &Agent{ID: "agent-1", Name: "cold", ThreadID: "loom-thread", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "/native/session"}, RuntimeTurnBindings: map[string]string{"turn-loom": "native-turn"}, Status: "idle"}

	history, err := h.CanonicalHistory("agent-1", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Turns) != 1 || history.Turns[0].TurnID != "turn-loom" || history.Turns[0].Content[0].ID == "native-content" {
		t.Fatalf("cold canonical history = %#v", history)
	}
}

func TestCanonicalHistoryUsesTypedNotFoundAndGetTurnPreservesBackendFailure(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	driver := &controlPlaneDriver{historyContract: &controlPlaneContract{historyFailure: &runtimecontract.Failure{Code: "history_not_found", Phase: runtimecontract.FailurePhaseHistory, Message: "localized missing history"}}}
	h.runtimeHostDrivers = map[string]RuntimeHostDriver{"fake": driver}
	h.agents["missing"] = &Agent{ID: "missing", Name: "missing", ThreadID: "loom-missing", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "/native/missing"}, Status: "idle"}
	if history, err := h.CanonicalHistory("missing", 10, 0); err != nil || len(history.Turns) != 0 {
		t.Fatalf("typed not found history=%#v err=%v", history, err)
	}

	driver.historyContract = &controlPlaneContract{historyFailure: &runtimecontract.Failure{Code: "transport_failed", Phase: runtimecontract.FailurePhaseHistory, Message: "backend unavailable"}}
	if _, err := h.GetCanonicalTurn("turn-unknown"); err == nil || !strings.Contains(err.Error(), "backend unavailable") {
		t.Fatalf("GetCanonicalTurn error = %v", err)
	}
}

func TestScopedCapabilitySnapshotRevisionChangesWithModelAndConfiguration(t *testing.T) {
	base := runtimecontract.CapabilitySnapshot{
		Revision: "driver-7",
		Capabilities: []runtimecontract.CapabilityDescriptor{{
			ID: runtimecontract.CapabilityContextDelivery, Availability: runtimecontract.CapabilityAvailable, Revision: "cap-1",
		}},
	}
	first, err := scopeRuntimeCapabilitySnapshot(base, runtimecontract.CapabilityScope{RuntimeKind: "fake", BindingRevision: "binding-1", Model: "model-a", ConfigurationRevision: "config-a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := scopeRuntimeCapabilitySnapshot(base, runtimecontract.CapabilityScope{RuntimeKind: "fake", BindingRevision: "binding-1", Model: "model-b", ConfigurationRevision: "config-a"})
	if err != nil {
		t.Fatal(err)
	}
	third, err := scopeRuntimeCapabilitySnapshot(base, runtimecontract.CapabilityScope{RuntimeKind: "fake", BindingRevision: "binding-1", Model: "model-a", ConfigurationRevision: "config-b"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision == "" || first.Revision == second.Revision || first.Revision == third.Revision {
		t.Fatalf("scoped revisions first=%q second=%q third=%q", first.Revision, second.Revision, third.Revision)
	}
}

func TestCanonicalTurnFailureFallsBackToPublicAgentError(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	contract := &controlPlaneContract{history: runtimecontract.History{Total: 1, Turns: []runtimecontract.HistoryTurn{{TurnID: "turn-loom", State: runtimecontract.LifecycleFailed, Content: []runtimecontract.ContentBlock{}}}}}
	h.runtimeHostDrivers = map[string]RuntimeHostDriver{"fake": &controlPlaneDriver{historyContract: contract}}
	nativeRef := "/private/native/session.jsonl"
	h.agents["agent-1"] = &Agent{ID: "agent-1", Name: "failed", ThreadID: "thread-loom", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: nativeRef}, Status: "idle", LastError: "read " + nativeRef + ": token=private"}
	detail, err := h.GetCanonicalTurn("turn-loom")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Error == "" || strings.Contains(detail.Error, nativeRef) || strings.Contains(detail.Error, "token=private") || !strings.Contains(detail.Error, "[redacted]") {
		t.Fatalf("public Turn failure = %q", detail.Error)
	}
}

func TestUnsupportedConfigUsesCapabilityReasonAndAlternativeBeforePersistence(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	contract := &controlPlaneContract{snapshot: runtimecontract.CapabilitySnapshot{Revision: "test-1", Capabilities: []runtimecontract.CapabilityDescriptor{{
		ID: runtimecontract.CapabilitySandboxConfiguration, Availability: runtimecontract.CapabilityUnavailable,
		Reason: "no whole-process isolation", Alternative: "use per-tool Approval", Revision: "test-1",
	}}}}
	meta := &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "thread-loom",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "native-secret"},
		Sandbox:        "unchanged", Status: "idle", CreatedAt: now(), UpdatedAt: now(),
	}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = &runtime{agentID: meta.ID, runtimeContract: contract, approvals: map[string]*approval{}}
	requested := "workspace-write"

	_, err = h.UpdateAgentConfig(meta.ID, ConfigParams{Sandbox: &requested})
	if err == nil || !strings.Contains(err.Error(), "no whole-process isolation") || !strings.Contains(err.Error(), "use per-tool Approval") {
		t.Fatalf("unsupported config error=%v", err)
	}
	if meta.Sandbox != "unchanged" || h.seqs[meta.ID] != 0 {
		t.Fatalf("unsupported config persisted or published: Agent=%#v seq=%d", meta, h.seqs[meta.ID])
	}
}

func TestCapabilitySnapshotRunsOutsideHubLockAndRevalidatesBinding(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	contract := &controlPlaneContract{snapshot: runtimecontract.CapabilitySnapshot{Revision: "test-1", Capabilities: []runtimecontract.CapabilityDescriptor{{
		ID: runtimecontract.CapabilitySandboxConfiguration, Availability: runtimecontract.CapabilityAvailable, Revision: "test-1",
	}}}}
	meta := &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "thread-loom",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "native-old"},
		Sandbox:        "unchanged", Status: "idle", CreatedAt: now(), UpdatedAt: now(),
	}
	original := &runtime{agentID: meta.ID, runtimeContract: contract, binding: runtimeContractBinding(meta), approvals: map[string]*approval{}}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = original
	queriedOutsideLock := false
	contract.snapshotHook = func() {
		if !h.mu.TryLock() {
			return
		}
		queriedOutsideLock = true
		h.runtimes[meta.ID] = &runtime{
			agentID: meta.ID,
			binding: runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "fake", NativeRef: "native-new"},
		}
		h.mu.Unlock()
	}
	requested := "workspace-write"

	_, err = h.UpdateAgentConfig(meta.ID, ConfigParams{Sandbox: &requested})
	if !queriedOutsideLock {
		t.Fatal("CapabilitySnapshot ran while the Hub mutex was held")
	}
	if err == nil || !strings.Contains(err.Error(), "binding changed") {
		t.Fatalf("UpdateAgentConfig error=%v, want stale capability query rejection", err)
	}
	if meta.Sandbox != "unchanged" || h.seqs[meta.ID] != 0 {
		t.Fatalf("stale capability result persisted or published: Agent=%#v seq=%d", meta, h.seqs[meta.ID])
	}
}

func TestCapabilitySnapshotRejectsConcurrentModelOrConfigurationChange(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Agent)
	}{
		{name: "model", mutate: func(meta *Agent) { meta.Model = "model-b" }},
		{name: "configuration", mutate: func(meta *Agent) { meta.ApprovalPolicy = "never" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			h := testHub(st)
			contract := &controlPlaneContract{snapshot: runtimecontract.CapabilitySnapshot{Revision: "test-1", Capabilities: []runtimecontract.CapabilityDescriptor{{
				ID: runtimecontract.CapabilitySandboxConfiguration, Availability: runtimecontract.CapabilityAvailable,
				Scope: runtimecontract.CapabilityScope{RuntimeKind: "fake"}, Revision: "test-1",
			}}}}
			meta := &Agent{
				ID: "agent-1", Name: "worker", ThreadID: "thread-loom", Model: "model-a", ApprovalPolicy: "on-request",
				RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "native-binding"},
				Sandbox:        "unchanged", Status: "idle", CreatedAt: now(), UpdatedAt: now(),
			}
			rt := &runtime{agentID: meta.ID, runtimeContract: contract, binding: runtimeContractBinding(meta), approvals: map[string]*approval{}}
			h.agents[meta.ID], h.runtimes[meta.ID] = meta, rt
			contract.snapshotHook = func() {
				h.mu.Lock()
				test.mutate(meta)
				h.mu.Unlock()
			}
			requested := "workspace-write"

			_, err = h.UpdateAgentConfig(meta.ID, ConfigParams{Sandbox: &requested})
			if err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("stale %s capability error = %v", test.name, err)
			}
			if meta.Sandbox != "unchanged" {
				t.Fatalf("stale %s capability persisted sandbox %q", test.name, meta.Sandbox)
			}
		})
	}
}

func TestCapabilityDescriptorScopeMustMatchCurrentRuntimeBindingModelAndConfiguration(t *testing.T) {
	for _, scope := range []runtimecontract.CapabilityScope{
		{RuntimeKind: "other"},
		{RuntimeKind: "fake", BindingRevision: "stale-binding"},
		{RuntimeKind: "fake", Model: "stale-model"},
		{RuntimeKind: "fake", ConfigurationRevision: "stale-config"},
	} {
		st, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		h := testHub(st)
		contract := &controlPlaneContract{snapshot: runtimecontract.CapabilitySnapshot{Revision: "test-1", Capabilities: []runtimecontract.CapabilityDescriptor{{
			ID: runtimecontract.CapabilitySandboxConfiguration, Availability: runtimecontract.CapabilityAvailable, Scope: scope, Revision: "test-1",
		}}}}
		meta := &Agent{
			ID: "agent-1", Name: "worker", ThreadID: "thread-loom", Model: "model-a", ApprovalPolicy: "on-request",
			RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "native-binding"},
			Sandbox:        "unchanged", Status: "idle", CreatedAt: now(), UpdatedAt: now(),
		}
		h.agents[meta.ID] = meta
		h.runtimes[meta.ID] = &runtime{agentID: meta.ID, runtimeContract: contract, binding: runtimeContractBinding(meta), approvals: map[string]*approval{}}
		requested := "workspace-write"

		_, err = h.UpdateAgentConfig(meta.ID, ConfigParams{Sandbox: &requested})
		if err == nil || !strings.Contains(err.Error(), "scope") {
			t.Fatalf("mismatched scope %#v error = %v", scope, err)
		}
		if meta.Sandbox != "unchanged" {
			t.Fatalf("mismatched scope %#v persisted sandbox %q", scope, meta.Sandbox)
		}
		st.Close()
	}
}

func TestColdCreateUsesRegisteredDriverCapabilitySnapshot(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	driver := &controlPlaneDriver{snapshot: runtimecontract.CapabilitySnapshot{Revision: "test-1", Capabilities: []runtimecontract.CapabilityDescriptor{{
		ID: runtimecontract.CapabilitySandboxConfiguration, Availability: runtimecontract.CapabilityUnavailable,
		Reason: "driver says no isolation", Alternative: "driver says use approval", Revision: "test-1",
	}}}}
	h.runtimeHostDrivers["codex"] = driver

	_, err = h.CreateAgent(CreateParams{
		Name: "worker", Cwd: t.TempDir(), RuntimeKind: "codex", Sandbox: "workspace-write",
	})
	if err == nil || !strings.Contains(err.Error(), "driver says no isolation") || !strings.Contains(err.Error(), "driver says use approval") {
		t.Fatalf("cold capability error=%v", err)
	}
	if len(h.agents) != 0 || len(h.runtimes) != 0 || len(h.seqs) != 0 {
		t.Fatalf("unsupported create reached binding/persistence: agents=%d runtimes=%d seqs=%d", len(h.agents), len(h.runtimes), len(h.seqs))
	}
}

func TestCreateRejectsUnsupportedRuntimeConfigBeforeBindingOrPersistence(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	_, err = h.CreateAgent(CreateParams{
		Name: "pi-worker", Cwd: t.TempDir(), RuntimeKind: "pi", Sandbox: "read-only",
	})
	if err == nil || !strings.Contains(err.Error(), "whole-process sandbox isolation") || !strings.Contains(err.Error(), "Approval policy") {
		t.Fatalf("unsupported create config error=%v", err)
	}
	if len(h.agents) != 0 || len(h.runtimes) != 0 || len(h.seqs) != 0 {
		t.Fatalf("unsupported create reached binding/persistence: agents=%d runtimes=%d seqs=%d", len(h.agents), len(h.runtimes), len(h.seqs))
	}
}

func TestPiSkillPolicyIsRejectedBeforePersistence(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	meta := &Agent{
		ID: "pi-agent", Name: "pi-worker", Cwd: t.TempDir(), ThreadID: "thread-loom",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "pi", NativeRef: "native-session"},
		Status:         "idle", CreatedAt: now(), UpdatedAt: now(),
	}
	h.agents[meta.ID] = meta

	_, err = h.UpdateAgentSkillConfig(meta.ID, AgentSkillConfigParams{Path: "/tmp/codex-skill/SKILL.md", Enabled: false})
	if err == nil || !strings.Contains(err.Error(), "does not apply Loom Skill policy") || !strings.Contains(err.Error(), "native Runtime settings") {
		t.Fatalf("Pi Skill policy error=%v", err)
	}
	if len(h.agentSkillConfigs) != 0 || h.seqs[meta.ID] != 0 {
		t.Fatalf("Pi Skill policy persisted or published: configs=%#v seq=%d", h.agentSkillConfigs, h.seqs[meta.ID])
	}
}

func TestPiSkillInventoryDoesNotProjectCodexCatalog(t *testing.T) {
	h := testHub(nil)
	cwd := t.TempDir()
	h.agents["codex-agent"] = &Agent{ID: "codex-agent", Name: "codex", Cwd: cwd, RuntimeBinding: RuntimeBinding{Kind: "codex"}}
	h.agents["pi-agent"] = &Agent{ID: "pi-agent", Name: "pi", Cwd: cwd, RuntimeBinding: RuntimeBinding{Kind: "pi"}}
	inventory := SkillInventory{Data: []SkillInventoryEntry{{
		Cwd: cwd, Skills: []SkillInventorySkill{{Name: "codex-only", Path: "/tmp/codex-only/SKILL.md", Enabled: true}},
	}}}

	h.projectAgentSkillInventory(&inventory)
	var codexSkills, piSkills []SkillInventorySkill
	for _, entry := range inventory.Agents {
		switch entry.AgentID {
		case "codex-agent":
			codexSkills = entry.Skills
		case "pi-agent":
			piSkills = entry.Skills
		}
	}
	if len(codexSkills) != 1 || len(piSkills) != 0 {
		t.Fatalf("projected Codex/Pi Skills = %#v / %#v", codexSkills, piSkills)
	}
}

func TestReloadSkillsSuppressesPersistedCodexPolicyForPiAgentsAfterReopen(t *testing.T) {
	installFakeSharedCodexHost(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seed := testHub(st)
	const disabledPath = "/tmp/skill/SKILL.md"
	for _, id := range []string{"pi-applied", "pi-restart"} {
		seed.agents[id] = &Agent{
			ID: id, Name: id, Cwd: "/tmp/one", ThreadID: "loom-" + id,
			RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "pi", NativeRef: "native-" + id},
			Status:         "idle", CreatedAt: now(), UpdatedAt: now(),
		}
	}
	if err := seed.persistAgentsLocked(); err != nil {
		t.Fatal(err)
	}
	persisted := map[string]*AgentSkillConfig{
		"pi-applied": {AgentID: "pi-applied", DisabledPaths: []string{disabledPath}, UpdatedAt: now()},
		"pi-restart": {AgentID: "pi-restart", DisabledPaths: []string{disabledPath}, UpdatedAt: now()},
	}
	if err := st.SaveAgentSkillConfigs(persisted); err != nil {
		t.Fatal(err)
	}

	h := New(st)
	defer h.Shutdown()
	h.mu.Lock()
	for _, id := range []string{"pi-applied", "pi-restart"} {
		meta := h.agents[id]
		h.runtimes[id] = &runtime{
			agentID: id, runtimeContract: &controlPlaneContract{snapshot: piControlPlaneCapabilitySnapshot()},
			binding: runtimeContractBinding(meta), skillConfigLoaded: true, approvals: map[string]*approval{},
		}
	}
	h.runtimes["pi-applied"].skillConfigHash = agentSkillConfigHash([]string{disabledPath})
	h.runtimes["pi-restart"].skillConfigHash = agentSkillConfigHash(nil)
	h.mu.Unlock()

	inventory, err := h.ReloadSkills()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pi-applied", "pi-restart"} {
		entry := findAgentSkillInventory(t, inventory, id)
		if len(entry.Skills) != 0 || len(entry.Errors) != 0 || len(entry.DisabledPaths) != 0 || entry.Applied || entry.RestartRequired {
			t.Fatalf("Pi %s leaked stale Codex Skill projection after reopen: %#v", id, entry)
		}
	}
	var retained map[string]*AgentSkillConfig
	if err := st.LoadAgentSkillConfigs(&retained); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pi-applied", "pi-restart"} {
		if config := retained[id]; config == nil || len(config.DisabledPaths) != 1 || config.DisabledPaths[0] != disabledPath {
			t.Fatalf("historical persisted config for %s was mutated: %#v", id, config)
		}
	}
}

func TestPiResumeAndTurnDoNotClaimCodexSkillPolicyApplied(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	contract := &controlPlaneContract{
		contextMode: runtimecontract.ContextDeliveryFullPerTurn,
		snapshot:    piControlPlaneCapabilitySnapshot(),
		startOutcome: runtimecontract.Outcome{
			State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: "native-turn",
		},
	}
	ready := make(chan struct{})
	close(ready)
	meta := &Agent{
		ID: "pi-agent", Name: "pi-worker", Cwd: t.TempDir(), ThreadID: "thread-loom",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "pi", NativeRef: "native-session"},
		Status:         "idle", CreatedAt: now(), UpdatedAt: now(),
	}
	rt := &runtime{
		agentID: meta.ID, runtimeContract: contract, binding: runtimeContractBinding(meta), ready: ready,
		skillConfigLoaded: true, skillConfigHash: agentSkillConfigHash(nil), approvals: map[string]*approval{},
	}
	h.agents[meta.ID], h.runtimes[meta.ID] = meta, rt

	if _, err := h.SendTask(meta.ID, "verify Pi", time.Second); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	view := h.agentSkillConfigViewLocked(meta)
	h.mu.Unlock()
	if view.Applied || view.RestartRequired || rt.skillConfigLoaded || rt.skillConfigHash != "" {
		t.Fatalf("Pi Skill projection after resume/Turn = %#v loaded=%v hash=%q", view, rt.skillConfigLoaded, rt.skillConfigHash)
	}
	if _, err := h.GetAgentSkillConfig(meta.ID); err == nil || !strings.Contains(err.Error(), "does not apply Loom Skill policy") {
		t.Fatalf("Pi Skill capability error = %v", err)
	}
}
func (c *controlPlaneContract) CloseBinding(_ context.Context, binding runtimecontract.Binding) runtimecontract.Outcome {
	c.closeCalls++
	c.closeBinding = binding
	c.callOrder = append(c.callOrder, "close")
	if c.closeOutcome.State != "" || c.closeOutcome.Failure != nil {
		return c.closeOutcome
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}
func (c *controlPlaneContract) ArchiveBinding(_ context.Context, binding runtimecontract.Binding) runtimecontract.Outcome {
	c.archiveCalls++
	c.archiveBinding = binding
	c.callOrder = append(c.callOrder, "archive")
	return c.archiveOutcome
}

func TestArchiveCommitsLoomStateWhenOptionalNativeArchiveFails(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	contract := &controlPlaneContract{archiveOutcome: runtimecontract.Outcome{
		State: runtimecontract.LifecycleFailed,
		Failure: &runtimecontract.Failure{
			Code: "native_archive_failed", Phase: runtimecontract.FailurePhaseBindingArchive,
			Message: "native archive failed", Cause: errors.New("native archive failed"),
		},
	}}
	meta := &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "thread-loom",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "native-secret"},
		Status:         "idle", CreatedAt: now(), UpdatedAt: now(),
	}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = &runtime{agentID: meta.ID, runtimeContract: contract, approvals: map[string]*approval{}}

	result, err := h.ArchiveAgent(meta.ID)
	if err != nil || result["archived"] != true {
		t.Fatalf("Archive result=%#v err=%v", result, err)
	}
	if _, err := h.GetAgent(meta.ID); hubErrorStatus(err) != 404 {
		t.Fatalf("committed Loom archive remained visible: %v", err)
	}
	if contract.archiveCalls != 1 || contract.closeCalls != 1 {
		t.Fatalf("native archive calls=%d close calls=%d", contract.archiveCalls, contract.closeCalls)
	}
}

func TestArchiveInterruptsCommittedActiveBindingBeforeArchiveAndClose(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	contract := &controlPlaneContract{
		interruptOutcome: runtimecontract.Outcome{State: runtimecontract.LifecycleFailed, Failure: &runtimecontract.Failure{Code: "interrupt_failed", Phase: runtimecontract.FailurePhaseTurnInterrupt, Message: "interrupt failed"}},
		archiveOutcome:   runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted},
	}
	meta := &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "thread-loom",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "stale-native-binding"},
		Status:         "running", CurrentTurnID: "turn-loom", CreatedAt: now(), UpdatedAt: now(),
	}
	turn := &turnState{turnID: "turn-loom", nativeTurnID: "native-turn", stopWatchdog: make(chan struct{})}
	currentBinding := runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "fake", NativeRef: "current-native-binding"}
	rt := &runtime{agentID: meta.ID, runtimeContract: contract, binding: currentBinding, activeTurn: turn, approvals: map[string]*approval{}}
	h.agents[meta.ID], h.runtimes[meta.ID] = meta, rt

	if _, err := h.ArchiveAgent(meta.ID); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(contract.callOrder, ","); got != "interrupt,archive,close" {
		t.Fatalf("native effect order = %s", got)
	}
	if contract.interruptRequest.Binding.NativeRef != currentBinding.NativeRef || contract.archiveBinding.NativeRef != currentBinding.NativeRef || contract.closeBinding.NativeRef != currentBinding.NativeRef {
		t.Fatalf("archive used stale/replaced binding: interrupt=%#v archive=%#v close=%#v", contract.interruptRequest.Binding, contract.archiveBinding, contract.closeBinding)
	}
	if !rt.effectDomainInvalidated || rt.activeTurn != nil {
		t.Fatalf("failed archive interrupt was not fenced/settled: invalidated=%v active=%#v", rt.effectDomainInvalidated, rt.activeTurn)
	}
}

func TestArchiveWaitsForResumeAndStartThenClosesTheBinding(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	resumeStarted, resumeRelease := make(chan struct{}), make(chan struct{})
	startStarted, startRelease := make(chan struct{}), make(chan struct{})
	contract := &controlPlaneContract{
		contextMode:   runtimecontract.ContextDeliveryFullPerTurn,
		resumeStarted: resumeStarted, resumeRelease: resumeRelease,
		startStarted: startStarted, startRelease: startRelease,
		startOutcome:   runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: "native-turn"},
		archiveOutcome: runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted},
	}
	host := &controlPlaneHost{contract: contract, alive: true}
	ready := make(chan struct{})
	close(ready)
	meta := &Agent{
		ID: "agent-1", Name: "worker", Cwd: t.TempDir(), ThreadID: "thread-loom",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "native-binding"},
		Status:         "idle", CreatedAt: now(), UpdatedAt: now(),
	}
	rt := &runtime{
		agentID: meta.ID, agentHost: host, runtimeContract: contract, binding: runtimeContractBinding(meta),
		ready: ready, approvals: map[string]*approval{},
	}
	h.agents[meta.ID], h.runtimes[meta.ID] = meta, rt

	sendDone := make(chan error, 1)
	go func() {
		_, sendErr := h.SendTask(meta.ID, "run once", time.Second)
		sendDone <- sendErr
	}()
	<-resumeStarted
	archiveDone := make(chan error, 1)
	go func() {
		_, archiveErr := h.ArchiveAgent(meta.ID)
		archiveDone <- archiveErr
	}()
	assertArchiveStillWaiting(t, h, meta.ID, archiveDone, "ResumeBinding")
	close(resumeRelease)
	<-startStarted
	assertArchiveStillWaiting(t, h, meta.ID, archiveDone, "StartTurn")
	close(startRelease)
	if err := <-sendDone; err != nil {
		t.Fatalf("SendTask: %v", err)
	}
	if err := <-archiveDone; err != nil {
		t.Fatalf("ArchiveAgent: %v", err)
	}
	if host.Alive() {
		t.Fatal("archived Runtime Host is still alive")
	}
	if got := strings.Join(contract.callOrder, ","); got != "interrupt,archive,close" {
		t.Fatalf("archive lifecycle order = %q", got)
	}
}

func assertArchiveStillWaiting(t *testing.T, h *Hub, agentID string, done <-chan error, phase string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("ArchiveAgent returned during %s: %v", phase, err)
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := h.GetAgent(agentID); err != nil {
		t.Fatalf("Loom Agent archived before %s settled: %v", phase, err)
	}
}

func TestArchiveFallsBackToAgentHostCloseForNonCompletedOrInvalidCloseOutcome(t *testing.T) {
	for name, closeOutcome := range map[string]runtimecontract.Outcome{
		"non-completed": {State: runtimecontract.LifecycleAccepted},
		"invalid":       {State: runtimecontract.LifecycleInterrupted, Failure: &runtimecontract.Failure{Code: "malformed"}},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			h := testHub(st)
			contract := &controlPlaneContract{
				archiveOutcome: runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted},
				closeOutcome:   closeOutcome,
			}
			host := &controlPlaneHost{contract: contract, alive: true}
			meta := &Agent{
				ID: "agent-1", Name: "worker", ThreadID: "thread-loom",
				RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "native-binding"},
				Status:         "idle", CreatedAt: now(), UpdatedAt: now(),
			}
			h.agents[meta.ID] = meta
			h.runtimes[meta.ID] = &runtime{agentID: meta.ID, agentHost: host, runtimeContract: contract, binding: runtimeContractBinding(meta), approvals: map[string]*approval{}}

			if _, err := h.ArchiveAgent(meta.ID); err != nil {
				t.Fatal(err)
			}
			if host.Alive() {
				t.Fatalf("CloseBinding outcome %#v left the Agent Host alive", closeOutcome)
			}
		})
	}
}

func TestCausalSteerUsesV2ContractAndKeepsNativeIdentityPrivate(t *testing.T) {
	h := testHub(nil)
	contract := &controlPlaneContract{continueOutcome: runtimecontract.Outcome{
		State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: "native-secret-returned-by-runtime",
	}}
	meta := &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "thread-loom",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "binding-native-secret"},
		Status:         "running", CurrentTurnID: "turn-loom", CreatedAt: now(), UpdatedAt: now(),
	}
	rt := &runtime{
		agentID: meta.ID, runtimeContract: contract, binding: runtimeContractBinding(meta), approvals: map[string]*approval{},
		activeTurn: &turnState{turnID: "turn-loom", nativeTurnID: "turn-native-secret", stopWatchdog: make(chan struct{})},
	}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = rt

	accepted, err := h.requestTurnSteer(rt, meta.RuntimeBinding.NativeRef, "turn-loom", "continue", 0)
	if err != nil {
		t.Fatal(err)
	}
	if accepted != "turn-loom" {
		t.Fatalf("accepted public Turn ID = %q", accepted)
	}
	if contract.continueCalls != 1 || contract.continueRequest.TurnID != "turn-loom" || contract.continueRequest.RuntimeTurnRef != "turn-native-secret" {
		t.Fatalf("ContinueTurn request = %#v calls=%d", contract.continueRequest, contract.continueCalls)
	}
}

func TestOwnerInterruptUsesV2ContractWithLoomTurnIdentity(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	contract := &controlPlaneContract{interruptOutcome: runtimecontract.Outcome{
		State: runtimecontract.LifecycleInterrupted, RuntimeTurnRef: "native-secret-returned-by-runtime",
	}}
	meta := &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "thread-loom",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "binding-native-secret"},
		Status:         "running", CurrentTask: "work", CurrentTurnID: "turn-loom", CreatedAt: now(), UpdatedAt: now(),
	}
	turn := &turnState{
		turnID: "turn-loom", nativeTurnID: "turn-native-secret", task: "work", stopWatchdog: make(chan struct{}),
	}
	rt := &runtime{
		agentID: meta.ID, runtimeContract: contract, binding: runtimeContractBinding(meta), approvals: map[string]*approval{}, activeTurn: turn,
	}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = rt

	result, err := h.Interrupt(meta.ID, "Owner abort")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Interrupted || strings.Contains(result.Message+result.Reason, "native-secret") {
		t.Fatalf("public interrupt result = %#v", result)
	}
	if contract.interruptCalls != 1 || contract.interruptRequest.TurnID != "turn-loom" || contract.interruptRequest.RuntimeTurnRef != "turn-native-secret" {
		t.Fatalf("InterruptTurn request = %#v calls=%d", contract.interruptRequest, contract.interruptCalls)
	}
	h.Shutdown()
}

func TestContextDeliveryPolicyComesFromContractNotRuntimeKind(t *testing.T) {
	h, meta := contextTestHub(t)
	contract := &controlPlaneContract{contextMode: runtimecontract.ContextDeliveryFullPerTurn}
	h.runtimes[meta.ID] = &runtime{agentID: meta.ID, runtimeContract: contract, binding: runtimeContractBinding(meta)}

	plan, err := h.prepareTurnContext(meta.ID, authenticatedOwnerContext("direct_input", "", "", ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Attempt != nil || plan.DeveloperContext == "" {
		t.Fatalf("full-per-Turn context plan = %#v", plan)
	}
}

func TestSendTaskDeliversContextAndStartsThroughV2Contract(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	contract := &controlPlaneContract{
		contextMode:  runtimecontract.ContextDeliveryFullPerTurn,
		startOutcome: runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: "native-turn-secret"},
	}
	meta := &Agent{
		ID: "agent-v2", Name: "worker", Cwd: t.TempDir(), ThreadID: "thread-loom",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "binding-native-secret"},
		Status:         "idle", CreatedAt: now(), UpdatedAt: now(),
	}
	ready := make(chan struct{})
	close(ready)
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = &runtime{
		agentID: meta.ID, runtimeContract: contract, binding: runtimeContractBinding(meta), ready: ready, approvals: map[string]*approval{},
	}

	result, err := h.SendTask(meta.ID, "hello", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Dispatched || result.TurnID == "" || strings.Contains(result.TurnID, "native") {
		t.Fatalf("public SendTask result = %#v", result)
	}
	if contract.startCalls != 1 || contract.startRequest.TurnID != result.TurnID {
		t.Fatalf("StartTurn request = %#v calls=%d", contract.startRequest, contract.startCalls)
	}
	var userText, developerText string
	for _, block := range contract.startRequest.Input {
		switch block.Role {
		case runtimecontract.InputRoleDeveloper:
			developerText += block.Text
		case runtimecontract.InputRoleUser:
			userText += block.Text
		}
	}
	if !strings.Contains(userText, "hello") || !strings.Contains(developerText, "loom_developer_context") {
		t.Fatalf("v2 Input roles user=%q developer=%q", userText, developerText)
	}
	h.Shutdown()
}

func TestShutdownClosesEveryV2BindingBeforeEachDriverOnce(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	driver := &controlPlaneDriver{}
	h.runtimeHostDrivers = map[string]RuntimeHostDriver{"fake": driver}
	for _, id := range []string{"agent-1", "agent-2"} {
		contract := &controlPlaneContract{}
		host := &controlPlaneHost{contract: contract, alive: true}
		driver.hosts = append(driver.hosts, host)
		binding := runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "fake", NativeRef: "native-" + id}
		h.agents[id] = &Agent{ID: id, Name: id, RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: binding.NativeRef}, Status: "idle"}
		h.runtimes[id] = &runtime{agentID: id, agentHost: host, runtimeContract: contract, binding: binding, approvals: map[string]*approval{}}
	}

	h.Shutdown()
	h.Shutdown()
	if driver.shutdownCalls != 1 || !driver.closedBeforeShutdown {
		t.Fatalf("driver shutdown calls=%d bindingsClosedFirst=%v", driver.shutdownCalls, driver.closedBeforeShutdown)
	}
	for _, host := range driver.hosts {
		if host.contract.closeCalls != 1 || host.Alive() {
			t.Fatalf("binding close calls=%d alive=%v", host.contract.closeCalls, host.Alive())
		}
	}
}

func TestV2ConsumerRejectsMalformedLifecycleOutcomes(t *testing.T) {
	for name, malformed := range map[string]runtimecontract.Outcome{
		"missing state":                 {},
		"accepted with failure":         {State: runtimecontract.LifecycleAccepted, Failure: &runtimecontract.Failure{Code: "bad"}},
		"interrupted with failure":      {State: runtimecontract.LifecycleInterrupted, Failure: &runtimecontract.Failure{Code: "bad"}},
		"failed without failure":        {State: runtimecontract.LifecycleFailed},
		"indeterminate without failure": {State: runtimecontract.LifecycleIndeterminate},
	} {
		t.Run(name, func(t *testing.T) {
			h := testHub(nil)
			contract := &controlPlaneContract{continueOutcome: malformed, continueSet: true}
			meta := &Agent{ID: "agent-1", ThreadID: "thread-loom", RuntimeBinding: RuntimeBinding{Kind: "fake", NativeRef: "native-binding"}}
			rt := &runtime{
				agentID: meta.ID, runtimeContract: contract, binding: runtimeContractBinding(meta),
				activeTurn: &turnState{turnID: "turn-loom", nativeTurnID: "native-turn", stopWatchdog: make(chan struct{})},
			}
			h.agents[meta.ID], h.runtimes[meta.ID] = meta, rt
			if _, err := h.requestTurnSteer(rt, meta.RuntimeBinding.NativeRef, "turn-loom", "continue", time.Second); err == nil || !strings.Contains(err.Error(), "invalid Runtime lifecycle outcome") {
				t.Fatalf("malformed outcome %#v error=%v", malformed, err)
			}
		})
	}
}
