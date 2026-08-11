package hub

import (
	"context"
	"strings"
	"time"
)

// ThreadCompactResult is the product-facing acknowledgement returned after a
// manual Codex compaction request has been accepted by the shared app-server.
type ThreadCompactResult struct {
	AgentID   string `json:"agentId"`
	AgentName string `json:"agentName"`
	ThreadID  string `json:"threadId"`
	Started   bool   `json:"started"`
}

// CompactAgentThread asks Codex to compact one Agent's primary Thread. The
// operation is intentionally manual and bounded: an active Turn or active Goal
// owns the next model request, so compaction is rejected until that work is
// paused or completed. Existing epoch coverage will re-deliver Loom durable
// context on the next Turn after Codex writes the compaction marker.
func (h *Hub) CompactAgentThread(key string) (ThreadCompactResult, error) {
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		return ThreadCompactResult{}, errf(503, "CodexLoom is shutting down")
	}
	agent := h.resolveLocked(key)
	if agent == nil {
		h.mu.Unlock()
		return ThreadCompactResult{}, errf(404, "agent not found: %s", key)
	}
	if agent.Status == "running" {
		h.mu.Unlock()
		return ThreadCompactResult{}, errf(409, "agent %q is running; stop the active Turn before compacting", agent.Name)
	}
	if rt := h.runtimes[agent.ID]; rt != nil && rt.activeTurn != nil && !rt.activeTurn.finished {
		h.mu.Unlock()
		return ThreadCompactResult{}, errf(409, "agent %q has an active Turn; stop it before compacting", agent.Name)
	}
	if rt := h.runtimes[agent.ID]; rt != nil && len(rt.approvals) > 0 {
		h.mu.Unlock()
		return ThreadCompactResult{}, errf(409, "agent %q has a pending approval; resolve it before compacting", agent.Name)
	}
	if goal := h.goals[agent.ID]; goal != nil && goal.ClearedAt == 0 && goal.Status == GoalStatusActive {
		h.mu.Unlock()
		return ThreadCompactResult{}, errf(409, "agent %q has an active Goal; pause it before compacting", agent.Name)
	}
	rt, err := h.getRuntimeLocked(agent)
	if err != nil {
		h.mu.Unlock()
		return ThreadCompactResult{}, err
	}
	agentID, agentName := agent.ID, agent.Name
	loomThreadID := agent.ThreadID
	threadID := strings.TrimSpace(agent.RuntimeBinding.NativeRef)
	h.mu.Unlock()

	if threadID == "" {
		return ThreadCompactResult{}, errf(409, "agent has no Codex Thread binding")
	}
	rt.startMu.Lock()
	defer rt.startMu.Unlock()
	if err := h.verifyRuntimeThreadControl(agentID, rt); err != nil {
		return ThreadCompactResult{}, err
	}
	if err := waitReady(rt); err != nil {
		return ThreadCompactResult{}, errf(500, "Runtime not ready: %s", err)
	}
	if err := h.resumeAgentThread(agentID, rt); err != nil {
		return ThreadCompactResult{}, errf(500, "resume Runtime binding before compaction: %s", err)
	}
	capability, ok := rt.runtimeContract.(runtimeCompactionCapability)
	if !ok {
		return ThreadCompactResult{}, unsupportedRuntimeCapability(agent, "manual compaction")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := capability.CompactRuntimeBinding(ctx, rt.binding); err != nil {
		return ThreadCompactResult{}, errf(500, "start Runtime compaction: %s", err)
	}

	h.mu.Lock()
	if agent := h.agents[agentID]; agent != nil {
		h.emitLocked(agentID, "loom/agent-compacted", map[string]any{
			"agentId": agentID, "agentName": agentName, "threadId": loomThreadID,
		})
	}
	h.mu.Unlock()
	return ThreadCompactResult{AgentID: agentID, AgentName: agentName, ThreadID: loomThreadID, Started: true}, nil
}
