package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/store"
)

func TestAgentEventIsMultiplexedToGlobalSubscribers(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	defer h.Shutdown()
	global, cancel := h.SubscribeGlobal()
	defer cancel()

	h.mu.Lock()
	local := h.emitLocked("agent-1", "item/completed", map[string]any{"item": map[string]any{"id": "answer-1"}})
	h.mu.Unlock()

	select {
	case event := <-global:
		if event.Type != "loom/thread-event" {
			t.Fatalf("global event type = %q", event.Type)
		}
		if event.Seq <= 0 {
			t.Fatalf("global event has no durable cursor: %#v", event)
		}
		var payload struct {
			AgentID string      `json:"agentId"`
			Event   store.Event `json:"event"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.AgentID != "agent-1" || payload.Event.Seq != local.Seq || payload.Event.Type != local.Type {
			t.Fatalf("multiplexed payload = %#v, local = %#v", payload, local)
		}
		replayed, err := h.ReadGlobalEvents(event.Seq-1, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(replayed) != 1 || replayed[0].Seq != event.Seq || replayed[0].Type != event.Type {
			t.Fatalf("global replay = %#v, want cursor %d", replayed, event.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("global subscriber did not receive Agent event")
	}
}

func TestAgentStatusIncludesLiveRuntimeCapabilities(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	defer h.Shutdown()
	global, cancel := h.SubscribeGlobal()
	defer cancel()
	capabilities := RuntimeCapabilities{History: true, ImageInput: true}
	meta := &Agent{ID: "agent-1", Name: "worker", RuntimeBinding: RuntimeBinding{Kind: "pi"}}
	h.mu.Lock()
	h.runtimes[meta.ID] = &runtime{agentID: meta.ID, agentRuntime: &fakeAgentRuntime{capabilities: &capabilities}}
	h.emitStatusLocked(meta, "idle")
	h.mu.Unlock()

	select {
	case event := <-global:
		var payload struct {
			ProcessAlive        bool                `json:"processAlive"`
			RuntimeCapabilities RuntimeCapabilities `json:"runtimeCapabilities"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if !payload.ProcessAlive || !payload.RuntimeCapabilities.History || !payload.RuntimeCapabilities.ImageInput {
			t.Fatalf("runtime status payload = %#v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("global subscriber did not receive Agent status")
	}
}

func TestRuntimeFailureDuringShutdownIsIgnored(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	meta := &Agent{
		ID: "agent-1", Name: "worker", Status: "idle", RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "/tmp/pi.jsonl"},
		LastTurn: &TurnSummary{TurnID: "turn-1", Status: "completed"},
	}
	rt := &runtime{agentID: meta.ID, agentRuntime: &fakeAgentRuntime{}}
	h.agents[meta.ID] = meta
	h.runtimes[meta.ID] = rt
	h.stopping = true

	h.onRuntimeFailure(rt, errors.New("Pi RPC process exited: signal: interrupt"))

	if meta.LastError != "" {
		t.Fatalf("shutdown Runtime failure persisted as LastError: %q", meta.LastError)
	}
}

func TestOpenRepairsPersistedPiShutdownInterrupt(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", filepath.Join(t.TempDir(), "missing.json"))
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAgents(map[string]*Agent{
		"agent-1": {
			ID: "agent-1", Name: "worker", Cwd: t.TempDir(), ThreadID: "loom-thread-1", Status: "idle",
			RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "/tmp/pi.jsonl"},
			LastError:      "Pi RPC process exited: signal: interrupt",
			LastTurn:       &TurnSummary{TurnID: "turn-1", Status: "completed"},
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
	if view.LastError != "" {
		t.Fatalf("repaired LastError = %q", view.LastError)
	}
}

func TestCompletedNotificationWithFailedTurnStatusProjectsFailure(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.stopping = true
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", ThreadID: "loom-thread-1", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thread-1"}, Status: "running",
		RuntimeTurnBindings: map[string]string{"turn-loom-1": "turn-1"},
		CurrentTurnID:       "turn-loom-1", CurrentTask: "Do work", CreatedAt: now(), UpdatedAt: now(),
	}
	rt := &runtime{
		agentID:   "agent-1",
		approvals: map[string]*approval{},
		activeTurn: &turnState{
			turnID: "turn-loom-1", nativeTurnID: "turn-1", task: "Do work", startedAt: time.Now(), stopWatchdog: make(chan struct{}),
		},
	}
	h.onNotification(rt, "turn/completed", json.RawMessage(`{
		"threadId":"thread-1",
		"turn":{"id":"turn-1","status":"failed","error":{"message":"model is unavailable"}}
	}`))

	meta := h.agents["agent-1"]
	if meta.Status != "idle" || meta.LastError != "model is unavailable" {
		t.Fatalf("agent failure projection = %#v", meta)
	}
	if meta.LastTurn == nil || meta.LastTurn.Status != "failed" || meta.LastTurn.TurnID != "turn-loom-1" {
		t.Fatalf("last turn = %#v", meta.LastTurn)
	}
	events, err := st.ReadEvents("agent-1", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type == "loom/turn-failed" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("events do not contain loom/turn-failed: %#v", events)
	}
}

func TestTurnStartedNotificationBindsNativeIDWithoutRewritingLoomCausality(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.stopping = true
	h.attempts = map[string]*HandlingAttempt{}
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "research", ThreadID: "loom-thread-1", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thread-1"}, Status: "running",
		CurrentTurnID: "turn-loom", CurrentTask: "Investigate", CreatedAt: now(), UpdatedAt: now(),
	}
	h.attempts["att-1"] = &HandlingAttempt{
		ID: "att-1", InboxItemID: "inb-1", AgentID: "agent-1", Status: "running", TurnID: "turn-loom", StartedAt: now(),
	}
	h.comms["msg-1"] = &AgentMessage{
		ID: "msg-1", ToAgentID: "agent-1", DeliveryStatus: "delivered", DeliveredTurnID: "turn-loom",
		HandlingStatus: "running", ActiveHandlingID: "matt-1", UpdatedAt: now(),
		HandlingAttempts: []AgentMessageHandlingAttempt{{
			ID: "matt-1", TurnID: "turn-loom", Status: "running", StartedAt: now(),
		}},
	}
	turn := &turnState{
		turnID: "turn-loom", nativeTurnID: "turn-stale", task: "Investigate", source: "internal", attemptID: "att-1",
		agentMessageID: "msg-1", handlingAttemptID: "matt-1", startedAt: time.Now(),
		stopWatchdog: make(chan struct{}),
	}
	rt := &runtime{agentID: "agent-1", approvals: map[string]*approval{}, activeTurn: turn}
	h.runtimes["agent-1"] = rt

	h.onNotification(rt, "turn/started", json.RawMessage(`{
		"threadId":"thread-1","turn":{"id":"turn-actual","status":"inProgress"}
	}`))

	if rt.activeTurn != turn || turn.turnID != "turn-loom" || turn.nativeTurnID != "turn-actual" || !turn.startedConfirmed {
		t.Fatalf("active Turn = %#v, want stable Loom Turn bound to turn-actual", rt.activeTurn)
	}
	if turn.task != "Investigate" || turn.source != "internal" {
		t.Fatalf("native binding update lost local work context: %#v", turn)
	}
	if got := h.agents["agent-1"].CurrentTurnID; got != "turn-loom" {
		t.Fatalf("Agent current Turn = %q", got)
	}
	if got := h.agents["agent-1"].RuntimeTurnBindings["turn-loom"]; got != "turn-actual" {
		t.Fatalf("native Turn binding = %q", got)
	}
	if got := h.attempts["att-1"].TurnID; got != "turn-loom" {
		t.Fatalf("Inbox attempt Turn = %q", got)
	}
	message := h.comms["msg-1"]
	if message.DeliveredTurnID != "turn-loom" || len(message.HandlingAttempts) != 1 || message.HandlingAttempts[0].TurnID != "turn-loom" {
		t.Fatalf("message handling lost Loom causality: %#v", message)
	}
}

func TestStaleTerminalNotificationDoesNotFinishCurrentTurn(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.stopping = true
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "research", ThreadID: "loom-thread-1", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thread-1"}, Status: "running",
		RuntimeTurnBindings: map[string]string{"turn-current": "native-turn-current"},
		CurrentTurnID:       "turn-current", CurrentTask: "Current work", CreatedAt: now(), UpdatedAt: now(),
	}
	turn := &turnState{
		turnID: "turn-current", nativeTurnID: "native-turn-current", startedConfirmed: true, task: "Current work", source: "owner",
		startedAt: time.Now(), stopWatchdog: make(chan struct{}),
	}
	rt := &runtime{agentID: "agent-1", approvals: map[string]*approval{}, activeTurn: turn}
	h.runtimes["agent-1"] = rt

	h.onNotification(rt, "turn/completed", json.RawMessage(`{
		"threadId":"thread-1","turn":{"id":"turn-previous","status":"completed"}
	}`))

	if rt.activeTurn != turn || turn.finished {
		t.Fatalf("stale terminal event finished current Turn: %#v", rt.activeTurn)
	}
	if meta := h.agents["agent-1"]; meta.Status != "running" || meta.CurrentTurnID != "turn-current" || meta.LastTurn != nil {
		t.Fatalf("stale terminal event changed Agent projection: %#v", meta)
	}
}

