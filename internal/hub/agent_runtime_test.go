package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/rollout"
	"github.com/yan5xu/codex-loom/internal/store"
)

type fakeAgentRuntime struct {
	created, resumed, started, steered, interrupted bool
	closed                                          bool
	binding                                         string
	history                                         RuntimeHistory
	interruptedNative                               string
}

func (f *fakeAgentRuntime) Alive() bool { return !f.closed }
func (f *fakeAgentRuntime) Create(RuntimeBindingRequest) (string, error) {
	f.created = true
	return f.binding, nil
}
func (f *fakeAgentRuntime) Resume(RuntimeBindingRequest, time.Duration) error {
	f.resumed = true
	return nil
}
func (f *fakeAgentRuntime) InjectDeveloperContext(string, string, time.Duration) error { return nil }
func (f *fakeAgentRuntime) StartTurn(RuntimeTurnRequest) (string, error) {
	f.started = true
	return "native-turn-1", nil
}
func (f *fakeAgentRuntime) Steer(string, string, string, time.Duration) (string, error) {
	f.steered = true
	return "native-turn-1", nil
}
func (f *fakeAgentRuntime) Interrupt(_ string, nativeTurnID string, _ time.Duration) error {
	f.interrupted = true
	f.interruptedNative = nativeTurnID
	return nil
}
func (f *fakeAgentRuntime) NormalizeEvent(string, json.RawMessage) []RuntimeEvent { return nil }
func (f *fakeAgentRuntime) ReadHistory(string, int, int) (RuntimeHistory, error) {
	return f.history, nil
}
func (f *fakeAgentRuntime) ReadTurn(string, string) (RuntimeHistoryTurn, error) {
	return RuntimeHistoryTurn{}, rollout.ErrTurnNotFound
}
func (f *fakeAgentRuntime) LatestTurn(string) (*RuntimeHistoryTurn, error) { return nil, nil }
func (f *fakeAgentRuntime) Capabilities() RuntimeCapabilities {
	return RuntimeCapabilities{History: true, CausalSteer: true, Interrupt: true}
}
func (f *fakeAgentRuntime) Close() { f.closed = true }

