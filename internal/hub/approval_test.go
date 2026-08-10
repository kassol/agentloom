package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/store"
)

func TestRuntimeApprovalRequestIsDurableAndVisibleInAgentSnapshot(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "loom-thread-1", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "native-thread-1"}, Status: "running",
	}
	rt := &runtime{
		agentID: "agent-1", agentRuntime: &fakeAgentRuntime{}, approvals: map[string]*approval{},
		activeTurn: &turnState{turnID: "turn-loom-1", nativeTurnID: "native-turn-1", startedAt: time.Now(), stopWatchdog: make(chan struct{})},
	}
	h.runtimes["agent-1"] = rt

	h.onServerRequest(rt, json.RawMessage(`41`), "item/commandExecution/requestApproval", json.RawMessage(`{"command":"rm draft.txt"}`))

	view, err := h.GetAgent("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.PendingApprovals) != 1 {
		t.Fatalf("pending Approval snapshot = %#v", view.PendingApprovals)
	}
	approval := view.PendingApprovals[0]
	if !strings.HasPrefix(approval.ApprovalID, "ap-agent-1-") || approval.AgentID != "agent-1" || approval.TurnID != "turn-loom-1" || approval.RuntimeKind != "codex" || approval.Status != "pending" {
		t.Fatalf("pending Approval = %#v", approval)
	}

	var records []ApprovalView
	if err := st.ReadApprovals(func(raw json.RawMessage) {
		var record struct {
			Approval ApprovalView `json:"approval"`
		}
		if json.Unmarshal(raw, &record) == nil {
			records = append(records, record.Approval)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Status != "pending" || records[0].Method != "item/commandExecution/requestApproval" {
		t.Fatalf("durable Approval records = %#v", records)
	}
}

func TestApprovePersistsTerminalRecordAndUsesCodexDecisionAdapter(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	h.agents["agent-1"] = &Agent{ID: "agent-1", Name: "worker", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "native-thread-1"}}
	rt := &runtime{
		agentID: "agent-1", agentRuntime: &fakeAgentRuntime{}, approvals: map[string]*approval{},
		activeTurn: &turnState{turnID: "turn-loom-1", startedAt: time.Now(), stopWatchdog: make(chan struct{})},
	}
	h.runtimes["agent-1"] = rt
	h.onServerRequest(rt, json.RawMessage(`41`), "item/commandExecution/requestApproval", json.RawMessage(`{"command":"rm draft.txt"}`))
	view, err := h.GetAgent("agent-1")
	if err != nil || len(view.PendingApprovals) != 1 {
		t.Fatalf("pending Approval = %#v, err=%v", view.PendingApprovals, err)
	}
	approvalID := view.PendingApprovals[0].ApprovalID
	responded := make(chan string, 1)
	rt.approvals[approvalID].respond = func(decision string) error {
		responded <- decision
		return nil
	}

	result, err := h.ResolveApproval("agent-1", approvalID, "approve")
	if err != nil {
		t.Fatal(err)
	}
	if result["decision"] != "approve" || result["status"] != "approved" {
		t.Fatalf("Approval result = %#v", result)
	}
	select {
	case decision := <-responded:
		if decision != "approve" {
			t.Fatalf("Runtime-neutral decision = %q", decision)
		}
	case <-time.After(time.Second):
		t.Fatal("Codex Approval request was not unblocked")
	}
	view, err = h.GetAgent("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.PendingApprovals) != 0 {
		t.Fatalf("resolved Approval remained pending: %#v", view.PendingApprovals)
	}
	if got := h.approvals[approvalID]; got == nil || got.Status != "approved" || got.Decision != "approve" || got.ResolvedAt == "" {
		t.Fatalf("durable Approval terminal = %#v", got)
	}
}

func TestCodexApprovalDecisionAdapter(t *testing.T) {
	if got := codexApprovalDecision("approve"); got != "accept" {
		t.Fatalf("approve = %q", got)
	}
	for _, decision := range []string{"deny", "timeout", "abort"} {
		if got := codexApprovalDecision(decision); got != "cancel" {
			t.Fatalf("%s = %q", decision, got)
		}
	}
}

func TestApprovalTerminalDecisionsAreDurableAndUnblockRuntime(t *testing.T) {
	for _, test := range []struct {
		decision string
		status   string
	}{
		{decision: "deny", status: "denied"},
		{decision: "timeout", status: "timed_out"},
		{decision: "abort", status: "aborted"},
	} {
		t.Run(test.decision, func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			h := testHub(st)
			h.agents["agent-1"] = &Agent{ID: "agent-1", RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "pi-thread"}}
			rt := &runtime{agentID: "agent-1", agentRuntime: &fakeAgentRuntime{}, approvals: map[string]*approval{}}
			h.runtimes["agent-1"] = rt
			responded := make(chan string, 1)
			created, err := h.requestRuntimeApprovalLocked(runtimeApprovalRequest{
				AgentID: "agent-1", TurnID: "turn-1", RuntimeKind: "pi", Method: "tool/bash", Params: json.RawMessage(`{"command":"false"}`),
			}, func(decision string) error {
				responded <- decision
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}

			result, err := h.ResolveApproval("agent-1", created.ApprovalID, test.decision)
			if err != nil {
				t.Fatal(err)
			}
			if result["status"] != test.status || h.approvals[created.ApprovalID].Status != test.status {
				t.Fatalf("result=%#v Approval=%#v", result, h.approvals[created.ApprovalID])
			}
			select {
			case got := <-responded:
				if got != test.decision {
					t.Fatalf("Runtime decision = %q", got)
				}
			case <-time.After(time.Second):
				t.Fatal("Runtime Approval remained blocked")
			}
		})
	}
}

