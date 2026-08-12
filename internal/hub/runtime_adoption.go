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
	RuntimeKind  string                          `json:"runtimeKind"`
	Revision     string                          `json:"revision"`
	Capabilities []RuntimeConversationCapability `json:"capabilities"`
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
}

type nativeConversationCandidate struct {
	RuntimeConversationCandidate
	nativeRef string
}

type runtimeConversationCatalog interface {
	DiscoverConversations(context.Context) ([]nativeConversationCandidate, error)
	InspectConversation(context.Context, string) (nativeConversationCandidate, error)
}

type AdoptConversationParams struct {
	CandidateID      string `json:"candidateId"`
	ExpectedRevision string `json:"expectedRevision"`
	Name             string `json:"name"`
	Sandbox          string `json:"sandbox"`
	ApprovalPolicy   string `json:"approvalPolicy"`
	ProviderID       string `json:"providerId"`
	Model            string `json:"model"`
	Effort           string `json:"effort"`
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
	return conversationCapabilitySnapshot(kind, available), nil
}

func (h *Hub) RuntimeConversationCatalogs() []RuntimeConversationCapabilities {
	result := make([]RuntimeConversationCapabilities, 0, 2)
	for _, kind := range []string{"codex", "pi"} {
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
		result = append(result, candidate.RuntimeConversationCandidate)
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
	return inspected.RuntimeConversationCandidate, nil
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
	p.CandidateID, p.ExpectedRevision, p.Name = strings.TrimSpace(p.CandidateID), strings.TrimSpace(p.ExpectedRevision), strings.TrimSpace(p.Name)
	if p.CandidateID == "" || p.ExpectedRevision == "" || p.Name == "" {
		return AgentView{}, errf(400, "candidateId, expectedRevision, and name are required")
	}
	if !nameRe.MatchString(p.Name) {
		return AgentView{}, errf(400, "name must match [a-zA-Z0-9_-]+")
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
		if adoptionIntentMatches(existing, existing.Cwd, p) {
			view := h.viewLocked(existing)
			h.mu.Unlock()
			return view, nil
		}
		h.mu.Unlock()
		return AgentView{}, errf(409, "conversation candidate is already bound with different Agent configuration")
	}
	h.mu.Unlock()

	candidate, catalog, err := h.resolveRuntimeConversation(kind, p.CandidateID)
	if err != nil {
		return AgentView{}, err
	}
	if candidate.Revision != p.ExpectedRevision {
		return AgentView{}, errf(409, "conversation candidate changed; inspect it again")
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
	host, err := driver.Acquire(context.Background(), AgentHostRequest{AgentID: agentID})
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
	configureRuntimeBinding(contract, p.Sandbox, p.ProviderID, p.Model, p.Effort, nil)
	binding := runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: kind, NativeRef: candidate.nativeRef}
	ctx, cancel = context.WithTimeout(context.Background(), h.effectiveThreadResumeTimeout())
	outcome := contract.ResumeBinding(ctx, binding)
	cancel()
	if err := runtimeLifecycleOutcomeError(outcome, runtimecontract.LifecycleCompleted, false); err != nil {
		return AgentView{}, errf(409, "Runtime conversation could not be resumed")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
	history, failure := contract.ReadHistory(ctx, runtimecontract.HistoryRequest{Binding: binding, Count: 1})
	cancel()
	if failure != nil || history.Validate() != nil {
		return AgentView{}, errf(409, "Runtime conversation history could not be verified")
	}

	meta := &Agent{ID: agentID, Name: p.Name, Cwd: candidate.Cwd, ThreadID: newIntegrationID("thr"), RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: kind, NativeRef: candidate.nativeRef}, RuntimeTurnBindings: map[string]string{}, Sandbox: p.Sandbox, ApprovalPolicy: p.ApprovalPolicy, ProviderID: p.ProviderID, Model: p.Model, Effort: p.Effort, Status: "idle", CreatedAt: now(), UpdatedAt: now()}
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
	committed = true
	host.SetFailureHandler(func(err error) { h.onRuntimeFailure(rt, err) })
	contract.SetEventHandler(func(event runtimecontract.Event) { h.onCanonicalRuntimeEvent(rt, event) })
	h.emitLocked(agentID, "loom/agent-adopted", map[string]any{"id": agentID, "name": p.Name, "cwd": candidate.Cwd, "threadId": meta.ThreadID, "runtimeKind": kind})
	h.emitStatusLocked(meta, meta.Status)
	view := h.viewLocked(meta)
	h.mu.Unlock()
	return view, nil
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
	return agent != nil && agent.Name == p.Name && agent.Cwd == cwd && agent.Sandbox == p.Sandbox && agent.ApprovalPolicy == p.ApprovalPolicy && agent.ProviderID == p.ProviderID && agent.Model == p.Model && agent.Effort == p.Effort
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
