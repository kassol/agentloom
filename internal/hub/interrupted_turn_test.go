package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yan5xu/codex-loom/internal/store"
)

func TestReconcileInterruptedPiTurnKeepsCanonicalLoomTurnIdentity(t *testing.T) {
	sessionFile := filepath.Join(t.TempDir(), "session.jsonl")
	contents := strings.Join([]string{
		`{"type":"session","version":3,"id":"session-1","timestamp":"2026-08-10T01:00:00Z","cwd":"/tmp/work"}`,
		`{"type":"message","id":"native-user-1","parentId":null,"timestamp":"2026-08-10T01:00:01Z","message":{"role":"user","content":[{"type":"text","text":"recover canonical work"}]}}`,
		`{"type":"message","id":"native-assistant-1","parentId":"native-user-1","timestamp":"2026-08-10T01:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stopReason":"stop"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(sessionFile, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := &Agent{
		ID: "agent-1", RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: sessionFile},
		RuntimeTurnBindings: map[string]string{"turn-loom-1": "native-user-1"},
		Status:              "running", CurrentTurnID: "turn-loom-1", CurrentTask: "recover canonical work",
	}

	summary, missingTerminal := reconcileInterruptedTurn(meta)
	if !missingTerminal || summary.TurnID != "turn-loom-1" || summary.Status != "interrupted" {
		t.Fatalf("reconciled Pi Turn = %#v, missingTerminal=%v", summary, missingTerminal)
	}
}

func TestOpenKeepsRestartInterruptedAgentVisible(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", t.TempDir()+"/missing.json")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAgents(map[string]*Agent{
		"agent-1": {
			ID: "agent-1", Name: "worker", Cwd: t.TempDir(), ThreadID: "loom-thread-1", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thread-1"},
			Status: "running", CurrentTask: "finish the release", CurrentTurnID: "turn-1",
			CreatedAt: now(), UpdatedAt: now(),
		},
	}); err != nil {
		t.Fatal(err)
	}

	h, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown()
	view, err := h.GetAgent("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "interrupted" || view.CurrentTurnID != "" || view.CurrentTask != "" {
		t.Fatalf("recovered Agent = %#v", view)
	}
	if view.LastTurn == nil || view.LastTurn.TurnID != "turn-1" || view.LastTurn.Task != "finish the release" || view.LastTurn.Status != "interrupted" {
		t.Fatalf("interrupted Turn = %#v", view.LastTurn)
	}
	if !strings.Contains(view.LastError, "restarted") {
		t.Fatalf("last error = %q", view.LastError)
	}
}

func TestDismissInterruptedTurnKeepsHistoryAndClearsAttention(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", Status: "interrupted", LastError: restartInterruptedError,
		LastTurn:  &TurnSummary{TurnID: "turn-1", Task: "unfinished", Status: "interrupted", CompletedAt: now()},
		CreatedAt: now(), UpdatedAt: now(),
	}

	view, err := h.DismissInterruptedTurn("worker")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "idle" || view.LastError != "" {
		t.Fatalf("dismissed Agent = %#v", view)
	}
	if view.LastTurn == nil || view.LastTurn.TurnID != "turn-1" || view.LastTurn.Status != "interrupted" {
		t.Fatalf("dismiss removed Turn history: %#v", view.LastTurn)
	}
}

func TestContinueInterruptedNotificationReusesMessageIdentity(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.draining = true // Keep the async delivery worker from starting Codex.
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", Status: "interrupted", LastError: restartInterruptedError,
		LastTurn:  &TurnSummary{TurnID: "turn-1", Task: "process notification", Status: "interrupted", CompletedAt: now()},
		CreatedAt: now(), UpdatedAt: now(),
	}
	h.comms["msg-1"] = &AgentMessage{
		ID: "msg-1", ToAgentID: "agent-1", To: "worker", FromAgentID: "agent-2", From: "sender",
		Subject: "Background context", Response: "none", Status: "closed",
		DeliveryStatus: "delivered", DeliveryMode: "turn_start", DeliveredTurnID: "turn-1",
		HandlingStatus: "interrupted", LastHandlingError: restartInterruptedError,
		HandlingAttempts: []AgentMessageHandlingAttempt{{ID: "matt-1", TurnID: "turn-1", Status: "interrupted", StartedAt: now(), CompletedAt: now()}},
		CreatedAt:        now(), UpdatedAt: now(),
	}
	h.commOrder = []string{"msg-1"}

	action, err := h.ContinueInterruptedTurn("worker", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.workers.Wait()
	if action.Mode != "message" || action.Source == nil || action.Source.ID != "msg-1" {
		t.Fatalf("action = %#v", action)
	}
	if action.Agent.Status != "idle" || action.Agent.LastTurn == nil || action.Agent.LastTurn.TurnID != "turn-1" {
		t.Fatalf("continued Agent = %#v", action.Agent)
	}
	message := h.comms["msg-1"]
	if message == nil || message.ID != "msg-1" || message.DeliveryStatus != "queued" || message.HandlingStatus != "pending" || len(message.HandlingAttempts) != 1 {
		t.Fatalf("continued Message = %#v", message)
	}
}

func TestInterruptedTurnRecoveryPromptRequiresFactAndIdempotencyChecks(t *testing.T) {
	prompt := interruptedTurnRecoveryPrompt(TurnSummary{TurnID: "turn-1", Task: "deploy & verify", Status: "interrupted", CompletedAt: "2026-07-21T01:02:03Z"}, nil)
	for _, fragment := range []string{"loom_turn_recovery", `previous_turn_id="turn-1"`, "deploy &amp; verify", "Re-check current external facts", "idempotency"} {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("prompt missing %q: %s", fragment, prompt)
		}
	}
}
