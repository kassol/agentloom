package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestRuntimeConversationHTTPDiscoveryInspectionAndRedaction(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	sessionDir := filepath.Join(home, ".pi", "agent", "sessions", "workspace")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(sessionDir, "existing.jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": "native-private-uuid", "timestamp": "2026-08-12T00:00:00Z", "cwd": workspace})
	if err := os.WriteFile(sessionPath, append(header, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(st)
	defer h.Shutdown()
	server := New(h, st, fstest.MapFS{"index.html": {Data: []byte("ok")}}).Handler()

	runtimes := topicRequest(t, server, http.MethodGet, "/api/runtimes", nil, http.StatusOK)
	if len(runtimes["runtimes"].([]any)) != 3 {
		t.Fatalf("runtime capabilities = %#v", runtimes)
	}
	discovered := topicRequest(t, server, http.MethodGet, "/api/runtimes/pi/conversations", nil, http.StatusOK)
	candidates := discovered["candidates"].([]any)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %#v", candidates)
	}
	candidate := candidates[0].(map[string]any)
	id := candidate["id"].(string)
	if !strings.HasPrefix(id, "cand_") || candidate["cwd"] != workspace {
		t.Fatalf("candidate = %#v", candidate)
	}
	raw, _ := json.Marshal(discovered)
	if strings.Contains(string(raw), sessionPath) || strings.Contains(string(raw), "native-private-uuid") || strings.Contains(string(raw), "nativeRef") {
		t.Fatalf("discovery leaked private identity: %s", raw)
	}

	inspected := topicRequest(t, server, http.MethodGet, "/api/runtimes/pi/conversations/"+id, nil, http.StatusOK)
	inspectRaw, _ := json.Marshal(inspected)
	if strings.Contains(string(inspectRaw), sessionPath) || inspected["candidate"].(map[string]any)["revision"] == "" {
		t.Fatalf("inspection = %s", inspectRaw)
	}
}
