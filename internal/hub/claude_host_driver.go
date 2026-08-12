package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yan5xu/codex-loom/internal/claudebridge"
	"github.com/yan5xu/codex-loom/internal/claudegen"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

type claudeRuntimeHostDriver struct {
	st        *store.Store
	bridge    *claudebridge.Driver
	preflight func(context.Context) error

	mu        sync.Mutex
	handles   map[string]*claudeAgentHost
	shutdown  bool
	closeOnce sync.Once
}

func newClaudeRuntimeHostDriver(st *store.Store, options claudebridge.DriverOptions) *claudeRuntimeHostDriver {
	resolveActive := options.ResolveActive
	d := &claudeRuntimeHostDriver{st: st, handles: map[string]*claudeAgentHost{}, preflight: func(ctx context.Context) error {
		if resolveActive == nil {
			return errors.New("Claude bridge active generation resolver is required")
		}
		_, err := resolveActive(ctx)
		return err
	}}
	previousEvent, previousFailure := options.OnEvent, options.OnFailure
	options.OnEvent = func(event claudebridge.Event) {
		if previousEvent != nil {
			previousEvent(event)
		}
		d.onBridgeEvent(event)
	}
	options.OnFailure = func(agentID string, err error) {
		if previousFailure != nil {
			previousFailure(agentID, err)
		}
		d.onBridgeFailure(agentID, err)
	}
	d.bridge = claudebridge.NewDriver(options)
	return d
}

func newDefaultClaudeRuntimeHostDriver(st *store.Store, manager *claudegen.Manager) *claudeRuntimeHostDriver {
	if manager == nil {
		manager = claudegen.Default()
	}
	driver := newClaudeRuntimeHostDriver(st, claudebridge.DriverOptions{ResolveActive: func(ctx context.Context) (claudebridge.LaunchSpec, error) {
		spec, err := manager.ResolveActive(ctx)
		if err != nil {
			return claudebridge.LaunchSpec{}, err
		}
		return claudebridge.LaunchSpec{NodePath: spec.NodePath, BridgePath: spec.BridgePath, Manifest: spec.Manifest}, nil
	}})
	driver.preflight = manager.Preflight
	return driver
}

func (d *claudeRuntimeHostDriver) Preflight(ctx context.Context) error {
	if d == nil || d.bridge == nil {
		return errors.New("Claude Runtime Host Driver is unavailable")
	}
	if d.preflight == nil {
		return errors.New("Claude Runtime generation preflight is unavailable")
	}
	return d.preflight(ctx)
}

func (d *claudeRuntimeHostDriver) Acquire(ctx context.Context, request AgentHostRequest) (AgentHost, error) {
	if err := d.Preflight(ctx); err != nil {
		return nil, err
	}
	d.mu.Lock()
	if d.shutdown {
		d.mu.Unlock()
		return nil, errors.New("Claude Runtime Host Driver is shut down")
	}
	if existing := d.handles[request.AgentID]; existing != nil && existing.Alive() {
		d.mu.Unlock()
		return existing, nil
	}
	d.mu.Unlock()
	bridge, err := d.bridge.Acquire(ctx, request.AgentID)
	if err != nil {
		return nil, err
	}
	contract := newClaudeRuntimeContract(request.AgentID, d.st, bridge)
	contract.cwd = request.Cwd
	host := &claudeAgentHost{bridge: bridge, contract: contract}
	contract.host = host
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shutdown {
		host.Close()
		return nil, errors.New("Claude Runtime Host Driver shut down during acquire")
	}
	if existing := d.handles[request.AgentID]; existing != nil && existing.Alive() {
		// Driver.Acquire deduplicates the per-Agent Bridge. A concurrent loser
		// therefore owns only this wrapper, not the shared Bridge itself.
		return existing, nil
	}
	d.handles[request.AgentID] = host
	return host, nil
}

func (d *claudeRuntimeHostDriver) HistoryContract(request AgentHostRequest) runtimecontract.Contract {
	return newClaudeRuntimeContract(request.AgentID, d.st, nil)
}

func (d *claudeRuntimeHostDriver) CapabilitySnapshot(context.Context, runtimecontract.Binding) runtimecontract.CapabilitySnapshot {
	return claudeControlPlaneCapabilitySnapshot()
}

func (d *claudeRuntimeHostDriver) Shutdown(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.shutdown = true
		handles := make([]*claudeAgentHost, 0, len(d.handles))
		for _, handle := range d.handles {
			handles = append(handles, handle)
		}
		d.mu.Unlock()
		for _, handle := range handles {
			handle.Close()
		}
	})
	return d.bridge.Shutdown(ctx)
}

