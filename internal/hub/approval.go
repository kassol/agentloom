package hub

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

type runtimeApprovalRequest struct {
	AgentID     string
	TurnID      string
	RuntimeKind string
	Proposal    runtimecontract.ApprovalProposal
}

type ApprovalResolutionParams struct {
	Decision      string          `json:"decision"`
	ModifiedInput json.RawMessage `json:"modifiedInput,omitempty"`
}

func newApprovalID(agentID string) string {
	return "ap-" + agentID + "-" + strings.TrimPrefix(newIntegrationID("approval"), "approval_")
}

func normalizeApprovalDecision(decision string) (string, string, bool) {
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "approve", "accept":
		return "approve", "approved", true
	case "deny", "reject", "cancel":
		return "deny", "denied", true
	case "timeout", "timed_out":
		return "timeout", "timed_out", true
	case "abort", "aborted":
		return "abort", "aborted", true
	default:
		return decision, "", false
	}
}

func approvalEventPayload(approval ApprovalView) map[string]any {
	return map[string]any{
		"approvalId": approval.ApprovalID, "agentId": approval.AgentID, "turnId": approval.TurnID,
		"runtimeKind": approval.RuntimeKind, "method": approval.Method, "params": approval.Params,
		"status": approval.Status, "decision": approval.Decision, "requestedAt": approval.RequestedAt,
		"resolvedAt": approval.ResolvedAt, "resolutionError": approval.ResolutionError,
		"deliveryStatus": approval.DeliveryStatus, "deliveryError": approval.DeliveryError,
		"effectStatus": approval.EffectStatus,
	}
}

// requestRuntimeApprovalLocked is the Runtime-neutral ingress for a live
// request. The callback receives only Loom decisions; each Runtime adapter is
// responsible for its own wire vocabulary.
func (h *Hub) requestRuntimeApprovalLocked(request runtimeApprovalRequest, respond func(runtimecontract.ApprovalDecision) error) (ApprovalView, error) {
	meta := h.agents[request.AgentID]
	rt := h.runtimes[request.AgentID]
	if meta == nil || rt == nil {
		return ApprovalView{}, fmt.Errorf("Agent Runtime is unavailable")
	}
	requestedAt := now()
	publicParams := canonicalApprovalParams(meta, request.TurnID, request.Proposal)
	method := request.Proposal.Action
	if method == "" {
		method = request.Proposal.ToolName
	}
	next := ApprovalView{
		ApprovalID: newApprovalID(request.AgentID), AgentID: request.AgentID, TurnID: request.TurnID,
		RuntimeKind: request.RuntimeKind, Method: method, Params: publicParams,
		Status: "pending", DeliveryStatus: "waiting", EffectStatus: "unobserved", RequestedAt: requestedAt, TS: requestedAt,
	}
	if err := h.commitApprovalLocked(next); err != nil {
		if respond != nil {
			h.startWorkerLocked(func() { _ = respond(runtimecontract.ApprovalAbort) })
		}
		return ApprovalView{}, err
	}
	if rt.approvals == nil {
		rt.approvals = map[string]*approval{}
	}
	rt.approvals[next.ApprovalID] = &approval{toolCallID: request.Proposal.ToolCallID, respond: respond, done: make(chan struct{})}
	if rt.activeTurn != nil && !rt.activeTurn.finished {
		rt.activeTurn.lastActivity = time.Now()
	}
	h.emitLocked(meta.ID, "loom/approval-requested", approvalEventPayload(next))
	return next, nil
}

func canonicalApprovalParams(meta *Agent, turnID string, proposal runtimecontract.ApprovalProposal) json.RawMessage {
	public := map[string]any{}
	if meta != nil && meta.ThreadID != "" {
		public["threadId"] = meta.ThreadID
	}
	if turnID != "" {
		public["turnId"] = turnID
	}
	if proposal.Action != "" {
		public["action"] = proposal.Action
	}
	if proposal.ToolName != "" {
		public["toolName"] = proposal.ToolName
	}
	if len(proposal.Arguments) > 0 {
		arguments := make(map[string]any, len(proposal.Arguments))
		for _, argument := range proposal.Arguments {
			if approvalActionKey(argument.Name) && !privateRuntimeCollaborationArgument(proposal.ToolName, argument.Name) {
				arguments[argument.Name] = projectApprovalAction(proposal.ToolName, argument.Value)
			}
		}
		public["arguments"] = arguments
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		return json.RawMessage(`{"redacted":true}`)
	}
	return encoded
}

