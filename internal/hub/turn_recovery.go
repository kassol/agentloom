package hub

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

const (
	TurnRecoveryObserved   = "observed"
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
	ResolutionTurnID  string                `json:"resolutionTurnId,omitempty"`
	TopicID           string                `json:"topicId,omitempty"`
	EvidenceLeafID    string                `json:"evidenceLeafId,omitempty"`
	UnfinishedTools   []RuntimeToolEvidence `json:"unfinishedTools,omitempty"`
	RuntimeKind       string                `json:"runtimeKind,omitempty"`
	Cause             string                `json:"cause,omitempty"`
	FailurePhase      string                `json:"failurePhase,omitempty"`
	FailureCode       string                `json:"failureCode,omitempty"`
	Summary           string                `json:"summary,omitempty"`
	CreatedAt         string                `json:"createdAt"`
	UpdatedAt         string                `json:"updatedAt"`
}

func recoveryHumanRequestID(source turnContextSource) string {
	if source.Origin == "owner" && source.Trust == "authenticated" && source.Authority == "current_intent" && source.Kind == "needs_you_answer" {
		return source.RefID
	}
	return ""
}

func (h *Hub) bindRecoveryHumanTurnMarkerLocked(meta *Agent, requestID, turnID string) bool {
	if meta == nil || requestID == "" || turnID == "" {
		return false
	}
	for predecessorTurnID, marker := range meta.TurnRecoveryMarkers {
		if marker.HumanRequestID != requestID || marker.State == TurnRecoveryCompleted {
			continue
		}
		marker.ResolutionTurnID = turnID
		marker.UpdatedAt = now()
		meta.TurnRecoveryMarkers[predecessorTurnID] = marker
		return true
	}
	return false
}

func (h *Hub) bindRecoveryHumanAnswerLocked(agentID, requestID, turnID string) error {
	meta := h.agents[agentID]
	if meta == nil || turnID == "" {
		return nil
	}
	previousMarkers := make(map[string]TurnRecoveryMarker, len(meta.TurnRecoveryMarkers))
	for id, current := range meta.TurnRecoveryMarkers {
		previousMarkers[id] = current
	}
	if !h.bindRecoveryHumanTurnMarkerLocked(meta, requestID, turnID) {
		return nil
	}
	terminalTurnID := ""
	if meta.LastTurn != nil && meta.LastTurn.TurnID == turnID && meta.LastTurn.Status != "interrupted" {
		terminalTurnID = turnID
	}
	h.completeTurnRecoveryMarkersLocked(meta, terminalTurnID)
	if err := h.persistAgentsLocked(); err != nil {
		meta.TurnRecoveryMarkers = previousMarkers
		return err
	}
	if h.goalContinuationReadyLocked(agentID) {
		h.startWorkerLocked(func() { h.continueGoal(agentID) })
	}
	return nil
}

func (h *Hub) reconcileRecoveryHumanAnswersLocked() {
	for _, meta := range h.agents {
		for predecessorTurnID, marker := range meta.TurnRecoveryMarkers {
			request := h.humanRequests[marker.HumanRequestID]
			if marker.State == TurnRecoveryCompleted || request == nil || request.DeliveryStatus != "delivered" || request.ResumedTurnID == "" {
				continue
			}
			marker.ResolutionTurnID = request.ResumedTurnID
			marker.UpdatedAt = now()
			meta.TurnRecoveryMarkers[predecessorTurnID] = marker
			terminalTurnID := ""
			if meta.LastTurn != nil && meta.LastTurn.TurnID == request.ResumedTurnID && meta.LastTurn.Status != "interrupted" {
				terminalTurnID = request.ResumedTurnID
			}
			h.completeTurnRecoveryMarkersLocked(meta, terminalTurnID)
		}
	}
}

func (h *Hub) completeTurnRecoveryMarkersLocked(meta *Agent, terminalTurnID string) {
	if meta == nil {
		return
	}
	completedTurns := map[string]bool{}
	if terminalTurnID != "" {
		completedTurns[terminalTurnID] = true
	}
	for changed := true; changed; {
		changed = false
		for predecessorTurnID, marker := range meta.TurnRecoveryMarkers {
			if marker.State == TurnRecoveryCompleted {
				completedTurns[predecessorTurnID] = true
				continue
			}
			if !completedTurns[marker.RecoveryTurnID] && !completedTurns[marker.ResolutionTurnID] {
				continue
			}
			marker.State = TurnRecoveryCompleted
			marker.UpdatedAt = now()
			meta.TurnRecoveryMarkers[predecessorTurnID] = marker
			completedTurns[predecessorTurnID] = true
			changed = true
		}
	}
}

func (h *Hub) recoverPiInterruptedTurn(agentID, predecessorTurnID string) {
	h.recoverInterruptedTurn(agentID, predecessorTurnID)
}