func (d *claudeRuntimeHostDriver) onBridgeEvent(event claudebridge.Event) {
	d.mu.Lock()
	host := d.handles[event.AgentID]
	d.mu.Unlock()
	if event.Kind == "binding_resumed" {
		if host != nil {
			host.contract.forgetOperation(event.Operation)
		}
		return
	}
	if event.Kind == "interrupt_receipt" {
		return
	}
	if host != nil {
		host.contract.handleBridgeEvent(event)
	}
}

func (d *claudeRuntimeHostDriver) onBridgeFailure(agentID string, err error) {
	d.mu.Lock()
	host := d.handles[agentID]
	d.mu.Unlock()
	if host != nil {
		host.fail(host.contract.runtimeFailure(err))
	}
}

type claudeAgentHost struct {
	mu       sync.Mutex
	bridge   *claudebridge.Bridge
	contract *claudeRuntimeContract
	failure  func(error)
	closed   bool
}

func (h *claudeAgentHost) Alive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.closed && h.bridge != nil && h.bridge.Alive() && h.contract.ledgerHealthy()
}

func (h *claudeAgentHost) Contract() runtimecontract.Contract { return h.contract }

func (h *claudeAgentHost) SetFailureHandler(handler func(error)) {
	h.mu.Lock()
	h.failure = handler
	h.mu.Unlock()
}

func (h *claudeAgentHost) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	h.failure = nil
	bridge := h.bridge
	h.mu.Unlock()
	h.contract.SetEventHandler(nil)
	bridge.Close()
}

func (h *claudeAgentHost) fail(err error) {
	h.mu.Lock()
	handler, closed := h.failure, h.closed
	h.mu.Unlock()
	if !closed && handler != nil {
		handler(err)
	}
}

type claudeRuntimeContract struct {
	agentID string
	st      *store.Store
	bridge  *claudebridge.Bridge
	host    *claudeAgentHost

	mu                        sync.Mutex
	handler                   func(runtimecontract.Event)
	approvalHandler           func(runtimecontract.ApprovalProposal)
	needsYouHandler           func(runtimecontract.NeedsYouProposal) error
	approvalPolicy            string
	callbacks                 map[string]claudeCallback
	callbackProposals         map[string]string
	turns                     []runtimecontract.HistoryTurn
	terminal                  map[string]bool
	waiting                   map[string]bool
	fenced                    map[string]bool
	pendingOps                map[string]bool
	terminalOps               map[string]string
	observed                  bool
	ledgerErr                 error
	cwd                       string
	release                   func()
	ops                       atomic.Uint64
	opPhases                  map[string]runtimecontract.FailurePhase
	opTurns                   map[string]string
	afterLedgerCommitForTest  func()
	beforeLedgerCommitForTest func()
}

type claudeCallback struct {
	callbackID  string
	toolCallID  string
	turnID      string
	fingerprint string
	settled     bool
}

func newClaudeRuntimeContract(agentID string, st *store.Store, bridge *claudebridge.Bridge) *claudeRuntimeContract {
	c := &claudeRuntimeContract{
		agentID: agentID, st: st, bridge: bridge, terminal: map[string]bool{}, waiting: map[string]bool{}, fenced: map[string]bool{}, pendingOps: map[string]bool{}, terminalOps: map[string]string{},
		opPhases: map[string]runtimecontract.FailurePhase{}, opTurns: map[string]string{}, callbacks: map[string]claudeCallback{}, callbackProposals: map[string]string{},
	}
	if st != nil {
		if history, err := st.LoadCanonicalTurnLedger(agentID, int(^uint(0)>>1), 0); err == nil {
			c.turns = history.Turns
			for _, turn := range c.turns {
				if turn.State != runtimecontract.LifecycleAccepted {
					c.terminal[turn.TurnID] = true
				}
			}
		} else {
			c.ledgerErr = err
		}
	}
	return c
}

func (c *claudeRuntimeContract) SetApprovalHandler(handler func(runtimecontract.ApprovalProposal)) {
	c.mu.Lock()
	c.approvalHandler = handler
	c.mu.Unlock()
}

func (c *claudeRuntimeContract) SetRuntimeApprovalPolicy(policy string) {
	c.mu.Lock()
	c.approvalPolicy = policy
	c.mu.Unlock()
}

