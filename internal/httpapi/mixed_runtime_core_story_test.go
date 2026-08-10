package httpapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yan5xu/codex-loom/internal/hub"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestMixedRuntimeCoreStorySurvivesRestartWithoutLeakingNativeIdentity(t *testing.T) {
	paths := configureMixedRuntimeStoryPi(t)
	t.Setenv("CODEX_BIN", t.TempDir()+"/missing-codex")
	t.Setenv("PINIX_EDGE_NAMES", t.TempDir()+"/missing-edge-names.json")
	dataDir := t.TempDir()
	workDir := t.TempDir()
	web := fstest.MapFS{"index.html": {Data: []byte("ok")}}

	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAgents(map[string]*hub.Agent{
		"codex-participant": {
			ID: "codex-participant", Name: "codex-reviewer", Cwd: workDir, ThreadID: "loom-thread-codex",
			RuntimeBinding: hub.RuntimeBinding{Kind: "codex", NativeRef: "native-codex-thread-secret"},
			Source:         "edge", Status: "running", CreatedAt: nowForTest(), UpdatedAt: nowForTest(),
		},
	}); err != nil {
		t.Fatal(err)
	}
	h, err := hub.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(h, st, web).Handler()

	createdAgent := topicRequest(t, handler, http.MethodPost, "/api/agents", map[string]any{
		"name": "pi-integrator", "cwd": workDir, "runtimeKind": "pi", "approvalPolicy": "never",
	}, http.StatusCreated)["agent"].(map[string]any)
	piAgentID := createdAgent["id"].(string)
	loomThreadID := createdAgent["threadId"].(string)
	nativePiPathFragment := "/pi/" + piAgentID + "/"
	assertRuntimeNeutralJSON(t, createdAgent, nativePiPathFragment, "native-codex-thread-secret", "native-user-", "native-assistant-")
	if binding := createdAgent["runtimeBinding"].(map[string]any); binding["kind"] != "pi" || binding["nativeRef"] != nil {
		t.Fatalf("created Pi Runtime Binding projection = %#v", binding)
	}

	profile := topicRequest(t, handler, http.MethodPut, "/api/agents/"+piAgentID+"/profile", map[string]any{
		"identity": "Release integration lead", "domain": "Mixed Runtime release evidence", "scope": "Integrate bounded participant evidence and escalate only Owner decisions",
	}, http.StatusOK)["profile"].(map[string]any)
	if profile["agentId"] != piAgentID || profile["version"] != float64(1) {
		t.Fatalf("Pi Profile = %#v", profile)
	}

	createdTopic := topicRequest(t, handler, http.MethodPost, "/api/topics", map[string]any{
		"title": "Mixed Runtime acceptance", "purpose": "Integrate participant evidence with one Owner decision",
		"completionBoundary": "The Responsible Agent publishes the integrated result",
		"responsibleAgent":   piAgentID, "createdBy": piAgentID,
		"participants": []map[string]any{{"agent": "codex-participant", "responsibility": "Validate the bounded compatibility evidence"}},
		"initialBrief": map[string]any{"summary": "Participant evidence and Owner rollout choice are pending"},
	}, http.StatusCreated)["topic"].(map[string]any)
	topicID := createdTopic["id"].(string)

	started := topicRequest(t, handler, http.MethodPost, "/api/topics/"+topicID+"/send", map[string]any{
		"text": "Coordinate the participant review, obtain the rollout choice, and publish one integrated result.", "timeoutSec": 3,
	}, http.StatusAccepted)
	firstTurnID := started["turnId"].(string)
	waitForStoryFile(t, paths.firstActive)

	requestMessage := topicRequest(t, handler, http.MethodPost, "/api/comms/messages", map[string]any{
		"from": piAgentID, "to": "codex-participant", "topicId": topicID,
		"subject": "Validate participant boundary", "body": "Return the bounded compatibility evidence.", "response": "required",
	}, http.StatusAccepted)["message"].(map[string]any)
	requestMessageID := requestMessage["id"].(string)
	if requestMessage["sourceTurnId"] != firstTurnID || requestMessage["response"] != "required" {
		t.Fatalf("required participant Message = %#v", requestMessage)
	}

	replyMessage := topicRequest(t, handler, http.MethodPost, "/api/comms/messages", map[string]any{
		"from": "codex-participant", "replyTo": requestMessageID,
		"body": "Compatibility evidence is verified; retain the conservative fallback.",
	}, http.StatusAccepted)["message"].(map[string]any)
	replyMessageID := replyMessage["id"].(string)
	if replyMessage["deliveryMode"] != "turn_steer" || replyMessage["deliveredTurnId"] != firstTurnID || replyMessage["topicId"] != topicID {
		t.Fatalf("participant reply delivery = %#v", replyMessage)
	}
	answeredMessage := topicRequest(t, handler, http.MethodGet, "/api/comms/messages/"+requestMessageID, nil, http.StatusOK)["message"].(map[string]any)
	if answeredMessage["status"] != "answered" || answeredMessage["resolution"] != "reply" {
		t.Fatalf("required Message after participant reply = %#v", answeredMessage)
	}
	assertRuntimeNeutralJSON(t, []any{requestMessage, replyMessage, answeredMessage}, nativePiPathFragment, "native-codex-thread-secret", "native-user-", "native-assistant-")
	if requests := topicRequest(t, handler, http.MethodGet, "/api/human-requests?state=all", nil, http.StatusOK)["requests"].([]any); len(requests) != 0 {
		t.Fatalf("participant reply entered Needs You: %#v", requests)
	}

	humanRequest := topicRequest(t, handler, http.MethodPost, "/api/human-requests", map[string]any{
		"agent": piAgentID, "expectation": "required", "topicId": topicID,
		"question": "Which rollout posture should the final result recommend?",
		"context":  "Both compatible postures are technically valid; the Owner owns the risk choice.",
	}, http.StatusCreated)["request"].(map[string]any)
	humanRequestID := humanRequest["id"].(string)
	if humanRequest["threadId"] != loomThreadID || humanRequest["sourceTurnId"] != firstTurnID || humanRequest["topicId"] != topicID {
		t.Fatalf("Needs You causality = %#v", humanRequest)
	}

	if err := os.WriteFile(paths.firstRelease, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForStoryAgentTurn(t, handler, piAgentID, firstTurnID)
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
	restartHandler := New(restarted, reopened, web).Handler()
	restoredAgent := topicRequest(t, restartHandler, http.MethodGet, "/api/agents/"+piAgentID, nil, http.StatusOK)["agent"].(map[string]any)
	if restoredAgent["id"] != piAgentID || restoredAgent["threadId"] != loomThreadID || restoredAgent["runtimeBinding"].(map[string]any)["kind"] != "pi" {
		t.Fatalf("restarted Pi Agent identity = %#v", restoredAgent)
	}
	assertRuntimeNeutralJSON(t, restoredAgent, nativePiPathFragment, "native-codex-thread-secret", "native-user-", "native-assistant-")

	topicRequest(t, restartHandler, http.MethodPost, "/api/human-requests/"+humanRequestID+"/answer", map[string]any{
		"answer": "Recommend the conservative rollout and retain the verified fallback.",
	}, http.StatusAccepted)
	deliveredRequest := waitForStoryHumanRequest(t, restartHandler, humanRequestID)
	resumedTurnID := deliveredRequest["resumedTurnId"].(string)
	if deliveredRequest["threadId"] != loomThreadID || resumedTurnID == "" || resumedTurnID == firstTurnID {
		t.Fatalf("Needs You resumed Thread = %#v", deliveredRequest)
	}
	waitForStoryAgentTurn(t, restartHandler, piAgentID, resumedTurnID)

	var history hub.RuntimeHistory
	historyResponse := storyJSONRequest(t, restartHandler, http.MethodGet, "/api/agents/"+piAgentID+"/thread/history?count=10", nil, http.StatusOK)
	if err := json.Unmarshal(historyResponse, &history); err != nil {
		t.Fatal(err)
	}
	if history.Total != 2 || len(history.Turns) != 2 || history.Turns[0].ID != firstTurnID || history.Turns[1].ID != resumedTurnID {
		t.Fatalf("restarted Pi history = %#v", history)
	}
	assertRuntimeNeutralJSON(t, json.RawMessage(historyResponse), nativePiPathFragment, "native-codex-thread-secret", "native-user-", "native-assistant-")

	resumePath, err := os.ReadFile(paths.resumeSession)
	if err != nil {
		t.Fatal(err)
	}
	wantSession := filepath.Join(dataDir, "pi", piAgentID, "session-"+piAgentID+".jsonl")
	gotSession, err := filepath.EvalSymlinks(strings.TrimSpace(string(resumePath)))
	if err != nil {
		t.Fatal(err)
	}
	wantSession, err = filepath.EvalSymlinks(wantSession)
	if err != nil {
		t.Fatal(err)
	}
	if gotSession != wantSession {
		t.Fatalf("Pi resumed Session = %q, want %q", strings.TrimSpace(string(resumePath)), wantSession)
	}
	prompts, err := os.ReadFile(paths.prompts)
	if err != nil {
		t.Fatal(err)
	}
	promptParts := strings.Split(string(prompts), "\n--- story prompt ---\n")
	if len(promptParts) < 3 {
		t.Fatalf("Pi prompt history = %q", prompts)
	}
	for _, want := range []string{"Release integration lead", "codex-reviewer", "Validate the bounded compatibility evidence", "Coordinate the participant review"} {
		if !strings.Contains(promptParts[1], want) {
			t.Fatalf("initial Pi prompt did not discover governed context %q:\n%s", want, promptParts[1])
		}
	}
	for _, want := range []string{"Release integration lead", "Mixed Runtime acceptance", "Participant evidence and Owner rollout choice are pending", "Recommend the conservative rollout"} {
		if !strings.Contains(promptParts[len(promptParts)-1], want) {
			t.Fatalf("resumed Pi prompt missing earlier context %q:\n%s", want, promptParts[len(promptParts)-1])
		}
	}
	steer, err := os.ReadFile(paths.steer)
	if err != nil || !strings.Contains(string(steer), "Compatibility evidence is verified") {
		t.Fatalf("participant reply steer = %q, err=%v", steer, err)
	}

	finalTopic := topicRequest(t, restartHandler, http.MethodPatch, "/api/topics/"+topicID, map[string]any{
		"actor": piAgentID, "expectedVersion": 1, "publishResult": true,
		"brief": map[string]any{
			"summary":      "Integrated result: compatibility is verified and the conservative rollout retains the fallback.",
			"currentState": "Ready for Owner review",
			"evidence":     []map[string]any{{"type": "message", "id": replyMessageID, "label": "Codex participant evidence"}},
		},
	}, http.StatusOK)["topic"].(map[string]any)
	if finalTopic["resultsReady"] != true || finalTopic["responsibleAgentId"] != piAgentID {
		t.Fatalf("final Topic result = %#v", finalTopic)
	}
	assertStoryTopicAudit(t, finalTopic, requestMessageID, replyMessageID, humanRequestID)
	assertRuntimeNeutralJSON(t, []any{humanRequest, deliveredRequest, finalTopic}, nativePiPathFragment, "native-codex-thread-secret", "native-user-", "native-assistant-")
}

