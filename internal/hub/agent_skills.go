package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

// AgentSkillConfig stores Agent-scoped Codex Skill exceptions. Runtime
// inventory and policy evidence are always read through the selected adapter.
type AgentSkillConfig struct {
	AgentID       string   `json:"agentId"`
	DisabledPaths []string `json:"disabledPaths"`
	UpdatedAt     string   `json:"updatedAt"`
}

type AgentSkillConfigParams struct {
	Path             string `json:"path"`
	Enabled          bool   `json:"enabled"`
	ExpectedRevision string `json:"expectedRevision"`
}

type RuntimeResourcePolicyView struct {
	Available     bool                                 `json:"available"`
	Mutable       bool                                 `json:"mutable"`
	Reason        string                               `json:"reason,omitempty"`
	Alternative   string                               `json:"alternative,omitempty"`
	Revision      string                               `json:"revision,omitempty"`
	DisabledPaths []string                             `json:"disabledPaths,omitempty"`
	Effective     bool                                 `json:"effective"`
	Evidence      []runtimecontract.CapabilityEvidence `json:"evidence,omitempty"`
}

type RuntimeResourceSnapshot struct {
	AgentID               string                        `json:"agentId"`
	AgentName             string                        `json:"agentName"`
	Cwd                   string                        `json:"cwd"`
	RuntimeKind           string                        `json:"runtimeKind"`
	BindingRevision       string                        `json:"bindingRevision"`
	ConfigurationRevision string                        `json:"configurationRevision"`
	Revision              string                        `json:"revision"`
	Semantics             string                        `json:"semantics"`
	Resources             []runtimecontract.Resource    `json:"resources"`
	Policy                RuntimeResourcePolicyView     `json:"policy"`
	Configuration         *RuntimeConfigurationEvidence `json:"configuration,omitempty"`

	generationRevision string
}

