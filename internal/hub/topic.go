package hub

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

const (
	TopicStatusActive   = "active"
	TopicStatusWaiting  = "waiting"
	TopicStatusResolved = "resolved"
	TopicStatusArchived = "archived"
	topicIdentity       = "topic"
	topicAgentID        = "system:topic"
)

// Topic is a thin, durable coordination record. Agent Threads remain the
// execution and high-resolution context boundary; Topic only keeps the shared
// brief, scoped responsibility, waiting condition and evidence references.
type Topic struct {
	ID                    string             `json:"id"`
	Title                 string             `json:"title"`
	Purpose               string             `json:"purpose"`
	CompletionBoundary    string             `json:"completionBoundary"`
	Status                string             `json:"status"`
	ResponsibleAgentID    string             `json:"responsibleAgentId"`
	ResponsibleAgent      string             `json:"responsibleAgent"`
	Participants          []TopicParticipant `json:"participants"`
	CurrentBrief          TopicBrief         `json:"currentBrief"`
	BriefHistory          []TopicBrief       `json:"briefHistory,omitempty"`
	WaitingOn             *TopicWaitingOn    `json:"waitingOn,omitempty"`
	Links                 []TopicLink        `json:"links,omitempty"`
	Events                []TopicEvent       `json:"events,omitempty"`
	DeliveryCursors       map[string]int64   `json:"deliveryCursors,omitempty"`
	ResultReadyVersion    int                `json:"resultReadyVersion,omitempty"`
	OwnerSeenBriefVersion int                `json:"ownerSeenBriefVersion,omitempty"`
	Version               int                `json:"version"`
	NextEventSeq          int64              `json:"nextEventSeq"`
	CreatedBy             string             `json:"createdBy"`
	CreatedAt             string             `json:"createdAt"`
	UpdatedAt             string             `json:"updatedAt"`
	ResolvedAt            string             `json:"resolvedAt,omitempty"`
}

type TopicParticipant struct {
	AgentID        string `json:"agentId"`
	Agent          string `json:"agent"`
	Responsibility string `json:"responsibility"`
	JoinedAt       string `json:"joinedAt"`
}

type TopicBrief struct {
	Version      int        `json:"version"`
	Summary      string     `json:"summary"`
	CurrentState string     `json:"currentState,omitempty"`
	NextStep     string     `json:"nextStep,omitempty"`
	Limitations  string     `json:"limitations,omitempty"`
	UpdatedBy    string     `json:"updatedBy"`
	UpdatedAt    string     `json:"updatedAt"`
	Evidence     []TopicRef `json:"evidence,omitempty"`
}

type TopicWaitingOn struct {
	Kind         string `json:"kind"`
	RefID        string `json:"refId,omitempty"`
	Summary      string `json:"summary"`
	ResumeAction string `json:"resumeAction,omitempty"`
	Since        string `json:"since"`
}

type TopicRef struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

type TopicLink struct {
	Type          string `json:"type"`
	ID            string `json:"id"`
	Relation      string `json:"relation"`
	Label         string `json:"label,omitempty"`
	LinkedBy      string `json:"linkedBy"`
	InheritedFrom string `json:"inheritedFrom,omitempty"`
	CreatedAt     string `json:"createdAt"`
}

type TopicEvent struct {
	Seq       int64     `json:"seq"`
	Type      string    `json:"type"`
	Actor     string    `json:"actor"`
	AgentID   string    `json:"agentId,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	Summary   string    `json:"summary"`
	Ref       *TopicRef `json:"ref,omitempty"`
	CreatedAt string    `json:"createdAt"`
}

type TopicActiveTurn struct {
	AgentID   string `json:"agentId"`
	Agent     string `json:"agent"`
	TurnID    string `json:"turnId"`
	Task      string `json:"task"`
	Source    string `json:"source"`
	StartedAt string `json:"startedAt"`
}

type TopicView struct {
	Topic
	NeedsMeCount int               `json:"needsMeCount"`
	ResultsReady bool              `json:"resultsReady"`
	ActiveTurns  []TopicActiveTurn `json:"activeTurns"`
}

// TopicSummaryView is the bounded list projection used by the Web workspace.
// Audit events, brief history, evidence links and participant detail remain on
// GET /api/topics/{id} and are loaded only for the selected Topic.
type TopicSummaryView struct {
	ID                    string            `json:"id"`
	Title                 string            `json:"title"`
	Purpose               string            `json:"purpose,omitempty"`
	Status                string            `json:"status"`
	ResponsibleAgentID    string            `json:"responsibleAgentId"`
	ResponsibleAgent      string            `json:"responsibleAgent"`
	CurrentBrief          TopicBrief        `json:"currentBrief"`
	WaitingOn             *TopicWaitingOn   `json:"waitingOn,omitempty"`
	ResultReadyVersion    int               `json:"resultReadyVersion,omitempty"`
	OwnerSeenBriefVersion int               `json:"ownerSeenBriefVersion,omitempty"`
	Version               int               `json:"version"`
	CreatedAt             string            `json:"createdAt"`
	UpdatedAt             string            `json:"updatedAt"`
	ResolvedAt            string            `json:"resolvedAt,omitempty"`
	NeedsMeCount          int               `json:"needsMeCount"`
	ResultsReady          bool              `json:"resultsReady"`
	ActiveTurns           []TopicActiveTurn `json:"activeTurns"`
}

type TopicParticipantParams struct {
	Agent          string `json:"agent"`
	Responsibility string `json:"responsibility"`
}

type CreateTopicParams struct {
	Title              string                   `json:"title"`
	Purpose            string                   `json:"purpose"`
	CompletionBoundary string                   `json:"completionBoundary"`
	ResponsibleAgent   string                   `json:"responsibleAgent"`
	Participants       []TopicParticipantParams `json:"participants"`
	InitialBrief       TopicBriefDraft          `json:"initialBrief"`
	CreatedFrom        *TopicLink               `json:"createdFrom,omitempty"`
	CreatedBy          string                   `json:"createdBy"`
}

type TopicBriefDraft struct {
	Summary      string     `json:"summary"`
	CurrentState string     `json:"currentState"`
	NextStep     string     `json:"nextStep"`
	Limitations  string     `json:"limitations"`
	Evidence     []TopicRef `json:"evidence"`
}

type UpdateTopicParams struct {
	Actor              string           `json:"actor"`
	ExpectedVersion    int              `json:"expectedVersion"`
	Title              *string          `json:"title,omitempty"`
	Purpose            *string          `json:"purpose,omitempty"`
	CompletionBoundary *string          `json:"completionBoundary,omitempty"`
	Status             *string          `json:"status,omitempty"`
	Brief              *TopicBriefDraft `json:"brief,omitempty"`
	WaitingOn          *TopicWaitingOn  `json:"waitingOn,omitempty"`
	ClearWaiting       bool             `json:"clearWaiting,omitempty"`
	PublishResult      bool             `json:"publishResult,omitempty"`
}

type TopicInputParams struct {
	Text       string `json:"text"`
	TimeoutSec int    `json:"timeoutSec"`
}

type TopicInterventionParams struct {
	Agent  string `json:"agent"`
	Action string `json:"action"` // steer, interrupt
	Text   string `json:"text,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type TopicInterventionResult struct {
	TopicID string     `json:"topicId"`
	AgentID string     `json:"agentId"`
	Agent   string     `json:"agent"`
	TurnID  string     `json:"turnId"`
	Action  string     `json:"action"`
	Event   TopicEvent `json:"event"`
}

