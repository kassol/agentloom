package hub

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/store"
)

func TestCodexRuntimeHostDriverRealRestartSafeStory(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("CODEX_REAL_BIN"))
	if bin == "" {
		t.Skip("set CODEX_REAL_BIN to run the real Codex Runtime Host Driver story")
	}
	t.Setenv("CODEX_REMOTE_BIN", bin)
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	t.Cleanup(func() {
		if h != nil {
			h.Shutdown()
		}
		if st != nil {
			_ = st.Close()
		}
	})
	if err := h.codexDriverLocked().Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent, err := h.CreateAgent(CreateParams{Name: "codex-real-smoke", Cwd: t.TempDir(), RuntimeKind: "codex", ApprovalPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := h.GetRuntimeDiagnostics(agent.ID)
	if err != nil || diagnostics.NativeRef == "" {
		t.Fatalf("created binding diagnostics = %#v, err=%v", diagnostics, err)
	}
	dispatched, err := h.SendTask(agent.ID, "Reply with exactly: codex-runtime-host-smoke-ok", 2*time.Minute)
	if err != nil {
		if codexRealSmokeAuthUnavailable(err.Error()) {
			t.Fatalf("CODEX_REAL_BIN is not authenticated; gated story cannot run: %v", err)
		}
		t.Fatal(err)
	}
	terminal := waitRealCodexTerminal(t, h, agent.ID, dispatched.TurnID, 2*time.Minute)
	if terminal.Status != "completed" {
		failedView, _ := h.GetAgent(agent.ID)
		if codexRealSmokeAuthUnavailable(failedView.LastError) {
			t.Fatalf("CODEX_REAL_BIN is not authenticated; gated story cannot run: %s", failedView.LastError)
		}
		t.Fatalf("real Turn terminal = %#v, last error=%q", terminal, failedView.LastError)
	}
	history := waitRealCodexHistory(t, h, agent.ID, dispatched.TurnID, 30*time.Second)
	if len(history.Turns) == 0 || history.Turns[len(history.Turns)-1].TurnID != dispatched.TurnID {
		t.Fatalf("real canonical history = %#v", history)
	}
	if usage, err := h.AgentTokenUsage(agent.ID, 7); err != nil || !usage.Available || usage.Lifetime.TotalTokens <= 0 {
		t.Fatalf("real Codex passive usage = %#v, err=%v", usage, err)
	}
	interruptedTurnID := exerciseRealRuntimeSteerInterrupt(t, h, agent.ID)
	loomThreadID := agent.ThreadID
	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h, err = Open(st)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := h.GetAgent(agent.ID)
	if err != nil || reopened.ID != agent.ID || reopened.ThreadID != loomThreadID {
		t.Fatalf("reopened Agent = %#v, err=%v", reopened.Agent, err)
	}
	h.mu.Lock()
	rt, err := h.getRuntimeLocked(h.agents[agent.ID])
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(rt); err != nil {
		if codexRealSmokeAuthUnavailable(err.Error()) {
			t.Fatalf("CODEX_REAL_BIN cannot resume the binding; gated story failed authentication: %v", err)
		}
		t.Fatal(err)
	}
	afterRestart, err := h.GetRuntimeDiagnostics(agent.ID)
	if err != nil || afterRestart.NativeRef != diagnostics.NativeRef {
		t.Fatalf("reopened binding diagnostics = %#v, err=%v; want %q", afterRestart, err, diagnostics.NativeRef)
	}
	reopenedHistory, err := h.CanonicalHistory(agent.ID, 10, 0)
	if err != nil || !canonicalHistoryHasTurn(reopenedHistory, dispatched.TurnID, "completed") || !canonicalHistoryHasTurn(reopenedHistory, interruptedTurnID, "interrupted") {
		t.Fatalf("reopened canonical history = %#v, err=%v", reopenedHistory, err)
	}
	if usage, err := h.AgentTokenUsage(agent.ID, 7); err != nil || usage.Lifetime.TotalTokens <= 0 {
		t.Fatalf("reopened real Codex passive usage = %#v, err=%v", usage, err)
	}
	handle := rt.agentHost
	if _, err := h.ArchiveAgent(agent.ID); err != nil {
		t.Fatal(err)
	}
	if handle.Alive() {
		t.Fatal("Archive did not release the per-Agent binding handle")
	}
	h.mu.Lock()
	sharedHostAlive := h.codexHost != nil && !h.codexHost.client.Closed()
	h.mu.Unlock()
	if !sharedHostAlive {
		t.Fatal("per-binding close terminated the shared Codex Host")
	}
}

func exerciseRealRuntimeSteerInterrupt(t *testing.T, h *Hub, agentID string) string {
	t.Helper()
	dispatched, err := h.SendTask(agentID, "Use the shell tool to run sleep 30, then reply with the result.", time.Minute)
	if err != nil {
		t.Fatalf("start harmless interrupt smoke Turn: %v", err)
	}
	h.mu.Lock()
	rt := h.runtimes[agentID]
	meta := h.agents[agentID]
	threadID := ""
	if meta != nil {
		threadID = meta.RuntimeBinding.NativeRef
	}
	h.mu.Unlock()
	if rt == nil {
		t.Fatal("real Runtime disappeared before steer")
	}
	if _, err := h.requestTurnSteer(rt, threadID, dispatched.TurnID, "Keep the current Turn active until interrupted.", 15*time.Second); err != nil {
		t.Fatalf("real Runtime steer: %v", err)
	}
	result, err := h.Interrupt(agentID, "real Runtime harmless interrupt smoke")
	if err != nil || !result.Interrupted {
		t.Fatalf("real Runtime interrupt = %#v, err=%v", result, err)
	}
	terminal := waitRealCodexTerminal(t, h, agentID, dispatched.TurnID, 30*time.Second)
	if terminal.Status != "interrupted" {
		t.Fatalf("real Runtime interrupted terminal = %#v", terminal)
	}
	return dispatched.TurnID
}

func canonicalHistoryHasTurn(history CanonicalHistory, turnID, state string) bool {
	for _, turn := range history.Turns {
		if turn.TurnID == turnID && string(turn.State) == state {
			return true
		}
	}
	return false
}

func waitRealCodexTerminal(t *testing.T, h *Hub, agentID, turnID string, timeout time.Duration) TurnSummary {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		view, err := h.GetAgent(agentID)
		if err != nil {
			t.Fatal(err)
		}
		if view.LastTurn != nil && view.LastTurn.TurnID == turnID {
			return *view.LastTurn
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("real Codex Turn %s did not reach a terminal state within %s", turnID, timeout)
	return TurnSummary{}
}

func waitRealCodexHistory(t *testing.T, h *Hub, agentID, turnID string, timeout time.Duration) CanonicalHistory {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		history, err := h.CanonicalHistory(agentID, 10, 0)
		if err == nil {
			for _, turn := range history.Turns {
				if turn.TurnID == turnID {
					return history
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("real Codex history did not expose Loom Turn %s within %s", turnID, timeout)
	return CanonicalHistory{}
}

func codexRealSmokeAuthUnavailable(message string) bool {
	message = strings.ToLower(message)
	for _, marker := range []string{"authentication", "unauthorized", "not logged in", "login required", "401"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
