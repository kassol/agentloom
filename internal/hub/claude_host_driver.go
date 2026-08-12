package hub

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
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
	if event.Kind == "binding_resumed" {
		return
	}
	d.mu.Lock()
	host := d.handles[event.AgentID]
	d.mu.Unlock()
	if host != nil {
		host.contract.handleBridgeEvent(event)
	}
}

func (d *claudeRuntimeHostDriver) onBridgeFailure(agentID string, err error) {
	d.mu.Lock()
	host := d.handles[agentID]
	d.mu.Unlock()
	if host != nil {
		host.fail(err)
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

	mu        sync.Mutex
	handler   func(runtimecontract.Event)
	turns     []runtimecontract.HistoryTurn
	terminal  map[string]bool
	observed  bool
	ledgerErr error
	cwd       string
	release   func()
	ops       atomic.Uint64
}

func newClaudeRuntimeContract(agentID string, st *store.Store, bridge *claudebridge.Bridge) *claudeRuntimeContract {
	c := &claudeRuntimeContract{agentID: agentID, st: st, bridge: bridge, terminal: map[string]bool{}}
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
	c.mu.Lock()
	resume := len(c.turns) > 0 || c.observed
	c.mu.Unlock()
	response, outcome := c.command(ctx, "start_turn", request.TurnID, runtimecontract.FailurePhaseTurnStart, map[string]any{"sessionRef": request.Binding.NativeRef, "cwd": c.cwd, "resume": resume, "input": request.Input})
	if outcome.State != runtimecontract.LifecycleAccepted {
		return outcome
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
	_, outcome := c.command(ctx, "continue_turn", request.TurnID, runtimecontract.FailurePhaseTurnContinue, map[string]any{"sessionRef": request.Binding.NativeRef, "runtimeTurnRef": request.RuntimeTurnRef, "input": request.Input})
	if outcome.State == runtimecontract.LifecycleAccepted {
		outcome.RuntimeTurnRef = request.RuntimeTurnRef
	}
	return outcome
}

func (c *claudeRuntimeContract) InterruptTurn(ctx context.Context, request runtimecontract.TurnTarget) runtimecontract.Outcome {
	_, outcome := c.command(ctx, "interrupt_turn", request.TurnID, runtimecontract.FailurePhaseTurnInterrupt, map[string]any{"runtimeTurnRef": request.RuntimeTurnRef})
	if outcome.State == runtimecontract.LifecycleAccepted {
		outcome.State = runtimecontract.LifecycleInterrupted
		outcome.RuntimeTurnRef = request.RuntimeTurnRef
	}
	return outcome
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
	response, err := c.bridge.Request(ctx, claudebridge.Command{Kind: kind, TurnID: turnID, Operation: operation, Payload: payload})
	if err != nil {
		state, code := runtimecontract.LifecycleFailed, "runtime_error"
		var indeterminate *claudebridge.IndeterminateError
		if errors.As(err, &indeterminate) {
			state, code = runtimecontract.LifecycleIndeterminate, "transport_indeterminate"
		}
		return claudebridge.Response{}, claudeFailure(state, phase, code, err)
	}
	if !response.Accepted {
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
	event, err := claudeContractEvent(source)
	if err != nil {
		c.fail(err)
		return
	}
	c.mu.Lock()
	if c.terminal[event.TurnID] {
		c.mu.Unlock()
		return
	}
	if c.ledgerErr != nil {
		c.mu.Unlock()
		return
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
		c.mu.Unlock()
		return
	}
	c.turns, c.terminal = nextTurns, nextTerminal
	c.mu.Unlock()
	if handler != nil {
		handler(event)
	}
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
