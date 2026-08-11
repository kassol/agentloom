package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

func TestRuntimeNeutralModelControlStory(t *testing.T) {
	for _, fixture := range []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "pi", run: runPiRuntimeNeutralModelControlStory},
		{name: "codex", run: runCodexRuntimeNeutralModelControlStory},
	} {
		t.Run(fixture.name, fixture.run)
	}
}

func runPiRuntimeNeutralModelControlStory(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
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
		before.ThinkingLevel != "medium" {
		t.Fatalf("initial Pi models = %#v", before)
	}
	if strings.Join(before.Models[0].ThinkingLevels, ",") != "off,minimal,low,medium,high,xhigh" ||
		strings.Join(before.Models[1].ThinkingLevels, ",") != "low,medium,high" {
		t.Fatalf("per-model Pi thinking levels = %#v", before.Models)
	}
	if !before.Models[0].ImageInput || before.Models[1].ImageInput {
		t.Fatalf("per-model Pi image input = %#v", before.Models)
	}
	if !before.Current.ImageInput || !before.Current.Reasoning || before.Current.DefaultThinkingLevel != "medium" {
		t.Fatalf("Pi Current was not canonicalized from its catalog descriptor: %#v", before.Current)
	}
	if _, err := h.SwitchRuntimeModel(agent.ID, RuntimeModelSelection{Provider: "xai", Model: "grok-4.5", ThinkingLevel: "max"}); err == nil || !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("invalid pending Pi thinking selection error = %v", err)
	}
	unchanged, err := h.GetRuntimeModels(agent.ID)
	if err != nil || unchanged.Current.Provider != before.Current.Provider || unchanged.Current.ID != before.Current.ID || unchanged.ThinkingLevel != before.ThinkingLevel {
		t.Fatalf("invalid pending selection mutated Pi: before=%#v after=%#v err=%v", before, unchanged, err)
	}
	diagnostics, err := h.GetRuntimeDiagnostics(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	global, cancel := h.SubscribeGlobal()
	defer cancel()

	after, err := h.SwitchRuntimeModel(agent.ID, RuntimeModelSelection{Provider: "xai", Model: "grok-4.5", ThinkingLevel: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if after.Current.Provider != "xai" || after.Current.ID != "grok-4.5" || after.ThinkingLevel != "high" {
		t.Fatalf("switched Pi model = %#v", after)
	}
	if after.Current.ImageInput || !after.Current.Reasoning || after.Current.DefaultThinkingLevel != "medium" {
		t.Fatalf("switched Pi Current disagrees with catalog descriptor: %#v", after.Current)
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
	if descriptor, ok := capabilityDescriptor(view.CapabilitySnapshot, runtimecontract.CapabilityModelConfiguration); !ok || descriptor.Availability != runtimecontract.CapabilityAvailable {
		t.Fatalf("Pi capabilities = %#v", view.CapabilitySnapshot)
	}
	if len(view.CapabilitySnapshot.Capabilities) == 0 || view.CapabilitySnapshot.Capabilities[0].Scope.Model != "grok-4.5" {
		t.Fatalf("fresh capability snapshot = %#v", view.CapabilitySnapshot)
	}
	foundLiveSnapshot := false
	deadline := time.After(time.Second)
	for !foundLiveSnapshot {
		select {
		case event := <-global:
			if event.Type != "loom/agent-status" {
				continue
			}
			var payload struct {
				Model              string                             `json:"model"`
				CapabilitySnapshot runtimecontract.CapabilitySnapshot `json:"capabilitySnapshot"`
			}
			if json.Unmarshal(event.Data, &payload) == nil && payload.Model == "grok-4.5" && len(payload.CapabilitySnapshot.Capabilities) > 0 {
				foundLiveSnapshot = true
			}
		case <-deadline:
			t.Fatal("model switch did not emit a fresh Capability Snapshot")
		}
	}
	back, err := h.SwitchRuntimeModel(agent.ID, RuntimeModelSelection{Provider: "openai-codex", Model: "gpt-5.4-mini", ThinkingLevel: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if !back.Current.ImageInput || !back.Current.Reasoning || back.ThinkingLevel != "low" || back.Current.DefaultThinkingLevel != "medium" {
		t.Fatalf("Pi switch-back lost canonical vision/thinking truth: %#v", back)
	}
	image, err := h.StageThreadArtifact(agent.ID, "pi-model-control.png", "image/png", strings.NewReader("\x89PNG\r\n\x1a\nfixture"))
	if err != nil {
		t.Fatal(err)
	}
	imageTurn, err := h.SendTaskWithArtifacts(agent.ID, "verify Pi restored image input", []string{image.ID}, time.Minute)
	if err != nil {
		t.Fatalf("restored image-capable Pi selection rejected image: %v", err)
	}
	waitForPiTurn(t, h, agent.ID, imageTurn.TurnID)
	after, err = h.SwitchRuntimeModel(agent.ID, RuntimeModelSelection{Provider: "xai", Model: "grok-4.5", ThinkingLevel: "high"})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.agents[agent.ID].Status = "running"
	h.mu.Unlock()
	if _, err := h.SwitchRuntimeModel(agent.ID, RuntimeModelSelection{Provider: "openai-codex", Model: "gpt-5.4-mini"}); err == nil || !strings.Contains(err.Error(), "between Turns") {
		t.Fatalf("busy Pi model switch error = %v", err)
	}
	h.mu.Lock()
	h.agents[agent.ID].Status = "idle"
	h.mu.Unlock()
	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	staleNativeState, _ := json.Marshal(map[string]string{"provider": "openai-codex", "model": "gpt-5.4-mini", "thinkingLevel": "medium"})
	if err := os.WriteFile(filepath.Join(dataDir, "pi", agent.ID, "loom-model-state.json"), staleNativeState, 0o600); err != nil {
		t.Fatal(err)
	}
	reopenedStore, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	reopenedHub := New(reopenedStore)
	defer func() { reopenedHub.Shutdown(); _ = reopenedStore.Close() }()
	reopenedAgent, err := reopenedHub.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	reopenedModels, err := reopenedHub.GetRuntimeModels(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reopenedAgent.ID != agent.ID || reopenedAgent.ThreadID != agent.ThreadID || reopenedModels.Current.Provider != "xai" || reopenedModels.Current.ID != "grok-4.5" || reopenedModels.ThinkingLevel != "high" {
		t.Fatalf("reopened Pi model identity/state = Agent %#v models %#v", reopenedAgent.Agent, reopenedModels)
	}
}

func runCodexRuntimeNeutralModelControlStory(t *testing.T) {
	logPath := installFakeSharedCodexHost(t)
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h := New(st)
	agent, err := h.CreateAgent(CreateParams{Name: "codex-model", Cwd: "/tmp/one", RuntimeKind: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := h.GetRuntimeModels(agent.ID)
	if err != nil || len(state.Models) == 0 || state.Current.Provider != "openai" {
		t.Fatalf("initial Codex model state = %#v, err=%v", state, err)
	}
	if _, err := h.SwitchRuntimeModel(agent.ID, RuntimeModelSelection{Provider: "deepseek", Model: "missing", ThinkingLevel: "max"}); err == nil {
		t.Fatal("invalid Codex pending selection was accepted")
	}
	unchanged, err := h.GetRuntimeModels(agent.ID)
	if err != nil || unchanged.Current.Provider != state.Current.Provider || unchanged.Current.ID != state.Current.ID || unchanged.ThinkingLevel != state.ThinkingLevel {
		t.Fatalf("invalid Codex pending selection mutated active state: before=%#v after=%#v err=%v", state, unchanged, err)
	}
	selected, err := h.SwitchRuntimeModel(agent.ID, RuntimeModelSelection{Provider: "deepseek", Model: "deepseek-v4-flash", ThinkingLevel: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Current.Provider != "deepseek" || selected.Current.ID != "deepseek-v4-flash" || selected.Current.ImageInput || selected.ThinkingLevel != "low" {
		t.Fatalf("selected Codex model = %#v", selected)
	}
	selected, err = h.SwitchRuntimeModel(agent.ID, RuntimeModelSelection{Provider: "deepseek", Model: "deepseek-v4-flash", ThinkingLevel: "max"})
	if err != nil || selected.ThinkingLevel != "max" {
		t.Fatalf("updated Codex non-default thinking = %#v, err=%v", selected, err)
	}
	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedStore, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	reopenedHub := New(reopenedStore)
	defer func() { reopenedHub.Shutdown(); _ = reopenedStore.Close() }()
	reopenedAgent, err := reopenedHub.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	reopenedState, err := reopenedHub.GetRuntimeModels(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reopenedAgent.ID != agent.ID || reopenedAgent.ThreadID != agent.ThreadID || reopenedState.Current.Provider != "deepseek" || reopenedState.Current.ID != "deepseek-v4-flash" || reopenedState.ThinkingLevel != "max" || reopenedState.Current.ImageInput {
		t.Fatalf("reopened Codex identity/state = Agent %#v models %#v", reopenedAgent.Agent, reopenedState)
	}
	resume := lastRequestParams(t, logPath, "thread/resume")
	if resume["modelProvider"] != "deepseek" || resume["model"] != "deepseek-v4-flash" {
		t.Fatalf("reopened Codex model selection did not reach native resume: %#v", resume)
	}
	waitForModelStoryTerminal(t, reopenedHub, agent.ID)
	turn := lastRequestParams(t, logPath, "turn/start")
	if turn["model"] != "deepseek-v4-flash" || turn["effort"] != "max" {
		t.Fatalf("reopened Codex Turn did not use persisted non-default model effort: %#v", turn)
	}
	back, err := reopenedHub.SwitchRuntimeModel(agent.ID, RuntimeModelSelection{Provider: state.Current.Provider, Model: state.Current.ID, ThinkingLevel: state.Current.DefaultThinkingLevel})
	if err != nil {
		t.Fatal(err)
	}
	if back.Current.Provider != state.Current.Provider || back.Current.ID != state.Current.ID || !back.Current.ImageInput || back.ThinkingLevel != state.Current.DefaultThinkingLevel {
		t.Fatalf("Codex provider/model switch-back truth = %#v", back)
	}
	image, err := reopenedHub.StageThreadArtifact(agent.ID, "model-control.png", "image/png", strings.NewReader("\x89PNG\r\n\x1a\nfixture"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := reopenedHub.SendTaskWithArtifacts(agent.ID, "verify restored image input", []string{image.ID}, time.Minute)
	if err != nil {
		t.Fatalf("restored image-capable Codex selection rejected image: %v", err)
	}
	waitForPiTurn(t, reopenedHub, agent.ID, result.TurnID)
	encodedTurn, _ := json.Marshal(lastRequestParams(t, logPath, "turn/start"))
	if !strings.Contains(string(encodedTurn), "localImage") {
		t.Fatalf("restored image input did not reach native Codex Turn: %s", encodedTurn)
	}
}

func waitForModelStoryTerminal(t *testing.T, h *Hub, agentID string) {
	t.Helper()
	events, cancel, err := h.Subscribe(agentID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if _, err := h.SendTask(agentID, "model control restart verification", time.Minute); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if event.Type == "loom/turn-completed" {
				return
			}
			if event.Type == "loom/turn-failed" || event.Type == "loom/turn-interrupted" {
				t.Fatalf("model control verification Turn ended with %s: %s", event.Type, event.Data)
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for model control verification Turn")
		}
	}
}

func TestPiModelNativeWriteFailuresAreTypedIndeterminate(t *testing.T) {
	for _, scenario := range []string{"set-model-timeout", "thinking-timeout", "rollback-failure"} {
		t.Run(scenario, func(t *testing.T) {
			configureFakePiModelRPC(t)
			t.Setenv("FAKE_PI_MODEL_FAILURE", scenario)
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			h := testHub(st)
			h.stop = make(chan struct{})
			defer func() { h.Shutdown(); _ = st.Close() }()
			agent, err := h.CreateAgent(CreateParams{Name: "pi-indeterminate", Cwd: t.TempDir(), RuntimeKind: "pi"})
			if err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			rt := h.runtimes[agent.ID]
			h.mu.Unlock()
			capability := rt.runtimeContract.(runtimecontract.ModelControlCapability)
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			_, failure := capability.SelectModel(ctx, rt.binding, runtimecontract.ModelSelection{Provider: "xai", Model: "grok-4.5", ThinkingLevel: "high"})
			if failure == nil || failure.Code != "model_selection_indeterminate" {
				t.Fatalf("%s failure = %#v", scenario, failure)
			}
		})
	}
}

func TestPiRemovedCurrentModelRemainsVisibleAndCanSwitchToCatalog(t *testing.T) {
	configureFakePiModelRPC(t)
	t.Setenv("FAKE_PI_REMOVED_CURRENT", "1")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.stop = make(chan struct{})
	defer func() { h.Shutdown(); _ = st.Close() }()
	agent, err := h.CreateAgent(CreateParams{Name: "pi-removed-current", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := h.GetRuntimeModels(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Current.Provider != "removed" || state.Current.ID != "vision-old" || !state.Current.ImageInput || !state.Current.Reasoning || state.ThinkingLevel != "high" || len(state.Models) != 2 {
		t.Fatalf("synthetic removed Current = %#v", state)
	}
	selected, err := h.SwitchRuntimeModel(agent.ID, RuntimeModelSelection{Provider: "xai", Model: "grok-4.5", ThinkingLevel: "high"})
	if err != nil || selected.Current.Provider != "xai" || selected.Current.ID != "grok-4.5" {
		t.Fatalf("switch from removed Current = %#v err=%v", selected, err)
	}
}

func configureFakePiModelRPC(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "pi")
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = \"--version\" ] && { echo 0.84.1; exit 0; }\nexec %q -test.run=TestFakePiModelRPCProcess -- \"$@\"\n", os.Args[0])
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
	sessionFile := argumentValue(args, "--session")
	if sessionDir == "" || (sessionID == "" && sessionFile == "") || !containsArgumentPair(args, "--mode", "rpc") {
		os.Exit(40)
	}
	if sessionFile == "" {
		sessionFile = filepath.Join(sessionDir, "session-"+sessionID+".jsonl")
	} else if encoded, readErr := os.ReadFile(sessionFile); readErr == nil {
		var header struct {
			ID string `json:"id"`
		}
		if first, _, ok := strings.Cut(string(encoded), "\n"); ok && json.Unmarshal([]byte(first), &header) == nil {
			sessionID = header.ID
		}
	}
	if sessionID == "" {
		sessionID = strings.TrimSuffix(strings.TrimPrefix(filepath.Base(sessionFile), "session-"), ".jsonl")
	}
	_ = os.MkdirAll(sessionDir, 0o700)
	_ = os.WriteFile(sessionFile, []byte(fmt.Sprintf(`{"type":"session","version":3,"id":%q}`, sessionID)+"\n"), 0o600)

	reader := bufio.NewReader(os.Stdin)
	provider, model, thinkingLevel := "openai-codex", "gpt-5.4-mini", "medium"
	modelStatePath := filepath.Join(sessionDir, "loom-model-state.json")
	var saved struct {
		Provider      string `json:"provider"`
		Model         string `json:"model"`
		ThinkingLevel string `json:"thinkingLevel"`
	}
	if encoded, readErr := os.ReadFile(modelStatePath); readErr == nil && json.Unmarshal(encoded, &saved) == nil && saved.Provider != "" && saved.Model != "" {
		provider, model, thinkingLevel = saved.Provider, saved.Model, saved.ThinkingLevel
	}
	persistModelState := func() {
		encoded, _ := json.Marshal(map[string]string{"provider": provider, "model": model, "thinkingLevel": thinkingLevel})
		_ = os.WriteFile(modelStatePath, encoded, 0o600)
	}
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
			if os.Getenv("FAKE_PI_REMOVED_CURRENT") != "" {
				fmt.Printf(`{"id":%q,"type":"response","command":"get_state","success":true,"data":{"sessionFile":%q,"sessionId":%q,"thinkingLevel":"high","model":{"provider":"removed","id":"vision-old","input":["text","image"],"reasoning":true}}}`+"\n", id, sessionFile, sessionID)
				continue
			}
			fmt.Printf(`{"id":%q,"type":"response","command":"get_state","success":true,"data":{"sessionFile":%q,"sessionId":%q,"thinkingLevel":%q,"model":{"provider":%q,"id":%q,"input":["text"]}}}`+"\n", id, sessionFile, sessionID, thinkingLevel, provider, model)
		case "get_available_models":
			if os.Getenv("FAKE_PI_REMOVED_CURRENT") != "" {
				fmt.Printf(`{"id":%q,"type":"response","command":"get_available_models","success":true,"data":{"models":[{"provider":"xai","id":"grok-4.5","input":["text"],"reasoning":true,"thinkingLevelMap":{"high":"high"}}]}}`+"\n", id)
				continue
			}
			fmt.Printf(`{"id":%q,"type":"response","command":"get_available_models","success":true,"data":{"models":[{"provider":"openai-codex","id":"gpt-5.4-mini","input":["text","image"],"contextWindow":128000,"reasoning":true,"thinkingLevelMap":{"minimal":"low","xhigh":"xhigh"}},{"provider":"xai","id":"grok-4.5","input":["text"],"contextWindow":256000,"reasoning":true,"thinkingLevelMap":{"off":null,"minimal":null,"low":"low","medium":"medium","high":"high","xhigh":null,"max":null}}]}}`+"\n", id)
		case "get_entries":
			respondFakePiEntries(command, sessionFile)
		case "set_model":
			nextProvider, _ := command["provider"].(string)
			nextModel, _ := command["modelId"].(string)
			if (nextProvider != "xai" || nextModel != "grok-4.5") && (nextProvider != "openai-codex" || nextModel != "gpt-5.4-mini") {
				fmt.Printf(`{"id":%q,"type":"response","command":"set_model","success":false,"error":"Model not found"}`+"\n", id)
				continue
			}
			provider, model = nextProvider, nextModel
			persistModelState()
			if os.Getenv("FAKE_PI_MODEL_FAILURE") == "set-model-timeout" && nextProvider == "xai" {
				time.Sleep(time.Second)
				continue
			}
			if os.Getenv("FAKE_PI_MODEL_FAILURE") == "rollback-failure" {
				if nextProvider == "xai" {
					fmt.Printf(`{"id":%q,"type":"response","command":"set_model","success":true,"data":{"provider":"","id":""}}`+"\n", id)
					continue
				}
				fmt.Printf(`{"id":%q,"type":"response","command":"set_model","success":false,"error":"rollback unavailable"}`+"\n", id)
				continue
			}
			fmt.Printf(`{"id":%q,"type":"response","command":"set_model","success":true,"data":{"provider":%q,"id":%q,"input":["text"]}}`+"\n", id, provider, model)
		case "set_thinking_level":
			thinkingLevel, _ = command["level"].(string)
			persistModelState()
			if os.Getenv("FAKE_PI_MODEL_FAILURE") == "thinking-timeout" && thinkingLevel == "high" {
				time.Sleep(time.Second)
				continue
			}
			fmt.Printf(`{"id":%q,"type":"response","command":"set_thinking_level","success":true}`+"\n", id)
		case "prompt":
			message, _ := json.Marshal(command["message"])
			appendFakePiTurn(t, sessionFile, string(message), "model image accepted", "stop")
			fmt.Printf(`{"id":%q,"type":"response","command":"prompt","success":true}`+"\n", id)
			fmt.Print("{\"type\":\"agent_start\"}\n")
			fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":\"model image accepted\"}]}}\n")
			fmt.Print("{\"type\":\"agent_end\",\"messages\":[]}\n")
			fmt.Print("{\"type\":\"agent_settled\"}\n")
		default:
			os.Exit(42)
		}
	}
}