func (c *claudeRuntimeContract) ResolveApproval(ctx context.Context, proposalID string, decision runtimecontract.ApprovalDecision) error {
	c.mu.Lock()
	callback, ok := c.callbacks[proposalID]
	if ok && callback.settled {
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("Claude Runtime Approval proposal %s is unavailable", proposalID)
	}
	_, outcome := c.command(ctx, "resolve_approval", callback.turnID, runtimecontract.FailurePhaseTurnContinue, map[string]any{
		"callbackId": callback.callbackID, "decision": decision,
	})
	c.mu.Lock()
	callback.settled = true
	c.callbacks[proposalID] = callback
	c.mu.Unlock()
	if outcome.State != runtimecontract.LifecycleAccepted {
		return lifecycleOutcomeError(outcome)
	}
	return nil
}

func (c *claudeRuntimeContract) SetNeedsYouHandler(handler func(runtimecontract.NeedsYouProposal) error) {
	c.mu.Lock()
	c.needsYouHandler = handler
	c.mu.Unlock()
}

func (c *claudeRuntimeContract) ContractVersion() int { return runtimecontract.Version }

func (c *claudeRuntimeContract) ContextDeliveryMode() runtimecontract.ContextDeliveryMode {
	return runtimecontract.ContextDeliveryFullPerTurn
}

func (c *claudeRuntimeContract) CreateBinding(ctx context.Context, request runtimecontract.BindingRequest) (runtimecontract.Binding, runtimecontract.Outcome) {
	c.cwd = request.Cwd
	if err := ctx.Err(); err != nil {
		return runtimecontract.Binding{}, claudeFailure(runtimecontract.LifecycleRejected, runtimecontract.FailurePhaseBindingCreate, "binding_create_cancelled", err)
	}
	sessionRef, err := newClaudeSessionID()
	if err != nil {
		return runtimecontract.Binding{}, claudeFailure(runtimecontract.LifecycleFailed, runtimecontract.FailurePhaseBindingCreate, "binding_create_failed", err)
	}
	return runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "claude", NativeRef: sessionRef}, runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}
}

func (c *claudeRuntimeContract) ResumeBinding(ctx context.Context, binding runtimecontract.Binding) runtimecontract.Outcome {
	c.mu.Lock()
	unstarted, ledgerErr := len(c.turns) == 0 && !c.observed, c.ledgerErr
	c.mu.Unlock()
	if ledgerErr != nil {
		return claudeFailure(runtimecontract.LifecycleFailed, runtimecontract.FailurePhaseBindingResume, "ledger_unavailable", ledgerErr)
	}
	if unstarted {
		if err := ctx.Err(); err != nil {
			return claudeFailure(runtimecontract.LifecycleRejected, runtimecontract.FailurePhaseBindingResume, "binding_resume_cancelled", err)
		}
		return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
	}
	_, outcome := c.command(ctx, "resume_binding", "", runtimecontract.FailurePhaseBindingResume, map[string]any{"sessionRef": binding.NativeRef, "cwd": c.cwd})
	if outcome.State == runtimecontract.LifecycleAccepted {
		outcome.State = runtimecontract.LifecycleCompleted
	}
	return outcome
}

func (c *claudeRuntimeContract) seedTurnBindings(bindings map[string]string) {
	c.mu.Lock()
	c.observed = len(bindings) > 0
	c.mu.Unlock()
}

func (c *claudeRuntimeContract) StartTurn(ctx context.Context, request runtimecontract.TurnRequest) runtimecontract.Outcome {
	if err := c.persistLedgerFence(); err != nil {
		return claudeFailure(runtimecontract.LifecycleFailed, runtimecontract.FailurePhaseTurnStart, "ledger_unavailable", err)
	}
	c.mu.Lock()
	resume := len(c.turns) > 0 || c.observed
	c.mu.Unlock()
	response, outcome := c.command(ctx, "start_turn", request.TurnID, runtimecontract.FailurePhaseTurnStart, map[string]any{"sessionRef": request.Binding.NativeRef, "cwd": c.cwd, "resume": resume, "input": request.Input})
	if outcome.State != runtimecontract.LifecycleAccepted {
		return outcome
	}
	if err := c.ledgerFailure(); err != nil {
		return claudeFailure(runtimecontract.LifecycleIndeterminate, runtimecontract.FailurePhaseTurnStart, "ledger_commit_indeterminate", err)
	}
	var data struct {
		RuntimeTurnRef string `json:"runtimeTurnRef"`
	}
	if json.Unmarshal(response.Data, &data) != nil || data.RuntimeTurnRef == "" {
		return claudeFailure(runtimecontract.LifecycleFailed, runtimecontract.FailurePhaseTurnStart, "invalid_turn_binding", errors.New("Claude bridge returned no Turn binding"))
	}
	outcome.RuntimeTurnRef = data.RuntimeTurnRef
	return outcome
}

