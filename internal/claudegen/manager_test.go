package claudegen

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestEmbeddedBridgeHasValidJavaScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	path := filepath.Join(t.TempDir(), "bridge.mjs")
	if err := os.WriteFile(path, currentAssets().Bridge, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
		t.Fatalf("embedded bridge syntax: %v\n%s", err, output)
	}
}

func TestEmbeddedBridgeAcceptsOnlyExplicitLocalClaudeSubscriptionEvidence(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	app := t.TempDir()
	module := filepath.Join(app, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(module, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "bridge.mjs"), currentAssets().Bridge, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "package.json"), []byte(`{"type":"module","exports":"./index.js","version":"0.3.228","claudeCodeVersion":"2.1.228"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := `
export async function getSessionInfo() { return undefined; }
export function query() {
  return {
    async accountInfo() {
      return process.env.LOOM_TEST_SUBSCRIPTION === "missing"
        ? {apiProvider:"firstParty"}
        : {apiProvider:"firstParty",subscriptionType:"Claude Max",email:"must-not-escape@example.com"};
    },
    close() {}
  };
}`
	if err := os.WriteFile(filepath.Join(module, "index.js"), []byte(fake), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, missing bool) map[string]any {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, node, filepath.Join(app, "bridge.mjs"))
		if missing {
			cmd.Env = append(os.Environ(), "LOOM_TEST_SUBSCRIPTION=missing")
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		scanner := bufio.NewScanner(stdout)
		read := func() map[string]any {
			t.Helper()
			if !scanner.Scan() {
				t.Fatalf("bridge output ended: %v", scanner.Err())
			}
			var frame map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
				t.Fatalf("bridge frame = %s: %v", scanner.Bytes(), err)
			}
			return frame
		}
		write := func(frame any) {
			encoded, _ := json.Marshal(frame)
			if _, err := stdin.Write(append(encoded, '\n')); err != nil {
				t.Fatal(err)
			}
		}
		_ = read()
		write(map[string]any{"kind": "initialize", "requestId": "init", "agentId": "agent"})
		_ = read()
		write(map[string]any{
			"kind": "command", "command": "inspect_configuration", "requestId": "inspect", "operation": "op-inspect",
			"payload": map[string]any{"cwd": app, "configuration": map[string]any{
				"settingSources": []string{"user"}, "authentication": map[string]any{"category": "subscription", "source": "claude_ai"},
			}},
		})
		return read()
	}
	t.Run("accepted without identity leakage", func(t *testing.T) {
		frame := run(t, false)
		encoded, _ := json.Marshal(frame)
		if frame["accepted"] != true || bytes.Contains(encoded, []byte("must-not-escape")) {
			t.Fatalf("subscription evidence = %s", encoded)
		}
		auth := frame["data"].(map[string]any)["authentication"].(map[string]any)
		if auth["category"] != "subscription" || auth["source"] != "claude_ai" || auth["validation"] != "accepted" || len(auth) != 3 {
			t.Fatalf("safe subscription evidence = %#v", auth)
		}
	})
	t.Run("missing subscription rejected", func(t *testing.T) {
		if frame := run(t, true); frame["accepted"] != false {
			t.Fatalf("missing subscription accepted = %#v", frame)
		}
	})
}

func TestReadActiveGenerationIDDoesNotRequireInstalledRuntime(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(`{"version":1,"active":"generation-static"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := New(Options{Root: root, Manifest: Manifest{ID: "generation-static"}, Platform: Platform{Supported: true}})
	got, err := manager.ReadActiveGenerationID()
	if err != nil || got != "generation-static" {
		t.Fatalf("ReadActiveGenerationID() = %q, %v", got, err)
	}
}

func TestReadActiveGenerationIDRejectsAnotherBuildGeneration(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(`{"version":1,"active":"generation-old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := New(Options{Root: root, Manifest: Manifest{ID: "generation-current"}, Platform: Platform{Supported: true}})
	if got, err := manager.ReadActiveGenerationID(); err == nil || got != "" {
		t.Fatalf("ReadActiveGenerationID() = %q, %v; want fail closed", got, err)
	}
}

func TestReadActiveGenerationIDRejectsUnsupportedPlatform(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(`{"version":1,"active":"generation-current"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := New(Options{Root: root, Manifest: Manifest{ID: "generation-current"}, Platform: Platform{Supported: false}})
	if got, err := manager.ReadActiveGenerationID(); err == nil || got != "" {
		t.Fatalf("ReadActiveGenerationID() = %q, %v; want unsupported", got, err)
	}
}

func TestEmbeddedBridgeWaitsForAcceptedCausalInputBeforeTerminal(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	app := t.TempDir()
	module := filepath.Join(app, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(module, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "bridge.mjs"), currentAssets().Bridge, 0o600); err != nil {
		t.Fatal(err)
	}
	pkg := `{"type":"module","exports":"./index.js","version":"0.3.228","claudeCodeVersion":"2.1.228"}`
	if err := os.WriteFile(filepath.Join(module, "package.json"), []byte(pkg), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := `
export async function getSessionInfo() { return undefined; }
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
export function query({prompt, options}) {
  if (JSON.stringify(options.settingSources) !== JSON.stringify(["project","local"])) throw new Error("settingSources not explicit");
  const queued = [];
	let selectedModel = "", selectedEffort;
  return {
	async accountInfo() { return {apiProvider:"firstParty",apiKeySource:"project",email:"must-not-escape@example.com"}; },
		async reloadSkills() { return {skills:[{name:"ship",description:"skill",argumentHint:""}]}; },
		async reloadPlugins() { return {commands:[{name:"ship",description:"skill",argumentHint:""},{name:"review",description:"command",argumentHint:""}],agents:[],plugins:[{name:"safe-plugin",path:"/private/native/plugin"}],mcpServers:[{name:"safe-mcp",status:"connected",config:{command:"secret-helper",args:["--token","secret"]}}],error_count:0}; },
	async supportedModels() { return [{value:"sonnet",resolvedModel:"claude-sonnet-4-6",displayName:"Sonnet",supportsEffort:true,supportedEffortLevels:["low","high"],supportsAdaptiveThinking:true},{value:"legacy",resolvedModel:"claude-3-7-sonnet-20250219",displayName:"Legacy"},{value:"unknown",displayName:"Unknown"}]; },
	async setModel(model) { selectedModel = model; },
	async applyFlagSettings(settings) { selectedEffort = settings.effortLevel; },
    async streamInput(stream) { for await (const message of stream) queued.push(message); },
    async interrupt() {}, close() {},
    async *[Symbol.asyncIterator]() {
      const first = await prompt[Symbol.asyncIterator]().next();
	  const firstContent = first.value.message.content;
	  const configured = selectedModel === "sonnet" && selectedEffort === "high" && firstContent.some((part) => part.type === "image" && part.source.media_type === "image/png" && part.source.data === "aW1hZ2U=");
	  yield {type:"system", subtype:"hook_started", hook_id:"startup", hook_name:"SessionStart", hook_event:"SessionStart", uuid:"hook-started", session_id:options.sessionId};
	  yield {type:"system", subtype:"hook_response", hook_id:"startup", hook_name:"SessionStart", hook_event:"SessionStart", output:"ready", stdout:"", stderr:"", outcome:"success", uuid:"hook-response", session_id:options.sessionId};
	  yield {type:"system", subtype:"init", session_id:options.sessionId, model:"claude-sonnet-4-6"};
      await delay(80);
	  yield {type:"assistant", uuid:"first", session_id:options.sessionId, message:{model:"claude-sonnet-4-6",content:[{type:"text",text:configured?"configured-image-observed":"missing-configuration"}]}};
      yield {type:"result", subtype:"success", session_id:options.sessionId, usage:{input_tokens:1,output_tokens:1},num_turns:1,total_cost_usd:0,modelUsage:{"claude-main":{inputTokens:1,outputTokens:0,cacheReadInputTokens:0,costUSD:0,contextWindow:200000,provider:"firstParty"}}};
      while (!queued.length) await delay(5);
      const causal = queued.shift();
	  const sawDeveloper = JSON.stringify(causal.message.content).includes("loom_developer_context");
	  yield {type:"assistant", uuid:"causal", session_id:options.sessionId, message:{model:"claude-sonnet-4-6",content:[{type:"text",text:sawDeveloper?"causal-developer-observed":"missing-developer"}]}};
      yield {type:"result", subtype:"success", session_id:options.sessionId, usage:{input_tokens:2,output_tokens:2},num_turns:1,total_cost_usd:0,modelUsage:{"claude-main":{inputTokens:3,outputTokens:2,cacheReadInputTokens:0,cacheCreationInputTokens:1,costUSD:0.000006,contextWindow:200000,provider:"firstParty"},"claude-side":{inputTokens:4,outputTokens:1,cacheReadInputTokens:2,cacheCreationInputTokens:0,costUSD:0.000007,contextWindow:100000,provider:"gateway"}}};
    }
  };
}`
	if err := os.WriteFile(filepath.Join(module, "index.js"), []byte(fake), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, filepath.Join(app, "bridge.mjs"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	scanner := bufio.NewScanner(stdout)
	read := func() map[string]any {
		t.Helper()
		if !scanner.Scan() {
			t.Fatalf("bridge output ended: %v", scanner.Err())
		}
		var frame map[string]any
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			t.Fatalf("frame = %s", scanner.Bytes())
		}
		return frame
	}
	write := func(frame any) {
		t.Helper()
		encoded, _ := json.Marshal(frame)
		encoded = append(encoded, '\n')
		if _, err := stdin.Write(encoded); err != nil {
			t.Fatal(err)
		}
	}
	_ = read()
	write(map[string]any{"kind": "initialize", "requestId": "init", "agentId": "agent"})
	_ = read()
	configuration := map[string]any{"settingSources": []string{"project", "local"}, "authentication": map[string]any{"category": "console", "source": "api_key"}}
	write(map[string]any{"kind": "command", "command": "inspect_resources", "requestId": "empty-resources", "operation": "op-empty-resources", "payload": map[string]any{"cwd": app, "configuration": map[string]any{"settingSources": []string{}, "authentication": map[string]any{"category": "console", "source": "api_key"}}}})
	emptyResources := read()
	if emptyResources["accepted"] != false {
		t.Fatalf("empty settingSources accepted = %#v", emptyResources)
	}
	write(map[string]any{"kind": "command", "command": "inspect_resources", "requestId": "helper-resources", "operation": "op-helper-resources", "payload": map[string]any{"cwd": app, "configuration": map[string]any{"settingSources": []string{"project"}, "authentication": map[string]any{"category": "console", "source": "helper"}}}})
	helperResources := read()
	if helperResources["accepted"] != false {
		t.Fatalf("unsupported helper authentication accepted = %#v", helperResources)
	}
	write(map[string]any{"kind": "command", "command": "inspect_model_control", "requestId": "inspect", "operation": "op-inspect", "payload": map[string]any{"provider": "anthropic", "model": "sonnet", "thinkingLevel": "default", "configuration": configuration}})
	inspected := read()
	state := inspected["data"].(map[string]any)
	catalogModels := state["models"].([]any)
	if state["thinkingLevel"] != "default" || catalogModels[0].(map[string]any)["id"] != "default" || catalogModels[0].(map[string]any)["imageInput"] != false || catalogModels[1].(map[string]any)["imageInput"] != true || catalogModels[2].(map[string]any)["imageInput"] != true || catalogModels[3].(map[string]any)["imageInput"] != false {
		t.Fatalf("generation model evidence = %#v", state)
	}
	write(map[string]any{"kind": "command", "command": "select_model", "requestId": "select", "operation": "op-select", "payload": map[string]any{"sessionRef": "11111111-1111-4111-8111-111111111111", "cwd": app, "current": map[string]any{"provider": "anthropic", "model": "sonnet", "thinkingLevel": "default"}, "selection": map[string]any{"provider": "anthropic", "model": "sonnet", "thinkingLevel": "high"}, "configuration": configuration}})
	selected := read()
	if selected["accepted"] != true || selected["data"].(map[string]any)["thinkingLevel"] != "high" {
		t.Fatalf("selected model evidence = %#v", selected)
	}
	write(map[string]any{"kind": "command", "command": "inspect_resources", "requestId": "resources", "operation": "op-resources", "payload": map[string]any{"cwd": app, "configuration": configuration}})
	resources := read()
	encodedResources, _ := json.Marshal(resources)
	if resources["accepted"] != true || bytes.Contains(encodedResources, []byte("must-not-escape")) || bytes.Contains(encodedResources, []byte("private/native")) || bytes.Contains(encodedResources, []byte("secret-helper")) || bytes.Contains(encodedResources, []byte("--token")) {
		t.Fatalf("unsafe Claude resource evidence = %s", encodedResources)
	}
	resourceData := resources["data"].(map[string]any)
	items := resourceData["resources"].([]any)
	wantIDs := []string{"command:review", "extension:safe-plugin", "mcp:safe-mcp", "skill:ship"}
	gotIDs := make([]string, 0, len(items))
	for _, item := range items {
		gotIDs = append(gotIDs, item.(map[string]any)["id"].(string))
	}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("Claude resource kinds = %#v, want %#v", gotIDs, wantIDs)
	}
	auth := resourceData["configuration"].(map[string]any)["authentication"].(map[string]any)
	if auth["category"] != "console" || auth["source"] != "api_key" || auth["validation"] != "accepted" || len(auth) != 3 {
		t.Fatalf("Claude authentication evidence = %#v", auth)
	}
	session := "11111111-1111-4111-8111-111111111111"
	imagePath := filepath.Join(app, "screen.png")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	write(map[string]any{"kind": "command", "command": "start_turn", "requestId": "start", "turnId": "turn", "operation": "op-start", "payload": map[string]any{"sessionRef": session, "cwd": app, "model": "sonnet", "thinkingLevel": "high", "configuration": configuration, "input": []any{map[string]any{"kind": "text", "role": "developer", "text": "first-dev"}, map[string]any{"kind": "text", "role": "user", "text": "first-user"}, map[string]any{"kind": "image", "role": "user", "ref": imagePath, "mimeType": "image/png"}}}})
	sentContinue := false
	terminalCount := 0
	texts := []string{}
	usageFrames := []map[string]any{}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && terminalCount < 1 {
		frame := read()
		if frame["kind"] == "response" && frame["operation"] == "op-start" && !sentContinue {
			sentContinue = true
			write(map[string]any{"kind": "command", "command": "continue_turn", "requestId": "continue", "turnId": "turn", "operation": "op-continue", "payload": map[string]any{"sessionRef": session, "runtimeTurnRef": frame["data"].(map[string]any)["runtimeTurnRef"], "input": []any{map[string]any{"kind": "text", "role": "developer", "text": "causal-dev"}, map[string]any{"kind": "text", "role": "user", "text": "causal-user"}}}})
		}
		if frame["kind"] == "event" && frame["event"] == "content" {
			content := frame["data"].(map[string]any)["content"].(map[string]any)
			if text, ok := content["text"].(string); ok {
				texts = append(texts, text)
			}
		}
		if frame["kind"] == "event" && frame["event"] == "usage" {
			usageFrames = append(usageFrames, frame["data"].(map[string]any))
		}
		if frame["kind"] == "event" && frame["event"] == "turn_completed" {
			terminalCount++
			if frame["operation"] != "op-start" {
				t.Fatalf("terminal operation = %#v, want stable Turn operation op-start", frame["operation"])
			}
		}
	}
	write(map[string]any{"kind": "close"})
	for scanner.Scan() {
		var frame map[string]any
		if json.Unmarshal(scanner.Bytes(), &frame) == nil && frame["kind"] == "event" && frame["event"] == "turn_completed" {
			terminalCount++
		}
	}
	if terminalCount != 1 || !slices.Contains(texts, "configured-image-observed") || !slices.Contains(texts, "causal-developer-observed") {
		t.Fatalf("terminalCount=%d texts=%#v", terminalCount, texts)
	}
	if len(usageFrames) != 2 {
		t.Fatalf("usage frames = %#v", usageFrames)
	}
	firstUsage := usageFrames[0]["usage"].(map[string]any)
	if firstUsage["inputTokens"].(map[string]any)["available"] != false || firstUsage["cachedInputTokens"].(map[string]any)["value"] != float64(0) || firstUsage["totalTokens"].(map[string]any)["available"] != false {
		t.Fatalf("absent versus observed-zero usage = %#v", firstUsage)
	}
	latestUsage, latestDetails := usageFrames[1]["usage"].(map[string]any), usageFrames[1]["details"].(map[string]any)
	models := latestDetails["models"].([]any)
	if latestUsage["inputTokens"].(map[string]any)["value"] != float64(10) || latestUsage["costMicros"].(map[string]any)["value"] != float64(13) || latestUsage["calls"].(map[string]any)["available"] != false || len(models) != 2 {
		t.Fatalf("latest cumulative multi-model usage = usage %#v details %#v", latestUsage, latestDetails)
	}
	mainModel, sideModel := models[0].(map[string]any), models[1].(map[string]any)
	if mainModel["model"].(map[string]any)["value"] != "claude-main" || mainModel["provider"].(map[string]any)["value"] != "firstParty" || mainModel["contextWindow"].(map[string]any)["value"] != float64(200000) || sideModel["model"].(map[string]any)["value"] != "claude-side" || sideModel["provider"].(map[string]any)["value"] != "gateway" || sideModel["contextWindow"].(map[string]any)["value"] != float64(100000) || latestDetails["observedAt"].(map[string]any)["source"] != "canonical_turn_ledger" {
		t.Fatalf("multi-model attribution = %#v", latestDetails)
	}
}

func TestEmbeddedBridgeResolvesApprovalAndReleasesNeedsYou(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	app := t.TempDir()
	module := filepath.Join(app, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(module, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "bridge.mjs"), currentAssets().Bridge, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "package.json"), []byte(`{"type":"module","exports":"./index.js","version":"0.3.228","claudeCodeVersion":"2.1.228"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := `
export async function getSessionInfo() { return undefined; }
export function query({prompt, options}) {
  if (JSON.stringify(options.settingSources) !== JSON.stringify(["project","local"])) throw new Error("settingSources not explicit");
  let interrupted = false;
  return {
    async accountInfo() { return {apiProvider:"firstParty",apiKeySource:"project"}; },
    async streamInput() {}, async interrupt() { interrupted = true; }, close() {},
    async *[Symbol.asyncIterator]() {
      await prompt[Symbol.asyncIterator]().next();
      yield {type:"system", subtype:"init", session_id:options.sessionId};
      const approval = await options.canUseTool("Bash", {command:"printf original"}, {signal:new AbortController().signal, requestId:"callback-approval", toolUseID:"tool-approval"});
      yield {type:"assistant", uuid:"approval", session_id:options.sessionId, message:{content:[{type:"text",text:approval.updatedInput?.command || approval.behavior}]}};
      await options.canUseTool("AskUserQuestion", {questions:[{question:"Ship now?",options:[{label:"Yes",description:"Ship"},{label:"No",description:"Wait"}]}]}, {signal:new AbortController().signal, requestId:"callback-question", toolUseID:"tool-question"});
      yield {type:"result", subtype:"error_during_execution", terminal_reason:interrupted?"aborted_tools":"other", session_id:options.sessionId, usage:{input_tokens:1,output_tokens:1},total_cost_usd:0};
    }
  };
}`
	if err := os.WriteFile(filepath.Join(module, "index.js"), []byte(fake), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, filepath.Join(app, "bridge.mjs"))
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	scanner := bufio.NewScanner(stdout)
	read := func() map[string]any {
		t.Helper()
		if !scanner.Scan() {
			t.Fatalf("bridge output ended: %v", scanner.Err())
		}
		var frame map[string]any
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			t.Fatalf("frame=%s", scanner.Bytes())
		}
		return frame
	}
	write := func(frame any) { encoded, _ := json.Marshal(frame); _, _ = stdin.Write(append(encoded, '\n')) }
	_ = read()
	write(map[string]any{"kind": "initialize", "requestId": "init", "agentId": "agent"})
	_ = read()
	session := "11111111-1111-4111-8111-111111111111"
	configuration := map[string]any{"settingSources": []string{"project", "local"}, "authentication": map[string]any{"category": "console", "source": "api_key"}}
	write(map[string]any{"kind": "command", "command": "start_turn", "requestId": "start", "turnId": "turn", "operation": "op-start", "payload": map[string]any{"sessionRef": session, "cwd": app, "configuration": configuration, "input": []any{map[string]any{"kind": "text", "role": "user", "text": "work"}}}})
	for {
		frame := read()
		if frame["event"] == "approval" {
			data := frame["data"].(map[string]any)
			write(map[string]any{"kind": "command", "command": "resolve_approval", "requestId": "resolve", "turnId": "turn", "operation": "op-approval", "payload": map[string]any{"callbackId": data["callbackId"], "decision": "approve"}})
			break
		}
	}
	sawOriginal := false
	for {
		frame := read()
		if frame["event"] == "content" {
			content := frame["data"].(map[string]any)["content"].(map[string]any)
			sawOriginal = sawOriginal || content["text"] == "printf original"
		}
		if frame["event"] == "needs_you" {
			data := frame["data"].(map[string]any)
			write(map[string]any{"kind": "command", "command": "resolve_needs_you", "requestId": "needs", "turnId": "turn", "operation": "op-needs", "payload": map[string]any{"callbackId": data["callbackId"], "persisted": true}})
			break
		}
	}
	sawInterrupted := false
	for !sawInterrupted {
		frame := read()
		sawInterrupted = frame["event"] == "turn_interrupted"
	}
	if !sawOriginal {
		t.Fatal("exact Approval input did not reach the SDK callback")
	}
	write(map[string]any{"kind": "close"})
}

func TestEmbeddedBridgeRecoversAReservedSessionCreatedBeforeInitAck(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	app := t.TempDir()
	module := filepath.Join(app, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(module, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "bridge.mjs"), currentAssets().Bridge, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "package.json"), []byte(`{"type":"module","exports":"./index.js","version":"0.3.228","claudeCodeVersion":"2.1.228"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := `
export async function getSessionInfo(sessionId, options) { return {sessionId, cwd:options.dir}; }
export async function getSessionMessages() { return []; }
export async function listSessions() { return []; }
export function query({prompt, options}) {
  if (!options.resume || options.sessionId) throw new Error("existing reserved session was not resumed");
  return {
    async accountInfo() { return {apiProvider:"firstParty",subscriptionType:"Claude Max"}; },
    async supportedModels() { return [{value:"haiku",resolvedModel:"claude-haiku-4-5-20251001",displayName:"Haiku"}]; },
    async setModel() {}, async applyFlagSettings() {}, close() {},
    async *[Symbol.asyncIterator]() {
      await prompt[Symbol.asyncIterator]().next();
	  yield {type:"user",uuid:"replayed-user",session_id:options.resume,message:{role:"user",content:[{type:"text",text:"previous interrupted input"}]}};
      yield {type:"system",subtype:"init",session_id:options.resume,model:"claude-haiku-4-5-20251001"};
      yield {type:"assistant",uuid:"answer",session_id:options.resume,message:{model:"claude-haiku-4-5-20251001",content:[{type:"text",text:"recovered"}]}};
      yield {type:"result",subtype:"success",session_id:options.resume,usage:{input_tokens:1,output_tokens:1},num_turns:1,total_cost_usd:0};
    }
  };
}`
	if err := os.WriteFile(filepath.Join(module, "index.js"), []byte(fake), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, filepath.Join(app, "bridge.mjs"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	scanner := bufio.NewScanner(stdout)
	read := func() map[string]any {
		t.Helper()
		if !scanner.Scan() {
			t.Fatalf("bridge output ended: %v", scanner.Err())
		}
		var frame map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatalf("bridge frame = %s: %v", scanner.Bytes(), err)
		}
		return frame
	}
	write := func(frame any) {
		encoded, _ := json.Marshal(frame)
		if _, err := stdin.Write(append(encoded, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	_ = read()
	write(map[string]any{"kind": "initialize", "requestId": "init", "agentId": "agent"})
	_ = read()
	session := "11111111-1111-4111-8111-111111111111"
	write(map[string]any{
		"kind": "command", "command": "start_turn", "requestId": "start", "turnId": "turn", "operation": "op-start",
		"payload": map[string]any{
			"sessionRef": session, "cwd": app, "resume": false, "model": "haiku", "thinkingLevel": "default",
			"configuration": map[string]any{"settingSources": []string{"user"}, "authentication": map[string]any{"category": "subscription", "source": "claude_ai"}},
			"input":         []any{map[string]any{"kind": "text", "role": "user", "text": "work"}},
		},
	})
	accepted, completed := false, false
	for !completed {
		frame := read()
		accepted = accepted || frame["kind"] == "response" && frame["requestId"] == "start" && frame["accepted"] == true
		completed = frame["kind"] == "event" && frame["event"] == "turn_completed"
	}
	if !accepted {
		t.Fatal("recovered reserved session did not accept the Turn")
	}
}

func TestEmbeddedBridgeLetsTheSDKResolveTheDefaultModel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	app := t.TempDir()
	module := filepath.Join(app, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(module, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "bridge.mjs"), currentAssets().Bridge, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "package.json"), []byte(`{"type":"module","exports":"./index.js","version":"0.3.228","claudeCodeVersion":"2.1.228"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := `
export async function getSessionInfo() { return undefined; }
export async function getSessionMessages() { return []; }
export async function listSessions() { return []; }
export function query({prompt, options}) {
  return {
    async accountInfo() { return {apiProvider:"firstParty",subscriptionType:"Claude Max"}; },
    async supportedModels() { return [{value:"default",resolvedModel:"catalog-snapshot",displayName:"Default"}]; },
    async setModel(model) { if (model !== undefined) throw new Error("default model was pinned"); },
    async applyFlagSettings() {}, close() {},
    async *[Symbol.asyncIterator]() {
      await prompt[Symbol.asyncIterator]().next();
	  yield {type:"user",session_id:options.sessionId,message:{role:"user",content:[{type:"text",text:"work"}]}};
      yield {type:"system",subtype:"init",session_id:options.sessionId,model:"subscription-routed-model"};
      yield {type:"assistant",uuid:"answer",session_id:options.sessionId,message:{model:"subscription-routed-model",content:[{type:"text",text:"done"}]}};
      yield {type:"result",subtype:"success",session_id:options.sessionId,usage:{input_tokens:1,output_tokens:1},num_turns:1,total_cost_usd:0};
    }
  };
}`
	if err := os.WriteFile(filepath.Join(module, "index.js"), []byte(fake), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, filepath.Join(app, "bridge.mjs"))
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	scanner := bufio.NewScanner(stdout)
	read := func() map[string]any {
		t.Helper()
		if !scanner.Scan() {
			t.Fatalf("bridge output ended: %v", scanner.Err())
		}
		var frame map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatal(err)
		}
		return frame
	}
	write := func(frame any) { encoded, _ := json.Marshal(frame); _, _ = stdin.Write(append(encoded, '\n')) }
	_ = read()
	write(map[string]any{"kind": "initialize", "requestId": "init", "agentId": "agent"})
	_ = read()
	write(map[string]any{
		"kind": "command", "command": "start_turn", "requestId": "start", "turnId": "turn", "operation": "op-start",
		"payload": map[string]any{
			"sessionRef": "11111111-1111-4111-8111-111111111111", "cwd": app, "model": "default", "thinkingLevel": "default",
			"configuration": map[string]any{"settingSources": []string{"user"}, "authentication": map[string]any{"category": "subscription", "source": "claude_ai"}},
			"input":         []any{map[string]any{"kind": "text", "role": "user", "text": "work"}},
		},
	})
	accepted, completed := false, false
	for !completed {
		frame := read()
		accepted = accepted || frame["kind"] == "response" && frame["accepted"] == true
		completed = frame["kind"] == "event" && frame["event"] == "turn_completed"
	}
	if !accepted {
		t.Fatal("SDK-routed default model Turn was not accepted")
	}
}

func TestEmbeddedBridgeCertificationStreamsBeforeInterruptAndClassifiesTerminal(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	app := t.TempDir()
	module := filepath.Join(app, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(module, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "bridge.mjs"), currentAssets().Bridge, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "package.json"), []byte(`{"type":"module","exports":"./index.js","version":"0.3.228","claudeCodeVersion":"2.1.228"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := `
export async function getSessionInfo() { return undefined; }
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
let interruptRequested = false;
export function query({prompt, options}) {
  if (JSON.stringify(options.settingSources) !== JSON.stringify(["project","local"])) throw new Error("settingSources not explicit");
  if (!options.includePartialMessages || JSON.stringify(options.tools) !== "[]" || JSON.stringify(options.allowedTools) !== "[]" || options.maxTurns !== 1 || options.maxBudgetUsd !== 0.05) throw new Error("certification policy not enforced");
  return {
    async accountInfo() { return {apiProvider:"firstParty",apiKeySource:"project"}; },
    async streamInput() {},
    async interrupt() { interruptRequested = true; await delay(80); },
    close() {},
    async *[Symbol.asyncIterator]() {
      await prompt[Symbol.asyncIterator]().next();
      yield {type:"system", subtype:"init", session_id:options.sessionId};
      yield {type:"stream_event", uuid:"stream", session_id:options.sessionId, parent_tool_use_id:null, event:{type:"content_block_delta",index:0,delta:{type:"text_delta",text:"streamed"}}};
      while (!interruptRequested) await delay(2);
      yield {type:"result", subtype:"error_during_execution", terminal_reason:"aborted_streaming", session_id:options.sessionId, usage:{},num_turns:1,total_cost_usd:0};
    }
  };
}`
	if err := os.WriteFile(filepath.Join(module, "index.js"), []byte(fake), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, filepath.Join(app, "bridge.mjs"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	scanner := bufio.NewScanner(stdout)
	read := func() map[string]any {
		t.Helper()
		if !scanner.Scan() {
			t.Fatalf("bridge output ended: %v", scanner.Err())
		}
		var frame map[string]any
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			t.Fatalf("frame = %s", scanner.Bytes())
		}
		return frame
	}
	write := func(frame any) {
		t.Helper()
		encoded, _ := json.Marshal(frame)
		if _, err := stdin.Write(append(encoded, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	_ = read()
	write(map[string]any{"kind": "initialize", "requestId": "init", "agentId": "agent"})
	_ = read()
	session := "11111111-1111-4111-8111-111111111111"
	configuration := map[string]any{"settingSources": []string{"project", "local"}, "authentication": map[string]any{"category": "console", "source": "api_key"}}
	policy := map[string]any{"purpose": "real_smoke", "allowedTools": []string{}, "maxTurns": 1, "maxBudgetUsd": 0.05}
	write(map[string]any{"kind": "command", "command": "start_turn", "requestId": "start", "turnId": "turn", "operation": "op-start", "payload": map[string]any{"sessionRef": session, "cwd": app, "configuration": configuration, "certificationPolicy": policy, "input": []any{map[string]any{"kind": "text", "role": "user", "text": "work"}}}})
	var runtimeTurnRef any
	sawAssistantDelta := false
	for runtimeTurnRef == nil || !sawAssistantDelta {
		frame := read()
		if frame["kind"] == "response" && frame["operation"] == "op-start" {
			runtimeTurnRef = frame["data"].(map[string]any)["runtimeTurnRef"]
		}
		if frame["kind"] == "event" && frame["event"] == "content" {
			data := frame["data"].(map[string]any)
			content := data["content"].(map[string]any)
			sawAssistantDelta = data["phase"] == "delta" && content["kind"] == "assistant_text" && content["text"] == "streamed"
		}
	}
	write(map[string]any{"kind": "command", "command": "interrupt_turn", "requestId": "interrupt", "turnId": "turn", "operation": "op-interrupt", "payload": map[string]any{"runtimeTurnRef": runtimeTurnRef}})
	terminalBeforeReceipt, receipt := false, false
	for !receipt {
		frame := read()
		if frame["kind"] == "event" && frame["event"] == "turn_interrupted" {
			terminalBeforeReceipt = true
		}
		if frame["kind"] == "event" && frame["event"] == "interrupt_receipt" {
			receipt = true
		}
	}
	if !terminalBeforeReceipt {
		t.Fatal("terminal observed before interrupt Promise resolved was not classified as interrupted")
	}
}

func TestOwnerStagesActivatesAndRollsBackVerifiedGenerations(t *testing.T) {
	archive := fakeNodeArchive(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	first := testManifest("claude-test-a", server.URL, archive)
	manager := New(Options{Root: root, Manifest: first, Platform: testPlatform()})

	status := manager.Status(context.Background())
	if status.State != StateInstallRequired || status.Required.ID != first.ID || status.Active != nil {
		t.Fatalf("initial status = %#v", status)
	}
	if _, err := manager.Install(context.Background(), InstallRequest{}); err == nil {
		t.Fatal("install without terms acknowledgement succeeded")
	}
	staged, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true})
	if err != nil {
		t.Fatal(err)
	}
	if staged.State != StateStaged || staged.Staged == nil || staged.Staged.ID != first.ID || staged.Active != nil {
		t.Fatalf("staged status = %#v", staged)
	}
	if _, err := manager.Verify(context.Background(), "stgaed"); err == nil || !strings.Contains(PublicMessage(err), "active or staged") {
		t.Fatalf("invalid verification target = %v", err)
	}
	active, err := manager.Activate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if active.State != StateActive || active.Active == nil || active.Active.ID != first.ID || active.Previous != nil {
		t.Fatalf("active status = %#v", active)
	}
	if err := manager.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	launch, err := manager.ResolveActive(context.Background())
	if err != nil {
		t.Fatalf("ResolveActive: %v", err)
	}
	if launch.Manifest.ID != first.ID || filepath.Base(launch.NodePath) != "node" || filepath.Base(launch.BridgePath) != "bridge.mjs" || strings.Contains(launch.NodePath, "/usr/") {
		t.Fatalf("active launch spec = %#v", launch)
	}

	second := testManifest("claude-test-b", server.URL, archive)
	manager = New(Options{Root: root, Manifest: second, Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{}); err != nil {
		t.Fatalf("accepted terms were not reused: %v", err)
	}
	if _, err := manager.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := manager.Rollback(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Active == nil || rolledBack.Active.ID != first.ID || rolledBack.Previous == nil || rolledBack.Previous.ID != second.ID {
		t.Fatalf("rollback status = %#v", rolledBack)
	}
	entries, err := os.ReadDir(filepath.Join(root, "generations"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("generation count = %d, want active + previous", len(entries))
	}
}

func TestActiveGenerationIsImmutableAndPreflightChecksBridgeIntegrity(t *testing.T) {
	archive := fakeNodeArchive(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	manifest := testManifest("claude-test-immutable", server.URL, archive)
	manager := New(Options{Root: root, Manifest: manifest, Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	bridge := filepath.Join(root, "generations", manifest.ID, "app", "bridge.mjs")
	if err := os.Chmod(bridge, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bridge, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Preflight(context.Background()); err == nil {
		t.Fatal("Preflight accepted a damaged bridge")
	}
	if status := manager.Status(context.Background()); status.State != StateBroken {
		t.Fatalf("damaged active status = %#v", status)
	}
	if _, err := manager.Install(context.Background(), InstallRequest{}); err == nil {
		t.Fatal("install replaced an existing immutable generation")
	}
	data, err := os.ReadFile(bridge)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "damaged" {
		t.Fatalf("existing generation was overwritten: %q", data)
	}
}

func TestDamagedPreviousGenerationKeepsActiveAvailableAndExplainsRollback(t *testing.T) {
	archive := fakeNodeArchive(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	root := t.TempDir()
	first := testManifest("claude-previous-a", server.URL, archive)
	manager := New(Options{Root: root, Manifest: first, Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := testManifest("claude-previous-b", server.URL, archive)
	manager = New(Options{Root: root, Manifest: second, Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	removeGeneration(filepath.Join(root, "generations", first.ID))
	status := manager.Status(context.Background())
	if status.State != StateActive || status.Active == nil || status.Previous != nil || !strings.Contains(status.Reason, "rollback is unavailable") {
		t.Fatalf("status = %#v", status)
	}
}

func TestVerifiedOrphanGenerationCanBeRestagedWithoutRedownload(t *testing.T) {
	archive := fakeNodeArchive(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	manifest := testManifest("claude-orphan", server.URL, archive)
	manager := New(Options{Root: root, Manifest: manifest, Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "state.json")); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateStaged || status.Staged == nil || requests != 1 {
		t.Fatalf("restaged status=%#v requests=%d", status, requests)
	}
}

func TestExistingGenerationPersistsNewTermsAcknowledgement(t *testing.T) {
	archive := fakeNodeArchive(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	root := t.TempDir()
	manifest := testManifest("claude-terms", server.URL, archive)
	manager := New(Options{Root: root, Manifest: manifest, Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err != nil {
		t.Fatal(err)
	}
	manifest.TermsRevision = "new-terms"
	manager = New(Options{Root: root, Manifest: manifest, Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err != nil {
		t.Fatal(err)
	}
	reopened := New(Options{Root: root, Manifest: manifest, Platform: testPlatform()})
	if status := reopened.Status(context.Background()); !status.TermsAccepted {
		t.Fatalf("new terms acknowledgement was not durable: %#v", status)
	}
}

func TestStatusReportsUnreadableStateAsBroken(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := New(Options{Root: root, Manifest: CurrentManifest(), Platform: testPlatform()})
	status := manager.Status(context.Background())
	if status.State != StateBroken || !strings.Contains(status.Reason, "unreadable") {
		t.Fatalf("status = %#v", status)
	}
}

func TestCurrentCompatibilityRowAndSupportedPlatformsAreExact(t *testing.T) {
	manifest := CurrentManifest()
	if manifest.ID != "claude-runtime-v14-node24.19.0-sdk0.3.228" ||
		manifest.NodeVersion != "24.19.0" || manifest.SDKVersion != "0.3.228" || manifest.ClaudeCodeVersion != "2.1.228" || len(manifest.Platforms) != 4 {
		t.Fatalf("current manifest = %#v", manifest)
	}
	if manifest.BridgeSHA256 == "" || manifest.PackageLockSHA256 == "" || manifest.SDKIntegrity == "" {
		t.Fatalf("current manifest lacks integrity: %#v", manifest)
	}

	tests := []struct {
		name                        string
		platform                    Platform
		wantSupported               bool
		wantReason, wantAlternative string
	}{
		{"macOS arm64", ClassifyPlatform("darwin", "arm64", "macos", "14.0", ""), true, "", ""},
		{"Ubuntu x64", ClassifyPlatform("linux", "amd64", "ubuntu", "22.04", "glibc"), true, "", ""},
		{"musl", ClassifyPlatform("linux", "amd64", "alpine", "3.20", "musl"), false, "musl", "Ubuntu"},
		{"Windows", ClassifyPlatform("windows", "amd64", "windows", "11", ""), false, "Windows", "Ubuntu"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.platform.Supported != tt.wantSupported || !strings.Contains(tt.platform.Reason, tt.wantReason) || !strings.Contains(tt.platform.Alternative, tt.wantAlternative) {
				t.Fatalf("platform = %#v", tt.platform)
			}
		})
	}
}

func TestCompatibilityVerificationRejectsVersionAndCapabilityMismatch(t *testing.T) {
	manifest := testManifest("claude-compatibility", "https://example.invalid", []byte("archive"))
	root := t.TempDir()
	node := filepath.Join(root, "node")
	bridge := filepath.Join(root, "bridge.mjs")
	if err := os.WriteFile(bridge, []byte("// test bridge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := `{"protocolVersion":1,"bridgeBuild":"test-bridge","nodeVersion":"24.19.0","sdkVersion":"0.3.228","claudeCodeVersion":"2.1.228","capabilities":["interrupt"]}`
	for name, output := range map[string]string{
		"protocol version": strings.Replace(valid, `"protocolVersion":1`, `"protocolVersion":2`, 1),
		"bridge version":   strings.Replace(valid, `"bridgeBuild":"test-bridge"`, `"bridgeBuild":"wrong"`, 1),
		"node version":     strings.Replace(valid, `"nodeVersion":"24.19.0"`, `"nodeVersion":"24.18.0"`, 1),
		"sdk version":      strings.Replace(valid, `"sdkVersion":"0.3.228"`, `"sdkVersion":"0.3.227"`, 1),
		"Claude Code":      strings.Replace(valid, `"claudeCodeVersion":"2.1.228"`, `"claudeCodeVersion":"2.1.227"`, 1),
		"capabilities":     strings.Replace(valid, `["interrupt"]`, `["interrupt","extra"]`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			script := "#!/bin/sh\nprintf '%s\\n' '" + output + "'\n"
			if err := os.WriteFile(node, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := runSelfTest(context.Background(), node, bridge, manifest); err == nil {
				t.Fatal("mismatched self-test succeeded")
			}
		})
	}

	app := filepath.Join(root, "app")
	artifact := manifest.Platforms[0]
	for path, version := range map[string]string{
		filepath.Join(app, "node_modules", "@anthropic-ai", "claude-agent-sdk", "package.json"):      manifest.SDKVersion,
		filepath.Join(app, "node_modules", filepath.FromSlash(artifact.PackageName), "package.json"): artifact.PackageVersion,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"version":%q}`, version)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, path := range map[string]string{
		"SDK package":      filepath.Join(app, "node_modules", "@anthropic-ai", "claude-agent-sdk", "package.json"),
		"platform package": filepath.Join(app, "node_modules", filepath.FromSlash(artifact.PackageName), "package.json"),
	} {
		t.Run(name, func(t *testing.T) {
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"version":"0.3.227"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyPackageVersions(app, manifest, artifact); err == nil {
				t.Fatal("mismatched installed package version succeeded")
			}
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInstallEnvironmentUsesOnlyPinnedNodePath(t *testing.T) {
	t.Setenv("PATH", "/system/bin:/other/bin")
	environment := installEnvironment("/pinned/node/bin", "/cache")
	if environment[0] != "PATH=/pinned/node/bin" {
		t.Fatalf("environment PATH = %q", environment[0])
	}
}

func TestActivationIsQuiescedAndConcurrentMutationIsRejected(t *testing.T) {
	archive := fakeNodeArchive(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	root := t.TempDir()
	manager := New(Options{Root: root, Manifest: testManifest("claude-quiesced", server.URL, archive), Platform: testPlatform()})
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	manager.quiesce = func(context.Context) error {
		close(started)
		<-release
		return nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := manager.Activate(context.Background())
		result <- err
	}()
	<-started
	if status := manager.Status(context.Background()); status.State != StateStaged {
		t.Fatalf("status blocked or changed during quiesce: %#v", status)
	}
	if _, err := manager.Activate(context.Background()); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("concurrent activation = %v", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedAndFailedInstallHaveNoDurableGeneration(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte("not the pinned archive"))
	}))
	t.Cleanup(server.Close)
	root := filepath.Join(t.TempDir(), "runtime")
	manifest := testManifest("claude-failure", server.URL, []byte("expected"))
	unsupported := New(Options{Root: root, Manifest: manifest, Platform: Platform{OS: "windows", Arch: "x64", Reason: "unsupported"}})
	if _, err := unsupported.Install(context.Background(), InstallRequest{AcceptTerms: true}); err == nil || requests != 0 {
		t.Fatalf("unsupported install err=%v requests=%d", err, requests)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("unsupported install mutated root: %v", err)
	}
	failed := New(Options{Root: root, Manifest: manifest, Platform: testPlatform()})
	if _, err := failed.Install(context.Background(), InstallRequest{AcceptTerms: true}); err == nil {
		t.Fatal("integrity failure succeeded")
	}
	if status := failed.Status(context.Background()); status.Staged != nil || status.Active != nil {
		t.Fatalf("failed install became durable: %#v", status)
	}
}

func TestManifestPackageIntegrityMismatchFailsBeforeDownload(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write(fakeNodeArchive(t))
	}))
	t.Cleanup(server.Close)
	manifest := CurrentManifest()
	manifest.Platforms[0].NodeURL = server.URL
	manifest.Platforms[0].PackageIntegrity = "sha512-wrong"
	manager := New(Options{Root: t.TempDir(), Manifest: manifest, Platform: Platform{OS: "darwin", Arch: "arm64", Supported: true}})
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err == nil || requests != 0 {
		t.Fatalf("integrity mismatch err=%v requests=%d", err, requests)
	}
}

func TestPreflightReportsExactRowOrExplicitAvailability(t *testing.T) {
	missing := New(Options{Root: t.TempDir(), Manifest: testManifest("claude-preflight", "https://example.invalid", []byte("archive")), Platform: testPlatform()})
	if report := missing.InspectPreflight(context.Background()); report.Availability != "unavailable" || report.Required.ID != "claude-preflight" || report.Alternative == "" {
		t.Fatalf("missing preflight = %#v", report)
	}
	unsupported := New(Options{Root: t.TempDir(), Manifest: CurrentManifest(), Platform: Platform{OS: "windows", Arch: "x64", Reason: "Windows unsupported", Alternative: "Use Ubuntu"}})
	if report := unsupported.InspectPreflight(context.Background()); report.Availability != "unsupported" || !strings.Contains(report.Reason, "Windows") || report.Alternative == "" {
		t.Fatalf("unsupported preflight = %#v", report)
	}
}

func TestCanceledInstallLeavesNoGenerationAndManagerCanRetry(t *testing.T) {
	archive := fakeNodeArchive(t)
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-release:
			_, _ = w.Write(archive)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	manager := New(Options{Root: root, Manifest: testManifest("claude-cancel", server.URL, archive), Platform: testPlatform()})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := manager.Install(ctx, InstallRequest{AcceptTerms: true})
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; err == nil {
		t.Fatal("canceled install succeeded")
	}
	entries, err := os.ReadDir(filepath.Join(root, "generations"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 || manager.Status(context.Background()).Staged != nil {
		t.Fatalf("canceled install left durable generation: %#v", entries)
	}
	close(release)
	if _, err := manager.Install(context.Background(), InstallRequest{AcceptTerms: true}); err != nil {
		t.Fatalf("retry after cancellation: %v", err)
	}
}

func testManifest(id, url string, archive []byte) Manifest {
	digest := sha256.Sum256(archive)
	bridgeDigest := sha256.Sum256(currentAssets().Bridge)
	lockDigest := sha256.Sum256(currentAssets().PackageLock)
	return Manifest{
		ID: id, Compatibility: "claude-runtime-v1", BridgeProtocol: 1, BridgeBuild: "test-bridge",
		BridgeSHA256: fmt.Sprintf("%x", bridgeDigest), NodeVersion: "24.19.0", SDKVersion: "0.3.228", ClaudeCodeVersion: "2.1.228", PackageLockSHA256: fmt.Sprintf("%x", lockDigest),
		TermsRevision: "2026-08-12", TermsURL: "https://example.test/terms",
		RequiredCapabilities: []string{"interrupt"},
		Platforms:            []PlatformArtifact{{OS: "linux", Arch: "x64", NodeURL: url, NodeSHA256: fmt.Sprintf("%x", digest), PackageName: "@anthropic-ai/claude-agent-sdk-linux-x64", PackageVersion: "0.3.228"}},
	}
}

func testPlatform() Platform {
	return Platform{OS: "linux", Arch: "x64", Distribution: "ubuntu", Version: "22.04", Libc: "glibc", Supported: true}
}

func fakeNodeArchive(t *testing.T) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	files := map[string]struct {
		mode int64
		body string
	}{
		"node-test/bin/node": {0o755, `#!/bin/sh
case "$1" in
*npm-cli.js*)
  /bin/mkdir -p node_modules/@anthropic-ai/claude-agent-sdk node_modules/@anthropic-ai/claude-agent-sdk-linux-x64
  printf '{"version":"0.3.228","claudeCodeVersion":"2.1.228"}' > node_modules/@anthropic-ai/claude-agent-sdk/package.json
  printf '{"version":"0.3.228"}' > node_modules/@anthropic-ai/claude-agent-sdk-linux-x64/package.json
  exit 0
  ;;
esac
printf '{"protocolVersion":1,"bridgeBuild":"test-bridge","nodeVersion":"24.19.0","sdkVersion":"0.3.228","claudeCodeVersion":"2.1.228","capabilities":["interrupt"]}\n'
`},
		"node-test/lib/node_modules/npm/bin/npm-cli.js": {0o644, "// test npm cli\n"},
	}
	for name, file := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: file.mode, Size: int64(len(file.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}
