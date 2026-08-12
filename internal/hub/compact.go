package hub

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

const contextMaintenanceStarted = "started"

// ContextMaintenanceOperation is the latest durable Owner-requested context
// maintenance result. Runtime-native references and summaries stay private.
type ContextMaintenanceOperation struct {
	ID               string `json:"id"`
	AgentID          string `json:"agentId"`
	ThreadID         string `json:"threadId"`
	Origin           string `json:"origin"`
	State            string `json:"state"`
	StartedAt        string `json:"startedAt"`
	CompletedAt      string `json:"completedAt,omitempty"`
	Error            string `json:"error,omitempty"`
	BaselineRevision string `json:"baselineRevision"`
	BindingRevision  string `json:"bindingRevision"`
}

func (operation ContextMaintenanceOperation) Validate(agent *Agent) error {
	if operation.ID == "" || operation.AgentID == "" || operation.ThreadID == "" || operation.Origin != "owner" || operation.StartedAt == "" || operation.BaselineRevision == "" || operation.BindingRevision == "" {
		return fmt.Errorf("context maintenance operation is incomplete")
	}
	if agent == nil || operation.AgentID != agent.ID || operation.ThreadID != agent.ThreadID {
		return fmt.Errorf("context maintenance operation does not belong to its Agent")
	}
	switch runtimecontract.LifecycleState(operation.State) {
	case runtimecontract.LifecycleCompleted, runtimecontract.LifecycleInterrupted:
		if operation.CompletedAt == "" || operation.Error != "" {
			return fmt.Errorf("context maintenance terminal result is invalid")
		}
	case runtimecontract.LifecycleFailed, runtimecontract.LifecycleIndeterminate:
		if operation.CompletedAt == "" || operation.Error == "" {
			return fmt.Errorf("context maintenance failure result is invalid")
		}
	default:
		if operation.State != contextMaintenanceStarted || operation.CompletedAt != "" || operation.Error != "" {
			return fmt.Errorf("unknown context maintenance state %q", operation.State)
		}
	}
	return nil
}

type ThreadCompactResult struct {
	AgentID   string                      `json:"agentId"`
	AgentName string                      `json:"agentName"`
	ThreadID  string                      `json:"threadId"`
	Started   bool                        `json:"started"`
	Operation ContextMaintenanceOperation `json:"operation"`
}

