package hub

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/store"
)

// TestPiRuntimeHostDriverRealRestartSafeStory is explicitly opt-in. Fake RPC
// tests cover deterministic CI; PI_REAL_BIN proves the official process and
// persisted session can traverse the same Driver/Contract seam.
func TestPiRuntimeHostDriverRealRestartSafeStory(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("PI_REAL_BIN"))
	if bin == "" {
		t.Skip("set PI_REAL_BIN to run the real Pi Runtime Host Driver story")
	}
	t.Setenv("PI_BIN", bin)
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
	if err := h.piDriverLocked().Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent, err := h.CreateAgent(CreateParams{Name: "pi-real-smoke", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := h.GetRuntimeDiagnostics(agent.ID)
	if err != nil || diagnostics.NativeRef == "" {
		t.Fatalf("created Pi binding diagnostics = %#v, err=%v", diagnostics, err)
	}
	dispatched, err := h.SendTask(agent.ID, "Reply with exactly: pi-runtime-host-smoke-ok", 2*time.Minute)
	if err != nil {
		t.Fatalf("PI_REAL_BIN story could not start a Turn (verify model authentication): %v", err)
	}
	terminal := waitRealCodexTerminal(t, h, agent.ID, dispatched.TurnID, 2*time.Minute)
	if terminal.Status != "completed" {
		view, _ := h.GetAgent(agent.ID)
		t.Fatalf("real Pi Turn terminal = %#v, last error=%q", terminal, view.LastError)
	}
	history := waitRealCodexHistory(t, h, agent.ID, dispatched.TurnID, 30*time.Second)
	if len(history.Turns) == 0 {
		t.Fatal("real Pi history is empty")
	}
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
		t.Fatalf("reopened Pi Agent = %#v, err=%v", reopened.Agent, err)
	}
	h.mu.Lock()
	rt, err := h.getRuntimeLocked(h.agents[agent.ID])
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(rt); err != nil {
		t.Fatalf("resume real Pi binding: %v", err)
	}
	afterRestart, err := h.GetRuntimeDiagnostics(agent.ID)
	if err != nil || afterRestart.NativeRef != diagnostics.NativeRef {
		t.Fatalf("reopened Pi diagnostics = %#v, err=%v; want %q", afterRestart, err, diagnostics.NativeRef)
	}
	reopenedHistory, err := h.History(agent.ID, 10, 0)
	if err != nil || len(reopenedHistory.Turns) == 0 || reopenedHistory.Turns[len(reopenedHistory.Turns)-1].ID != dispatched.TurnID {
		t.Fatalf("reopened Pi history = %#v, err=%v", reopenedHistory, err)
	}
	handle := rt.agentHost
	if _, err := h.ArchiveAgent(agent.ID); err != nil {
		t.Fatal(err)
	}
	if handle.Alive() {
		t.Fatal("Archive did not release the real Pi process")
	}
}
