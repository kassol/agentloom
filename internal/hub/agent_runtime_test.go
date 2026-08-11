package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/rollout"
	"github.com/yan5xu/codex-loom/internal/store"
)

type fakeAgentRuntime struct {
	created, resumed, started, steered, interrupted bool
	closed                                          atomic.Bool
	binding                                         string
	history                                         RuntimeHistory
	historyErr                                      error
	interruptedNative                               string
	steeredNativeRef, steeredExpected, steeredInput string
	capabilities                                    *RuntimeCapabilities
	lastTurnRequest                                 RuntimeTurnRequest
	startErr                                        error
	steerErr, interruptErr                          error
	startCount                                      int
}

func (f *fakeAgentRuntime) Alive() bool { return !f.closed.Load() }
func (f *fakeAgentRuntime) Create(RuntimeBindingRequest) (string, error) {
	f.created = true
	return f.binding, nil
}
func (f *fakeAgentRuntime) Resume(RuntimeBindingRequest, time.Duration) error {
	f.resumed = true
	return nil
}
func (f *fakeAgentRuntime) InjectDeveloperContext(string, string, time.Duration) error { return nil }
func (f *fakeAgentRuntime) StartTurn(request RuntimeTurnRequest) (string, error) {
	f.started = true
	f.startCount++
	f.lastTurnRequest = request
	if f.startErr != nil {
		return "", f.startErr
	}
	return "native-turn-1", nil
}
func (f *fakeAgentRuntime) Steer(nativeRef, expectedNativeTurnID, input string, _ time.Duration) (string, error) {
	f.steered = true
	f.steeredNativeRef, f.steeredExpected, f.steeredInput = nativeRef, expectedNativeTurnID, input
	return expectedNativeTurnID, f.steerErr
}
func (f *fakeAgentRuntime) Interrupt(_ string, nativeTurnID string, _ time.Duration) error {
	f.interrupted = true
	f.interruptedNative = nativeTurnID
	return f.interruptErr
}
func (f *fakeAgentRuntime) NormalizeEvent(string, json.RawMessage) []RuntimeEvent { return nil }
func (f *fakeAgentRuntime) ReadHistory(string, int, int) (RuntimeHistory, error) {
	return f.history, f.historyErr
}
func (f *fakeAgentRuntime) ReadTurn(string, string) (RuntimeHistoryTurn, error) {
	return RuntimeHistoryTurn{}, rollout.ErrTurnNotFound
}
func (f *fakeAgentRuntime) LatestTurn(string) (*RuntimeHistoryTurn, error) { return nil, nil }
func (f *fakeAgentRuntime) Capabilities() RuntimeCapabilities {
	if f.capabilities != nil {
		return *f.capabilities
	}
	return RuntimeCapabilities{History: true, CausalSteer: true, Interrupt: true}
}
func (f *fakeAgentRuntime) Close() { f.closed.Store(true) }

func TestHistorySurfacesRuntimeReadFailure(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeAgentRuntime{historyErr: errors.New("corrupt native history")}
	h := testHub(st)
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "thr-loom", Status: "idle",
		RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "native-session"},
	}
	h.runtimes["agent-1"] = &runtime{agentID: "agent-1", agentRuntime: fake}

	if _, err := h.History("agent-1", 10, 0); err == nil || !strings.Contains(err.Error(), "corrupt native history") {
		t.Fatalf("History error = %v, want Runtime read failure", err)
	}
}

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
	if !fake.closed.Load() {
		t.Fatal("Shutdown did not close Agent Runtime")
	}
}

