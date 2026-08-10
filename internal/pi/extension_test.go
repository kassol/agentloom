package pi

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeLoomExtensionCreatesOnePrivateCollaborationExtension(t *testing.T) {
	dataDir := t.TempDir()
	path, err := MaterializeLoomExtension(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dataDir, "pi", "runtime", "loom-extension.ts") {
		t.Fatalf("extension path = %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("extension mode = %o", info.Mode().Perm())
	}
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, name := range []string{
		"loom_agents_find", "loom_message_send", "loom_message_receive", "loom_message_reply",
		"loom_topic_list", "loom_topic_get", "loom_topic_create", "loom_topic_participant_upsert",
		"loom_topic_participant_remove", "loom_topic_wait", "loom_topic_resume",
		"loom_topic_publish_progress", "loom_topic_publish_result",
		"loom_needs_you",
	} {
		if strings.Count(text, `name: "`+name+`"`) != 1 {
			t.Fatalf("tool %s is not registered exactly once", name)
		}
	}
	for _, forbidden := range []string{"loom_topic_resolve", "loom_approval", "--no-skills", "--no-extensions"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("extension contains out-of-scope capability %q", forbidden)
		}
	}
	for _, want := range []string{
		`request("/api/human-requests"`,
		`agent: agentID`,
		`terminate: true`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("Needs You extension missing %q", want)
		}
	}
}

func TestLoomExtensionGatesMutatingToolCallsOnlyForOnRequestPolicy(t *testing.T) {
	approved := runExtensionToolCall(t, "on-request", "bash", "approve", false)
	if approved.Blocked || len(approved.UIRequests) != 1 {
		t.Fatalf("approved bash = %#v", approved)
	}
	request := approved.UIRequests[0]
	if request.Title != "codex-loom:approval:v1" || request.Timeout <= 0 {
		t.Fatalf("Approval UI request = %#v", request)
	}
	var payload struct {
		Version    int            `json:"version"`
		Operation  string         `json:"operation"`
		ToolCallID string         `json:"toolCallId"`
		ToolName   string         `json:"toolName"`
		Input      map[string]any `json:"input"`
	}
	if err := json.Unmarshal([]byte(request.Placeholder), &payload); err != nil {
		t.Fatalf("Approval payload = %q: %v", request.Placeholder, err)
	}
	if payload.Version != 1 || payload.Operation != "request_approval" || payload.ToolCallID != "call-test" || payload.ToolName != "bash" || payload.Input["command"] != "pwd" {
		t.Fatalf("Approval payload = %#v", payload)
	}
	for _, test := range []struct {
		policy string
		tool   string
	}{
		{policy: "on-request", tool: "read"},
		{policy: "never", tool: "bash"},
	} {
		got := runExtensionToolCall(t, test.policy, test.tool, "approve", false)
		if got.Blocked || len(got.UIRequests) != 0 {
			t.Fatalf("%s %s = %#v", test.policy, test.tool, got)
		}
	}
}

func TestLoomExtensionFailsClosedForDeniedTimedOutAbortedAndUnknownTools(t *testing.T) {
	for _, test := range []struct {
		name      string
		tool      string
		uiResult  string
		aborted   bool
		reason    string
		wantUI    bool
		wantBlock bool
	}{
		{name: "denied", tool: "write", uiResult: "deny", reason: "denied", wantUI: true, wantBlock: true},
		{name: "timed out", tool: "edit", uiResult: "timeout", reason: "timed out", wantUI: true, wantBlock: true},
		{name: "cancelled timeout", tool: "bash", uiResult: "undefined", reason: "timed out or was cancelled", wantUI: true, wantBlock: true},
		{name: "aborted", tool: "bash", uiResult: "undefined", aborted: true, reason: "aborted", wantUI: true, wantBlock: true},
		{name: "unknown custom", tool: "owner_custom_tool", uiResult: "deny", reason: "denied", wantUI: true, wantBlock: true},
		{name: "unowned Loom prefix", tool: "loom_unowned_custom", uiResult: "deny", reason: "denied", wantUI: true, wantBlock: true},
		{name: "Loom control plane", tool: "loom_message_send", uiResult: "deny", wantUI: false, wantBlock: false},
		{name: "read-only grep", tool: "grep", uiResult: "deny", wantUI: false, wantBlock: false},
		{name: "read-only find", tool: "find", uiResult: "deny", wantUI: false, wantBlock: false},
		{name: "read-only ls", tool: "ls", uiResult: "deny", wantUI: false, wantBlock: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := runExtensionToolCall(t, "on-request", test.tool, test.uiResult, test.aborted)
			if got.Blocked != test.wantBlock || (len(got.UIRequests) != 0) != test.wantUI || !strings.Contains(got.Reason, test.reason) {
				t.Fatalf("tool_call result = %#v", got)
			}
		})
	}
}

