package hub

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

type RuntimeModel = runtimecontract.Model
type RuntimeModelState = runtimecontract.ModelControlState
type RuntimeModelSelection = runtimecontract.ModelSelection

type runtimeModelSelectionCommitter interface{ CommitModelSelection() }

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
	capability, ok := rt.runtimeContract.(runtimecontract.ModelControlCapability)
	if !ok {
		return RuntimeModelState{}, unsupportedRuntimeCapability(agent, "native Runtime model catalog")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	state, failure := capability.InspectModelControl(ctx, rt.binding)
	if failure != nil {
		return RuntimeModelState{}, errf(500, "list Runtime models: %s", failure.Message)
	}
	if err := state.Validate(); err != nil {
		return RuntimeModelState{}, errf(500, "invalid Runtime model state: %s", err)
	}
	return state, nil
}

func (h *Hub) SwitchRuntimeModel(key string, selection RuntimeModelSelection) (RuntimeModelState, error) {
	selection.Provider = strings.TrimSpace(selection.Provider)
	selection.Model = strings.TrimSpace(selection.Model)
	selection.ThinkingLevel = strings.TrimSpace(selection.ThinkingLevel)
	if selection.Provider == "" {
		return RuntimeModelState{}, errf(400, "provider is required")
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
	if err := h.runtimeModelSwitchGuardLocked(agent.ID, h.runtimes[agent.ID]); err != nil {
		h.mu.Unlock()
		return RuntimeModelState{}, err
	}
	rt, err := h.getRuntimeLocked(agent)
	agentID := agent.ID
	expectedBinding := runtimeContractBinding(agent)
	expectedScope := h.runtimeCapabilityScopeLocked(agent)
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
	capability, ok := rt.runtimeContract.(runtimecontract.ModelControlCapability)
	if !ok {
		return RuntimeModelState{}, unsupportedRuntimeCapability(agent, "native Runtime model switching")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	preview, failure := capability.InspectModelControl(ctx, rt.binding)
	if failure != nil {
		return RuntimeModelState{}, errf(409, "inspect Runtime model selection: %s", failure.Message)
	}
	if err := preview.Validate(); err != nil {
		return RuntimeModelState{}, errf(500, "invalid Runtime model state: %s", err)
	}
	if err := preview.ValidateSelection(selection); err != nil {
		return RuntimeModelState{}, errf(400, "invalid Runtime model selection: %s", err)
	}
	rollback := runtimecontract.ModelSelection{Provider: preview.Current.Provider, Model: preview.Current.ID, ThinkingLevel: preview.ThinkingLevel}
	h.mu.Lock()
	agent = h.agents[agentID]
	if err := h.runtimeModelSwitchGuardLocked(agentID, rt); err != nil {
		h.mu.Unlock()
		return RuntimeModelState{}, err
	}
	stale := agent == nil || h.runtimes[agentID] != rt || runtimeContractBinding(agent) != expectedBinding || h.runtimeCapabilityScopeLocked(agent) != expectedScope || agent.Status == "running"
	h.mu.Unlock()
	if stale {
		return RuntimeModelState{}, errf(409, "Agent binding or model configuration changed while validating the Runtime selection; reopen the current selection and retry")
	}
	state, failure := capability.SelectModel(ctx, rt.binding, selection)
	if failure != nil {
		typed := &runtimeIndeterminateError{failure: failure}
		h.invalidateRuntimeEffectDomain(rt, typed)
		return RuntimeModelState{}, &HubError{Status: 500, Message: "switch Runtime model is indeterminate; the Runtime was fenced and closed", Cause: typed}
	}
	if err := state.Validate(); err != nil {
		return RuntimeModelState{}, h.runtimeModelPostSelectionFailure(rt, capability, rollback, 500, fmt.Errorf("invalid Runtime model state after selection: %w", err))
	}
	if state.Current.Provider != selection.Provider || state.Current.ID != selection.Model || (selection.ThinkingLevel != "" && state.ThinkingLevel != selection.ThinkingLevel) {
		return RuntimeModelState{}, h.runtimeModelPostSelectionFailure(rt, capability, rollback, 500, fmt.Errorf("Runtime did not activate the selected model and thinking level"))
	}
	h.mu.Lock()
	agent = h.agents[agentID]
	if err := h.runtimeModelSwitchGuardLocked(agentID, rt); err != nil {
		h.mu.Unlock()
		return RuntimeModelState{}, h.runtimeModelPostSelectionFailure(rt, capability, rollback, 409, err)
	}
	stale = agent == nil || h.runtimes[agentID] != rt || runtimeContractBinding(agent) != expectedBinding || h.runtimeCapabilityScopeLocked(agent) != expectedScope || agent.Status == "running"
	if stale {
		h.mu.Unlock()
		return RuntimeModelState{}, h.runtimeModelPostSelectionFailure(rt, capability, rollback, 409, fmt.Errorf("Agent binding or model configuration changed while applying the Runtime selection"))
	}
	previousProvider, previousModel, previousEffort, previousUpdatedAt := agent.ProviderID, agent.Model, agent.Effort, agent.UpdatedAt
	previousHistoryLen := len(agent.ProviderHistory)
	providerChanged := publicProviderID(previousProvider) != publicProviderID(state.Current.Provider)
	if providerChanged {
		agent.ProviderHistory = append(agent.ProviderHistory, ProviderBindingChange{
			PreviousProviderID: previousProvider, PreviousModel: previousModel,
			ProviderID: state.Current.Provider, Model: state.Current.ID, SwitchedAt: now(),
		})
	}
	agent.ProviderID, agent.Model, agent.Effort, agent.UpdatedAt = state.Current.Provider, state.Current.ID, state.ThinkingLevel, now()
	if err := h.persistAgentsLocked(); err != nil {
		agent.ProviderID, agent.Model, agent.Effort, agent.UpdatedAt = previousProvider, previousModel, previousEffort, previousUpdatedAt
		agent.ProviderHistory = agent.ProviderHistory[:previousHistoryLen]
		h.mu.Unlock()
		return RuntimeModelState{}, h.runtimeModelPostSelectionFailure(rt, capability, rollback, 500, fmt.Errorf("save Runtime model selection: %w", err))
	}
	h.mu.Unlock()
	if committer, ok := rt.runtimeContract.(runtimeModelSelectionCommitter); ok {
		committer.CommitModelSelection()
	}
	if providerChanged {
		clearAgentUsageCache()
	}
	h.refreshRuntimeCapabilitySnapshot(agentID, true)
	return state, nil
}

func (h *Hub) runtimeModelSwitchGuardLocked(agentID string, rt *runtime) error {
	if goal := h.goals[agentID]; goal != nil && goal.Status == GoalStatusActive {
		return errf(409, "pause the active Goal before switching the Runtime model")
	}
	if rt != nil && len(rt.approvals) > 0 {
		return errf(409, "resolve the pending approval before switching the Runtime model")
	}
	return nil
}

func (h *Hub) runtimeModelPostSelectionFailure(rt *runtime, capability runtimecontract.ModelControlCapability, previous runtimecontract.ModelSelection, status int, cause error) error {
	rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer rollbackCancel()
	restored, failure := capability.SelectModel(rollbackCtx, rt.binding, previous)
	var rollbackErr error
	if failure != nil {
		rollbackErr = fmt.Errorf("%s", failure.Message)
	} else if err := restored.Validate(); err != nil {
		rollbackErr = fmt.Errorf("invalid restored Runtime model state: %w", err)
	} else if restored.Current.Provider != previous.Provider || restored.Current.ID != previous.Model || restored.ThinkingLevel != previous.ThinkingLevel {
		rollbackErr = fmt.Errorf("Runtime restored %s/%s thinking %q instead of %s/%s thinking %q", restored.Current.Provider, restored.Current.ID, restored.ThinkingLevel, previous.Provider, previous.Model, previous.ThinkingLevel)
	}
	if rollbackErr == nil {
		return errf(status, "%s; the exact previous selection was restored", cause)
	}
	typed := &runtimeIndeterminateError{failure: &runtimecontract.Failure{
		Code: "model_selection_indeterminate", Phase: runtimecontract.FailurePhaseModelControl,
		Message: fmt.Sprintf("Runtime model selection is indeterminate after %v; rollback failed: %v", cause, rollbackErr), Cause: rollbackErr,
	}}
	h.invalidateRuntimeEffectDomain(rt, typed)
	return &HubError{Status: 500, Message: typed.Error() + "; the Runtime was fenced and closed", Cause: typed}
}
