package hub

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type runtimeApprovalRequest struct {
	AgentID     string
	TurnID      string
	RuntimeKind string
	Method      string
	Params      json.RawMessage
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

// codexApprovalDecision is the Codex-specific wire adapter. Loom stores and
// emits only Runtime-neutral decisions.
func codexApprovalDecision(decision string) string {
	if decision == "approve" {
		return "accept"
	}
	return "cancel"
}

func approvalEventPayload(approval ApprovalView) map[string]any {
	return map[string]any{
		"approvalId": approval.ApprovalID, "agentId": approval.AgentID, "turnId": approval.TurnID,
		"runtimeKind": approval.RuntimeKind, "method": approval.Method, "params": approval.Params,
		"status": approval.Status, "decision": approval.Decision, "requestedAt": approval.RequestedAt,
		"resolvedAt": approval.ResolvedAt, "resolutionError": approval.ResolutionError,
	}
}

// requestRuntimeApprovalLocked is the Runtime-neutral ingress for a live
// request. The callback receives only Loom decisions; each Runtime adapter is
// responsible for its own wire vocabulary.
func (h *Hub) requestRuntimeApprovalLocked(request runtimeApprovalRequest, respond func(decision string) error) (ApprovalView, error) {
	meta := h.agents[request.AgentID]
	rt := h.runtimes[request.AgentID]
	if meta == nil || rt == nil {
		return ApprovalView{}, fmt.Errorf("Agent Runtime is unavailable")
	}
	requestedAt := now()
	publicParams := canonicalApprovalParams(meta, request.TurnID, request.Params)
	next := ApprovalView{
		ApprovalID: newApprovalID(request.AgentID), AgentID: request.AgentID, TurnID: request.TurnID,
		RuntimeKind: request.RuntimeKind, Method: request.Method, Params: publicParams,
		Status: "pending", RequestedAt: requestedAt, TS: requestedAt,
	}
	if err := h.commitApprovalLocked(next); err != nil {
		if respond != nil {
			h.startWorkerLocked(func() { _ = respond("abort") })
		}
		return ApprovalView{}, err
	}
	if rt.approvals == nil {
		rt.approvals = map[string]*approval{}
	}
	rt.approvals[next.ApprovalID] = &approval{respond: respond, done: make(chan struct{})}
	if rt.activeTurn != nil && !rt.activeTurn.finished {
		rt.activeTurn.lastActivity = time.Now()
	}
	h.emitLocked(meta.ID, "loom/approval-requested", approvalEventPayload(next))
	return next, nil
}

func canonicalApprovalParams(meta *Agent, turnID string, raw json.RawMessage) json.RawMessage {
	public := map[string]any{}
	if meta != nil && meta.ThreadID != "" {
		public["threadId"] = meta.ThreadID
	}
	if turnID != "" {
		public["turnId"] = turnID
	}
	var input map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &input) != nil {
		public["redacted"] = true
	} else {
		for key, value := range input {
			if approvalActionKey(key) {
				public[key] = projectApprovalAction(value)
			}
		}
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		return json.RawMessage(`{"redacted":true}`)
	}
	return encoded
}

func projectApprovalAction(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		projected := map[string]any{}
		for key, nested := range typed {
			if approvalActionKey(key) {
				projected[key] = projectApprovalAction(nested)
			}
		}
		return projected
	case []any:
		projected := make([]any, len(typed))
		for index, nested := range typed {
			projected[index] = projectApprovalAction(nested)
		}
		return projected
	default:
		return value
	}
}

func approvalActionKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "toolname", "command", "cwd", "path", "filepath", "source", "destination", "target",
		"url", "host", "port", "method", "reason", "justification", "description", "query",
		"pattern", "prompt", "content", "oldtext", "newtext", "patch", "input", "args",
		"arguments", "changes", "edits", "permissions":
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
		if current == nil || current.Status != "pending" {
			continue
		}
		next := *current
		next.Status = "aborted"
		next.Decision = "abort"
		next.ResolvedAt = now()
		next.ResolutionError = "CodexLoom restarted before the Runtime Approval was resolved"
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
			h.startWorkerLocked(func() { _ = respond("abort") })
		}
	}
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
