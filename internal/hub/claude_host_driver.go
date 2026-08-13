package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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
	st               *store.Store
	bridge           *claudebridge.Driver
	preflight        func(context.Context) error
	activeGeneration func() (string, error)

	mu         sync.Mutex
	handles    map[string]*claudeAgentHost
	shutdown   bool
	closeOnce  sync.Once
	catalogMu  sync.Mutex
	catalogOps atomic.Uint64
}

type claudeConversationEvidence struct {
	SessionRef string `json:"sessionRef"`
	Name       string `json:"name"`
	Cwd        string `json:"cwd"`
	UpdatedAt  string `json:"updatedAt"`
	Revision   string `json:"revision"`
	Compatible bool   `json:"compatible"`
}

func (d *claudeRuntimeHostDriver) catalogRequest(ctx context.Context, command string, payload any) (claudebridge.Response, error) {
	if err := d.Preflight(ctx); err != nil {
		return claudebridge.Response{}, err
	}
	// Passive catalog calls share a short-lived bridge identity. Serialize the
	// acquire/request/close sequence so one caller cannot close that shared
	// bridge while another request is still using it.
	d.catalogMu.Lock()
	defer d.catalogMu.Unlock()
	bridge, err := d.bridge.Acquire(ctx, "claude-passive-catalog")
	if err != nil {
		return claudebridge.Response{}, err
	}
	defer bridge.Close()
	return bridge.Request(ctx, claudebridge.Command{
		Kind: command, Operation: fmt.Sprintf("catalog-%d", d.catalogOps.Add(1)), Payload: payload,
	})
}

func (d *claudeRuntimeHostDriver) DiscoverConversations(ctx context.Context) ([]nativeConversationCandidate, error) {
	response, err := d.catalogRequest(ctx, "discover_sessions", nil)
	if err != nil || !response.Accepted {
		if err == nil {
			err = errors.New("Claude passive session discovery was rejected")
		}
		return nil, err
	}
	var data struct {
		Candidates []claudeConversationEvidence `json:"candidates"`
	}
	if json.Unmarshal(response.Data, &data) != nil {
		return nil, errors.New("Claude passive session discovery returned malformed evidence")
	}
	result := make([]nativeConversationCandidate, 0, len(data.Candidates))
	for _, evidence := range data.Candidates {
		candidate, candidateErr := claudeConversationCandidate(evidence)
		if candidateErr == nil {
			result = append(result, candidate)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt > result[j].UpdatedAt })
	return result, nil
}

func (d *claudeRuntimeHostDriver) InspectConversation(ctx context.Context, nativeRef string) (nativeConversationCandidate, error) {
	response, err := d.catalogRequest(ctx, "inspect_session", map[string]any{"sessionRef": nativeRef})
	if err != nil || !response.Accepted {
		if err == nil {
			err = errors.New("Claude passive session inspection was rejected")
		}
		return nativeConversationCandidate{}, err
	}
	var evidence claudeConversationEvidence
	if json.Unmarshal(response.Data, &evidence) != nil {
		return nativeConversationCandidate{}, errors.New("Claude passive session inspection returned malformed evidence")
	}
	return claudeConversationCandidate(evidence)
}

