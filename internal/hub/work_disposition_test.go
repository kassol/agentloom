package hub

import (
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestTerminalTurnRecordsUnclassifiedUnfinishedWorkWithoutGuessingFromProse(t *testing.T) {
	h, meta, rt := terminalDispositionFixture(t, "Finish the rollout")
	h.topics["topic-release"] = &Topic{
		ID: "topic-release", Title: "Release", Purpose: "Ship it", CompletionBoundary: "Verified",
		Status: TopicStatusActive, ResponsibleAgentID: meta.ID, ResponsibleAgent: meta.Name,
		Version: 1, CreatedAt: now(), UpdatedAt: now(),
	}
	rt.activeTurn.topicID = "topic-release"

	finishTerminalDispositionTurn(t, h, meta, rt, "completed")

	view, err := h.GetAgent(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.WorkDisposition == nil || view.WorkDisposition.Kind != WorkDispositionUnclassified {
		t.Fatalf("work disposition = %#v, want unclassified", view.WorkDisposition)
	}
	if view.WorkDisposition.TurnID != "turn-release" || view.WorkDisposition.ThreadID != meta.ThreadID || view.WorkDisposition.TopicID != "topic-release" {
		t.Fatalf("causality = %#v", view.WorkDisposition)
	}
	if len(view.WorkDisposition.Unfinished) != 1 || view.WorkDisposition.Unfinished[0].Kind != "topic" {
		t.Fatalf("unfinished obligations = %#v", view.WorkDisposition.Unfinished)
	}
}

func TestTerminalTurnDoesNotPersistAnApprovalThatItImmediatelyAbortsAsAWakeSource(t *testing.T) {
	h, meta, rt := terminalDispositionFixture(t, "Finish the rollout")
	h.topics["topic-release"] = &Topic{
		ID: "topic-release", Title: "Release", Purpose: "Ship it", CompletionBoundary: "Verified",
		Status: TopicStatusActive, ResponsibleAgentID: meta.ID, ResponsibleAgent: meta.Name,
		Version: 1, CreatedAt: now(), UpdatedAt: now(),
	}
	rt.activeTurn.topicID = "topic-release"
	h.mu.Lock()
	if _, err := h.requestRuntimeApprovalLocked(runtimeApprovalRequest{
		AgentID: meta.ID, TurnID: rt.activeTurn.turnID, RuntimeKind: "pi",
		Proposal: runtimecontract.ApprovalProposal{Action: "shell"},
	}, nil); err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	h.mu.Unlock()

	finishTerminalDispositionTurn(t, h, meta, rt, "completed")

	view, err := h.GetAgent(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.WorkDisposition == nil || view.WorkDisposition.Kind != WorkDispositionUnclassified {
		t.Fatalf("work disposition = %#v, want unclassified after terminal Approval abort", view.WorkDisposition)
	}
	if len(view.WorkDisposition.WakeSources) != 0 || len(view.PendingApprovals) != 0 {
		t.Fatalf("terminal Turn retained aborted Approval as a wake source: disposition=%#v approvals=%#v", view.WorkDisposition, view.PendingApprovals)
	}
}

func TestTerminalTurnClassifiesEachManagedWakeSourceAndKeepsCausality(t *testing.T) {
	tests := []struct {
		name string
		want string
		seed func(*Hub, *Agent)
	}{
		{name: "Needs You", want: WorkDispositionNeedsYou, seed: func(h *Hub, meta *Agent) {
			h.humanRequests["hrq-release"] = &HumanRequest{ID: "hrq-release", AgentID: meta.ID, ThreadID: meta.ThreadID, SourceTurnID: "turn-release", Expectation: HumanRequestRequired, State: "open", DeliveryStatus: "waiting"}
			h.humanRequestOrder = append(h.humanRequestOrder, "hrq-release")
		}},
		{name: "external Trigger", want: WorkDispositionExternal, seed: func(h *Hub, meta *Agent) {
			h.triggers["trg-release"] = &Trigger{ID: "trg-release", AgentID: meta.ID, State: "armed", ResumeInstruction: "Re-read the provider", Work: TriggerWorkAnchor{AgentID: meta.ID, ThreadIDAtCreation: meta.ThreadID, SourceTurnID: "turn-release", TopicID: "topic-release"}}
		}},
		{name: "Agent Message", want: WorkDispositionAgent, seed: func(h *Hub, meta *Agent) {
			h.comms["msg-review"] = &AgentMessage{ID: "msg-review", FromAgentID: meta.ID, ToAgentID: "reviewer", SourceTurnID: "turn-release", TopicID: "topic-release", Response: "required", Status: "open", Subject: "Review evidence"}
			h.commOrder = append(h.commOrder, "msg-review")
		}},
		{name: "Schedule", want: WorkDispositionTime, seed: func(h *Hub, meta *Agent) {
			h.schedules["sched-release"] = &Schedule{ID: "sched-release", To: meta.Name, AgentID: meta.ID, ThreadID: meta.ThreadID, SourceTurnID: "turn-release", TopicID: "topic-release", Enabled: true, NextRunAt: "2026-08-14T00:00:00Z"}
		}},
		{name: "Topic external waiting", want: WorkDispositionExternal, seed: func(h *Hub, meta *Agent) {
			h.topics["topic-release"] = &Topic{ID: "topic-release", Status: TopicStatusWaiting, ResponsibleAgentID: meta.ID, ResponsibleAgent: meta.Name, WaitingOn: &TopicWaitingOn{Kind: "trigger", RefID: "trg-release", Summary: "Wait for merge"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, meta, rt := terminalDispositionFixture(t, "This prose may say anything")
			rt.activeTurn.topicID = "topic-release"
			h.mu.Lock()
			tt.seed(h, meta)
			h.mu.Unlock()
			finishTerminalDispositionTurn(t, h, meta, rt, "completed")
			view, err := h.GetAgent(meta.ID)
			if err != nil {
				t.Fatal(err)
			}
			if view.WorkDisposition == nil || view.WorkDisposition.Kind != tt.want {
				t.Fatalf("work disposition = %#v, want %s", view.WorkDisposition, tt.want)
			}
			if view.WorkDisposition.TurnID != "turn-release" || view.WorkDisposition.ThreadID != meta.ThreadID {
				t.Fatalf("causality = %#v", view.WorkDisposition)
			}
			if len(view.WorkDisposition.WakeSources) == 0 {
				t.Fatalf("wake sources = %#v", view.WorkDisposition.WakeSources)
			}
		})
	}
}

func TestTerminalTurnDoesNotFlagOrdinaryIdleAgentAndDispositionSurvivesReopen(t *testing.T) {
	h, meta, rt := terminalDispositionFixture(t, "Done")
	dataDir := h.st.Dir()
	originalStore := h.st
	finishTerminalDispositionTurn(t, h, meta, rt, "completed")
	view, err := h.GetAgent(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.WorkDisposition == nil || view.WorkDisposition.Kind != WorkDispositionCompleted {
		t.Fatalf("ordinary idle disposition = %#v", view.WorkDisposition)
	}
	h.Shutdown()
	h = nil
	if err := originalStore.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedStore, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWithOptions(reopenedStore, OpenOptions{Passive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Shutdown()
	reloaded, err := reopened.GetAgent(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.WorkDisposition == nil || reloaded.WorkDisposition.Kind != WorkDispositionCompleted || reloaded.WorkDisposition.TurnID != "turn-release" {
		t.Fatalf("reloaded disposition = %#v", reloaded.WorkDisposition)
	}
}

func terminalDispositionFixture(t *testing.T, task string) (*Hub, *Agent, *runtime) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	t.Cleanup(func() {
		if h != nil {
			h.Shutdown()
		}
	})
	meta := &Agent{ID: "agent-release", Name: "release", Cwd: t.TempDir(), ThreadID: "thread-release", RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "native-release"}, Status: "running", CurrentTask: task, CurrentTurnID: "turn-release", CreatedAt: now(), UpdatedAt: now()}
	rt := &runtime{agentID: meta.ID, approvals: map[string]*approval{}, activeTurn: &turnState{turnID: "turn-release", task: task, startedAt: time.Now(), stopWatchdog: make(chan struct{})}}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = rt
	return h, meta, rt
}

func finishTerminalDispositionTurn(t *testing.T, h *Hub, meta *Agent, rt *runtime, status string) {
	t.Helper()
	h.mu.Lock()
	ok := h.finishTurnWithPendingLocked(meta, rt, status, "", false)
	h.mu.Unlock()
	if !ok {
		t.Fatal("Turn did not finish")
	}
}
