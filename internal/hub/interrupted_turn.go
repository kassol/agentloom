package hub

import (
	"fmt"
	"strings"
	"time"
)

const restartInterruptedError = "interrupted: CodexLoom restarted while task was running"

// InterruptedTurnAction is the result of continuing one restart-interrupted
// Agent. Mode describes whether Loom resumed the Owner Thread directly or
// returned the durable work item to its existing handling queue.
type InterruptedTurnAction struct {
	Mode     string         `json:"mode"`
	Agent    AgentView      `json:"agent"`
	Dispatch *SendResult    `json:"dispatch,omitempty"`
	Source   *TurnReference `json:"source,omitempty"`
}

// reconcileInterruptedTurn creates the durable recovery projection for a
// running Agent left behind by a dead Loom process. The Agent registry is the
// crash journal; the rollout supplements an ID, task, and last activity when
// the final runtime checkpoint raced with process loss.
func reconcileInterruptedTurn(meta *Agent) (*TurnSummary, bool) {
	summary := &TurnSummary{
		TurnID: meta.CurrentTurnID, Task: displayUserTask(meta.CurrentTask),
		Status: "interrupted", CompletedAt: now(),
	}
	// Startup records only Loom's durable observation. The recovery worker
	// acquires the per-Agent v2 Contract and inspects optional native evidence
	// after this checkpoint, so registry restoration never routes by Runtime
	// kind or consults a process-global v1 backend.
	return summary, true
}

// ContinueInterruptedTurn resumes the existing causal work without replaying
// the original Owner prompt. Internal and external work reuse their durable
// Message/Inbox identity; direct Owner work starts a continuation in the same
// Codex Thread with a recovery control envelope.
func (h *Hub) ContinueInterruptedTurn(key string, inactivity time.Duration) (InterruptedTurnAction, error) {
	h.mu.Lock()
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return InterruptedTurnAction{}, errf(404, "agent not found: %s", key)
	}
	if meta.Status != "interrupted" || meta.LastTurn == nil || meta.LastTurn.Status != "interrupted" {
		h.mu.Unlock()
		return InterruptedTurnAction{}, errf(409, "agent %s has no restart-interrupted Turn", meta.Name)
	}
	if h.turnRecoveryInFlight[recoveryKey(meta.ID, meta.LastTurn.TurnID)] {
		h.mu.Unlock()
		return InterruptedTurnAction{}, errf(409, "automatic recovery is already running for interrupted Turn %s", meta.LastTurn.TurnID)
	}
	if marker, exists := meta.TurnRecoveryMarkers[meta.LastTurn.TurnID]; exists && marker.State != TurnRecoveryCompleted {
		h.mu.Unlock()
		return InterruptedTurnAction{}, errf(409, "automatic recovery is already recorded for interrupted Turn %s", meta.LastTurn.TurnID)
	}
	agentID := meta.ID
	interrupted := *meta.LastTurn
	source, _ := h.turnReferenceLocked(agentID, interrupted.TurnID)
	if source == nil {
		source = h.topicTurnReferenceLocked(agentID, interrupted.TurnID)
	}
	h.mu.Unlock()

	action := InterruptedTurnAction{Mode: "thread", Source: source}
	switch {
	case source != nil && source.Kind == "external":
		if _, err := h.RetryInboxItem(source.ID); err != nil {
			return InterruptedTurnAction{}, err
		}
		action.Mode = "inbox"
		h.clearInterruptedAgent(agentID, interrupted.TurnID)
		h.startWorker(func() { h.deliverNextInboxForAgent(agentID) })
	case source != nil && (source.Kind == "internal" || source.Kind == "trigger" || source.Kind == "schedule"):
		if _, err := h.continueInterruptedAgentMessage(source.ID, agentID, interrupted.TurnID); err != nil {
			return InterruptedTurnAction{}, err
		}
		action.Mode = "message"
		h.clearInterruptedAgent(agentID, interrupted.TurnID)
	default:
		topicID := ""
		if source != nil {
			topicID = source.TopicID
		}
		prompt := interruptedTurnRecoveryPrompt(interrupted, source)
		displayTask := "Continue interrupted work"
		if task := strings.TrimSpace(interrupted.Task); task != "" {
			displayTask += ": " + task
		}
		sourceContext := turnContextSource{
			Origin: "platform_runtime", Trust: "loom_managed", Authority: "recovery_control",
			Kind: "restart_recovery", RefID: interrupted.TurnID, TopicID: topicID,
		}
		result, err := h.sendTaskWithContext(agentID, prompt, nil, inactivity, "", "", "", topicID, displayTask, sourceContext)
		if err != nil {
			return InterruptedTurnAction{}, err
		}
		action.Dispatch = &result
	}

	view, err := h.GetAgent(agentID)
	if err != nil {
		return InterruptedTurnAction{}, err
	}
	action.Agent = view
	return action, nil
}

// DismissInterruptedTurn clears only the Agent-list attention state. The
// interrupted Turn and any linked held Inbox item remain in history.
func (h *Hub) DismissInterruptedTurn(key string) (AgentView, error) {
	h.mu.Lock()
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return AgentView{}, errf(404, "agent not found: %s", key)
	}
	if meta.Status != "interrupted" || meta.LastTurn == nil || meta.LastTurn.Status != "interrupted" {
		h.mu.Unlock()
		return AgentView{}, errf(409, "agent %s has no restart-interrupted Turn", meta.Name)
	}
	if h.turnRecoveryInFlight[recoveryKey(meta.ID, meta.LastTurn.TurnID)] {
		h.mu.Unlock()
		return AgentView{}, errf(409, "automatic recovery is already running for interrupted Turn %s", meta.LastTurn.TurnID)
	}
	if marker, exists := meta.TurnRecoveryMarkers[meta.LastTurn.TurnID]; exists && marker.State != TurnRecoveryCompleted {
		h.mu.Unlock()
		return AgentView{}, errf(409, "automatic recovery is already recorded for interrupted Turn %s", meta.LastTurn.TurnID)
	}
	turnID := meta.LastTurn.TurnID
	if err := h.clearInterruptedAgentLocked(meta, turnID); err != nil {
		h.mu.Unlock()
		return AgentView{}, err
	}
	view := h.viewLocked(meta)
	h.mu.Unlock()
	return view, nil
}