func TestFinishingTurnAbortsPendingRuntimeApproval(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	meta := &Agent{ID: "agent-1", RuntimeBinding: RuntimeBinding{Kind: "pi"}, Status: "running"}
	turn := &turnState{turnID: "turn-loom-1", startedAt: time.Now(), stopWatchdog: make(chan struct{})}
	rt := &runtime{agentID: meta.ID, approvals: map[string]*approval{}, activeTurn: turn}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = rt
	responded := make(chan string, 1)
	created, err := h.requestRuntimeApprovalLocked(runtimeApprovalRequest{
		AgentID: meta.ID, TurnID: turn.turnID, RuntimeKind: "pi", Method: "tool/write", Params: json.RawMessage(`{"path":"draft.txt"}`),
	}, func(decision string) error {
		responded <- decision
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	h.mu.Lock()
	h.finishTurnLocked(meta, rt, "completed", "")
	h.mu.Unlock()
	select {
	case decision := <-responded:
		if decision != "abort" {
			t.Fatalf("Turn finish decision = %q", decision)
		}
	case <-time.After(time.Second):
		t.Fatal("Turn finish left Runtime Approval blocked")
	}
	terminal := h.approvals[created.ApprovalID]
	if terminal == nil || terminal.Status != "aborted" || terminal.Decision != "abort" || !strings.Contains(terminal.ResolutionError, "Turn ended") {
		t.Fatalf("Turn finish Approval terminal = %#v", terminal)
	}
}

func TestOpenAbortsPersistedPendingApprovalThatCannotResume(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	agent := &Agent{ID: "agent-1", Name: "worker", ThreadID: "loom-thread", RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "pi-thread"}, Status: "idle"}
	if err := st.SaveAgents(map[string]*Agent{agent.ID: agent}); err != nil {
		t.Fatal(err)
	}
	pending := ApprovalView{
		ApprovalID: "ap-agent-1-old", AgentID: agent.ID, TurnID: "turn-old", RuntimeKind: "pi", Method: "tool/bash",
		Status: "pending", RequestedAt: now(),
	}
	if err := st.AppendApproval(approvalRecord{Approval: pending}); err != nil {
		t.Fatal(err)
	}

	h, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown()
	view, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.PendingApprovals) != 0 {
		t.Fatalf("stale Approval remained actionable: %#v", view.PendingApprovals)
	}
	got := h.approvals[pending.ApprovalID]
	if got == nil || got.Status != "aborted" || got.Decision != "abort" || !strings.Contains(got.ResolutionError, "restarted") {
		t.Fatalf("recovered Approval = %#v", got)
	}
	var records int
	if err := st.ReadApprovals(func(json.RawMessage) { records++ }); err != nil {
		t.Fatal(err)
	}
	if records != 2 {
		t.Fatalf("Approval records = %d, want pending + aborted", records)
	}
}

func TestApprovalRequestAppendFailureUnblocksRuntime(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	h.st = st.RetiredReadOnlyView()
	h.agents["agent-1"] = &Agent{ID: "agent-1", RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "pi-thread"}}
	h.runtimes["agent-1"] = &runtime{agentID: "agent-1", agentRuntime: &fakeAgentRuntime{}, approvals: map[string]*approval{}}
	responded := make(chan string, 1)

	h.mu.Lock()
	_, err = h.requestRuntimeApprovalLocked(runtimeApprovalRequest{
		AgentID: "agent-1", RuntimeKind: "pi", Method: "tool/bash", Params: json.RawMessage(`{"command":"false"}`),
	}, func(decision string) error {
		responded <- decision
		return nil
	})
	h.mu.Unlock()
	if err == nil {
		t.Fatal("request unexpectedly persisted to retired Store")
	}
	select {
	case got := <-responded:
		if got != "abort" {
			t.Fatalf("append failure decision = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("append failure left Runtime Approval blocked")
	}
}
