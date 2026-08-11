package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestPiContextEvidenceProvesExactFullPerTurnDelivery(t *testing.T) {
	developer := `<loom_developer_context version="1" prompt_revision="owner:3" prompt_hash="prompt-sha" profile_revision="profile:7" profile_hash="profile-sha"><loom_agent_profile_data /></loom_developer_context>`
	input := `<loom_context version="1"><loom_agent_relationships revision="relationships:abc" hash="relationships-sha"/><loom_agent_goal id="goal-1" revision="4" status="active"/><loom_turn_context kind="needs_you_answer" ref_id="hrq-1" topic_id="topic-1"/></loom_context>`
	prompt := developer + "\n\nOwner answer\n\n" + input
	message, err := json.Marshal(map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": prompt}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	data := `{"type":"session","version":3,"id":"session-1"}` + "\n" +
		`{"type":"message","id":"user-1","parentId":null,"timestamp":"2026-08-12T00:00:00Z","message":` + string(message) + `}` + "\n" +
		`{"type":"message","id":"user-current","parentId":null,"timestamp":"2026-08-12T00:01:00Z","message":{"role":"user","content":"current branch"}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	report, failure := newPiRuntimeContract("agent-1", nil).InspectContextEvidence(context.Background(), piContractBinding(path), runtimecontract.ContextEvidenceQuery{
		TurnID: "turn-loom", RuntimeTurnRef: "user-1",
	})
	if failure != nil {
		t.Fatal(failure)
	}
	if report.State != runtimecontract.ContextEvidenceProven || report.TurnID != "turn-loom" || report.Mode != runtimecontract.ContextDeliveryFullPerTurn {
		t.Fatalf("Pi evidence summary = %#v", report)
	}
	if len(report.Deliveries) != 2 || report.Deliveries[0].Content != developer || report.Deliveries[1].Content != input || report.Deliveries[0].Hash != testContextEvidenceSHA(developer) || report.Deliveries[0].Role != "user" {
		t.Fatalf("Pi exact deliveries = %#v", report.Deliveries)
	}
	encoded, _ := json.Marshal(report)
	for _, source := range []string{"loom_agent_prompt", "loom_agent_profile", "loom_agent_relationships", "loom_agent_goal", "turn_source", "needs_you", "topic"} {
		if !strings.Contains(string(encoded), source) {
			t.Fatalf("Pi evidence omitted %s: %s", source, encoded)
		}
	}
	if strings.Contains(string(encoded), path) || strings.Contains(string(encoded), "user-1") {
		t.Fatalf("Pi evidence leaked native correlation: %s", encoded)
	}
}

type blockingContextEvidenceContract struct {
	*controlPlaneContract
	started chan struct{}
	release chan struct{}
	once    sync.Once
	fail    bool
}

func (c *blockingContextEvidenceContract) InspectContextEvidence(_ context.Context, _ runtimecontract.Binding, query runtimecontract.ContextEvidenceQuery) (runtimecontract.ContextEvidence, *runtimecontract.Failure) {
	c.once.Do(func() { close(c.started) })
	<-c.release
	if c.fail {
		return runtimecontract.ContextEvidence{}, &runtimecontract.Failure{Message: "old binding failure"}
	}
	return runtimecontract.ContextEvidence{State: runtimecontract.ContextEvidenceProven, TurnID: query.TurnID, Mode: runtimecontract.ContextDeliveryFullPerTurn, Sources: []runtimecontract.ContextEvidenceSource{}, Deliveries: []runtimecontract.ContextEvidenceDelivery{}, UnsupportedDimensions: []string{}}, nil
}

func TestContextEvidenceRevalidatesBindingAfterUnlockedRuntimeInspection(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	contract := &blockingContextEvidenceContract{controlPlaneContract: &controlPlaneContract{}, started: make(chan struct{}), release: make(chan struct{})}
	h.runtimeHostDrivers["fake"] = &controlPlaneDriver{historyContract: contract}
	h.agents["agent-1"] = &Agent{ID: "agent-1", Name: "Agent", ThreadID: "loom-thread", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "native-thread"}, RuntimeTurnBindings: map[string]string{"turn-1": "native-turn-1"}}
	done := make(chan ContextExplainView, 1)
	go func() {
		view, _ := h.ExplainTurnContext("agent-1", "turn-1")
		done <- view
	}()
	<-contract.started
	h.mu.Lock()
	h.agents["agent-1"].RuntimeTurnBindings["turn-1"] = "native-turn-2"
	h.mu.Unlock()
	close(contract.release)
	view := <-done
	if view.State != runtimecontract.ContextEvidenceUnknown || !strings.Contains(view.Reason, "changed") {
		t.Fatalf("stale Runtime evidence escaped revalidation: %#v", view)
	}
}

func TestContextEvidenceRevalidatesBindingBeforePublishingRuntimeFailure(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	contract := &blockingContextEvidenceContract{controlPlaneContract: &controlPlaneContract{contextMode: runtimecontract.ContextDeliveryFullPerTurn}, started: make(chan struct{}), release: make(chan struct{}), fail: true}
	h.runtimeHostDrivers["fake"] = &controlPlaneDriver{historyContract: contract}
	h.agents["agent-1"] = &Agent{ID: "agent-1", Name: "Agent", ThreadID: "loom-thread", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "fake", NativeRef: "native-thread"}, RuntimeTurnBindings: map[string]string{"turn-1": "native-turn-1"}}
	done := make(chan ContextExplainView, 1)
	go func() {
		view, _ := h.ExplainTurnContext("agent-1", "turn-1")
		done <- view
	}()
	<-contract.started
	h.mu.Lock()
	h.agents["agent-1"].RuntimeTurnBindings["turn-1"] = "native-turn-2"
	h.mu.Unlock()
	close(contract.release)
	view := <-done
	if view.State != runtimecontract.ContextEvidenceUnknown || !strings.Contains(view.Reason, "changed") || strings.Contains(view.Reason, "old binding failure") {
		t.Fatalf("stale Runtime failure escaped revalidation: %#v", view)
	}
}

func TestPiContextEvidenceRejectsMalformedLoomBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	message, _ := json.Marshal(map[string]any{"role": "user", "content": `<loom_developer_context><broken></loom_developer_context>`})
	if err := os.WriteFile(path, []byte(`{"type":"session","version":3,"id":"session-1"}`+"\n"+`{"type":"message","id":"user-1","message":`+string(message)+`}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, failure := newPiRuntimeContract("agent-1", nil).InspectContextEvidence(context.Background(), piContractBinding(path), runtimecontract.ContextEvidenceQuery{TurnID: "turn-loom", RuntimeTurnRef: "user-1"})
	if failure != nil || report.State != runtimecontract.ContextEvidenceUnavailable || !strings.Contains(report.Reason, "malformed") {
		t.Fatalf("malformed Pi evidence = %#v, failure=%v", report, failure)
	}
}

func TestPiContextEvidenceRejectsIncompleteSourceMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	message, _ := json.Marshal(map[string]any{"role": "user", "content": `<loom_developer_context version="1" prompt_revision="owner:3" profile_revision="profile:7" profile_hash="profile-sha"></loom_developer_context>`})
	if err := os.WriteFile(path, []byte(`{"type":"session","version":3,"id":"session-1"}`+"\n"+`{"type":"message","id":"user-1","message":`+string(message)+`}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, failure := newPiRuntimeContract("agent-1", nil).InspectContextEvidence(context.Background(), piContractBinding(path), runtimecontract.ContextEvidenceQuery{TurnID: "turn-loom", RuntimeTurnRef: "user-1"})
	if failure != nil || report.State != runtimecontract.ContextEvidenceUnavailable || !strings.Contains(report.Reason, "malformed") {
		t.Fatalf("incomplete Pi source metadata = %#v, failure=%v", report, failure)
	}
}

func TestPiContextEvidenceDoesNotProveSingleChannelDelivery(t *testing.T) {
	developer := `<loom_developer_context prompt_revision="builtin:2" prompt_hash="prompt" profile_revision="profile:0" profile_hash="profile"></loom_developer_context>`
	input := `<loom_context><loom_agent_relationships revision="relationships:1" hash="relationships"></loom_agent_relationships></loom_context>`
	for name, content := range map[string]string{"developer only": developer, "input only": input} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			message, _ := json.Marshal(map[string]any{"role": "user", "content": content})
			if err := os.WriteFile(path, []byte(`{"type":"session","version":3,"id":"session-1"}`+"\n"+`{"type":"message","id":"user-1","message":`+string(message)+`}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			report, failure := newPiRuntimeContract("agent-1", nil).InspectContextEvidence(context.Background(), piContractBinding(path), runtimecontract.ContextEvidenceQuery{TurnID: "turn-loom", RuntimeTurnRef: "user-1"})
			if failure != nil || report.State != runtimecontract.ContextEvidenceUnknown || !strings.Contains(report.Reason, "incomplete") {
				t.Fatalf("single-channel Pi evidence = %#v, failure=%v", report, failure)
			}
		})
	}
}

func TestPiContextEvidenceAttributesAgentMessageSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	message, _ := json.Marshal(map[string]any{"role": "user", "content": `<loom_developer_context prompt_revision="builtin:2" prompt_hash="prompt" profile_revision="profile:0" profile_hash="profile"></loom_developer_context>` + "\n" + `<loom_context><loom_agent_relationships revision="relationships:1" hash="relationships"></loom_agent_relationships><loom_turn_context kind="agent_message" ref_id="msg-1" topic_id="topic-1"></loom_turn_context></loom_context>`})
	if err := os.WriteFile(path, []byte(`{"type":"session","version":3,"id":"session-1"}`+"\n"+`{"type":"message","id":"user-1","message":`+string(message)+`}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, failure := newPiRuntimeContract("agent-1", nil).InspectContextEvidence(context.Background(), piContractBinding(path), runtimecontract.ContextEvidenceQuery{TurnID: "turn-loom", RuntimeTurnRef: "user-1"})
	encoded, _ := json.Marshal(report.Sources)
	if failure != nil || report.State != runtimecontract.ContextEvidenceProven || !strings.Contains(string(encoded), `"key":"message"`) || !strings.Contains(string(encoded), `"key":"topic"`) {
		t.Fatalf("Agent Message context sources = %s, failure=%v", encoded, failure)
	}
}

func TestCodexContextEvidenceUsesExactMappedHistoricalTurn(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_SESSIONS_DIR", root)
	nativeThread := "thread-codex-exact"
	path := filepath.Join(root, "2026", "08", "12", "rollout-2026-08-12T00-00-00-"+nativeThread+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	oldDeveloper := `<loom_developer_context version="1" epoch_id="initial" delivery_id="old:developer" prompt_revision="owner:1" prompt_hash="prompt-old" profile_revision="profile:1" profile_hash="profile-old"></loom_developer_context>`
	oldInput := `<loom_context version="1" epoch_id="initial" delivery_id="old:input"><loom_agent_relationships revision="relationships:old" hash="relationships-old"></loom_agent_relationships><loom_turn_context kind="agent_message" ref_id="message-old"></loom_turn_context></loom_context>`
	newDeveloper := `<loom_developer_context version="1" epoch_id="window:new" delivery_id="new:developer" prompt_revision="owner:2" prompt_hash="prompt-new" profile_revision="profile:2" profile_hash="profile-new"></loom_developer_context>`
	line := func(role, content, nativeTurn string) string {
		message := map[string]any{"type": "message", "role": role, "content": []map[string]any{{"type": "input_text", "text": content}}}
		if nativeTurn != "" {
			message["internal_chat_message_metadata_passthrough"] = map[string]any{"turn_id": nativeTurn}
		}
		payload, _ := json.Marshal(message)
		return `{"timestamp":"2026-08-12T00:00:00Z","type":"response_item","payload":` + string(payload) + `}` + "\n"
	}
	data := line("developer", oldDeveloper, "") + line("user", oldInput, "native-old") +
		`{"timestamp":"2026-08-12T00:01:00Z","type":"compacted","payload":{"window_number":2,"window_id":"new"}}` + "\n" +
		line("developer", newDeveloper, "") + line("user", `<loom_context version="1" epoch_id="window:new" delivery_id="new:input"><loom_turn_context kind="direct_input"></loom_turn_context></loom_context>`, "native-new")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	h.runtimeHostDrivers["codex"] = newCodexRuntimeHostDriver(h)
	h.agents["agent-1"] = &Agent{
		ID: "agent-1", Name: "Codex Agent", ThreadID: "loom-thread",
		RuntimeBinding:      RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "codex", NativeRef: nativeThread},
		RuntimeTurnBindings: map[string]string{"turn-old": "native-old", "turn-new": "native-new"}, Status: "idle",
	}
	view, err := h.ExplainTurnContext("agent-1", "turn-old")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(view)
	if view.State != runtimecontract.ContextEvidenceProven || view.Mode != runtimecontract.ContextDeliveryEpochIncremental || len(view.Deliveries) != 2 || !strings.Contains(string(encoded), "prompt-old") || !strings.Contains(string(encoded), "message-old") {
		t.Fatalf("historical Codex Turn evidence = %s", encoded)
	}
	if strings.Contains(string(encoded), "prompt-new") || strings.Contains(string(encoded), "native-old") || strings.Contains(string(encoded), nativeThread) {
		t.Fatalf("historical Codex evidence mixed current/native data: %s", encoded)
	}
}

func TestPiContextEvidenceUsesDurableLoomTurnCorrelationAfterStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	message, _ := json.Marshal(map[string]any{"role": "user", "content": `<loom_developer_context version="1" prompt_revision="builtin:2" prompt_hash="p" profile_revision="profile:0" profile_hash="q"></loom_developer_context>` + "\n" + `<loom_context version="1"><loom_agent_relationships revision="relationships:1" hash="r"></loom_agent_relationships></loom_context>`})
	if err := os.WriteFile(path, []byte(`{"type":"session","version":3,"id":"session-1"}`+"\n"+`{"type":"message","id":"user-1","timestamp":"2026-08-12T00:00:00Z","message":`+string(message)+`}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	agent := &Agent{ID: "agent-1", Name: "Pi Agent", ThreadID: "loom-thread", RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "pi", NativeRef: path}, RuntimeTurnBindings: map[string]string{"turn-loom": "user-1"}, Status: "idle"}
	if err := st.SaveAgents(map[string]*Agent{agent.ID: agent}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var agents map[string]*Agent
	if err := st.LoadAgents(&agents); err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents = agents
	h.runtimeHostDrivers["pi"] = newPiRuntimeHostDriver(h)

	view, err := h.ExplainTurnContext(agent.ID, "turn-loom")
	if err != nil || view.State != runtimecontract.ContextEvidenceProven || view.TurnID != "turn-loom" || len(view.Deliveries) != 2 {
		t.Fatalf("reopened Pi evidence = %#v, err=%v", view, err)
	}
	missing, err := h.ExplainTurnContext(agent.ID, "turn-missing")
	if err != nil || missing.State != runtimecontract.ContextEvidenceUnknown || missing.Mode != runtimecontract.ContextDeliveryFullPerTurn || len(missing.UnsupportedDimensions) != 4 || !strings.Contains(missing.Reason, "correlation") {
		t.Fatalf("missing Pi evidence = %#v, err=%v", missing, err)
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), path) || strings.Contains(string(encoded), "user-1") || strings.Contains(string(encoded), `"epoch":`) {
		t.Fatalf("public Pi view leaked native details or fabricated an epoch: %s", encoded)
	}
}

func testContextEvidenceSHA(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
