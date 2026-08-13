package httpapi

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestAcceptedTurnTransportExitReturnsHTTPErrorButDurablyReconciles(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then printf 'codex-cli 0.144.1\n'; exit 0; fi
while IFS= read -r line; do
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  [ -z "$id" ] && continue
  case "$line" in
    *'"method":"initialize"'*) printf '{"id":%s,"result":{"userAgent":"fake"}}\n' "$id" ;;
    *'"method":"skills/list"'*) printf '{"id":%s,"result":{"data":[]}}\n' "$id" ;;
    *'"method":"thread/start"'*) printf '{"id":%s,"result":{"thread":{"id":"native-thread"}}}\n' "$id" ;;
    *'"method":"thread/resume"'*) printf '{"id":%s,"result":{"thread":{"id":"native-thread"}}}\n' "$id" ;;
    *'"method":"turn/start"'*) exit 0 ;;
    *) printf '{"id":%s,"result":{}}\n' "$id" ;;
  esac
done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_LOOM_CODEX_BIN", bin)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := hub.New(st)
	defer h.Shutdown()
	agent, err := h.CreateAgent(hub.CreateParams{Name: "worker", Cwd: t.TempDir(), RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	web := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok"), Mode: fs.FileMode(0o644)}}
	handler := New(h, st, web).Handler()
	topicRequest(t, handler, http.MethodPost, "/api/agents/"+agent.ID+"/turns", map[string]any{"text": "perform once", "timeoutSec": 1}, http.StatusInternalServerError)

	deadline := time.Now().Add(2 * time.Second)
	var recovery map[string]any
	for time.Now().Before(deadline) {
		view := topicRequest(t, handler, http.MethodGet, "/api/agents/"+agent.ID, nil, http.StatusOK)["agent"].(map[string]any)
		recovery, _ = view["recovery"].(map[string]any)
		if recovery != nil && recovery["state"] == hub.TurnRecoveryDispatched {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if recovery == nil || recovery["runtimeKind"] != "codex" {
		t.Fatalf("HTTP uncertain recovery=%#v", recovery)
	}
	switch recovery["cause"] {
	case "process_exit":
	case "command_indeterminate":
		if recovery["failurePhase"] != "turn_start" || recovery["failureCode"] != "transport_closed" {
			t.Fatalf("HTTP uncertain recovery=%#v", recovery)
		}
	default:
		t.Fatalf("HTTP uncertain recovery=%#v", recovery)
	}
}

func TestRuntimeRecoveryIsDurableBeforeHTTPAndSSEAndSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := nowForTest()
	if err := st.SaveAgents(map[string]*hub.Agent{
		"agent-1": {
			ID: "agent-1", Name: "worker", Cwd: t.TempDir(), ThreadID: "thr-loom",
			RuntimeBinding: hub.RuntimeBinding{Kind: "codex", NativeRef: "native-thread-secret"},
			Status:         "running", CurrentTask: "publish once", CurrentTurnID: "turn-loom",
			CreatedAt: stamp, UpdatedAt: stamp,
		},
	}); err != nil {
		t.Fatal(err)
	}
	h, err := hub.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	web := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok"), Mode: fs.FileMode(0o644)}}
	handler := New(h, st, web).Handler()

	deadline := time.Now().Add(2 * time.Second)
	var recovery map[string]any
	for time.Now().Before(deadline) {
		agent := topicRequest(t, handler, http.MethodGet, "/api/agents/agent-1", nil, http.StatusOK)["agent"].(map[string]any)
		recovery, _ = agent["recovery"].(map[string]any)
		if recovery != nil && recovery["state"] == hub.TurnRecoveryDispatched {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if recovery == nil || recovery["cause"] != "hub_restart" || recovery["runtimeKind"] != "codex" {
		t.Fatalf("HTTP recovery=%#v", recovery)
	}
	events := readGlobalSSE(t, h, "/api/agents/agent-1/thread/events?since=0", 1)
	if events[0].Type != "loom/turn-interrupted" {
		t.Fatalf("recovery SSE=%#v", events[0])
	}
	encoded, _ := json.Marshal(events[0])
	if string(encoded) == "" || containsAny(string(encoded), "native-thread-secret", "nativeRef", "nativeTurnId", "evidenceLeafId") {
		t.Fatalf("recovery SSE leaked native identity: %s", encoded)
	}

	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := hub.Open(reopened)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Shutdown()
	restartHandler := New(restarted, reopened, web).Handler()
	agent := topicRequest(t, restartHandler, http.MethodGet, "/api/agents/agent-1", nil, http.StatusOK)["agent"].(map[string]any)
	recovery, _ = agent["recovery"].(map[string]any)
	if recovery == nil || recovery["cause"] != "hub_restart" {
		t.Fatalf("reopened HTTP recovery=%#v", recovery)
	}
}
