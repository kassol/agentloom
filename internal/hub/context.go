package hub

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/yan5xu/codex-loom/internal/rollout"
)

const (
	contextCoverageSchemaVersion  = 2
	builtinLoomAgentPromptVersion = 2
	maxDeveloperContextBytes      = 128 << 10
)

//go:embed prompts/loom-agent-prompt.md.tmpl
var builtinLoomAgentPrompt string

type LoomAgentPrompt struct {
	Content   string `json:"content"`
	Version   int    `json:"version"`
	Source    string `json:"source"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type LoomAgentPromptParams struct {
	Content         string `json:"content"`
	ExpectedVersion *int   `json:"expectedVersion,omitempty"`
}

type ContextEpoch struct {
	ID           string `json:"id"`
	WindowNumber int    `json:"windowNumber,omitempty"`
	CompactedAt  string `json:"compactedAt,omitempty"`
}

type ContextFragmentCoverage struct {
	Key       string `json:"key"`
	Revision  string `json:"revision"`
	Hash      string `json:"hash"`
	CoveredAt string `json:"coveredAt"`
	TurnID    string `json:"turnId"`
}

type ContextDeliveryFragment struct {
	Key      string `json:"key"`
	Revision string `json:"revision"`
	Hash     string `json:"hash"`
	Channel  string `json:"channel"`
}

type ContextDeliveryEvidence struct {
	Channel string `json:"channel"`
	Role    string `json:"role"`
	Marker  string `json:"marker"`
	Hash    string `json:"hash"`
}

type ContextDeliveryAttempt struct {
	ID                   string                    `json:"id"`
	EpochID              string                    `json:"epochId"`
	TurnID               string                    `json:"turnId,omitempty"`
	State                string                    `json:"state"`
	Fragments            []ContextDeliveryFragment `json:"fragments"`
	Deliveries           []ContextDeliveryEvidence `json:"deliveries"`
	PlannedAt            string                    `json:"plannedAt"`
	SubmittedAt          string                    `json:"submittedAt,omitempty"`
	ModelEventObservedAt string                    `json:"modelEventObservedAt,omitempty"`
	HistoryObservedAt    string                    `json:"historyObservedAt,omitempty"`
	CoveredAt            string                    `json:"coveredAt,omitempty"`
}

type ContextCoverageLedger struct {
	SchemaVersion int                                `json:"schemaVersion"`
	AgentID       string                             `json:"agentId"`
	ThreadID      string                             `json:"threadId"`
	Epoch         ContextEpoch                       `json:"epoch"`
	Covered       map[string]ContextFragmentCoverage `json:"covered"`
	Pending       *ContextDeliveryAttempt            `json:"pending,omitempty"`
	UpdatedAt     string                             `json:"updatedAt"`
}

type ContextSourceView struct {
	Key      string `json:"key"`
	Revision string `json:"revision"`
	Hash     string `json:"hash"`
	Channel  string `json:"channel"`
	Covered  bool   `json:"covered"`
}

type ContextExplainView struct {
	AgentID    string                  `json:"agentId"`
	AgentName  string                  `json:"agentName"`
	ThreadID   string                  `json:"threadId"`
	Epoch      ContextEpoch            `json:"epoch"`
	Sources    []ContextSourceView     `json:"sources"`
	Pending    *ContextDeliveryAttempt `json:"pending,omitempty"`
	Policy     string                  `json:"policy"`
	Limitation string                  `json:"limitation"`
}

type turnContextSource struct {
	Origin      string
	Trust       string
	Authority   string
	Kind        string
	RefID       string
	TopicID     string
	WorkContext string
	DisplayText string
}

type contextHistoryProbeFunc func(threadID string, query rollout.ContextHistoryQuery) (rollout.ContextHistoryState, error)

type contextFragment struct {
	ContextDeliveryFragment
	XML string
}

type TurnContextPlan struct {
	DeveloperContext string
	InputContext     string
	Attempt          *ContextDeliveryAttempt
}

type loomAgentPromptTemplateData struct {
	AgentProfileRevisionXMLAttr string
	AgentIDXMLAttr              string
	AgentNameXMLAttr            string
	ProfileIdentityCDATA        string
	ProfileDomainCDATA          string
	ProfileScopeCDATA           string
}

func authenticatedOwnerContext(kind, refID, topicID, workContext string) turnContextSource {
	return turnContextSource{
		Origin: "owner", Trust: "authenticated", Authority: "current_intent",
		Kind: kind, RefID: refID, TopicID: topicID, WorkContext: workContext,
	}
}

func internalBusinessContext(origin, kind, refID, topicID, workContext string) turnContextSource {
	return turnContextSource{
		Origin: origin, Trust: "loom_managed", Authority: "business_input",
		Kind: kind, RefID: refID, TopicID: topicID, WorkContext: workContext,
	}
}

func externalBusinessContext(kind, refID, workContext string) turnContextSource {
	return turnContextSource{
		Origin: "external_connector", Trust: "managed_external", Authority: "business_input",
		Kind: kind, RefID: refID, WorkContext: workContext,
	}
}

func (h *Hub) inferTurnContextSource(inboxItemID, agentMessageID, topicID, workContext string) turnContextSource {
	if inboxItemID != "" {
		return externalBusinessContext("inbox_message", inboxItemID, workContext)
	}
	if agentMessageID != "" {
		h.mu.Lock()
		message := h.comms[agentMessageID]
		var scheduleID, triggerID string
		if message != nil {
			scheduleID, triggerID = message.ScheduleID, message.TriggerID
			if topicID == "" {
				topicID = message.TopicID
			}
		}
		h.mu.Unlock()
		switch {
		case triggerID != "":
			return internalBusinessContext("external_trigger", "trigger", agentMessageID, topicID, workContext)
		case scheduleID != "":
			return internalBusinessContext("schedule", "schedule", agentMessageID, topicID, workContext)
		default:
			return internalBusinessContext("internal_agent", "agent_message", agentMessageID, topicID, workContext)
		}
	}
	return authenticatedOwnerContext("direct_input", "", topicID, workContext)
}

func (h *Hub) loadLoomAgentPrompt() error {
	var prompt LoomAgentPrompt
	if err := h.st.LoadLoomAgentPrompt(&prompt); err != nil {
		return err
	}
	if strings.TrimSpace(prompt.Content) == "" || prompt.Version <= 0 {
		h.loomAgentPrompt = nil
		return nil
	}
	if err := validateLoomAgentPromptTemplate(prompt.Content); err != nil {
		return fmt.Errorf("load Loom Agent Prompt template: %w", err)
	}
	prompt.Source = "owner"
	h.loomAgentPrompt = &prompt
	return nil
}

func (h *Hub) effectiveLoomAgentPromptLocked() LoomAgentPrompt {
	if h.loomAgentPrompt != nil {
		prompt := *h.loomAgentPrompt
		prompt.Source = "owner"
		return prompt
	}
	return LoomAgentPrompt{
		Content: strings.TrimSpace(builtinLoomAgentPrompt), Version: builtinLoomAgentPromptVersion,
		Source: "builtin",
	}
}

func (h *Hub) GetLoomAgentPrompt() LoomAgentPrompt {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.effectiveLoomAgentPromptLocked()
}

func (h *Hub) UpdateLoomAgentPrompt(params LoomAgentPromptParams) (LoomAgentPrompt, error) {
	content := strings.TrimSpace(params.Content)
	if content == "" {
		return LoomAgentPrompt{}, errf(400, "content is required")
	}
	if len(content) > 64<<10 {
		return LoomAgentPrompt{}, errf(400, "Loom Agent Prompt must be at most 64 KiB")
	}
	if err := validateLoomAgentPromptTemplate(content); err != nil {
		return LoomAgentPrompt{}, errf(400, "invalid Loom Agent Prompt template: %s", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	currentVersion := h.effectiveLoomAgentPromptLocked().Version
	if params.ExpectedVersion != nil && *params.ExpectedVersion != currentVersion {
		return LoomAgentPrompt{}, errf(409, "Loom Agent Prompt version changed: expected %d, current %d", *params.ExpectedVersion, currentVersion)
	}
	if h.loomAgentPrompt != nil && h.loomAgentPrompt.Content == content {
		return *h.loomAgentPrompt, nil
	}
	prompt := &LoomAgentPrompt{
		Content: content, Version: currentVersion + 1, Source: "owner", UpdatedAt: now(),
	}
	if err := h.st.SaveLoomAgentPrompt(prompt); err != nil {
		return LoomAgentPrompt{}, errf(500, "save Loom Agent Prompt: %s", err)
	}
	h.loomAgentPrompt = prompt
	h.emitGlobalLocked("loom/agent-prompt-updated", map[string]any{"prompt": prompt})
	return *prompt, nil
}

func (h *Hub) ClearLoomAgentPrompt(expectedVersion *int) (LoomAgentPrompt, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	currentVersion := h.effectiveLoomAgentPromptLocked().Version
	if expectedVersion != nil && *expectedVersion != currentVersion {
		return LoomAgentPrompt{}, errf(409, "Loom Agent Prompt version changed: expected %d, current %d", *expectedVersion, currentVersion)
	}
	if err := h.st.DeleteLoomAgentPrompt(); err != nil {
		return LoomAgentPrompt{}, errf(500, "clear Loom Agent Prompt: %s", err)
	}
	h.loomAgentPrompt = nil
	prompt := h.effectiveLoomAgentPromptLocked()
	h.emitGlobalLocked("loom/agent-prompt-cleared", map[string]any{"prompt": prompt})
	return prompt, nil
}

func (h *Hub) currentContextFragments(agentID string, compiledAt string) ([]contextFragment, string, string, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	agent := h.agents[agentID]
	if agent == nil {
		return nil, "", "", "", errf(404, "agent vanished")
	}
	prompt := h.effectiveLoomAgentPromptLocked()
	profile := AgentProfile{AgentID: agentID}
	if current := h.profiles[agentID]; current != nil {
		profile = *current
	}
	relationships := h.directRelationshipSnapshotLocked(agentID)

	promptRevision := fmt.Sprintf("%s:%d", prompt.Source, prompt.Version)
	profileRevision := fmt.Sprintf("profile:%d", profile.Version)
	profileXML := renderLoomAgentProfileData(*agent, profile, profileRevision)
	developerPayload, err := renderLoomAgentPromptTemplate(prompt.Content, *agent, profile, profileRevision)
	if err != nil {
		return nil, "", "", "", errf(500, "render Loom Agent Prompt template: %s", err)
	}
	relationshipJSON, _ := json.Marshal(relationships)
	relationshipHash := sha256Hex(relationshipJSON)
	relationshipRevision := "relationships:" + relationshipHash[:16]
	relationshipXML := renderLoomRelationshipsFragment(agentID, relationships, relationshipRevision, relationshipHash, compiledAt)

	return []contextFragment{
		newContextFragment("loom_agent_prompt", promptRevision, "developer", prompt.Content),
		newContextFragment("loom_agent_profile", profileRevision, "developer", profileXML),
		{
			ContextDeliveryFragment: ContextDeliveryFragment{
				Key: "loom_agent_relationships", Revision: relationshipRevision, Hash: relationshipHash,
				Channel: "input",
			},
			XML: relationshipXML,
		},
	}, agent.RuntimeBinding.NativeRef, agent.Name, developerPayload, nil
}

type directRelationshipSnapshot struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Direction   string `json:"direction"`
	Counterpart string `json:"counterpart"`
	Name        string `json:"name"`
	Description string `json:"description"`
	UpdatedAt   string `json:"updatedAt"`
}

func (h *Hub) directRelationshipSnapshotLocked(agentID string) []directRelationshipSnapshot {
	out := []directRelationshipSnapshot{}
	for _, relationship := range h.organizationLinks {
		var direction, counterpart, name string
		switch agentID {
		case relationship.ParentAgentID:
			direction, counterpart = "parent_to_child", relationship.ChildAgentID
			name = h.agentNameLocked(counterpart, relationship.Child)
		case relationship.ChildAgentID:
			direction, counterpart = "child_to_parent", relationship.ParentAgentID
			name = h.agentNameLocked(counterpart, relationship.Parent)
		default:
			continue
		}
		out = append(out, directRelationshipSnapshot{
			ID: relationship.ID, Type: "organization", Direction: direction,
			Counterpart: counterpart, Name: name, Description: relationship.Description,
			UpdatedAt: relationship.UpdatedAt,
		})
	}
	for _, relationship := range h.teamLinks {
		var direction, counterpart, name string
		switch agentID {
		case relationship.FromAgentID:
			direction, counterpart = "outgoing", relationship.ToAgentID
			name = h.agentNameLocked(counterpart, relationship.To)
		case relationship.ToAgentID:
			direction, counterpart = "incoming", relationship.FromAgentID
			name = h.agentNameLocked(counterpart, relationship.From)
		default:
			continue
		}
		out = append(out, directRelationshipSnapshot{
			ID: relationship.ID, Type: "collaboration", Direction: direction,
			Counterpart: counterpart, Name: name, Description: relationship.Description,
			UpdatedAt: relationship.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func newContextFragment(key, revision, channel, xml string) contextFragment {
	return contextFragment{
		ContextDeliveryFragment: ContextDeliveryFragment{
			Key: key, Revision: revision, Hash: sha256Hex([]byte(xml)), Channel: channel,
		},
		XML: xml,
	}
}

func (h *Hub) prepareTurnContext(agentID string, source turnContextSource, artifacts []ThreadArtifact) (TurnContextPlan, error) {
	compiledAt := now()
	fragments, threadID, _, developerPayload, err := h.currentContextFragments(agentID, compiledAt)
	if err != nil {
		return TurnContextPlan{}, err
	}
	h.mu.Lock()
	runtimeKind := ""
	if agent := h.agents[agentID]; agent != nil {
		runtimeKind = agent.RuntimeBinding.Kind
	}
	h.mu.Unlock()
	if runtimeKind == "pi" {
		developerContext := renderPiDeveloperContext(compiledAt, developerPayload)
		if len(developerContext) > maxDeveloperContextBytes {
			return TurnContextPlan{}, errf(409, "compiled Loom Developer context is %d bytes; maximum is %d bytes and it will not be truncated", len(developerContext), maxDeveloperContextBytes)
		}
		return TurnContextPlan{
			DeveloperContext: developerContext,
			InputContext:     renderLoomInputContext(compiledAt, ContextEpoch{}, source, contextFragmentsForChannel(fragments, fragments, "input"), artifacts, nil),
		}, nil
	}
	h.contextCoverageMu.Lock()
	defer h.contextCoverageMu.Unlock()
	history, err := h.contextHistory(threadID, rollout.ContextHistoryQuery{})
	if err != nil {
		return TurnContextPlan{}, errf(500, "read context epoch: %s", err)
	}
	ledger, err := h.loadContextCoverage(agentID, threadID, history, true)
	if err != nil {
		return TurnContextPlan{}, err
	}
	if err := h.reconcilePendingContext(&ledger); err != nil {
		return TurnContextPlan{}, err
	}

	missing := make([]contextFragment, 0, len(fragments))
	developerMissing := false
	for _, fragment := range fragments {
		covered, ok := ledger.Covered[fragment.Key]
		if !ok || covered.Revision != fragment.Revision || covered.Hash != fragment.Hash {
			missing = append(missing, fragment)
			developerMissing = developerMissing || fragment.Channel == "developer"
		}
	}
	// Loom Agent Prompt and Agent Profile are one atomic Developer delivery.
	// A change to either source recompiles and re-delivers the complete pair.
	if developerMissing {
		missing = missing[:0]
		for _, fragment := range fragments {
			if fragment.Channel == "developer" {
				missing = append(missing, fragment)
				continue
			}
			covered, ok := ledger.Covered[fragment.Key]
			if !ok || covered.Revision != fragment.Revision || covered.Hash != fragment.Hash {
				missing = append(missing, fragment)
			}
		}
	}

	var attempt *ContextDeliveryAttempt
	if len(missing) > 0 {
		attempt = &ContextDeliveryAttempt{
			ID: newContextAttemptID(), EpochID: ledger.Epoch.ID, State: "planned",
			PlannedAt: now(), Fragments: make([]ContextDeliveryFragment, 0, len(missing)),
		}
		for _, fragment := range missing {
			attempt.Fragments = append(attempt.Fragments, fragment.ContextDeliveryFragment)
		}
	}

	developerFragments := contextFragmentsForChannel(fragments, missing, "developer")
	inputFragments := contextFragmentsForChannel(fragments, missing, "input")
	developerContext := renderLoomDeveloperContext(compiledAt, ledger.Epoch, developerPayload, developerFragments, attempt)
	if len(developerContext) > maxDeveloperContextBytes {
		return TurnContextPlan{}, errf(409, "compiled Loom Developer context is %d bytes; maximum is %d bytes and it will not be truncated", len(developerContext), maxDeveloperContextBytes)
	}
	inputContext := renderLoomInputContext(compiledAt, ledger.Epoch, source, inputFragments, artifacts, attempt)

	if attempt != nil {
		if developerContext != "" {
			attempt.Deliveries = append(attempt.Deliveries, newContextDeliveryEvidence(attempt.ID, "developer", developerContext))
		}
		if inputContext != "" && len(inputFragments) > 0 {
			attempt.Deliveries = append(attempt.Deliveries, newContextDeliveryEvidence(attempt.ID, "input", inputContext))
		}
		ledger.Pending = cloneContextAttempt(attempt)
		ledger.UpdatedAt = now()
		if err := h.st.SaveContextCoverage(threadID, ledger); err != nil {
			return TurnContextPlan{}, errf(500, "save planned context coverage: %s", err)
		}
	}
	return TurnContextPlan{
		DeveloperContext: developerContext,
		InputContext:     inputContext,
		Attempt:          cloneContextAttempt(attempt),
	}, nil
}

func renderPiDeveloperContext(compiledAt, payload string) string {
	if strings.TrimSpace(payload) == "" {
		return ""
	}
	return `<loom_developer_context version="1" compiled_at="` + xmlEscape(compiledAt) + `" complete="true" atomic="true">` + "\n" + payload + "\n</loom_developer_context>"
}

func contextFragmentsForChannel(all, selected []contextFragment, channel string) []contextFragment {
	selectedKeys := make(map[string]bool, len(selected))
	for _, fragment := range selected {
		selectedKeys[fragment.Key] = true
	}
	out := make([]contextFragment, 0, len(all))
	for _, fragment := range all {
		if fragment.Channel == channel && selectedKeys[fragment.Key] {
			out = append(out, fragment)
		}
	}
	return out
}

func cloneContextAttempt(value *ContextDeliveryAttempt) *ContextDeliveryAttempt {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Fragments = append([]ContextDeliveryFragment(nil), value.Fragments...)
	copy.Deliveries = append([]ContextDeliveryEvidence(nil), value.Deliveries...)
	return &copy
}

func newContextDeliveryEvidence(attemptID, channel, payload string) ContextDeliveryEvidence {
	role := "user"
	if channel == "developer" {
		role = "developer"
	}
	return ContextDeliveryEvidence{
		Channel: channel,
		Role:    role,
		Marker:  contextDeliveryMarker(attemptID, channel),
		Hash:    sha256Hex([]byte(payload)),
	}
}

func contextDeliveryMarker(attemptID, channel string) string {
	return `delivery_id="` + attemptID + `:` + channel + `"`
}

func (h *Hub) loadContextCoverage(agentID, threadID string, history rollout.ContextHistoryState, persistReset bool) (ContextCoverageLedger, error) {
	ledger := ContextCoverageLedger{}
	if err := h.st.LoadContextCoverage(threadID, &ledger); err != nil {
		return ledger, errf(500, "load context coverage: %s", err)
	}
	if ledger.SchemaVersion != contextCoverageSchemaVersion || ledger.ThreadID != threadID || ledger.Epoch.ID != history.EpochID {
		ledger = ContextCoverageLedger{
			SchemaVersion: contextCoverageSchemaVersion,
			AgentID:       agentID, ThreadID: threadID,
			Epoch: ContextEpoch{
				ID: history.EpochID, WindowNumber: history.WindowNumber, CompactedAt: history.CompactedAt,
			},
			Covered: map[string]ContextFragmentCoverage{}, UpdatedAt: now(),
		}
		if persistReset {
			if err := h.st.SaveContextCoverage(threadID, ledger); err != nil {
				return ledger, errf(500, "reset context coverage epoch: %s", err)
			}
		}
	}
	if ledger.Covered == nil {
		ledger.Covered = map[string]ContextFragmentCoverage{}
	}
	return ledger, nil
}

func (h *Hub) reconcilePendingContext(ledger *ContextCoverageLedger) error {
	if ledger == nil || ledger.Pending == nil || ledger.Pending.EpochID != ledger.Epoch.ID {
		return nil
	}
	pending := ledger.Pending
	if pending.ModelEventObservedAt == "" || pending.TurnID == "" {
		return nil
	}
	query := rollout.ContextHistoryQuery{
		TurnID:     pending.TurnID,
		Deliveries: make([]rollout.ContextDeliveryProbe, 0, len(pending.Deliveries)),
	}
	for _, delivery := range pending.Deliveries {
		query.Deliveries = append(query.Deliveries, rollout.ContextDeliveryProbe{
			Role: delivery.Role, Marker: delivery.Marker, Hash: delivery.Hash,
		})
	}
	history, err := h.contextHistory(ledger.ThreadID, query)
	if err != nil {
		return errf(500, "verify replayable context deliveries: %s", err)
	}
	if history.EpochID != pending.EpochID || !history.DeliveriesPersisted {
		return nil
	}
	timestamp := now()
	pending.HistoryObservedAt = timestamp
	pending.CoveredAt = timestamp
	pending.State = "covered"
	for _, fragment := range pending.Fragments {
		ledger.Covered[fragment.Key] = ContextFragmentCoverage{
			Key: fragment.Key, Revision: fragment.Revision, Hash: fragment.Hash,
			CoveredAt: timestamp, TurnID: pending.TurnID,
		}
	}
	ledger.Pending = nil
	ledger.UpdatedAt = timestamp
	return h.st.SaveContextCoverage(ledger.ThreadID, ledger)
}

func (h *Hub) markContextSubmitted(threadID string, attempt *ContextDeliveryAttempt, turnID string) {
	if attempt == nil || strings.TrimSpace(threadID) == "" {
		return
	}
	h.contextCoverageMu.Lock()
	defer h.contextCoverageMu.Unlock()
	var ledger ContextCoverageLedger
	if err := h.st.LoadContextCoverage(threadID, &ledger); err != nil || ledger.Pending == nil || ledger.Pending.ID != attempt.ID {
		return
	}
	ledger.Pending.TurnID = turnID
	if ledger.Pending.ModelEventObservedAt == "" {
		ledger.Pending.State = "submitted"
	}
	ledger.Pending.SubmittedAt = now()
	ledger.UpdatedAt = ledger.Pending.SubmittedAt
	if err := h.st.SaveContextCoverage(threadID, ledger); err != nil {
		return
	}
	attempt.TurnID = turnID
	if attempt.ModelEventObservedAt == "" {
		attempt.State = "submitted"
	}
	attempt.SubmittedAt = ledger.Pending.SubmittedAt
}

func (h *Hub) rebindContextAttemptTurn(threadID, attemptID, previousTurnID, turnID string) {
	if attemptID == "" || turnID == "" {
		return
	}
	h.contextCoverageMu.Lock()
	defer h.contextCoverageMu.Unlock()
	var ledger ContextCoverageLedger
	if err := h.st.LoadContextCoverage(threadID, &ledger); err != nil || ledger.Pending == nil || ledger.Pending.ID != attemptID {
		return
	}
	if ledger.Pending.TurnID != "" && ledger.Pending.TurnID != previousTurnID {
		return
	}
	ledger.Pending.TurnID = turnID
	ledger.UpdatedAt = now()
	_ = h.st.SaveContextCoverage(threadID, ledger)
}

func (h *Hub) observeContextModelEventLocked(meta *Agent, turn *turnState) {
	if meta == nil || turn == nil || turn.contextAttemptID == "" {
		return
	}
	h.contextCoverageMu.Lock()
	defer h.contextCoverageMu.Unlock()
	var ledger ContextCoverageLedger
	if err := h.st.LoadContextCoverage(meta.RuntimeBinding.NativeRef, &ledger); err != nil || ledger.Pending == nil {
		return
	}
	if ledger.Pending.ID != turn.contextAttemptID || ledger.Pending.EpochID != turn.contextEpochID {
		return
	}
	if ledger.Pending.TurnID == "" {
		ledger.Pending.TurnID = turn.turnID
	}
	observedNow := false
	if ledger.Pending.ModelEventObservedAt == "" {
		ledger.Pending.ModelEventObservedAt = now()
		ledger.Pending.State = "model_observed"
		ledger.UpdatedAt = ledger.Pending.ModelEventObservedAt
		if err := h.st.SaveContextCoverage(meta.RuntimeBinding.NativeRef, ledger); err != nil {
			return
		}
		observedNow = true
	}
	if observedNow {
		_ = h.reconcilePendingContext(&ledger)
	}
}

func isModelProducedNotification(method string) bool {
	return method == "item/agentMessage/delta" ||
		strings.HasPrefix(method, "item/reasoning/") ||
		method == "item/reasoning/delta"
}

func (h *Hub) ContextCoverage(key string) (ContextCoverageLedger, error) {
	h.mu.Lock()
	agent := h.resolveLocked(strings.TrimSpace(key))
	if agent == nil {
		h.mu.Unlock()
		return ContextCoverageLedger{}, errf(404, "agent not found: %s", key)
	}
	agentID, threadID, runtimeRef := agent.ID, agent.ThreadID, agent.RuntimeBinding.NativeRef
	h.mu.Unlock()
	h.contextCoverageMu.Lock()
	defer h.contextCoverageMu.Unlock()
	history, err := h.contextHistory(runtimeRef, rollout.ContextHistoryQuery{})
	if err != nil {
		return ContextCoverageLedger{}, err
	}
	ledger, err := h.loadContextCoverage(agentID, runtimeRef, history, false)
	ledger.ThreadID = threadID
	return ledger, err
}

func (h *Hub) ExplainContext(key string) (ContextExplainView, error) {
	h.mu.Lock()
	agent := h.resolveLocked(strings.TrimSpace(key))
	if agent == nil {
		h.mu.Unlock()
		return ContextExplainView{}, errf(404, "agent not found: %s", key)
	}
	agentID, agentName, threadID := agent.ID, agent.Name, agent.ThreadID
	h.mu.Unlock()
	compiledAt := now()
	fragments, _, _, _, err := h.currentContextFragments(agentID, compiledAt)
	if err != nil {
		return ContextExplainView{}, err
	}
	ledger, err := h.ContextCoverage(agentID)
	if err != nil {
		return ContextExplainView{}, err
	}
	view := ContextExplainView{
		AgentID: agentID, AgentName: agentName, ThreadID: threadID, Epoch: ledger.Epoch,
		Pending:    ledger.Pending,
		Policy:     "The Loom Agent Prompt template and complete Agent Profile snapshot are rendered as one atomic Developer message while retaining separate logical coverage. Other durable and bounded context is delivered through Turn input.",
		Limitation: "A compaction during an active Turn is recovered at the next Turn start; this version does not restore context inside the same Turn.",
	}
	for _, fragment := range fragments {
		covered := ledger.Covered[fragment.Key]
		view.Sources = append(view.Sources, ContextSourceView{
			Key: fragment.Key, Revision: fragment.Revision, Hash: fragment.Hash, Channel: fragment.Channel,
			Covered: covered.Revision == fragment.Revision && covered.Hash == fragment.Hash,
		})
	}
	return view, nil
}

func (h *Hub) contextHistory(threadID string, query rollout.ContextHistoryQuery) (rollout.ContextHistoryState, error) {
	if h.contextHistoryProbe != nil {
		return h.contextHistoryProbe(threadID, query)
	}
	return rollout.ContextHistory(threadID, query)
}

func renderLoomAgentProfileData(agent Agent, profile AgentProfile, revision string) string {
	var b strings.Builder
	b.WriteString(`<loom_agent_profile_data version="1"` + "\n")
	b.WriteString(`  revision="` + xmlEscape(revision) + `"` + "\n")
	b.WriteString(`  agent_id="` + xmlEscape(agent.ID) + `"` + "\n")
	b.WriteString(`  name="` + xmlEscape(agent.Name) + `"` + "\n")
	b.WriteString(`  complete="true"` + "\n")
	b.WriteString(`  supersedes_previous="true"` + "\n")
	b.WriteString(`  declarative_not_authorization="true">` + "\n")
	writeXMLCDATA(&b, "identity", profile.Identity)
	writeXMLCDATA(&b, "domain", profile.Domain)
	writeXMLCDATA(&b, "scope", profile.Scope)
	b.WriteString("</loom_agent_profile_data>")
	return b.String()
}

func renderLoomAgentPromptTemplate(content string, agent Agent, profile AgentProfile, profileRevision string) (string, error) {
	tmpl, err := template.New("loom-agent-prompt").Option("missingkey=error").Parse(content)
	if err != nil {
		return "", err
	}
	data := loomAgentPromptTemplateData{
		AgentProfileRevisionXMLAttr: xmlEscape(profileRevision),
		AgentIDXMLAttr:              xmlEscape(agent.ID),
		AgentNameXMLAttr:            xmlEscape(agent.Name),
		ProfileIdentityCDATA:        xmlCDATA(profile.Identity),
		ProfileDomainCDATA:          xmlCDATA(profile.Domain),
		ProfileScopeCDATA:           xmlCDATA(profile.Scope),
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		return "", err
	}
	return strings.TrimSpace(rendered.String()), nil
}

func validateLoomAgentPromptTemplate(content string) error {
	const (
		identity = "PROFILE-IDENTITY"
		domain   = "PROFILE-DOMAIN"
		scope    = "PROFILE-SCOPE"
	)
	rendered, err := renderLoomAgentPromptTemplate(content, Agent{
		ID: "agent-template", Name: "template-agent",
	}, AgentProfile{
		Identity: identity, Domain: domain, Scope: scope,
	}, "profile:7")
	if err != nil {
		return err
	}
	const closingTag = "</loom_agent_profile_data>"
	start := strings.Index(rendered, "<loom_agent_profile_data ")
	end := strings.Index(rendered, closingTag)
	if start < 0 || end < start ||
		strings.Count(rendered, "<loom_agent_profile_data ") != 1 ||
		strings.Count(rendered, closingTag) != 1 {
		return fmt.Errorf("template must render exactly one loom_agent_profile_data block")
	}
	end += len(closingTag)
	var snapshot struct {
		XMLName                     xml.Name `xml:"loom_agent_profile_data"`
		Version                     string   `xml:"version,attr"`
		Revision                    string   `xml:"revision,attr"`
		AgentID                     string   `xml:"agent_id,attr"`
		Name                        string   `xml:"name,attr"`
		Complete                    string   `xml:"complete,attr"`
		SupersedesPrevious          string   `xml:"supersedes_previous,attr"`
		DeclarativeNotAuthorization string   `xml:"declarative_not_authorization,attr"`
		Identity                    string   `xml:"identity"`
		Domain                      string   `xml:"domain"`
		Scope                       string   `xml:"scope"`
	}
	if err := xml.Unmarshal([]byte(rendered[start:end]), &snapshot); err != nil {
		return fmt.Errorf("invalid loom_agent_profile_data block: %w", err)
	}
	if snapshot.Version != "1" || snapshot.Revision != "profile:7" ||
		snapshot.AgentID != "agent-template" || snapshot.Name != "template-agent" ||
		snapshot.Complete != "true" || snapshot.SupersedesPrevious != "true" ||
		snapshot.DeclarativeNotAuthorization != "true" ||
		snapshot.Identity != identity || snapshot.Domain != domain || snapshot.Scope != scope {
		return fmt.Errorf("loom_agent_profile_data must preserve the complete versioned Profile snapshot")
	}
	return nil
}

func xmlCDATA(value string) string {
	return strings.ReplaceAll(value, "]]>", "]]]]><![CDATA[>")
}

func renderLoomRelationshipsFragment(agentID string, relationships []directRelationshipSnapshot, revision, hash, compiledAt string) string {
	var b strings.Builder
	b.WriteString(`<loom_agent_relationships version="1" agent_id="` + xmlEscape(agentID) + `" revision="` + xmlEscape(revision) + `" hash="` + xmlEscape(hash) + `" as_of="` + xmlEscape(compiledAt) + `" complete="true" supersedes_previous="true" declarative_not_authorization="true" scope="direct_active_organization_and_collaboration">` + "\n")
	b.WriteString("  <relationships>\n")
	for _, relationship := range relationships {
		b.WriteString(`    <relationship id="` + xmlEscape(relationship.ID) + `" type="` + xmlEscape(relationship.Type) + `" direction="` + xmlEscape(relationship.Direction) + `" counterpart_agent_id="` + xmlEscape(relationship.Counterpart) + `" counterpart_name="` + xmlEscape(relationship.Name) + `" updated_at="` + xmlEscape(relationship.UpdatedAt) + `">` + "\n")
		b.WriteString("      <description><![CDATA[")
		b.WriteString(strings.ReplaceAll(relationship.Description, "]]>", "]]]]><![CDATA[>"))
		b.WriteString("]]></description>\n")
		b.WriteString("    </relationship>\n")
	}
	b.WriteString("  </relationships>\n")
	b.WriteString("  <scope_note>This complete snapshot covers only the Agent's direct active Organization and Collaboration relationships. It does not describe Topic participation, Conversation Membership, indirect organization, Collaboration Groups, Activity, ACLs, credentials, tools, or production authority.</scope_note>\n")
	b.WriteString("</loom_agent_relationships>")
	return b.String()
}

func renderLoomDeveloperContext(compiledAt string, epoch ContextEpoch, payload string, fragments []contextFragment, attempt *ContextDeliveryAttempt) string {
	if len(fragments) == 0 || attempt == nil || strings.TrimSpace(payload) == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<loom_developer_context version="1" compiled_at="` + xmlEscape(compiledAt) + `" epoch_id="` + xmlEscape(epoch.ID) + `" ` + contextDeliveryMarker(attempt.ID, "developer") + ` complete="true" atomic="true"`)
	for _, fragment := range fragments {
		switch fragment.Key {
		case "loom_agent_prompt":
			writeXMLAttribute(&b, "prompt_revision", fragment.Revision)
		case "loom_agent_profile":
			writeXMLAttribute(&b, "profile_revision", fragment.Revision)
		}
	}
	b.WriteString(">\n")
	b.WriteString(payload)
	b.WriteByte('\n')
	b.WriteString("</loom_developer_context>")
	return b.String()
}

