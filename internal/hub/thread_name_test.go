package hub

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yan5xu/codex-loom/internal/store"
)

func TestSyncThreadNamesSendsPersistedNames(t *testing.T) {
	h := testHub(nil)
	fakeContract, codexContract := &controlPlaneContract{}, &controlPlaneContract{}
	h.agents["a"] = &Agent{ID: "a", Name: "agent-a", ThreadID: "loom-thread-b", RuntimeBinding: RuntimeBinding{Kind: "fake", NativeRef: "thread-b"}}
	h.agents["b"] = &Agent{ID: "b", Name: "agent-b", ThreadID: "loom-thread-a", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thread-a"}}
	h.agents["c"] = &Agent{ID: "c", Name: "pi-without-live-contract", RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: "session-c"}}
	h.runtimes["a"] = &runtime{agentID: "a", runtimeContract: fakeContract, binding: runtimeContractBinding(h.agents["a"])}
	h.runtimes["b"] = &runtime{agentID: "b", runtimeContract: codexContract, binding: runtimeContractBinding(h.agents["b"])}

	if err := h.SyncThreadNames(); err != nil {
		t.Fatal(err)
	}
	if fakeContract.nameCalls != 1 || fakeContract.nameBinding.RuntimeKind != "fake" || fakeContract.name != "agent-a" {
		t.Fatalf("non-Codex v2 name backfill = calls %d binding %#v name %q", fakeContract.nameCalls, fakeContract.nameBinding, fakeContract.name)
	}
	if codexContract.nameCalls != 1 || codexContract.nameBinding.RuntimeKind != "codex" || codexContract.name != "agent-b" {
		t.Fatalf("Codex v2 name backfill = calls %d binding %#v name %q", codexContract.nameCalls, codexContract.nameBinding, codexContract.name)
	}
}

func TestUpdateConfigSyncsRenamedThread(t *testing.T) {
	logPath := installFakeCodexNameServer(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["sess"] = &Agent{ID: "sess", Name: "old-name", ThreadID: "loom-thread-1", RuntimeBinding: RuntimeBinding{Kind: "codex", NativeRef: "thread-1"}, Status: "idle"}
	host, err := h.ensureCodexHost()
	if err != nil {
		t.Fatal(err)
	}
	h.runtimes["sess"] = &runtime{
		agentID: "sess", runtimeContract: &codexRuntimeContract{native: &codexAgentRuntime{client: host.client}},
	}
	defer host.close()
	name := "new-name"

	view, err := h.UpdateConfig("sess", ConfigParams{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if view.Name != name {
		t.Fatalf("Agent name = %q, want %q", view.Name, name)
	}
	requests := readThreadNameRequests(t, logPath)
	if len(requests) != 1 || requests[0].ThreadID != "thread-1" || requests[0].Name != name {
		t.Fatalf("thread/name/set requests = %#v", requests)
	}
}

func TestRenamePiAgentCommitsLoomNameWithoutInvokingCodex(t *testing.T) {
	logPath := installFakeCodexNameServer(t)
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := testHub(st)
	h.agents["pi-agent"] = &Agent{
		ID: "pi-agent", Name: "old-name", ThreadID: "loom-thread-1",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "pi", NativeRef: "/private/pi/session.jsonl"},
		Status:         "idle", CreatedAt: now(), UpdatedAt: now(),
	}
	name := "new-name"

	view, err := h.UpdateAgentConfig("pi-agent", ConfigParams{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if view.Name != name {
		t.Fatalf("Agent name = %q, want %q", view.Name, name)
	}
	if requests := readThreadNameRequests(t, logPath); len(requests) != 0 {
		t.Fatalf("Pi rename invoked Codex native naming: %#v", requests)
	}
}

type threadNameRequest struct {
	ThreadID string
	Name     string
}

func installFakeCodexNameServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "codex")
	logPath := filepath.Join(dir, "requests.ndjson")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  printf 'codex-cli 0.144.1\n'
  exit 0
fi
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$CODEX_NAME_LOG"
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  if [ -n "$id" ]; then
    printf '{"id":%s,"result":{}}\n' "$id"
  fi
done
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_BIN", binPath)
	t.Setenv("CODEX_NAME_LOG", logPath)
	return logPath
}

func readThreadNameRequests(t *testing.T, path string) []threadNameRequest {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var requests []threadNameRequest
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var message struct {
			Method string `json:"method"`
			Params struct {
				ThreadID string `json:"threadId"`
				Name     string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			t.Fatal(err)
		}
		if message.Method == "thread/name/set" {
			requests = append(requests, threadNameRequest{
				ThreadID: message.Params.ThreadID,
				Name:     message.Params.Name,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return requests
}