type mixedRuntimeStoryPaths struct {
	prompts, steer, firstActive, firstRelease, resumeSession string
}

func configureMixedRuntimeStoryPi(t *testing.T) mixedRuntimeStoryPaths {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "pi")
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = \"--version\" ] && { echo 0.84.1; exit 0; }\nexec %q -test.run=^TestFakePiMixedRuntimeStoryProcess$ -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	paths := mixedRuntimeStoryPaths{
		prompts: filepath.Join(dir, "prompts"), steer: filepath.Join(dir, "steer"),
		firstActive: filepath.Join(dir, "first-active"), firstRelease: filepath.Join(dir, "first-release"),
		resumeSession: filepath.Join(dir, "resume-session"),
	}
	t.Setenv("PI_BIN", bin)
	t.Setenv("FAKE_PI_MIXED_RUNTIME_STORY", "1")
	t.Setenv("FAKE_PI_STORY_PROMPTS", paths.prompts)
	t.Setenv("FAKE_PI_STORY_STEER", paths.steer)
	t.Setenv("FAKE_PI_STORY_FIRST_ACTIVE", paths.firstActive)
	t.Setenv("FAKE_PI_STORY_FIRST_RELEASE", paths.firstRelease)
	t.Setenv("FAKE_PI_STORY_RESUME_SESSION", paths.resumeSession)
	return paths
}

