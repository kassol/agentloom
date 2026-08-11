package hub

import (
	"context"
	"log"
)

func (h *Hub) Shutdown() {
	h.shutdownOnce.Do(func() {
		h.mu.Lock()
		h.stopping = true
		snapshots := map[string]Agent{}
		shutdownTurns := map[string]*turnState{}
		for agentID, rt := range h.runtimes {
			if rt.activeTurn != nil && !rt.activeTurn.finished {
				turn := rt.activeTurn
				turn.finished = true
				if turn.stopWatchdog != nil {
					close(turn.stopWatchdog)
				}
				rt.activeTurn = nil
				if meta := h.agents[agentID]; meta != nil {
					previous := *meta
					if meta.LastTurn != nil {
						last := *meta.LastTurn
						previous.LastTurn = &last
					}
					previous.TurnRecoveryMarkers = make(map[string]TurnRecoveryMarker, len(meta.TurnRecoveryMarkers))
					for predecessorTurnID, marker := range meta.TurnRecoveryMarkers {
						previous.TurnRecoveryMarkers[predecessorTurnID] = marker
					}
					snapshots[agentID] = previous
					shutdownTurns[agentID] = turn
					meta.Status = "interrupted"
					meta.CurrentTask = ""
					meta.CurrentTurnID = ""
					meta.LastError = "interrupted: CodexLoom shut down before the Turn outcome was confirmed"
					meta.LastTurn = &TurnSummary{TurnID: turn.turnID, Task: turn.task, Status: "interrupted", CompletedAt: now()}
					if meta.TurnRecoveryMarkers == nil {
						meta.TurnRecoveryMarkers = map[string]TurnRecoveryMarker{}
					}
					stamp := now()
					meta.TurnRecoveryMarkers[turn.turnID] = TurnRecoveryMarker{
						PredecessorTurnID: turn.turnID, NativeTurnID: turn.nativeTurnID,
						RuntimeKind: meta.RuntimeBinding.Kind, Cause: "hub_shutdown", State: TurnRecoveryObserved,
						Summary: "CodexLoom shut down before the Turn outcome was confirmed", CreatedAt: stamp, UpdatedAt: stamp,
					}
					meta.UpdatedAt = stamp
				}
			}
		}
		checkpointed := true
		if !h.passive && len(shutdownTurns) > 0 {
			if err := h.persistAgentsLocked(); err != nil {
				checkpointed = false
				for agentID, previous := range snapshots {
					if meta := h.agents[agentID]; meta != nil {
						*meta = previous
					}
				}
				log.Printf("[codex-loom] persist shutdown recovery checkpoint: %v", err)
			}
		}
		if checkpointed {
			for agentID, turn := range shutdownTurns {
				if rt := h.runtimes[agentID]; rt != nil {
					h.abortTurnApprovalsLocked(agentID, turn.turnID, rt, "CodexLoom shut down before the Approval was resolved")
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
		driver := h.codexHostDriver
		piDriver := h.piHostDriver
		backends := make([]AgentRuntime, 0, len(h.runtimes))
		for _, rt := range h.runtimes {
			if backend := runtimeBackend(rt); backend != nil && rt.agentHost == nil {
				backends = append(backends, backend)
			}
		}
		if driver == nil {
			h.codexHost = nil
		}
		h.remoteRuntime = nil
		ownedStore := h.st
		if !h.passive {
			h.persistRuntimeProjectionLocked()
			h.st = ownedStore.RetiredReadOnlyView()
		}
		h.mu.Unlock()
		if driver != nil {
			if err := driver.Shutdown(context.Background()); err != nil {
				log.Printf("[codex-loom] shut down Codex Runtime Host Driver: %v", err)
			}
		} else if host != nil {
			host.close()
		}
		if piDriver != nil {
			if err := piDriver.Shutdown(context.Background()); err != nil {
				log.Printf("[codex-loom] shut down Pi Runtime Host Driver: %v", err)
			}
		}
		for _, backend := range backends {
			backend.Close()
		}
		if h.writerOwnership != nil {
			h.writerOwnership.Release()
		}
	})
}