type extensionToolCallResult struct {
	Blocked    bool   `json:"blocked"`
	Reason     string `json:"reason"`
	UIRequests []struct {
		Title       string `json:"title"`
		Placeholder string `json:"placeholder"`
		Timeout     int    `json:"timeout"`
	} `json:"uiRequests"`
}

func runExtensionToolCall(t *testing.T, policy, toolName, uiResult string, aborted bool) extensionToolCallResult {
	t.Helper()
	piBin, err := ResolveBin(os.Getenv("PI_REAL_BIN"))
	if err != nil {
		t.Skipf("Pi Extension contract requires a local Pi %s+: %v", MinimumVersion, err)
	}
	packageRoot := piPackageRoot(t, piBin)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the Pi Extension contract")
	}
	dataDir := t.TempDir()
	extensionPath, err := MaterializeLoomExtension(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	const harness = `
import { pathToFileURL } from "node:url";
const [loaderPath, extensionPath, toolName, uiResult, aborted] = process.argv.slice(1);
const { loadExtensions } = await import(pathToFileURL(loaderPath).href);
const loaded = await loadExtensions([extensionPath], process.cwd());
if (loaded.errors.length) throw new Error(JSON.stringify(loaded.errors));
const handlers = loaded.extensions[0].handlers.get("tool_call") ?? [];
if (handlers.length !== 1) throw new Error("expected exactly one tool_call handler");
const uiRequests = [];
const signal = aborted === "true" ? AbortSignal.abort() : new AbortController().signal;
const ctx = { signal, ui: { input: async (title, placeholder, options) => {
  uiRequests.push({ title, placeholder, timeout: options?.timeout ?? 0 });
  return uiResult === "undefined" ? undefined : uiResult;
} } };
const outcome = await handlers[0]({ type: "tool_call", toolCallId: "call-test", toolName, input: { command: "pwd" } }, ctx);
console.log(JSON.stringify({ blocked: outcome?.block === true, reason: outcome?.reason ?? "", uiRequests }));
`
	loaderPath := filepath.Join(packageRoot, "dist", "core", "extensions", "index.js")
	command := exec.Command(node, "--input-type=module", "-e", harness, loaderPath, extensionPath, toolName, uiResult, jsonBool(aborted))
	command.Env = append(os.Environ(), "CODEX_LOOM_AGENT_ID=agent-contract", "CODEX_LOOM_API_URL=http://127.0.0.1:1", "CODEX_LOOM_APPROVAL_POLICY="+policy)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Pi Extension contract: %v\n%s", err, output)
	}
	var result extensionToolCallResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("Pi Extension contract output = %q: %v", output, err)
	}
	return result
}

func piPackageRoot(t *testing.T, piBin string) string {
	t.Helper()
	if root := strings.TrimSpace(os.Getenv("PI_PACKAGE_ROOT")); root != "" {
		return root
	}
	data, err := os.ReadFile(piBin)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			const marker = "# cmd-shim-target="
			if target, ok := strings.CutPrefix(line, marker); ok {
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(piBin), target)
				}
				return filepath.Dir(filepath.Dir(filepath.Clean(target)))
			}
		}
	}
	resolved, err := filepath.EvalSymlinks(piBin)
	if err == nil && filepath.Base(resolved) == "cli.js" {
		return filepath.Dir(filepath.Dir(resolved))
	}
	t.Skip("set PI_PACKAGE_ROOT to the installed @earendil-works/pi-coding-agent package")
	return ""
}

func jsonBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
