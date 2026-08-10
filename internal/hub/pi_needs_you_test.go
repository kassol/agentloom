package hub

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/store"
)

func TestPiNeedsYouAnswerSurvivesRestartAndResumesSameThread(t *testing.T) {
	configureFakePiHubRPC(t, "persistence")
	dataDir := t.TempDir()

	st1, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h1 := New(st1)
	agent, err := h1.CreateAgent(CreateParams{Name: "pi-owner-question", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := h1.SendTask(agent.ID, "Prepare the release without guessing the window", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request, err := h1.CreateHumanRequest(CreateHumanRequestParams{
		Agent: agent.ID, Expectation: HumanRequestRequired,
		Question: "Which release window should I use?", Context: "Both windows passed validation.",
		BlockedWork: "Prepare the release", Options: []HumanRequestOption{{Label: "Tonight"}, {Label: "Tomorrow"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.AgentID != agent.ID || request.ThreadID != agent.ThreadID || request.SourceTurnID != first.TurnID {
		t.Fatalf("Pi Needs You causality = %#v, Agent=%q Thread=%q Turn=%q", request, agent.ID, agent.ThreadID, first.TurnID)
	}
	// Model an operator restart while the source Turn is finishing. The answer
	// remains durable, but drain mode must not start its continuation until the
	// reopened Hub owns the Runtime.
	h1.BeginDrain()
	answered, err := h1.AnswerHumanRequest(request.ID, AnswerHumanRequestParams{Answer: "Use tomorrow morning."})
	if err != nil {
		t.Fatal(err)
	}
	if answered.DeliveryStatus != "queued" {
		t.Fatalf("answer delivery before source Turn settles = %#v", answered)
	}
	time.Sleep(20 * time.Millisecond)
	queued, err := h1.GetHumanRequest(request.ID)
	if err != nil || queued.DeliveryStatus != "queued" {
		t.Fatalf("answer was not durably queued while Pi was running: %#v, err=%v", queued, err)
	}
	waitForPiTurn(t, h1, agent.ID, first.TurnID)
	nativeSession := h1.agents[agent.ID].RuntimeBinding.NativeRef
	h1.Shutdown()
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h2 := New(st2)
	defer func() {
		h2.Shutdown()
		_ = st2.Close()
	}()

	var delivered HumanRequest
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		delivered, err = h2.GetHumanRequest(request.ID)
		if err == nil && delivered.DeliveryStatus == "delivered" && delivered.ResumedTurnID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || delivered.DeliveryStatus != "delivered" || delivered.ResumedTurnID == "" {
		t.Fatalf("restarted Pi answer delivery = %#v, err=%v", delivered, err)
	}
	if delivered.AgentID != agent.ID || delivered.ThreadID != agent.ThreadID || delivered.SourceTurnID != first.TurnID || delivered.ResumedTurnID == first.TurnID {
		t.Fatalf("resumed Pi causal chain = %#v", delivered)
	}
	waitForPiTurn(t, h2, agent.ID, delivered.ResumedTurnID)

	reopened, err := h2.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.ThreadID != agent.ThreadID || h2.agents[agent.ID].RuntimeBinding.NativeRef != nativeSession {
		t.Fatalf("Pi identity changed across Needs You resume: Agent=%#v native=%q, want Thread=%q native=%q", reopened.Agent, h2.agents[agent.ID].RuntimeBinding.NativeRef, agent.ThreadID, nativeSession)
	}
	h2.mu.Lock()
	source, sourceErr := h2.turnReferenceLocked(agent.ID, delivered.ResumedTurnID)
	h2.mu.Unlock()
	if sourceErr != "" || source == nil || source.Kind != "needs_you" || source.ID != request.ID {
		t.Fatalf("resumed Pi Turn source = %#v, err=%q", source, sourceErr)
	}
	history, err := h2.History(agent.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	encodedHistory, _ := json.Marshal(history.Turns)
	if history.Total != 2 || len(history.Turns) != 2 || history.Turns[0].ID != first.TurnID || history.Turns[1].ID != delivered.ResumedTurnID || !strings.Contains(string(encodedHistory), "hello from Pi") {
		t.Fatalf("resumed Pi history/final response = %s", encodedHistory)
	}
	prompts, err := os.ReadFile(os.Getenv("FAKE_PI_PROMPTS_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(prompts), "\n--- prompt ---\n")
	latest := parts[len(parts)-1]
	for _, want := range []string{request.ID, first.TurnID, "Prepare the release", "Use tomorrow morning."} {
		if !strings.Contains(latest, want) {
			t.Fatalf("resumed Pi prompt missing %q:\n%s", want, latest)
		}
	}
}