func TestSendTaskRejectsImageWhenRuntimeDoesNotAdvertiseImageInput(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	capabilities := RuntimeCapabilities{Interrupt: true}
	fake := &fakeAgentRuntime{binding: "/tmp/pi-session.jsonl", capabilities: &capabilities}
	h := testHub(st)
	defer h.Shutdown()
	h.agentRuntimeFactory = func(string) (AgentRuntime, error) { return fake, nil }
	h.contextHistoryProbe = func(threadID string, _ rollout.ContextHistoryQuery) (rollout.ContextHistoryState, error) {
		return rollout.ContextHistoryState{EpochID: "initial:" + threadID}, nil
	}
	agent, err := h.CreateAgent(CreateParams{Name: "pi-worker", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	imageBytes := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
	image, err := h.StageThreadArtifact(agent.ID, "diagram.png", "image/png", bytes.NewReader(imageBytes))
	if err != nil {
		t.Fatal(err)
	}

	_, err = h.SendTaskWithArtifacts(agent.ID, "Inspect the diagram", []string{image.ID}, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "image input") || !strings.Contains(err.Error(), "Pi Runtime") {
		t.Fatalf("unsupported image error = %v", err)
	}
	if fake.started {
		t.Fatal("unsupported image reached the Agent Runtime")
	}
}

func TestArchivePersistenceFailureLeavesActiveRuntimeUntouched(t *testing.T) {
	dir := t.TempDir()
	writable, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := store.OpenWithOptions(dir, store.OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	h := testHub(readOnly)
	fake := &fakeAgentRuntime{binding: "native-binding"}
	turn := &turnState{
		turnID: "turn-active", nativeTurnID: "native-turn", task: "keep working",
		startedAt: time.Now(), lastActivity: time.Now(), stopWatchdog: make(chan struct{}),
	}
	meta := &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "thread-loom", RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "native-binding"},
		Status: "running", CurrentTask: turn.task, CurrentTurnID: turn.turnID, CreatedAt: now(), UpdatedAt: now(),
	}
	rt := &runtime{agentID: meta.ID, agentRuntime: fake, activeTurn: turn, approvals: map[string]*approval{}}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = rt

	if _, err := h.ArchiveAgent(meta.ID); err == nil {
		t.Fatal("Archive succeeded despite registry persistence failure")
	}
	if fake.interrupted || fake.closed.Load() || rt.activeTurn != turn || turn.finished {
		t.Fatalf("failed Loom archive changed Runtime: interrupted=%v closed=%v active=%#v finished=%v", fake.interrupted, fake.closed.Load(), rt.activeTurn, turn.finished)
	}
	if _, err := h.GetAgent(meta.ID); err != nil {
		t.Fatalf("failed Loom archive removed Agent: %v", err)
	}
	turn.finished = true
	close(turn.stopWatchdog)
}

func TestSendTaskBuildsRuntimeImageInputAndFilePathGuidance(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	capabilities := RuntimeCapabilities{Interrupt: true, ImageInput: true}
	fake := &fakeAgentRuntime{binding: "/tmp/pi-session.jsonl", capabilities: &capabilities}
	h := testHub(st)
	defer h.Shutdown()
	h.agentRuntimeFactory = func(string) (AgentRuntime, error) { return fake, nil }
	h.contextHistoryProbe = func(threadID string, _ rollout.ContextHistoryQuery) (rollout.ContextHistoryState, error) {
		return rollout.ContextHistoryState{EpochID: "initial:" + threadID}, nil
	}
	agent, err := h.CreateAgent(CreateParams{Name: "pi-vision", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	imageBytes := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
	image, err := h.StageThreadArtifact(agent.ID, "diagram.png", "image/png", bytes.NewReader(imageBytes))
	if err != nil {
		t.Fatal(err)
	}
	document, err := h.StageThreadArtifact(agent.ID, "brief.pdf", "application/pdf", strings.NewReader("%PDF-1.4\nloom"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.SendTaskWithArtifacts(agent.ID, "Review the inputs", []string{image.ID, document.ID}, time.Minute); err != nil {
		t.Fatal(err)
	}
	var imageInputs []RuntimeInput
	var textInputs []string
	for _, input := range fake.lastTurnRequest.Input {
		switch input.Kind {
		case RuntimeInputLocalImage:
			imageInputs = append(imageInputs, input)
		case RuntimeInputText:
			textInputs = append(textInputs, input.Text)
		}
	}
	if len(imageInputs) != 1 || imageInputs[0].Path != image.Path || imageInputs[0].MimeType != "image/png" {
		t.Fatalf("Runtime image inputs = %#v", imageInputs)
	}
	joinedText := strings.Join(textInputs, "\n")
	if !strings.Contains(joinedText, document.Path) {
		t.Fatalf("generic file path guidance missing from Runtime text: %s", joinedText)
	}
	if strings.Contains(joinedText, `"type":"file"`) || strings.Contains(joinedText, "data:application/pdf") {
		t.Fatalf("generic file was encoded as Runtime content: %s", joinedText)
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
		"threadId":"thr-native","thread":{"id":"thr-native"},"turnId":"turn-1","itemId":"answer-1","delta":"hello"
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
	thread, _ := raw["thread"].(map[string]any)
	if raw["threadId"] != "thr-loom" || thread["id"] != "thr-loom" || raw["turnId"] == "turn-1" || strings.Contains(string(events[0].Data), "nativeTurnId") {
		t.Fatalf("public Runtime events leaked native identity: normalized=%s compatibility=%s", events[0].Data, events[1].Data)
	}
}

func TestRuntimePublicEventsDeeplyRedactNativeIdentityAcrossReopen(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	meta := &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "loom-thread",
		RuntimeBinding:      RuntimeBinding{Kind: "codex", NativeRef: "native-thread-secret"},
		RuntimeTurnBindings: map[string]string{"turn-loom": "native-turn-secret"}, Status: "running", CurrentTurnID: "turn-loom",
	}
	rt := &runtime{
		agentID: meta.ID, approvals: map[string]*approval{},
		activeTurn: &turnState{turnID: "turn-loom", nativeTurnID: "native-turn-secret", startedConfirmed: true, startedAt: time.Now(), stopWatchdog: make(chan struct{})},
	}
	h.agents[meta.ID], h.runtimes[meta.ID] = meta, rt
	h.onNotification(rt, "item/completed", json.RawMessage(`{
		"threadId":"native-thread-secret","turnId":"native-turn-secret","itemId":"native-item-secret",
		"sessionId":"native-session-secret","sessionPath":"/private/native-session-secret.jsonl",
		"item":{"id":"native-item-secret","type":"commandExecution","command":"printf safe","status":"completed","output":"safe output",
			"toolCallId":"native-tool-secret","callId":"native-call-secret","diagnostic":{"nativeThreadId":"native-thread-secret","nativeTurnId":"native-turn-secret","sessionFile":"/private/native-session-secret.jsonl"}}
	}`))

	assertPublicRuntimeEvents := func(events []store.Event) {
		t.Helper()
		encoded, _ := json.Marshal(events)
		for _, secret := range []string{"native-thread-secret", "native-turn-secret", "native-item-secret", "native-session-secret", "native-tool-secret", "native-call-secret", "/private/native-session-secret.jsonl"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("public Runtime event leaked %q: %s", secret, encoded)
			}
		}
		for _, actionable := range []string{"printf safe", "completed", "safe output", "loom-thread", "turn-loom"} {
			if !strings.Contains(string(encoded), actionable) {
				t.Fatalf("public Runtime event omitted %q: %s", actionable, encoded)
			}
		}
	}
	events, err := st.ReadEvents(meta.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertPublicRuntimeEvents(events)
	h.stopping = true
	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	events, err = reopened.ReadEvents(meta.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertPublicRuntimeEvents(events)
}
