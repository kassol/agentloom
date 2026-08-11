package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/modelcatalog"
	"github.com/yan5xu/codex-loom/internal/rollout"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

func (h *Hub) ListAgents() []AgentView {
	h.mu.Lock()
	out := make([]AgentView, 0, len(h.agents))
	for _, meta := range h.agents {
		out = append(out, h.viewLocked(meta))
	}
	h.mu.Unlock()
	for i := range out {
		applyRolloutStatus(&out[i])
		if snapshot, ok := h.refreshRuntimeCapabilitySnapshot(out[i].ID, false); ok {
			out[i].CapabilitySnapshot = snapshot
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ListAgentSummaries returns the live fields needed by the Web workspace
// without copying unbounded task envelopes or Provider audit history into
// every reconciliation snapshot. The canonical Agent detail endpoint remains
// the source for the complete record.
func (h *Hub) ListAgentSummaries() []AgentView {
	views := h.ListAgents()
	for i := range views {
		views[i].CurrentTask = boundedDisplayTask(views[i].CurrentTask, 320)
		views[i].ProviderHistory = nil
		if views[i].LastTurn != nil {
			last := *views[i].LastTurn
			last.Task = boundedDisplayTask(last.Task, 320)
			views[i].LastTurn = &last
		}
		if views[i].Goal != nil {
			goal := *views[i].Goal
			goal.Objective = boundedDisplayTask(goal.Objective, 320)
			views[i].Goal = &goal
		}
	}
	return views
}

func boundedDisplayTask(text string, limit int) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	text = displayUserTask(text)
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

// ListSessions is the pre-CodexLoom compatibility method.
func (h *Hub) ListSessions() []SessionView { return h.ListAgents() }

func (h *Hub) GetAgent(key string) (AgentView, error) {
	h.mu.Lock()
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return AgentView{}, errf(404, "agent not found: %s", key)
	}
	view := h.viewLocked(meta)
	h.mu.Unlock()
	applyRolloutStatus(&view)
	if snapshot, ok := h.refreshRuntimeCapabilitySnapshot(view.ID, false); ok {
		view.CapabilitySnapshot = snapshot
	}
	return view, nil
}

func (h *Hub) GetRuntimeDiagnostics(key string) (RuntimeDiagnostics, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta := h.resolveLocked(key)
	if meta == nil {
		return RuntimeDiagnostics{}, errf(404, "agent not found: %s", key)
	}
	bindings := make(map[string]string, len(meta.RuntimeTurnBindings))
	for turnID, nativeTurnID := range meta.RuntimeTurnBindings {
		bindings[turnID] = nativeTurnID
	}
	return RuntimeDiagnostics{
		AgentID: meta.ID, ThreadID: meta.ThreadID, RuntimeKind: meta.RuntimeBinding.Kind,
		NativeRef: meta.RuntimeBinding.NativeRef, TurnBindings: bindings,
	}, nil
}

// GetSession is the pre-CodexLoom compatibility method.
func (h *Hub) GetSession(key string) (SessionView, error) { return h.GetAgent(key) }

func (h *Hub) ActiveAgents() []ActiveAgent {
	views := h.ListAgents()
	out := []ActiveAgent{}
	for _, view := range views {
		if view.Status != "running" {
			continue
		}
		out = append(out, ActiveAgent{ID: view.ID, Name: view.Name, CurrentTask: view.CurrentTask})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// RunningSessions is the pre-CodexLoom compatibility method.
func (h *Hub) RunningSessions() []RunningSession { return h.ActiveAgents() }

type CreateParams struct {
	Name           string `json:"name"`
	Cwd            string `json:"cwd"`
	RuntimeKind    string `json:"runtimeKind"`
	Sandbox        string `json:"sandbox"`
	ApprovalPolicy string `json:"approvalPolicy"`
	ProviderID     string `json:"providerId"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
}

// RestoreAgentParams re-registers a previously archived Agent without
// creating a replacement identity or starting a Turn. Profiles and team
// relationships are stored independently and reconnect through the stable ID.
type RestoreAgentParams struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Cwd            string         `json:"cwd"`
	ThreadID       string         `json:"threadId"`
	RuntimeBinding RuntimeBinding `json:"runtimeBinding"`
	Sandbox        string         `json:"sandbox"`
	ApprovalPolicy string         `json:"approvalPolicy"`
	ProviderID     string         `json:"providerId"`
	Model          string         `json:"model"`
	Effort         string         `json:"effort"`
	CreatedAt      string         `json:"createdAt"`
}

type ConfigParams struct {
	Name           *string `json:"name"`
	Model          *string `json:"model"`
	Effort         *string `json:"effort"`
	Sandbox        *string `json:"sandbox"`
	ApprovalPolicy *string `json:"approvalPolicy"`
	ProviderID     *string `json:"providerId"`
	RuntimeKind    *string `json:"runtimeKind"`
}

func (h *Hub) CreateAgent(p CreateParams) (AgentView, error) {
	if p.Name == "" || p.Cwd == "" {
		return AgentView{}, errf(400, "name and cwd are required")
	}
	p.RuntimeKind = strings.TrimSpace(p.RuntimeKind)
	if p.RuntimeKind == "" {
		return AgentView{}, errf(400, "runtimeKind is required")
	}
	if p.RuntimeKind != "codex" && p.RuntimeKind != "pi" {
		return AgentView{}, errf(400, "unsupported Runtime kind %q", p.RuntimeKind)
	}
	if err := h.validateRequestedRuntimeConfiguration(
		p.RuntimeKind, p.Sandbox,
		strings.TrimSpace(p.ProviderID) != "" || strings.TrimSpace(p.Model) != "" || strings.TrimSpace(p.Effort) != "",
		strings.TrimSpace(p.ApprovalPolicy) != "",
	); err != nil {
		return AgentView{}, err
	}
	if !nameRe.MatchString(p.Name) {
		return AgentView{}, errf(400, "name must match [a-zA-Z0-9_-]+")
	}
	if p.Sandbox == "" {
		p.Sandbox = "danger-full-access"
	}
	if p.ApprovalPolicy == "" {
		p.ApprovalPolicy = "never"
	}
	p.Model = strings.TrimSpace(p.Model)
	p.ProviderID = normalizeProviderID(p.ProviderID)
	if p.ProviderID != "" && !nameRe.MatchString(p.ProviderID) {
		return AgentView{}, errf(400, "providerId must match [a-zA-Z0-9_-]+")
	}
	if p.ProviderID == deepSeekProviderID && p.Model == "" {
		p.Model = deepSeekModel
	}
	if p.ProviderID != "" && p.Model == "" {
		return AgentView{}, errf(400, "model is required for a custom Provider")
	}
	if p.ProviderID == deepSeekProviderID && p.Model != deepSeekModel {
		return AgentView{}, errf(400, "DeepSeek Responses currently supports model %s", deepSeekModel)
	}
	if p.ProviderID != "" {
		provider, err := h.GetModelProvider(p.ProviderID)
		if err != nil {
			return AgentView{}, err
		}
		if !provider.Configured || !provider.CredentialConfigured {
			return AgentView{}, errf(409, "Provider %s is not ready; configure and verify it before creating an Agent", p.ProviderID)
		}
	}
	p.Effort = normalizeEffort(strings.TrimSpace(p.Effort))
	if err := validateModelEffort(p.ProviderID, p.Model, p.Effort); err != nil {
		return AgentView{}, err
	}
	idBytes := make([]byte, 4)
	_, _ = rand.Read(idBytes)
	id := hex.EncodeToString(idBytes)

	h.mu.Lock()
	if h.resolveLocked(p.Name) != nil {
		h.mu.Unlock()
		return AgentView{}, errf(409, "agent %q already exists", p.Name)
	}
	meta := &Agent{
		ID: id, Name: p.Name, Cwd: p.Cwd, ThreadID: newIntegrationID("thr"),
		RuntimeBinding:      RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: p.RuntimeKind},
		RuntimeTurnBindings: map[string]string{},
		Sandbox:             p.Sandbox, ApprovalPolicy: p.ApprovalPolicy, ProviderID: p.ProviderID, Model: p.Model, Effort: p.Effort,
		Status: "idle", CreatedAt: now(), UpdatedAt: now(),
	}
	h.agents[id] = meta
	h.seqs[id] = 0
	rt, err := h.getRuntimeLocked(meta)
	if err != nil {
		delete(h.agents, id)
		delete(h.seqs, id)
		h.mu.Unlock()
		return AgentView{}, err
	}
	h.mu.Unlock()

	if err := waitReady(rt); err != nil {
		h.mu.Lock()
		delete(h.agents, id)
		delete(h.runtimes, id)
		delete(h.seqs, id)
		persistErr := h.persistAgentsLocked()
		h.mu.Unlock()
		closeRuntimeBinding(rt, rt.binding)
		if rt.agentHost != nil {
			rt.agentHost.Close()
		}
		if persistErr != nil {
			return AgentView{}, errf(500, "failed to start Runtime binding: %s; remove failed Agent: %s", err, persistErr)
		}
		return AgentView{}, errf(500, "failed to start Runtime binding: %s", err)
	}

	// initRuntime durably commits the newly created native binding together
	// with the Agent. A second registry write here creates no new state and can
	// only turn a successful commit into a false failure that later resurrects.
	h.mu.Lock()
	h.emitLocked(id, "loom/agent-created", map[string]any{
		"id": id, "name": meta.Name, "cwd": meta.Cwd, "threadId": meta.ThreadID, "runtimeKind": meta.RuntimeBinding.Kind, "providerId": meta.ProviderID,
	})
	h.emitStatusLocked(meta, meta.Status)
	view := h.viewLocked(meta)
	h.mu.Unlock()
	return view, nil
}

func (h *Hub) RestoreAgent(p RestoreAgentParams) (AgentView, error) {
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	p.Cwd = strings.TrimSpace(p.Cwd)
	p.ThreadID = strings.TrimSpace(p.ThreadID)
	p.RuntimeBinding.Kind = strings.TrimSpace(p.RuntimeBinding.Kind)
	p.RuntimeBinding.NativeRef = strings.TrimSpace(p.RuntimeBinding.NativeRef)
	if p.RuntimeBinding.SchemaVersion == 0 || p.RuntimeBinding.SchemaVersion == 1 {
		p.RuntimeBinding.SchemaVersion = RuntimeBindingSchemaVersion
	}
	if p.ID == "" || p.Name == "" || p.Cwd == "" || p.ThreadID == "" || p.RuntimeBinding.Kind == "" || p.RuntimeBinding.NativeRef == "" {
		return AgentView{}, errf(400, "id, name, cwd, threadId, and Runtime Binding are required")
	}
	if p.RuntimeBinding.SchemaVersion != RuntimeBindingSchemaVersion {
		return AgentView{}, errf(400, "unsupported Runtime Binding schema version %d", p.RuntimeBinding.SchemaVersion)
	}
	if p.RuntimeBinding.Kind != "codex" && p.RuntimeBinding.Kind != "pi" {
		return AgentView{}, errf(400, "unsupported Runtime kind %q", p.RuntimeBinding.Kind)
	}
	if err := h.validateRequestedRuntimeConfiguration(
		p.RuntimeBinding.Kind, p.Sandbox,
		strings.TrimSpace(p.ProviderID) != "" || strings.TrimSpace(p.Model) != "" || strings.TrimSpace(p.Effort) != "",
		strings.TrimSpace(p.ApprovalPolicy) != "",
	); err != nil {
		return AgentView{}, err
	}
	if !nameRe.MatchString(p.Name) {
		return AgentView{}, errf(400, "name must match [a-zA-Z0-9_-]+")
	}
	if p.Sandbox == "" {
		p.Sandbox = "danger-full-access"
	}
	if p.ApprovalPolicy == "" {
		p.ApprovalPolicy = "never"
	}
	p.Effort = normalizeEffort(strings.TrimSpace(p.Effort))
	p.ProviderID = normalizeProviderID(p.ProviderID)
	p.Model = strings.TrimSpace(p.Model)
	if p.ProviderID != "" && !nameRe.MatchString(p.ProviderID) {
		return AgentView{}, errf(400, "providerId must match [a-zA-Z0-9_-]+")
	}
	if p.ProviderID == deepSeekProviderID && p.Model == "" {
		p.Model = deepSeekModel
	}
	if p.ProviderID != "" && p.Model == "" {
		return AgentView{}, errf(400, "model is required for a custom Provider")
	}
	if err := validateModelEffort(p.ProviderID, p.Model, p.Effort); err != nil {
		return AgentView{}, err
	}
	if p.CreatedAt == "" {
		p.CreatedAt = now()
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.agents[p.ID] != nil {
		return AgentView{}, errf(409, "agent id %q already exists", p.ID)
	}
	if existing := h.resolveLocked(p.Name); existing != nil {
		return AgentView{}, errf(409, "agent %q already exists", p.Name)
	}
	for _, existing := range h.agents {
		if existing.ThreadID == p.ThreadID || existing.RuntimeBinding.NativeRef == p.RuntimeBinding.NativeRef {
			return AgentView{}, errf(409, "Thread or Runtime Binding is already bound to agent %q", existing.Name)
		}
	}
	meta := &Agent{
		ID: p.ID, Name: p.Name, Cwd: p.Cwd, ThreadID: p.ThreadID, RuntimeBinding: p.RuntimeBinding,
		Sandbox: p.Sandbox, ApprovalPolicy: p.ApprovalPolicy,
		ProviderID: p.ProviderID, Model: p.Model, Effort: p.Effort,
		Status: "idle", CreatedAt: p.CreatedAt, UpdatedAt: now(),
	}
	h.agents[p.ID] = meta
	h.seqs[p.ID] = h.st.LastSeq(p.ID)
	if err := h.persistAgentsLocked(); err != nil {
		delete(h.agents, p.ID)
		delete(h.seqs, p.ID)
		return AgentView{}, errf(500, "save restored agent: %s", err)
	}
	h.emitLocked(p.ID, "loom/agent-restored", map[string]any{
		"id": p.ID, "name": p.Name, "cwd": p.Cwd, "threadId": p.ThreadID, "runtimeKind": p.RuntimeBinding.Kind, "providerId": p.ProviderID,
	})
	h.emitStatusLocked(meta, meta.Status)
	return h.viewLocked(meta), nil
}

// CreateSession is the pre-CodexLoom compatibility method.
func (h *Hub) CreateSession(p CreateParams) (SessionView, error) { return h.CreateAgent(p) }

func (h *Hub) UpdateAgentConfig(key string, p ConfigParams) (AgentView, error) {
	h.mu.Lock()
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return AgentView{}, errf(404, "agent not found: %s", key)
	}
	if meta.Status == "running" {
		h.mu.Unlock()
		return AgentView{}, errf(409, "agent %q is running; config changes apply between Turns", meta.Name)
	}
	if p.RuntimeKind != nil && strings.TrimSpace(*p.RuntimeKind) != meta.RuntimeBinding.Kind {
		h.mu.Unlock()
		return AgentView{}, errf(409, "Agent Runtime kind is immutable")
	}
	var snapshot runtimecontract.CapabilitySnapshot
	if p.Sandbox != nil || p.ProviderID != nil || p.Model != nil || p.Effort != nil || p.ApprovalPolicy != nil {
		capabilityQuery := h.captureRuntimeCapabilityQueryLocked(meta, h.runtimes[meta.ID])
		h.mu.Unlock()
		var capabilityErr error
		snapshot, capabilityErr = h.queryRuntimeCapabilities(capabilityQuery)
		h.mu.Lock()
		meta = h.agents[capabilityQuery.agentID]
		if capabilityErr != nil {
			h.mu.Unlock()
			return AgentView{}, capabilityErr
		}
		if meta == nil || !h.runtimeCapabilityQueryCurrentLocked(capabilityQuery) {
			h.mu.Unlock()
			return AgentView{}, errf(409, "Agent Runtime binding changed while capabilities were checked; retry the config update")
		}
		if meta.Status == "running" {
			h.mu.Unlock()
			return AgentView{}, errf(409, "agent %q started running while capabilities were checked", meta.Name)
		}
	}
	if p.Sandbox != nil {
		if err := requireCapability(snapshot, runtimecontract.CapabilitySandboxConfiguration, "sandbox configuration"); err != nil {
			h.mu.Unlock()
			return AgentView{}, err
		}
	}
	if p.ProviderID != nil || p.Model != nil || p.Effort != nil {
		if err := requireCapability(snapshot, runtimecontract.CapabilityProviderConfiguration, "Provider configuration"); err != nil {
			h.mu.Unlock()
			return AgentView{}, err
		}
	}
	if p.ApprovalPolicy != nil {
		if err := requireCapability(snapshot, runtimecontract.CapabilityApprovalPolicy, "Approval policy configuration"); err != nil {
			h.mu.Unlock()
			return AgentView{}, err
		}
	}

	nextName := meta.Name
	nextModel := meta.Model
	nextEffort := meta.Effort
	nextSandbox := meta.Sandbox
	nextApprovalPolicy := meta.ApprovalPolicy
	nextProviderID := meta.ProviderID

	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			h.mu.Unlock()
			return AgentView{}, errf(400, "name is required")
		}
		if !nameRe.MatchString(name) {
			h.mu.Unlock()
			return AgentView{}, errf(400, "name must match [a-zA-Z0-9_-]+")
		}
		for _, existing := range h.agents {
			if existing.ID == meta.ID {
				continue
			}
			if existing.ID == name || existing.Name == name {
				h.mu.Unlock()
				return AgentView{}, errf(409, "agent %q already exists", name)
			}
		}
		nextName = name
	}
	if p.Model != nil {
		nextModel = strings.TrimSpace(*p.Model)
	}
	if p.ProviderID != nil {
		nextProviderID = normalizeProviderID(*p.ProviderID)
		if nextProviderID != meta.ProviderID && strings.TrimSpace(meta.RuntimeBinding.NativeRef) != "" {
			h.mu.Unlock()
			return AgentView{}, errf(409, "agent %q already has a primary Thread; use the Provider switch operation", meta.Name)
		}
	}
	if nextProviderID != "" && !nameRe.MatchString(nextProviderID) {
		h.mu.Unlock()
		return AgentView{}, errf(400, "providerId must match [a-zA-Z0-9_-]+")
	}
	if nextProviderID == deepSeekProviderID && nextModel == "" {
		nextModel = deepSeekModel
	}
	if nextProviderID != "" && nextModel == "" {
		h.mu.Unlock()
		return AgentView{}, errf(400, "model is required for a custom Provider")
	}
	if nextProviderID == deepSeekProviderID && nextModel != deepSeekModel {
		h.mu.Unlock()
		return AgentView{}, errf(400, "DeepSeek Responses currently supports model %s", deepSeekModel)
	}
	if p.Effort != nil {
		effort := normalizeEffort(strings.TrimSpace(*p.Effort))
		if effort == "" || validEffort(effort) {
			nextEffort = effort
		} else {
			h.mu.Unlock()
			return AgentView{}, errf(400, "effort must be one of: minimal, low, medium, high, xhigh, max, ultra")
		}
	}
	if err := validateModelEffort(nextProviderID, nextModel, nextEffort); err != nil {
		h.mu.Unlock()
		return AgentView{}, err
	}
	if p.Sandbox != nil {
		nextSandbox = strings.TrimSpace(*p.Sandbox)
	}
	if p.ApprovalPolicy != nil {
		nextApprovalPolicy = strings.TrimSpace(*p.ApprovalPolicy)
	}
	previous := *meta
	nameChanged := meta.Name != nextName
	meta.Source = "" // editing config adopts an edge mirror into CodexLoom's registry
	meta.Name = nextName
	meta.ProviderID = nextProviderID
	meta.Model = nextModel
	meta.Effort = nextEffort
	meta.Sandbox = nextSandbox
	meta.ApprovalPolicy = nextApprovalPolicy
	meta.UpdatedAt = now()
	if err := h.persistAgentsLocked(); err != nil {
		*meta = previous
		h.mu.Unlock()
		return AgentView{}, errf(500, "save agent config: %s", err)
	}
	view := h.viewLocked(meta)
	binding := runtimeContractBinding(meta)
	rt := h.runtimes[meta.ID]
	h.mu.Unlock()

	if nameChanged && binding.NativeRef != "" {
		if err := h.syncRuntimeBindingName(rt, binding, nextName); err != nil {
			// The Hub name remains authoritative. Runtime initialization and the
			// later native synchronization may retry this optional convenience.
			log.Printf("[codex-loom] sync native binding name for Agent %s to %q: %v", meta.ID, nextName, err)
		}
	}
	if snapshot, ok := h.refreshRuntimeCapabilitySnapshot(view.ID, true); ok {
		view.CapabilitySnapshot = snapshot
	}
	return view, nil
}

// UpdateConfig is the pre-CodexLoom compatibility method.
func (h *Hub) UpdateConfig(key string, p ConfigParams) (SessionView, error) {
	return h.UpdateAgentConfig(key, p)
}

func (h *Hub) syncRuntimeBindingName(rt *runtime, binding runtimecontract.Binding, name string) error {
	if rt == nil || rt.runtimeContract == nil {
		return nil
	}
	capability, ok := rt.runtimeContract.(runtimecontract.BindingNameCapability)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return compatibilityLifecycleOutcomeError(capability.UpdateBindingName(ctx, binding, name))
}

func runtimeContractBinding(meta *Agent) runtimecontract.Binding {
	if meta == nil {
		return runtimecontract.Binding{}
	}
	return runtimecontract.Binding{
		SchemaVersion: meta.RuntimeBinding.SchemaVersion,
		RuntimeKind:   meta.RuntimeBinding.Kind,
		NativeRef:     meta.RuntimeBinding.NativeRef,
	}
}

// SyncThreadNames backfills authoritative Loom Agent names through already-live
// optional v2 naming capabilities. Missing Runtime handles are left for normal
// binding initialization, which performs the same synchronization after resume.
func (h *Hub) SyncThreadNames() error {
	type namedBinding struct {
		key     string
		name    string
		source  string
		rt      *runtime
		binding runtimecontract.Binding
	}
	h.mu.Lock()
	byBinding := make(map[string]namedBinding, len(h.agents))
	for _, meta := range h.agents {
		if strings.TrimSpace(meta.RuntimeBinding.NativeRef) == "" || strings.TrimSpace(meta.Name) == "" {
			continue
		}
		rt := h.runtimes[meta.ID]
		if rt == nil || rt.runtimeContract == nil {
			log.Printf("[codex-loom] skip native binding name sync for Agent %s: live Runtime contract unavailable", meta.ID)
			continue
		}
		if _, ok := rt.runtimeContract.(runtimecontract.BindingNameCapability); !ok {
			log.Printf("[codex-loom] skip native binding name sync for Agent %s: Runtime naming capability unavailable", meta.ID)
			continue
		}
		binding := rt.binding
		if binding.RuntimeKind == "" {
			binding = runtimeContractBinding(meta)
		}
		key := binding.RuntimeKind + "\x00" + binding.NativeRef
		current, exists := byBinding[key]
		if !exists || (current.source == "edge" && meta.Source != "edge") {
			byBinding[key] = namedBinding{key: key, name: meta.Name, source: meta.Source, rt: rt, binding: binding}
		}
	}
	h.mu.Unlock()
	bindings := make([]namedBinding, 0, len(byBinding))
	for _, binding := range byBinding {
		bindings = append(bindings, binding)
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].key < bindings[j].key })

	var syncErrs []error
	for _, binding := range bindings {
		if err := h.syncRuntimeBindingName(binding.rt, binding.binding, binding.name); err != nil {
			syncErrs = append(syncErrs, fmt.Errorf("%s (%s): %w", binding.name, binding.binding.RuntimeKind, err))
		}
	}
	return errors.Join(syncErrs...)
}

func normalizeEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "extra-high", "extra_high", "extra high":
		return "xhigh"
	default:
		return strings.ToLower(strings.TrimSpace(effort))
	}
}

func validEffort(effort string) bool {
	switch effort {
	case "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	default:
		return false
	}
}

func validateModelEffort(providerID, model, effort string) error {
	if effort == "" {
		return nil
	}
	if !validEffort(effort) {
		return errf(400, "effort must be one of: minimal, low, medium, high, xhigh, max, ultra")
	}
	providerID = normalizePublicProviderID(providerID)
	snapshot, err := modelcatalog.Describe(os.Getenv("CODEX_LOOM_MODEL_CATALOG"))
	if err != nil {
		return errf(500, "read Codex model catalog: %s", err)
	}
	for _, candidate := range snapshot.PublicModels() {
		if candidate.ProviderID != providerID || candidate.ID != model || len(candidate.ReasoningEfforts) == 0 {
			continue
		}
		for _, supported := range candidate.ReasoningEfforts {
			if effort == supported {
				return nil
			}
		}
		return errf(400, "effort for model %s must be one of: %s", model, strings.Join(candidate.ReasoningEfforts, ", "))
	}
	return nil
}

func normalizeProviderID(providerID string) string {
	providerID = strings.TrimSpace(providerID)
	if providerID == "openai" {
		return ""
	}
	return providerID
}

type SendResult struct {
	Dispatched bool   `json:"dispatched"`
	AgentID    string `json:"agentId"`
	SessionID  string `json:"sessionId"`
	TurnID     string `json:"turnId"`
}

func (h *Hub) SendTask(key, text string, inactivity time.Duration) (SendResult, error) {
	return h.sendTask(key, text, inactivity, "", "", "")
}

func (h *Hub) SendTaskWithArtifacts(key, text string, artifactIDs []string, inactivity time.Duration) (SendResult, error) {
	return h.sendTaskWithArtifacts(key, text, artifactIDs, inactivity, "", "", "", "", "")
}

func (h *Hub) sendTask(key, text string, inactivity time.Duration, inboxItemID, attemptID, agentMessageID string) (SendResult, error) {
	return h.sendTaskWithArtifacts(key, text, nil, inactivity, inboxItemID, attemptID, agentMessageID, "", "")
}

func (h *Hub) sendTaskWithTopic(key, text string, inactivity time.Duration, topicID string) (SendResult, error) {
	return h.sendTaskWithArtifacts(key, text, nil, inactivity, "", "", "", topicID, "")
}

func (h *Hub) sendTaskWithTopicDisplay(key, text, displayTask string, inactivity time.Duration, topicID string) (SendResult, error) {
	return h.sendTaskWithArtifacts(key, text, nil, inactivity, "", "", "", topicID, displayTask)
}

func (h *Hub) sendTaskWithArtifacts(key, text string, artifactIDs []string, inactivity time.Duration, inboxItemID, attemptID, agentMessageID, topicID, displayTask string) (SendResult, error) {
	source := h.inferTurnContextSource(inboxItemID, agentMessageID, topicID, "")
	return h.sendTaskWithContext(key, text, artifactIDs, inactivity, inboxItemID, attemptID, agentMessageID, topicID, displayTask, source)
}

func (h *Hub) sendTaskWithContext(key, text string, artifactIDs []string, inactivity time.Duration, inboxItemID, attemptID, agentMessageID, topicID, displayTask string, source turnContextSource) (SendResult, error) {
	return h.sendTaskWithContextReserved(key, text, artifactIDs, inactivity, inboxItemID, attemptID, agentMessageID, topicID, displayTask, source, "")
}

func (h *Hub) sendTaskWithContextReserved(key, text string, artifactIDs []string, inactivity time.Duration, inboxItemID, attemptID, agentMessageID, topicID, displayTask string, source turnContextSource, reservedTurnID string) (SendResult, error) {
	text = strings.TrimSpace(text)
	if text == "" && len(artifactIDs) == 0 {
		return SendResult{}, errf(400, "text or an artifact is required")
	}
	if inactivity <= 0 {
		inactivity = defaultInactivity
	}

	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		return SendResult{}, errf(503, "CodexLoom is shutting down")
	}
	if h.isDrainingLocked() {
		h.mu.Unlock()
		return SendResult{}, errf(409, "CodexLoom is draining for restart")
	}
	if h.providerSwitching {
		h.mu.Unlock()
		return SendResult{}, errf(409, "CodexLoom is switching an Agent Provider")
	}
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return SendResult{}, errf(404, "agent not found: %s", key)
	}
	if topicID != "" {
		topic := h.topics[topicID]
		if topic == nil || !topicHasAgent(topic, meta.ID, meta.ID) {
			h.mu.Unlock()
			return SendResult{}, errf(403, "Agent %s is not part of Topic %s", meta.Name, topicID)
		}
	}
	if rt, ok := h.runtimes[meta.ID]; ok && rt.activeTurn != nil && !rt.activeTurn.finished {
		h.mu.Unlock()
		return SendResult{}, errf(409, "agent %q is already running a task", meta.Name)
	}
	if meta.Status == "running" {
		// Stale state without a live turn (crash leftovers): repair.
		meta.Status = "idle"
		meta.LastError = "repaired stale running state"
	}
	rt, err := h.getRuntimeLocked(meta)
	if err != nil {
		h.mu.Unlock()
		return SendResult{}, err
	}
	agentID := meta.ID
	providerID := meta.ProviderID
	imageCapabilityErr := unsupportedRuntimeCapability(meta, "image input for the active model")
	h.mu.Unlock()
	if providerID == deepSeekProviderID && len(artifactIDs) > 0 {
		return SendResult{}, errf(400, "DeepSeek %s currently supports text input only; remove image and file attachments", deepSeekModel)
	}
	artifacts, err := h.resolveThreadArtifacts(agentID, artifactIDs)
	if err != nil {
		return SendResult{}, err
	}
	hasImageArtifact := false
	for _, artifact := range artifacts {
		if strings.HasPrefix(strings.ToLower(artifact.MimeType), "image/") {
			hasImageArtifact = true
			break
		}
	}
	taskText := codexTaskText(text, artifacts)
	visibleText := text
	if displayTask = strings.TrimSpace(displayTask); displayTask != "" {
		taskText = displayTask
		visibleText = displayTask
	}
	if displayText := strings.TrimSpace(source.DisplayText); displayText != "" {
		visibleText = displayText
	}

	// Serialize readiness, epoch context injection and turn reservation for one
	// runtime. Concurrent callers must not inject the same context revisions.
	rt.startMu.Lock()
	// getRuntimeLocked runs before startMu. Another caller may have timed out a
	// mutating Thread RPC while this caller was waiting for serialization, so
	// re-check the per-host fence at the actual control boundary.
	if err := h.verifyRuntimeThreadControl(agentID, rt); err != nil {
		rt.startMu.Unlock()
		return SendResult{}, err
	}
	if err := waitReady(rt); err != nil {
		rt.startMu.Unlock()
		return SendResult{}, errf(500, "Runtime not ready: %s", err)
	}
	// A shared app-server may unload an idle Thread. Resume immediately before
	// every Turn so Web, CLI and queued deliveries do not depend on a stale
	// in-memory binding left by an earlier request.
	if err := h.resumeAgentThread(agentID, rt); err != nil {
		rt.startMu.Unlock()
		return SendResult{}, err
	}
	backend := runtimeBackend(rt)
	if hasImageArtifact && (backend == nil || !backend.Capabilities().ImageInput) {
		rt.startMu.Unlock()
		return SendResult{}, imageCapabilityErr
	}
	contextPlan, err := h.prepareTurnContext(agentID, source, artifacts)
	if err != nil {
		rt.startMu.Unlock()
		return SendResult{}, err
	}
	if rt.runtimeContract == nil && contextPlan.DeveloperContext != "" {
		if err := h.injectLegacyDeveloperContext(agentID, rt, contextPlan.DeveloperContext); err != nil {
			rt.startMu.Unlock()
			return SendResult{}, err
		}
	}
	input := codexArtifactInput(text, contextPlan.InputContext, artifacts)
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		rt.startMu.Unlock()
		return SendResult{}, errf(503, "CodexLoom is shutting down")
	}
	if h.isDrainingLocked() {
		h.mu.Unlock()
		rt.startMu.Unlock()
		return SendResult{}, errf(409, "CodexLoom is draining for restart")
	}
	if h.providerSwitching {
		h.mu.Unlock()
		rt.startMu.Unlock()
		return SendResult{}, errf(409, "CodexLoom is switching an Agent Provider")
	}
	meta = h.agents[agentID]
	if meta == nil {
		h.mu.Unlock()
		rt.startMu.Unlock()
		return SendResult{}, errf(404, "agent vanished")
	}
	if rt.activeTurn != nil && !rt.activeTurn.finished {
		h.mu.Unlock()
		rt.startMu.Unlock()
		return SendResult{}, errf(409, "agent %q is already running a task", meta.Name)
	}
	reservedCanonicalTurnID := strings.TrimSpace(reservedTurnID)
	if reservedCanonicalTurnID == "" {
		reservedCanonicalTurnID = newIntegrationID("turn")
	}
	turn := &turnState{
		turnID:         reservedCanonicalTurnID,
		task:           taskText,
		source:         turnSource(inboxItemID, agentMessageID),
		inboxItemID:    inboxItemID,
		attemptID:      attemptID,
		agentMessageID: agentMessageID,
		topicID:        topicID,
		contextAttemptID: func() string {
			if contextPlan.Attempt == nil {
				return ""
			}
			return contextPlan.Attempt.ID
		}(),
		contextEpochID: func() string {
			if contextPlan.Attempt == nil {
				return ""
			}
			return contextPlan.Attempt.EpochID
		}(),
		startedAt:    time.Now(),
		lastActivity: time.Now(),
		stopWatchdog: make(chan struct{}),
	}
	previous := *meta
	meta.Source = "" // adopting an edge mirror into CodexLoom's own registry
	meta.Status = "running"
	meta.CurrentTask = taskText
	meta.CurrentTurnID = turn.turnID
	meta.LastError = ""
	meta.UpdatedAt = now()
	if err := h.persistAgentsLocked(); err != nil {
		*meta = previous
		h.mu.Unlock()
		rt.startMu.Unlock()
		return SendResult{}, errf(500, "persist Turn start: %s", err)
	}
	rt.activeTurn = turn
	h.emitLocked(agentID, "loom/user-message", map[string]any{"text": visibleText, "attachments": artifacts, "topicId": topicID})
	h.emitStatusLocked(meta, "running")
	threadID, approvalPolicy, sandbox, model, effort := meta.RuntimeBinding.NativeRef, meta.ApprovalPolicy, meta.Sandbox, meta.Model, meta.Effort
	contract, binding := rt.runtimeContract, rt.binding
	if binding.RuntimeKind == "" {
		binding = runtimeContractBinding(meta)
	}
	h.mu.Unlock()
	defer rt.startMu.Unlock()

	h.startWorker(func() { h.watchdog(agentID, turn, inactivity) })

	startTurn := func() (string, error) {
		if contract != nil {
			controls := RuntimeTurnRequest{
				LoomTurnID: turn.turnID, NativeRef: threadID, Input: input, ApprovalPolicy: approvalPolicy,
				Sandbox: sandbox, Model: model, Effort: effort, Timeout: 30 * time.Second,
			}
			if compatibility, ok := contract.(runtimeCompatibilityControls); ok {
				compatibility.setCompatibilityTurn(controls)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			outcome := contract.StartTurn(ctx, runtimecontract.TurnRequest{
				Binding: binding, TurnID: turn.turnID, Input: runtimeContractTurnInput(contextPlan.DeveloperContext, input),
			})
			return outcome.RuntimeTurnRef, compatibilityLifecycleOutcomeError(outcome)
		}
		if backend == nil {
			return "", errors.New("Agent Runtime is unavailable")
		}
		return backend.StartTurn(RuntimeTurnRequest{
			LoomTurnID: turn.turnID, NativeRef: threadID, Input: input, ApprovalPolicy: approvalPolicy,
			Sandbox: sandbox, Model: model, Effort: effort, Timeout: 30 * time.Second,
		})
	}
	turnID, err := startTurn()
	if err != nil && isThreadNotFoundError(err) {
		// The Thread can be evicted between resume and turn/start. Keep the
		// already-reserved Turn and retry this idempotent pre-start sequence once.
		if resumeErr := h.resumeAgentThread(agentID, rt); resumeErr == nil {
			turnID, err = startTurn()
		} else {
			err = fmt.Errorf("%v; retry %v", err, resumeErr)
		}
	}
	if err != nil {
		if isRuntimeIndeterminate(err) {
			h.onRuntimeIndeterminate(rt, err)
			return SendResult{}, errf(500, "turn/start outcome is indeterminate: %s", err)
		}
		h.mu.Lock()
		if m := h.agents[agentID]; m != nil {
			h.finishTurnLocked(m, rt, "failed", "turn/start failed: "+err.Error())
		}
		h.mu.Unlock()
		return SendResult{}, errf(500, "turn/start failed: %s", err)
	}
	h.mu.Lock()
	if turn.finished {
		// Pi identifies a native Turn by the user Session entry discovered after
		// prompt acceptance. A very fast Turn can settle before get_entries
		// returns, but the response still belongs to this serialized StartTurn.
		if turnID != "" {
			h.recordRuntimeTurnBindingLocked(meta, turn.turnID, turnID)
			h.persistRuntimeProjectionLocked()
		}
		canonicalTurnID := turn.turnID
		h.mu.Unlock()
		return SendResult{Dispatched: true, AgentID: agentID, SessionID: agentID, TurnID: canonicalTurnID}, nil
	}
	if turnID != "" && !turn.finished {
		// A turn/started notification can race ahead of the turn/start response.
		// Once that notification confirmed a native Turn, a stale response must
		// not replace the authoritative binding.
		if turn.nativeTurnID == "" || !turn.startedConfirmed {
			h.bindActiveNativeTurnIDLocked(meta, turn, turnID)
		}
		h.markInboxAttemptRunningLocked(turn)
	}
	if agentMessageID != "" && !turn.finished {
		if err := h.markAgentMessageHandlingRunningLocked(turn, agentID); err != nil {
			log.Printf("[codex-loom] save started message handling %s: %v", agentMessageID, err)
		}
	}
	h.emitLocked(agentID, "loom/turn-started", map[string]any{
		"turnId": turn.turnID, "task": taskText, "source": turn.source, "topicId": topicID,
		"providerId": publicProviderID(meta.ProviderID), "model": meta.Model,
	})
	if topicID != "" {
		h.recordTopicWorkEventLocked(topicID, TopicEvent{Type: "turn_started", Actor: meta.Name, AgentID: agentID, Agent: meta.Name, Summary: summarizeTopicText(taskText), Ref: &TopicRef{Type: "turn", ID: turn.turnID, Label: meta.Name}, CreatedAt: now()})
	}
	canonicalTurnID := turn.turnID
	h.mu.Unlock()

	h.markContextSubmitted(threadID, contextPlan.Attempt, canonicalTurnID)
	return SendResult{Dispatched: true, AgentID: agentID, SessionID: agentID, TurnID: canonicalTurnID}, nil
}

func turnSource(inboxItemID, agentMessageID string) string {
	if inboxItemID != "" {
		return "external"
	}
	if agentMessageID != "" {
		return "internal"
	}
	return "owner"
}

func codexTaskText(text string, artifacts []ThreadArtifact) string {
	taskText := strings.TrimSpace(text)
	if taskText == "" {
		names := make([]string, 0, len(artifacts))
		for _, artifact := range artifacts {
			names = append(names, artifact.Name)
		}
		taskText = "Attached: " + strings.Join(names, ", ")
	}
	return taskText
}

func codexArtifactInput(text, loomContext string, artifacts []ThreadArtifact) []RuntimeInput {
	input := make([]RuntimeInput, 0, len(artifacts)+2)
	original := strings.TrimSpace(text)
	if original == "" && len(artifacts) > 0 {
		original = "Review the attached files."
	}
	if original != "" {
		input = append(input, RuntimeInput{Kind: RuntimeInputText, Text: original})
	}
	if strings.TrimSpace(loomContext) != "" {
		input = append(input, RuntimeInput{Kind: RuntimeInputText, Text: loomContext})
	}
	for _, artifact := range artifacts {
		if strings.HasPrefix(strings.ToLower(artifact.MimeType), "image/") {
			input = append(input, RuntimeInput{Kind: RuntimeInputLocalImage, Path: artifact.Path, MimeType: artifact.MimeType})
		}
	}
	return input
}

func runtimeContractTurnInput(developerContext string, input []RuntimeInput) []runtimecontract.InputBlock {
	blocks := make([]runtimecontract.InputBlock, 0, len(input)+1)
	if developerContext = strings.TrimSpace(developerContext); developerContext != "" {
		blocks = append(blocks, runtimecontract.InputBlock{
			Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleDeveloper, Text: developerContext,
		})
	}
	for _, block := range v1InputToContract(input) {
		block.Role = runtimecontract.InputRoleUser
		blocks = append(blocks, block)
	}
	return blocks
}

func isThreadNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "thread not found") ||
		(strings.Contains(message, "thread") && strings.Contains(message, "not found"))
}

func (h *Hub) watchdog(agentID string, turn *turnState, inactivity time.Duration) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-turn.stopWatchdog:
			return
		case <-ticker.C:
			h.mu.Lock()
			finished := turn.finished
			idle := time.Since(turn.lastActivity)
			total := time.Since(turn.startedAt)
			h.mu.Unlock()
			if finished {
				return
			}
			if idle > inactivity {
				_, _ = h.Interrupt(agentID, fmt.Sprintf("inactivity timeout (%s)", inactivity))
				return
			}
			if total > absoluteTurnCap {
				_, _ = h.Interrupt(agentID, "absolute turn cap (4h)")
				return
			}
		}
	}
}

type InterruptResult struct {
	Interrupted   bool   `json:"interrupted"`
	Message       string `json:"message,omitempty"`
	Reason        string `json:"reason,omitempty"`
	HeldMessageID string `json:"heldMessageId,omitempty"`
	HeldSubject   string `json:"heldSubject,omitempty"`
}

func activeTurnInterruptMismatch(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	const prefix = "expected active turn id "
	const separator = " but found "
	message := strings.TrimSpace(err.Error())
	rest, ok := strings.CutPrefix(message, prefix)
	if !ok {
		return "", false
	}
	_, actualTurnID, ok := strings.Cut(rest, separator)
	actualTurnID = strings.TrimSpace(actualTurnID)
	if !ok || actualTurnID == "" || strings.ContainsAny(actualTurnID, " \t\r\n") {
		return "", false
	}
	return actualTurnID, true
}

func (h *Hub) Interrupt(key, reason string) (InterruptResult, error) {
	if reason == "" {
		reason = "interrupted by caller"
	}
	h.mu.Lock()
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return InterruptResult{}, errf(404, "agent not found: %s", key)
	}
	rt := h.runtimes[meta.ID]
	if rt == nil || rt.activeTurn == nil || rt.activeTurn.finished {
		if meta.Status == "running" {
			previous := *meta
			meta.Status = "idle"
			meta.CurrentTask = ""
			meta.CurrentTurnID = ""
			meta.LastError = reason
			meta.UpdatedAt = now()
			if err := h.persistAgentsLocked(); err != nil {
				*meta = previous
				h.mu.Unlock()
				return InterruptResult{}, errf(500, "persist stale Turn repair: %s", err)
			}
			h.emitStatusLocked(meta, "idle")
		}
		h.mu.Unlock()
		return InterruptResult{Interrupted: false, Message: "no active task"}, nil
	}
	turn := rt.activeTurn
	agentID := meta.ID
	threadID := meta.RuntimeBinding.NativeRef
	turnID := turn.nativeTurnID
	contract := rt.runtimeContract
	binding := rt.binding
	if binding.RuntimeKind == "" {
		binding = runtimeContractBinding(meta)
	}
	backend := runtimeBackend(rt)
	heldMessageID := turn.agentMessageID
	heldSubject := ""
	if message := h.comms[heldMessageID]; message != nil {
		heldSubject = message.Subject
	}
	h.mu.Unlock()

	if contract == nil && turnID == "" {
		return InterruptResult{}, errf(409, "active Turn is still starting; retry shortly")
	}
	interrupt := func(targetTurnID string) error {
		if contract != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return compatibilityLifecycleOutcomeError(contract.InterruptTurn(ctx, runtimecontract.TurnTarget{
				Binding: binding, TurnID: turn.turnID, RuntimeTurnRef: targetTurnID,
			}))
		}
		if backend == nil {
			return errors.New("Agent Runtime is unavailable")
		}
		return backend.Interrupt(threadID, targetTurnID, 10*time.Second)
	}
	err := interrupt(turnID)
	if actualTurnID, mismatch := activeTurnInterruptMismatch(err); contract == nil && mismatch && actualTurnID != turnID {
		h.mu.Lock()
		currentMeta := h.agents[agentID]
		currentRuntime := h.runtimes[agentID]
		var retryTurn *turnState
		if currentMeta != nil && currentRuntime == rt && rt.activeTurn != nil && !rt.activeTurn.finished {
			current := rt.activeTurn
			switch {
			case current.nativeTurnID == actualTurnID:
				retryTurn = current
			case current.nativeTurnID == turnID && !current.startedConfirmed:
				h.bindActiveNativeTurnIDLocked(currentMeta, current, actualTurnID)
				current.startedConfirmed = true
				retryTurn = current
			case current.nativeTurnID == turnID:
				h.finishTurnLocked(currentMeta, rt, "interrupted", "superseded by active Turn "+actualTurnID)
				retryTurn = h.adoptRemoteTurnLocked(currentMeta, rt, actualTurnID)
			}
		}
		h.mu.Unlock()
		if retryTurn != nil {
			turn = retryTurn
			turnID = actualTurnID
			heldMessageID = retryTurn.agentMessageID
			heldSubject = ""
			if heldMessageID != "" {
				h.mu.Lock()
				if message := h.comms[heldMessageID]; message != nil {
					heldSubject = message.Subject
				}
				h.mu.Unlock()
			}
			err = interrupt(actualTurnID)
		}
	}
	if err != nil {
		if isRuntimeIndeterminate(err) {
			h.onRuntimeIndeterminate(rt, err)
			return InterruptResult{}, errf(500, "turn/interrupt outcome is indeterminate: %s", err)
		}
		return InterruptResult{}, errf(500, "turn/interrupt failed: %s", err)
	}
	// codex should follow up with turn/completed(status=interrupted); force
	// the bookkeeping if that doesn't arrive shortly.
	h.startWorker(func() {
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-turn.stopWatchdog:
			return
		case <-timer.C:
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if !turn.finished && rt.activeTurn == turn {
			if m := h.agents[agentID]; m != nil {
				h.finishTurnLocked(m, rt, "interrupted", reason)
			}
		}
	})
	return InterruptResult{Interrupted: true, Reason: reason, HeldMessageID: heldMessageID, HeldSubject: heldSubject}, nil
}

func (h *Hub) ArchiveAgent(key string) (map[string]any, error) {
	var meta *Agent
	var rt *runtime
	h.mu.Lock()
	meta = h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return nil, errf(404, "agent not found: %s", key)
	}
	agentID := meta.ID
	h.mu.Unlock()
	for {
		h.mu.Lock()
		meta = h.agents[agentID]
		if meta == nil {
			h.mu.Unlock()
			return nil, errf(404, "agent not found: %s", key)
		}
		rt = h.runtimes[agentID]
		h.mu.Unlock()
		if rt != nil {
			rt.startMu.Lock()
		}
		h.mu.Lock()
		if h.agents[agentID] == meta && h.runtimes[agentID] == rt {
			break
		}
		h.mu.Unlock()
		if rt != nil {
			rt.startMu.Unlock()
		}
	}
	if rt != nil {
		defer rt.startMu.Unlock()
	}
	for _, topic := range h.topics {
		if topic != nil && topic.Status != TopicStatusArchived && topicHasAgent(topic, agentID, meta.Name) {
			h.mu.Unlock()
			return nil, errf(409, "agent %s is part of Topic %s (%s); archive the Topic or remove the participant first", meta.Name, topic.ID, topic.Title)
		}
	}
	for _, group := range h.collaborationGroups {
		if group == nil || group.Status != CollaborationGroupStatusActive {
			continue
		}
		for _, memberAgentID := range group.MemberAgentIDs {
			if memberAgentID == agentID {
				h.mu.Unlock()
				return nil, errf(409, "agent %s is part of active Collaboration Group %s (%s); archive or update the Group first", meta.Name, group.ID, group.Name)
			}
		}
	}
	binding := runtimeContractBinding(meta)
	if rt != nil && rt.binding.RuntimeKind != "" {
		binding = rt.binding
	}
	name := meta.Name
	killed := *meta
	delete(h.agents, agentID)
	if err := h.persistAgentsLocked(); err != nil {
		h.agents[agentID] = meta
		h.mu.Unlock()
		return nil, errf(500, "save archived agent: %s", err)
	}
	delete(h.runtimes, agentID)
	delete(h.goals, agentID)
	var activeTurnID, activeNativeTurnID string
	if rt != nil && rt.activeTurn != nil && !rt.activeTurn.finished {
		activeTurnID, activeNativeTurnID = rt.activeTurn.turnID, rt.activeTurn.nativeTurnID
	}
	h.emitLocked(agentID, "loom/agent-archived", map[string]any{"id": agentID, "name": name})
	killed.Status = "killed"
	h.emitStatusLocked(&killed, "killed")
	h.mu.Unlock()

	if activeTurnID != "" {
		if err := interruptRuntimeForArchive(rt, binding, activeTurnID, activeNativeTurnID); err != nil {
			h.mu.Lock()
			rt.effectDomainInvalidated = true
			h.mu.Unlock()
			log.Printf("[codex-loom] interrupt active Runtime Turn after Loom archive commit: %v", err)
		}
		h.mu.Lock()
		if rt.activeTurn != nil && rt.activeTurn.turnID == activeTurnID && !rt.activeTurn.finished {
			turn := rt.activeTurn
			turn.finished = true
			if turn.stopWatchdog != nil {
				close(turn.stopWatchdog)
			}
			h.abortTurnApprovalsLocked(agentID, turn.turnID, rt, "the Agent was archived")
			rt.activeTurn = nil
		}
		h.mu.Unlock()
	}

	// Native archival is an optional consequence of the committed Loom state.
	// Its outcome cannot resurrect the governance entity; mandatory close still
	// releases the per-Agent binding resource.
	archiveRuntimeBinding(rt, binding)
	closeRuntimeBinding(rt, binding)
	return map[string]any{"archived": true, "killed": true, "id": agentID, "name": name}, nil
}

func interruptRuntimeForArchive(rt *runtime, binding runtimecontract.Binding, turnID, runtimeTurnRef string) error {
	if rt == nil || rt.runtimeContract == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return compatibilityLifecycleOutcomeError(rt.runtimeContract.InterruptTurn(ctx, runtimecontract.TurnTarget{
		Binding: binding, TurnID: turnID, RuntimeTurnRef: runtimeTurnRef,
	}))
}

func archiveRuntimeBinding(rt *runtime, binding runtimecontract.Binding) {
	if rt == nil || rt.runtimeContract == nil {
		return
	}
	capability, ok := rt.runtimeContract.(runtimecontract.BindingArchiveCapability)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := compatibilityLifecycleOutcomeError(capability.ArchiveBinding(ctx, binding)); err != nil {
		log.Printf("[codex-loom] archive native Runtime binding for Agent: %v", err)
	}
}

func closeRuntimeBinding(rt *runtime, binding runtimecontract.Binding) {
	if rt == nil {
		return
	}
	if rt.runtimeContract != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		outcome := rt.runtimeContract.CloseBinding(ctx, binding)
		err := compatibilityLifecycleOutcomeError(outcome)
		if err == nil && outcome.State == runtimecontract.LifecycleCompleted {
			if rt.agentHost != nil && rt.agentHost.Alive() {
				rt.agentHost.Close()
			}
			return
		}
		if err == nil {
			err = fmt.Errorf("Runtime returned non-completed close state %q", outcome.State)
		}
		log.Printf("[codex-loom] close Runtime binding: %v; forcing Agent Host close", err)
		if rt.agentHost != nil {
			rt.agentHost.Close()
			return
		}
	}
	// Compatibility-only test runtimes are removed after the v2 consumer seam.
	if backend := runtimeBackend(rt); backend != nil {
		backend.Close()
	}
}

// KillSession is the pre-CodexLoom compatibility method.
func (h *Hub) KillSession(key string) (map[string]any, error) { return h.ArchiveAgent(key) }

// ---- history (read from codex rollout files) ----
//
// History is NOT reconstructed from CodexLoom's own event log. The real,
// complete history of any Agent lives in the Codex rollout file that
// `codex app-server` writes for its thread; we read it directly (see the
// rollout package). This means imported/adopted agents show their full
// history immediately, and no "migration/conversion" step exists. Live events
// (from an Agent CodexLoom is actively driving) still flow through the store
// event log for real-time SSE broadcast — but historical viewing always reads
// the rollout, so a non-driven Agent is fully viewable too.

type HistoryTurn struct {
	ID             string              `json:"id"`
	Status         string              `json:"status"`
	Items          []map[string]any    `json:"items"`
	Model          string              `json:"model,omitempty"`
	Usage          *rollout.TokenUsage `json:"usage,omitempty"`
	UsageUpdatedAt string              `json:"usageUpdatedAt,omitempty"`
}

type History struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Cwd      string        `json:"cwd"`
	ThreadID string        `json:"threadId"`
	Status   string        `json:"status"`
	Total    int           `json:"total"` // total turns in the rollout (for scroll-up paging)
	Turns    []HistoryTurn `json:"turns"`
}

// CanonicalHistory is the Runtime Contract v2 history projection returned by
// ordinary Agent APIs. Native Runtime references and diagnostic payloads are
// intentionally absent.
type CanonicalHistory struct {
	ID       string                 `json:"id"`
	Name     string                 `json:"name"`
	Cwd      string                 `json:"cwd"`
	ThreadID string                 `json:"threadId"`
	Status   string                 `json:"status"`
	Total    int                    `json:"total"`
	Turns    []CanonicalHistoryTurn `json:"turns"`
}

type CanonicalHistoryTurn struct {
	runtimecontract.HistoryTurn
	Source *TurnReference `json:"source,omitempty"`
	Error  string         `json:"error,omitempty"`
}

type CanonicalTurnDetail struct {
	runtimecontract.HistoryTurn
	AgentID  string         `json:"agentId"`
	Agent    string         `json:"agent"`
	ThreadID string         `json:"threadId"`
	Cwd      string         `json:"cwd"`
	Source   *TurnReference `json:"source,omitempty"`
	Error    string         `json:"error,omitempty"`
}

type TurnReference struct {
	Kind    string `json:"kind"`
	ID      string `json:"id,omitempty"`
	TopicID string `json:"topicId,omitempty"`
}

type TurnDetail struct {
	ID             string              `json:"id"`
	AgentID        string              `json:"agentId"`
	Agent          string              `json:"agent"`
	ThreadID       string              `json:"threadId"`
	Cwd            string              `json:"cwd"`
	Status         string              `json:"status"`
	StartedAt      string              `json:"startedAt,omitempty"`
	CompletedAt    string              `json:"completedAt,omitempty"`
	Model          string              `json:"model,omitempty"`
	Usage          *rollout.TokenUsage `json:"usage,omitempty"`
	UsageUpdatedAt string              `json:"usageUpdatedAt,omitempty"`
	Source         *TurnReference      `json:"source,omitempty"`
	Error          string              `json:"error,omitempty"`
	Items          []map[string]any    `json:"items"`
}

// GetTurn locates a Loom Turn while the Runtime remains the history source.
func (h *Hub) GetTurn(turnID string) (TurnDetail, error) {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return TurnDetail{}, errf(400, "turn id is required")
	}

	h.mu.Lock()
	agents := make([]AgentView, 0, len(h.agents))
	for _, agent := range h.agents {
		if agent.ThreadID != "" {
			agents = append(agents, h.viewLocked(agent))
		}
	}
	h.mu.Unlock()
	for i := range agents {
		applyRolloutStatus(&agents[i])
	}
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].Name != agents[j].Name {
			return agents[i].Name < agents[j].Name
		}
		return agents[i].ID < agents[j].ID
	})

	var lookupErr error
	for _, agent := range agents {
		backend := runtimeForKind(agent.RuntimeBinding.Kind)
		if backend == nil || !backend.Capabilities().History {
			continue
		}
		nativeTurnID := nativeTurnIDFor(&agent, turnID)
		turn, err := backend.ReadTurn(agent.nativeRuntimeRef, nativeTurnID)
		if err != nil {
			if !errors.Is(err, rollout.ErrTurnNotFound) && !errors.Is(err, rollout.ErrRolloutNotFound) && lookupErr == nil {
				lookupErr = fmt.Errorf("Agent %s (%s): %w", agent.Name, agent.ThreadID, err)
			}
			continue
		}
		applyInterruptedHistoryTurn(&agent, turnID, &turn)
		items := turn.Items
		if items == nil {
			items = []map[string]any{}
		}
		detail := TurnDetail{
			ID: turnID, AgentID: agent.ID, Agent: agent.Name, ThreadID: agent.ThreadID,
			Cwd: agent.Cwd, Status: turn.Status, StartedAt: turn.StartedAt, CompletedAt: turn.CompletedAt,
			Model: turn.Model, Usage: rolloutTokenUsage(turn.Usage), UsageUpdatedAt: turn.UsageUpdatedAt, Items: items,
		}
		if detail.Status == "running" && (agent.Status != "running" || agent.CurrentTurnID != turnID) {
			detail.Status = "interrupted"
		}
		h.mu.Lock()
		detail.Source, detail.Error = h.turnReferenceLocked(agent.ID, turnID)
		if detail.Error == "" && agent.LastTurn != nil && agent.LastTurn.TurnID == turnID && agent.LastError != "" {
			detail.Error = agent.LastError
		}
		h.mu.Unlock()
		return detail, nil
	}
	if lookupErr != nil {
		return TurnDetail{}, errf(500, "turn lookup failed: %v", lookupErr)
	}
	return TurnDetail{}, errf(404, "turn not found: %s", turnID)
}

func (h *Hub) turnReferenceLocked(agentID, turnID string) (*TurnReference, string) {
	if meta := h.agents[agentID]; meta != nil {
		for predecessorTurnID, marker := range meta.TurnRecoveryMarkers {
			if marker.RecoveryTurnID == turnID {
				return &TurnReference{Kind: "recovery", ID: predecessorTurnID, TopicID: marker.TopicID}, ""
			}
		}
	}
	for _, attempt := range h.attempts {
		if attempt == nil || attempt.AgentID != agentID || attempt.TurnID != turnID {
			continue
		}
		return &TurnReference{Kind: "external", ID: attempt.InboxItemID}, attempt.Error
	}
	for _, request := range h.humanRequests {
		if request != nil && request.AgentID == agentID && request.ResumedTurnID == turnID {
			return &TurnReference{Kind: "needs_you", ID: request.ID, TopicID: request.TopicID}, request.LastError
		}
	}
	for _, messageID := range h.commOrder {
		message := h.comms[messageID]
		if message == nil || message.ToAgentID != agentID {
			continue
		}
		matched := message.DeliveryMode == "turn_start" && message.DeliveredTurnID == turnID
		for _, attempt := range message.HandlingAttempts {
			if attempt.TurnID == turnID {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		kind := "internal"
		switch {
		case message.TriggerID != "":
			kind = "trigger"
		case message.ScheduleID != "":
			kind = "schedule"
		}
		return &TurnReference{Kind: kind, ID: message.ID, TopicID: message.TopicID}, message.LastHandlingError
	}
	return nil, ""
}

func (h *Hub) History(key string, count, offset int) (History, error) {
	if count <= 0 {
		count = 10
	}
	if offset < 0 {
		offset = 0
	}
	h.mu.Lock()
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return History{}, errf(404, "agent not found: %s", key)
	}
	if !h.runtimeCapabilitiesLocked(meta).History {
		h.mu.Unlock()
		return History{}, unsupportedRuntimeCapability(meta, "history")
	}
	view := h.viewLocked(meta)
	backend := runtimeForKind(meta.RuntimeBinding.Kind)
	if rt := h.runtimes[meta.ID]; rt != nil {
		backend = runtimeBackend(rt)
	}
	h.mu.Unlock()
	applyRolloutStatus(&view)

	threadID := view.nativeRuntimeRef
	hist := History{ID: view.ID, Name: view.Name, Cwd: view.Cwd, ThreadID: view.ThreadID, Status: view.Status}

	if threadID == "" {
		return hist, nil // no thread started yet → no rollout, no history
	}

	if backend == nil || !backend.Capabilities().History {
		return History{}, errf(503, "%s Runtime history backend is unavailable", view.RuntimeBinding.Kind)
	}
	window, err := backend.ReadHistory(threadID, count, offset)
	if err != nil {
		if errors.Is(err, rollout.ErrRolloutNotFound) {
			return hist, nil
		}
		return History{}, errf(500, "read %s Runtime history: %v", view.RuntimeBinding.Kind, err)
	}
	all := window.Turns
	for i := range all {
		all[i].ID = loomTurnIDFor(&view, all[i].ID)
		applyInterruptedHistoryTurn(&view, all[i].ID, &all[i])
	}
	hist.Total = window.Total
	if len(all) > 0 && all[len(all)-1].Status == "running" && hist.Status != "running" {
		if latest, err := backend.LatestTurn(threadID); err == nil && latest != nil && latest.Status == "running" && externalTurnLooksLive(threadID, latest.UpdatedAt) {
			hist.Status = "running"
		} else {
			all[len(all)-1].Status = "interrupted"
		}
	}
	for _, t := range all {
		items := t.Items
		if items == nil {
			items = []map[string]any{}
		}
		turn := HistoryTurn{ID: t.ID, Status: t.Status, Items: items}
		if t.Usage != nil {
			turn.Model = t.Model
			turn.Usage = rolloutTokenUsage(t.Usage)
			turn.UsageUpdatedAt = t.UsageUpdatedAt
		}
		hist.Turns = append(hist.Turns, turn)
	}
	return hist, nil
}

// CanonicalHistory reads through Runtime Contract v2 and applies the public
// Loom identity projection. The v1 History method remains for /api/sessions.
func (h *Hub) CanonicalHistory(key string, count, offset int) (CanonicalHistory, error) {
	if count <= 0 {
		count = 10
	}
	if offset < 0 {
		offset = 0
	}
	h.mu.Lock()
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return CanonicalHistory{}, errf(404, "agent not found: %s", key)
	}
	view := h.viewLocked(meta)
	contract := runtimecontract.Contract(nil)
	var historyProvider runtimeHistoryContractProvider
	if rt := h.runtimes[meta.ID]; rt != nil {
		contract = rt.runtimeContract
	}
	if contract == nil {
		if driver, err := h.runtimeHostDriverLocked(meta.RuntimeBinding.Kind); err == nil {
			historyProvider, _ = driver.(runtimeHistoryContractProvider)
		}
	}
	h.mu.Unlock()
	applyRolloutStatus(&view)
	result := CanonicalHistory{ID: view.ID, Name: view.Name, Cwd: view.Cwd, ThreadID: view.ThreadID, Status: view.Status, Turns: []CanonicalHistoryTurn{}}
	if view.nativeRuntimeRef == "" {
		return result, nil
	}
	if contract == nil && historyProvider != nil {
		contract = historyProvider.HistoryContract(AgentHostRequest{AgentID: view.ID})
		if seeder, ok := contract.(runtimeTurnCorrelationSeeder); ok {
			seeder.seedTurnBindings(view.nativeTurnBindings)
		}
	}
	if contract == nil {
		return CanonicalHistory{}, errf(503, "%s Runtime history backend is unavailable", view.RuntimeBinding.Kind)
	}
	history, failure := contract.ReadHistory(context.Background(), runtimecontract.HistoryRequest{
		Binding: runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: view.RuntimeBinding.Kind, NativeRef: view.nativeRuntimeRef},
		Count:   count, Offset: offset,
	})
	if failure != nil {
		if failure.Code == "history_not_found" {
			return result, nil
		}
		return CanonicalHistory{}, errf(500, "read %s Runtime history: %s", view.RuntimeBinding.Kind, publicRuntimeFailureMessage(&view.Agent, "", failure.Message))
	}
	result.Total = history.Total
	for _, turn := range history.Turns {
		projected := projectCanonicalHistoryTurn(&view, turn)
		h.mu.Lock()
		source, turnError := h.turnReferenceLocked(view.ID, projected.TurnID)
		h.mu.Unlock()
		if strings.TrimSpace(turnError) == "" && view.LastTurn != nil && view.LastTurn.TurnID == projected.TurnID {
			turnError = view.LastError
		}
		result.Turns = append(result.Turns, CanonicalHistoryTurn{
			HistoryTurn: projected,
			Source:      source,
			Error:       publicRuntimeFailureMessage(&view.Agent, projected.TurnID, turnError),
		})
	}
	return result, nil
}

func projectCanonicalHistoryTurn(view *AgentView, turn runtimecontract.HistoryTurn) runtimecontract.HistoryTurn {
	turnID := strings.TrimSpace(turn.TurnID)
	if turnID == "" && view != nil {
		for loomTurnID, nativeTurnID := range view.nativeTurnBindings {
			if nativeTurnID == turn.RuntimeTurnRef {
				turnID = loomTurnID
				break
			}
		}
	}
	if turnID == "" {
		seed := turn.RuntimeTurnRef
		if view != nil {
			seed = view.ThreadID + "\x00" + seed
		}
		digest := sha256Hex([]byte(seed))
		turnID = "turn_" + digest[:16]
	}
	turn.TurnID = turnID
	turn.RuntimeTurnRef = ""
	turn.Diagnostic = nil
	if view != nil {
		if view.LastTurn != nil && view.LastTurn.TurnID == turnID {
			switch view.LastTurn.Status {
			case "completed":
				turn.State = runtimecontract.LifecycleCompleted
			case "failed":
				turn.State = runtimecontract.LifecycleFailed
			case "interrupted":
				turn.State = runtimecontract.LifecycleInterrupted
			}
			if view.LastTurn.CompletedAt != "" {
				turn.CompletedAt = view.LastTurn.CompletedAt
			}
		}
		if marker, ok := view.turnRecoveryMarkers[turnID]; ok {
			turn.State = runtimecontract.LifecycleInterrupted
			if turn.CompletedAt == "" {
				turn.CompletedAt = marker.UpdatedAt
				if turn.CompletedAt == "" {
					turn.CompletedAt = marker.CreatedAt
				}
			}
		}
	}
	projected := make([]runtimecontract.ContentBlock, 0, len(turn.Content))
	for index := range turn.Content {
		if content, ok := projectRuntimeContentBlock(view, turnID, turn.Content[index]); ok {
			projected = append(projected, content)
		}
	}
	turn.Content = projected
	return turn
}

func (h *Hub) GetCanonicalTurn(turnID string) (CanonicalTurnDetail, error) {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return CanonicalTurnDetail{}, errf(400, "turn id is required")
	}
	var firstErr error
	for _, agent := range h.ListAgents() {
		history, err := h.CanonicalHistory(agent.ID, 10_000, 0)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, turn := range history.Turns {
			if turn.TurnID != turnID {
				continue
			}
			detail := CanonicalTurnDetail{HistoryTurn: turn.HistoryTurn, AgentID: agent.ID, Agent: agent.Name, ThreadID: agent.ThreadID, Cwd: agent.Cwd, Source: turn.Source, Error: turn.Error}
			if strings.TrimSpace(detail.Error) == "" && turn.State == runtimecontract.LifecycleFailed {
				detail.Error = agent.LastError
			}
			detail.Error = publicRuntimeFailureMessage(&agent.Agent, turnID, detail.Error)
			return detail, nil
		}
	}
	if firstErr != nil {
		return CanonicalTurnDetail{}, firstErr
	}
	return CanonicalTurnDetail{}, errf(404, "turn not found: %s", turnID)
}

func applyInterruptedHistoryTurn(view *AgentView, turnID string, turn *RuntimeHistoryTurn) {
	if view == nil || turn == nil {
		return
	}
	marker, ok := view.turnRecoveryMarkers[turnID]
	if !ok {
		return
	}
	turn.Status = "interrupted"
	if turn.CompletedAt == "" {
		turn.CompletedAt = marker.CreatedAt
	}
	for _, item := range turn.Items {
		if item["type"] == "command" && item["status"] == "running" {
			item["status"] = "interrupted"
		}
	}
}

func nativeTurnIDFor(view *AgentView, turnID string) string {
	if view != nil {
		if nativeTurnID := view.nativeTurnBindings[turnID]; nativeTurnID != "" {
			return nativeTurnID
		}
	}
	return turnID
}

func loomTurnIDFor(view *AgentView, nativeTurnID string) string {
	if view != nil {
		for turnID, candidate := range view.nativeTurnBindings {
			if candidate == nativeTurnID {
				return turnID
			}
		}
	}
	return nativeTurnID
}

func rolloutTokenUsage(usage *RuntimeTokenUsage) *rollout.TokenUsage {
	if usage == nil {
		return nil
	}
	return &rollout.TokenUsage{
		InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens,
		OutputTokens: usage.OutputTokens, ReasoningOutputTokens: usage.ReasoningOutputTokens,
		TotalTokens: usage.TotalTokens, Calls: usage.Calls,
	}
}

// Shutdown closes all codex processes. Running agents keep status=running
// on disk so the next startup marks them interrupted.