func (c *claudeRuntimeContract) ContinueTurn(ctx context.Context, request runtimecontract.CausalInput) runtimecontract.Outcome {
	if err := c.persistLedgerFence(); err != nil {
		return claudeFailure(runtimecontract.LifecycleFailed, runtimecontract.FailurePhaseTurnContinue, "ledger_unavailable", err)
	}
	_, outcome := c.command(ctx, "continue_turn", request.TurnID, runtimecontract.FailurePhaseTurnContinue, map[string]any{"sessionRef": request.Binding.NativeRef, "runtimeTurnRef": request.RuntimeTurnRef, "input": request.Input})
	if outcome.State == runtimecontract.LifecycleAccepted {
		if err := c.ledgerFailure(); err != nil {
			return claudeFailure(runtimecontract.LifecycleIndeterminate, runtimecontract.FailurePhaseTurnContinue, "ledger_commit_indeterminate", err)
		}
	}
	if outcome.State == runtimecontract.LifecycleAccepted {
		outcome.RuntimeTurnRef = request.RuntimeTurnRef
	}
	return outcome
}

func (c *claudeRuntimeContract) InterruptTurn(ctx context.Context, request runtimecontract.TurnTarget) runtimecontract.Outcome {
	if err := c.persistLedgerFence(); err != nil {
		return claudeFailure(runtimecontract.LifecycleFailed, runtimecontract.FailurePhaseTurnInterrupt, "ledger_unavailable", err)
	}
	_, outcome := c.command(ctx, "interrupt_turn", request.TurnID, runtimecontract.FailurePhaseTurnInterrupt, map[string]any{"runtimeTurnRef": request.RuntimeTurnRef})
	if outcome.State == runtimecontract.LifecycleAccepted {
		if err := c.ledgerFailure(); err != nil {
			return claudeFailure(runtimecontract.LifecycleIndeterminate, runtimecontract.FailurePhaseTurnInterrupt, "ledger_commit_indeterminate", err)
		}
		outcome.RuntimeTurnRef = request.RuntimeTurnRef
	}
	return outcome
}

func (c *claudeRuntimeContract) persistLedgerFence() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ledgerErr != nil {
		return c.ledgerErr
	}
	if c.st == nil {
		return errors.New("Canonical Turn Ledger Store is unavailable")
	}
	if err := c.st.SaveCanonicalTurnLedger(c.agentID, c.turns); err != nil {
		c.ledgerErr = err
		return err
	}
	return nil
}

func (c *claudeRuntimeContract) prepareTurnPersistence() error { return c.persistLedgerFence() }

func (c *claudeRuntimeContract) claimTurnUnsettled(turnID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminal[turnID] || c.fenced[turnID] {
		return false
	}
	c.fenced[turnID] = true
	return true
}

func (c *claudeRuntimeContract) ledgerFailure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ledgerErr
}

func (c *claudeRuntimeContract) forgetOperation(operation string) {
	c.mu.Lock()
	delete(c.opPhases, operation)
	delete(c.opTurns, operation)
	delete(c.pendingOps, operation)
	delete(c.terminalOps, operation)
	c.mu.Unlock()
}

func (c *claudeRuntimeContract) runtimeFailure(err error) error {
	var indeterminate *claudebridge.IndeterminateError
	if !errors.As(err, &indeterminate) {
		return err
	}
	c.mu.Lock()
	phase := c.opPhases[indeterminate.Operation]
	c.mu.Unlock()
	if phase == "" {
		phase = runtimecontract.FailurePhaseTurnStart
	}
	return &runtimeIndeterminateError{failure: &runtimecontract.Failure{
		Code: "transport_indeterminate", Phase: phase,
		Message: "Claude Runtime Turn outcome is indeterminate", Diagnostic: err.Error(), Cause: err,
	}}
}

func (c *claudeRuntimeContract) SetEventHandler(handler func(runtimecontract.Event)) {
	c.mu.Lock()
	c.handler = handler
	c.mu.Unlock()
}