func privateRuntimeCollaborationArgument(toolName, argumentName string) bool {
	normalize := func(value string) string {
		return strings.NewReplacer("_", "", "-", "", " ", "", "/", "").Replace(strings.ToLower(strings.TrimSpace(value)))
	}
	argument := normalize(argumentName)
	if strings.Contains(argument, "native") || strings.Contains(argument, "session") || argument == "subagentid" ||
		argument == "teamid" || argument == "teamname" || argument == "resume" {
		return true
	}
	tool := normalize(toolName)
	return argument == "name" && (strings.Contains(tool, "agent") || strings.Contains(tool, "team") || strings.Contains(tool, "task"))
}

func projectApprovalAction(toolName string, value any) any {
	switch typed := value.(type) {
	case map[string]any:
		projected := map[string]any{}
		for key, nested := range typed {
			if approvalActionKey(key) && !privateRuntimeCollaborationArgument(toolName, key) {
				projected[key] = projectApprovalAction(toolName, nested)
			}
		}
		return projected
	case []any:
		projected := make([]any, len(typed))
		for index, nested := range typed {
			projected[index] = projectApprovalAction(toolName, nested)
		}
		return projected
	case string:
		return redactRuntimeDiagnosticValue("value", typed)
	default:
		return value
	}
}

func approvalActionKey(key string) bool {
	normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(strings.TrimSpace(key)))
	switch normalized {
	case "toolname", "command", "cwd", "path", "filepath", "source", "destination", "target",
		"url", "host", "port", "method", "reason", "justification", "description", "query",
		"pattern", "prompt", "content", "oldtext", "newtext", "patch", "input", "args",
		"arguments", "changes", "edits", "permissions", "oldstring", "newstring", "replaceall",
		"offset", "limit", "pages", "timeout", "runinbackground", "dangerouslydisablesandbox",
		"glob", "outputmode", "context", "linenumber", "ignorecase", "type", "headlimit", "multiline",
		"alloweddomains", "blockeddomains", "notebookpath", "cellid", "newsource", "celltype", "editmode",
		"subagenttype", "model", "resume", "maxturns", "name", "teamname", "mode", "skill":
		return true
	default:
		return false
	}
}

func (h *Hub) commitApprovalLocked(approval ApprovalView) error {
	if h.approvals == nil {
		h.approvals = map[string]*ApprovalView{}
	}
	if err := h.st.AppendApproval(approvalRecord{Approval: approval}); err != nil {
		return err
	}
	if h.approvals[approval.ApprovalID] == nil {
		h.approvalOrder = append(h.approvalOrder, approval.ApprovalID)
	}
	copy := approval
	h.approvals[approval.ApprovalID] = &copy
	return nil
}

func (h *Hub) loadApprovals() error {
	h.approvals = map[string]*ApprovalView{}
	h.approvalOrder = nil
	return h.st.ReadApprovals(func(raw json.RawMessage) {
		var record approvalRecord
		if json.Unmarshal(raw, &record) != nil || record.Approval.ApprovalID == "" {
			return
		}
		if record.Approval.DeliveryStatus == "" {
			if record.Approval.Status == "pending" {
				record.Approval.DeliveryStatus = "waiting"
			} else {
				record.Approval.DeliveryStatus = "delivered"
			}
		}
		if record.Approval.EffectStatus == "" {
			record.Approval.EffectStatus = "unobserved"
		}
		if h.approvals[record.Approval.ApprovalID] == nil {
			h.approvalOrder = append(h.approvalOrder, record.Approval.ApprovalID)
		}
		approval := record.Approval
		h.approvals[approval.ApprovalID] = &approval
	})
}

