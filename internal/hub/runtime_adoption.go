package hub

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

const (
	RuntimeConversationDiscovery     = "conversation_discovery"
	RuntimeConversationInspection    = "conversation_inspection"
	RuntimeConversationAdoption      = "conversation_adoption"
	RuntimeConversationManualRestore = "manual_restore"
)

type RuntimeConversationCapability struct {
	ID        string `json:"id"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type RuntimeConversationCapabilities struct {
	RuntimeKind   string                          `json:"runtimeKind"`
	Revision      string                          `json:"revision"`
	Capabilities  []RuntimeConversationCapability `json:"capabilities"`
	Configuration *RuntimeConfigurationDescriptor `json:"configuration,omitempty"`
}

type RuntimeConversationCandidate struct {
	ID            string `json:"id"`
	Revision      string `json:"revision"`
	RuntimeKind   string `json:"runtimeKind"`
	Name          string `json:"name,omitempty"`
	Cwd           string `json:"cwd"`
	UpdatedAt     string `json:"updatedAt"`
	Compatible    bool   `json:"compatible"`
	Compatibility string `json:"compatibility,omitempty"`
	AlreadyBound  bool   `json:"alreadyBound"`
}

type nativeConversationCandidate struct {
	RuntimeConversationCandidate
	nativeRef      string
	nativeRevision string
}

type HistoryBoundary struct {
	Kind           string `json:"kind"`
	CreatedAt      string `json:"createdAt"`
	ImportedTurns  int    `json:"importedTurns"`
	Disclosure     string `json:"disclosure"`
	NativeRevision string `json:"nativeRevision,omitempty"`
}

type NativeConversationDivergence struct {
	Code           string `json:"code"`
	DetectedAt     string `json:"detectedAt"`
	Summary        string `json:"summary"`
	Recovery       string `json:"recovery"`
	NativeRevision string `json:"nativeRevision,omitempty"`
}

type runtimeNativeDivergenceEvidence interface {
	NativeDivergenceRevision() string
}

const historyBoundaryDisclosure = "Native conversation content before this History Boundary remains in Claude context but is not imported as Loom Turns."

type runtimeHistoryBoundaryConfiguration interface {
	SetRuntimeHistoryBoundary(*HistoryBoundary, func(string) error)
}

type runtimeConversationCatalog interface {
	DiscoverConversations(context.Context) ([]nativeConversationCandidate, error)
	InspectConversation(context.Context, string) (nativeConversationCandidate, error)
}

type AdoptConversationParams struct {
	CandidateID          string               `json:"candidateId"`
	ExpectedRevision     string               `json:"expectedRevision"`
	ThreadID             string               `json:"threadId,omitempty"`
	Name                 string               `json:"name"`
	Cwd                  string               `json:"cwd"`
	Sandbox              string               `json:"sandbox"`
	ApprovalPolicy       string               `json:"approvalPolicy"`
	ProviderID           string               `json:"providerId"`
	Model                string               `json:"model"`
	Effort               string               `json:"effort"`
	RuntimeConfiguration RuntimeConfiguration `json:"runtimeConfiguration"`
}

func conversationCapabilitySnapshot(kind string, available bool) RuntimeConversationCapabilities {
	reason := "this Runtime does not expose a native conversation catalog"
	capabilities := []RuntimeConversationCapability{
		{ID: RuntimeConversationDiscovery, Available: available},
		{ID: RuntimeConversationInspection, Available: available},
		{ID: RuntimeConversationAdoption, Available: available},
		{ID: RuntimeConversationManualRestore, Available: true},
	}
	if !available {
		for index := 0; index < 3; index++ {
			capabilities[index].Reason = reason
		}
	}
	encoded, _ := json.Marshal(capabilities)
	return RuntimeConversationCapabilities{RuntimeKind: kind, Revision: "conversation:" + shortDigest(encoded), Capabilities: capabilities}
}

func (h *Hub) RuntimeConversationCapabilities(kind string) (RuntimeConversationCapabilities, error) {
	h.mu.Lock()
	driver, err := h.runtimeHostDriverLocked(strings.TrimSpace(kind))
	h.mu.Unlock()
	if err != nil {
		return RuntimeConversationCapabilities{}, err
	}
	_, available := driver.(runtimeConversationCatalog)
	snapshot := conversationCapabilitySnapshot(kind, available)
	if provider, ok := driver.(runtimeConfigurationDescriptorProvider); ok {
		descriptor := provider.RuntimeConfigurationDescriptor()
		snapshot.Configuration = &descriptor
		encoded, _ := json.Marshal(snapshot)
		snapshot.Revision = "conversation:" + shortDigest(encoded)
	}
	return snapshot, nil
}

func (h *Hub) RuntimeConversationCatalogs() []RuntimeConversationCapabilities {
	result := make([]RuntimeConversationCapabilities, 0, 3)
	for _, kind := range []string{"codex", "pi", "claude"} {
		if snapshot, err := h.RuntimeConversationCapabilities(kind); err == nil {
			result = append(result, snapshot)
		}
	}
	return result
}

func (h *Hub) DiscoverRuntimeConversations(kind string) ([]RuntimeConversationCandidate, error) {
	catalog, err := h.runtimeConversationCatalog(kind)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	native, err := catalog.DiscoverConversations(ctx)
	if err != nil {
		return nil, errf(502, "discover %s Runtime conversations: %s", kind, publicConversationCatalogError(err))
	}
	result := make([]RuntimeConversationCandidate, 0, len(native))
	for _, candidate := range native {
		public := candidate.RuntimeConversationCandidate
		h.mu.Lock()
		public.AlreadyBound = h.agentByNativeBindingLocked(kind, candidate.nativeRef) != nil
		h.mu.Unlock()
		result = append(result, public)
	}
	return result, nil
}

func (h *Hub) InspectRuntimeConversation(kind, candidateID string) (RuntimeConversationCandidate, error) {
	candidate, catalog, err := h.resolveRuntimeConversation(kind, candidateID)
	if err != nil {
		return RuntimeConversationCandidate{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	inspected, err := catalog.InspectConversation(ctx, candidate.nativeRef)
	if err != nil || inspected.ID != candidateID {
		return RuntimeConversationCandidate{}, errf(409, "conversation candidate is no longer available")
	}
	public := inspected.RuntimeConversationCandidate
	h.mu.Lock()
	public.AlreadyBound = h.agentByNativeBindingLocked(kind, inspected.nativeRef) != nil
	h.mu.Unlock()
	return public, nil
}

func (h *Hub) AdoptRuntimeConversation(kind string, p AdoptConversationParams) (AgentView, error) {
	h.conversationAdoptionMu.Lock()
	defer h.conversationAdoptionMu.Unlock()
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		return AgentView{}, errf(503, "CodexLoom is shutting down")
	}
	h.mu.Unlock()
	p.CandidateID, p.ExpectedRevision, p.ThreadID, p.Name = strings.TrimSpace(p.CandidateID), strings.TrimSpace(p.ExpectedRevision), strings.TrimSpace(p.ThreadID), strings.TrimSpace(p.Name)
	if p.Name == "" || p.ThreadID == "" && (p.CandidateID == "" || p.ExpectedRevision == "") {
		return AgentView{}, errf(400, "name and either threadId or candidateId with expectedRevision are required")
	}
	if p.ThreadID != "" {
		if kind != "codex" {
			return AgentView{}, errf(400, "threadId adoption is supported only for the Codex Runtime")
		}
		if p.CandidateID != "" || p.ExpectedRevision != "" {
			return AgentView{}, errf(400, "threadId cannot be combined with candidateId or expectedRevision")
		}
		p.CandidateID = candidateToken(kind, p.ThreadID)
	}
	if strings.TrimSpace(p.Cwd) != "" {
		stableCwd, cwdErr := normalizeAdoptionCwd(p.Cwd)
		if cwdErr != nil {
			return AgentView{}, cwdErr
		}
		p.Cwd = stableCwd
	}
	if !validAgentName(p.Name) {
		return AgentView{}, errf(400, "name may contain Unicode letters, marks, numbers, hyphens, and underscores")
	}
	approvalRequested := strings.TrimSpace(p.ApprovalPolicy) != ""
	providerRequested := strings.TrimSpace(p.ProviderID) != "" || strings.TrimSpace(p.Model) != "" || strings.TrimSpace(p.Effort) != ""
	p.Sandbox = strings.TrimSpace(p.Sandbox)
	if p.Sandbox == "" {
		p.Sandbox = "danger-full-access"
	}
	p.ApprovalPolicy = strings.TrimSpace(p.ApprovalPolicy)
	if p.ApprovalPolicy == "" {
		p.ApprovalPolicy = "never"
	}
	p.ProviderID = normalizeProviderID(p.ProviderID)
	p.Model = strings.TrimSpace(p.Model)
	p.Effort = normalizeEffort(strings.TrimSpace(p.Effort))
	configuration, configurationErr := normalizeRuntimeConfiguration(kind, p.RuntimeConfiguration)
	if configurationErr != nil {
		return AgentView{}, configurationErr
	}
	p.RuntimeConfiguration = configuration
	if err := h.validateRequestedRuntimeConfiguration(kind, p.Sandbox, providerRequested, approvalRequested); err != nil {
		return AgentView{}, err
	}
	if p.ProviderID != "" && !nameRe.MatchString(p.ProviderID) {
		return AgentView{}, errf(400, "providerId must match [a-zA-Z0-9_-]+")
	}
	if p.ProviderID != "" && p.Model == "" {
		return AgentView{}, errf(400, "model is required for a custom Provider")
	}
	if err := validateModelEffort(p.ProviderID, p.Model, p.Effort); err != nil {
		return AgentView{}, err
	}
	// The opaque token is stable from the private binding, so an exact retry
	// remains idempotent even when resuming changed native recency metadata or
	// the catalog temporarily stops listing an already-bound conversation.
	h.mu.Lock()
	for _, existing := range h.agents {
		if existing.RuntimeBinding.Kind != kind || candidateToken(kind, existing.RuntimeBinding.NativeRef) != p.CandidateID {
			continue
		}
		if adoptionIntentMatches(existing, existing.SourceCwd, p) {
			view := h.viewLocked(existing)
			h.mu.Unlock()
			return view, nil
		}
		h.mu.Unlock()
		return AgentView{}, errf(409, "conversation candidate is already bound with different Agent configuration")
	}
	h.mu.Unlock()

	var candidate nativeConversationCandidate
	var catalog runtimeConversationCatalog
	var err error
	if p.ThreadID != "" {
		catalog, err = h.runtimeConversationCatalog(kind)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			candidate, err = catalog.InspectConversation(ctx, p.ThreadID)
			cancel()
		}
		if err != nil || candidate.nativeRef != p.ThreadID || candidate.ID != p.CandidateID {
			return AgentView{}, errf(404, "Codex Thread %q was not found or is not available for adoption", p.ThreadID)
		}
		p.ExpectedRevision = candidate.Revision
	} else {
		candidate, catalog, err = h.resolveRuntimeConversation(kind, p.CandidateID)
		if err != nil {
			return AgentView{}, err
		}
		if candidate.Revision != p.ExpectedRevision {
			return AgentView{}, errf(409, "conversation candidate changed; inspect it again")
		}
	}
	if !candidate.Compatible {
		return AgentView{}, errf(409, "conversation candidate is incompatible: %s", candidate.Compatibility)
	}
	// Re-read after the Owner's expected-revision check so adoption never binds
	// a stale catalog row whose native conversation changed in the meantime.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	inspected, inspectErr := catalog.InspectConversation(ctx, candidate.nativeRef)
	cancel()
	if inspectErr != nil {
		return AgentView{}, errf(409, "conversation candidate is no longer available")
	}
	if inspected.ID != p.CandidateID || inspected.Revision != p.ExpectedRevision {
		return AgentView{}, errf(409, "conversation candidate changed; inspect it again")
	}
	candidate = inspected
	if !candidate.Compatible {
		return AgentView{}, errf(409, "conversation candidate is incompatible: %s", candidate.Compatibility)
	}
	if p.Cwd == "" {
		stableCwd, cwdErr := normalizeAdoptionCwd(candidate.Cwd)
		if cwdErr != nil {
			return AgentView{}, errf(409, "conversation candidate has no stable workspace")
		}
		p.Cwd = stableCwd
	}
	if kind == "claude" {
		sourceCwd, cwdErr := normalizeAdoptionCwd(candidate.Cwd)
		if cwdErr != nil || p.Cwd != sourceCwd {
			return AgentView{}, errf(409, "Claude adoption requires the inspected conversation workspace")
		}
	}

	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		return AgentView{}, errf(503, "CodexLoom is shutting down")
	}
	if existing := h.agentByNativeBindingLocked(kind, candidate.nativeRef); existing != nil {
		if adoptionIntentMatches(existing, candidate.Cwd, p) {
			view := h.viewLocked(existing)
			h.mu.Unlock()
			return view, nil
		}
		h.mu.Unlock()
		return AgentView{}, errf(409, "conversation candidate is already bound with different Agent configuration")
	}
	if existing := h.resolveLocked(p.Name); existing != nil {
		h.mu.Unlock()
		return AgentView{}, errf(409, "agent %q already exists", p.Name)
	}
	driver, driverErr := h.runtimeHostDriverLocked(kind)
	h.mu.Unlock()
	if driverErr != nil {
		return AgentView{}, driverErr
	}

	idBytes := make([]byte, 4)
	if _, err := rand.Read(idBytes); err != nil {
		return AgentView{}, errf(500, "create Agent identity")
	}
	agentID := hex.EncodeToString(idBytes)
	host, err := driver.Acquire(context.Background(), AgentHostRequest{AgentID: agentID, Cwd: p.Cwd})
	if err != nil {
		return AgentView{}, errf(502, "prepare %s Runtime adoption: %s", kind, publicConversationCatalogError(err))
	}
	committed := false
	defer func() {
		if !committed {
			host.Close()
		}
	}()
	contract := host.Contract()
	if contract == nil || contract.ContractVersion() != runtimecontract.Version {
		return AgentView{}, errf(500, "Runtime Contract is unavailable for adoption")
	}
	configureRuntimeBinding(contract, p.Sandbox, p.ProviderID, p.Model, p.Effort, runtimeModelImageEvidence{}, nil)
	configureRuntimeWorkspace(contract, p.Cwd)
	configureRuntimeOwnerConfiguration(contract, p.RuntimeConfiguration)
	binding := runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: kind, NativeRef: candidate.nativeRef}
	boundary := (*HistoryBoundary)(nil)
	if kind == "claude" {
		inspector, ok := contract.(runtimeOwnerConfigurationInspector)
		if !ok {
			return AgentView{}, errf(500, "Claude Runtime configuration inspection is unavailable")
		}
		ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
		_, configurationFailure := inspector.InspectRuntimeOwnerConfiguration(ctx, binding, p.Cwd, p.RuntimeConfiguration)
		cancel()
		if configurationFailure != nil {
			return AgentView{}, errf(409, "Claude Runtime configuration scope could not be validated")
		}
		if candidate.nativeRevision == "" {
			return AgentView{}, errf(409, "Claude conversation boundary could not be verified")
		}
		boundary = &HistoryBoundary{Kind: "history_boundary", CreatedAt: now(), ImportedTurns: 0, Disclosure: historyBoundaryDisclosure, NativeRevision: candidate.nativeRevision}
		if configured, ok := contract.(runtimeHistoryBoundaryConfiguration); ok {
			configured.SetRuntimeHistoryBoundary(boundary, nil)
		} else {
			return AgentView{}, errf(500, "Claude Runtime History Boundary support is unavailable")
		}
	}
	ctx, cancel = context.WithTimeout(context.Background(), h.effectiveThreadResumeTimeout())
	outcome := contract.ResumeBinding(ctx, binding)
	cancel()
	if err := runtimeLifecycleOutcomeError(outcome, runtimecontract.LifecycleCompleted, false); err != nil {
		return AgentView{}, errf(409, "Runtime conversation could not be resumed")
	}
	if kind != "claude" {
		ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
		history, failure := contract.ReadHistory(ctx, runtimecontract.HistoryRequest{Binding: binding, Count: 1})
		cancel()
		if failure != nil || history.Validate() != nil {
			return AgentView{}, errf(409, "Runtime conversation history could not be verified")
		}
	}

	threadID := newIntegrationID("thr")
	importedGoal, goalErr := readAdoptedRuntimeGoal(contract, binding, threadID)
	if goalErr != nil {
		return AgentView{}, goalErr
	}
	meta := &Agent{ID: agentID, Name: p.Name, Cwd: p.Cwd, SourceCwd: candidate.Cwd, ThreadID: threadID, RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: kind, NativeRef: candidate.nativeRef}, HistoryBoundary: boundary, RuntimeTurnBindings: map[string]string{}, Sandbox: p.Sandbox, ApprovalPolicy: p.ApprovalPolicy, ProviderID: p.ProviderID, Model: p.Model, Effort: p.Effort, RuntimeConfiguration: p.RuntimeConfiguration, Status: "idle", CreatedAt: now(), UpdatedAt: now()}
	rt := &runtime{agentID: agentID, agentHost: host, runtimeContract: contract, binding: binding, ready: make(chan struct{}), approvals: map[string]*approval{}}
	close(rt.ready)
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		return AgentView{}, errf(503, "CodexLoom is shutting down")
	}
	if existing := h.agentByNativeBindingLocked(kind, candidate.nativeRef); existing != nil {
		h.mu.Unlock()
		if adoptionIntentMatches(existing, candidate.Cwd, p) {
			return h.GetAgent(existing.ID)
		}
		return AgentView{}, errf(409, "conversation candidate is already bound")
	}
	if h.resolveLocked(p.Name) != nil {
		h.mu.Unlock()
		return AgentView{}, errf(409, "agent %q already exists", p.Name)
	}
	h.agents[agentID], h.seqs[agentID], h.runtimes[agentID] = meta, h.st.LastSeq(agentID), rt
	if err := h.persistAgentsLocked(); err != nil {
		delete(h.agents, agentID)
		delete(h.seqs, agentID)
		delete(h.runtimes, agentID)
		h.mu.Unlock()
		return AgentView{}, errf(500, "save adopted Agent: %s", err)
	}
	if importedGoal != nil {
		h.goals[agentID] = importedGoal
		if err := h.persistGoalsLocked(); err != nil {
			delete(h.goals, agentID)
			delete(h.agents, agentID)
			delete(h.seqs, agentID)
			delete(h.runtimes, agentID)
			compensationErr := h.persistAgentsLocked()
			h.mu.Unlock()
			if compensationErr != nil {
				return AgentView{}, errf(500, "save adopted Goal: %s; remove failed Agent: %s", err, compensationErr)
			}
			return AgentView{}, errf(500, "save adopted Goal: %s", err)
		}
	}
	committed = true
	if configured, ok := contract.(runtimeHistoryBoundaryConfiguration); ok && boundary != nil {
		configured.SetRuntimeHistoryBoundary(boundary, func(revision string) error {
			return h.commitHistoryBoundaryRevision(agentID, revision)
		})
	}
	host.SetFailureHandler(func(err error) { h.onRuntimeFailure(rt, err) })
	contract.SetEventHandler(func(event runtimecontract.Event) { h.onCanonicalRuntimeEvent(rt, event) })
	h.emitLocked(agentID, "loom/agent-adopted", map[string]any{"id": agentID, "name": p.Name, "cwd": p.Cwd, "sourceCwd": candidate.Cwd, "threadId": meta.ThreadID, "runtimeKind": kind})
	h.emitStatusLocked(meta, meta.Status)
	view := h.viewLocked(meta)
	h.mu.Unlock()
	return view, nil
}

func (h *Hub) commitHistoryBoundaryRevision(agentID, revision string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta := h.agents[agentID]
	if meta == nil || meta.HistoryBoundary == nil || strings.TrimSpace(revision) == "" {
		return errors.New("History Boundary is unavailable")
	}
	previous := meta.HistoryBoundary.NativeRevision
	previousUpdatedAt := meta.UpdatedAt
	meta.HistoryBoundary.NativeRevision = revision
	meta.UpdatedAt = now()
	if err := h.persistAgentsLocked(); err != nil {
		meta.HistoryBoundary.NativeRevision = previous
		meta.UpdatedAt = previousUpdatedAt
		return err
	}
	return nil
}

func (h *Hub) fenceNativeConversationDivergence(agentID, revision string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta := h.agents[agentID]
	if meta == nil || meta.HistoryBoundary == nil {
		return errf(409, "Agent has no adoptive History Boundary")
	}
	previous := *meta
	meta.NativeConversationDivergence = &NativeConversationDivergence{
		Code: runtimecontract.FailureCodeNativeConversationDivergence, DetectedAt: now(),
		Summary:        "Claude native context changed after adoption. Loom imported no native content and fenced execution.",
		Recovery:       "Explicitly accept the current native revision to establish a new History Boundary.",
		NativeRevision: revision,
	}
	meta.Status = "fenced"
	meta.LastError = "Native Conversation Divergence"
	meta.CurrentTask = ""
	meta.CurrentTurnID = ""
	meta.UpdatedAt = now()
	if err := h.persistAgentsLocked(); err != nil {
		*meta = previous
		return err
	}
	if rt := h.runtimes[agentID]; rt != nil && rt.activeTurn != nil && !rt.activeTurn.finished {
		turn := rt.activeTurn
		turn.finished = true
		close(turn.stopWatchdog)
		rt.activeTurn = nil
		h.abortTurnApprovalsLocked(agentID, turn.turnID, rt, "the native conversation diverged before the Turn started")
		rt.approvals = map[string]*approval{}
		h.finishInboxAttemptLocked(turn, "interrupted", "Native Conversation Divergence")
		h.finishAgentMessageTurnLocked(turn, "interrupted", "Native Conversation Divergence")
	}
	h.emitStatusLocked(meta, meta.Status)
	return nil
}

func (h *Hub) RecoverNativeConversationDivergence(key string) (AgentView, error) {
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		return AgentView{}, errf(503, "CodexLoom is shutting down")
	}
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return AgentView{}, errf(404, "agent not found: %s", key)
	}
	if meta.NativeConversationDivergence == nil || meta.HistoryBoundary == nil || meta.NativeConversationDivergence.NativeRevision == "" {
		h.mu.Unlock()
		return AgentView{}, errf(409, "Agent has no recoverable Native Conversation Divergence")
	}
	agentID := meta.ID
	targetRevision := meta.NativeConversationDivergence.NativeRevision
	if rt := h.runtimes[agentID]; rt != nil && rt.agentHost != nil {
		host := rt.agentHost
		h.mu.Unlock()
		host.Close()
		h.mu.Lock()
		meta = h.agents[agentID]
		if meta == nil || meta.NativeConversationDivergence == nil || meta.HistoryBoundary == nil || meta.NativeConversationDivergence.NativeRevision != targetRevision {
			h.mu.Unlock()
			return AgentView{}, errf(409, "Native Conversation Divergence changed during recovery")
		}
	}
	previousBoundary := *meta.HistoryBoundary
	previousDivergence := meta.NativeConversationDivergence
	previousStatus, previousLastError, previousUpdatedAt := meta.Status, meta.LastError, meta.UpdatedAt
	meta.HistoryBoundary.NativeRevision = targetRevision
	meta.HistoryBoundary.CreatedAt = now()
	meta.NativeConversationDivergence = nil
	meta.Status, meta.LastError, meta.UpdatedAt = "idle", "", now()
	if err := h.persistAgentsLocked(); err != nil {
		*meta.HistoryBoundary = previousBoundary
		meta.NativeConversationDivergence = previousDivergence
		meta.Status, meta.LastError, meta.UpdatedAt = previousStatus, previousLastError, previousUpdatedAt
		h.mu.Unlock()
		return AgentView{}, errf(500, "persist Native Conversation Divergence recovery: %s", err)
	}
	delete(h.runtimes, meta.ID)
	publicBoundary := *meta.HistoryBoundary
	publicBoundary.NativeRevision = ""
	h.emitLocked(meta.ID, "loom/native-conversation-recovered", map[string]any{"agentId": meta.ID, "historyBoundary": &publicBoundary})
	h.emitStatusLocked(meta, meta.Status)
	view := h.viewLocked(meta)
	h.mu.Unlock()
	return view, nil
}

func readAdoptedRuntimeGoal(contract runtimecontract.Contract, binding runtimecontract.Binding, loomThreadID string) (*ThreadGoal, error) {
	if binding.RuntimeKind != "codex" {
		return nil, nil
	}
	capability, ok := contract.(runtimeGoalCapability)
	if !ok {
		return nil, errf(500, "Codex Runtime Goal inspection is unavailable during adoption")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	nativeGoal, err := capability.RuntimeGoal(ctx, binding)
	cancel()
	if err != nil {
		return nil, errf(409, "Codex Runtime Goal could not be inspected during adoption")
	}
	if nativeGoal == nil {
		return nil, nil
	}
	goal := cloneGoalRecord(nativeGoal)
	goal.Objective = strings.TrimSpace(goal.Objective)
	if goal.Objective == "" || len(goal.Objective) > 4000 || !validGoalStatus(goal.Status) || goal.TokenBudget != nil && *goal.TokenBudget <= 0 {
		return nil, errf(409, "Codex Runtime returned an invalid Goal during adoption")
	}
	stamp := time.Now().UnixMilli()
	goal.ID, goal.Version, goal.ThreadID = newIntegrationID("goal"), 1, loomThreadID
	if goal.CreatedAt == 0 {
		goal.CreatedAt = stamp
	}
	if goal.UpdatedAt == 0 {
		goal.UpdatedAt = stamp
	}
	goal.NativeSyncState = goalNativeSyncPending
	goal.NativeSyncedAt, goal.NativeSyncError = 0, ""
	goal.NativeSyncBindingRevision = goalBindingRevision(binding)
	goal.NativeMigrationBlocked = false
	goal.NativeMigrationBindingRevision = ""
	return goal, nil
}

func (h *Hub) runtimeConversationCatalog(kind string) (runtimeConversationCatalog, error) {
	h.mu.Lock()
	driver, err := h.runtimeHostDriverLocked(strings.TrimSpace(kind))
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	catalog, ok := driver.(runtimeConversationCatalog)
	if !ok {
		return nil, errf(409, "%s Runtime does not support conversation discovery; use manual Restore", kind)
	}
	return catalog, nil
}

func (h *Hub) resolveRuntimeConversation(kind, candidateID string) (nativeConversationCandidate, runtimeConversationCatalog, error) {
	candidateID = strings.TrimSpace(candidateID)
	if candidateID == "" {
		return nativeConversationCandidate{}, nil, errf(400, "candidate id is required")
	}
	catalog, err := h.runtimeConversationCatalog(kind)
	if err != nil {
		return nativeConversationCandidate{}, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	candidates, err := catalog.DiscoverConversations(ctx)
	if err != nil {
		return nativeConversationCandidate{}, nil, errf(502, "discover %s Runtime conversations: %s", kind, publicConversationCatalogError(err))
	}
	for _, candidate := range candidates {
		if candidate.ID == candidateID {
			return candidate, catalog, nil
		}
	}
	return nativeConversationCandidate{}, nil, errf(404, "conversation candidate not found")
}

func (h *Hub) agentByNativeBindingLocked(kind, nativeRef string) *Agent {
	for _, agent := range h.agents {
		if agent.RuntimeBinding.Kind == kind && agent.RuntimeBinding.NativeRef == nativeRef {
			return agent
		}
	}
	return nil
}

func adoptionIntentMatches(agent *Agent, cwd string, p AdoptConversationParams) bool {
	return agent != nil && agent.Name == p.Name && agent.Cwd == p.Cwd && agent.SourceCwd == cwd && agent.Sandbox == p.Sandbox && agent.ApprovalPolicy == p.ApprovalPolicy && agent.ProviderID == p.ProviderID && agent.Model == p.Model && agent.Effort == p.Effort &&
		strings.Join(agent.RuntimeConfiguration.SettingSources, "\x00") == strings.Join(p.RuntimeConfiguration.SettingSources, "\x00") && agent.RuntimeConfiguration.Authentication == p.RuntimeConfiguration.Authentication
}

func normalizeAdoptionCwd(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !filepath.IsAbs(value) {
		return "", errf(400, "cwd must be an absolute path")
	}
	return filepath.Clean(value), nil
}

func candidateToken(kind, nativeRef string) string {
	digest := sha256.Sum256([]byte(kind + "\x00" + nativeRef))
	return "cand_" + hex.EncodeToString(digest[:16])
}
func candidateRevision(fields ...string) string {
	return "candidate:" + shortDigest([]byte(strings.Join(fields, "\x00")))
}
func shortDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:8])
}
func publicConversationCatalogError(_ error) string { return "Runtime catalog is unavailable" }

func readPiConversationCandidate(path string) (nativeConversationCandidate, error) {
	file, err := os.Open(path)
	if err != nil {
		return nativeConversationCandidate{}, err
	}
	defer file.Close()
	line, err := bufio.NewReader(file).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nativeConversationCandidate{}, err
	}
	var header struct{ Type, ID, Timestamp, Cwd string }
	if json.Unmarshal(line, &header) != nil || header.Type != "session" || header.ID == "" || !filepath.IsAbs(header.Cwd) {
		return nativeConversationCandidate{}, fmt.Errorf("unsupported Pi session header")
	}
	stat, err := file.Stat()
	if err != nil {
		return nativeConversationCandidate{}, err
	}
	updated := stat.ModTime().UTC().Format(time.RFC3339Nano)
	name := filepath.Base(header.Cwd)
	if name == "." || name == string(filepath.Separator) {
		name = "Pi session"
	}
	return nativeConversationCandidate{RuntimeConversationCandidate: RuntimeConversationCandidate{ID: candidateToken("pi", path), Revision: candidateRevision(header.ID, header.Timestamp, header.Cwd, updated, fmt.Sprint(stat.Size())), RuntimeKind: "pi", Name: name, Cwd: header.Cwd, UpdatedAt: updated, Compatible: true}, nativeRef: path}, nil
}

func discoverPiConversationFiles() ([]string, error) {
	root, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root = filepath.Join(root, ".pi", "agent", "sessions")
	files := []string{}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				if os.IsNotExist(walkErr) {
					return filepath.SkipDir
				}
				return walkErr
			}
			return nil
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return files, nil
}

func (d *piRuntimeHostDriver) DiscoverConversations(ctx context.Context) ([]nativeConversationCandidate, error) {
	paths, err := discoverPiConversationFiles()
	if err != nil {
		return nil, err
	}
	result := make([]nativeConversationCandidate, 0, len(paths))
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		candidate, err := readPiConversationCandidate(path)
		if err == nil {
			result = append(result, candidate)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt > result[j].UpdatedAt })
	return result, nil
}

func (d *piRuntimeHostDriver) InspectConversation(ctx context.Context, nativeRef string) (nativeConversationCandidate, error) {
	if err := ctx.Err(); err != nil {
		return nativeConversationCandidate{}, err
	}
	candidate, err := readPiConversationCandidate(nativeRef)
	if err != nil {
		return nativeConversationCandidate{}, err
	}
	if _, _, err := readPiSessionEntries(nativeRef); err != nil {
		candidate.Compatible = false
		candidate.Compatibility = "Pi Session history is unreadable"
	}
	return candidate, nil
}