func (c *claudeRuntimeContract) ReadHistory(ctx context.Context, request runtimecontract.HistoryRequest) (runtimecontract.History, *runtimecontract.Failure) {
	if err := ctx.Err(); err != nil {
		failure := claudeFailure(runtimecontract.LifecycleRejected, runtimecontract.FailurePhaseHistory, "history_cancelled", err)
		return runtimecontract.History{}, failure.Failure
	}
	if c.st == nil {
		failure := claudeFailure(runtimecontract.LifecycleFailed, runtimecontract.FailurePhaseHistory, "history_unavailable", errors.New("Canonical Turn Ledger is unavailable"))
		return runtimecontract.History{}, failure.Failure
	}
	c.mu.Lock()
	ledgerErr := c.ledgerErr
	c.mu.Unlock()
	if ledgerErr != nil {
		failure := claudeFailure(runtimecontract.LifecycleFailed, runtimecontract.FailurePhaseHistory, "history_unavailable", ledgerErr)
		return runtimecontract.History{}, failure.Failure
	}
	history, err := c.st.LoadCanonicalTurnLedger(c.agentID, request.Count, request.Offset)
	if err != nil {
		failure := claudeFailure(runtimecontract.LifecycleFailed, runtimecontract.FailurePhaseHistory, "history_unavailable", err)
		return runtimecontract.History{}, failure.Failure
	}
	return history, nil
}

func (c *claudeRuntimeContract) CapabilitySnapshot(context.Context, runtimecontract.Binding) runtimecontract.CapabilitySnapshot {
	return claudeControlPlaneCapabilitySnapshot()
}

func (c *claudeRuntimeContract) CloseBinding(context.Context, runtimecontract.Binding) runtimecontract.Outcome {
	if c.release != nil {
		c.release()
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}

func (c *claudeRuntimeContract) command(ctx context.Context, kind, turnID string, phase runtimecontract.FailurePhase, payload any) (claudebridge.Response, runtimecontract.Outcome) {
	if c.bridge == nil {
		return claudebridge.Response{}, claudeFailure(runtimecontract.LifecycleFailed, phase, "runtime_unavailable", errors.New("Claude bridge is unavailable"))
	}
	operation := fmt.Sprintf("%s-%d", c.agentID, c.ops.Add(1))
	c.mu.Lock()
	c.opPhases[operation] = phase
	c.opTurns[operation] = turnID
	c.pendingOps[operation] = true
	c.mu.Unlock()
	response, err := c.bridge.Request(ctx, claudebridge.Command{Kind: kind, TurnID: turnID, Operation: operation, Payload: payload})
	if err != nil {
		c.mu.Lock()
		runtimeTurnRef, settled := c.terminalOps[operation]
		delete(c.pendingOps, operation)
		delete(c.terminalOps, operation)
		delete(c.opPhases, operation)
		delete(c.opTurns, operation)
		c.mu.Unlock()
		if settled {
			data, _ := json.Marshal(map[string]string{"runtimeTurnRef": runtimeTurnRef})
			return claudebridge.Response{TurnID: turnID, Operation: operation, Accepted: true, Data: data}, runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: runtimeTurnRef}
		}
		state, code := runtimecontract.LifecycleFailed, "runtime_error"
		var indeterminate *claudebridge.IndeterminateError
		if errors.As(err, &indeterminate) {
			state, code = runtimecontract.LifecycleIndeterminate, "transport_indeterminate"
		}
		return claudebridge.Response{}, claudeFailure(state, phase, code, err)
	}
	c.mu.Lock()
	delete(c.pendingOps, operation)
	delete(c.terminalOps, operation)
	c.mu.Unlock()
	if !response.Accepted {
		c.forgetOperation(operation)
		return response, claudeFailure(runtimecontract.LifecycleRejected, phase, "runtime_rejected", errors.New(publicClaudeError(response.Error)))
	}
	return response, runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}
}

func publicClaudeError(message string) string {
	return "Claude Runtime rejected the operation"
}

func claudeFailure(state runtimecontract.LifecycleState, phase runtimecontract.FailurePhase, code string, err error) runtimecontract.Outcome {
	return runtimecontract.Outcome{State: state, Failure: &runtimecontract.Failure{Code: code, Phase: phase, Message: "Claude Runtime operation failed", Diagnostic: err.Error(), Cause: err}}
}

func newClaudeSessionID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate Claude session binding: %w", err)
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
}