func TestHubRoutesCoreExecutionThroughAgentRuntime(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeAgentRuntime{
		binding: "native-thread-1",
		history: RuntimeHistory{Total: 1, Turns: []RuntimeHistoryTurn{{ID: "native-turn-1", Status: "completed"}}},
	}
	h := testHub(st)
	h.agentRuntimeFactory = func(string) (AgentRuntime, error) { return fake, nil }
	h.contextHistoryProbe = func(threadID string, _ rollout.ContextHistoryQuery) (rollout.ContextHistoryState, error) {
		return rollout.ContextHistoryState{EpochID: "initial:" + threadID}, nil
	}

	agent, err := h.CreateAgent(CreateParams{Name: "worker", Cwd: t.TempDir(), RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if !fake.created || agent.RuntimeBinding.Kind != "codex" {
		t.Fatalf("created Agent = %#v, Runtime create=%v", agent.Agent, fake.created)
	}
	result, err := h.SendTask(agent.ID, "do the work", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !fake.resumed || !fake.started || result.TurnID == "" || result.TurnID == "native-turn-1" {
		t.Fatalf("send result = %#v, resumed=%v started=%v", result, fake.resumed, fake.started)
	}
	if got := h.agents[agent.ID].RuntimeTurnBindings[result.TurnID]; got != "native-turn-1" {
		t.Fatalf("durable Runtime Turn binding = %q", got)
	}

	history, err := h.History(agent.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if history.Total != 1 || len(history.Turns) != 1 || history.Turns[0].ID != result.TurnID {
		t.Fatalf("History = %#v", history)
	}

	if _, err := h.Interrupt(agent.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	if !fake.interrupted || fake.interruptedNative != "native-turn-1" {
		t.Fatalf("Interrupt native Turn = %q", fake.interruptedNative)
	}
	h.Shutdown()
	if !fake.closed {
		t.Fatal("Shutdown did not close Agent Runtime")
	}
}

func TestCodexHistoryAndReadTranslateLoomAndNativeTurnIDs(t *testing.T) {
	sessions := t.TempDir()
	nativeThreadID := "native-thread-map"
	nativeTurnID := "native-turn-map"
	loomTurnID := "turn_loom_map"
	day := filepath.Join(sessions, "2026", "08", "10")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(day, "rollout-2026-08-10T00-00-00-"+nativeThreadID+".jsonl")
	rolloutData := `{"timestamp":"2026-08-10T00:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"native-turn-map"}}
{"timestamp":"2026-08-10T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"mapped work"}}
{"timestamp":"2026-08-10T00:00:02Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"native-turn-map"}}
`
	if err := os.WriteFile(rolloutPath, []byte(rolloutData), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SESSIONS_DIR", sessions)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", Cwd: t.TempDir(), ThreadID: "thr-loom", Status: "idle",
		RuntimeBinding:      RuntimeBinding{Kind: "codex", NativeRef: nativeThreadID},
		RuntimeTurnBindings: map[string]string{loomTurnID: nativeTurnID},
	}
	history, err := h.History("agent-1", 10, 0)
	if err != nil || len(history.Turns) != 1 || history.Turns[0].ID != loomTurnID {
		t.Fatalf("mapped History = %#v, err=%v", history, err)
	}
	detail, err := h.GetTurn(loomTurnID)
	if err != nil || detail.ID != loomTurnID {
		t.Fatalf("mapped GetTurn = %#v, err=%v", detail, err)
	}
	legacyDetail, err := h.GetTurn(nativeTurnID)
	if err != nil || legacyDetail.ID != nativeTurnID {
		t.Fatalf("native compatibility GetTurn = %#v, err=%v", legacyDetail, err)
	}
}

func TestCodexRuntimeNormalizesCoreExecutionEvents(t *testing.T) {
	runtime := &codexAgentRuntime{}
	tests := []struct {
		method string
		params string
		kind   RuntimeEventKind
	}{
		{"turn/started", `{"threadId":"native-thread","turn":{"id":"native-turn"}}`, RuntimeTurnStarted},
		{"item/agentMessage/delta", `{"threadId":"native-thread","turnId":"native-turn","itemId":"answer","delta":"hello"}`, RuntimeTextDelta},
		{"item/reasoning/delta", `{"threadId":"native-thread","turnId":"native-turn","itemId":"thought","delta":"hmm"}`, RuntimeReasoningDelta},
		{"item/started", `{"threadId":"native-thread","turnId":"native-turn","item":{"id":"tool","type":"commandExecution"}}`, RuntimeToolStarted},
		{"turn/completed", `{"threadId":"native-thread","turn":{"id":"native-turn","status":"completed"}}`, RuntimeTurnCompleted},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			events := runtime.NormalizeEvent(tt.method, json.RawMessage(tt.params))
			if len(events) != 1 || events[0].Kind != tt.kind || events[0].NativeRef != "native-thread" || events[0].NativeTurnID != "native-turn" {
				t.Fatalf("NormalizeEvent() = %#v", events)
			}
		})
	}
}

func TestHubPublishesNormalizedCoreEventBeforeRawCompatibilityEvent(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "thr-loom",
		RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thr-native"},
	}
	rt := &runtime{agentID: "agent-1", agentRuntime: &codexAgentRuntime{}}
	h.onNotification(rt, "item/agentMessage/delta", json.RawMessage(`{
		"threadId":"thr-native","turnId":"turn-1","itemId":"answer-1","delta":"hello"
	}`))

	events, err := st.ReadEvents("agent-1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != "loom/text-delta" || events[1].Type != "item/agentMessage/delta" {
		t.Fatalf("events = %#v", events)
	}
	var raw map[string]any
	if err := json.Unmarshal(events[1].Data, &raw); err != nil || raw["compatibility"] != true {
		t.Fatalf("raw compatibility event = %s, err=%v", events[1].Data, err)
	}
}