func TestActiveTurnInterruptMismatch(t *testing.T) {
	actual, ok := activeTurnInterruptMismatch(errors.New("expected active turn id turn-old but found turn-current"))
	if !ok || actual != "turn-current" {
		t.Fatalf("parsed mismatch = %q, %v", actual, ok)
	}
	for _, message := range []string{
		"some other interrupt failure",
		"expected active turn id turn-old",
		"expected active turn id turn-old but found invalid turn",
	} {
		if actual, ok := activeTurnInterruptMismatch(errors.New(message)); ok {
			t.Fatalf("unexpected mismatch parse for %q: %q", message, actual)
		}
	}
}

func TestRestoreAgentKeepsStableIdentityAndDoesNotStartRuntime(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	defer h.Shutdown()

	view, err := h.RestoreAgent(RestoreAgentParams{
		ID: "a07193ea", Name: "parall-edge-dev", Cwd: "/tmp/parall-edge",
		ThreadID: "loom-thread-restored", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "019f53a7-5485-7733-87f8-5b513420f62a"},
		Model: "gpt-5.6-sol", Effort: "high",
		CreatedAt: "2026-07-12T00:08:21Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "a07193ea" || view.ThreadID != "loom-thread-restored" || view.RuntimeBinding.Kind != "codex" || view.RuntimeBinding.NativeRef != "" {
		t.Fatalf("restored identity = %#v", view.Agent)
	}
	if view.Status != "idle" || view.ProcessAlive || view.CurrentTurnID != "" || view.CurrentTask != "" {
		t.Fatalf("restored runtime state = %#v", view)
	}

	var persisted map[string]*Agent
	if err := st.LoadAgents(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted["a07193ea"] == nil || persisted["a07193ea"].Name != "parall-edge-dev" {
		t.Fatalf("persisted agents = %#v", persisted)
	}
	if _, err := h.RestoreAgent(RestoreAgentParams{
		ID: "a07193ea", Name: "duplicate", Cwd: "/tmp/duplicate", ThreadID: "loom-thread-duplicate", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thread-duplicate"},
	}); err == nil {
		t.Fatal("duplicate stable id restore succeeded")
	}
}

func TestOpenRejectsCorruptRegistryWithoutOverwritingIt(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", filepath.Join(t.TempDir(), "missing.json"))
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("{not-json\n")
	if err := os.WriteFile(filepath.Join(dataDir, "agents.json"), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(st); err == nil {
		t.Fatal("Open accepted a corrupt Agent registry")
	}
	got, err := os.ReadFile(filepath.Join(dataDir, "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(corrupt) {
		t.Fatalf("corrupt registry was overwritten: %q", got)
	}
}

func TestOpenRejectsLegacyAgentRegistryWithRecreateRequiredError(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "agents.json"), []byte(`{"legacy":{"id":"legacy","name":"legacy","cwd":"/tmp","threadId":"codex-thread"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(st)
	if err == nil || !strings.Contains(err.Error(), "recreate") {
		t.Fatalf("Open legacy registry error = %v, want recreate-required error", err)
	}
}

func TestOpenMigratesCurrentRuntimeBindingSchemaOnceWithoutChangingLoomIdentity(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", filepath.Join(t.TempDir(), "missing.json"))
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	current := `{"agent-1":{"id":"agent-1","name":"worker","cwd":"/tmp","threadId":"thr-loom","runtimeBinding":{"kind":"pi","nativeRef":"/tmp/pi-session.jsonl"},"runtimeTurnBindings":{"turn-loom":"entry-native"},"status":"idle","createdAt":"2026-08-10T00:00:00Z","updatedAt":"2026-08-10T00:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(dataDir, "agents.json"), []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	view, err := h.GetAgent("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "agent-1" || view.ThreadID != "thr-loom" || view.CurrentTurnID != "" {
		t.Fatalf("migrated Loom identity = %#v", view.Agent)
	}
	diagnostics, err := h.GetRuntimeDiagnostics("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.NativeRef != "/tmp/pi-session.jsonl" || diagnostics.TurnBindings["turn-loom"] != "entry-native" {
		t.Fatalf("migrated Runtime Binding = %#v", diagnostics)
	}
	h.Shutdown()

	var persisted map[string]*Agent
	if err := st.LoadAgents(&persisted); err != nil {
		t.Fatal(err)
	}
	if got := persisted["agent-1"].RuntimeBinding.SchemaVersion; got != RuntimeBindingSchemaVersion {
		t.Fatalf("Runtime Binding schema version = %d, want %d", got, RuntimeBindingSchemaVersion)
	}
	first, err := os.ReadFile(filepath.Join(dataDir, "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
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
	h.Shutdown()
	second, err := os.ReadFile(filepath.Join(dataDir, "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Fatalf("idempotent reopen rewrote agents.json:\nfirst:  %s\nsecond: %s", first, second)
	}
}

func TestOpenMigratesRuntimeBindingSchemaV1AndPreservesControlPlaneIDs(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", filepath.Join(t.TempDir(), "missing.json"))
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	agent := &Agent{
		ID: "agent-1", Name: "worker", Cwd: "/tmp", ThreadID: "thr-loom", Status: "idle",
		RuntimeBinding:      RuntimeBinding{SchemaVersion: 1, Kind: "codex", NativeRef: "native-thread"},
		RuntimeTurnBindings: map[string]string{"turn-loom": "native-turn"},
		CreatedAt:           "2026-08-10T00:00:00Z", UpdatedAt: "2026-08-10T00:00:00Z",
	}
	if err := st.SaveAgents(map[string]*Agent{agent.ID: agent}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveProfiles(map[string]*AgentProfile{agent.ID: {AgentID: agent.ID, Identity: "runtime specialist", Version: 1}}); err != nil {
		t.Fatal(err)
	}
	message := AgentMessage{
		ID: "msg-1", FromAgentID: schedulerAgentID, ToAgentID: agent.ID, From: schedulerIdentity, To: agent.Name,
		Subject: "review", Body: "inspect", Response: "none", Status: "closed", Resolution: "no_reply",
		DeliveryStatus: "delivered", CreatedAt: agent.CreatedAt, UpdatedAt: agent.UpdatedAt,
	}
	if err := st.AppendComm(commRecord{Message: message}); err != nil {
		t.Fatal(err)
	}
	topic := &Topic{
		ID: "tpc-1", Title: "Runtime contract", Purpose: "preserve continuity", CompletionBoundary: "migration complete",
		Status: TopicStatusActive, ResponsibleAgentID: agent.ID, ResponsibleAgent: agent.Name,
		Participants:    []TopicParticipant{{AgentID: agent.ID, Agent: agent.Name, Responsibility: "own", JoinedAt: agent.CreatedAt}},
		CurrentBrief:    TopicBrief{Version: 1, Summary: "ready", UpdatedBy: agent.Name, UpdatedAt: agent.UpdatedAt},
		BriefHistory:    []TopicBrief{{Version: 1, Summary: "ready", UpdatedBy: agent.Name, UpdatedAt: agent.UpdatedAt}},
		DeliveryCursors: map[string]int64{}, Version: 1, CreatedBy: agent.Name, CreatedAt: agent.CreatedAt, UpdatedAt: agent.UpdatedAt,
	}
	if err := st.SaveTopics(map[string]*Topic{topic.ID: topic}); err != nil {
		t.Fatal(err)
	}
	request := HumanRequest{
		ID: "hrq-1", AgentID: agent.ID, AgentName: agent.Name, ThreadID: agent.ThreadID, SourceTurnID: "turn-loom", TopicID: topic.ID,
		Expectation: HumanRequestRequired, Question: "continue?", State: "open", DeliveryStatus: "waiting", CreatedAt: agent.CreatedAt, UpdatedAt: agent.UpdatedAt,
	}
	if err := st.AppendHumanRequest(request); err != nil {
		t.Fatal(err)
	}

	assertControlPlane := func(h *Hub) {
		t.Helper()
		view, err := h.GetAgent(agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		profile, profileErr := h.GetProfile(agent.ID)
		gotMessage, messageErr := h.GetAgentMessage(message.ID)
		gotTopic, topicErr := h.GetTopic(topic.ID)
		gotRequest, requestErr := h.GetHumanRequest(request.ID)
		if profileErr != nil || messageErr != nil || topicErr != nil || requestErr != nil {
			t.Fatalf("read migrated control plane: profile=%v message=%v topic=%v request=%v", profileErr, messageErr, topicErr, requestErr)
		}
		if view.ID != agent.ID || view.ThreadID != agent.ThreadID || profile.AgentID != agent.ID || gotMessage.ID != message.ID || gotMessage.ToAgentID != agent.ID || gotTopic.ID != topic.ID || gotTopic.ResponsibleAgentID != agent.ID || gotRequest.ID != request.ID || gotRequest.AgentID != agent.ID || gotRequest.ThreadID != agent.ThreadID || gotRequest.SourceTurnID != "turn-loom" || gotRequest.TopicID != topic.ID {
			t.Fatalf("control-plane identities changed: agent=%q thread=%q profile=%q message=%#v topic=%#v request=%#v", view.ID, view.ThreadID, profile.AgentID, gotMessage, gotTopic, gotRequest)
		}
		if h.agents[agent.ID].RuntimeBinding.SchemaVersion != RuntimeBindingSchemaVersion || h.agents[agent.ID].RuntimeTurnBindings["turn-loom"] != "native-turn" {
			t.Fatalf("migrated Agent = %#v", h.agents[agent.ID])
		}
	}

	var firstRegistry []byte
	for open := 0; open < 2; open++ {
		h, err := Open(st)
		if err != nil {
			t.Fatal(err)
		}
		assertControlPlane(h)
		h.Shutdown()
		registry, err := os.ReadFile(filepath.Join(dataDir, "agents.json"))
		if err != nil {
			t.Fatal(err)
		}
		if open == 0 {
			firstRegistry = registry
		} else if string(registry) != string(firstRegistry) {
			t.Fatalf("reopen rewrote migrated registry:\nfirst: %s\nnext:  %s", firstRegistry, registry)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		if open == 0 {
			st, err = store.Open(dataDir)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestOpenRejectsUnsupportedRuntimeBindingSchemaVersions(t *testing.T) {
	for _, version := range []int{-1, RuntimeBindingSchemaVersion + 1} {
		t.Run(fmt.Sprintf("version-%d", version), func(t *testing.T) {
			dataDir := t.TempDir()
			st, err := store.Open(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			registry := fmt.Sprintf(`{"agent-1":{"id":"agent-1","name":"worker","cwd":"/tmp","threadId":"thr-loom","runtimeBinding":{"schemaVersion":%d,"kind":"pi","nativeRef":"native"},"status":"idle"}}`, version)
			if err := os.WriteFile(filepath.Join(dataDir, "agents.json"), []byte(registry), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(st); err == nil || !strings.Contains(err.Error(), "unsupported Runtime Binding schema version") {
				t.Fatalf("Open schema version %d error = %v", version, err)
			}
		})
	}
}

func TestOpenReportsFutureRuntimeBindingSchemaBeforeCurrentShapeValidation(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	registry := fmt.Sprintf(`{"agent-1":{"id":"agent-1","name":"future","runtimeBinding":{"schemaVersion":%d}}}`, RuntimeBindingSchemaVersion+1)
	if err := os.WriteFile(filepath.Join(dataDir, "agents.json"), []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(st)
	if err == nil || !strings.Contains(err.Error(), "unsupported Runtime Binding schema version") {
		t.Fatalf("future schema error = %v", err)
	}
	if strings.Contains(err.Error(), "legacy") {
		t.Fatalf("future schema was misclassified as legacy: %v", err)
	}
}

func TestPassiveOpenNormalizesRuntimeBindingSchemaWithoutWriting(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", filepath.Join(t.TempDir(), "missing.json"))
	dataDir := t.TempDir()
	writable, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	registry := []byte(`{"agent-1":{"id":"agent-1","name":"worker","cwd":"/tmp","threadId":"thr-loom","runtimeBinding":{"kind":"pi","nativeRef":"native"},"status":"idle"}}`)
	if err := os.WriteFile(filepath.Join(dataDir, "agents.json"), registry, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	readonly, err := store.OpenWithOptions(dataDir, store.OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	h, err := OpenWithOptions(readonly, OpenOptions{Passive: true})
	if err != nil {
		t.Fatal(err)
	}
	if h.agents["agent-1"].RuntimeBinding.SchemaVersion != RuntimeBindingSchemaVersion {
		t.Fatalf("passive in-memory schema version = %d", h.agents["agent-1"].RuntimeBinding.SchemaVersion)
	}
	h.Shutdown()
	after, err := os.ReadFile(filepath.Join(dataDir, "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(registry) {
		t.Fatalf("passive Open wrote registry: before=%s after=%s", registry, after)
	}
}

func TestRuntimeBindingMigrationFailureLeavesCanonicalRegistryUntouched(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", filepath.Join(t.TempDir(), "missing.json"))
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	registry := []byte(`{"agent-1":{"id":"agent-1","name":"worker","cwd":"/tmp","threadId":"thr-loom","runtimeBinding":{"kind":"pi","nativeRef":"native"},"status":"idle"}}`)
	if err := os.WriteFile(filepath.Join(dataDir, "agents.json"), registry, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dataDir, "sessions.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(st); err == nil {
		t.Fatal("Open succeeded when Runtime Binding migration could not be saved")
	}
	after, err := os.ReadFile(filepath.Join(dataDir, "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(registry) {
		t.Fatalf("failed migration changed canonical registry: before=%s after=%s", registry, after)
	}
}

func TestUpdateAgentConfigRollsBackWhenRegistryCommitFails(t *testing.T) {
	t.Setenv("PINIX_EDGE_NAMES", filepath.Join(t.TempDir(), "missing.json"))
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	defer h.Shutdown()
	if _, err := h.RestoreAgent(RestoreAgentParams{
		ID: "agent-1", Name: "before", Cwd: "/tmp", ThreadID: "loom-thread-1", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thread-1"},
	}); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(dataDir, "agents.json")
	if err := os.Remove(registry); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.RemoveAll(registry)
	}()

	rename := "after"
	if _, err := h.UpdateAgentConfig("agent-1", ConfigParams{Name: &rename}); err == nil {
		t.Fatal("config update succeeded after registry commit failure")
	}
	view, err := h.GetAgent("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Name != "before" {
		t.Fatalf("in-memory Agent name = %q, want rollback to before", view.Name)
	}
}

func TestUpdateAgentConfigRejectsProviderChangeForBoundThread(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "worker", Cwd: t.TempDir(), ThreadID: "loom-thread-1", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thread-1"},
		ProviderID: "deepseek", Model: "deepseek-v4-flash", Status: "idle",
	}

	providerID := "openrouter"
	_, err = h.UpdateAgentConfig("agent-1", ConfigParams{ProviderID: &providerID})
	if err == nil || !strings.Contains(err.Error(), "Provider switch operation") {
		t.Fatalf("UpdateAgentConfig Provider change error = %v", err)
	}
	if h.agents["agent-1"].ProviderID != "deepseek" {
		t.Fatalf("Provider binding changed after rejection: %#v", h.agents["agent-1"])
	}
}

func TestImportEdgeSkipsAliasForOwnedThread(t *testing.T) {
	edgeFile := filepath.Join(t.TempDir(), "names.json")
	if err := os.WriteFile(edgeFile, []byte(`{
  "old-edge-name": {"threadId":"thread-shared","cwd":"/edge"},
  "other-edge-name": {"threadId":"thread-other","cwd":"/other"}
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PINIX_EDGE_NAMES", edgeFile)

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAgents(map[string]*Agent{
		"owned": {
			ID: "owned", Name: "renamed-in-loom", ThreadID: "loom-thread-shared", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thread-shared"}, Cwd: "/owned",
			Sandbox: "danger-full-access", ApprovalPolicy: "never", Status: "idle",
		},
	}); err != nil {
		t.Fatal(err)
	}

	h := New(st)
	defer h.Shutdown()
	agents := h.ListAgents()
	if len(agents) != 2 {
		t.Fatalf("agents = %#v, want owned Agent plus one distinct edge Agent", agents)
	}
	for _, agent := range agents {
		if agent.Name == "old-edge-name" {
			t.Fatalf("edge alias for owned Thread was imported: %#v", agent)
		}
	}
}

func TestApplyRolloutStatusShowsRecentExternalRunningTurn(t *testing.T) {
	const threadID = "test-thread-recent-running"
	dir := t.TempDir()
	writeTestRollout(t, dir, threadID, time.Now().UTC().Format(time.RFC3339Nano))
	t.Setenv("CODEX_SESSIONS_DIR", dir)

	view := AgentView{Agent: Agent{ThreadID: "loom-" + threadID, Status: "idle"}, nativeRuntimeRef: threadID}
	applyRolloutStatus(&view)

	if view.Status != "running" {
		t.Fatalf("status = %q, want running", view.Status)
	}
	if view.CurrentTurnID != "turn-running" || view.CurrentTask != "keep working" {
		t.Fatalf("view = %#v, want current running turn", view)
	}
}

func TestApplyRolloutStatusSummarizesCompletedTopicControlEnvelope(t *testing.T) {
	const threadID = "test-thread-topic-display"
	dir := t.TempDir()
	day := filepath.Join(dir, "2026", "07", "20")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := `<loom_topic_context version="1"><brief><summary>Internal state</summary></brief></loom_topic_context>
<owner_topic_input version="1"><message><![CDATA[Verify the visible Topic task.]]></message></owner_topic_input>`
	records := []map[string]any{
		{"timestamp": "2026-07-20T01:00:00Z", "type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": "turn-topic"}},
		{"timestamp": "2026-07-20T01:00:01Z", "type": "event_msg", "payload": map[string]any{"type": "user_message", "message": prompt}},
		{"timestamp": "2026-07-20T01:00:02Z", "type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": "turn-topic"}},
	}
	var data []byte
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	path := filepath.Join(day, "rollout-2026-07-20T01-00-00-"+threadID+".jsonl")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SESSIONS_DIR", dir)

	view := AgentView{Agent: Agent{ThreadID: "loom-" + threadID, Status: "idle"}, nativeRuntimeRef: threadID}
	applyRolloutStatus(&view)
	if view.LastTurn == nil || view.LastTurn.Task != "Verify the visible Topic task." {
		t.Fatalf("last Turn = %#v", view.LastTurn)
	}
}

func TestApplyRolloutStatusMarksStaleExternalRunningTurnInterrupted(t *testing.T) {
	const threadID = "test-thread-stale-running"
	dir := t.TempDir()
	writeTestRollout(t, dir, threadID, "2000-01-01T00:00:00Z")
	t.Setenv("CODEX_SESSIONS_DIR", dir)

	view := AgentView{Agent: Agent{ThreadID: "loom-" + threadID, Status: "idle"}, nativeRuntimeRef: threadID}
	applyRolloutStatus(&view)

	if view.Status != "interrupted" {
		t.Fatalf("status = %q, want interrupted", view.Status)
	}
	if view.CurrentTurnID != "" {
		t.Fatalf("current turn = %q, want empty", view.CurrentTurnID)
	}
	if view.LastTurn == nil || view.LastTurn.Status != "interrupted" || view.LastTurn.TurnID != "turn-running" {
		t.Fatalf("last turn = %#v, want stale running turn summarized as interrupted", view.LastTurn)
	}
}

func TestApplyRolloutStatusMarksPersistedStaleRunningTurnInterrupted(t *testing.T) {
	const threadID = "test-thread-persisted-stale-running"
	dir := t.TempDir()
	writeTestRollout(t, dir, threadID, "2000-01-01T00:00:00Z")
	t.Setenv("CODEX_SESSIONS_DIR", dir)

	view := AgentView{
		Agent: Agent{
			ThreadID:      "loom-" + threadID,
			Status:        "running",
			CurrentTask:   "old task",
			CurrentTurnID: "turn-running",
		},
		ProcessAlive: false, nativeRuntimeRef: threadID,
	}
	applyRolloutStatus(&view)

	if view.Status != "interrupted" {
		t.Fatalf("status = %q, want interrupted", view.Status)
	}
	if view.CurrentTask != "" || view.CurrentTurnID != "" {
		t.Fatalf("current task/turn = %q/%q, want empty", view.CurrentTask, view.CurrentTurnID)
	}
	if view.LastTurn == nil || view.LastTurn.Status != "interrupted" || view.LastTurn.TurnID != "turn-running" {
		t.Fatalf("last turn = %#v, want stale persisted running turn summarized as interrupted", view.LastTurn)
	}
}

func TestApplyRolloutStatusKeepsDismissedInterruptedTurnIdle(t *testing.T) {
	const threadID = "test-thread-dismissed-interruption"
	dir := t.TempDir()
	writeTestRollout(t, dir, threadID, time.Now().UTC().Format(time.RFC3339Nano))
	t.Setenv("CODEX_SESSIONS_DIR", dir)

	view := AgentView{Agent: Agent{
		ThreadID: threadID, Status: "idle",
		LastTurn: &TurnSummary{TurnID: "turn-running", Task: "keep working", Status: "interrupted", CompletedAt: now()},
	}}
	applyRolloutStatus(&view)

	if view.Status != "idle" || view.CurrentTurnID != "" {
		t.Fatalf("dismissed view = %#v, want idle", view)
	}
}

func TestGetTurnLocatesAgentAndDurableSource(t *testing.T) {
	const threadID = "test-thread-turn-get"
	dir := t.TempDir()
	writeTestRollout(t, dir, threadID, "2026-07-21T01:00:00Z")
	t.Setenv("CODEX_SESSIONS_DIR", dir)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-0"] = &Agent{ID: "agent-0", Name: "no-rollout", ThreadID: "loom-thread-without-rollout", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thread-without-rollout"}, Status: "idle"}
	h.agents["agent-1"] = &Agent{ID: "agent-1", Name: "worker", ThreadID: "loom-" + threadID, RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: threadID}, Cwd: "/repo", Status: "idle"}
	h.comms["msg-1"] = &AgentMessage{
		ID: "msg-1", ToAgentID: "agent-1", DeliveryMode: "turn_start", DeliveredTurnID: "turn-running",
		DeliveryStatus: "delivered", HandlingStatus: "interrupted", LastHandlingError: "CodexLoom restarted",
		TopicID: "tpc-1",
	}
	h.commOrder = []string{"msg-1"}

	turn, err := h.GetTurn("turn-running")
	if err != nil {
		t.Fatal(err)
	}
	if turn.AgentID != "agent-1" || turn.Agent != "worker" || turn.ThreadID != "loom-"+threadID || turn.Status != "interrupted" {
		t.Fatalf("Turn identity/status = %#v", turn)
	}
	if turn.Source == nil || turn.Source.Kind != "internal" || turn.Source.ID != "msg-1" || turn.Source.TopicID != "tpc-1" {
		t.Fatalf("Turn source = %#v", turn.Source)
	}
	if turn.Error != "CodexLoom restarted" || len(turn.Items) != 1 || turn.Items[0]["text"] != "keep working" {
		t.Fatalf("Turn detail = %#v", turn)
	}
	if _, err := h.GetTurn("turn-missing"); err == nil {
		t.Fatal("missing Turn did not return an error")
	}
}

func TestGetTurnPreservesRecentExternalRunningStatus(t *testing.T) {
	const threadID = "test-thread-turn-get-live"
	dir := t.TempDir()
	writeTestRollout(t, dir, threadID, time.Now().UTC().Format(time.RFC3339Nano))
	t.Setenv("CODEX_SESSIONS_DIR", dir)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-1"] = &Agent{ID: "agent-1", Name: "worker", ThreadID: "loom-" + threadID, RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: threadID}, Status: "idle"}

	turn, err := h.GetTurn("turn-running")
	if err != nil {
		t.Fatal(err)
	}
	if turn.Status != "running" {
		t.Fatalf("status = %q, want running for recent external Turn", turn.Status)
	}
}

func TestTurnSource(t *testing.T) {
	tests := []struct {
		name           string
		inboxItemID    string
		agentMessageID string
		want           string
	}{
		{name: "owner", want: "owner"},
		{name: "internal", agentMessageID: "msg_123", want: "internal"},
		{name: "external", inboxItemID: "inb_123", want: "external"},
		{name: "external wins when both identifiers exist", inboxItemID: "inb_123", agentMessageID: "msg_123", want: "external"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := turnSource(test.inboxItemID, test.agentMessageID); got != test.want {
				t.Fatalf("turnSource(%q, %q) = %q, want %q", test.inboxItemID, test.agentMessageID, got, test.want)
			}
		})
	}
}

func TestAgentViewDoesNotAliasLastTurn(t *testing.T) {
	h := &Hub{}
	meta := &Agent{ID: "agent-1", LastTurn: &TurnSummary{TurnID: "turn-1", Status: "completed"}}

	view := h.viewLocked(meta)
	meta.LastTurn.Status = "failed"

	if view.LastTurn == nil || view.LastTurn.Status != "completed" {
		t.Fatalf("AgentView LastTurn changed through Hub state alias: %#v", view.LastTurn)
	}
}

func writeTestRollout(t *testing.T, dir, threadID, ts string) {
	t.Helper()
	day := filepath.Join(dir, "2026", "07", "08")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(day, "rollout-2026-07-08T10-00-00-"+threadID+".jsonl")
	data := `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-running"}}
{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"user_message","message":"keep working"}}
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