func renderLoomInputContext(compiledAt string, epoch ContextEpoch, source turnContextSource, fragments []contextFragment, artifacts []ThreadArtifact, attempt *ContextDeliveryAttempt) string {
	includeTurnContext := source.Origin != "owner" || source.Kind != "direct_input" ||
		source.RefID != "" || source.TopicID != "" || strings.TrimSpace(source.WorkContext) != "" ||
		len(artifacts) > 0
	if len(fragments) == 0 && !includeTurnContext {
		return ""
	}

	var b strings.Builder
	b.WriteString(`<loom_context version="1" compiled_at="` + xmlEscape(compiledAt) + `"`)
	if epoch.ID != "" {
		b.WriteString(` epoch_id="` + xmlEscape(epoch.ID) + `"`)
	}
	if len(fragments) > 0 && attempt != nil {
		b.WriteByte(' ')
		b.WriteString(contextDeliveryMarker(attempt.ID, "input"))
	}
	b.WriteString(">\n")
	for _, fragment := range fragments {
		b.WriteString(fragment.XML)
		b.WriteByte('\n')
	}
	if includeTurnContext {
		b.WriteString(`  <loom_turn_context origin="` + xmlEscape(source.Origin) + `" trust="` + xmlEscape(source.Trust) + `" authority="` + xmlEscape(source.Authority) + `" kind="` + xmlEscape(source.Kind) + `"`)
		writeXMLAttribute(&b, "ref_id", source.RefID)
		writeXMLAttribute(&b, "topic_id", source.TopicID)
		b.WriteString(">\n")
		b.WriteString("    <original_input location=\"preceding_turn_input_item\" />\n")
		if strings.TrimSpace(source.WorkContext) != "" {
			writeXMLCDATAIndent(&b, "payload", source.WorkContext, "    ")
		}
		if len(artifacts) > 0 {
			b.WriteString("    <attachments>\n")
			for _, artifact := range artifacts {
				b.WriteString(`      <attachment id="` + xmlEscape(artifact.ID) + `" name="` + xmlEscape(artifact.Name) + `" mime_type="` + xmlEscape(artifact.MimeType) + `" size="` + fmt.Sprint(artifact.Size) + `" path="` + xmlEscape(artifact.Path) + `" url="` + xmlEscape(artifact.URL) + `" />` + "\n")
			}
			b.WriteString("    </attachments>\n")
		}
		b.WriteString("  </loom_turn_context>\n")
	}
	b.WriteString("</loom_context>")
	return b.String()
}

func newContextAttemptID() string {
	value := make([]byte, 8)
	if _, err := rand.Read(value); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", now(), time.Now().UnixNano())))
		return "ctxa_" + hex.EncodeToString(sum[:8])
	}
	return "ctxa_" + hex.EncodeToString(value)
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