func (c *claudeRuntimeContract) handleBridgeEvent(source claudebridge.Event) {
	if source.Kind == "approval" {
		c.handleApproval(source)
		return
	}
	if source.Kind == "needs_you" {
		c.handleNeedsYou(source)
		return
	}
	event, err := claudeContractEvent(source)
	if err != nil {
		c.fail(err)
		return
	}
	c.mu.Lock()
	beforeCommit := c.beforeLedgerCommitForTest
	c.mu.Unlock()
	if beforeCommit != nil {
		beforeCommit()
	}
	c.mu.Lock()
	if c.fenced[event.TurnID] {
		c.mu.Unlock()
		return
	}
	if event.Kind == runtimecontract.EventTerminal && c.pendingOps[source.Operation] {
		c.terminalOps[source.Operation] = event.RuntimeTurnRef
	}
	if c.terminal[event.TurnID] {
		c.mu.Unlock()
		return
	}
	if c.ledgerErr != nil {
		c.mu.Unlock()
		return
	}
	if event.Kind == runtimecontract.EventTerminal && c.waiting[event.TurnID] {
		event.Outcome = &runtimecontract.Outcome{State: runtimecontract.LifecycleInterrupted, RuntimeTurnRef: event.RuntimeTurnRef}
	}
	nextTurns := cloneClaudeLedgerTurns(c.turns)
	nextTerminal := make(map[string]bool, len(c.terminal)+1)
	for turnID, terminal := range c.terminal {
		nextTerminal[turnID] = terminal
	}
	turn := claudeLedgerTurn(&nextTurns, event.TurnID)
	switch event.Kind {
	case runtimecontract.EventTurnStarted:
		turn.State = runtimecontract.LifecycleAccepted
		turn.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	case runtimecontract.EventContent:
		projected, ok := projectRuntimeContentBlock(&AgentView{Agent: Agent{ID: c.agentID}}, event.TurnID, *event.Content)
		if !ok {
			c.mu.Unlock()
			return
		}
		replaced := false
		for index := range turn.Content {
			if turn.Content[index].ID == projected.ID {
				turn.Content[index], replaced = projected, true
				break
			}
		}
		if !replaced {
			turn.Content = append(turn.Content, projected)
		}
	case runtimecontract.EventUsage:
		usage := *event.Usage
		turn.Usage = &usage
	case runtimecontract.EventTerminal:
		turn.State = event.Outcome.State
		turn.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
		nextTerminal[event.TurnID] = true
	}
	handler := c.handler
	if c.st == nil {
		c.mu.Unlock()
		c.fail(errors.New("persist Canonical Turn Ledger before canonical event: Store is unavailable"))
		return
	}
	if err := c.st.SaveCanonicalTurnLedger(c.agentID, nextTurns); err != nil {
		c.ledgerErr = err
		phase := c.opPhases[source.Operation]
		c.mu.Unlock()
		if phase == "" {
			phase = runtimecontract.FailurePhaseTurnStart
		}
		go c.fail(&runtimeIndeterminateError{failure: &runtimecontract.Failure{
			Code: "ledger_commit_indeterminate", Phase: phase,
			Message: "Claude Runtime Turn outcome is indeterminate", Diagnostic: err.Error(), Cause: err,
		}})
		return
	}
	c.turns, c.terminal = nextTurns, nextTerminal
	if event.Kind == runtimecontract.EventTerminal {
		delete(c.waiting, event.TurnID)
		for operation, turnID := range c.opTurns {
			if turnID == event.TurnID {
				delete(c.opPhases, operation)
				delete(c.opTurns, operation)
			}
		}
	}
	afterCommit := c.afterLedgerCommitForTest
	c.mu.Unlock()
	if afterCommit != nil {
		afterCommit()
	}
	if handler != nil {
		handler(event)
	}
}

func (c *claudeRuntimeContract) handleApproval(source claudebridge.Event) {
	var data struct {
		CallbackID string         `json:"callbackId"`
		ToolCallID string         `json:"toolCallId"`
		ToolName   string         `json:"toolName"`
		Input      map[string]any `json:"input"`
	}
	if json.Unmarshal(source.Data, &data) != nil || data.CallbackID == "" || data.ToolCallID == "" || data.ToolName == "" {
		c.fail(errors.New("Claude bridge emitted malformed Approval"))
		return
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(source.Data))
	c.mu.Lock()
	if existingID := c.callbackProposals[data.CallbackID]; existingID != "" {
		existing := c.callbacks[existingID]
		handler := c.approvalHandler
		c.mu.Unlock()
		if existing.turnID != source.TurnID || existing.fingerprint != fingerprint {
			c.fail(errors.New("Claude Runtime Approval callback diverged"))
		} else if proposal, safe := claudeApprovalProposal(existingID, source.TurnID, data.ToolCallID, data.ToolName, data.Input); !safe {
			go func() { _ = c.ResolveApproval(context.Background(), existingID, runtimecontract.ApprovalAbort) }()
		} else if handler != nil {
			handler(proposal)
		}
		return
	}
	proposalID := "runtime-approval-" + strings.TrimPrefix(newIntegrationID("proposal"), "proposal_")
	c.callbacks[proposalID] = claudeCallback{callbackID: data.CallbackID, toolCallID: data.ToolCallID, turnID: source.TurnID, fingerprint: fingerprint}
	c.callbackProposals[data.CallbackID] = proposalID
	handler := c.approvalHandler
	policy := c.approvalPolicy
	c.mu.Unlock()
	proposal, safe := claudeApprovalProposal(proposalID, source.TurnID, data.ToolCallID, data.ToolName, data.Input)
	if !safe {
		go func() { _ = c.ResolveApproval(context.Background(), proposalID, runtimecontract.ApprovalAbort) }()
		return
	}
	if policy == "never" {
		go func() {
			_ = c.ResolveApproval(context.Background(), proposalID, runtimecontract.ApprovalApprove)
		}()
		return
	}
	if handler == nil {
		return
	}
	handler(proposal)
}