func (h *Hub) GetRuntimeResources(key string) (RuntimeResourceSnapshot, error) {
	h.mu.Lock()
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, errf(404, "agent not found: %s", key)
	}
	if h.resourcePolicyApplying[meta.ID] {
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, errf(409, "Runtime resource policy is being applied; retry after it settles")
	}
	agentID, name, cwd, binding := meta.ID, meta.Name, meta.Cwd, runtimeContractBinding(meta)
	configuration := meta.RuntimeConfiguration
	configuration.SettingSources = append([]string(nil), configuration.SettingSources...)
	providerID, model := effectiveProviderBinding(meta)
	sandbox, effort, imageEvidence := meta.Sandbox, meta.Effort, agentModelImageEvidence(meta)
	disabled := h.disabledSkillPathsLocked(agentID)
	rt := h.runtimes[agentID]
	if !runtimeHandleAlive(rt) {
		rt = nil
	}
	driver, driverErr := h.runtimeHostDriverLocked(binding.RuntimeKind)
	h.mu.Unlock()
	if driverErr != nil {
		return RuntimeResourceSnapshot{}, driverErr
	}
	var contract runtimecontract.Contract
	var provider runtimeResourceContractProvider
	var hostRevision string
	if rt == nil {
		provider, _ = driver.(runtimeResourceContractProvider)
	}
	if provider != nil {
		var err error
		contract, hostRevision, err = provider.ResourceContract(context.Background(), AgentHostRequest{AgentID: agentID})
		if err != nil {
			return RuntimeResourceSnapshot{}, err
		}
		configureRuntimeBinding(contract, sandbox, providerID, model, effort, imageEvidence, disabled)
		configureRuntimeOwnerConfiguration(contract, configuration)
	} else {
		if rt == nil {
			h.mu.Lock()
			meta = h.agents[agentID]
			if meta == nil || meta.Cwd != cwd || runtimeContractBinding(meta) != binding {
				h.mu.Unlock()
				return RuntimeResourceSnapshot{}, errf(409, "Agent binding or CWD changed while resources were opened; retry")
			}
			var err error
			rt, err = h.getRuntimeLocked(meta)
			h.mu.Unlock()
			if err != nil {
				return RuntimeResourceSnapshot{}, err
			}
		}
		rt.startMu.Lock()
		defer rt.startMu.Unlock()
		if err := waitReady(rt); err != nil {
			return RuntimeResourceSnapshot{}, errf(500, "Runtime not ready: %s", err)
		}
		contract = rt.runtimeContract
	}
	h.mu.Lock()
	meta = h.agents[agentID]
	if meta == nil || meta.Cwd != cwd || runtimeContractBinding(meta) != binding || (rt != nil && h.runtimes[agentID] != rt) {
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, errf(409, "Agent Runtime binding changed while resources were checked; retry")
	}
	query := h.captureRuntimeCapabilityQueryLocked(meta, rt)
	query.contract = contract
	h.mu.Unlock()
	snapshot, err := h.queryRuntimeCapabilities(query)
	if err != nil {
		return RuntimeResourceSnapshot{}, err
	}
	if err := requireCapability(snapshot, runtimecontract.CapabilityResourceInventory, "Runtime resource inventory"); err != nil {
		return RuntimeResourceSnapshot{}, err
	}
	capability, ok := contract.(runtimecontract.ResourceInventoryCapability)
	if !ok {
		return RuntimeResourceSnapshot{}, errf(500, "Runtime resource inventory hook is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	inventory, failure := capability.InspectResources(ctx, runtimecontract.ResourceInventoryRequest{Binding: binding, Cwd: cwd})
	if failure != nil {
		return RuntimeResourceSnapshot{}, errf(500, "inspect Runtime resources: %s", failure.Message)
	}
	if err := inventory.Validate(); err != nil {
		return RuntimeResourceSnapshot{}, errf(500, "invalid Runtime resource inventory: %s", err)
	}
	var configurationEvidence *RuntimeConfigurationEvidence
	if binding.RuntimeKind == "claude" {
		provider, ok := contract.(runtimeConfigurationEvidenceProvider)
		if !ok {
			return RuntimeResourceSnapshot{}, errf(500, "Claude Runtime configuration evidence hook is unavailable")
		}
		evidence, ok := provider.RuntimeConfigurationEvidence()
		if !ok {
			return RuntimeResourceSnapshot{}, errf(500, "Claude Runtime did not return verified configuration evidence")
		}
		if err := validateRuntimeConfigurationEvidence(configuration, evidence); err != nil {
			return RuntimeResourceSnapshot{}, errf(500, "invalid Claude Runtime configuration evidence: %s", err)
		}
		configurationEvidence = &evidence
	}
	policy := RuntimeResourcePolicyView{}
	if descriptor, found := capabilityDescriptor(snapshot, runtimecontract.CapabilityResourcePolicy); found {
		policy.Reason, policy.Alternative = descriptor.Reason, descriptor.Alternative
		policy.Available = descriptor.Availability == runtimecontract.CapabilityAvailable
		policy.Mutable = policy.Available
	}
	if policy.Available {
		policyCapability := contract.(runtimecontract.ResourcePolicyCapability)
		state, policyFailure := policyCapability.InspectResourcePolicy(ctx, runtimecontract.ResourcePolicyRequest{Binding: binding, Cwd: cwd})
		if policyFailure != nil {
			return RuntimeResourceSnapshot{}, errf(500, "inspect Runtime resource policy: %s", policyFailure.Message)
		}
		if err := state.Validate(); err != nil {
			return RuntimeResourceSnapshot{}, errf(500, "invalid Runtime resource policy: %s", err)
		}
		policy.Revision, policy.DisabledPaths, policy.Effective, policy.Evidence = state.Revision, state.DisabledPaths, state.Effective, state.Evidence
		if policy.Effective {
			for i := range inventory.Resources {
				for _, path := range policy.DisabledPaths {
					if skillPathMatches(inventory.Resources[i].Path, path) {
						inventory.Resources[i].Enabled = false
						break
					}
				}
			}
		}
	}
	if policy.Available && rt != nil {
		policy.Mutable = false
		policy.Reason = "This Runtime binding is already loaded and cannot safely hot-apply resource policy."
		policy.Alternative = "Restart CodexLoom, reopen Resources, and apply the policy before the binding loads."
	}
	h.mu.Lock()
	current := h.agents[agentID]
	stale := current == nil || current.Cwd != cwd || runtimeContractBinding(current) != binding || (rt != nil && h.runtimes[agentID] != rt)
	configurationRevision := ""
	if current != nil {
		configurationRevision = resourcePolicyConfigurationRevision(current, h.disabledSkillPathsLocked(agentID))
	}
	if rt != nil && rt.resourceGeneration == "" {
		rt.resourceGeneration = newRuntimeResourceGeneration()
	}
	runtimeGeneration, nativeGeneration := hostRevision, hostRevision
	if rt != nil {
		runtimeGeneration = rt.resourceGeneration
		if rt.hostGeneration != 0 {
			nativeGeneration = fmt.Sprintf("codex-host:%d", rt.hostGeneration)
		} else {
			nativeGeneration = rt.resourceGeneration
		}
	}
	h.mu.Unlock()
	if provider != nil && !provider.ResourceContractCurrent(hostRevision) {
		stale = true
	}
	if stale {
		return RuntimeResourceSnapshot{}, errf(409, "Agent binding or CWD changed while resources were checked; retry")
	}
	encoded, _ := json.Marshal([]any{snapshot.Revision, inventory.Revision, policy.Revision, policy.Effective, runtimeGeneration, configurationRevision})
	return RuntimeResourceSnapshot{
		AgentID: agentID, AgentName: name, Cwd: cwd, RuntimeKind: binding.RuntimeKind, BindingRevision: query.scope.BindingRevision, ConfigurationRevision: configurationRevision,
		Revision: "resources:" + sha256Hex(encoded)[:16], Semantics: inventory.Semantics,
		Resources: inventory.Resources, Policy: policy, Configuration: configurationEvidence, generationRevision: nativeGeneration,
	}, nil
}

func newRuntimeResourceGeneration() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return "runtime:" + hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("runtime:%d", time.Now().UnixNano())
}

func normalizeAgentSkillPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errf(400, "Skill path is required")
	}
	if !filepath.IsAbs(path) {
		return "", errf(400, "Skill path must be absolute")
	}
	path = filepath.Clean(path)
	if filepath.Base(path) != "SKILL.md" {
		return "", errf(400, "Skill path must point to SKILL.md")
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = filepath.Clean(resolved)
	}
	return path, nil
}

func (h *Hub) UpdateRuntimeResourcePolicy(key string, params AgentSkillConfigParams) (RuntimeResourceSnapshot, error) {
	path, err := normalizeAgentSkillPath(params.Path)
	if err != nil {
		return RuntimeResourceSnapshot{}, err
	}
	if strings.TrimSpace(params.ExpectedRevision) == "" {
		return RuntimeResourceSnapshot{}, errf(400, "expectedRevision is required")
	}
	h.resourcePolicyMu.Lock()
	defer h.resourcePolicyMu.Unlock()
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, errf(409, "CodexLoom is shutting down; Runtime resource policy cannot be changed")
	}
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, errf(404, "agent not found: %s", key)
	}
	if err := h.runtimeMutationAllowedLocked(meta.ID); err != nil {
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, err
	}
	h.mu.Unlock()
	current, err := h.GetRuntimeResources(key)
	if err != nil {
		return RuntimeResourceSnapshot{}, err
	}
	if current.Revision != params.ExpectedRevision {
		return RuntimeResourceSnapshot{}, errf(409, "Runtime resource snapshot is stale; reopen Resources and retry")
	}
	if !current.Policy.Available {
		return RuntimeResourceSnapshot{}, errf(409, "%s; %s", current.Policy.Reason, current.Policy.Alternative)
	}
	nextPaths := append([]string(nil), current.Policy.DisabledPaths...)
	if params.Enabled {
		kept := nextPaths[:0]
		for _, existing := range nextPaths {
			if !skillPathMatches(existing, path) {
				kept = append(kept, existing)
			}
		}
		nextPaths = kept
	} else {
		nextPaths = append(nextPaths, path)
	}
	nextPaths = normalizedDisabledSkillPaths(nextPaths)
	if current.Policy.Effective && strings.Join(nextPaths, "\x00") == strings.Join(normalizedDisabledSkillPaths(current.Policy.DisabledPaths), "\x00") {
		return current, nil
	}

	h.mu.Lock()
	meta = h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, errf(404, "agent not found: %s", key)
	}
	if err := h.runtimeMutationAllowedLocked(meta.ID); err != nil {
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, err
	}
	if meta.ID != current.AgentID || meta.Name != current.AgentName || meta.Cwd != current.Cwd || meta.RuntimeBinding.Kind != current.RuntimeKind || h.runtimeCapabilityScopeLocked(meta).BindingRevision != current.BindingRevision || resourcePolicyConfigurationRevision(meta, h.disabledSkillPathsLocked(meta.ID)) != current.ConfigurationRevision {
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, errf(409, "Agent binding or CWD changed after Resources were inspected; retry")
	}
	if meta.Status == "running" {
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, errf(409, "agent %q is running; change resource policy between Turns", meta.Name)
	}
	if h.stopping {
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, errf(409, "CodexLoom is shutting down; Runtime resource policy cannot be changed")
	}
	if meta.Source == "edge" {
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, errf(409, "edge Agent %q must be adopted before configuring resources", meta.Name)
	}
	if rt := h.runtimes[meta.ID]; runtimeHandleAlive(rt) {
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, errf(409, "the Runtime binding is already loaded and cannot safely hot-apply resource policy; restart CodexLoom, reopen Resources, and retry")
	}
	if h.resourcePolicyApplying == nil {
		h.resourcePolicyApplying = map[string]bool{}
	}
	if h.resourcePolicyApplying[meta.ID] {
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, errf(409, "Runtime resource policy is already being applied")
	}
	h.resourcePolicyApplying[meta.ID] = true
	previous, hadPrevious := h.agentSkillConfigs[meta.ID]
	if h.agentSkillConfigs == nil {
		h.agentSkillConfigs = map[string]*AgentSkillConfig{}
	}
	pending := &AgentSkillConfig{AgentID: meta.ID, DisabledPaths: nextPaths, UpdatedAt: now()}
	h.agentSkillConfigs[meta.ID] = pending
	expectedConfigurationRevision := resourcePolicyConfigurationRevision(meta, nextPaths)
	saveAgentSkillConfigs := h.agentSkillConfigSaverLocked()
	agentID, cwd, binding := meta.ID, meta.Cwd, runtimeContractBinding(meta)
	rt, acquireErr := h.getRuntimeLockedForResourcePolicy(meta)
	h.mu.Unlock()
	restoreLocked := func() {
		if h.agentSkillConfigs[agentID] != pending && h.agentSkillConfigs[agentID] != nil {
			return
		}
		if hadPrevious {
			h.agentSkillConfigs[agentID] = previous
		} else {
			delete(h.agentSkillConfigs, agentID)
		}
	}
	failApply := func(rt *runtime, cause error) {
		h.mu.Lock()
		restoreLocked()
		h.mu.Unlock()
		if rt != nil {
			h.invalidateRuntimeEffectDomain(rt, cause)
		}
		h.mu.Lock()
		delete(h.resourcePolicyApplying, agentID)
		h.mu.Unlock()
	}
	if acquireErr != nil {
		failApply(nil, acquireErr)
		return RuntimeResourceSnapshot{}, acquireErr
	}
	rt.startMu.Lock()
	readyErr := waitReady(rt)
	if readyErr == nil {
		nativeGeneration := rt.resourceGeneration
		if rt.hostGeneration != 0 {
			nativeGeneration = fmt.Sprintf("codex-host:%d", rt.hostGeneration)
		}
		if nativeGeneration != current.generationRevision {
			readyErr = fmt.Errorf("Runtime Host generation changed while resource policy was applied")
		}
	}
	if readyErr == nil {
		capability, ok := rt.runtimeContract.(runtimecontract.ResourcePolicyCapability)
		if !ok {
			readyErr = fmt.Errorf("Runtime resource policy hook is unavailable")
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			state, failure := capability.InspectResourcePolicy(ctx, runtimecontract.ResourcePolicyRequest{Binding: binding, Cwd: cwd, DisabledPaths: nextPaths})
			cancel()
			if failure != nil {
				readyErr = fmt.Errorf("%s", failure.Message)
			} else if !state.Effective || len(state.Evidence) == 0 || strings.Join(normalizedDisabledSkillPaths(state.DisabledPaths), "\x00") != strings.Join(nextPaths, "\x00") {
				readyErr = fmt.Errorf("Runtime did not prove the exact resource policy effective")
			}
		}
	}
	rt.startMu.Unlock()
	if readyErr != nil {
		failApply(rt, readyErr)
		return RuntimeResourceSnapshot{}, errf(409, "apply Runtime resource policy before persistence: %s", readyErr)
	}

	h.mu.Lock()
	meta = h.agents[agentID]
	pendingCurrent := h.agentSkillConfigs[agentID] == pending
	stale := meta == nil || h.runtimes[agentID] != rt || meta.Cwd != cwd || runtimeContractBinding(meta) != binding || meta.Status == "running" || h.stopping || !pendingCurrent
	if meta != nil && resourcePolicyConfigurationRevision(meta, h.disabledSkillPathsLocked(agentID)) != expectedConfigurationRevision {
		stale = true
	}
	if stale {
		restoreLocked()
		h.mu.Unlock()
		cause := fmt.Errorf("Agent state changed while resource policy was applied")
		h.invalidateRuntimeEffectDomain(rt, cause)
		h.mu.Lock()
		delete(h.resourcePolicyApplying, agentID)
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, errf(409, "Agent binding, CWD, or policy changed while applying Runtime resources; the Runtime was fenced")
	}
	if len(nextPaths) == 0 {
		delete(h.agentSkillConfigs, agentID)
	}
	persisted := cloneAgentSkillConfigs(h.agentSkillConfigs)
	h.mu.Unlock()
	saveErr := saveAgentSkillConfigs(persisted)
	h.mu.Lock()
	meta = h.agents[agentID]
	postStale := meta == nil || h.runtimes[agentID] != rt || meta.Cwd != cwd || runtimeContractBinding(meta) != binding || meta.Status == "running" || h.stopping || !h.resourcePolicyApplying[agentID]
	if meta != nil && resourcePolicyConfigurationRevision(meta, h.disabledSkillPathsLocked(agentID)) != expectedConfigurationRevision {
		postStale = true
	}
	if saveErr != nil || postStale {
		restoreLocked()
		compensation := cloneAgentSkillConfigs(h.agentSkillConfigs)
		h.mu.Unlock()
		if saveErr == nil {
			if err := saveAgentSkillConfigs(compensation); err != nil {
				saveErr = fmt.Errorf("Agent state changed after persistence and compensation failed: %w", err)
			} else {
				saveErr = fmt.Errorf("Agent state changed after persistence; previous policy was restored")
			}
		}
		h.invalidateRuntimeEffectDomain(rt, saveErr)
		h.mu.Lock()
		delete(h.resourcePolicyApplying, agentID)
		h.mu.Unlock()
		return RuntimeResourceSnapshot{}, errf(500, "save Runtime resource policy: %s; the Runtime was fenced", saveErr)
	}
	h.emitLocked(agentID, "loom/runtime-resources-updated", map[string]any{"path": path, "enabled": params.Enabled})
	delete(h.resourcePolicyApplying, agentID)
	h.mu.Unlock()
	return h.GetRuntimeResources(agentID)
}

