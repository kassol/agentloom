package hub

import (
	"sort"
	"strings"
)

const (
	WorkDispositionCompleted    = "completed"
	WorkDispositionContinuing   = "continuing"
	WorkDispositionNeedsYou     = "needs_you"
	WorkDispositionApproval     = "waiting_approval"
	WorkDispositionExternal     = "waiting_external"
	WorkDispositionAgent        = "waiting_agent"
	WorkDispositionTime         = "waiting_time"
	WorkDispositionPaused       = "paused"
	WorkDispositionFailed       = "failed"
	WorkDispositionUnclassified = "unclassified"
)

// WorkDisposition is the durable explanation of where work went when a Turn
// stopped. It is derived only from structured Loom state; final-answer prose
// is deliberately not an input.
type WorkDisposition struct {
	Kind        string           `json:"kind"`
	ThreadID    string           `json:"threadId"`
	TurnID      string           `json:"turnId"`
	TopicID     string           `json:"topicId,omitempty"`
	TurnStatus  string           `json:"turnStatus"`
	WakeSources []WorkWakeSource `json:"wakeSources,omitempty"`
	Unfinished  []WorkObligation `json:"unfinished,omitempty"`
	Guidance    string           `json:"guidance,omitempty"`
	RecordedAt  string           `json:"recordedAt"`
}

type WorkWakeSource struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	TopicID      string `json:"topicId,omitempty"`
	SourceTurnID string `json:"sourceTurnId,omitempty"`
	Summary      string `json:"summary,omitempty"`
	ResumeAction string `json:"resumeAction,omitempty"`
}

type WorkObligation struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	TopicID      string `json:"topicId,omitempty"`
	SourceTurnID string `json:"sourceTurnId,omitempty"`
	Summary      string `json:"summary,omitempty"`
}

func (h *Hub) terminalWorkDispositionLocked(meta *Agent, turn *turnState, status string) *WorkDisposition {
	if meta == nil || turn == nil || strings.TrimSpace(turn.turnID) == "" {
		return nil
	}
	disposition := &WorkDisposition{
		Kind: WorkDispositionCompleted, ThreadID: meta.ThreadID, TurnID: turn.turnID,
		TopicID: turn.topicID, TurnStatus: status, RecordedAt: now(),
	}

	addWake := func(source WorkWakeSource) {
		for _, existing := range disposition.WakeSources {
			if existing.Kind == source.Kind && existing.ID == source.ID {
				return
			}
		}
		disposition.WakeSources = append(disposition.WakeSources, source)
	}
	addObligation := func(obligation WorkObligation) {
		for _, existing := range disposition.Unfinished {
			if existing.Kind == obligation.Kind && existing.ID == obligation.ID {
				return
			}
		}
		disposition.Unfinished = append(disposition.Unfinished, obligation)
	}

	if goal := h.goals[meta.ID]; goal != nil && goal.ClearedAt == 0 && goal.Status != GoalStatusComplete {
		addObligation(WorkObligation{Kind: "goal", ID: goal.ID, SourceTurnID: turn.turnID, Summary: goal.Objective})
		if goal.Status == GoalStatusActive {
			addWake(WorkWakeSource{Kind: "goal", ID: goal.ID, SourceTurnID: turn.turnID, Summary: "Automatic Goal continuation"})
		}
	}

	if topic := h.topics[turn.topicID]; topic != nil && topic.Status != TopicStatusResolved && topic.Status != TopicStatusArchived {
		addObligation(WorkObligation{Kind: "topic", ID: topic.ID, TopicID: topic.ID, SourceTurnID: turn.turnID, Summary: topic.Title})
		if topic.WaitingOn != nil {
			kind := managedTopicWakeKind(topic.WaitingOn.Kind)
			if kind != "" {
				id := topic.WaitingOn.RefID
				if id == "" {
					id = topic.ID
				}
				addWake(WorkWakeSource{Kind: kind, ID: id, TopicID: topic.ID, SourceTurnID: turn.turnID, Summary: topic.WaitingOn.Summary, ResumeAction: topic.WaitingOn.ResumeAction})
			}
		}
	}

	for _, request := range h.humanRequests {
		if request != nil && request.AgentID == meta.ID && request.State == "open" && (request.SourceTurnID == turn.turnID || request.SourceTurnID == "" && request.TopicID != "" && request.TopicID == turn.topicID) {
			addWake(WorkWakeSource{Kind: "human_request", ID: request.ID, TopicID: request.TopicID, SourceTurnID: request.SourceTurnID, Summary: request.Question})
		}
	}
	for _, trigger := range h.triggers {
		if trigger == nil || trigger.AgentID != meta.ID || trigger.State != "pending" && trigger.State != "armed" {
			continue
		}
		if trigger.Work.SourceTurnID == turn.turnID || trigger.Work.SourceTurnID == "" && trigger.Work.TopicID != "" && trigger.Work.TopicID == turn.topicID {
			addWake(WorkWakeSource{Kind: "trigger", ID: trigger.ID, TopicID: trigger.Work.TopicID, SourceTurnID: trigger.Work.SourceTurnID, Summary: trigger.Provider + " " + trigger.ResourceKind, ResumeAction: trigger.ResumeInstruction})
		}
	}
	for _, message := range h.comms {
		if message == nil || message.FromAgentID != meta.ID || message.SourceTurnID != turn.turnID || message.Response != "required" || message.Status != "open" {
			continue
		}
		addWake(WorkWakeSource{Kind: "message", ID: message.ID, TopicID: message.TopicID, SourceTurnID: message.SourceTurnID, Summary: message.Subject})
	}
	for _, schedule := range h.schedules {
		if schedule == nil || schedule.AgentID != meta.ID || schedule.SourceTurnID != turn.turnID || !schedule.Enabled || schedule.NextRunAt == "" {
			continue
		}
		addWake(WorkWakeSource{Kind: "schedule", ID: schedule.ID, TopicID: schedule.TopicID, SourceTurnID: schedule.SourceTurnID, Summary: schedule.Subject, ResumeAction: schedule.NextRunAt})
	}

	// A required inbound Message remains a return obligation until the original
	// root is replied to or explicitly closed.
	if message := h.comms[turn.agentMessageID]; message != nil && message.Response == "required" && message.Status == "open" {
		addObligation(WorkObligation{Kind: "message", ID: message.ID, TopicID: message.TopicID, SourceTurnID: message.SourceTurnID, Summary: message.Subject})
	}
	if turn.inboxItemID != "" {
		if item := h.inbox[turn.inboxItemID]; item != nil && item.State != "handled" && item.State != "cancelled" {
			addObligation(WorkObligation{Kind: "inbox", ID: item.ID, SourceTurnID: turn.turnID, Summary: "External response obligation"})
		}
	}

	sort.Slice(disposition.WakeSources, func(i, j int) bool {
		if disposition.WakeSources[i].Kind != disposition.WakeSources[j].Kind {
			return disposition.WakeSources[i].Kind < disposition.WakeSources[j].Kind
		}
		return disposition.WakeSources[i].ID < disposition.WakeSources[j].ID
	})
	sort.Slice(disposition.Unfinished, func(i, j int) bool {
		if disposition.Unfinished[i].Kind != disposition.Unfinished[j].Kind {
			return disposition.Unfinished[i].Kind < disposition.Unfinished[j].Kind
		}
		return disposition.Unfinished[i].ID < disposition.Unfinished[j].ID
	})
	disposition.Kind = classifyWorkDisposition(status, disposition.WakeSources, disposition.Unfinished, h.goals[meta.ID])
	if disposition.Kind == WorkDispositionUnclassified {
		disposition.Guidance = "Create Needs You if Owner input is required; otherwise establish a Trigger, required Agent Message, Schedule, or Topic waiting condition."
	}
	return disposition
}

func managedTopicWakeKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch {
	case kind == "trigger" || strings.Contains(kind, "external") || strings.Contains(kind, "provider") || strings.Contains(kind, "github") || strings.Contains(kind, "pr"):
		return "trigger"
	case kind == "agent" || kind == "agent-message" || kind == "message":
		return "message"
	case kind == "schedule" || kind == "time" || kind == "calendar":
		return "schedule"
	case kind == "needs-you" || kind == "human-request" || kind == "owner":
		return "human_request"
	case kind == "approval":
		return "approval"
	default:
		return ""
	}
}

func classifyWorkDisposition(status string, wakes []WorkWakeSource, unfinished []WorkObligation, goal *ThreadGoal) string {
	priority := map[string]int{"human_request": 0, "approval": 1, "trigger": 2, "message": 3, "schedule": 4, "goal": 5}
	selected, selectedPriority := "", 100
	for _, wake := range wakes {
		if candidate, ok := priority[wake.Kind]; ok && candidate < selectedPriority {
			selected, selectedPriority = wake.Kind, candidate
		}
	}
	switch selected {
	case "human_request":
		return WorkDispositionNeedsYou
	case "approval":
		return WorkDispositionApproval
	case "trigger":
		return WorkDispositionExternal
	case "message":
		return WorkDispositionAgent
	case "schedule":
		return WorkDispositionTime
	case "goal":
		return WorkDispositionContinuing
	}
	if len(unfinished) > 0 {
		if goal != nil && goal.ClearedAt == 0 && goal.Status != GoalStatusActive && goal.Status != GoalStatusComplete {
			return WorkDispositionPaused
		}
		return WorkDispositionUnclassified
	}
	if status == "failed" || status == "interrupted" {
		return WorkDispositionFailed
	}
	return WorkDispositionCompleted
}
