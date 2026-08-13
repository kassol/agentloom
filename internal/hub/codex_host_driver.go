package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yan5xu/codex-loom/internal/codex"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

type codexRuntimeHostDriver struct {
	hub *Hub

	mu           sync.Mutex
	handles      map[string]*codexAgentHost
	failedHosts  map[uint64]bool
	shutdown     bool
	shutdownOnce sync.Once
	shutdownErr  error
}

func newCodexRuntimeHostDriver(h *Hub) *codexRuntimeHostDriver {
	return &codexRuntimeHostDriver{hub: h, handles: map[string]*codexAgentHost{}, failedHosts: map[uint64]bool{}}
}

func (d *codexRuntimeHostDriver) newContract(agentID string, native *codexAgentRuntime) *codexRuntimeContract {
	return &codexRuntimeContract{agentID: agentID, native: native, modelCatalog: d.availableModels}
}

func (d *codexRuntimeHostDriver) availableModels() ([]runtimecontract.Model, error) {
	providers, err := d.hub.ListModelProviders()
	if err != nil {
		return nil, err
	}
	models := make([]runtimecontract.Model, 0)
	for _, provider := range providers {
		if !provider.Configured || !provider.CredentialConfigured {
			continue
		}
		if provider.ID == "openai" {
			models = append(models, runtimecontract.Model{
				Provider: "openai", ID: "", DisplayName: "Default (Codex)", Reasoning: true,
				ThinkingLevels: []string{runtimecontract.ThinkingLevelDefault, "minimal", "low", "medium", "high", "xhigh"}, DefaultThinkingLevel: runtimecontract.ThinkingLevelDefault, ImageInput: true,
			})
		}
		for _, model := range provider.ModelDetails {
			models = append(models, runtimeModelFromCatalog(model))
		}
	}
	return models, nil
}

// agentContract keeps the native client inside the Codex Host adapter while
// startup hydration consumes only Runtime Contract capabilities.
func (h *codexHostRuntime) agentContract(agentID string) runtimecontract.Contract {
	return &codexRuntimeContract{agentID: agentID, native: &codexAgentRuntime{client: h.client}}
}

func (d *codexRuntimeHostDriver) Preflight(context.Context) error {
	if d == nil || d.hub == nil {
		return fmt.Errorf("Codex Runtime Host Driver is unavailable")
	}
	bin, err := codex.ResolveBin(codexHostBin())
	if err != nil {
		return err
	}
	version, err := codex.Version(bin)
	if err != nil {
		return fmt.Errorf("read Codex Runtime version: %w", err)
	}
	if version == "" {
		return fmt.Errorf("read Codex Runtime version: empty output")
	}
	return nil
}

func (d *codexRuntimeHostDriver) CapabilitySnapshot(context.Context, runtimecontract.Binding) runtimecontract.CapabilitySnapshot {
	return codexControlPlaneCapabilitySnapshot()
}

type codexConversationThread struct {
	ID        string `json:"id"`
	Preview   string `json:"preview"`
	Name      string `json:"name"`
	Cwd       string `json:"cwd"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	RecencyAt *int64 `json:"recencyAt"`
	Ephemeral bool   `json:"ephemeral"`
	Turns     []struct {
		Status string `json:"status"`
	} `json:"turns,omitempty"`
}

func (d *codexRuntimeHostDriver) DiscoverConversations(ctx context.Context) ([]nativeConversationCandidate, error) {
	host, err := d.hub.ensureCodexHost()
	if err != nil {
		return nil, err
	}
	result := []nativeConversationCandidate{}
	cursor := ""
	for {
		params := map[string]any{"limit": 100, "sortKey": "recency_at", "sortDirection": "desc"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := host.client.Request("thread/list", params, contractTimeout(ctx, 15*time.Second))
		if err != nil {
			return nil, err
		}
		var page struct {
			Data       []codexConversationThread `json:"data"`
			NextCursor string                    `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("decode Codex thread/list: %w", err)
		}
		for _, thread := range page.Data {
			result = append(result, codexConversationCandidate(thread))
		}
		if page.NextCursor == "" {
			return result, nil
		}
		cursor = page.NextCursor
	}
}

