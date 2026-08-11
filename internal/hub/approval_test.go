package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func testApprovalProposal(toolName string, arguments ...runtimecontract.ApprovalArgument) runtimecontract.ApprovalProposal {
	return runtimecontract.ApprovalProposal{ID: "runtime-proposal-1", ToolName: toolName, Action: toolName, Arguments: arguments}
}

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
		agentID: "agent-1", approvals: map[string]*approval{},
		activeTurn: &turnState{turnID: "turn-loom-1", nativeTurnID: "native-turn-1", startedAt: time.Now(), stopWatchdog: make(chan struct{})},
	}
	h.runtimes["agent-1"] = rt

	h.onRuntimeApprovalRequest(rt, testApprovalProposal("tool/bash", runtimecontract.ApprovalArgument{Name: "command", Value: "rm draft.txt"}))

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
	if len(records) != 1 || records[0].Status != "pending" || records[0].Method != "tool/bash" {
		t.Fatalf("durable Approval records = %#v", records)
	}
}

func TestApprovalIngressPersistsOnlyCanonicalLoomIdentity(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "loom-thread-1",
		RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "native-thread-secret"}, Status: "running",
	}
	rt := &runtime{
		agentID: "agent-1", approvals: map[string]*approval{},
		activeTurn: &turnState{turnID: "turn-loom-1", nativeTurnID: "native-turn-secret", startedAt: time.Now(), stopWatchdog: make(chan struct{})},
	}
	h.runtimes["agent-1"] = rt
	contract := &codexRuntimeContract{bindingRef: "native-thread-secret", pendingTurn: runtimeTurnCorrelation{turnID: "turn-loom-1"}}
	rt.runtimeContract = contract
	contract.SetApprovalHandler(func(proposal runtimecontract.ApprovalProposal) {
		proposal.Timeout = 0
		h.onRuntimeApprovalRequest(rt, proposal)
	})
	if !contract.handleNativeServerRequest(nil, json.RawMessage(`71`), "item/commandExecution/requestApproval",
		json.RawMessage(`{"threadId":"native-thread-secret","turnId":"native-turn-secret","toolName":"bash","command":"printf safe","justification":"verify the release"}`)) {
		t.Fatal("Codex adapter did not claim the native Approval request")
	}
	rt.runtimeContract = nil

	view, err := h.GetAgent("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(view)
	events, err := h.ReadEvents("agent-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	eventJSON, _ := json.Marshal(events)
	for _, raw := range [][]byte{encoded, eventJSON} {
		if strings.Contains(string(raw), "item/commandExecution/requestApproval") {
			t.Fatalf("public Approval projection leaked the native RPC method: %s", raw)
		}
		for _, secret := range []string{"native-thread-secret", "native-turn-secret", "native-session-secret", "native-session-id-secret", "native-tool-call-secret", "native-item-secret", "native-call-secret", "native-rpc-param-secret", "native-rpc-secret"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("public Approval projection leaked %q: %s", secret, raw)
			}
		}
		for _, forbiddenKey := range []string{"toolCallId", "itemId", "callId", "sessionFile", "sessionId", "rpcId", "nested"} {
			if strings.Contains(string(raw), `"`+forbiddenKey+`"`) {
				t.Fatalf("public Approval projection retained non-actionable key %q: %s", forbiddenKey, raw)
			}
		}
		if !strings.Contains(string(raw), "loom-thread-1") || !strings.Contains(string(raw), "turn-loom-1") {
			t.Fatalf("public Approval projection omitted Loom identity: %s", raw)
		}
		if !strings.Contains(string(raw), "printf safe") || !strings.Contains(string(raw), "verify the release") {
			t.Fatalf("public Approval projection omitted actionable fields: %s", raw)
		}
	}
	if len(view.PendingApprovals) != 1 || rt.approvals[view.PendingApprovals[0].ApprovalID] == nil {
		t.Fatalf("private Runtime Approval waiter was not retained: %#v", rt.approvals)
	}
	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := Open(reopened)
	if err != nil {
		t.Fatal(err)
	}
	reopenedView, err := h2.GetAgent("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	reopenedJSON, _ := json.Marshal(reopenedView)
	if strings.Contains(string(reopenedJSON), "native-") || strings.Contains(string(reopenedJSON), "native-session-secret") {
		t.Fatalf("reopened Approval leaked native identity: %s", reopenedJSON)
	}
	var reopenedApprovals []json.RawMessage
	if err := reopened.ReadApprovals(func(raw json.RawMessage) {
		reopenedApprovals = append(reopenedApprovals, append(json.RawMessage(nil), raw...))
	}); err != nil {
		t.Fatal(err)
	}
	reopenedEvents, err := h2.ReadEvents("agent-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	durableJSON, _ := json.Marshal(struct {
		Approvals []json.RawMessage `json:"approvals"`
		Events    []store.Event     `json:"events"`
	}{reopenedApprovals, reopenedEvents})
	if strings.Contains(string(durableJSON), "item/commandExecution/requestApproval") {
		t.Fatalf("reopened Approval Store/SSE projection leaked the native RPC method: %s", durableJSON)
	}
	for _, secret := range []string{"native-thread-secret", "native-turn-secret", "native-session-secret", "native-session-id-secret", "native-tool-call-secret", "native-item-secret", "native-call-secret", "native-rpc-param-secret", "native-rpc-secret"} {
		if strings.Contains(string(durableJSON), secret) {
			t.Fatalf("reopened Approval Store/SSE projection leaked %q: %s", secret, durableJSON)
		}
	}
	h2.Shutdown()
	reopened.Close()
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
		agentID: "agent-1", approvals: map[string]*approval{},
		activeTurn: &turnState{turnID: "turn-loom-1", startedAt: time.Now(), stopWatchdog: make(chan struct{})},
	}
	h.runtimes["agent-1"] = rt
	h.onRuntimeApprovalRequest(rt, testApprovalProposal("tool/bash", runtimecontract.ApprovalArgument{Name: "command", Value: "rm draft.txt"}))
	view, err := h.GetAgent("agent-1")
	if err != nil || len(view.PendingApprovals) != 1 {
		t.Fatalf("pending Approval = %#v, err=%v", view.PendingApprovals, err)
	}
	approvalID := view.PendingApprovals[0].ApprovalID
	responded := make(chan string, 1)
	rt.approvals[approvalID].respond = func(decision runtimecontract.ApprovalDecision) error {
		responded <- string(decision)
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

func TestCodexContractSurfacesNativeApprovalThroughTypedCallback(t *testing.T) {
	contract := &codexRuntimeContract{bindingRef: "native-thread"}
	requests := make(chan runtimecontract.ApprovalProposal, 1)
	contract.SetApprovalHandler(func(request runtimecontract.ApprovalProposal) { requests <- request })
	params := json.RawMessage(`{"threadId":"native-thread","command":"touch safe.txt"}`)
	if !contract.handleNativeServerRequest(nil, json.RawMessage(`91`), "item/commandExecution/requestApproval", params) {
		t.Fatal("Codex Contract did not claim its native Approval request")
	}
	select {
	case request := <-requests:
		if request.ID == "" || request.ToolName != "tool/command" || request.Action != "command" || len(request.Arguments) != 1 || request.Arguments[0].Name != "command" {
			t.Fatalf("typed Approval request = %#v", request)
		}
		encoded, _ := json.Marshal(request)
		if strings.Contains(string(encoded), "item/commandExecution/requestApproval") {
			t.Fatalf("typed Approval request leaked native RPC method: %s", encoded)
		}
	default:
		t.Fatal("Codex Contract did not surface a typed Approval request")
	}
	if contract.handleNativeServerRequest(nil, json.RawMessage(`92`), "unknown/request", params) {
		t.Fatal("Codex Contract claimed an unknown native request")
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
			rt := &runtime{agentID: "agent-1", approvals: map[string]*approval{}}
			h.runtimes["agent-1"] = rt
			responded := make(chan string, 1)
			created, err := h.requestRuntimeApprovalLocked(runtimeApprovalRequest{
				AgentID: "agent-1", TurnID: "turn-1", RuntimeKind: "pi", Proposal: testApprovalProposal("tool/bash", runtimecontract.ApprovalArgument{Name: "command", Value: "false"}),
			}, func(decision runtimecontract.ApprovalDecision) error {
				responded <- string(decision)
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
		AgentID: meta.ID, TurnID: turn.turnID, RuntimeKind: "pi", Proposal: testApprovalProposal("tool/write", runtimecontract.ApprovalArgument{Name: "path", Value: "draft.txt"}),
	}, func(decision runtimecontract.ApprovalDecision) error {
		responded <- string(decision)
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
	h.runtimes["agent-1"] = &runtime{agentID: "agent-1", approvals: map[string]*approval{}}
	responded := make(chan string, 1)

	h.mu.Lock()
	_, err = h.requestRuntimeApprovalLocked(runtimeApprovalRequest{
		AgentID: "agent-1", RuntimeKind: "pi", Proposal: testApprovalProposal("tool/bash", runtimecontract.ApprovalArgument{Name: "command", Value: "false"}),
	}, func(decision runtimecontract.ApprovalDecision) error {
		responded <- string(decision)
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