// Runtime server requests cannot survive their owning process. On startup,
// stale pending records become terminal before the snapshot is exposed.
func (h *Hub) recoverPendingApprovals(persist bool) error {
	for _, id := range h.approvalOrder {
		current := h.approvals[id]
		if current == nil {
			continue
		}
		next := *current
		switch {
		case current.Status == "pending":
			next.Status = "aborted"
			next.Decision = "abort"
			next.DeliveryStatus = "unavailable"
			next.ResolvedAt = now()
			next.ResolutionError = "CodexLoom restarted before the Runtime Approval was resolved"
		case current.DeliveryStatus == "pending":
			next.DeliveryStatus = "indeterminate"
			next.DeliveryError = "Runtime callback delivery outcome is indeterminate after restart"
		default:
			continue
		}
		if persist {
			if err := h.commitApprovalLocked(next); err != nil {
				return err
			}
		} else {
			h.approvals[id] = &next
		}
	}
	return nil
}

func (h *Hub) abortTurnApprovalsLocked(agentID, turnID string, rt *runtime, reason string) {
	for id, waiter := range rt.approvals {
		current := h.approvals[id]
		if current == nil || current.AgentID != agentID || current.Status != "pending" || (turnID != "" && current.TurnID != turnID) {
			continue
		}
		next := *current
		next.Status = "aborted"
		next.Decision = "abort"
		next.DeliveryStatus = "pending"
		next.ResolvedAt = now()
		next.ResolutionError = reason
		if err := h.commitApprovalLocked(next); err != nil {
			next.ResolutionError += "; persist failure: " + err.Error()
			h.approvals[id] = &next
		}
		h.emitLocked(agentID, "loom/approval-resolved", approvalEventPayload(next))
		closeApprovalWaiter(waiter)
		delete(rt.approvals, id)
		if waiter != nil && waiter.respond != nil {
			respond := waiter.respond
			h.startWorkerLocked(func() { h.deliverApprovalAbort(agentID, id, respond) })
		}
	}
}

func (h *Hub) deliverApprovalAbort(agentID, approvalID string, respond func(runtimecontract.ApprovalDecision) error) {
	err := respond(runtimecontract.ApprovalAbort)
	h.mu.Lock()
	defer h.mu.Unlock()
	current := h.approvals[approvalID]
	if current == nil || current.Decision != "abort" || current.DeliveryStatus != "pending" {
		return
	}
	next := *current
	if err != nil {
		next.DeliveryStatus = "failed"
		next.DeliveryError = "Runtime callback delivery failed"
		if isRuntimeIndeterminate(err) {
			next.DeliveryStatus = "indeterminate"
			next.DeliveryError = "Runtime callback delivery outcome is indeterminate"
		}
		log.Printf("[codex-loom] deliver Approval abort %s: %v", approvalID, err)
	} else {
		next.DeliveryStatus = "delivered"
	}
	if persistErr := h.commitApprovalLocked(next); persistErr != nil {
		log.Printf("[codex-loom] persist Approval abort delivery %s: %v", approvalID, persistErr)
		next.DeliveryStatus = "indeterminate"
		next.DeliveryError = "Approval delivery evidence could not be persisted"
		h.approvals[approvalID] = &next
	}
	h.emitLocked(agentID, "loom/approval-resolved", approvalEventPayload(next))
}

func closeApprovalWaiter(waiter *approval) {
	if waiter != nil && waiter.done != nil {
		close(waiter.done)
	}
}

func (h *Hub) pendingApprovalsLocked(agentID string) []ApprovalView {
	pending := make([]ApprovalView, 0)
	for _, id := range h.approvalOrder {
		approval := h.approvals[id]
		if approval == nil || approval.AgentID != agentID || approval.Status != "pending" {
			continue
		}
		pending = append(pending, *approval)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].RequestedAt < pending[j].RequestedAt })
	return pending
}