func claudeApprovalProposal(id, turnID, toolCallID, toolName string, input map[string]any) (runtimecontract.ApprovalProposal, bool) {
	if !safeClaudeToolName(toolName) || !safeClaudeApprovalValue(input, 0) {
		return runtimecontract.ApprovalProposal{}, false
	}
	arguments := make([]runtimecontract.ApprovalArgument, 0, len(input))
	total := 0
	for name, value := range input {
		encoded, err := json.Marshal(value)
		if err != nil || len(encoded) > 16<<10 {
			return runtimecontract.ApprovalProposal{}, false
		}
		total += len(encoded)
		arguments = append(arguments, runtimecontract.ApprovalArgument{Name: name, Value: strings.Trim(string(encoded), `"`)})
	}
	if total > 64<<10 {
		return runtimecontract.ApprovalProposal{}, false
	}
	sort.Slice(arguments, func(i, j int) bool { return arguments[i].Name < arguments[j].Name })
	action := "tool/" + toolName
	if !knownClaudeTool(toolName) {
		action = "tool/custom"
	}
	return runtimecontract.ApprovalProposal{ID: id, ToolCallID: toolCallID, TurnID: turnID, ToolName: toolName, Action: action, Arguments: arguments, Timeout: 5 * time.Minute}, true
}

func safeClaudeToolName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("_.:-", char)) {
			return false
		}
	}
	return true
}

func safeClaudeApprovalValue(value any, depth int) bool {
	if depth > 8 {
		return false
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if !approvalActionKey(key) || !safeClaudeApprovalValue(nested, depth+1) {
				return false
			}
		}
	case []any:
		for _, nested := range typed {
			if !safeClaudeApprovalValue(nested, depth+1) {
				return false
			}
		}
	}
	return true
}

func knownClaudeTool(name string) bool {
	switch strings.ToLower(name) {
	case "bash", "read", "edit", "write", "webfetch", "websearch", "notebookedit", "task", "skill":
		return true
	default:
		return false
	}
}

