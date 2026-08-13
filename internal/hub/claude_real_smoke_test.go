package hub

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/claudegen"
	"github.com/yan5xu/codex-loom/internal/processlifecycle"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestClaudeCertificationTurnPolicyPreservesExplicitNoTools(t *testing.T) {
	contract := newClaudeRuntimeContract("certification-policy", nil, nil)
	contract.setCertificationTurnPolicy(claudeCertificationTurnPolicy{
		Purpose: "real_smoke", AllowedTools: []string{}, MaxTurns: 1, MaxBudgetUSD: 0.05,
	})
	contract.mu.Lock()
	encoded, err := json.Marshal(contract.certificationPolicy)
	contract.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"allowedTools":[]`) {
		t.Fatalf("no-tool certification policy = %s", encoded)
	}
}

// TestClaudeRuntimeManagedGenerationRealSmoke is deliberately opt-in. It uses
// only the exact active managed generation and an explicit product-safe API
// credential. Ordinary tests neither discover nor consume real credentials.
func TestClaudeRuntimeManagedGenerationRealSmoke(t *testing.T) {
	if strings.TrimSpace(os.Getenv("CLAUDE_REAL_SMOKE")) != "1" {
		t.Skip("set CLAUDE_REAL_SMOKE=1 to run the managed-generation Claude smoke")
	}
	apiKey := strings.TrimSpace(os.Getenv("CLAUDE_REAL_API_KEY"))
	if apiKey == "" {
		t.Skip("CLAUDE_REAL_API_KEY is absent")
	}
	manager := claudegen.Default()
	status := manager.Status(context.Background())
	if (status.Active == nil || status.State != claudegen.StateActive) && strings.TrimSpace(os.Getenv("CLAUDE_REAL_INSTALL")) == "1" {
		if strings.TrimSpace(os.Getenv("CLAUDE_REAL_ACCEPT_INSTALL_TERMS")) != "1" {
			t.Skip("managed generation install was requested without explicit terms acknowledgement")
		}
		if _, err := manager.Install(context.Background(), claudegen.InstallRequest{AcceptTerms: true}); err != nil {
			t.Fatalf("install exact managed Claude generation: %v", err)
		}
		if _, err := manager.Verify(context.Background(), "staged"); err != nil {
			t.Fatalf("verify staged managed Claude generation: %v", err)
		}
		if _, err := manager.Activate(context.Background()); err != nil {
			t.Fatalf("activate exact managed Claude generation: %v", err)
		}
		status = manager.Status(context.Background())
	}
	if status.Active == nil || status.State != claudegen.StateActive {
		t.Skip("the exact managed Claude generation is not active")
	}
	manifest := claudegen.CurrentManifest()
	if status.Active.ID != manifest.ID || status.ProductionReady {
		t.Fatalf("managed generation status = %#v; require exact preview generation with productionReady=false", status)
	}
	t.Setenv("ANTHROPIC_API_KEY", apiKey)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := OpenWithOptions(st, OpenOptions{ClaudeGenerations: manager})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if h != nil {
			h.Shutdown()
		}
		if st != nil {
			_ = st.Close()
		}
	}()
	agent, err := h.CreateAgent(CreateParams{
		Name: "claude-real-preview-smoke", Cwd: t.TempDir(), RuntimeKind: "claude", ApprovalPolicy: "never",
		RuntimeConfiguration: testClaudeRuntimeConfiguration(),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	rt, err := h.getRuntimeLocked(h.agents[agent.ID])
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(rt); err != nil {
		t.Fatal(err)
	}
	contract := rt.runtimeContract.(*claudeRuntimeContract)
	contract.setCertificationTurnPolicy(claudeCertificationTurnPolicy{
		Purpose: "real_smoke", AllowedTools: []string{}, MaxTurns: 1, MaxBudgetUSD: 0.05,
	})
	processID := rt.agentHost.(*claudeAgentHost).bridge.ProcessID()
	if processID <= 0 {
		t.Fatal("managed Claude Bridge has no supervised process-group leader")
	}
	liveEvents, cancelLiveEvents, err := h.Subscribe(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelLiveEvents()

	dispatched, err := h.SendTask(agent.ID, "Do not use tools or write anything. Begin a long plain-text explanation of runtime-neutral ledgers and continue until interrupted.", 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	waitClaudeRealSmokeLiveEvent(t, liveEvents, dispatched.TurnID, `"kind":"assistant_text"`, 90*time.Second)
	receipt, err := h.Interrupt(agent.ID, "Claude managed-generation preview smoke")
	if err != nil || !receipt.Interrupted {
		t.Fatalf("interrupt receipt = %#v, err=%v", receipt, err)
	}
	terminal := waitRealCodexTerminal(t, h, agent.ID, dispatched.TurnID, 90*time.Second)
	if terminal.Status != "interrupted" {
		t.Fatalf("real Claude terminal = %#v", terminal)
	}
	waitClaudeRealSmokeEvent(t, h, agent.ID, dispatched.TurnID, `"kind":"usage"`, 30*time.Second)
	waitClaudeRealSmokeEvent(t, h, agent.ID, dispatched.TurnID, `"kind":"terminal"`, 30*time.Second)
	history := waitRealCodexHistory(t, h, agent.ID, dispatched.TurnID, 30*time.Second)
	turn := history.Turns[len(history.Turns)-1]
	if turn.State != runtimecontract.LifecycleInterrupted || turn.Usage == nil ||
		(!turn.Usage.InputTokens.Available && !turn.Usage.OutputTokens.Available && !turn.Usage.CostMicros.Available) {
		t.Fatalf("typed Claude History/usage = %#v", turn)
	}

	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	assertClaudeProcessGroupStopped(t, processID)
	st, err = store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h, err = OpenWithOptions(st, OpenOptions{ClaudeGenerations: manager})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	reopenedRuntime, err := h.getRuntimeLocked(h.agents[agent.ID])
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(reopenedRuntime); err != nil {
		t.Fatal(err)
	}
	reopened, err := h.CanonicalHistory(agent.ID, 10, 0)
	if err != nil || !canonicalHistoryHasTurn(reopened, dispatched.TurnID, "interrupted") {
		t.Fatalf("Canonical Ledger after fresh Bridge and Store reopen = %#v, err=%v", reopened, err)
	}
	reopenedPID := reopenedRuntime.agentHost.(*claudeAgentHost).bridge.ProcessID()
	h.Shutdown()
	assertClaudeProcessGroupStopped(t, reopenedPID)
}

func waitClaudeRealSmokeLiveEvent(t *testing.T, events <-chan store.Event, turnID, marker string, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("real Claude event stream closed before %s", marker)
			}
			if event.Type == "loom/runtime-event" && strings.Contains(string(event.Data), turnID) && strings.Contains(string(event.Data), marker) {
				return
			}
		case <-timer.C:
			t.Fatalf("real Claude Turn %s did not stream %s within %s", turnID, marker, timeout)
		}
	}
}

func waitClaudeRealSmokeEvent(t *testing.T, h *Hub, agentID, turnID, marker string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events, err := h.ReadEvents(agentID, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type == "loom/runtime-event" && strings.Contains(string(event.Data), turnID) && strings.Contains(string(event.Data), marker) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("real Claude Turn %s did not emit %s within %s", turnID, marker, timeout)
}

func assertClaudeProcessGroupStopped(t *testing.T, processID int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for processlifecycle.GroupAlive(processID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processlifecycle.GroupAlive(processID) {
		t.Fatalf("managed Claude process tree %d survived Driver shutdown", processID)
	}
}
