package hub

func (h *Hub) Shutdown() {
	h.shutdownOnce.Do(func() {
		h.mu.Lock()
		h.stopping = true
		for agentID, rt := range h.runtimes {
			if rt.activeTurn != nil && !rt.activeTurn.finished {
				h.abortTurnApprovalsLocked(agentID, rt.activeTurn.turnID, rt, "CodexLoom shut down before the Approval was resolved")
				rt.activeTurn.finished = true
				if rt.activeTurn.stopWatchdog != nil {
					close(rt.activeTurn.stopWatchdog)
				}
			}
		}
		h.mu.Unlock()
		h.stopOnce.Do(func() {
			if h.stop != nil {
				close(h.stop)
			}
		})
		h.background.Wait()
		h.workers.Wait()
		h.mu.Lock()
		host := h.codexHost
		backends := make([]AgentRuntime, 0, len(h.runtimes))
		for _, rt := range h.runtimes {
			if backend := runtimeBackend(rt); backend != nil {
				backends = append(backends, backend)
			}
		}
		h.codexHost = nil
		h.remoteRuntime = nil
		ownedStore := h.st
		if !h.passive {
			h.persistRuntimeProjectionLocked()
			h.st = ownedStore.RetiredReadOnlyView()
		}
		h.mu.Unlock()
		if host != nil {
			host.client.Close()
		}
		for _, backend := range backends {
			backend.Close()
		}
		if h.writerOwnership != nil {
			h.writerOwnership.Release()
		}
	})
}