func (d *codexRuntimeHostDriver) InspectConversation(ctx context.Context, nativeRef string) (nativeConversationCandidate, error) {
	host, err := d.hub.ensureCodexHost()
	if err != nil {
		return nativeConversationCandidate{}, err
	}
	raw, err := host.client.Request("thread/read", map[string]any{"threadId": nativeRef, "includeTurns": true}, contractTimeout(ctx, 15*time.Second))
	if err != nil {
		return nativeConversationCandidate{}, err
	}
	var response struct {
		Thread codexConversationThread `json:"thread"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || response.Thread.ID != nativeRef {
		return nativeConversationCandidate{}, fmt.Errorf("Codex thread/read returned an invalid conversation")
	}
	return codexConversationCandidate(response.Thread), nil
}

func codexConversationCandidate(thread codexConversationThread) nativeConversationCandidate {
	updatedAt := thread.UpdatedAt
	if thread.RecencyAt != nil {
		updatedAt = *thread.RecencyAt
	}
	name := strings.TrimSpace(thread.Name)
	if name == "" {
		name = strings.TrimSpace(thread.Preview)
	}
	if len([]rune(name)) > 80 {
		name = string([]rune(name)[:80]) + "…"
	}
	active := false
	for _, turn := range thread.Turns {
		switch strings.ToLower(strings.TrimSpace(turn.Status)) {
		case "running", "inprogress", "in_progress", "active":
			active = true
		}
	}
	compatible, reason := !thread.Ephemeral && thread.ID != "" && strings.TrimSpace(thread.Cwd) != "" && !active, ""
	if !compatible {
		if active {
			reason = "Codex Thread has an active Turn"
		} else {
			reason = "ephemeral or incomplete Codex Thread"
		}
	}
	updated := time.Unix(updatedAt, 0).UTC().Format(time.RFC3339)
	return nativeConversationCandidate{RuntimeConversationCandidate: RuntimeConversationCandidate{
		ID: candidateToken("codex", thread.ID), Revision: candidateRevision(thread.ID, thread.Name, thread.Preview, thread.Cwd, fmt.Sprint(thread.CreatedAt), fmt.Sprint(thread.UpdatedAt), fmt.Sprint(updatedAt), fmt.Sprint(thread.Ephemeral), fmt.Sprint(active)),
		RuntimeKind: "codex", Name: name, Cwd: thread.Cwd, UpdatedAt: updated, Compatible: compatible, Compatibility: reason,
	}, nativeRef: thread.ID}
}

func (d *codexRuntimeHostDriver) HistoryContract(request AgentHostRequest) runtimecontract.Contract {
	return &codexRuntimeContract{agentID: request.AgentID, native: &codexAgentRuntime{}, turnsByNative: map[string]runtimeTurnCorrelation{}}
}

func (d *codexRuntimeHostDriver) ResourceContract(_ context.Context, request AgentHostRequest) (runtimecontract.Contract, string, error) {
	host, err := d.hub.ensureCodexHost()
	if err != nil {
		return nil, "", err
	}
	return d.newContract(request.AgentID, &codexAgentRuntime{client: host.client}), fmt.Sprintf("codex-host:%d", host.generation), nil
}

func (d *codexRuntimeHostDriver) ResourceContractCurrent(revision string) bool {
	d.hub.mu.Lock()
	defer d.hub.mu.Unlock()
	return d.hub.codexHost != nil && revision == fmt.Sprintf("codex-host:%d", d.hub.codexHost.generation) && !d.hub.codexHost.client.Closed()
}

func (d *codexRuntimeHostDriver) Acquire(ctx context.Context, request AgentHostRequest) (AgentHost, error) {
	if err := d.Preflight(ctx); err != nil {
		return nil, err
	}
	d.hub.mu.Lock()
	handle, host, err := d.acquireLocked(request)
	d.hub.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := waitCodexHost(host); err != nil {
		return nil, err
	}
	return handle, nil
}

// acquireLocked binds one Agent handle to the current shared Codex host.
// d.hub.mu must be held; initialization is awaited by the public Acquire or by
// Hub's existing runtime readiness path after releasing the lock.
func (d *codexRuntimeHostDriver) acquireLocked(request AgentHostRequest) (*codexAgentHost, *codexHostRuntime, error) {
	d.mu.Lock()
	if d.shutdown {
		d.mu.Unlock()
		return nil, nil, fmt.Errorf("Codex Runtime Host Driver is shut down")
	}
	if handle := d.handles[request.AgentID]; handle != nil && handle.Alive() {
		d.mu.Unlock()
		return handle, handle.host, nil
	}
	d.mu.Unlock()

	host, err := d.ensureLocked()
	if err != nil {
		return nil, nil, err
	}
	contract := d.newContract(request.AgentID, &codexAgentRuntime{client: host.client})
	handle := &codexAgentHost{host: host, contract: contract}
	contract.release = handle.Close
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shutdown {
		return nil, nil, fmt.Errorf("Codex Runtime Host Driver shut down during acquire")
	}
	if existing := d.handles[request.AgentID]; existing != nil && existing.Alive() {
		return existing, existing.host, nil
	}
	d.handles[request.AgentID] = handle
	return handle, host, nil
}

func (d *codexRuntimeHostDriver) Shutdown(context.Context) error {
	if d == nil {
		return nil
	}
	d.shutdownOnce.Do(func() {
		d.mu.Lock()
		d.shutdown = true
		handles := make([]*codexAgentHost, 0, len(d.handles))
		for _, handle := range d.handles {
			handles = append(handles, handle)
		}
		d.mu.Unlock()
		for _, handle := range handles {
			handle.Close()
		}
		d.hub.mu.Lock()
		host := d.hub.detachCodexHostLocked()
		d.hub.mu.Unlock()
		d.shutdownErr = closeCodexHost(host)
	})
	return d.shutdownErr
}

func (d *codexRuntimeHostDriver) dispatchNativeEvent(agentID, method string, params json.RawMessage) bool {
	d.mu.Lock()
	handle := d.handles[agentID]
	d.mu.Unlock()
	if handle != nil && handle.Alive() {
		return handle.contract.handleNativeEvent(method, params) > 0
	}
	return false
}

func (d *codexRuntimeHostDriver) onNativeServerRequest(host *codexHostRuntime, id json.RawMessage, method string, params json.RawMessage) {
	threadID := notificationThreadID(params)
	d.mu.Lock()
	handles := make([]*codexAgentHost, 0, len(d.handles))
	for _, handle := range d.handles {
		if handle != nil && handle.Alive() && handle.host == host {
			handles = append(handles, handle)
		}
	}
	d.mu.Unlock()
	var target *codexRuntimeContract
	for _, handle := range handles {
		if handle.contract.handlesNativeThread(threadID) {
			target = handle.contract
			break
		}
	}
	if target == nil && threadID == "" {
		for _, handle := range handles {
			if !handle.contract.canHandleApproval() {
				continue
			}
			if target != nil {
				target = nil
				break
			}
			target = handle.contract
		}
	}
	if target != nil && target.handleNativeServerRequest(host.client, id, method, params) {
		return
	}
	_ = host.client.RespondError(id, -32601, "CodexLoom does not handle "+method)
}

func (d *codexRuntimeHostDriver) fanoutHostFailure(generation uint64, err error) {
	if err == nil {
		return
	}
	d.mu.Lock()
	if d.failedHosts[generation] {
		d.mu.Unlock()
		return
	}
	d.failedHosts[generation] = true
	handles := make([]*codexAgentHost, 0, len(d.handles))
	for _, handle := range d.handles {
		if handle.host != nil && handle.host.generation == generation {
			handles = append(handles, handle)
		}
	}
	d.mu.Unlock()
	for _, handle := range handles {
		handle.fail(err)
	}
}

// suppressHostFailureFanout records that an explicitly invalidated generation
// has already been checkpointed. Its later process-exit callback must not
// checkpoint or schedule the same Agents a second time.
func (d *codexRuntimeHostDriver) suppressHostFailureFanout(generation uint64) {
	d.mu.Lock()
	d.failedHosts[generation] = true
	d.mu.Unlock()
}

type codexAgentHost struct {
	mu       sync.Mutex
	host     *codexHostRuntime
	contract *codexRuntimeContract
	failure  func(error)
	closed   bool
}

func (h *codexAgentHost) Alive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.closed && h.host != nil && h.host.client != nil && !h.host.client.Closed()
}

func (h *codexAgentHost) Contract() runtimecontract.Contract { return h.contract }

func (h *codexAgentHost) RuntimeHostGeneration() uint64 {
	if h == nil || h.host == nil {
		return 0
	}
	return h.host.generation
}

func (h *codexAgentHost) waitRuntimeHostReady(context.Context) error {
	if h == nil {
		return fmt.Errorf("Codex Agent Host is unavailable")
	}
	return waitCodexHost(h.host)
}

func (h *codexAgentHost) SetFailureHandler(handler func(error)) {
	h.mu.Lock()
	h.failure = handler
	h.mu.Unlock()
}

func (h *codexAgentHost) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	h.failure = nil
	h.mu.Unlock()
	h.contract.SetEventHandler(nil)
}

func (h *codexAgentHost) fail(err error) {
	h.mu.Lock()
	handler := h.failure
	closed := h.closed
	h.mu.Unlock()
	if !closed && handler != nil {
		handler(err)
	}
}