func (h *Hub) runtimeMutationAllowedLocked(agentID string) error {
	if agent := h.agents[agentID]; agent != nil && agent.NativeConversationDivergence != nil {
		return errf(409, "Native Conversation Divergence fences this Agent until Owner recovery")
	}
	if h.resourcePolicyApplying[agentID] {
		return errf(409, "Runtime resource policy is being applied; retry after it settles")
	}
	if h.runtimeConfigurationApplying[agentID] {
		return errf(409, "Runtime configuration is being applied; retry after it settles")
	}
	if agent := h.agents[agentID]; agent != nil && agent.ContextMaintenance != nil && agent.ContextMaintenance.State == contextMaintenanceStarted {
		return errf(409, "Runtime context maintenance is in progress; retry after it settles")
	}
	return nil
}

func (h *Hub) agentSkillConfigSaverLocked() func(any) error {
	if h.saveAgentSkillConfigsForTest != nil {
		return h.saveAgentSkillConfigsForTest
	}
	st := h.st
	return st.SaveAgentSkillConfigs
}

func resourcePolicyConfigurationRevision(meta *Agent, disabledPaths []string) string {
	if meta == nil {
		return ""
	}
	digest := sha256Hex([]byte(strings.Join([]string{
		meta.Name, meta.Cwd, meta.ProviderID, meta.Model, meta.Effort, meta.Sandbox, meta.ApprovalPolicy,
		agentSkillConfigHash(normalizedDisabledSkillPaths(disabledPaths)),
	}, "\x00")))
	return "resource-config:" + digest[:16]
}

