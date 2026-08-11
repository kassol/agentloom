package hub

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/yan5xu/codex-loom/internal/pi"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

// piRuntimeHostDriver owns Pi's per-Agent process boundary. Unlike Codex,
// Pi has no shared process: every acquired handle starts/resumes and closes
// only its own RPC process through its Contract.
type piRuntimeHostDriver struct {
	hub *Hub

	mu           sync.Mutex
	handles      map[string]*piAgentHost
	shutdown     bool
	shutdownOnce sync.Once
}

func newPiRuntimeHostDriver(h *Hub) *piRuntimeHostDriver {
	return &piRuntimeHostDriver{hub: h, handles: map[string]*piAgentHost{}}
}

func (d *piRuntimeHostDriver) Preflight(context.Context) error {
	return pi.Check("")
}

func (d *piRuntimeHostDriver) CapabilitySnapshot(context.Context, runtimecontract.Binding) runtimecontract.CapabilitySnapshot {
	return piControlPlaneCapabilitySnapshot()
}

// PreflightPiRuntime exposes the Driver-owned startup prerequisite without
// opening the Store or creating any per-Agent session directory.
func PreflightPiRuntime(ctx context.Context) error {
	return (&piRuntimeHostDriver{}).Preflight(ctx)
}

func (d *piRuntimeHostDriver) Acquire(ctx context.Context, request AgentHostRequest) (AgentHost, error) {
	return d.acquireWhileHubLocked(ctx, request)
}

func (d *piRuntimeHostDriver) acquireWhileHubLocked(ctx context.Context, request AgentHostRequest) (AgentHost, error) {
	if err := d.Preflight(ctx); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shutdown {
		return nil, fmt.Errorf("Pi Runtime Host Driver is shut down")
	}
	if existing := d.handles[request.AgentID]; existing != nil && !existing.closedState() {
		if !existing.started() || existing.Alive() {
			return existing, nil
		}
		existing.Close()
	}
	native := newPiAgentRuntime(request.AgentID, d.hub.st.Dir(), d.hub.runtimeAPIURL)
	contract := newPiRuntimeContract(request.AgentID, native)
	handle := &piAgentHost{native: native, contract: contract}
	contract.host = handle
	contract.release = handle.Close
	handle.facade = &piRuntimeV1Facade{host: handle, contract: contract, native: native}
	native.SetRuntimeEventHandlers(contract.handleNativeEvent, handle.fail)
	d.handles[request.AgentID] = handle
	return handle, nil
}

func (d *piRuntimeHostDriver) Shutdown(context.Context) error {
	if d == nil {
		return nil
	}
	d.shutdownOnce.Do(func() {
		d.mu.Lock()
		d.shutdown = true
		handles := make([]*piAgentHost, 0, len(d.handles))
		for _, handle := range d.handles {
			handles = append(handles, handle)
		}
		d.mu.Unlock()
		for _, handle := range handles {
			handle.Close()
		}
	})
	return nil
}

func (d *piRuntimeHostDriver) aliveCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	count := 0
	for _, handle := range d.handles {
		if handle.Alive() {
			count++
		}
	}
	return count
}

type piAgentHost struct {
	mu       sync.Mutex
	native   *piAgentRuntime
	contract *piRuntimeContract
	facade   *piRuntimeV1Facade
	failure  func(error)
	closed   bool
}

var _ RuntimeHostDriver = (*piRuntimeHostDriver)(nil)
var _ AgentHost = (*piAgentHost)(nil)

func (h *piAgentHost) Alive() bool {
	h.mu.Lock()
	closed, native := h.closed, h.native
	h.mu.Unlock()
	return !closed && native != nil && native.Alive()
}

func (h *piAgentHost) closedState() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

func (h *piAgentHost) started() bool {
	h.mu.Lock()
	native := h.native
	h.mu.Unlock()
	if native == nil {
		return false
	}
	native.mu.Lock()
	defer native.mu.Unlock()
	return native.rpc != nil
}

func (h *piAgentHost) Contract() runtimecontract.Contract { return h.contract }

func (h *piAgentHost) legacyRuntime() AgentRuntime { return h.facade }

func (h *piAgentHost) waitRuntimeHostReady(context.Context) error { return nil }

func (h *piAgentHost) createBinding(request RuntimeBindingRequest) (string, error) {
	h.mu.Lock()
	closed, native := h.closed, h.native
	h.mu.Unlock()
	if closed || native == nil {
		return "", fmt.Errorf("Pi Agent Host is closed")
	}
	return native.Create(request)
}

func (h *piAgentHost) resumeBinding(request RuntimeBindingRequest, timeout time.Duration) error {
	h.mu.Lock()
	closed, native := h.closed, h.native
	h.mu.Unlock()
	if closed || native == nil {
		return fmt.Errorf("Pi Agent Host is closed")
	}
	return native.Resume(request, timeout)
}

func (h *piAgentHost) SetFailureHandler(handler func(error)) {
	h.mu.Lock()
	h.failure = handler
	h.mu.Unlock()
}

func (h *piAgentHost) fail(err error) {
	h.mu.Lock()
	handler, closed := h.failure, h.closed
	h.mu.Unlock()
	if !closed && handler != nil {
		handler(err)
	}
}

func (h *piAgentHost) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	h.failure = nil
	native, contract := h.native, h.contract
	h.mu.Unlock()
	contract.SetEventHandler(nil)
	if native != nil {
		native.SetRuntimeEventHandlers(nil, nil)
		native.Close()
	}
}