func (h *Hub) persistTopicsLocked() error { return h.st.SaveTopics(h.topics) }

func (h *Hub) normalizeTopicsLocked() bool {
	changed := false
	clean := func(value *string) {
		normalized := cleanLegacyTopicText(*value)
		if normalized != *value {
			*value = normalized
			changed = true
		}
	}
	cleanBrief := func(brief *TopicBrief) {
		clean(&brief.Summary)
		clean(&brief.CurrentState)
		clean(&brief.NextStep)
		clean(&brief.Limitations)
		clean(&brief.UpdatedBy)
		for index := range brief.Evidence {
			clean(&brief.Evidence[index].Type)
			clean(&brief.Evidence[index].ID)
			clean(&brief.Evidence[index].Label)
		}
	}
	for key, topic := range h.topics {
		if topic == nil {
			delete(h.topics, key)
			changed = true
			continue
		}
		if topic.ID == "" {
			topic.ID = key
			changed = true
		}
		if !validTopicStatus(topic.Status) {
			topic.Status = TopicStatusActive
			changed = true
		}
		if topic.Version < 1 {
			topic.Version = 1
			changed = true
		}
		if topic.CurrentBrief.Version < 1 {
			topic.CurrentBrief.Version = 1
			changed = true
		}
		if len(topic.BriefHistory) == 0 {
			topic.BriefHistory = []TopicBrief{topic.CurrentBrief}
			changed = true
		}
		if topic.DeliveryCursors == nil {
			topic.DeliveryCursors = map[string]int64{}
			changed = true
		}
		if agent := h.agents[topic.ResponsibleAgentID]; agent != nil {
			if topic.ResponsibleAgent != agent.Name {
				topic.ResponsibleAgent = agent.Name
				changed = true
			}
		}
		for i := range topic.Participants {
			if agent := h.agents[topic.Participants[i].AgentID]; agent != nil {
				if topic.Participants[i].Agent != agent.Name {
					topic.Participants[i].Agent = agent.Name
					changed = true
				}
			}
			clean(&topic.Participants[i].Agent)
			clean(&topic.Participants[i].Responsibility)
		}
		for _, event := range topic.Events {
			if event.Seq > topic.NextEventSeq {
				topic.NextEventSeq = event.Seq
				changed = true
			}
		}
		clean(&topic.Title)
		clean(&topic.Purpose)
		clean(&topic.CompletionBoundary)
		clean(&topic.ResponsibleAgent)
		if topic.WaitingOn != nil {
			clean(&topic.WaitingOn.Kind)
			clean(&topic.WaitingOn.RefID)
			clean(&topic.WaitingOn.Summary)
			clean(&topic.WaitingOn.ResumeAction)
		}
		cleanBrief(&topic.CurrentBrief)
		for index := range topic.BriefHistory {
			cleanBrief(&topic.BriefHistory[index])
		}
		for index := range topic.Links {
			clean(&topic.Links[index].Type)
			clean(&topic.Links[index].ID)
			clean(&topic.Links[index].Relation)
			clean(&topic.Links[index].Label)
		}
		for index := range topic.Events {
			clean(&topic.Events[index].Type)
			clean(&topic.Events[index].Actor)
			clean(&topic.Events[index].Agent)
			clean(&topic.Events[index].Summary)
			if topic.Events[index].Ref != nil {
				clean(&topic.Events[index].Ref.Type)
				clean(&topic.Events[index].Ref.ID)
				clean(&topic.Events[index].Ref.Label)
			}
		}
	}
	return changed
}

func cloneTopic(topic Topic) Topic {
	topic.Participants = append([]TopicParticipant{}, topic.Participants...)
	topic.BriefHistory = append([]TopicBrief(nil), topic.BriefHistory...)
	for i := range topic.BriefHistory {
		topic.BriefHistory[i].Evidence = append([]TopicRef(nil), topic.BriefHistory[i].Evidence...)
	}
	topic.CurrentBrief.Evidence = append([]TopicRef(nil), topic.CurrentBrief.Evidence...)
	topic.Links = append([]TopicLink(nil), topic.Links...)
	topic.Events = append([]TopicEvent(nil), topic.Events...)
	for i := range topic.Events {
		if topic.Events[i].Ref != nil {
			ref := *topic.Events[i].Ref
			topic.Events[i].Ref = &ref
		}
	}
	if topic.WaitingOn != nil {
		waiting := *topic.WaitingOn
		topic.WaitingOn = &waiting
	}
	cursors := topic.DeliveryCursors
	topic.DeliveryCursors = map[string]int64{}
	for key, value := range cursors {
		topic.DeliveryCursors[key] = value
	}
	return topic
}

func validTopicStatus(status string) bool {
	switch status {
	case TopicStatusActive, TopicStatusWaiting, TopicStatusResolved, TopicStatusArchived:
		return true
	default:
		return false
	}
}

