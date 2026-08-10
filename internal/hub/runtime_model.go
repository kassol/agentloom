package hub

import (
	"strings"
	"time"
)

type RuntimeModel struct {
	Provider      string `json:"provider"`
	ID            string `json:"id"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	Reasoning     bool   `json:"reasoning"`
}

type RuntimeModelState struct {
	Current RuntimeModel   `json:"current"`
	Models  []RuntimeModel `json:"models"`
}

type RuntimeModelSelection struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func (h *Hub) GetRuntimeModels(key string) (RuntimeModelState, error) {
	h.mu.Lock()
	agent := h.resolveLocked(key)
	if agent == nil {
		h.mu.Unlock()
		return RuntimeModelState{}, errf(404, "agent not found: %s", key)
	}
	if agent.RuntimeBinding.Kind != "pi" {
		h.mu.Unlock()
		return RuntimeModelState{}, unsupportedRuntimeCapability(agent, "native Runtime model catalog")
	}
	rt, err := h.getRuntimeLocked(agent)
	h.mu.Unlock()
	if err != nil {
		return RuntimeModelState{}, err
	}
	rt.startMu.Lock()
	defer rt.startMu.Unlock()
	if err := waitReady(rt); err != nil {
		return RuntimeModelState{}, errf(500, "Runtime not ready: %s", err)
	}
	backend, ok := runtimeBackend(rt).(*piAgentRuntime)
	if !ok {
		return RuntimeModelState{}, errf(500, "Pi Runtime model control is unavailable")
	}
	state, err := backend.models(15 * time.Second)
	if err != nil {
		return RuntimeModelState{}, errf(500, "list Pi models: %s", err)
	}
	return state, nil
}

func (h *Hub) SwitchRuntimeModel(key string, selection RuntimeModelSelection) (RuntimeModelState, error) {
	selection.Provider = strings.TrimSpace(selection.Provider)
	selection.Model = strings.TrimSpace(selection.Model)
	if selection.Provider == "" || selection.Model == "" {
		return RuntimeModelState{}, errf(400, "provider and model are required")
	}
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		return RuntimeModelState{}, errf(503, "CodexLoom is shutting down")
	}
	agent := h.resolveLocked(key)
	if agent == nil {
		h.mu.Unlock()
		return RuntimeModelState{}, errf(404, "agent not found: %s", key)
	}
	if agent.RuntimeBinding.Kind != "pi" {
		h.mu.Unlock()
		return RuntimeModelState{}, unsupportedRuntimeCapability(agent, "native Runtime model switching")
	}
	if agent.Status == "running" {
		h.mu.Unlock()
		return RuntimeModelState{}, errf(409, "agent %q is running; switch models between Turns", agent.Name)
	}
	rt, err := h.getRuntimeLocked(agent)
	agentID := agent.ID
	h.mu.Unlock()
	if err != nil {
		return RuntimeModelState{}, err
	}
	rt.startMu.Lock()
	defer rt.startMu.Unlock()
	if err := waitReady(rt); err != nil {
		return RuntimeModelState{}, errf(500, "Runtime not ready: %s", err)
	}
	h.mu.Lock()
	agent = h.agents[agentID]
	busy := agent == nil || h.runtimes[agentID] != rt || agent.Status == "running" || (rt.activeTurn != nil && !rt.activeTurn.finished)
	h.mu.Unlock()
	if busy {
		return RuntimeModelState{}, errf(409, "agent is running; switch models between Turns")
	}
	backend, ok := runtimeBackend(rt).(*piAgentRuntime)
	if !ok {
		return RuntimeModelState{}, errf(500, "Pi Runtime model control is unavailable")
	}
	state, err := backend.switchModel(selection, 15*time.Second)
	if err != nil {
		return RuntimeModelState{}, errf(409, "switch Pi model: %s", err)
	}
	h.mu.Lock()
	if agent = h.agents[agentID]; agent != nil {
		agent.ProviderID, agent.Model, agent.UpdatedAt = state.Current.Provider, state.Current.ID, now()
		if err := h.persistAgentsLocked(); err != nil {
			h.mu.Unlock()
			return RuntimeModelState{}, errf(500, "save Pi model selection: %s", err)
		}
	}
	h.mu.Unlock()
	return state, nil
}