func TestFakePiMixedRuntimeStoryProcess(t *testing.T) {
	if os.Getenv("FAKE_PI_MIXED_RUNTIME_STORY") == "" {
		return
	}
	args := os.Args
	sessionDir := mixedRuntimeStoryArgument(args, "--session-dir")
	sessionID := mixedRuntimeStoryArgument(args, "--session-id")
	sessionFile := mixedRuntimeStoryArgument(args, "--session")
	if sessionFile != "" {
		if err := os.WriteFile(os.Getenv("FAKE_PI_STORY_RESUME_SESSION"), []byte(sessionFile), 0o600); err != nil {
			os.Exit(31)
		}
		sessionID = strings.TrimSuffix(strings.TrimPrefix(filepath.Base(sessionFile), "session-"), ".jsonl")
	}
	if sessionDir == "" || sessionID == "" {
		os.Exit(32)
	}
	if sessionFile == "" {
		sessionFile = filepath.Join(sessionDir, "session-"+sessionID+".jsonl")
	}
	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		header := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":"2026-08-10T01:00:00Z","cwd":"/tmp/work"}`+"\n", sessionID)
		if err := os.WriteFile(sessionFile, []byte(header), 0o600); err != nil {
			os.Exit(33)
		}
	}

	reader := bufio.NewReader(os.Stdin)
	state := mixedRuntimeStoryReadCommand(reader)
	if state["type"] != "get_state" {
		os.Exit(34)
	}
	stateID, _ := state["id"].(string)
	fmt.Printf(`{"id":%q,"type":"response","command":"get_state","success":true,"data":{"sessionFile":%q,"sessionId":%q,"model":{"id":"fake-text","input":["text"]}}}`+"\n", stateID, sessionFile, sessionID)

	pendingSettlement := false
	firstTurn := mixedRuntimeStoryUserCount(sessionFile) == 0
	for {
		command, ok := mixedRuntimeStoryReadCommandOK(reader)
		if !ok {
			return
		}
		switch command["type"] {
		case "get_entries":
			mixedRuntimeStoryRespondEntries(command, sessionFile)
			if !pendingSettlement {
				continue
			}
			fmt.Print("{\"type\":\"agent_start\"}\n")
			if firstTurn {
				if err := os.WriteFile(os.Getenv("FAKE_PI_STORY_FIRST_ACTIVE"), []byte("active"), 0o600); err != nil {
					os.Exit(35)
				}
				steer := mixedRuntimeStoryReadCommand(reader)
				if steer["type"] != "steer" {
					os.Exit(36)
				}
				message, _ := steer["message"].(string)
				if err := os.WriteFile(os.Getenv("FAKE_PI_STORY_STEER"), []byte(message), 0o600); err != nil {
					os.Exit(37)
				}
				steerID, _ := steer["id"].(string)
				fmt.Printf(`{"id":%q,"type":"response","command":"steer","success":true}`+"\n", steerID)
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					if _, err := os.Stat(os.Getenv("FAKE_PI_STORY_FIRST_RELEASE")); err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if _, err := os.Stat(os.Getenv("FAKE_PI_STORY_FIRST_RELEASE")); err != nil {
					os.Exit(38)
				}
			}
			fmt.Print("{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n")
			fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":\"mixed Runtime evidence integrated\"}]}}\n")
			fmt.Print("{\"type\":\"agent_settled\"}\n")
			pendingSettlement = false
			firstTurn = false
		case "prompt":
			message, _ := command["message"].(string)
			if err := mixedRuntimeStoryAppendTurn(sessionFile, message); err != nil {
				os.Exit(39)
			}
			prompts, err := os.OpenFile(os.Getenv("FAKE_PI_STORY_PROMPTS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				os.Exit(40)
			}
			_, _ = prompts.WriteString("\n--- story prompt ---\n" + message)
			_ = prompts.Close()
			id, _ := command["id"].(string)
			fmt.Printf(`{"id":%q,"type":"response","command":"prompt","success":true}`+"\n", id)
			pendingSettlement = true
		default:
			os.Exit(41)
		}
	}
}