// CompactAgentThread durably reserves one Owner operation before asking the
// selected Runtime to perform its native context maintenance asynchronously.
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
	if err := h.contextMaintenanceGuardLocked(agent); err != nil {
		h.mu.Unlock()
		return ThreadCompactResult{}, err
	}
	rt, err := h.getRuntimeLocked(agent)
	if err != nil {
		h.mu.Unlock()
		return ThreadCompactResult{}, err
	}
	agentID, agentName, loomThreadID := agent.ID, agent.Name, agent.ThreadID
	binding := runtimeContractBinding(agent)
	h.mu.Unlock()

	rt.startMu.Lock()
	defer rt.startMu.Unlock()
	if err := h.verifyRuntimeThreadControl(agentID, rt); err != nil {
		return ThreadCompactResult{}, err
	}
	if err := waitReady(rt); err != nil {
		return ThreadCompactResult{}, errf(500, "Runtime not ready: %s", err)
	}
	if err := h.resumeAgentThread(agentID, rt); err != nil {
		return ThreadCompactResult{}, errf(500, "resume Runtime binding before context maintenance: %s", err)
	}
	capability, ok := rt.runtimeContract.(runtimecontract.ContextMaintenanceCapability)
	if !ok {
		return ThreadCompactResult{}, unsupportedRuntimeCapability(agent, "context maintenance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	inspection, failure := capability.InspectContextMaintenance(ctx, binding)
	cancel()
	if failure != nil {
		return ThreadCompactResult{}, errf(500, "inspect Runtime context maintenance: %s", publicRuntimeFailureMessage(agent, "", failure.Message))
	}
	if err := inspection.Validate(); err != nil {
		return ThreadCompactResult{}, errf(500, "invalid Runtime context maintenance inspection: %s", err)
	}

	h.mu.Lock()
	agent = h.agents[agentID]
	if h.stopping {
		h.mu.Unlock()
		return ThreadCompactResult{}, errf(503, "CodexLoom is shutting down")
	}
	if agent == nil || h.runtimes[agentID] != rt || runtimeContractBinding(agent) != binding {
		h.mu.Unlock()
		return ThreadCompactResult{}, errf(409, "Agent Runtime binding changed while context maintenance was inspected; retry")
	}
	if err := h.contextMaintenanceGuardLocked(agent); err != nil {
		h.mu.Unlock()
		return ThreadCompactResult{}, err
	}
	operation := ContextMaintenanceOperation{
		ID: newIntegrationID("cmop"), AgentID: agentID, ThreadID: loomThreadID, Origin: "owner",
		State: contextMaintenanceStarted, StartedAt: now(),
		BaselineRevision: contextMaintenanceRevisionHash(inspection.Revision),
		BindingRevision:  h.runtimeCapabilityScopeLocked(agent).BindingRevision,
	}
	previous := agent.ContextMaintenance
	previousUpdatedAt := agent.UpdatedAt
	agent.ContextMaintenance = &operation
	agent.UpdatedAt = operation.StartedAt
	if err := h.persistAgentsLocked(); err != nil {
		agent.ContextMaintenance = previous
		agent.UpdatedAt = previousUpdatedAt
		h.mu.Unlock()
		return ThreadCompactResult{}, errf(500, "save context maintenance operation: %s", err)
	}
	h.emitLocked(agentID, "loom/context-maintenance", operation)
	if !h.startWorkerLocked(func() { h.runContextMaintenance(agentID, operation.ID, rt, binding, capability) }) {
		h.mu.Unlock()
		return ThreadCompactResult{}, errf(503, "CodexLoom is shutting down")
	}
	h.mu.Unlock()
	return ThreadCompactResult{AgentID: agentID, AgentName: agentName, ThreadID: loomThreadID, Started: true, Operation: operation}, nil
}

func (h *Hub) contextMaintenanceGuardLocked(agent *Agent) error {
	if agent.ContextMaintenance != nil && agent.ContextMaintenance.State == contextMaintenanceStarted {
		return errf(409, "agent %q already has context maintenance in progress", agent.Name)
	}
	if agent.Status == "running" {
		return errf(409, "agent %q is running; stop the active Turn before context maintenance", agent.Name)
	}
	if rt := h.runtimes[agent.ID]; rt != nil && rt.activeTurn != nil && !rt.activeTurn.finished {
		return errf(409, "agent %q has an active Turn; stop it before context maintenance", agent.Name)
	}
	if rt := h.runtimes[agent.ID]; rt != nil && len(rt.approvals) > 0 {
		return errf(409, "agent %q has a pending approval; resolve it before context maintenance", agent.Name)
	}
	if goal := h.goals[agent.ID]; goal != nil && goal.ClearedAt == 0 && goal.Status == GoalStatusActive {
		return errf(409, "agent %q has an active Goal; pause it before context maintenance", agent.Name)
	}
	return nil
}

func (h *Hub) runContextMaintenance(agentID, operationID string, rt *runtime, binding runtimecontract.Binding, capability runtimecontract.ContextMaintenanceCapability) {
	rt.startMu.Lock()
	defer rt.startMu.Unlock()
	h.mu.Lock()
	agent := h.agents[agentID]
	current := agent != nil && h.runtimes[agentID] == rt && runtimeContractBinding(agent) == binding &&
		agent.ContextMaintenance != nil && agent.ContextMaintenance.ID == operationID && agent.ContextMaintenance.State == contextMaintenanceStarted
	h.mu.Unlock()
	if !current {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	outcome := capability.MaintainContext(ctx, binding)
	cancel()
	if err := outcome.Validate(); err != nil || outcome.State == runtimecontract.LifecycleAccepted || outcome.State == runtimecontract.LifecycleRejected {
		outcome = runtimecontract.Outcome{State: runtimecontract.LifecycleIndeterminate, Failure: &runtimecontract.Failure{
			Code: "invalid_context_maintenance_outcome", Phase: runtimecontract.FailurePhaseContextMaintenance,
			Message: fmt.Sprintf("Runtime context maintenance returned no trustworthy terminal outcome: %v", err),
		}}
	}
	h.finishContextMaintenance(agentID, operationID, outcome)
	if outcome.State == runtimecontract.LifecycleIndeterminate {
		h.invalidateRuntimeEffectDomain(rt, &runtimeIndeterminateError{failure: outcome.Failure})
	}
}

func (h *Hub) finishContextMaintenance(agentID, operationID string, outcome runtimecontract.Outcome) {
	h.mu.Lock()
	defer h.mu.Unlock()
	agent := h.agents[agentID]
	if agent == nil || agent.ContextMaintenance == nil || agent.ContextMaintenance.ID != operationID || agent.ContextMaintenance.State != contextMaintenanceStarted {
		return
	}
	previous := *agent.ContextMaintenance
	previousUpdatedAt := agent.UpdatedAt
	next := previous
	next.State = string(outcome.State)
	next.CompletedAt = now()
	if outcome.Failure != nil {
		next.Error = contextMaintenancePublicError(outcome.State)
	}
	agent.ContextMaintenance = &next
	agent.UpdatedAt = next.CompletedAt
	if err := h.persistAgentsLocked(); err != nil {
		agent.ContextMaintenance = &previous
		agent.UpdatedAt = previousUpdatedAt
		log.Printf("[codex-loom] persist context maintenance terminal state: %v", err)
		return
	}
	h.emitLocked(agentID, "loom/context-maintenance", next)
}

func contextMaintenancePublicError(state runtimecontract.LifecycleState) string {
	switch state {
	case runtimecontract.LifecycleFailed:
		return "Runtime context maintenance failed"
	case runtimecontract.LifecycleIndeterminate:
		return "Runtime context maintenance completion is indeterminate"
	default:
		return "Runtime context maintenance did not complete"
	}
}

func contextMaintenanceRevisionHash(revision string) string {
	digest := sha256Hex([]byte(revision))
	return "maintenance:" + digest[:16]
}

func (h *Hub) contextMaintenanceStartupAgentIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := []string{}
	for id, agent := range h.agents {
		if agent != nil && agent.ContextMaintenance != nil && agent.ContextMaintenance.State == contextMaintenanceStarted {
			ids = append(ids, id)
		}
	}
	return ids
}

// reconcileContextMaintenanceAfterOpen never repeats the mutating command. A
// changed passive revision proves completion; every other crash window stays
// indeterminate and requires a fresh explicit Owner decision.
func (h *Hub) reconcileContextMaintenanceAfterOpen(agentID string) {
	h.mu.Lock()
	agent := h.agents[agentID]
	if agent == nil || agent.ContextMaintenance == nil || agent.ContextMaintenance.State != contextMaintenanceStarted {
		h.mu.Unlock()
		return
	}
	operationID := agent.ContextMaintenance.ID
	baseline := agent.ContextMaintenance.BaselineRevision
	binding := runtimeContractBinding(agent)
	driver, err := h.runtimeHostDriverLocked(binding.RuntimeKind)
	provider, ok := driver.(runtimeHistoryContractProvider)
	h.mu.Unlock()
	if err != nil || !ok {
		h.finishContextMaintenance(agentID, operationID, indeterminateContextMaintenanceOutcome("Runtime context maintenance could not be reconciled after restart"))
		return
	}
	contract := provider.HistoryContract(AgentHostRequest{AgentID: agentID})
	capability, ok := contract.(runtimecontract.ContextMaintenanceCapability)
	if !ok {
		h.finishContextMaintenance(agentID, operationID, indeterminateContextMaintenanceOutcome("Runtime context maintenance evidence is unavailable after restart"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	inspection, failure := capability.InspectContextMaintenance(ctx, binding)
	cancel()
	if failure != nil || inspection.Validate() != nil {
		h.finishContextMaintenance(agentID, operationID, indeterminateContextMaintenanceOutcome("Runtime context maintenance evidence is unavailable after restart"))
		return
	}
	if contextMaintenanceRevisionHash(inspection.Revision) != baseline {
		h.finishContextMaintenance(agentID, operationID, runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted})
		return
	}
	h.finishContextMaintenance(agentID, operationID, indeterminateContextMaintenanceOutcome("Runtime context maintenance completion is indeterminate after restart"))
}

func indeterminateContextMaintenanceOutcome(message string) runtimecontract.Outcome {
	return runtimecontract.Outcome{State: runtimecontract.LifecycleIndeterminate, Failure: &runtimecontract.Failure{
		Code: "context_maintenance_indeterminate", Phase: runtimecontract.FailurePhaseContextMaintenance, Message: message,
	}}
}