func cloneAgentSkillConfigs(configs map[string]*AgentSkillConfig) map[string]*AgentSkillConfig {
	clone := make(map[string]*AgentSkillConfig, len(configs))
	for id, config := range configs {
		if config == nil {
			continue
		}
		copied := *config
		copied.DisabledPaths = append([]string(nil), config.DisabledPaths...)
		clone[id] = &copied
	}
	return clone
}

func normalizedDisabledSkillPaths(paths []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if normalized, err := normalizeAgentSkillPath(path); err == nil {
			path = normalized
		} else {
			path = filepath.Clean(path)
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func agentSkillConfigHash(paths []string) string {
	sum := sha256.Sum256([]byte(strings.Join(normalizedDisabledSkillPaths(paths), "\x00")))
	return hex.EncodeToString(sum[:])
}

func codexAgentSkillConfig(paths []string) map[string]any {
	paths = normalizedDisabledSkillPaths(paths)
	entries := make([]map[string]any, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, map[string]any{
			"path":    path,
			"enabled": false,
		})
	}
	return map[string]any{
		"skills": map[string]any{
			"config": entries,
		},
	}
}

func (h *Hub) disabledSkillPathsLocked(agentID string) []string {
	if h.agentSkillConfigs == nil {
		return nil
	}
	config := h.agentSkillConfigs[agentID]
	if config == nil {
		return nil
	}
	return append([]string(nil), config.DisabledPaths...)
}

func skillPathMatches(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if left == right {
		return true
	}
	leftResolved, leftErr := filepath.EvalSymlinks(left)
	rightResolved, rightErr := filepath.EvalSymlinks(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftResolved) == filepath.Clean(rightResolved)
}