func (h *Hub) recoverInterruptedTurn(agentID, predecessorTurnID string) {
	h.mu.Lock()
	if !h.claimTurnRecoveryLocked(agentID, predecessorTurnID) {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	h.recoverInterruptedTurnClaimed(agentID, predecessorTurnID)
}

func recoveryKey(agentID, predecessorTurnID string) string {
	return agentID + "\x00" + predecessorTurnID
}

func (h *Hub) claimTurnRecoveryLocked(agentID, predecessorTurnID string) bool {
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
	return h.scheduleTurnRecoveryLocked(agentID, predecessorTurnID)
}

func (h *Hub) scheduleTurnRecoveryLocked(agentID, predecessorTurnID string) bool {
	if !h.claimTurnRecoveryLocked(agentID, predecessorTurnID) {
		return false
	}
	if h.startWorkerLocked(func() { h.recoverInterruptedTurnClaimed(agentID, predecessorTurnID) }) {
		return true
	}
	delete(h.turnRecoveryInFlight, recoveryKey(agentID, predecessorTurnID))
	return false
}

func (h *Hub) recoverInterruptedTurnClaimed(agentID, predecessorTurnID string) {
	key := recoveryKey(agentID, predecessorTurnID)
	defer func() {
		h.mu.Lock()
		delete(h.turnRecoveryInFlight, key)
		if h.goalContinuationReadyLocked(agentID) {
			h.startWorkerLocked(func() { h.continueGoal(agentID) })
		}
		h.mu.Unlock()
	}()

	h.mu.Lock()
	meta := h.agents[agentID]
	if meta == nil || meta.Source == "edge" {
		h.mu.Unlock()
		return
	}
	if existing, ok := meta.TurnRecoveryMarkers[predecessorTurnID]; ok {
		if h.recoveryTargetExistsLocked(meta, existing) {
			if existing.State != TurnRecoveryCompleted {
				previous := existing
				existing.State = TurnRecoveryDispatched
				existing.UpdatedAt = now()
				meta.TurnRecoveryMarkers[predecessorTurnID] = existing
				if err := h.persistAgentsLocked(); err != nil {
					meta.TurnRecoveryMarkers[predecessorTurnID] = previous
					log.Printf("[codex-loom] persist recovered target marker: %v", err)
				}
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
	binding := runtimeContractBinding(meta)
	runtimeKind := meta.RuntimeBinding.Kind
	rt := h.runtimes[agentID]
	if rt == nil {
		var acquireErr error
		rt, acquireErr = h.getRuntimeLocked(meta)
		if acquireErr != nil {
			log.Printf("[codex-loom] acquire Runtime for interrupted Turn inspection on Agent %s: %v", agentID, acquireErr)
		}
	}
	h.mu.Unlock()
	var runtimeReadyErr error
	if rt != nil && rt.ready != nil {
		runtimeReadyErr = waitReady(rt)
	}
	var inspect func(context.Context, runtimecontract.TurnTarget) (RuntimeInterruptionEvidence, error)
	if runtimeReadyErr == nil && rt != nil {
		if inspector, ok := rt.runtimeContract.(runtimeInterruptedTurnInspector); ok {
			inspect = inspector.InspectInterruptedTurn
		}
	}
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
	if runtimeReadyErr != nil {
		inspectErr = fmt.Errorf("Runtime is not ready for interruption inspection: %w", runtimeReadyErr)
	} else if nativeTurnID == "" {
		inspectErr = fmt.Errorf("no native Turn binding is available for interrupted Loom Turn %s", predecessorTurnID)
	} else if inspect == nil {
		inspectErr = fmt.Errorf("%s Runtime does not expose durable interruption evidence", runtimeKind)
	} else {
		evidence, inspectErr = inspect(context.Background(), runtimecontract.TurnTarget{
			Binding: binding, TurnID: predecessorTurnID, RuntimeTurnRef: nativeTurnID,
		})
	}
	if inspectErr != nil {
		log.Printf("[codex-loom] inspect interrupted Runtime Turn for Agent %s predecessor %s: %v", agentID, predecessorTurnID, inspectErr)
	}

	h.mu.Lock()
	meta = h.agents[agentID]
	if meta == nil || meta.Status != "interrupted" || meta.LastTurn == nil || meta.LastTurn.TurnID != predecessorTurnID || meta.LastTurn.Status != "interrupted" {
		h.mu.Unlock()
		return
	}
	if evidence.Status == RuntimeInterruptionTerminal && inspectErr == nil {
		previousStatus, previousError, previousUpdatedAt := meta.Status, meta.LastError, meta.UpdatedAt
		previousTurnStatus := meta.LastTurn.Status
		previousMarker, hadMarker := meta.TurnRecoveryMarkers[predecessorTurnID]
		previousMarkers := make(map[string]TurnRecoveryMarker, len(meta.TurnRecoveryMarkers))
		for id, marker := range meta.TurnRecoveryMarkers {
			previousMarkers[id] = marker
		}
		meta.Status = "idle"
		if evidence.TerminalStatus == "completed" {
			meta.LastError = ""
		}
		meta.LastTurn.Status = evidence.TerminalStatus
		meta.UpdatedAt = now()
		if hadMarker {
			previousMarker.State = TurnRecoveryCompleted
			previousMarker.Disposition = "terminal_evidence"
			previousMarker.EvidenceLeafID = evidence.LeafEntryID
			previousMarker.UpdatedAt = now()
			meta.TurnRecoveryMarkers[predecessorTurnID] = previousMarker
		}
		h.completeTurnRecoveryMarkersLocked(meta, predecessorTurnID)
		if err := h.persistAgentsLocked(); err != nil {
			meta.Status, meta.LastError, meta.UpdatedAt = previousStatus, previousError, previousUpdatedAt
			meta.LastTurn.Status = previousTurnStatus
			meta.TurnRecoveryMarkers = previousMarkers
			log.Printf("[codex-loom] persist terminal recovery evidence: %v", err)
			h.mu.Unlock()
			return
		}
		h.emitStatusLocked(meta, "idle")
		h.mu.Unlock()
		return
	}
	if meta.TurnRecoveryMarkers == nil {
		meta.TurnRecoveryMarkers = map[string]TurnRecoveryMarker{}
	}
	marker, exists := meta.TurnRecoveryMarkers[predecessorTurnID]
	if !exists || marker.State == TurnRecoveryObserved {
		previousMarker := marker
		disposition := "recovery_turn"
		if inspectErr != nil || evidence.Status != RuntimeInterruptionClean {
			disposition = "needs_you"
		}
		stamp := now()
		marker.PredecessorTurnID = predecessorTurnID
		marker.NativeTurnID = nativeTurnID
		marker.Disposition = disposition
		marker.State = TurnRecoveryPlanned
		marker.EvidenceLeafID = evidence.LeafEntryID
		marker.UnfinishedTools = append([]RuntimeToolEvidence(nil), evidence.UnfinishedTools...)
		if marker.CreatedAt == "" {
			marker.CreatedAt = stamp
		}
		marker.UpdatedAt = stamp
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
			if exists {
				meta.TurnRecoveryMarkers[predecessorTurnID] = previousMarker
			} else {
				delete(meta.TurnRecoveryMarkers, predecessorTurnID)
			}
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
		log.Printf("[codex-loom] dispatch recovery Turn for Agent %s predecessor %s: %v", agentID, predecessorTurnID, err)
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
	meta := h.agents[agentID]
	if meta == nil {
		h.mu.Unlock()
		return
	}
	current, ok := meta.TurnRecoveryMarkers[predecessorTurnID]
	if !ok || current.RecoveryTurnID != marker.RecoveryTurnID || current.HumanRequestID != marker.HumanRequestID {
		h.mu.Unlock()
		return
	}
	previousMarker := current
	previousStatus, previousError, previousUpdatedAt := meta.Status, meta.LastError, meta.UpdatedAt
	if current.State != TurnRecoveryCompleted {
		current.State = TurnRecoveryDispatched
	}
	current.UpdatedAt = now()
	meta.TurnRecoveryMarkers[predecessorTurnID] = current
	if clearAttention && meta.Status == "interrupted" && meta.LastTurn != nil && meta.LastTurn.TurnID == predecessorTurnID {
		meta.Status = "idle"
		meta.LastError = ""
		meta.UpdatedAt = now()
	}
	if err := h.persistAgentsLocked(); err != nil {
		meta.TurnRecoveryMarkers[predecessorTurnID] = previousMarker
		meta.Status, meta.LastError, meta.UpdatedAt = previousStatus, previousError, previousUpdatedAt
		log.Printf("[codex-loom] persist dispatched recovery marker: %v", err)
		h.mu.Unlock()
		return
	}
	if clearAttention && previousStatus == "interrupted" && meta.Status == "idle" {
		h.emitStatusLocked(meta, "idle")
	}
	h.mu.Unlock()
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
	if inspectErr != nil {
		b.WriteString("Loom could not safely confirm the durable Runtime outcome.\n")
	}
	for _, tool := range evidence.UnfinishedTools {
		fmt.Fprintf(&b, "Tool %s has no matching completion evidence", tool.Name)
		if tool.StartedAt != "" {
			fmt.Fprintf(&b, " (started at %s)", tool.StartedAt)
		}
		if tool.Command != "" {
			fmt.Fprintf(&b, ". Command: %s", tool.Command)
		}
		b.WriteString(".\n")
	}
	b.WriteString("The operation may have partially completed. Check its current effect before deciding how the Agent should continue; do not repeat the original action blindly.")
	return b.String()
}
