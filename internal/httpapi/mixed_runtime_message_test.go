package httpapi

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestMixedRuntimeMessagesShareDurableHTTPAndSSEControlPlaneAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string]*hub.Agent{
		"agent-codex": {
			ID: "agent-codex", Name: "codex-owner", Source: "edge", Status: "running", ThreadID: "loom-thread-codex",
			RuntimeBinding: hub.RuntimeBinding{Kind: "codex", NativeRef: "codex-thread"}, CreatedAt: nowForTest(), UpdatedAt: nowForTest(),
		},
		"agent-pi": {
			ID: "agent-pi", Name: "pi-worker", Source: "edge", Status: "running", ThreadID: "loom-thread-pi",
			RuntimeBinding: hub.RuntimeBinding{Kind: "pi", NativeRef: "/loom/pi/session.jsonl"}, CreatedAt: nowForTest(), UpdatedAt: nowForTest(),
		},
	}
	if err := st.SaveAgents(agents); err != nil {
		t.Fatal(err)
	}
	h, err := hub.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	web := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok"), Mode: fs.FileMode(0o644)}}
	server := httptest.NewServer(New(h, st, web).Handler())

	root := postComm(t, server.URL, map[string]any{
		"from": "agent-pi", "to": "agent-codex", "subject": "Need Domain decision",
		"body": "Please decide the contract.", "response": "required",
	})
	if root.FromAgentID != "agent-pi" || root.ToAgentID != "agent-codex" || root.Status != "open" || root.DeliveryStatus != "queued" {
		t.Fatalf("Pi to Codex root = %#v", root)
	}
	reply := postComm(t, server.URL, map[string]any{
		"from": "agent-codex", "replyTo": root.ID, "body": "Use the narrow Loom contract.",
	})
	if reply.FromAgentID != "agent-codex" || reply.ToAgentID != "agent-pi" || reply.ReplyTo != root.ID || reply.DeliveryStatus != "queued" {
		t.Fatalf("Codex to Pi reply = %#v", reply)
	}

	events := readGlobalSSE(t, h, "/api/events?since=0", 4)
	loomMessages := map[string]bool{}
	for _, event := range events {
		if event.Type != "loom/comms-message" {
			continue
		}
		var payload struct {
			Message hub.AgentMessage `json:"message"`
		}
		if json.Unmarshal(event.Data, &payload) != nil {
			t.Fatalf("decode mixed Runtime SSE event = %#v", event)
		}
		loomMessages[payload.Message.ID] = true
	}
	if !loomMessages[root.ID] || !loomMessages[reply.ID] {
		t.Fatalf("normalized Message SSE ids = %v", loomMessages)
	}

	server.Close()
	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := hub.Open(reopened)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Shutdown()
	restartServer := httptest.NewServer(New(restarted, reopened, web).Handler())
	defer restartServer.Close()
	gotRoot := getComm(t, restartServer.URL, root.ID)
	gotReply := getComm(t, restartServer.URL, reply.ID)
	if gotRoot.ID != root.ID || gotRoot.FromAgentID != "agent-pi" || gotRoot.ToAgentID != "agent-codex" || gotRoot.Status != "open" {
		t.Fatalf("restarted root = %#v", gotRoot)
	}
	if gotReply.ID != reply.ID || gotReply.ReplyTo != root.ID || gotReply.FromAgentID != "agent-codex" || gotReply.ToAgentID != "agent-pi" {
		t.Fatalf("restarted reply = %#v", gotReply)
	}
}

func postComm(t *testing.T, baseURL string, body map[string]any) hub.AgentMessage {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(baseURL+"/api/comms/messages", "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("POST Message status = %d", response.StatusCode)
	}
	var result hub.CommResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || result.Message == nil {
		t.Fatalf("decode POST Message: result=%#v err=%v", result, err)
	}
	return *result.Message
}

func getComm(t *testing.T, baseURL, id string) hub.AgentMessage {
	t.Helper()
	response, err := http.Get(baseURL + "/api/comms/messages/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result struct {
		Message hub.AgentMessage `json:"message"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result.Message
}
