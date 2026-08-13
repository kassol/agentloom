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

func (h *Hub) GetAgent(key string) (AgentView, error) {
	h.mu.Lock()
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return AgentView{}, errf(404, "agent not found: %s", key)
	}
	view := h.viewLocked(meta)
	h.mu.Unlock()
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

type CreateParams struct {
	Name                 string               `json:"name"`
	Cwd                  string               `json:"cwd"`
	RuntimeKind          string               `json:"runtimeKind"`
	Sandbox              string               `json:"sandbox"`
	ApprovalPolicy       string               `json:"approvalPolicy"`
	ProviderID           string               `json:"providerId"`
	Model                string               `json:"model"`
	Effort               string               `json:"effort"`
	RuntimeConfiguration RuntimeConfiguration `json:"runtimeConfiguration"`
}

// RestoreAgentParams re-registers a previously archived Agent without
// creating a replacement identity or starting a Turn. Profiles and team
// relationships are stored independently and reconnect through the stable ID.
type RestoreAgentParams struct {
	ID                   string               `json:"id"`
	Name                 string               `json:"name"`
	Cwd                  string               `json:"cwd"`
	ThreadID             string               `json:"threadId"`
	RuntimeBinding       RuntimeBinding       `json:"runtimeBinding"`
	Sandbox              string               `json:"sandbox"`
	ApprovalPolicy       string               `json:"approvalPolicy"`
	ProviderID           string               `json:"providerId"`
	Model                string               `json:"model"`
	Effort               string               `json:"effort"`
	RuntimeConfiguration RuntimeConfiguration `json:"runtimeConfiguration"`
	CreatedAt            string               `json:"createdAt"`
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
	p.Name = strings.TrimSpace(p.Name)
	p.Cwd = strings.TrimSpace(p.Cwd)
	if p.Name == "" || p.Cwd == "" {
		return AgentView{}, errf(400, "name and cwd are required")
	}
	p.RuntimeKind = strings.TrimSpace(p.RuntimeKind)
	if p.RuntimeKind == "" {
		return AgentView{}, errf(400, "runtimeKind is required")
	}
	if err := h.validateRequestedRuntimeConfiguration(
		p.RuntimeKind, p.Sandbox,
		strings.TrimSpace(p.ProviderID) != "" || strings.TrimSpace(p.Model) != "" || strings.TrimSpace(p.Effort) != "",
		strings.TrimSpace(p.ApprovalPolicy) != "",
	); err != nil {
		return AgentView{}, err
	}
	if !validAgentName(p.Name) {
		return AgentView{}, errf(400, "name may contain Unicode letters, marks, numbers, hyphens, and underscores")
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
	defaultClaudeModelConfiguration(p.RuntimeKind, &p.ProviderID, &p.Model, &p.Effort)
	configuration, err := normalizeRuntimeConfiguration(p.RuntimeKind, p.RuntimeConfiguration)
	if err != nil {
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
		Sandbox:             p.Sandbox, ApprovalPolicy: p.ApprovalPolicy, ProviderID: p.ProviderID, Model: p.Model, Effort: p.Effort, RuntimeConfiguration: configuration,
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
	if err := h.validateRequestedRuntimeConfiguration(
		p.RuntimeBinding.Kind, p.Sandbox,
		strings.TrimSpace(p.ProviderID) != "" || strings.TrimSpace(p.Model) != "" || strings.TrimSpace(p.Effort) != "",
		strings.TrimSpace(p.ApprovalPolicy) != "",
	); err != nil {
		return AgentView{}, err
	}
	if !validAgentName(p.Name) {
		return AgentView{}, errf(400, "name may contain Unicode letters, marks, numbers, hyphens, and underscores")
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
	defaultClaudeModelConfiguration(p.RuntimeBinding.Kind, &p.ProviderID, &p.Model, &p.Effort)
	configuration, err := normalizeRuntimeConfiguration(p.RuntimeBinding.Kind, p.RuntimeConfiguration)
	if err != nil {
		return AgentView{}, err
	}
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
		ProviderID: p.ProviderID, Model: p.Model, Effort: p.Effort, RuntimeConfiguration: configuration,
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
	view := h.viewLocked(meta)
	if h.activeGoalReservesThreadLocked(p.ID) {
		h.startWorkerLocked(func() { h.resumeGoalAfterOpen(p.ID) })
	}
	return view, nil
}

func (h *Hub) UpdateAgentConfig(key string, p ConfigParams) (AgentView, error) {
	h.mu.Lock()
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return AgentView{}, errf(404, "agent not found: %s", key)
	}
	if err := h.runtimeMutationAllowedLocked(meta.ID); err != nil {
		h.mu.Unlock()
		return AgentView{}, err
	}
	if meta.Status == "running" {
		h.mu.Unlock()
		return AgentView{}, errf(409, "agent %q is running; config changes apply between Turns", meta.Name)
	}
	if p.RuntimeKind != nil && strings.TrimSpace(*p.RuntimeKind) != meta.RuntimeBinding.Kind {
		h.mu.Unlock()
		return AgentView{}, errf(409, "Agent Runtime kind is immutable")
	}
	if p.ProviderID != nil || p.Model != nil || p.Effort != nil {
		h.mu.Unlock()
		return AgentView{}, errf(409, "Runtime model fields are changed through the typed Runtime model operation")
	}
	var snapshot runtimecontract.CapabilitySnapshot
	if p.Sandbox != nil || p.ApprovalPolicy != nil {
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
		if err := h.runtimeMutationAllowedLocked(meta.ID); err != nil {
			h.mu.Unlock()
			return AgentView{}, err
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
	if p.ApprovalPolicy != nil {
		if err := requireCapability(snapshot, runtimecontract.CapabilityApprovalPolicy, "Approval policy configuration"); err != nil {
			h.mu.Unlock()
			return AgentView{}, err
		}
	}

	nextName := meta.Name
	nextSandbox := meta.Sandbox
	nextApprovalPolicy := meta.ApprovalPolicy

	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			h.mu.Unlock()
			return AgentView{}, errf(400, "name is required")
		}
		if !validAgentName(name) {
			h.mu.Unlock()
			return AgentView{}, errf(400, "name may contain Unicode letters, marks, numbers, hyphens, and underscores")
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
	if p.Sandbox != nil {
		nextSandbox = strings.TrimSpace(*p.Sandbox)
	}
	if p.ApprovalPolicy != nil {
		nextApprovalPolicy = strings.TrimSpace(*p.ApprovalPolicy)
	}
	previous := *meta
	if err := h.runtimeMutationAllowedLocked(meta.ID); err != nil {
		h.mu.Unlock()
		return AgentView{}, err
	}
	nameChanged := meta.Name != nextName
	meta.Source = "" // editing config adopts an edge mirror into CodexLoom's registry
	meta.Name = nextName
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
		if rt != nil {
			rt.startMu.Lock()
		}
		syncErr := h.syncRuntimeBindingName(rt, binding, nextName)
		if rt != nil {
			rt.startMu.Unlock()
		}
		if syncErr != nil {
			// The Hub name remains authoritative. Runtime initialization and the
			// later native synchronization may retry this optional convenience.
			log.Printf("[codex-loom] sync native binding name for Agent %s to %q: %v", meta.ID, nextName, syncErr)
		}
	}
	if snapshot, ok := h.refreshRuntimeCapabilitySnapshot(view.ID, true); ok {
		view.CapabilitySnapshot = snapshot
	}
	return view, nil
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
	return runtimeLifecycleOutcomeError(capability.UpdateBindingName(ctx, binding, name), runtimecontract.LifecycleCompleted, false)
}

func runtimeContractBinding(meta *Agent) runtimecontract.Binding {
	if meta == nil {
		return runtimecontract.Binding{}
	}
	schemaVersion := meta.RuntimeBinding.SchemaVersion
	// Agents loaded through Store migration already carry the canonical schema.
	// Keep the in-memory test/adoption seam equally narrow: schema 0/1 was the
	// post-fork Binding shape and maps directly to v2 when kind/ref are present.
	if schemaVersion == 0 || schemaVersion == 1 {
		schemaVersion = runtimecontract.BindingSchemaVersion
	}
	return runtimecontract.Binding{
		SchemaVersion: schemaVersion,
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
	case runtimecontract.ThinkingLevelDefault, "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	default:
		return false
	}
}

func defaultClaudeModelConfiguration(kind string, provider, model, effort *string) bool {
	if kind != "claude" || strings.TrimSpace(*model) != "" {
		return false
	}
	*provider, *model, *effort = "anthropic", "default", runtimecontract.ThinkingLevelDefault
	return true
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
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return SendResult{}, errf(404, "agent not found: %s", key)
	}
	if err := h.runtimeMutationAllowedLocked(meta.ID); err != nil {
		h.mu.Unlock()
		return SendResult{}, err
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
	imageCapabilityErr := unsupportedRuntimeCapability(meta, "image input for the active model")
	h.mu.Unlock()
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
	if preparer, ok := rt.runtimeContract.(runtimeTurnPersistencePreparer); ok {
		if err := preparer.prepareTurnPersistence(); err != nil {
			rt.startMu.Unlock()
			return SendResult{}, errf(500, "prepare Runtime Turn persistence: %s", err)
		}
	}
	// A shared app-server may unload an idle Thread. Resume immediately before
	// every Turn so Web, CLI and queued deliveries do not depend on a stale
	// in-memory binding left by an earlier request.
	if err := h.resumeAgentThread(agentID, rt); err != nil {
		rt.startMu.Unlock()
		return SendResult{}, err
	}
	if hasImageArtifact {
		capability, ok := rt.runtimeContract.(runtimecontract.InputCapability)
		if !ok {
			rt.startMu.Unlock()
			return SendResult{}, imageCapabilityErr
		}
		if failure := capability.ValidateInput(context.Background(), rt.binding, []runtimecontract.InputBlock{{Kind: runtimecontract.InputImage}}); failure != nil {
			rt.startMu.Unlock()
			return SendResult{}, imageCapabilityErr
		}
	}
	contextPlan, err := h.prepareTurnContext(agentID, source, artifacts)
	if err != nil {
		rt.startMu.Unlock()
		return SendResult{}, err
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
	meta = h.agents[agentID]
	if meta == nil {
		h.mu.Unlock()
		rt.startMu.Unlock()
		return SendResult{}, errf(404, "agent vanished")
	}
	if err := h.runtimeMutationAllowedLocked(agentID); err != nil {
		h.mu.Unlock()
		rt.startMu.Unlock()
		return SendResult{}, err
	}
	goalID, goalVersion := "", int64(0)
	if goal := h.goals[agentID]; goal != nil {
		goalID, goalVersion = goal.ID, goal.Version
	}
	if goalID != contextPlan.GoalID || goalVersion != contextPlan.GoalVersion {
		h.mu.Unlock()
		rt.startMu.Unlock()
		return SendResult{}, errf(409, "Goal changed while Turn context was prepared; retry")
	}
	if source.GoalActive {
		goal := h.goals[agentID]
		if goal == nil || goal.ID != source.GoalID || goal.Version != source.GoalVersion || !h.goalContinuationReadyLocked(agentID) {
			h.mu.Unlock()
			rt.startMu.Unlock()
			return SendResult{}, errf(409, "Goal changed before its continuation Turn was reserved")
		}
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
		humanRequestID: recoveryHumanRequestID(source),
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
	meta.WorkDisposition = nil
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

	startTurn := func() (string, runtimecontract.Outcome, error) {
		if contract == nil {
			return "", runtimecontract.Outcome{}, errors.New("Agent Runtime Contract is unavailable")
		}
		configureRuntimeTurn(contract, approvalPolicy, sandbox, model, effort, h.effectiveDeveloperContextTimeout())
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		outcome := contract.StartTurn(ctx, runtimecontract.TurnRequest{
			Binding: binding, TurnID: turn.turnID, Input: runtimeContractTurnInput(contextPlan.DeveloperContext, input),
		})
		return outcome.RuntimeTurnRef, outcome, runtimeLifecycleOutcomeError(outcome, runtimecontract.LifecycleAccepted, true)
	}
	turnID, startOutcome, err := startTurn()
	typedBindingMissing := startOutcome.State == runtimecontract.LifecycleRejected && startOutcome.Failure != nil &&
		startOutcome.Failure.Code == runtimecontract.FailureCodeBindingNotFound && startOutcome.Failure.Phase == runtimecontract.FailurePhaseTurnStart
	if err != nil && typedBindingMissing {
		// The Thread can be evicted between resume and turn/start. Keep the
		// already-reserved Turn and retry this idempotent pre-start sequence once.
		if resumeErr := h.resumeAgentThread(agentID, rt); resumeErr == nil {
			turnID, _, err = startTurn()
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

func codexArtifactInput(text, loomContext string, artifacts []ThreadArtifact) []nativeInput {
	input := make([]nativeInput, 0, len(artifacts)+2)
	original := strings.TrimSpace(text)
	if original == "" && len(artifacts) > 0 {
		original = "Review the attached files."
	}
	if original != "" {
		input = append(input, nativeInput{Kind: nativeInputText, Text: original})
	}
	if strings.TrimSpace(loomContext) != "" {
		input = append(input, nativeInput{Kind: nativeInputText, Text: loomContext})
	}
	for _, artifact := range artifacts {
		if strings.HasPrefix(strings.ToLower(artifact.MimeType), "image/") {
			input = append(input, nativeInput{Kind: nativeInputLocalImage, Path: artifact.Path, MimeType: artifact.MimeType})
		}
	}
	return input
}

func runtimeContractTurnInput(developerContext string, input []nativeInput) []runtimecontract.InputBlock {
	blocks := make([]runtimecontract.InputBlock, 0, len(input)+1)
	if developerContext = strings.TrimSpace(developerContext); developerContext != "" {
		blocks = append(blocks, runtimecontract.InputBlock{
			Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleDeveloper, Text: developerContext,
		})
	}
	for _, block := range nativeInputToContract(input) {
		block.Role = runtimecontract.InputRoleUser
		blocks = append(blocks, block)
	}
	return blocks
}

func nativeInputToContract(input []nativeInput) []runtimecontract.InputBlock {
	result := make([]runtimecontract.InputBlock, 0, len(input))
	for _, item := range input {
		switch item.Kind {
		case nativeInputText:
			result = append(result, runtimecontract.InputBlock{Kind: runtimecontract.InputText, Text: item.Text})
		case nativeInputLocalImage:
			result = append(result, runtimecontract.InputBlock{Kind: runtimecontract.InputImage, Ref: item.Path, MIMEType: item.MimeType})
		}
	}
	return result
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
	State         string `json:"state,omitempty"`
	Message       string `json:"message,omitempty"`
	Reason        string `json:"reason,omitempty"`
	HeldMessageID string `json:"heldMessageId,omitempty"`
	HeldSubject   string `json:"heldSubject,omitempty"`
}

const interruptTerminalGrace = 3 * time.Second

func (h *Hub) effectiveInterruptTerminalGrace() time.Duration {
	if h.interruptTerminalGraceForTest > 0 {
		return h.interruptTerminalGraceForTest
	}
	return interruptTerminalGrace
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
	if err := h.runtimeMutationAllowedLocked(meta.ID); err != nil {
		h.mu.Unlock()
		return InterruptResult{}, err
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
	h.mu.Unlock()

	rt.startMu.Lock()
	defer rt.startMu.Unlock()
	h.mu.Lock()
	meta = h.agents[agentID]
	if h.stopping || meta == nil || h.runtimes[agentID] != rt || rt.activeTurn != turn || turn.finished {
		h.mu.Unlock()
		return InterruptResult{Interrupted: false, Message: "no active task"}, nil
	}
	if err := h.runtimeMutationAllowedLocked(agentID); err != nil {
		h.mu.Unlock()
		return InterruptResult{}, err
	}
	turnID := turn.nativeTurnID
	contract := rt.runtimeContract
	binding := rt.binding
	if binding.RuntimeKind == "" {
		binding = runtimeContractBinding(meta)
	}
	heldMessageID := turn.agentMessageID
	heldSubject := ""
	if message := h.comms[heldMessageID]; message != nil {
		heldSubject = message.Subject
	}
	h.mu.Unlock()

	if contract == nil {
		return InterruptResult{}, errf(500, "Agent Runtime Contract is unavailable")
	}
	if turnID == "" {
		return InterruptResult{}, errf(409, "active Turn is still starting; retry shortly")
	}
	interrupt := func(targetTurnID string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return runtimeInterruptReceiptError(contract.InterruptTurn(ctx, runtimecontract.TurnTarget{
			Binding: binding, TurnID: turn.turnID, RuntimeTurnRef: targetTurnID,
		}))
	}
	err := interrupt(turnID)
	if err != nil {
		if isRuntimeIndeterminate(err) {
			h.onRuntimeIndeterminate(rt, err)
			return InterruptResult{}, errf(500, "turn/interrupt outcome is indeterminate: %s", err)
		}
		return InterruptResult{}, errf(500, "turn/interrupt failed: %s", err)
	}
	// Interrupt success is only correlated receipt. The first canonical terminal
	// settles the Turn; silence is ambiguous and fences the effect domain.
	h.startWorker(func() {
		timer := time.NewTimer(h.effectiveInterruptTerminalGrace())
		defer timer.Stop()
		select {
		case <-turn.stopWatchdog:
			return
		case <-timer.C:
		}
		h.mu.Lock()
		active := !turn.finished && rt.activeTurn == turn
		h.mu.Unlock()
		if active {
			if inspector, ok := contract.(runtimeTurnSettlementInspector); ok {
				if !inspector.claimTurnUnsettled(turn.turnID) {
					return
				}
			}
			h.onRuntimeIndeterminate(rt, &runtimeIndeterminateError{failure: &runtimecontract.Failure{
				Code: "interrupt_terminal_missing", Phase: runtimecontract.FailurePhaseTurnInterrupt,
				Message: "Runtime interrupt terminal outcome is indeterminate", Cause: context.DeadlineExceeded,
			}})
		}
	})
	return InterruptResult{Interrupted: true, State: "accepted", Reason: reason, HeldMessageID: heldMessageID, HeldSubject: heldSubject}, nil
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
	if err := h.runtimeMutationAllowedLocked(meta.ID); err != nil {
		h.mu.Unlock()
		return nil, err
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
		if err := h.runtimeMutationAllowedLocked(agentID); err != nil {
			h.mu.Unlock()
			return nil, err
		}
		rt = h.runtimes[agentID]
		h.mu.Unlock()
		if rt != nil {
			rt.startMu.Lock()
			if h.archiveStartLockForTest != nil {
				h.archiveStartLockForTest(agentID)
			}
		}
		h.mu.Lock()
		if h.agents[agentID] == meta && h.runtimes[agentID] == rt {
			if err := h.runtimeMutationAllowedLocked(agentID); err != nil {
				h.mu.Unlock()
				if rt != nil {
					rt.startMu.Unlock()
				}
				return nil, err
			}
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
	return runtimeInterruptReceiptError(rt.runtimeContract.InterruptTurn(ctx, runtimecontract.TurnTarget{
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
	if err := runtimeLifecycleOutcomeError(capability.ArchiveBinding(ctx, binding), runtimecontract.LifecycleCompleted, false); err != nil {
		log.Printf("[codex-loom] archive native Runtime binding for Agent: %v", err)
	}
}

func closeRuntimeBinding(rt *runtime, binding runtimecontract.Binding) {
	if rt == nil || rt.runtimeContract == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	outcome := rt.runtimeContract.CloseBinding(ctx, binding)
	if err := runtimeLifecycleOutcomeError(outcome, runtimecontract.LifecycleCompleted, false); err != nil {
		log.Printf("[codex-loom] close Runtime binding: %v; forcing Agent Host close", err)
	}
	if rt.agentHost != nil {
		rt.agentHost.Close()
	}
}

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

// CanonicalHistory reads through Runtime Contract v2 and applies the public
// Loom identity projection.
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
	if contract.ContractVersion() != runtimecontract.Version {
		return CanonicalHistory{}, errf(503, "%s Runtime history Contract version %d is unsupported; expected %d", view.RuntimeBinding.Kind, contract.ContractVersion(), runtimecontract.Version)
	}
	history, failure := contract.ReadHistory(context.Background(), runtimecontract.HistoryRequest{
		Binding: runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: view.RuntimeBinding.Kind, NativeRef: view.nativeRuntimeRef},
		Count:   count, Offset: offset,
	})
	if failure != nil {
		if err := failure.Validate(); err != nil {
			return CanonicalHistory{}, errf(500, "read %s Runtime history: invalid Contract failure: %s", view.RuntimeBinding.Kind, err)
		}
		if failure.Code == "history_not_found" {
			return result, nil
		}
		return CanonicalHistory{}, errf(500, "read %s Runtime history: %s", view.RuntimeBinding.Kind, publicRuntimeFailureMessage(&view.Agent, "", failure.Message))
	}
	if err := history.Validate(); err != nil {
		return CanonicalHistory{}, errf(500, "read %s Runtime history: invalid Contract history: %s", view.RuntimeBinding.Kind, err)
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

// Shutdown closes all codex processes. Running agents keep status=running
// on disk so the next startup marks them interrupted.