func claudeConversationCandidate(evidence claudeConversationEvidence) (nativeConversationCandidate, error) {
	if strings.TrimSpace(evidence.SessionRef) == "" || strings.TrimSpace(evidence.Revision) == "" {
		return nativeConversationCandidate{}, errors.New("Claude passive session identity evidence is incomplete")
	}
	compatible := evidence.Compatible && filepath.IsAbs(evidence.Cwd)
	reason := ""
	if !compatible {
		reason = "Claude session is not a compatible top-level workspace conversation"
	}
	return nativeConversationCandidate{
		RuntimeConversationCandidate: RuntimeConversationCandidate{
			ID: candidateToken("claude", evidence.SessionRef), Revision: candidateRevision(evidence.Revision),
			RuntimeKind: "claude", Name: evidence.Name, Cwd: evidence.Cwd, UpdatedAt: evidence.UpdatedAt,
			Compatible: compatible, Compatibility: reason,
		},
		nativeRef: evidence.SessionRef, nativeRevision: evidence.Revision,
	}, nil
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
	driver.activeGeneration = manager.ReadActiveGenerationID
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

func (d *claudeRuntimeHostDriver) RuntimeConfigurationDescriptor() RuntimeConfigurationDescriptor {
	return claudeRuntimeConfigurationDescriptor()
}

func (d *claudeRuntimeHostDriver) CapabilitySnapshotWithModelImageEvidence(ctx context.Context, evidence runtimeModelImageEvidence) runtimecontract.CapabilitySnapshot {
	available := false
	if d != nil && d.activeGeneration != nil && evidence.Available && evidence.GenerationID != "" && evidence.ModelID != "" {
		if generationID, err := d.activeGeneration(); err == nil {
			available = generationID == evidence.GenerationID
		}
	}
	return claudeControlPlaneCapabilitySnapshot(available)
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
	provider                  string
	model                     string
	effort                    string
	modelState                *runtimecontract.ModelControlState
	imageInput                bool
	generationID              string
	modelRevision             uint64
	runtimeConfiguration      RuntimeConfiguration
	configurationEvidence     *RuntimeConfigurationEvidence
	release                   func()
	ops                       atomic.Uint64
	opPhases                  map[string]runtimecontract.FailurePhase
	opTurns                   map[string]string
	pendingContext            map[string]*runtimecontract.ContextEvidence
	afterLedgerCommitForTest  func()
	beforeLedgerCommitForTest func()
	historyBoundary           *HistoryBoundary
	historyBoundaryCommit     func(string) error
	divergenceRevision        string
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
		opPhases: map[string]runtimecontract.FailurePhase{}, opTurns: map[string]string{}, callbacks: map[string]claudeCallback{}, callbackProposals: map[string]string{}, pendingContext: map[string]*runtimecontract.ContextEvidence{},
	}
	if bridge != nil {
		c.generationID = bridge.GenerationID()
	}
	if st != nil {
		if history, err := st.LoadCanonicalTurnLedger(agentID, int(^uint(0)>>1), 0); err == nil {
			c.turns = sanitizeClaudeHistory(history.Turns)
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

func (c *claudeRuntimeContract) SetRuntimeOwnerConfiguration(configuration RuntimeConfiguration) {
	c.mu.Lock()
	changed := c.runtimeConfiguration.Configured != configuration.Configured ||
		c.runtimeConfiguration.Authentication != configuration.Authentication ||
		strings.Join(c.runtimeConfiguration.SettingSources, "\x00") != strings.Join(configuration.SettingSources, "\x00")
	c.runtimeConfiguration = configuration
	c.runtimeConfiguration.SettingSources = append([]string(nil), configuration.SettingSources...)
	if changed {
		c.configurationEvidence = nil
		c.modelState = nil
		c.modelRevision++
	}
	c.mu.Unlock()
}

func (c *claudeRuntimeContract) SetRuntimeHistoryBoundary(boundary *HistoryBoundary, commit func(string) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if boundary == nil {
		c.historyBoundary, c.historyBoundaryCommit = nil, nil
		return
	}
	copy := *boundary
	c.historyBoundary, c.historyBoundaryCommit = &copy, commit
	c.observed = true
}

func (c *claudeRuntimeContract) NativeDivergenceRevision() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.divergenceRevision
}

// RuntimeConfigurationEvidence returns only the safe, bridge-verified shape.
// In particular it never exposes account identity, credential values, helper
// output, native paths, or raw Claude configuration.
func (c *claudeRuntimeContract) RuntimeConfigurationEvidence() (RuntimeConfigurationEvidence, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.configurationEvidence == nil {
		return RuntimeConfigurationEvidence{}, false
	}
	evidence := *c.configurationEvidence
	evidence.SettingSources = append([]string(nil), evidence.SettingSources...)
	evidence.Authentication.Evidence = append([]runtimecontract.CapabilityEvidence(nil), evidence.Authentication.Evidence...)
	return evidence, true
}

func (c *claudeRuntimeContract) runtimeConfigurationPayloadLocked() map[string]any {
	return map[string]any{
		"settingSources": append([]string(nil), c.runtimeConfiguration.SettingSources...),
		"authentication": c.runtimeConfiguration.Authentication,
	}
}

func (c *claudeRuntimeContract) rememberConfigurationEvidence(evidence RuntimeConfigurationEvidence) error {
	if evidence.Authentication.Category == "" || evidence.Authentication.Source == "" || evidence.Authentication.Validation != "accepted" {
		return errors.New("Claude bridge returned invalid configuration evidence")
	}
	for _, source := range evidence.SettingSources {
		if source != "user" && source != "project" && source != "local" {
			return errors.New("Claude bridge returned an invalid setting source")
		}
	}
	// The bridge contract permits only these three safe evidence fields. Drop
	// any future optional evidence rather than accidentally widening output.
	evidence.Authentication.Evidence = nil
	c.mu.Lock()
	if err := validateRuntimeConfigurationEvidence(c.runtimeConfiguration, evidence); err != nil {
		c.mu.Unlock()
		return err
	}
	c.configurationEvidence = &evidence
	c.mu.Unlock()
	return nil
}

func (c *claudeRuntimeContract) InspectRuntimeOwnerConfiguration(ctx context.Context, _ runtimecontract.Binding, cwd string, configuration RuntimeConfiguration) (RuntimeConfigurationEvidence, *runtimecontract.Failure) {
	response, outcome := c.command(ctx, "inspect_configuration", "", runtimecontract.FailurePhaseRuntimeConfiguration, map[string]any{
		"cwd": cwd,
		"configuration": map[string]any{
			"settingSources": append([]string(nil), configuration.SettingSources...),
			"authentication": configuration.Authentication,
		},
	})
	if outcome.State != runtimecontract.LifecycleAccepted {
		return RuntimeConfigurationEvidence{}, outcome.Failure
	}
	var evidence RuntimeConfigurationEvidence
	if err := json.Unmarshal(response.Data, &evidence); err != nil {
		return RuntimeConfigurationEvidence{}, runtimeConfigurationFailure(errors.New("Claude bridge returned malformed configuration evidence"))
	}
	evidence.Authentication.Evidence = nil
	if err := validateRuntimeConfigurationEvidence(configuration, evidence); err != nil {
		return RuntimeConfigurationEvidence{}, runtimeConfigurationFailure(err)
	}
	return evidence, nil
}

func runtimeConfigurationFailure(err error) *runtimecontract.Failure {
	return &runtimecontract.Failure{
		Code: "runtime_configuration_invalid", Phase: runtimecontract.FailurePhaseRuntimeConfiguration,
		Message: "Claude Runtime configuration could not be validated", Diagnostic: err.Error(), Cause: err,
	}
}

func (c *claudeRuntimeContract) SetRuntimeProvider(provider, model string) {
	c.mu.Lock()
	provider, model = strings.TrimSpace(provider), strings.TrimSpace(model)
	if provider == "" && model == "" {
		provider, model = "anthropic", "default"
	}
	if c.provider != provider || c.model != model {
		c.provider, c.model, c.modelState, c.imageInput = provider, model, nil, false
		c.modelRevision++
	}
	c.mu.Unlock()
}

func (c *claudeRuntimeContract) SetRuntimeModel(model string) {
	c.mu.Lock()
	if model = strings.TrimSpace(model); c.model != model {
		c.model, c.modelState, c.imageInput = model, nil, false
		c.modelRevision++
	}
	c.mu.Unlock()
}

func (c *claudeRuntimeContract) SetRuntimeEffort(effort string) {
	c.mu.Lock()
	effort = strings.TrimSpace(effort)
	if effort == "" {
		effort = runtimecontract.ThinkingLevelDefault
	}
	if c.effort != effort {
		c.effort, c.modelState = effort, nil
		c.modelRevision++
	}
	c.mu.Unlock()
}

func (c *claudeRuntimeContract) SetRuntimeModelImageEvidence(evidence runtimeModelImageEvidence) {
	c.mu.Lock()
	c.imageInput = evidence.Available && evidence.GenerationID != "" && evidence.GenerationID == c.generationID && evidence.ModelID == c.model
	c.mu.Unlock()
}

func (c *claudeRuntimeContract) RuntimeModelImageEvidence() runtimeModelImageEvidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	return runtimeModelImageEvidence{Available: c.imageInput, GenerationID: c.generationID, ModelID: c.model}
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

func (c *claudeRuntimeContract) InspectContextEvidence(ctx context.Context, _ runtimecontract.Binding, query runtimecontract.ContextEvidenceQuery) (runtimecontract.ContextEvidence, *runtimecontract.Failure) {
	base := runtimecontract.ContextEvidence{State: runtimecontract.ContextEvidenceUnknown, TurnID: query.TurnID, Mode: runtimecontract.ContextDeliveryFullPerTurn, Sources: []runtimecontract.ContextEvidenceSource{}, Deliveries: []runtimecontract.ContextEvidenceDelivery{}, UnsupportedDimensions: []string{"coverage", "epoch", "replay", "resend"}}
	if err := ctx.Err(); err != nil {
		base.State, base.Reason = runtimecontract.ContextEvidenceUnavailable, "Runtime evidence inspection was cancelled"
		return base, nil
	}
	if c.st == nil {
		base.State, base.Reason = runtimecontract.ContextEvidenceUnavailable, "Canonical Turn Ledger is unavailable"
		return base, nil
	}
	history, err := c.st.LoadCanonicalTurnLedger(c.agentID, int(^uint(0)>>1), 0)
	if err != nil {
		base.State, base.Reason = runtimecontract.ContextEvidenceUnavailable, "Canonical Turn Ledger is unreadable"
		return base, nil
	}
	for _, turn := range history.Turns {
		if turn.TurnID != query.TurnID {
			continue
		}
		if turn.ContextEvidence == nil {
			base.Reason = "No durable Loom context evidence exists for this Turn"
			return base, nil
		}
		if err := validateClaudeContextEvidence(*turn.ContextEvidence); err != nil {
			base.State, base.Reason = runtimecontract.ContextEvidenceUnavailable, "Canonical Turn Ledger contains malformed Claude context evidence"
			return base, nil
		}
		report := *turn.ContextEvidence
		report.Sources = append([]runtimecontract.ContextEvidenceSource(nil), report.Sources...)
		report.Deliveries = append([]runtimecontract.ContextEvidenceDelivery(nil), report.Deliveries...)
		return report, nil
	}
	base.Reason = "No durable Runtime correlation exists for this Loom Turn"
	return base, nil
}

func (c *claudeRuntimeContract) InspectUsage(ctx context.Context, _ runtimecontract.Binding) (runtimecontract.UsageReport, *runtimecontract.Failure) {
	if err := ctx.Err(); err != nil {
		return runtimecontract.UsageReport{}, claudeUsageFailure(err)
	}
	if c.st == nil {
		return runtimecontract.UsageReport{}, claudeUsageFailure(errors.New("Canonical Turn Ledger is unavailable"))
	}
	history, err := c.st.LoadCanonicalTurnLedger(c.agentID, int(^uint(0)>>1), 0)
	if err != nil {
		return runtimecontract.UsageReport{}, claudeUsageFailure(err)
	}
	return projectClaudeUsage(history.Turns), nil
}

func (c *claudeRuntimeContract) InspectModelControl(ctx context.Context, binding runtimecontract.Binding) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
	c.mu.Lock()
	provider, model, effort, cached, revision := c.provider, c.model, c.effort, c.modelState, c.modelRevision
	configuration := c.runtimeConfigurationPayloadLocked()
	c.mu.Unlock()
	if cached != nil {
		return cloneClaudeModelState(*cached), nil
	}
	response, outcome := c.command(ctx, "inspect_model_control", "", runtimecontract.FailurePhaseModelControl, map[string]any{"provider": provider, "model": model, "thinkingLevel": effort, "configuration": configuration})
	if outcome.State != runtimecontract.LifecycleAccepted {
		return runtimecontract.ModelControlState{}, outcome.Failure
	}
	var state runtimecontract.ModelControlState
	if err := json.Unmarshal(response.Data, &state); err != nil {
		return runtimecontract.ModelControlState{}, modelControlFailure(errors.New("Claude bridge returned malformed model control evidence"))
	}
	if err := state.Validate(); err != nil {
		return runtimecontract.ModelControlState{}, modelControlFailure(fmt.Errorf("invalid Claude model control evidence: %w", err))
	}
	c.mu.Lock()
	if c.modelRevision != revision || c.provider != provider || c.model != model || c.effort != effort {
		current := c.modelState
		c.mu.Unlock()
		if current != nil {
			return cloneClaudeModelState(*current), nil
		}
		return runtimecontract.ModelControlState{}, modelControlFailure(errors.New("Claude model configuration changed during inspection; retry"))
	}
	copy := cloneClaudeModelState(state)
	c.imageInput = state.Current.ImageInput
	c.modelState = &copy
	c.mu.Unlock()
	return state, nil
}

func (c *claudeRuntimeContract) SelectModel(ctx context.Context, binding runtimecontract.Binding, selection runtimecontract.ModelSelection) (runtimecontract.ModelControlState, *runtimecontract.Failure) {
	preview, failure := c.InspectModelControl(ctx, binding)
	if failure != nil {
		return runtimecontract.ModelControlState{}, failure
	}
	if err := preview.ValidateSelection(selection); err != nil {
		return runtimecontract.ModelControlState{}, modelControlFailure(err)
	}
	c.mu.Lock()
	cwd := c.cwd
	resume := len(c.turns) > 0 || c.observed
	configuration := c.runtimeConfigurationPayloadLocked()
	c.mu.Unlock()
	response, outcome := c.command(ctx, "select_model", "", runtimecontract.FailurePhaseModelControl, map[string]any{
		"sessionRef":    binding.NativeRef,
		"cwd":           cwd,
		"resume":        resume,
		"current":       runtimecontract.ModelSelection{Provider: preview.Current.Provider, Model: preview.Current.ID, ThinkingLevel: preview.ThinkingLevel},
		"selection":     selection,
		"configuration": configuration,
	})
	if outcome.State != runtimecontract.LifecycleAccepted {
		if outcome.State == runtimecontract.LifecycleIndeterminate && outcome.Failure != nil {
			failure := *outcome.Failure
			failure.Code = "model_selection_indeterminate"
			return runtimecontract.ModelControlState{}, &failure
		}
		return runtimecontract.ModelControlState{}, outcome.Failure
	}
	var state runtimecontract.ModelControlState
	if err := json.Unmarshal(response.Data, &state); err != nil {
		return runtimecontract.ModelControlState{}, claudeModelSelectionIndeterminate(errors.New("Claude bridge returned malformed effective model evidence"))
	}
	if err := state.Validate(); err != nil {
		return runtimecontract.ModelControlState{}, claudeModelSelectionIndeterminate(fmt.Errorf("invalid Claude effective model evidence: %w", err))
	}
	c.mu.Lock()
	c.provider, c.model, c.effort = state.Current.Provider, state.Current.ID, state.ThinkingLevel
	c.imageInput = state.Current.ImageInput
	c.modelRevision++
	copy := cloneClaudeModelState(state)
	c.modelState = &copy
	c.mu.Unlock()
	return state, nil
}

func (c *claudeRuntimeContract) InspectResources(ctx context.Context, request runtimecontract.ResourceInventoryRequest) (runtimecontract.ResourceInventory, *runtimecontract.Failure) {
	c.mu.Lock()
	configuration := c.runtimeConfigurationPayloadLocked()
	c.mu.Unlock()
	response, outcome := c.command(ctx, "inspect_resources", "", runtimecontract.FailurePhaseResourceInventory, map[string]any{
		"cwd": request.Cwd, "configuration": configuration,
	})
	if outcome.State != runtimecontract.LifecycleAccepted {
		return runtimecontract.ResourceInventory{}, outcome.Failure
	}
	var data struct {
		Resources     []runtimecontract.Resource   `json:"resources"`
		Configuration RuntimeConfigurationEvidence `json:"configuration"`
	}
	if err := json.Unmarshal(response.Data, &data); err != nil {
		return runtimecontract.ResourceInventory{}, claudeResourceFailure("resource_inventory_invalid", errors.New("Claude bridge returned malformed resource evidence"))
	}
	if err := c.rememberConfigurationEvidence(data.Configuration); err != nil {
		return runtimecontract.ResourceInventory{}, claudeResourceFailure("resource_inventory_invalid", err)
	}
	for index := range data.Resources {
		resource := &data.Resources[index]
		switch resource.Kind {
		case runtimecontract.ResourceSkill, runtimecontract.ResourceCommand, runtimecontract.ResourceExtension, runtimecontract.ResourceMCP:
		default:
			return runtimecontract.ResourceInventory{}, claudeResourceFailure("resource_inventory_invalid", fmt.Errorf("Claude bridge returned unsupported resource kind %q", resource.Kind))
		}
		// Native paths and source-specific configuration are deliberately not
		// part of the Claude resource contract, even if a future bridge emits
		// them accidentally.
		resource.Path = ""
		resource.Source = "claude_agent_sdk_reload"
	}
	encoded, _ := json.Marshal(data.Resources)
	inventory := runtimecontract.ResourceInventory{
		Revision:  "claude-sdk-init:" + fmt.Sprintf("%x", sha256.Sum256(encoded))[:16],
		Semantics: "Claude resources observed from the public SDK reload contracts; native resources are read-only and paths are intentionally withheld",
		Resources: data.Resources,
	}
	if err := inventory.Validate(); err != nil {
		return runtimecontract.ResourceInventory{}, claudeResourceFailure("resource_inventory_invalid", err)
	}
	return inventory, nil
}

func claudeResourceFailure(code string, err error) *runtimecontract.Failure {
	return &runtimecontract.Failure{Code: code, Phase: runtimecontract.FailurePhaseResourceInventory, Message: "Claude Runtime resource inventory is unavailable", Diagnostic: err.Error(), Cause: err}
}

func (c *claudeRuntimeContract) ValidateInput(ctx context.Context, binding runtimecontract.Binding, input []runtimecontract.InputBlock) *runtimecontract.Failure {
	hasImage := false
	for _, block := range input {
		switch block.Kind {
		case runtimecontract.InputText:
		case runtimecontract.InputImage:
			hasImage = true
			if block.MIMEType != "" && !supportedClaudeImageMIME(block.MIMEType) {
				return claudeInputFailure("Claude Runtime supports PNG, JPEG, GIF, and WebP image input")
			}
		default:
			return claudeInputFailure("Claude Runtime does not support this input modality")
		}
	}
	if !hasImage {
		return nil
	}
	state, failure := c.InspectModelControl(ctx, binding)
	if failure != nil {
		return failure
	}
	if !state.Current.ImageInput {
		return claudeInputFailure("the active Claude model does not accept image input")
	}
	return nil
}

func supportedClaudeImageMIME(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func claudeInputFailure(message string) *runtimecontract.Failure {
	return &runtimecontract.Failure{Code: "input_unavailable", Phase: runtimecontract.FailurePhaseTurnStart, Message: message}
}

func claudeModelSelectionIndeterminate(err error) *runtimecontract.Failure {
	return &runtimecontract.Failure{Code: "model_selection_indeterminate", Phase: runtimecontract.FailurePhaseModelControl, Message: "Claude model selection effect is indeterminate", Diagnostic: err.Error(), Cause: err}
}

func cloneClaudeModelState(state runtimecontract.ModelControlState) runtimecontract.ModelControlState {
	state.Current.ThinkingLevels = append([]string(nil), state.Current.ThinkingLevels...)
	state.Models = append([]runtimecontract.Model(nil), state.Models...)
	for index := range state.Models {
		state.Models[index].ThinkingLevels = append([]string(nil), state.Models[index].ThinkingLevels...)
	}
	return state
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
	boundaryRevision := ""
	if c.historyBoundary != nil {
		boundaryRevision = c.historyBoundary.NativeRevision
		unstarted = false
	}
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
	response, outcome := c.command(ctx, "resume_binding", "", runtimecontract.FailurePhaseBindingResume, map[string]any{"sessionRef": binding.NativeRef, "cwd": c.cwd, "expectedRevision": boundaryRevision})
	if outcome.State != runtimecontract.LifecycleAccepted {
		var rejected struct {
			Code           string `json:"code"`
			ActualRevision string `json:"actualRevision"`
		}
		if json.Unmarshal(response.Data, &rejected) == nil && rejected.Code == runtimecontract.FailureCodeNativeConversationDivergence {
			c.mu.Lock()
			c.divergenceRevision = rejected.ActualRevision
			c.mu.Unlock()
			return runtimecontract.Outcome{State: runtimecontract.LifecycleRejected, Failure: &runtimecontract.Failure{
				Code: runtimecontract.FailureCodeNativeConversationDivergence, Phase: runtimecontract.FailurePhaseBindingResume,
				Message: "Native Conversation Divergence: Claude context changed outside Loom; Owner recovery is required",
			}}
		}
	}
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
	if failure := c.ValidateInput(ctx, request.Binding, request.Input); failure != nil {
		return runtimecontract.Outcome{State: runtimecontract.LifecycleRejected, Failure: failure}
	}
	if err := c.persistLedgerFence(); err != nil {
		return claudeFailure(runtimecontract.LifecycleFailed, runtimecontract.FailurePhaseTurnStart, "ledger_unavailable", err)
	}
	evidence := claudeContextEvidence(request.TurnID, request.Input, "developer")
	c.mu.Lock()
	if evidence != nil {
		c.pendingContext[request.TurnID] = evidence
	}
	resume := len(c.turns) > 0 || c.observed
	model, effort := c.model, c.effort
	boundaryRevision := ""
	if c.historyBoundary != nil {
		boundaryRevision = c.historyBoundary.NativeRevision
	}
	configuration := c.runtimeConfigurationPayloadLocked()
	c.mu.Unlock()
	response, outcome := c.command(ctx, "start_turn", request.TurnID, runtimecontract.FailurePhaseTurnStart, map[string]any{"sessionRef": request.Binding.NativeRef, "cwd": c.cwd, "resume": resume, "input": request.Input, "model": model, "thinkingLevel": effort, "configuration": configuration, "boundaryRevision": boundaryRevision})
	if outcome.State != runtimecontract.LifecycleAccepted {
		var rejected struct {
			Code           string `json:"code"`
			ActualRevision string `json:"actualRevision"`
		}
		if json.Unmarshal(response.Data, &rejected) == nil && rejected.Code == runtimecontract.FailureCodeNativeConversationDivergence {
			c.mu.Lock()
			c.fenced[request.TurnID] = true
			delete(c.pendingContext, request.TurnID)
			c.divergenceRevision = rejected.ActualRevision
			c.mu.Unlock()
			return runtimecontract.Outcome{State: runtimecontract.LifecycleRejected, Failure: &runtimecontract.Failure{
				Code: runtimecontract.FailureCodeNativeConversationDivergence, Phase: runtimecontract.FailurePhaseTurnStart,
				Message: "Native Conversation Divergence: Claude context changed outside Loom; Owner recovery is required",
			}}
		}
		c.mu.Lock()
		delete(c.pendingContext, request.TurnID)
		c.mu.Unlock()
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
	if failure := c.ValidateInput(ctx, request.Binding, request.Input); failure != nil {
		return runtimecontract.Outcome{State: runtimecontract.LifecycleRejected, Failure: failure}
	}
	if err := c.persistLedgerFence(); err != nil {
		return claudeFailure(runtimecontract.LifecycleFailed, runtimecontract.FailurePhaseTurnContinue, "ledger_unavailable", err)
	}
	_, outcome := c.command(ctx, "continue_turn", request.TurnID, runtimecontract.FailurePhaseTurnContinue, map[string]any{"sessionRef": request.Binding.NativeRef, "runtimeTurnRef": request.RuntimeTurnRef, "input": request.Input})
	if outcome.State == runtimecontract.LifecycleAccepted {
		if evidence := claudeContextEvidence(request.TurnID, request.Input, "user"); evidence != nil {
			if err := c.persistAcceptedContext(request.TurnID, evidence); err != nil {
				return claudeFailure(runtimecontract.LifecycleIndeterminate, runtimecontract.FailurePhaseTurnContinue, "ledger_commit_indeterminate", err)
			}
		}
		if err := c.ledgerFailure(); err != nil {
			return claudeFailure(runtimecontract.LifecycleIndeterminate, runtimecontract.FailurePhaseTurnContinue, "ledger_commit_indeterminate", err)
		}
	}
	if outcome.State == runtimecontract.LifecycleAccepted {
		outcome.RuntimeTurnRef = request.RuntimeTurnRef
	}
	return outcome
}

func (c *claudeRuntimeContract) persistAcceptedContext(turnID string, evidence *runtimecontract.ContextEvidence) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ledgerErr != nil {
		return c.ledgerErr
	}
	next := cloneClaudeLedgerTurns(c.turns)
	claudeLedgerTurn(&next, turnID).ContextEvidence = evidence
	if c.st == nil {
		return errors.New("Canonical Turn Ledger Store is unavailable")
	}
	if err := c.saveLedger(next); err != nil {
		c.ledgerErr = err
		return err
	}
	c.turns = next
	return nil
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
	if err := c.saveLedger(c.turns); err != nil {
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
	history.Turns = sanitizeClaudeHistory(history.Turns)
	return history, nil
}

func (c *claudeRuntimeContract) CapabilitySnapshot(context.Context, runtimecontract.Binding) runtimecontract.CapabilitySnapshot {
	c.mu.Lock()
	imageInput := c.imageInput
	c.mu.Unlock()
	return claudeControlPlaneCapabilitySnapshot(imageInput)
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
	nativeRevision := ""
	if event.Kind == runtimecontract.EventTerminal {
		var terminal struct {
			NativeRevision string `json:"nativeRevision"`
		}
		if json.Unmarshal(source.Data, &terminal) == nil {
			nativeRevision = terminal.NativeRevision
		}
	}
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
		if c.opTurns[source.Operation] == event.TurnID {
			turn.ContextEvidence = c.pendingContext[event.TurnID]
			delete(c.pendingContext, event.TurnID)
		}
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
		turn.UsageDetails = event.UsageDetails
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
	if err := c.saveLedger(nextTurns); err != nil {
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
	if nativeRevision != "" && c.historyBoundary != nil {
		if c.historyBoundaryCommit == nil {
			boundaryErr := errors.New("History Boundary persistence is unavailable")
			c.ledgerErr = boundaryErr
			c.mu.Unlock()
			go c.fail(boundaryErr)
			return
		}
		if err := c.historyBoundaryCommit(nativeRevision); err != nil {
			c.ledgerErr = err
			c.mu.Unlock()
			go c.fail(&runtimeIndeterminateError{failure: &runtimecontract.Failure{
				Code: "history_boundary_commit_indeterminate", Phase: runtimecontract.FailurePhaseTurnStart,
				Message: "Claude Runtime History Boundary update is indeterminate", Diagnostic: err.Error(), Cause: err,
			}})
			return
		}
		c.historyBoundary.NativeRevision = nativeRevision
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
		if cloned[index].UsageDetails != nil {
			details := *cloned[index].UsageDetails
			details.Models = append([]runtimecontract.ModelUsage(nil), details.Models...)
			cloned[index].UsageDetails = &details
		}
		if cloned[index].ContextEvidence != nil {
			evidence := *cloned[index].ContextEvidence
			evidence.Sources = append([]runtimecontract.ContextEvidenceSource(nil), evidence.Sources...)
			evidence.Deliveries = append([]runtimecontract.ContextEvidenceDelivery(nil), evidence.Deliveries...)
			cloned[index].ContextEvidence = &evidence
		}
	}
	return cloned
}

func claudeContextEvidence(turnID string, input []runtimecontract.InputBlock, developerRole string) *runtimecontract.ContextEvidence {
	report := &runtimecontract.ContextEvidence{State: runtimecontract.ContextEvidenceUnknown, TurnID: turnID, Mode: runtimecontract.ContextDeliveryFullPerTurn, Sources: []runtimecontract.ContextEvidenceSource{}, Deliveries: []runtimecontract.ContextEvidenceDelivery{}, UnsupportedDimensions: []string{"coverage", "epoch", "replay", "resend"}}
	seen := map[string]bool{}
	for _, block := range input {
		if block.Kind != runtimecontract.InputText {
			continue
		}
		content := block.Text
		trimmed := strings.TrimSpace(content)
		channel := ""
		if strings.HasPrefix(trimmed, "<loom_developer_context") {
			channel = "developer"
		} else if strings.HasPrefix(trimmed, "<loom_context") {
			channel = "input"
		} else {
			continue
		}
		if seen[channel] {
			return nil
		}
		seen[channel] = true
		sources, valid := piContextEvidenceSources(trimmed, channel)
		if !valid {
			return nil
		}
		report.Sources = append(report.Sources, sources...)
		role := "user"
		if channel == "developer" {
			role = developerRole
		}
		report.Deliveries = append(report.Deliveries, runtimecontract.ContextEvidenceDelivery{Channel: channel, Role: role, Hash: sha256Hex([]byte(content)), Content: content})
	}
	if !seen["developer"] || !seen["input"] || !contextEvidenceHasSources(report.Sources, "loom_agent_prompt", "loom_agent_profile", "loom_agent_relationships") {
		return nil
	}
	report.State = runtimecontract.ContextEvidenceProven
	return report
}

func (c *claudeRuntimeContract) saveLedger(turns []runtimecontract.HistoryTurn) error {
	if err := validateClaudeLedger(turns); err != nil {
		return err
	}
	return c.st.SaveCanonicalTurnLedger(c.agentID, turns)
}

func validateClaudeLedger(turns []runtimecontract.HistoryTurn) error {
	for _, turn := range turns {
		if turn.ContextEvidence != nil {
			if err := validateClaudeContextEvidence(*turn.ContextEvidence); err != nil {
				return fmt.Errorf("Claude Turn %q context evidence: %w", turn.TurnID, err)
			}
		}
	}
	return nil
}

func sanitizeClaudeHistory(turns []runtimecontract.HistoryTurn) []runtimecontract.HistoryTurn {
	turns = cloneClaudeLedgerTurns(turns)
	for index := range turns {
		evidence := turns[index].ContextEvidence
		if evidence == nil || validateClaudeContextEvidence(*evidence) == nil {
			continue
		}
		turns[index].ContextEvidence = &runtimecontract.ContextEvidence{
			State: runtimecontract.ContextEvidenceUnavailable, TurnID: turns[index].TurnID,
			Mode: runtimecontract.ContextDeliveryFullPerTurn, Reason: "Canonical Turn Ledger contains malformed Claude context evidence",
			Sources: []runtimecontract.ContextEvidenceSource{}, Deliveries: []runtimecontract.ContextEvidenceDelivery{}, UnsupportedDimensions: []string{"coverage", "epoch", "replay", "resend"},
		}
	}
	return turns
}

func validateClaudeContextEvidence(evidence runtimecontract.ContextEvidence) error {
	if err := evidence.Validate(); err != nil {
		return err
	}
	if evidence.State != runtimecontract.ContextEvidenceProven {
		return nil
	}
	if evidence.Mode != runtimecontract.ContextDeliveryFullPerTurn || len(evidence.Deliveries) != 2 {
		return errors.New("proven Claude context requires exact developer and input deliveries")
	}
	expected := map[string]runtimecontract.ContextEvidenceSource{}
	for _, delivery := range evidence.Deliveries {
		if delivery.Channel == "developer" {
			if delivery.Role != "developer" && delivery.Role != "user" {
				return errors.New("Claude developer context has an invalid accepted role")
			}
		} else if delivery.Channel == "input" {
			if delivery.Role != "user" {
				return errors.New("Claude input context has an invalid accepted role")
			}
		} else {
			return fmt.Errorf("unknown Claude context channel %q", delivery.Channel)
		}
		sources, valid := piContextEvidenceSources(strings.TrimSpace(delivery.Content), delivery.Channel)
		if !valid {
			return fmt.Errorf("Claude %s context is malformed", delivery.Channel)
		}
		for _, source := range sources {
			expected[source.Key+"\x00"+source.Channel] = source
		}
	}
	if len(expected) != len(evidence.Sources) {
		return errors.New("Claude context source attribution does not match accepted content")
	}
	for _, source := range evidence.Sources {
		key := source.Key + "\x00" + source.Channel
		if source.State != "delivered" || expected[key] != source {
			return fmt.Errorf("Claude context source %q does not match accepted content", source.Key)
		}
	}
	for _, key := range []string{"loom_agent_prompt\x00developer", "loom_agent_profile\x00developer", "loom_agent_relationships\x00input"} {
		source, ok := expected[key]
		if !ok || source.Revision == "" || source.Hash == "" {
			return errors.New("Claude context is missing required source revisions")
		}
	}
	return nil
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
			RuntimeTurnRef string                        `json:"runtimeTurnRef"`
			Usage          runtimecontract.Usage         `json:"usage"`
			Details        *runtimecontract.UsageDetails `json:"details"`
		}
		if json.Unmarshal(source.Data, &data) != nil {
			return runtimecontract.Event{}, errors.New("Claude bridge emitted malformed usage")
		}
		event.Kind, event.RuntimeTurnRef, event.Usage, event.UsageDetails = runtimecontract.EventUsage, data.RuntimeTurnRef, &data.Usage, data.Details
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