func mixedRuntimeStoryReadCommand(reader *bufio.Reader) map[string]any {
	command, ok := mixedRuntimeStoryReadCommandOK(reader)
	if !ok {
		os.Exit(42)
	}
	return command
}

func mixedRuntimeStoryReadCommandOK(reader *bufio.Reader) (map[string]any, bool) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, false
	}
	command := map[string]any{}
	if json.Unmarshal([]byte(line), &command) != nil {
		os.Exit(43)
	}
	return command, true
}

func mixedRuntimeStoryRespondEntries(command map[string]any, sessionFile string) {
	entries, leafID, err := mixedRuntimeStoryEntries(sessionFile)
	if err != nil {
		os.Exit(44)
	}
	data, _ := json.Marshal(map[string]any{"entries": entries, "leafId": leafID})
	id, _ := command["id"].(string)
	fmt.Printf(`{"id":%q,"type":"response","command":"get_entries","success":true,"data":%s}`+"\n", id, data)
}

func mixedRuntimeStoryEntries(sessionFile string) ([]json.RawMessage, string, error) {
	file, err := os.Open(sessionFile)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	entries := []json.RawMessage{}
	leafID := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var header struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(line, &header); err != nil {
			return nil, "", err
		}
		if header.Type == "session" {
			continue
		}
		entries = append(entries, json.RawMessage(line))
		leafID = header.ID
	}
	return entries, leafID, scanner.Err()
}