func (c *claudeRuntimeContract) handleNeedsYou(source claudebridge.Event) {
	var data struct {
		CallbackID string `json:"callbackId"`
		ToolCallID string `json:"toolCallId"`
		Questions  []struct {
			Question string                           `json:"question"`
			Options  []runtimecontract.NeedsYouOption `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(source.Data, &data) != nil || data.CallbackID == "" || data.ToolCallID == "" || len(data.Questions) != 1 || strings.TrimSpace(data.Questions[0].Question) == "" {
		c.fail(errors.New("Claude bridge emitted malformed Needs You request"))
		return
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(source.Data))
	c.mu.Lock()
	proposalID := c.callbackProposals[data.CallbackID]
	if proposalID == "" {
		proposalID = newIntegrationID("hrq")
		c.callbackProposals[data.CallbackID] = proposalID
		c.callbacks[proposalID] = claudeCallback{callbackID: data.CallbackID, toolCallID: data.ToolCallID, turnID: source.TurnID, fingerprint: fingerprint}
	} else if existing := c.callbacks[proposalID]; existing.turnID != source.TurnID || existing.fingerprint != fingerprint {
		c.mu.Unlock()
		c.fail(errors.New("Claude Runtime Needs You callback diverged"))
		return
	}
	handler := c.needsYouHandler
	c.mu.Unlock()
	if handler == nil {
		return
	}
	err := handler(runtimecontract.NeedsYouProposal{ID: proposalID, TurnID: source.TurnID, Question: data.Questions[0].Question, Options: data.Questions[0].Options})
	if err == nil {
		c.mu.Lock()
		c.waiting[source.TurnID] = true
		c.mu.Unlock()
	}
	if c.bridge == nil {
		return
	}
	go func() {
		_, outcome := c.command(context.Background(), "resolve_needs_you", source.TurnID, runtimecontract.FailurePhaseTurnContinue, map[string]any{"callbackId": data.CallbackID, "persisted": err == nil})
		c.mu.Lock()
		callback := c.callbacks[proposalID]
		callback.settled = true
		c.callbacks[proposalID] = callback
		c.mu.Unlock()
		if outcome.State != runtimecontract.LifecycleAccepted && err == nil {
			c.fail(errors.New("Claude Runtime could not release its Needs You callback"))
		}
	}()
}

func cloneClaudeLedgerTurns(turns []runtimecontract.HistoryTurn) []runtimecontract.HistoryTurn {
	cloned := append([]runtimecontract.HistoryTurn(nil), turns...)
	for index := range cloned {
		cloned[index].Content = append([]runtimecontract.ContentBlock(nil), cloned[index].Content...)
		if cloned[index].Usage != nil {
			usage := *cloned[index].Usage
			cloned[index].Usage = &usage
		}
	}
	return cloned
}

func (c *claudeRuntimeContract) ledgerHealthy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ledgerErr == nil
}

func claudeLedgerTurn(turns *[]runtimecontract.HistoryTurn, turnID string) *runtimecontract.HistoryTurn {
	for index := range *turns {
		if (*turns)[index].TurnID == turnID {
			return &(*turns)[index]
		}
	}
	*turns = append(*turns, runtimecontract.HistoryTurn{TurnID: turnID, State: runtimecontract.LifecycleAccepted, Content: []runtimecontract.ContentBlock{}})
	return &(*turns)[len(*turns)-1]
}

func (c *claudeRuntimeContract) fail(err error) {
	if c.host != nil {
		c.host.fail(err)
	}
}

func claudeContractEvent(source claudebridge.Event) (runtimecontract.Event, error) {
	event := runtimecontract.Event{TurnID: source.TurnID}
	switch source.Kind {
	case "turn_started":
		event.Kind = runtimecontract.EventTurnStarted
		var data struct {
			RuntimeTurnRef string `json:"runtimeTurnRef"`
		}
		if json.Unmarshal(source.Data, &data) != nil || data.RuntimeTurnRef == "" {
			return runtimecontract.Event{}, errors.New("Claude bridge emitted malformed turn_started")
		}
		event.RuntimeTurnRef = data.RuntimeTurnRef
	case "content":
		var data struct {
			RuntimeTurnRef string                       `json:"runtimeTurnRef"`
			Phase          runtimecontract.ContentPhase `json:"phase"`
			Content        runtimecontract.ContentBlock `json:"content"`
		}
		if json.Unmarshal(source.Data, &data) != nil {
			return runtimecontract.Event{}, errors.New("Claude bridge emitted malformed content")
		}
		event.Kind, event.RuntimeTurnRef, event.ContentPhase, event.Content = runtimecontract.EventContent, data.RuntimeTurnRef, data.Phase, &data.Content
	case "usage":
		var data struct {
			RuntimeTurnRef string                `json:"runtimeTurnRef"`
			Usage          runtimecontract.Usage `json:"usage"`
		}
		if json.Unmarshal(source.Data, &data) != nil {
			return runtimecontract.Event{}, errors.New("Claude bridge emitted malformed usage")
		}
		event.Kind, event.RuntimeTurnRef, event.Usage = runtimecontract.EventUsage, data.RuntimeTurnRef, &data.Usage
	case "turn_completed", "turn_failed", "turn_interrupted":
		var data struct {
			RuntimeTurnRef string `json:"runtimeTurnRef"`
			Message        string `json:"message"`
		}
		if json.Unmarshal(source.Data, &data) != nil {
			return runtimecontract.Event{}, errors.New("Claude bridge emitted malformed terminal")
		}
		state := runtimecontract.LifecycleCompleted
		if source.Kind == "turn_failed" {
			state = runtimecontract.LifecycleFailed
		}
		if source.Kind == "turn_interrupted" {
			state = runtimecontract.LifecycleInterrupted
		}
		outcome := runtimecontract.Outcome{State: state, RuntimeTurnRef: data.RuntimeTurnRef}
		if state == runtimecontract.LifecycleFailed {
			outcome.Failure = &runtimecontract.Failure{Code: "runtime_failed", Phase: runtimecontract.FailurePhaseTurnStart, Message: "Claude Runtime Turn failed"}
		}
		event.Kind, event.RuntimeTurnRef, event.Outcome = runtimecontract.EventTerminal, data.RuntimeTurnRef, &outcome
	default:
		return runtimecontract.Event{}, fmt.Errorf("unsupported mandatory Claude bridge event %q", source.Kind)
	}
	if err := event.Validate(); err != nil {
		return runtimecontract.Event{}, err
	}
	return event, nil
}