func (h *Hub) clearInterruptedAgent(agentID, turnID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta := h.agents[agentID]
	if meta == nil || meta.Status != "interrupted" {
		return
	}
	if err := h.clearInterruptedAgentLocked(meta, turnID); err != nil {
		// The queued work is durable even if this convenience projection could
		// not be cleared. Keep the warning visible and let normal reconciliation
		// repair it instead of rolling back the handling request.
		return
	}
}

func (h *Hub) clearInterruptedAgentLocked(meta *Agent, turnID string) error {
	if meta.LastTurn == nil || meta.LastTurn.Status != "interrupted" || (turnID != "" && meta.LastTurn.TurnID != turnID) {
		return errf(409, "interrupted Turn changed; refresh the Agent")
	}
	previous := *meta
	meta.Status = "idle"
	meta.CurrentTask = ""
	meta.CurrentTurnID = ""
	meta.LastError = ""
	meta.UpdatedAt = now()
	if err := h.persistAgentsLocked(); err != nil {
		*meta = previous
		return errf(500, "persist interrupted Turn dismissal: %s", err)
	}
	h.emitStatusLocked(meta, "idle")
	return nil
}

func (h *Hub) topicTurnReferenceLocked(agentID, turnID string) *TurnReference {
	if turnID == "" {
		return nil
	}
	for _, topic := range h.topics {
		if topic == nil || !topicHasAgent(topic, agentID, agentID) {
			continue
		}
		for i := len(topic.Events) - 1; i >= 0; i-- {
			event := topic.Events[i]
			if event.Ref != nil && event.Ref.Type == "turn" && event.Ref.ID == turnID {
				return &TurnReference{Kind: "topic", ID: turnID, TopicID: topic.ID}
			}
		}
	}
	return nil
}

func interruptedTurnRecoveryPrompt(turn TurnSummary, source *TurnReference) string {
	sourceKind := "owner"
	if source != nil && source.Kind != "" {
		sourceKind = source.Kind
	}
	return fmt.Sprintf(`<loom_turn_recovery version="1" previous_turn_id="%s" source="%s" interrupted_at="%s">
  <previous_task>%s</previous_task>
  <instruction>Continue the interrupted work in this same Agent Thread. First inspect the preceding Thread context and determine what already completed. Re-check current external facts before relying on pre-restart state. Do not repeat external writes or other side effects unless their current outcome and idempotency are verified. Continue only the unfinished work, then report the resulting state normally.</instruction>
</loom_turn_recovery>`, xmlEscape(turn.TurnID), xmlEscape(sourceKind), xmlEscape(turn.CompletedAt), xmlEscape(turn.Task))
}

// continueInterruptedAgentMessage begins another handling attempt for the same
// durable Message. It also supports response=none notifications, which the
// public RetryAgentMessage command intentionally excludes.
func (h *Hub) continueInterruptedAgentMessage(messageID, agentID, turnID string) (AgentMessage, error) {
	h.mu.Lock()
	message := h.comms[messageID]
	if message == nil {
		h.mu.Unlock()
		return AgentMessage{}, errf(404, "message not found: %s", messageID)
	}
	if message.ToAgentID != agentID {
		h.mu.Unlock()
		return AgentMessage{}, errf(409, "message %s no longer belongs to this Agent", message.ID)
	}
	matched := message.DeliveredTurnID == turnID
	for _, attempt := range message.HandlingAttempts {
		if attempt.TurnID == turnID {
			matched = true
			break
		}
	}
	if turnID != "" && !matched {
		h.mu.Unlock()
		return AgentMessage{}, errf(409, "message %s is not the source of interrupted Turn %s", message.ID, turnID)
	}
	if message.HandlingStatus != "interrupted" && message.HandlingStatus != "failed" {
		h.mu.Unlock()
		return AgentMessage{}, errf(409, "message %s is not held after interruption", message.ID)
	}
	if message.Resolution == "cancelled" || message.Resolution == "reply" || message.Resolution == "completed_elsewhere" || message.Resolution == "superseded" {
		h.mu.Unlock()
		return AgentMessage{}, errf(409, "message %s is already resolved", message.ID)
	}
	next := *message
	next.HandlingAttempts = cloneAgentMessageHandlingAttempts(message.HandlingAttempts)
	next.DeliveryStatus = "queued"
	next.DeliveryMode = ""
	next.DeliveredAt = ""
	next.DeliveredAgentID = ""
	next.DeliveredSessionID = ""
	next.DeliveredTurnID = ""
	next.LastDeliveryError = ""
	next.HandlingStatus = "pending"
	next.ActiveHandlingID = ""
	next.LastHandlingError = ""
	next.UpdatedAt = now()
	if err := h.commitAgentMessageLocked(next); err != nil {
		h.mu.Unlock()
		return AgentMessage{}, errf(500, "save continued message: %s", err)
	}
	h.mu.Unlock()
	h.startWorker(func() { h.deliverNextQueuedForTarget(agentID, defaultInactivity) })
	return next, nil
}