func mixedRuntimeStoryUserCount(sessionFile string) int {
	entries, _, err := mixedRuntimeStoryEntries(sessionFile)
	if err != nil {
		os.Exit(45)
	}
	count := 0
	for _, entry := range entries {
		var message struct {
			Message struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if json.Unmarshal(entry, &message) == nil && message.Message.Role == "user" {
			count++
		}
	}
	return count
}

func mixedRuntimeStoryAppendTurn(sessionFile, prompt string) error {
	entries, leafID, err := mixedRuntimeStoryEntries(sessionFile)
	if err != nil {
		return err
	}
	index := mixedRuntimeStoryUserCount(sessionFile) + 1
	userID := fmt.Sprintf("native-user-%d", index)
	assistantID := fmt.Sprintf("native-assistant-%d", index)
	user := map[string]any{
		"type": "message", "id": userID, "parentId": nil, "timestamp": fmt.Sprintf("2026-08-10T01:00:%02dZ", index*2),
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": prompt}}},
	}
	if len(entries) > 0 {
		user["parentId"] = leafID
	}
	assistant := map[string]any{
		"type": "message", "id": assistantID, "parentId": userID, "timestamp": fmt.Sprintf("2026-08-10T01:00:%02dZ", index*2+1),
		"message": map[string]any{
			"role": "assistant", "content": []map[string]any{{"type": "text", "text": "mixed Runtime evidence integrated"}},
			"stopReason": "stop", "model": "fake-pi",
		},
	}
	file, err := os.OpenFile(sessionFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, entry := range []map[string]any{user, assistant} {
		line, _ := json.Marshal(entry)
		if _, err := file.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

func mixedRuntimeStoryArgument(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}

func storyJSONRequest(t *testing.T, handler http.Handler, method, path string, body any, wantStatus int) []byte {
	t.Helper()
	result := topicRequest(t, handler, method, path, body, wantStatus)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func waitForStoryFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func waitForStoryAgentTurn(t *testing.T, handler http.Handler, agentID, turnID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		agent := topicRequest(t, handler, http.MethodGet, "/api/agents/"+agentID, nil, http.StatusOK)["agent"].(map[string]any)
		last, _ := agent["lastTurn"].(map[string]any)
		if agent["status"] == "idle" && last != nil && last["turnId"] == turnID && last["status"] == "completed" {
			return agent
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for Agent %s Turn %s", agentID, turnID)
	return nil
}

func waitForStoryHumanRequest(t *testing.T, handler http.Handler, requestID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		request := topicRequest(t, handler, http.MethodGet, "/api/human-requests/"+requestID, nil, http.StatusOK)["request"].(map[string]any)
		if request["deliveryStatus"] == "delivered" {
			return request
		}
		if request["deliveryStatus"] == "failed" {
			t.Fatalf("Needs You answer delivery failed: %#v", request)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for Needs You %s delivery", requestID)
	return nil
}

func assertStoryTopicAudit(t *testing.T, topic map[string]any, refs ...string) {
	t.Helper()
	wantTypes := map[string]bool{
		"created": false, "turn_started": false, "turn_completed": false,
		"message_created": false, "message_replied": false, "needs_you_created": false, "result_published": false,
	}
	seenRefs := map[string]bool{}
	for _, raw := range topic["events"].([]any) {
		event := raw.(map[string]any)
		if eventType, ok := event["type"].(string); ok {
			if _, wanted := wantTypes[eventType]; wanted {
				wantTypes[eventType] = true
			}
		}
		if ref, ok := event["ref"].(map[string]any); ok {
			seenRefs[ref["id"].(string)] = true
		}
	}
	for eventType, seen := range wantTypes {
		if !seen {
			t.Fatalf("Topic audit missing %s: %#v", eventType, topic["events"])
		}
	}
	for _, ref := range refs {
		if !seenRefs[ref] {
			t.Fatalf("Topic audit missing ref %s: %#v", ref, topic["events"])
		}
	}
}

func assertRuntimeNeutralJSON(t *testing.T, value any, secrets ...string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(string(encoded), secret) {
			t.Fatalf("public response leaked Runtime-native identity %q: %s", secret, encoded)
		}
	}
}
