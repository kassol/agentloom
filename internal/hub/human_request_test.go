package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/store"
)

func TestHumanRequestPersistsAndResumesSameAgentThread(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-one"] = &Agent{
		ID: "agent-one", Name: "one", ThreadID: "thread-one", Status: "running",
		CurrentTurnID: "turn-source", CurrentTask: "Prepare release", CreatedAt: now(), UpdatedAt: now(),
	}
	h.runtimes["agent-one"] = &runtime{activeTurn: &turnState{
		turnID: "turn-source", task: "Prepare release", startedAt: time.Now(), stopWatchdog: make(chan struct{}),
	}}

	request, err := h.CreateHumanRequest(CreateHumanRequestParams{
		Agent: "one", Question: "Which release window should I use?", Context: "Two windows are available.",
		Options: []HumanRequestOption{{Label: "Tonight", Description: "Faster"}, {Label: "Tomorrow", Description: "Lower risk"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.AgentID != "agent-one" || request.ThreadID != "thread-one" || request.SourceTurnID != "turn-source" {
		t.Fatalf("request source = %#v", request)
	}
	if request.BlockedWork != "Prepare release" || request.DeliveryStatus != "waiting" {
		t.Fatalf("request blocking state = %#v", request)
	}

	answered, err := h.AnswerHumanRequest(request.ID, AnswerHumanRequestParams{Answer: "Use tomorrow morning."})
	if err != nil {
		t.Fatal(err)
	}
	if answered.State != "answered" || answered.DeliveryStatus != "queued" {
		t.Fatalf("answered request = %#v", answered)
	}
	// The source Turn is still running, so the asynchronous delivery must wait.
	time.Sleep(20 * time.Millisecond)
	queued, err := h.GetHumanRequest(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.DeliveryStatus != "queued" {
		t.Fatalf("delivery while Agent is busy = %q, want queued", queued.DeliveryStatus)
	}

	restarted := testHub(st)
	restarted.agents["agent-one"] = &Agent{
		ID: "agent-one", Name: "one", ThreadID: "thread-one", Status: "idle", CreatedAt: now(), UpdatedAt: now(),
	}
	if err := restarted.loadHumanRequests(); err != nil {
		t.Fatal(err)
	}
	var deliveredAgent, envelope string
	restarted.dispatchHumanAnswer = func(key, text string) (SendResult, error) {
		deliveredAgent, envelope = key, text
		return SendResult{Dispatched: true, AgentID: key, SessionID: key, TurnID: "turn-resumed"}, nil
	}
	delivered, ok := restarted.deliverAnsweredHumanRequest("agent-one")
	if !ok {
		t.Fatalf("delivery failed: %#v", delivered)
	}
	if deliveredAgent != "agent-one" || delivered.AgentID != "agent-one" || delivered.ThreadID != "thread-one" || delivered.ResumedTurnID != "turn-resumed" || delivered.DeliveryStatus != "delivered" {
		t.Fatalf("delivered request = %#v, agent = %q", delivered, deliveredAgent)
	}
	for _, want := range []string{`request_id="` + request.ID + `"`, `source_turn_id="turn-source"`, "<answer><![CDATA[Use tomorrow morning.]]></answer>"} {
		if !strings.Contains(envelope, want) {
			t.Fatalf("response envelope missing %q:\n%s", want, envelope)
		}
	}
	restarted.mu.Lock()
	source, sourceErr := restarted.turnReferenceLocked("agent-one", "turn-resumed")
	restarted.mu.Unlock()
	if sourceErr != "" || source == nil || source.Kind != "needs_you" || source.ID != request.ID {
		t.Fatalf("resumed Turn source = %#v, err=%q", source, sourceErr)
	}

	reloaded := testHub(st)
	if err := reloaded.loadHumanRequests(); err != nil {
		t.Fatal(err)
	}
	stored, err := reloaded.GetHumanRequest(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Answer != "Use tomorrow morning." || stored.ResumedTurnID != "turn-resumed" || stored.DeliveryStatus != "delivered" {
		t.Fatalf("reloaded request = %#v", stored)
	}
}

func TestHumanRequestRejectsDuplicateAnswer(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-one"] = &Agent{ID: "agent-one", Name: "one", Status: "running", CreatedAt: now(), UpdatedAt: now()}
	request, err := h.CreateHumanRequest(CreateHumanRequestParams{Agent: "one", Expectation: HumanRequestOptional, Question: "Any preference?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.AnswerHumanRequest(request.ID, AnswerHumanRequestParams{Answer: "No preference."}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AnswerHumanRequest(request.ID, AnswerHumanRequestParams{Answer: "Second answer"}); err == nil {
		t.Fatal("duplicate answer succeeded")
	}
}

func TestCreateOrGetHumanRequestPreservesReservedCausality(t *testing.T) {
	h := topicTestHub(t)
	topic := createClipTopic(t, h)

	params := CreateHumanRequestParams{
		Agent: "parall-edge-dev", Question: "Did the upload complete?", Context: "The Runtime stopped after starting the upload.",
	}
	causality := HumanRequestCausality{
		ID: "hrq_recovery_upload", ThreadID: "loom-thread-edge", SourceTurnID: "turn-upload",
		SourceTask: "Upload the release", TopicID: topic.ID,
	}
	created, wasCreated, err := h.CreateOrGetHumanRequest(params, causality)
	if err != nil {
		t.Fatal(err)
	}
	if !wasCreated || created.ID != causality.ID || created.AgentID != "edge" || created.ThreadID != causality.ThreadID {
		t.Fatalf("created request = %#v, wasCreated=%v", created, wasCreated)
	}
	if created.SourceTurnID != causality.SourceTurnID || created.SourceTask != causality.SourceTask || created.TopicID != causality.TopicID || created.BlockedWork != causality.SourceTask {
		t.Fatalf("created causality = %#v", created)
	}

	restarted := testHub(h.st)
	restarted.agents["edge"] = h.agents["edge"]
	restarted.topics = map[string]*Topic{topic.ID: h.topics[topic.ID]}
	if err := restarted.loadHumanRequests(); err != nil {
		t.Fatal(err)
	}
	got, wasCreated, err := restarted.CreateOrGetHumanRequest(params, causality)
	if err != nil {
		t.Fatal(err)
	}
	if wasCreated || got.ID != created.ID || len(restarted.humanRequestOrder) != 1 {
		t.Fatalf("create-or-get duplicate = %#v, wasCreated=%v order=%v", got, wasCreated, restarted.humanRequestOrder)
	}

	conflict := causality
	conflict.SourceTurnID = "turn-other"
	if _, _, err := restarted.CreateOrGetHumanRequest(params, conflict); err == nil || !strings.Contains(err.Error(), "different causality") {
		t.Fatalf("reserved ID conflict error = %v", err)
	}
}
