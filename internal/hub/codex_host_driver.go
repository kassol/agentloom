package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/yan5xu/codex-loom/internal/codex"
	"github.com/yan5xu/codex-loom/internal/rollout"
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

func (d *codexRuntimeHostDriver) SanitizeProviderHistory(_ context.Context, nativeRef, backupDir string) (RuntimeProviderHistorySanitizeResult, error) {
	result, err := rollout.SanitizeReasoningContent(nativeRef, backupDir)
	if err != nil {
		return RuntimeProviderHistorySanitizeResult{}, err
	}
	return RuntimeProviderHistorySanitizeResult{Changed: result.Changed, OriginalPath: result.OriginalPath, BackupPath: result.BackupPath}, nil
}

func (d *codexRuntimeHostDriver) RestoreProviderHistory(_ context.Context, backupPath, originalPath string) error {
	return rollout.RestoreRolloutBackup(backupPath, originalPath)
}

func (d *codexRuntimeHostDriver) HistoryContract(request AgentHostRequest) runtimecontract.Contract {
	return &codexRuntimeContract{agentID: request.AgentID, native: &codexAgentRuntime{}, turnsByNative: map[string]runtimeTurnCorrelation{}}
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
	contract := &codexRuntimeContract{agentID: request.AgentID, native: &codexAgentRuntime{client: host.client}}
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
