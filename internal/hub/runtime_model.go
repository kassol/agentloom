package hub

import (
	"context"
	"strings"
	"time"
)

type RuntimeModel struct {
	Provider       string   `json:"provider"`
	ID             string   `json:"id"`
	ContextWindow  int      `json:"contextWindow,omitempty"`
	Reasoning      bool     `json:"reasoning"`
	ThinkingLevels []string `json:"thinkingLevels"`
	ImageInput     bool     `json:"imageInput"`
}

type RuntimeModelState struct {
	Current        RuntimeModel   `json:"current"`
	Models         []RuntimeModel `json:"models"`
	ThinkingLevel  string         `json:"thinkingLevel"`
	ThinkingLevels []string       `json:"thinkingLevels"`
}

type RuntimeModelSelection struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ThinkingLevel string `json:"thinkingLevel"`
}

func (h *Hub) GetRuntimeModels(key string) (RuntimeModelState, error) {
	h.mu.Lock()
	agent := h.resolveLocked(key)
	if agent == nil {
		h.mu.Unlock()
		return RuntimeModelState{}, errf(404, "agent not found: %s", key)
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
	capability, ok := rt.runtimeContract.(runtimeModelCatalogCapability)
	if !ok {
		return RuntimeModelState{}, unsupportedRuntimeCapability(agent, "native Runtime model catalog")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	state, err := capability.RuntimeModels(ctx, rt.binding)
	if err != nil {
		return RuntimeModelState{}, errf(500, "list Runtime models: %s", err)
	}
	return state, nil
}

func (h *Hub) SwitchRuntimeModel(key string, selection RuntimeModelSelection) (RuntimeModelState, error) {
	selection.Provider = strings.TrimSpace(selection.Provider)
	selection.Model = strings.TrimSpace(selection.Model)
	selection.ThinkingLevel = strings.TrimSpace(selection.ThinkingLevel)
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
	capability, ok := rt.runtimeContract.(runtimeModelCatalogCapability)
	if !ok {
		return RuntimeModelState{}, unsupportedRuntimeCapability(agent, "native Runtime model switching")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	state, err := capability.SwitchRuntimeModel(ctx, rt.binding, selection)
	if err != nil {
		return RuntimeModelState{}, errf(409, "switch Runtime model: %s", err)
	}
	h.mu.Lock()
	if agent = h.agents[agentID]; agent != nil {
		agent.ProviderID, agent.Model, agent.Effort, agent.UpdatedAt = state.Current.Provider, state.Current.ID, state.ThinkingLevel, now()
		if err := h.persistAgentsLocked(); err != nil {
			h.mu.Unlock()
			return RuntimeModelState{}, errf(500, "save Pi model selection: %s", err)
		}
	}
	h.mu.Unlock()
	h.refreshRuntimeCapabilitySnapshot(agentID, true)
	return state, nil
}