func (h *Hub) CreateTopic(params CreateTopicParams) (TopicView, error) {
	params.Title = strings.TrimSpace(params.Title)
	params.Purpose = strings.TrimSpace(params.Purpose)
	params.CompletionBoundary = strings.TrimSpace(params.CompletionBoundary)
	params.ResponsibleAgent = strings.TrimSpace(params.ResponsibleAgent)
	params.CreatedBy = strings.TrimSpace(params.CreatedBy)
	params.InitialBrief.Summary = strings.TrimSpace(params.InitialBrief.Summary)
	if params.Title == "" || params.Purpose == "" || params.CompletionBoundary == "" || params.ResponsibleAgent == "" {
		return TopicView{}, errf(400, "title, purpose, completionBoundary, and responsibleAgent are required")
	}
	if params.CreatedBy == "" {
		params.CreatedBy = "owner"
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	responsible := h.resolveLocked(params.ResponsibleAgent)
	if responsible == nil {
		return TopicView{}, errf(404, "responsible agent not found: %s", params.ResponsibleAgent)
	}
	stamp := now()
	participants := make([]TopicParticipant, 0, len(params.Participants))
	seen := map[string]bool{responsible.ID: true}
	for _, candidate := range params.Participants {
		agent := h.resolveLocked(strings.TrimSpace(candidate.Agent))
		if agent == nil {
			return TopicView{}, errf(404, "participant agent not found: %s", candidate.Agent)
		}
		if seen[agent.ID] {
			continue
		}
		responsibility := strings.TrimSpace(candidate.Responsibility)
		if responsibility == "" {
			return TopicView{}, errf(400, "responsibility is required for participant %s", agent.Name)
		}
		seen[agent.ID] = true
		participants = append(participants, TopicParticipant{AgentID: agent.ID, Agent: agent.Name, Responsibility: responsibility, JoinedAt: stamp})
	}
	brief := TopicBrief{
		Version: 1, Summary: params.InitialBrief.Summary, CurrentState: strings.TrimSpace(params.InitialBrief.CurrentState),
		NextStep: strings.TrimSpace(params.InitialBrief.NextStep), Limitations: strings.TrimSpace(params.InitialBrief.Limitations),
		UpdatedBy: params.CreatedBy, UpdatedAt: stamp, Evidence: append([]TopicRef(nil), params.InitialBrief.Evidence...),
	}
	topic := &Topic{
		ID: newIntegrationID("tpc"), Title: params.Title, Purpose: params.Purpose, CompletionBoundary: params.CompletionBoundary,
		Status: TopicStatusActive, ResponsibleAgentID: responsible.ID, ResponsibleAgent: responsible.Name,
		Participants: participants, CurrentBrief: brief, BriefHistory: []TopicBrief{brief}, DeliveryCursors: map[string]int64{},
		Version: 1, CreatedBy: params.CreatedBy, CreatedAt: stamp, UpdatedAt: stamp,
	}
	h.topics[topic.ID] = topic
	h.appendTopicEventMemoryLocked(topic, TopicEvent{Type: "created", Actor: params.CreatedBy, Summary: "Topic created", CreatedAt: stamp})
	if params.CreatedFrom != nil {
		link := *params.CreatedFrom
		link.LinkedBy = params.CreatedBy
		if link.Relation == "" {
			link.Relation = "source"
		}
		if link.CreatedAt == "" {
			link.CreatedAt = stamp
		}
		h.addTopicLinkMemoryLocked(topic, link)
	}
	for _, ref := range brief.Evidence {
		h.addTopicLinkMemoryLocked(topic, TopicLink{Type: ref.Type, ID: ref.ID, Label: ref.Label, Relation: "evidence", LinkedBy: params.CreatedBy, CreatedAt: stamp})
	}
	if err := h.persistTopicsLocked(); err != nil {
		delete(h.topics, topic.ID)
		return TopicView{}, errf(500, "persist Topic: %s", err)
	}
	h.emitGlobalLocked("loom/topic-updated", map[string]any{"topic": cloneTopic(*topic), "event": topic.Events[len(topic.Events)-1]})
	return h.topicViewLocked(topic), nil
}

func (h *Hub) ListTopics(status, agentKey string) []TopicView {
	h.mu.Lock()
	defer h.mu.Unlock()
	status = strings.TrimSpace(status)
	agentID := ""
	if agent := h.resolveLocked(strings.TrimSpace(agentKey)); agent != nil {
		agentID = agent.ID
	}
	out := make([]TopicView, 0, len(h.topics))
	for _, topic := range h.topics {
		if topic == nil || status != "" && status != "all" && topic.Status != status {
			continue
		}
		if agentKey != "" && !topicHasAgent(topic, agentID, agentKey) {
			continue
		}
		out = append(out, h.topicViewLocked(topic))
	}
	sort.SliceStable(out, func(i, j int) bool {
		left, right := topicViewOrder(out[i]), topicViewOrder(out[j])
		if left != right {
			return left < right
		}
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out
}

func (h *Hub) ListTopicSummaries(status, agentKey string) []TopicSummaryView {
	h.mu.Lock()
	defer h.mu.Unlock()
	status = strings.TrimSpace(status)
	agentID := ""
	if agent := h.resolveLocked(strings.TrimSpace(agentKey)); agent != nil {
		agentID = agent.ID
	}
	out := make([]TopicSummaryView, 0, len(h.topics))
	for _, topic := range h.topics {
		if topic == nil || status != "" && status != "all" && topic.Status != status {
			continue
		}
		if agentKey != "" && !topicHasAgent(topic, agentID, agentKey) {
			continue
		}
		out = append(out, h.topicSummaryViewLocked(topic))
	}
	sort.SliceStable(out, func(i, j int) bool {
		left, right := topicSummaryOrder(out[i]), topicSummaryOrder(out[j])
		if left != right {
			return left < right
		}
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out
}

func topicSummaryOrder(view TopicSummaryView) int {
	if view.NeedsMeCount > 0 {
		return 0
	}
	if view.ResultsReady {
		return 1
	}
	switch view.Status {
	case TopicStatusActive:
		return 2
	case TopicStatusWaiting:
		return 3
	case TopicStatusResolved:
		return 4
	default:
		return 5
	}
}

func topicViewOrder(view TopicView) int {
	if view.NeedsMeCount > 0 {
		return 0
	}
	if view.ResultsReady {
		return 1
	}
	switch view.Status {
	case TopicStatusActive:
		return 2
	case TopicStatusWaiting:
		return 3
	case TopicStatusResolved:
		return 4
	default:
		return 5
	}
}

func topicHasAgent(topic *Topic, agentID, key string) bool {
	if topic.ResponsibleAgentID == agentID || topic.ResponsibleAgentID == key || topic.ResponsibleAgent == key {
		return true
	}
	for _, participant := range topic.Participants {
		if participant.AgentID == agentID || participant.AgentID == key || participant.Agent == key {
			return true
		}
	}
	return false
}

func (h *Hub) GetTopic(id string) (TopicView, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	topic := h.topics[strings.TrimSpace(id)]
	if topic == nil {
		return TopicView{}, errf(404, "Topic not found: %s", id)
	}
	return h.topicViewLocked(topic), nil
}

func (h *Hub) topicViewLocked(topic *Topic) TopicView {
	view := TopicView{
		Topic:        cloneTopic(*topic),
		ResultsReady: topic.ResultReadyVersion > topic.OwnerSeenBriefVersion,
	}
	if responsible := h.agents[view.ResponsibleAgentID]; responsible != nil {
		view.ResponsibleAgent = responsible.Name
	}
	for index := range view.Participants {
		if participant := h.agents[view.Participants[index].AgentID]; participant != nil {
			view.Participants[index].Agent = participant.Name
		}
	}
	view.NeedsMeCount, view.ActiveTurns = h.topicRuntimeStateLocked(topic.ID, 0)
	return view
}

func (h *Hub) topicSummaryViewLocked(topic *Topic) TopicSummaryView {
	brief := topic.CurrentBrief
	brief.Summary = boundedDisplayTask(brief.Summary, 1_000)
	brief.CurrentState = ""
	brief.NextStep = boundedDisplayTask(brief.NextStep, 1_000)
	brief.Limitations = ""
	brief.Evidence = nil
	var waiting *TopicWaitingOn
	if topic.WaitingOn != nil {
		copy := *topic.WaitingOn
		waiting = &copy
	}
	needsMe, activeTurns := h.topicRuntimeStateLocked(topic.ID, 320)
	responsible := topic.ResponsibleAgent
	if agent := h.agents[topic.ResponsibleAgentID]; agent != nil {
		responsible = agent.Name
	}
	return TopicSummaryView{
		ID: topic.ID, Title: topic.Title, Purpose: boundedDisplayTask(topic.Purpose, 500), Status: topic.Status,
		ResponsibleAgentID: topic.ResponsibleAgentID, ResponsibleAgent: responsible,
		CurrentBrief: brief, WaitingOn: waiting,
		ResultReadyVersion: topic.ResultReadyVersion, OwnerSeenBriefVersion: topic.OwnerSeenBriefVersion,
		Version: topic.Version, CreatedAt: topic.CreatedAt, UpdatedAt: topic.UpdatedAt, ResolvedAt: topic.ResolvedAt,
		NeedsMeCount: needsMe, ResultsReady: topic.ResultReadyVersion > topic.OwnerSeenBriefVersion,
		ActiveTurns: activeTurns,
	}
}

func (h *Hub) topicRuntimeStateLocked(topicID string, taskLimit int) (int, []TopicActiveTurn) {
	needsMe := 0
	for _, request := range h.humanRequests {
		if request != nil && request.TopicID == topicID && request.State == "open" {
			needsMe++
		}
	}
	activeTurns := make([]TopicActiveTurn, 0)
	for agentID, rt := range h.runtimes {
		if rt == nil || rt.activeTurn == nil || rt.activeTurn.finished || rt.activeTurn.topicID != topicID {
			continue
		}
		agent := h.agents[agentID]
		if agent == nil {
			continue
		}
		task := rt.activeTurn.task
		if taskLimit > 0 {
			task = boundedDisplayTask(task, taskLimit)
		}
		activeTurns = append(activeTurns, TopicActiveTurn{
			AgentID: agent.ID, Agent: agent.Name, TurnID: rt.activeTurn.turnID,
			Task: task, Source: rt.activeTurn.source,
			StartedAt: rt.activeTurn.startedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	sort.SliceStable(activeTurns, func(i, j int) bool { return activeTurns[i].StartedAt < activeTurns[j].StartedAt })
	return needsMe, activeTurns
}

func (h *Hub) UpdateTopic(id string, params UpdateTopicParams) (TopicView, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	topic := h.topics[strings.TrimSpace(id)]
	if topic == nil {
		return TopicView{}, errf(404, "Topic not found: %s", id)
	}
	actor, err := h.authorizeTopicCoordinatorLocked(topic, params.Actor)
	if err != nil {
		return TopicView{}, err
	}
	if actor == "owner" && (params.Title != nil || params.Purpose != nil || params.CompletionBoundary != nil || params.ClearWaiting || params.WaitingOn != nil || params.Brief != nil || params.PublishResult) {
		return TopicView{}, errf(403, "send Topic scope, brief, waiting, and result changes to responsible Agent %s", topic.ResponsibleAgent)
	}
	if params.ExpectedVersion > 0 && params.ExpectedVersion != topic.Version {
		return TopicView{}, errf(409, "Topic version changed: expected %d, current %d", params.ExpectedVersion, topic.Version)
	}
	if params.PublishResult && actor == "owner" {
		return TopicView{}, errf(403, "only responsible Agent %s can publish a Topic result", topic.ResponsibleAgent)
	}
	previous := cloneTopic(*topic)
	stamp := now()
	changed := false
	if params.Title != nil {
		topic.Title = strings.TrimSpace(*params.Title)
		changed = true
	}
	if params.Purpose != nil {
		topic.Purpose = strings.TrimSpace(*params.Purpose)
		changed = true
	}
	if params.CompletionBoundary != nil {
		topic.CompletionBoundary = strings.TrimSpace(*params.CompletionBoundary)
		changed = true
	}
	if params.Status != nil {
		status := strings.TrimSpace(*params.Status)
		if !validTopicStatus(status) {
			return TopicView{}, errf(400, "invalid Topic status: %s", status)
		}
		if status != topic.Status {
			topic.Status = status
			if status == TopicStatusResolved {
				topic.ResolvedAt = stamp
			} else {
				topic.ResolvedAt = ""
			}
			h.appendTopicEventMemoryLocked(topic, TopicEvent{Type: "status_changed", Actor: actor, Summary: "Status changed to " + status, CreatedAt: stamp})
			changed = true
		}
	}
	if params.ClearWaiting && topic.WaitingOn != nil {
		topic.WaitingOn = nil
		if topic.Status == TopicStatusWaiting {
			topic.Status = TopicStatusActive
		}
		h.appendTopicEventMemoryLocked(topic, TopicEvent{Type: "waiting_cleared", Actor: actor, Summary: "Waiting condition cleared", CreatedAt: stamp})
		changed = true
	}
	if params.WaitingOn != nil {
		waiting := *params.WaitingOn
		waiting.Kind = strings.TrimSpace(waiting.Kind)
		waiting.RefID = strings.TrimSpace(waiting.RefID)
		waiting.Summary = strings.TrimSpace(waiting.Summary)
		waiting.ResumeAction = strings.TrimSpace(waiting.ResumeAction)
		if waiting.Kind == "" || waiting.Summary == "" {
			return TopicView{}, errf(400, "waitingOn kind and summary are required")
		}
		if waiting.Since == "" {
			waiting.Since = stamp
		}
		topic.WaitingOn = &waiting
		if topic.Status == TopicStatusActive {
			topic.Status = TopicStatusWaiting
		}
		ref := (*TopicRef)(nil)
		if waiting.RefID != "" {
			ref = &TopicRef{Type: waiting.Kind, ID: waiting.RefID, Label: waiting.Summary}
		}
		h.appendTopicEventMemoryLocked(topic, TopicEvent{Type: "waiting", Actor: actor, Summary: waiting.Summary, Ref: ref, CreatedAt: stamp})
		changed = true
	}
	if params.Brief != nil {
		draft := params.Brief
		brief := TopicBrief{Version: topic.CurrentBrief.Version + 1, Summary: strings.TrimSpace(draft.Summary), CurrentState: strings.TrimSpace(draft.CurrentState), NextStep: strings.TrimSpace(draft.NextStep), Limitations: strings.TrimSpace(draft.Limitations), UpdatedBy: actor, UpdatedAt: stamp, Evidence: append([]TopicRef(nil), draft.Evidence...)}
		if brief.Summary == "" {
			return TopicView{}, errf(400, "brief summary is required")
		}
		topic.CurrentBrief = brief
		topic.BriefHistory = append(topic.BriefHistory, brief)
		for _, ref := range brief.Evidence {
			h.addTopicLinkMemoryLocked(topic, TopicLink{Type: ref.Type, ID: ref.ID, Label: ref.Label, Relation: "evidence", LinkedBy: actor, CreatedAt: stamp})
		}
		eventType := "brief_updated"
		if params.PublishResult {
			eventType = "result_published"
			topic.ResultReadyVersion = brief.Version
		}
		h.appendTopicEventMemoryLocked(topic, TopicEvent{Type: eventType, Actor: actor, Summary: brief.Summary, CreatedAt: stamp})
		changed = true
	} else if params.PublishResult {
		return TopicView{}, errf(400, "publishResult requires a new brief")
	}
	if !changed {
		return h.topicViewLocked(topic), nil
	}
	if topic.Title == "" || topic.Purpose == "" || topic.CompletionBoundary == "" {
		*topic = previous
		return TopicView{}, errf(400, "title, purpose, and completionBoundary cannot be empty")
	}
	topic.Version++
	topic.UpdatedAt = stamp
	if err := h.persistTopicsLocked(); err != nil {
		*topic = previous
		return TopicView{}, errf(500, "persist Topic: %s", err)
	}
	h.emitGlobalLocked("loom/topic-updated", map[string]any{"topic": cloneTopic(*topic)})
	return h.topicViewLocked(topic), nil
}

func (h *Hub) authorizeTopicCoordinatorLocked(topic *Topic, actorKey string) (string, error) {
	actorKey = strings.TrimSpace(actorKey)
	if actorKey == "" || actorKey == "owner" {
		return "owner", nil
	}
	agent := h.resolveLocked(actorKey)
	if agent == nil || agent.ID != topic.ResponsibleAgentID {
		return "", errf(403, "only owner or responsible Agent %s can update Topic coordination state", topic.ResponsibleAgent)
	}
	return agent.Name, nil
}

func (h *Hub) authorizeTopicResponsibleLocked(topic *Topic, actorKey string) (string, error) {
	agent := h.resolveLocked(strings.TrimSpace(actorKey))
	if agent == nil || agent.ID != topic.ResponsibleAgentID {
		return "", errf(403, "only responsible Agent %s can change Topic participants after creation", topic.ResponsibleAgent)
	}
	return agent.Name, nil
}

func (h *Hub) AddTopicParticipant(id, actor string, params TopicParticipantParams) (TopicView, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	topic := h.topics[strings.TrimSpace(id)]
	if topic == nil {
		return TopicView{}, errf(404, "Topic not found: %s", id)
	}
	coordinator, err := h.authorizeTopicResponsibleLocked(topic, actor)
	if err != nil {
		return TopicView{}, err
	}
	agent := h.resolveLocked(strings.TrimSpace(params.Agent))
	if agent == nil {
		return TopicView{}, errf(404, "participant Agent not found: %s", params.Agent)
	}
	if agent.ID == topic.ResponsibleAgentID {
		return TopicView{}, errf(409, "%s is already the responsible Agent", agent.Name)
	}
	responsibility := strings.TrimSpace(params.Responsibility)
	if responsibility == "" {
		return TopicView{}, errf(400, "responsibility is required")
	}
	previous := cloneTopic(*topic)
	for i := range topic.Participants {
		if topic.Participants[i].AgentID == agent.ID {
			topic.Participants[i].Responsibility = responsibility
			topic.Version++
			topic.UpdatedAt = now()
			h.appendTopicEventMemoryLocked(topic, TopicEvent{Type: "participant_updated", Actor: coordinator, AgentID: agent.ID, Agent: agent.Name, Summary: responsibility, CreatedAt: topic.UpdatedAt})
			if err := h.persistTopicsLocked(); err != nil {
				*topic = previous
				return TopicView{}, errf(500, "persist Topic: %s", err)
			}
			h.emitGlobalLocked("loom/topic-updated", map[string]any{"topic": cloneTopic(*topic)})
			return h.topicViewLocked(topic), nil
		}
	}
	stamp := now()
	topic.Participants = append(topic.Participants, TopicParticipant{AgentID: agent.ID, Agent: agent.Name, Responsibility: responsibility, JoinedAt: stamp})
	topic.Version++
	topic.UpdatedAt = stamp
	h.appendTopicEventMemoryLocked(topic, TopicEvent{Type: "participant_added", Actor: coordinator, AgentID: agent.ID, Agent: agent.Name, Summary: responsibility, CreatedAt: stamp})
	if err := h.persistTopicsLocked(); err != nil {
		*topic = previous
		return TopicView{}, errf(500, "persist Topic: %s", err)
	}
	h.emitGlobalLocked("loom/topic-updated", map[string]any{"topic": cloneTopic(*topic)})
	return h.topicViewLocked(topic), nil
}

func (h *Hub) RemoveTopicParticipant(id, agentKey, actor string) (TopicView, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	topic := h.topics[strings.TrimSpace(id)]
	if topic == nil {
		return TopicView{}, errf(404, "Topic not found: %s", id)
	}
	coordinator, err := h.authorizeTopicResponsibleLocked(topic, actor)
	if err != nil {
		return TopicView{}, err
	}
	agent := h.resolveLocked(strings.TrimSpace(agentKey))
	if agent == nil {
		return TopicView{}, errf(404, "participant Agent not found: %s", agentKey)
	}
	previous := cloneTopic(*topic)
	for i, participant := range topic.Participants {
		if participant.AgentID != agent.ID {
			continue
		}
		topic.Participants = append(topic.Participants[:i], topic.Participants[i+1:]...)
		delete(topic.DeliveryCursors, agent.ID)
		topic.Version++
		topic.UpdatedAt = now()
		h.appendTopicEventMemoryLocked(topic, TopicEvent{Type: "participant_removed", Actor: coordinator, AgentID: agent.ID, Agent: agent.Name, Summary: "Participant removed", CreatedAt: topic.UpdatedAt})
		if err := h.persistTopicsLocked(); err != nil {
			*topic = previous
			return TopicView{}, errf(500, "persist Topic: %s", err)
		}
		h.emitGlobalLocked("loom/topic-updated", map[string]any{"topic": cloneTopic(*topic)})
		return h.topicViewLocked(topic), nil
	}
	return TopicView{}, errf(404, "%s is not a Topic participant", agent.Name)
}

func (h *Hub) LinkTopic(id, actor string, link TopicLink) (TopicView, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	topic := h.topics[strings.TrimSpace(id)]
	if topic == nil {
		return TopicView{}, errf(404, "Topic not found: %s", id)
	}
	if _, err := h.authorizeTopicCoordinatorLocked(topic, actor); err != nil {
		return TopicView{}, err
	}
	link.Type, link.ID, link.Relation = strings.TrimSpace(link.Type), strings.TrimSpace(link.ID), strings.TrimSpace(link.Relation)
	if link.Type == "" || link.ID == "" {
		return TopicView{}, errf(400, "link type and id are required")
	}
	if link.Relation == "" {
		link.Relation = "evidence"
	}
	for _, existing := range topic.Links {
		if existing.Type == link.Type && existing.ID == link.ID && existing.Relation == link.Relation {
			return h.topicViewLocked(topic), nil
		}
	}
	previous := cloneTopic(*topic)
	link.LinkedBy = strings.TrimSpace(actor)
	if link.LinkedBy == "" {
		link.LinkedBy = "owner"
	}
	link.CreatedAt = now()
	h.addTopicLinkMemoryLocked(topic, link)
	topic.Version++
	topic.UpdatedAt = link.CreatedAt
	h.appendTopicEventMemoryLocked(topic, TopicEvent{Type: "evidence_linked", Actor: link.LinkedBy, Summary: link.Label, Ref: &TopicRef{Type: link.Type, ID: link.ID, Label: link.Label}, CreatedAt: link.CreatedAt})
	if err := h.persistTopicsLocked(); err != nil {
		*topic = previous
		return TopicView{}, errf(500, "persist Topic: %s", err)
	}
	h.emitGlobalLocked("loom/topic-updated", map[string]any{"topic": cloneTopic(*topic)})
	return h.topicViewLocked(topic), nil
}

func (h *Hub) MarkTopicRead(id string) (TopicView, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	topic := h.topics[strings.TrimSpace(id)]
	if topic == nil {
		return TopicView{}, errf(404, "Topic not found: %s", id)
	}
	if topic.OwnerSeenBriefVersion < topic.ResultReadyVersion {
		previous := cloneTopic(*topic)
		topic.OwnerSeenBriefVersion = topic.ResultReadyVersion
		topic.UpdatedAt = now()
		if err := h.persistTopicsLocked(); err != nil {
			*topic = previous
			return TopicView{}, errf(500, "persist Topic read state: %s", err)
		}
	}
	return h.topicViewLocked(topic), nil
}

func (h *Hub) addTopicLinkMemoryLocked(topic *Topic, link TopicLink) {
	if topic == nil || link.Type == "" || link.ID == "" {
		return
	}
	for _, existing := range topic.Links {
		if existing.Type == link.Type && existing.ID == link.ID && existing.Relation == link.Relation {
			return
		}
	}
	topic.Links = append(topic.Links, link)
}

func (h *Hub) appendTopicEventMemoryLocked(topic *Topic, event TopicEvent) TopicEvent {
	if event.CreatedAt == "" {
		event.CreatedAt = now()
	}
	topic.NextEventSeq++
	event.Seq = topic.NextEventSeq
	topic.Events = append(topic.Events, event)
	if event.Ref != nil {
		h.addTopicLinkMemoryLocked(topic, TopicLink{Type: event.Ref.Type, ID: event.Ref.ID, Label: event.Ref.Label, Relation: "activity", LinkedBy: event.Actor, CreatedAt: event.CreatedAt})
	}
	return event
}

func (h *Hub) recordTopicWorkEventLocked(topicID string, event TopicEvent) {
	topic := h.topics[topicID]
	if topic == nil {
		return
	}
	previous := cloneTopic(*topic)
	stored := h.appendTopicEventMemoryLocked(topic, event)
	topic.UpdatedAt = stored.CreatedAt
	if err := h.persistTopicsLocked(); err != nil {
		*topic = previous
		log.Printf("[codex-loom] persist Topic event %s for %s: %v", event.Type, topicID, err)
		return
	}
	h.emitGlobalLocked("loom/topic-event", map[string]any{"topicId": topic.ID, "event": stored})
}

func (h *Hub) topicResponsibilityLocked(topic *Topic, agentID string) string {
	if agentID == topic.ResponsibleAgentID {
		return "Maintain the shared brief, route scoped work, manage waiting conditions, and close the Topic."
	}
	for _, participant := range topic.Participants {
		if participant.AgentID == agentID {
			return participant.Responsibility
		}
	}
	return ""
}

func (h *Hub) topicContextEnvelope(topicID, agentID string) (string, int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	topic := h.topics[topicID]
	if topic == nil {
		return "", 0, errf(404, "Topic not found: %s", topicID)
	}
	if !topicHasAgent(topic, agentID, agentID) {
		return "", 0, errf(403, "Agent is not part of Topic %s", topic.ID)
	}
	responsibility := h.topicResponsibilityLocked(topic, agentID)
	responsibleName := topic.ResponsibleAgent
	if responsible := h.agents[topic.ResponsibleAgentID]; responsible != nil {
		responsibleName = responsible.Name
	}
	cursor := topic.DeliveryCursors[agentID]
	deliveredCursor := cursor
	events := make([]TopicEvent, 0, 8)
	for _, event := range topic.Events {
		if event.Seq > cursor {
			deliveredCursor = event.Seq
			if agentID != topic.ResponsibleAgentID && strings.HasPrefix(event.Type, "participant_") && event.AgentID != agentID {
				continue
			}
			events = append(events, event)
			if len(events) == 8 {
				break
			}
		}
	}
	var b strings.Builder
	b.WriteString(`<loom_topic_context version="1" topic_id="` + xmlEscape(topic.ID) + `" status="` + xmlEscape(topic.Status) + `" brief_version="` + fmt.Sprint(topic.CurrentBrief.Version) + `" event_seq="` + fmt.Sprint(topic.NextEventSeq) + `">` + "\n")
	writeXMLText(&b, "title", topic.Title)
	writeXMLText(&b, "responsible_agent", responsibleName)
	writeXMLCDATA(&b, "purpose", topic.Purpose)
	writeXMLCDATA(&b, "completion_boundary", topic.CompletionBoundary)
	writeXMLCDATA(&b, "your_responsibility", responsibility)
	if agentID == topic.ResponsibleAgentID && len(topic.Participants) > 0 {
		b.WriteString("  <participants>\n")
		for _, participant := range topic.Participants {
			b.WriteString(`    <participant agent_id="` + xmlEscape(participant.AgentID) + `" name="` + xmlEscape(participant.Agent) + `">` + "\n")
			writeXMLCDATA(&b, "responsibility", participant.Responsibility)
			b.WriteString("    </participant>\n")
		}
		b.WriteString("  </participants>\n")
	}
	writeXMLCDATA(&b, "brief_summary", topic.CurrentBrief.Summary)
	if topic.CurrentBrief.CurrentState != "" {
		writeXMLCDATA(&b, "current_state", topic.CurrentBrief.CurrentState)
	}
	if topic.CurrentBrief.NextStep != "" {
		writeXMLCDATA(&b, "next_step", topic.CurrentBrief.NextStep)
	}
	if topic.CurrentBrief.Limitations != "" {
		writeXMLCDATA(&b, "limitations", topic.CurrentBrief.Limitations)
	}
	if topic.WaitingOn != nil {
		b.WriteString(`  <waiting_on kind="` + xmlEscape(topic.WaitingOn.Kind) + `" ref_id="` + xmlEscape(topic.WaitingOn.RefID) + `">` + "\n")
		writeXMLCDATA(&b, "summary", topic.WaitingOn.Summary)
		writeXMLCDATA(&b, "resume_action", topic.WaitingOn.ResumeAction)
		b.WriteString("  </waiting_on>\n")
	}
	links := make([]TopicLink, 0, 8)
	for index := len(topic.Links) - 1; index >= 0 && len(links) < 8; index-- {
		if topic.Links[index].Relation == "activity" {
			continue
		}
		links = append(links, topic.Links[index])
	}
	if len(links) > 0 {
		b.WriteString("  <key_links>\n")
		for index := len(links) - 1; index >= 0; index-- {
			link := links[index]
			b.WriteString(`    <link type="` + xmlEscape(link.Type) + `" id="` + xmlEscape(link.ID) + `" relation="` + xmlEscape(link.Relation) + `">` + xmlEscape(link.Label) + `</link>` + "\n")
		}
		b.WriteString("  </key_links>\n")
	}
	if len(events) > 0 {
		b.WriteString("  <delta>\n")
		for _, event := range events {
			b.WriteString(`    <event seq="` + fmt.Sprint(event.Seq) + `" type="` + xmlEscape(event.Type) + `" at="` + xmlEscape(event.CreatedAt) + `">` + xmlEscape(event.Summary) + `</event>` + "\n")
		}
		b.WriteString("  </delta>\n")
	}
	writeXMLText(&b, "instruction", "Work in your own Agent Thread. Keep high-resolution process here; return Topic-scoped results, limitations, context gaps, and evidence to the responsible Agent. Re-check provider facts before relying on them.")
	b.WriteString("</loom_topic_context>")
	return b.String(), deliveredCursor, nil
}

func (h *Hub) markTopicContextDelivered(topicID, agentID string, eventSeq int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	topic := h.topics[topicID]
	if topic == nil {
		return
	}
	if topic.DeliveryCursors == nil {
		topic.DeliveryCursors = map[string]int64{}
	}
	if eventSeq > topic.DeliveryCursors[agentID] {
		topic.DeliveryCursors[agentID] = eventSeq
		_ = h.persistTopicsLocked()
	}
}

func (h *Hub) formatAgentEnvelopeForDelivery(message *AgentMessage) (string, int64) {
	base := formatAgentEnvelope(message)
	if message == nil || message.TopicID == "" {
		return base, 0
	}
	context, cursor, err := h.topicContextEnvelope(message.TopicID, message.ToAgentID)
	if err != nil {
		return base, 0
	}
	return context + "\n" + base, cursor
}

func (h *Hub) agentMessageTurnInput(message *AgentMessage) (string, turnContextSource, int64) {
	if message == nil {
		return "", internalBusinessContext("internal_agent", "agent_message", "", "", ""), 0
	}
	originalInput := message.Body
	workContext := formatAgentEnvelopeContextAt(message, now())
	displayText := formatAgentEnvelope(message)
	origin := "internal_agent"
	kind := "agent_message"
	if message.TriggerID != "" && message.TriggerEvent != nil {
		origin = "external_trigger"
		kind = "trigger"
		workContext = formatTriggerEnvelopeContextAt(message, now())
		displayText = formatTriggerEnvelopeAt(message, now())
		if strings.TrimSpace(message.TriggerEvent.ResumeInstruction) != "" {
			originalInput = message.TriggerEvent.ResumeInstruction
		}
	} else if message.ScheduleID != "" {
		origin = "schedule"
		kind = "schedule"
	}
	cursor := int64(0)
	if message.TopicID != "" {
		if topicContext, topicCursor, err := h.topicContextEnvelope(message.TopicID, message.ToAgentID); err == nil {
			workContext = topicContext + "\n" + workContext
			displayText = topicContext + "\n" + displayText
			cursor = topicCursor
		}
	}
	source := internalBusinessContext(origin, kind, message.ID, message.TopicID, workContext)
	source.DisplayText = displayText
	return originalInput, source, cursor
}

func (h *Hub) SendTopicInput(id string, params TopicInputParams) (SendResult, error) {
	params.Text = strings.TrimSpace(params.Text)
	if params.Text == "" {
		return SendResult{}, errf(400, "text is required")
	}
	h.mu.Lock()
	topic := h.topics[strings.TrimSpace(id)]
	if topic == nil {
		h.mu.Unlock()
		return SendResult{}, errf(404, "Topic not found: %s", id)
	}
	target := h.resolveLocked(topic.ResponsibleAgentID)
	if target == nil {
		h.mu.Unlock()
		return SendResult{}, errf(409, "responsible Agent is unavailable")
	}
	topicID, targetID, targetName := topic.ID, target.ID, target.Name
	h.mu.Unlock()
	context, cursor, err := h.topicContextEnvelope(topicID, targetID)
	if err != nil {
		return SendResult{}, err
	}
	var b strings.Builder
	b.WriteString(context)
	b.WriteString("\n<owner_topic_input version=\"1\" topic_id=\"")
	b.WriteString(xmlEscape(topicID))
	b.WriteString("\">\n")
	b.WriteString("  <message source=\"original_input\" />\n")
	writeXMLText(&b, "instruction", "Treat this as Owner input for the Topic. If it changes scope, responsibility, or completion criteria, the responsible Agent must update and re-plan the shared Topic before downstream work continues.")
	b.WriteString("</owner_topic_input>")
	source := authenticatedOwnerContext("topic_input", topicID, topicID, b.String())
	result, err := h.sendTaskWithContext(targetID, params.Text, nil, time.Duration(params.TimeoutSec)*time.Second, "", "", "", topicID, summarizeTopicText(params.Text), source)
	if err != nil {
		return SendResult{}, err
	}
	h.markTopicContextDelivered(topicID, targetID, cursor)
	h.mu.Lock()
	h.recordTopicWorkEventLocked(topicID, TopicEvent{Type: "owner_input", Actor: "owner", AgentID: targetID, Agent: targetName, Summary: summarizeTopicText(params.Text), Ref: &TopicRef{Type: "turn", ID: result.TurnID, Label: targetName}, CreatedAt: now()})
	h.mu.Unlock()
	return result, nil
}

func summarizeTopicText(value string) string {
	value = cleanLegacyTopicText(value)
	runes := []rune(value)
	if len(runes) > 180 {
		return string(runes[:177]) + "..."
	}
	return value
}

func cleanLegacyTopicText(value string) string {
	value = strings.ReplaceAll(value, "\uFFFD", "")
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func (h *Hub) InterveneTopicTurn(id string, params TopicInterventionParams) (TopicInterventionResult, error) {
	id = strings.TrimSpace(id)
	params.Action = strings.ToLower(strings.TrimSpace(params.Action))
	params.Text = strings.TrimSpace(params.Text)
	params.Reason = strings.TrimSpace(params.Reason)
	if params.Action != "steer" && params.Action != "interrupt" {
		return TopicInterventionResult{}, errf(400, "action must be steer or interrupt")
	}
	if params.Action == "steer" && params.Text == "" {
		return TopicInterventionResult{}, errf(400, "text is required for steer")
	}
	h.mu.Lock()
	topic := h.topics[id]
	if topic == nil {
		h.mu.Unlock()
		return TopicInterventionResult{}, errf(404, "Topic not found: %s", id)
	}
	agent := h.resolveLocked(strings.TrimSpace(params.Agent))
	if agent == nil || !topicHasAgent(topic, agent.ID, params.Agent) {
		h.mu.Unlock()
		return TopicInterventionResult{}, errf(403, "Agent is not part of Topic %s", topic.ID)
	}
	rt := h.runtimes[agent.ID]
	if rt == nil || rt.activeTurn == nil || rt.activeTurn.finished || rt.activeTurn.topicID != topic.ID {
		h.mu.Unlock()
		return TopicInterventionResult{}, errf(409, "%s has no active Turn for Topic %s", agent.Name, topic.ID)
	}
	turnID, threadID, agentID, agentName, responsibleID := rt.activeTurn.turnID, agent.RuntimeBinding.NativeRef, agent.ID, agent.Name, topic.ResponsibleAgentID
	h.mu.Unlock()
	if params.Action == "steer" {
		var b strings.Builder
		b.WriteString(`<owner_topic_intervention version="1" topic_id="` + xmlEscape(id) + `" action="steer" turn_id="` + xmlEscape(turnID) + `">` + "\n")
		writeXMLCDATA(&b, "guidance", params.Text)
		if params.Reason != "" {
			writeXMLCDATA(&b, "reason", params.Reason)
		}
		writeXMLText(&b, "instruction", "This is a recorded correction to this active Turn, not a new Topic assignment. Continue within your scoped responsibility and return the resulting impact to the responsible Agent.")
		b.WriteString("</owner_topic_intervention>")
		if _, err := h.requestTurnSteer(rt, threadID, turnID, b.String(), 30*time.Second); err != nil {
			return TopicInterventionResult{}, errf(500, "steer active Turn: %s", err)
		}
	} else {
		reason := params.Reason
		if reason == "" {
			reason = "Topic Turn interrupted by Owner"
		}
		if _, err := h.Interrupt(agentID, reason); err != nil {
			return TopicInterventionResult{}, err
		}
	}
	summary := params.Reason
	if summary == "" {
		summary = params.Text
	}
	if summary == "" {
		summary = params.Action + " by Owner"
	}
	h.mu.Lock()
	topic = h.topics[id]
	if topic == nil {
		h.mu.Unlock()
		return TopicInterventionResult{}, errf(409, "Topic no longer exists: %s", id)
	}
	previous := cloneTopic(*topic)
	event := h.appendTopicEventMemoryLocked(topic, TopicEvent{Type: "owner_intervention", Actor: "owner", AgentID: agentID, Agent: agentName, Summary: params.Action + ": " + summarizeTopicText(summary), Ref: &TopicRef{Type: "turn", ID: turnID, Label: agentName}, CreatedAt: now()})
	topic.UpdatedAt = event.CreatedAt
	if err := h.persistTopicsLocked(); err != nil {
		*topic = previous
		h.mu.Unlock()
		return TopicInterventionResult{}, errf(500, "persist Topic intervention: %s", err)
	}
	h.emitGlobalLocked("loom/topic-event", map[string]any{"topicId": id, "event": event})
	h.mu.Unlock()
	if responsibleID != agentID {
		h.notifyTopicResponsible(id, responsibleID, agentName, turnID, params.Action, summary)
	}
	return TopicInterventionResult{TopicID: id, AgentID: agentID, Agent: agentName, TurnID: turnID, Action: params.Action, Event: event}, nil
}

func (h *Hub) notifyTopicResponsible(topicID, responsibleID, participant, turnID, action, summary string) {
	_, _ = h.SendAgentMessage(CommParams{From: topicIdentity, To: responsibleID, Subject: "Owner intervention in Topic participant Turn", Body: fmt.Sprintf("Owner performed %s on %s Turn %s. Reason/impact: %s. Re-plan the Topic if this changes scope, sequencing, or completion criteria.", action, participant, turnID, summary), Response: "none", TopicID: topicID, System: true, Timeout: time.Second})
}
