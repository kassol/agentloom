package rollout

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestContextHistoryResetsCoverageAtCompaction(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_SESSIONS_DIR", root)
	threadID := "thread-context"
	path := filepath.Join(root, "2026", "07", "28", "rollout-2026-07-28T00-00-00-"+threadID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPayload := `<loom_context delivery_id="old:input" />`
	newPayload := `<loom_context delivery_id="new:input" />`
	content := `{"timestamp":"2026-07-28T00:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<loom_context delivery_id=\"old:input\" />"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-old"}}}
{"timestamp":"2026-07-28T00:01:00Z","type":"compacted","payload":{"window_number":2,"window_id":"window-2","replacement_history":[]}}
{"timestamp":"2026-07-28T00:02:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<loom_context delivery_id=\"new:input\" />"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-new"}}}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	rolloutPathCache = syncMapForTest()

	old, err := ContextHistory(threadID, ContextHistoryQuery{
		TurnID: "turn-old",
		Deliveries: []ContextDeliveryProbe{{
			Role: "user", Marker: `delivery_id="old:input"`, Hash: testSHA256(oldPayload),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if old.EpochID != "window:window-2" || old.DeliveriesPersisted {
		t.Fatalf("old epoch state = %#v", old)
	}
	current, err := ContextHistory(threadID, ContextHistoryQuery{
		TurnID: "turn-new",
		Deliveries: []ContextDeliveryProbe{{
			Role: "user", Marker: `delivery_id="new:input"`, Hash: testSHA256(newPayload),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if current.EpochID != "window:window-2" || current.WindowNumber != 2 || !current.DeliveriesPersisted {
		t.Fatalf("current epoch state = %#v", current)
	}
}

func TestContextHistoryUsesStableInitialEpochWithoutRollout(t *testing.T) {
	t.Setenv("CODEX_SESSIONS_DIR", t.TempDir())
	rolloutPathCache = syncMapForTest()
	state, err := ContextHistory("thread-new", ContextHistoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if state.EpochID != "initial:thread-new" || state.DeliveriesPersisted {
		t.Fatalf("initial state = %#v", state)
	}
}

func TestContextHistoryMatchesCapturedCodexUserMessageShape(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_SESSIONS_DIR", root)
	threadID := "thread-captured"
	path := filepath.Join(root, "2026", "07", "28", "rollout-2026-07-28T04-13-48-"+threadID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join("testdata", "context-history-captured.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	rolloutPathCache = syncMapForTest()

	payload := `<loom_context version="1"><coverage_manifest attempt_id="ctxa_captured"><fragment key="loom_agent_profile" revision="profile:1" hash="abc" /></coverage_manifest></loom_context>`
	state, err := ContextHistory(threadID, ContextHistoryQuery{
		TurnID: "019fa6ed-c76f-7120-8b17-aa32c0f0083a",
		Deliveries: []ContextDeliveryProbe{{
			Role: "user", Marker: `attempt_id="ctxa_captured"`, Hash: testSHA256(payload),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.EpochID != "initial:"+threadID || !state.DeliveriesPersisted {
		t.Fatalf("captured context history = %#v", state)
	}
	mismatch, err := ContextHistory(threadID, ContextHistoryQuery{
		TurnID: "different-turn",
		Deliveries: []ContextDeliveryProbe{{
			Role: "user", Marker: `attempt_id="ctxa_captured"`, Hash: testSHA256(payload),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mismatch.DeliveriesPersisted {
		t.Fatalf("captured marker matched the wrong Turn: %#v", mismatch)
	}
}

func TestContextHistoryRequiresCompleteExactDeveloperDelivery(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_SESSIONS_DIR", root)
	threadID := "thread-developer"
	path := filepath.Join(root, "2026", "07", "28", "rollout-2026-07-28T00-00-00-"+threadID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	complete := `<loom_developer_context delivery_id="ctxa_one:developer"><loom_agent_prompt>full</loom_agent_prompt><loom_agent_profile>full</loom_agent_profile></loom_developer_context>`
	truncated := `<loom_developer_context delivery_id="ctxa_one:developer"><loom_agent_prompt>full</loom_agent_prompt>`
	content := `{"timestamp":"2026-07-28T00:00:00Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":` +
		mustJSON(t, truncated) + `}]}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	rolloutPathCache = syncMapForTest()
	query := ContextHistoryQuery{Deliveries: []ContextDeliveryProbe{{
		Role: "developer", Marker: `delivery_id="ctxa_one:developer"`, Hash: testSHA256(complete),
	}}}
	state, err := ContextHistory(threadID, query)
	if err != nil {
		t.Fatal(err)
	}
	if state.DeliveriesPersisted {
		t.Fatalf("truncated Developer delivery was accepted: %#v", state)
	}

	content += `{"timestamp":"2026-07-28T00:01:00Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":` +
		mustJSON(t, complete) + `}]}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	rolloutPathCache = syncMapForTest()
	state, err = ContextHistory(threadID, query)
	if err != nil {
		t.Fatal(err)
	}
	if !state.DeliveriesPersisted {
		t.Fatalf("complete Developer delivery was not accepted: %#v", state)
	}
}

func mustJSON(t *testing.T, value string) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func testSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func syncMapForTest() sync.Map {
	return sync.Map{}
}
