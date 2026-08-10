package hub

import (
	"fmt"
	"strings"
	"time"
)

const (
	TurnRecoveryPlanned    = "planned"
	TurnRecoveryDispatched = "dispatched"
	TurnRecoveryCompleted  = "completed"
)

type TurnRecoveryMarker struct {
	PredecessorTurnID string                `json:"predecessorTurnId"`
	NativeTurnID      string                `json:"nativeTurnId,omitempty"`
	Disposition       string                `json:"disposition"`
	State             string                `json:"state"`
	RecoveryTurnID    string                `json:"recoveryTurnId,omitempty"`
	HumanRequestID    string                `json:"humanRequestId,omitempty"`
	TopicID           string                `json:"topicId,omitempty"`
	EvidenceLeafID    string                `json:"evidenceLeafId,omitempty"`
	UnfinishedTools   []RuntimeToolEvidence `json:"unfinishedTools,omitempty"`
	CreatedAt         string                `json:"createdAt"`
	UpdatedAt         string                `json:"updatedAt"`
}

func (h *Hub) recoverPiInterruptedTurn(agentID, predecessorTurnID string) {
	h.mu.Lock()
	if !h.claimPiRecoveryLocked(agentID, predecessorTurnID) {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	h.recoverPiInterruptedTurnClaimed(agentID, predecessorTurnID)
}

func recoveryKey(agentID, predecessorTurnID string) string {
	return agentID + "\x00" + predecessorTurnID
}

func (h *Hub) claimPiRecoveryLocked(agentID, predecessorTurnID string) bool {
	if h.turnRecoveryInFlight == nil {
		h.turnRecoveryInFlight = map[string]bool{}
	}
	key := recoveryKey(agentID, predecessorTurnID)
	if h.turnRecoveryInFlight[key] {
		return false
	}
	h.turnRecoveryInFlight[key] = true
	return true
}

// schedulePiRecoveryLocked claims the interrupted Turn before releasing h.mu,
// so a manual Continue or Dismiss cannot slip in before the durable marker is
// written by the recovery worker.
func (h *Hub) schedulePiRecoveryLocked(agentID, predecessorTurnID string) bool {
	if !h.claimPiRecoveryLocked(agentID, predecessorTurnID) {
		return false
	}
	if h.startWorkerLocked(func() { h.recoverPiInterruptedTurnClaimed(agentID, predecessorTurnID) }) {
		return true
	}
	delete(h.turnRecoveryInFlight, recoveryKey(agentID, predecessorTurnID))
	return false
}

func (h *Hub) recoverPiInterruptedTurnClaimed(agentID, predecessorTurnID string) {
	key := recoveryKey(agentID, predecessorTurnID)
	defer func() {
		h.mu.Lock()
		delete(h.turnRecoveryInFlight, key)
		h.mu.Unlock()
	}()

	h.mu.Lock()
	meta := h.agents[agentID]
	if meta == nil || meta.RuntimeBinding.Kind != "pi" {
		h.mu.Unlock()
		return
	}
	if existing, ok := meta.TurnRecoveryMarkers[predecessorTurnID]; ok {
		if h.recoveryTargetExistsLocked(meta, existing) {
			if existing.State != TurnRecoveryCompleted {
				existing.State = TurnRecoveryDispatched
				existing.UpdatedAt = now()
				meta.TurnRecoveryMarkers[predecessorTurnID] = existing
				_ = h.persistAgentsLocked()
			}
			h.mu.Unlock()
			return
		}
		if existing.State == TurnRecoveryDispatched || existing.State == TurnRecoveryCompleted {
			h.mu.Unlock()
			return
		}
	}
	if meta.Status != "interrupted" || meta.LastTurn == nil || meta.LastTurn.TurnID != predecessorTurnID || meta.LastTurn.Status != "interrupted" {
		h.mu.Unlock()
		return
	}
	predecessor := *meta.LastTurn
	nativeTurnID := meta.RuntimeTurnBindings[predecessorTurnID]
	nativeRef := meta.RuntimeBinding.NativeRef
	backend := runtimeForKind("pi")
	if rt := h.runtimes[agentID]; rt != nil && runtimeBackend(rt) != nil {
		backend = runtimeBackend(rt)
	}
	inspector, canInspect := backend.(RuntimeInterruptedTurnInspector)
	h.mu.Unlock()
	// A Pi process can close immediately after replying to get_entries. The
	// failure callback then races the serialized StartTurn caller that records
	// the returned native user-entry ID. Give that already-accepted response a
	// short chance to publish the binding before treating evidence as unknown.
	for attempt := 0; nativeTurnID == "" && attempt < 20; attempt++ {
		time.Sleep(10 * time.Millisecond)
		h.mu.Lock()
		if current := h.agents[agentID]; current != nil {
			nativeTurnID = current.RuntimeTurnBindings[predecessorTurnID]
		}
		h.mu.Unlock()
	}

	var evidence RuntimeInterruptionEvidence
	var inspectErr error
	if nativeTurnID == "" {
		inspectErr = fmt.Errorf("no native Turn binding is available for interrupted Loom Turn %s", predecessorTurnID)
	} else if !canInspect {
		inspectErr = fmt.Errorf("Pi Runtime does not expose durable interruption evidence")
	} else {
		evidence, inspectErr = inspector.InspectInterruptedTurn(nativeRef, nativeTurnID)
	}

	h.mu.Lock()
	meta = h.agents[agentID]
	if meta == nil || meta.Status != "interrupted" || meta.LastTurn == nil || meta.LastTurn.TurnID != predecessorTurnID || meta.LastTurn.Status != "interrupted" {
		h.mu.Unlock()
		return
	}
	if evidence.Status == RuntimeInterruptionTerminal && inspectErr == nil {
		meta.Status = "idle"
		if evidence.TerminalStatus == "completed" {
			meta.LastError = ""
		}
		meta.LastTurn.Status = evidence.TerminalStatus
		meta.UpdatedAt = now()
		_ = h.persistAgentsLocked()
		h.emitStatusLocked(meta, "idle")
		h.mu.Unlock()
		return
	}
	if meta.TurnRecoveryMarkers == nil {
		meta.TurnRecoveryMarkers = map[string]TurnRecoveryMarker{}
	}
	marker, exists := meta.TurnRecoveryMarkers[predecessorTurnID]
	if !exists {
		disposition := "recovery_turn"
		if inspectErr != nil || evidence.Status != RuntimeInterruptionClean {
			disposition = "needs_you"
		}
		stamp := now()
		marker = TurnRecoveryMarker{
			PredecessorTurnID: predecessorTurnID, NativeTurnID: nativeTurnID, Disposition: disposition,
			State: TurnRecoveryPlanned, EvidenceLeafID: evidence.LeafEntryID,
			UnfinishedTools: append([]RuntimeToolEvidence(nil), evidence.UnfinishedTools...), CreatedAt: stamp, UpdatedAt: stamp,
		}
		if source, _ := h.turnReferenceLocked(agentID, predecessorTurnID); source != nil {
			marker.TopicID = source.TopicID
		}
		if marker.TopicID == "" {
			if source := h.topicTurnReferenceLocked(agentID, predecessorTurnID); source != nil {
				marker.TopicID = source.TopicID
			}
		}
		if disposition == "recovery_turn" {
			marker.RecoveryTurnID = newIntegrationID("turn")
		} else {
			marker.HumanRequestID = newIntegrationID("hrq")
		}
		meta.TurnRecoveryMarkers[predecessorTurnID] = marker
		if err := h.persistAgentsLocked(); err != nil {
			delete(meta.TurnRecoveryMarkers, predecessorTurnID)
			h.mu.Unlock()
			return
		}
	}
	agentName, threadID := meta.Name, meta.ThreadID
	h.mu.Unlock()

	if marker.Disposition == "needs_you" {
		context := ambiguousRecoveryContext(predecessor, evidence, inspectErr)
		_, _, err := h.CreateOrGetHumanRequest(CreateHumanRequestParams{
			Agent: agentID, Question: "Confirm the outcome of interrupted work before continuing", Context: context,
			BlockedWork: predecessor.Task,
		}, HumanRequestCausality{
			ID: marker.HumanRequestID, ThreadID: threadID, SourceTurnID: predecessorTurnID,
			SourceTask: predecessor.Task, TopicID: marker.TopicID,
		})
		if err != nil {
			return
		}
		h.markRecoveryDispatched(agentID, predecessorTurnID, marker, true)
		return
	}

	source := h.recoverySource(agentID, predecessorTurnID, marker.TopicID)
	prompt := interruptedTurnRecoveryPrompt(predecessor, source)
	displayTask := "Continue interrupted work"
	if task := strings.TrimSpace(predecessor.Task); task != "" {
		displayTask += ": " + task
	}
	contextSource := turnContextSource{
		Origin: "platform_runtime", Trust: "loom_managed", Authority: "recovery_control",
		Kind: "restart_recovery", RefID: predecessorTurnID, TopicID: marker.TopicID,
	}
	_, err := h.sendTaskWithContextReserved(agentName, prompt, nil, defaultInactivity, "", "", "", marker.TopicID, displayTask, contextSource, marker.RecoveryTurnID)
	if err != nil {
		return
	}
	h.markRecoveryDispatched(agentID, predecessorTurnID, marker, false)
}

func (h *Hub) recoveryTargetExistsLocked(meta *Agent, marker TurnRecoveryMarker) bool {
	if marker.RecoveryTurnID != "" {
		if meta.CurrentTurnID == marker.RecoveryTurnID || meta.LastTurn != nil && meta.LastTurn.TurnID == marker.RecoveryTurnID || meta.RuntimeTurnBindings[marker.RecoveryTurnID] != "" {
			return true
		}
	}
	return marker.HumanRequestID != "" && h.humanRequests[marker.HumanRequestID] != nil
}

func (h *Hub) markRecoveryDispatched(agentID, predecessorTurnID string, marker TurnRecoveryMarker, clearAttention bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta := h.agents[agentID]
	if meta == nil {
		return
	}
	current, ok := meta.TurnRecoveryMarkers[predecessorTurnID]
	if !ok || current.RecoveryTurnID != marker.RecoveryTurnID || current.HumanRequestID != marker.HumanRequestID {
		return
	}
	if current.State != TurnRecoveryCompleted {
		current.State = TurnRecoveryDispatched
	}
	current.UpdatedAt = now()
	meta.TurnRecoveryMarkers[predecessorTurnID] = current
	if clearAttention && meta.Status == "interrupted" && meta.LastTurn != nil && meta.LastTurn.TurnID == predecessorTurnID {
		meta.Status = "idle"
		meta.LastError = ""
		meta.UpdatedAt = now()
		h.emitStatusLocked(meta, "idle")
	}
	_ = h.persistAgentsLocked()
}

func (h *Hub) recoverySource(agentID, predecessorTurnID, topicID string) *TurnReference {
	h.mu.Lock()
	defer h.mu.Unlock()
	if source, _ := h.turnReferenceLocked(agentID, predecessorTurnID); source != nil {
		return source
	}
	if source := h.topicTurnReferenceLocked(agentID, predecessorTurnID); source != nil {
		return source
	}
	if topicID != "" {
		return &TurnReference{Kind: "topic", ID: predecessorTurnID, TopicID: topicID}
	}
	return nil
}

func ambiguousRecoveryContext(predecessor TurnSummary, evidence RuntimeInterruptionEvidence, inspectErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Interrupted Turn: %s\n", predecessor.TurnID)
	if evidence.LeafEntryID != "" {
		fmt.Fprintf(&b, "Durable Pi evidence leaf: %s\n", evidence.LeafEntryID)
	}
	if inspectErr != nil {
		fmt.Fprintf(&b, "Loom could not safely inspect the durable Pi history: %v\n", inspectErr)
	}
	for _, tool := range evidence.UnfinishedTools {
		fmt.Fprintf(&b, "Tool %s (%s) started at %s without matching completion evidence. Command: %s\n", tool.Name, tool.ID, tool.StartedAt, tool.Command)
	}
	b.WriteString("The operation may have partially completed. Check its current effect before deciding how the Agent should continue; do not repeat the original action blindly.")
	return b.String()
}
