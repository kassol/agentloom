package hub

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yan5xu/codex-loom/internal/store"
)

func TestPiAgentSwitchesNativeModelWithoutReplacingSession(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiModelRPC(t)
	h := testHub(st)
	h.stop = make(chan struct{})
	defer h.Shutdown()

	agent, err := h.CreateAgent(CreateParams{Name: "pi-model", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := h.GetRuntimeModels(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Current.Provider != "openai-codex" || before.Current.ID != "gpt-5.4-mini" || len(before.Models) != 2 ||
		before.ThinkingLevel != "medium" || strings.Join(before.ThinkingLevels, ",") != "off,minimal,low,medium,high" {
		t.Fatalf("initial Pi models = %#v", before)
	}
	if strings.Join(before.Models[0].ThinkingLevels, ",") != "off,minimal,low,medium,high,xhigh" ||
		strings.Join(before.Models[1].ThinkingLevels, ",") != "low,medium,high" {
		t.Fatalf("per-model Pi thinking levels = %#v", before.Models)
	}
	if !before.Models[0].ImageInput || before.Models[1].ImageInput {
		t.Fatalf("per-model Pi image input = %#v", before.Models)
	}
	diagnostics, err := h.GetRuntimeDiagnostics(agent.ID)
	if err != nil {
		t.Fatal(err)
	}

	after, err := h.SwitchRuntimeModel(agent.ID, RuntimeModelSelection{Provider: "xai", Model: "grok-4.5", ThinkingLevel: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if after.Current.Provider != "xai" || after.Current.ID != "grok-4.5" || after.ThinkingLevel != "high" {
		t.Fatalf("switched Pi model = %#v", after)
	}
	view, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	diagnosticsAfter, err := h.GetRuntimeDiagnostics(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != agent.ID || view.ThreadID != agent.ThreadID || diagnosticsAfter.NativeRef != diagnostics.NativeRef {
		t.Fatalf("model switch replaced durable identity: before=%#v/%#v after=%#v/%#v", agent, diagnostics, view, diagnosticsAfter)
	}
	if !view.RuntimeCapabilities.Provider {
		t.Fatalf("Pi capabilities = %#v", view.RuntimeCapabilities)
	}
	h.mu.Lock()
	h.agents[agent.ID].Status = "running"
	h.mu.Unlock()
	if _, err := h.SwitchRuntimeModel(agent.ID, RuntimeModelSelection{Provider: "openai-codex", Model: "gpt-5.4-mini"}); err == nil || !strings.Contains(err.Error(), "between Turns") {
		t.Fatalf("busy Pi model switch error = %v", err)
	}
}

func configureFakePiModelRPC(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "pi")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestFakePiModelRPCProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_BIN", bin)
	t.Setenv("FAKE_PI_MODEL_RPC", "1")
}

func TestFakePiModelRPCProcess(t *testing.T) {
	if os.Getenv("FAKE_PI_MODEL_RPC") == "" {
		return
	}
	args := os.Args
	sessionDir := argumentValue(args, "--session-dir")
	sessionID := argumentValue(args, "--session-id")
	if sessionDir == "" || sessionID == "" || !containsArgumentPair(args, "--mode", "rpc") {
		os.Exit(40)
	}
	sessionFile := filepath.Join(sessionDir, "session-"+sessionID+".jsonl")
	_ = os.MkdirAll(sessionDir, 0o700)
	_ = os.WriteFile(sessionFile, []byte(fmt.Sprintf(`{"type":"session","version":3,"id":%q}`, sessionID)+"\n"), 0o600)

	reader := bufio.NewReader(os.Stdin)
	provider, model, thinkingLevel := "openai-codex", "gpt-5.4-mini", "medium"
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		var command map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &command) != nil {
			os.Exit(41)
		}
		id, _ := command["id"].(string)
		switch command["type"] {
		case "get_state":
			fmt.Printf(`{"id":%q,"type":"response","command":"get_state","success":true,"data":{"sessionFile":%q,"sessionId":%q,"thinkingLevel":%q,"model":{"provider":%q,"id":%q,"input":["text"]}}}`+"\n", id, sessionFile, sessionID, thinkingLevel, provider, model)
		case "get_available_models":
			fmt.Printf(`{"id":%q,"type":"response","command":"get_available_models","success":true,"data":{"models":[{"provider":"openai-codex","id":"gpt-5.4-mini","input":["text","image"],"contextWindow":128000,"reasoning":true,"thinkingLevelMap":{"minimal":"low","xhigh":"xhigh"}},{"provider":"xai","id":"grok-4.5","input":["text"],"contextWindow":256000,"reasoning":true,"thinkingLevelMap":{"off":null,"minimal":null,"low":"low","medium":"medium","high":"high","xhigh":null,"max":null}}]}}`+"\n", id)
		case "get_available_thinking_levels":
			fmt.Printf(`{"id":%q,"type":"response","command":"get_available_thinking_levels","success":true,"data":{"levels":["off","minimal","low","medium","high"]}}`+"\n", id)
		case "set_model":
			provider, _ = command["provider"].(string)
			model, _ = command["modelId"].(string)
			if provider != "xai" || model != "grok-4.5" {
				fmt.Printf(`{"id":%q,"type":"response","command":"set_model","success":false,"error":"Model not found"}`+"\n", id)
				continue
			}
			fmt.Printf(`{"id":%q,"type":"response","command":"set_model","success":true,"data":{"provider":%q,"id":%q,"input":["text"]}}`+"\n", id, provider, model)
		case "set_thinking_level":
			thinkingLevel, _ = command["level"].(string)
			fmt.Printf(`{"id":%q,"type":"response","command":"set_thinking_level","success":true}`+"\n", id)
		default:
			os.Exit(42)
		}
	}
}
