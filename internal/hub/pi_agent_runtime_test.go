package hub

import (
	"bufio"
	"bytes"
	"encoding/base64"
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

func TestPiAgentCompletesLiveLoomTurnOnlyAfterAgentSettled(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "happy")
	h := testHub(st)
	h.stop = make(chan struct{})

	agent, err := h.CreateAgent(CreateParams{Name: "pi-worker", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	if agent.RuntimeBinding.Kind != "pi" || agent.RuntimeBinding.NativeRef != "" || agent.RuntimeTurnBindings != nil {
		t.Fatalf("public Agent Runtime = %#v, turn bindings=%#v", agent.RuntimeBinding, agent.RuntimeTurnBindings)
	}
	if native := h.agents[agent.ID].RuntimeBinding.NativeRef; !strings.HasPrefix(native, filepath.Join(st.Dir(), "pi", agent.ID)+string(filepath.Separator)) {
		t.Fatalf("Pi session = %q, want under per-Agent Loom data directory", native)
	}

	result, err := h.SendTask(agent.ID, "hello Pi", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.TurnID == "" || !strings.HasPrefix(result.TurnID, "turn_") {
		t.Fatalf("Loom Turn result = %#v", result)
	}
	waitForPiFile(t, os.Getenv("FAKE_PI_AGENT_END_FILE"))
	view, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "running" || view.CurrentTurnID != result.TurnID || view.LastTurn != nil {
		t.Fatalf("Agent after agent_end = status %q current %q last %#v", view.Status, view.CurrentTurnID, view.LastTurn)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, _ = h.GetAgent(agent.ID)
		if view.LastTurn != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if view.Status != "idle" || view.LastTurn == nil || view.LastTurn.Status != "completed" || view.LastTurn.TurnID != result.TurnID {
		t.Fatalf("settled Agent = %#v", view.Agent)
	}
	events, err := st.ReadEvents(agent.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var answerSeen, completionSeen bool
	var normalized []string
	toolIDs := map[string]bool{}
	for _, event := range events {
		if event.Type != "loom/runtime-event" {
			continue
		}
		var canonical runtimecontract.Event
		if json.Unmarshal(event.Data, &canonical) != nil {
			continue
		}
		switch canonical.Kind {
		case runtimecontract.EventTurnStarted:
			normalized = append(normalized, "turn_started")
		case runtimecontract.EventContent:
			if canonical.Content == nil {
				continue
			}
			normalized = append(normalized, string(canonical.Content.Kind)+":"+string(canonical.ContentPhase))
			if canonical.Content.Kind == runtimecontract.ContentToolCall || canonical.Content.Kind == runtimecontract.ContentToolResult {
				toolIDs[canonical.Content.ID] = true
			}
			if canonical.Content.Kind == runtimecontract.ContentAssistantText && canonical.ContentPhase == runtimecontract.ContentPhaseCompleted {
				answerSeen = strings.Contains(canonical.Content.Text, "hello from Pi")
			}
		case runtimecontract.EventTerminal:
			normalized = append(normalized, "terminal")
			completionSeen = answerSeen && canonical.TurnID == result.TurnID && canonical.Outcome != nil && canonical.Outcome.State == runtimecontract.LifecycleCompleted
		}
	}
	if !answerSeen || !completionSeen {
		t.Fatalf("normalized Pi events = %#v", events)
	}
	wantNormalized := []string{
		"turn_started", "reasoning:delta", "reasoning:completed", "tool_call:started", "tool_call:updated",
		"tool_result:completed", "assistant_text:delta", "assistant_text:completed", "terminal",
	}
	if strings.Join(normalized, ",") != strings.Join(wantNormalized, ",") || len(toolIDs) != 1 {
		t.Fatalf("streamed normalized order=%v toolIDs=%v", normalized, toolIDs)
	}
	for itemID := range toolIDs {
		if !strings.HasPrefix(itemID, "item_") || strings.Contains(itemID, "call-1") {
			t.Fatalf("public tool item ID = %q", itemID)
		}
	}
	prompt, err := os.ReadFile(os.Getenv("FAKE_PI_PROMPT_FILE"))
	if err != nil || !strings.Contains(string(prompt), "hello Pi") || !strings.Contains(string(prompt), "loom_agent_profile") {
		t.Fatalf("Pi prompt context = %q, err=%v", prompt, err)
	}
	starts, err := os.ReadFile(os.Getenv("FAKE_PI_STARTS_FILE"))
	if err != nil || strings.Count(string(starts), "start\n") != 1 {
		t.Fatalf("Pi process starts = %q, err=%v", starts, err)
	}
	h.Shutdown()
}

func TestPiApprovalUsesCanonicalLoomTurnAndExecutesToolOnlyAfterApprove(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "approval")
	h := testHub(st)
	h.stop = make(chan struct{})
	defer h.Shutdown()

	agent, err := h.CreateAgent(CreateParams{
		Name: "pi-approval", Cwd: t.TempDir(), RuntimeKind: "pi", ApprovalPolicy: "on-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.SendTask(agent.ID, "run the gated tool", time.Second)
	if err != nil {
		t.Fatal(err)
	}

	var approval ApprovalView
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		view, getErr := h.GetAgent(agent.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if len(view.PendingApprovals) == 1 {
			approval = view.PendingApprovals[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if approval.ApprovalID == "" {
		t.Fatal("Pi tool did not produce a pending Loom Approval")
	}
	if approval.TurnID != result.TurnID || approval.RuntimeKind != "pi" || approval.Method != "tool/bash" ||
		strings.Contains(string(approval.Params), `"toolCallId"`) ||
		!strings.Contains(string(approval.Params), `"toolName":"bash"`) ||
		!strings.Contains(string(approval.Params), `"command":"touch approval-effect"`) {
		t.Fatalf("Pi Approval = %#v, canonical Turn = %q", approval, result.TurnID)
	}
	if _, err := os.Stat(os.Getenv("FAKE_PI_TOOL_EFFECT_FILE")); !os.IsNotExist(err) {
		t.Fatalf("tool executed before Approval: %v", err)
	}
	if _, err := h.ResolveApproval(agent.ID, approval.ApprovalID, "approve"); err != nil {
		t.Fatal(err)
	}
	waitForPiFile(t, os.Getenv("FAKE_PI_TOOL_EFFECT_FILE"))

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		view, _ := h.GetAgent(agent.ID)
		if view.LastTurn != nil {
			if view.LastTurn.TurnID != result.TurnID || view.LastTurn.Status != "completed" || len(view.PendingApprovals) != 0 {
				t.Fatalf("resolved Pi Turn = %#v, pending=%#v", view.LastTurn, view.PendingApprovals)
			}
			terminal := h.approvals[approval.ApprovalID]
			if terminal == nil || terminal.Status != "approved" || terminal.Decision != "approve" {
				t.Fatalf("durable Approval terminal = %#v", terminal)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("approved Pi Turn did not settle")
}

func TestPiApprovalPolicyNeverProceedsWithoutDurableApproval(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "approval-never")
	h := testHub(st)
	h.stop = make(chan struct{})
	defer h.Shutdown()
	agent, err := h.CreateAgent(CreateParams{Name: "pi-never", Cwd: t.TempDir(), RuntimeKind: "pi", ApprovalPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	if descriptor, ok := capabilityDescriptor(agent.CapabilitySnapshot, runtimecontract.CapabilityApprovalPolicy); !ok || descriptor.Availability != runtimecontract.CapabilityAvailable {
		t.Fatalf("Pi capabilities = %#v", agent.CapabilitySnapshot)
	}
	if _, err := h.SendTask(agent.ID, "run without prompting", time.Second); err != nil {
		t.Fatal(err)
	}
	waitForPiFile(t, os.Getenv("FAKE_PI_TOOL_EFFECT_FILE"))
	h.mu.Lock()
	approvalCount := len(h.approvals)
	h.mu.Unlock()
	if approvalCount != 0 {
		t.Fatalf("policy never created %d durable Approvals", approvalCount)
	}
}

func TestPiApprovalDenyAndTimeoutFailClosedBeforeToolExecution(t *testing.T) {
	for _, test := range []struct {
		name, scenario, decision, status string
	}{
		{name: "deny", scenario: "approval-deny", decision: "deny", status: "denied"},
		{name: "timeout", scenario: "approval-timeout", decision: "", status: "timed_out"},
		{name: "abort", scenario: "approval-abort", decision: "abort", status: "aborted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			configureFakePiHubRPC(t, test.scenario)
			h := testHub(st)
			h.stop = make(chan struct{})
			defer h.Shutdown()
			agent, err := h.CreateAgent(CreateParams{Name: "pi-" + test.name, Cwd: t.TempDir(), RuntimeKind: "pi", ApprovalPolicy: "on-request"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.SendTask(agent.ID, "run gated tool", time.Second); err != nil {
				t.Fatal(err)
			}
			approval := waitForPendingPiApproval(t, h, agent.ID)
			if test.decision != "" {
				if _, err := h.ResolveApproval(agent.ID, approval.ApprovalID, test.decision); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				h.mu.Lock()
				terminal := h.approvals[approval.ApprovalID]
				status := ""
				if terminal != nil {
					status = terminal.Status
				}
				h.mu.Unlock()
				if status == test.status {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			h.mu.Lock()
			terminal := h.approvals[approval.ApprovalID]
			h.mu.Unlock()
			if terminal == nil || terminal.Status != test.status {
				t.Fatalf("durable Approval terminal = %#v, want %s", terminal, test.status)
			}
			failedToolDeadline := time.Now().Add(2 * time.Second)
			failedTool := false
			for time.Now().Before(failedToolDeadline) {
				events, readErr := st.ReadEvents(agent.ID, 0, 100)
				if readErr != nil {
					t.Fatal(readErr)
				}
				for _, event := range events {
					if event.Type != "loom/runtime-event" {
						continue
					}
					var canonical runtimecontract.Event
					if json.Unmarshal(event.Data, &canonical) == nil && canonical.Kind == runtimecontract.EventContent &&
						canonical.Content != nil && canonical.Content.Kind == runtimecontract.ContentToolResult &&
						canonical.Content.ToolResult != nil && !canonical.Content.ToolResult.Success {
						failedTool = true
						break
					}
				}
				if failedTool {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !failedTool {
				t.Fatalf("%s did not produce a truthful failed tool event", test.name)
			}
			if _, err := os.Stat(os.Getenv("FAKE_PI_TOOL_EFFECT_FILE")); !os.IsNotExist(err) {
				t.Fatalf("tool executed after %s: %v", test.name, err)
			}
		})
	}
}

func waitForPendingPiApproval(t *testing.T, h *Hub, agentID string) ApprovalView {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		view, err := h.GetAgent(agentID)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.PendingApprovals) == 1 {
			return view.PendingApprovals[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Pi tool did not produce a pending Loom Approval")
	return ApprovalView{}
}

func TestPiRuntimeExplicitlyLoadsLoomExtensionWithoutDisablingNativeResources(t *testing.T) {
	configureFakePiHubRPC(t, "happy")
	dataDir := t.TempDir()
	runtime := newPiAgentRuntime("agent-pi", dataDir, "http://127.0.0.1:6123")
	defer runtime.Close()
	if _, err := runtime.Create(nativeBindingRequest{Name: "Pi Worker", Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(os.Getenv("FAKE_PI_START_ARGS_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	extensionPath := filepath.Join(dataDir, "pi", "runtime", "loom-extension.ts")
	if strings.Count(string(args), "--extension\t"+extensionPath) != 1 {
		t.Fatalf("Pi args do not explicitly load one Loom Extension: %q", args)
	}
	for _, disabled := range []string{"--no-extensions", "--no-skills", "--no-context-files", "--no-prompt-templates"} {
		if strings.Contains(string(args), disabled) {
			t.Fatalf("Pi args disable inherited resource %q: %q", disabled, args)
		}
	}
	environment, err := os.ReadFile(os.Getenv("FAKE_PI_RUNTIME_ENV_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	if string(environment) != "agent-pi\nhttp://127.0.0.1:6123" {
		t.Fatalf("Pi Loom environment = %q", environment)
	}
}

func TestPiRuntimeUsesHubLocalAPIURL(t *testing.T) {
	configureFakePiHubRPC(t, "happy")
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h, err := OpenWithOptions(st, OpenOptions{RuntimeAPIURL: "http://127.0.0.1:6234"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown()
	if _, err := h.CreateAgent(CreateParams{Name: "pi-api", Cwd: t.TempDir(), RuntimeKind: "pi"}); err != nil {
		t.Fatal(err)
	}
	environment, err := os.ReadFile(os.Getenv("FAKE_PI_RUNTIME_ENV_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(environment), "\nhttp://127.0.0.1:6234") {
		t.Fatalf("Pi API URL environment = %q", environment)
	}
}

func TestPiRuntimeSteersExactActiveNativeTurn(t *testing.T) {
	configureFakePiHubRPC(t, "steer")
	dataDir := t.TempDir()
	runtime := newPiAgentRuntime("agent-pi", dataDir, "http://127.0.0.1:6123")
	defer runtime.Close()
	nativeRef, err := runtime.Create(nativeBindingRequest{Name: "Pi Worker", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	nativeTurnID, err := runtime.StartTurn(nativeTurnRequest{
		NativeRef: nativeRef, Input: []nativeInput{{Kind: nativeInputText, Text: "request help"}}, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := runtime.Steer(nativeRef, nativeTurnID, "causal reply", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if accepted != nativeTurnID {
		t.Fatalf("accepted native Turn = %q, want %q", accepted, nativeTurnID)
	}
	steer, err := os.ReadFile(os.Getenv("FAKE_PI_STEER_FILE"))
	if err != nil || string(steer) != "causal reply" {
		t.Fatalf("Pi native steer = %q, err=%v", steer, err)
	}
}

func TestPiAgentSendsSupportedImagesAsNativeRPCContent(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "image")
	h := testHub(st)
	h.stop = make(chan struct{})
	defer h.Shutdown()

	agent, err := h.CreateAgent(CreateParams{Name: "pi-vision", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	if descriptor, ok := capabilityDescriptor(agent.CapabilitySnapshot, runtimecontract.CapabilityImageInput); !ok || descriptor.Availability != runtimecontract.CapabilityAvailable {
		t.Fatalf("Pi image capability = %#v", agent.CapabilitySnapshot)
	}
	imageBytes := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x5a}, 64)...)
	image, err := h.StageThreadArtifact(agent.ID, "diagram.png", "image/png", bytes.NewReader(imageBytes))
	if err != nil {
		t.Fatal(err)
	}
	document, err := h.StageThreadArtifact(agent.ID, "brief.pdf", "application/pdf", strings.NewReader("%PDF-1.4\nloom"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.SendTaskWithArtifacts(agent.ID, "Review both files", []string{image.ID, document.ID}, time.Second); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(os.Getenv("FAKE_PI_PROMPT_JSON_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	var prompt struct {
		Message string `json:"message"`
		Images  []struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"images"`
	}
	if err := json.Unmarshal(data, &prompt); err != nil {
		t.Fatal(err)
	}
	if len(prompt.Images) != 1 || prompt.Images[0].Type != "image" || prompt.Images[0].MimeType != "image/png" ||
		prompt.Images[0].Data != base64.StdEncoding.EncodeToString(imageBytes) {
		t.Fatalf("Pi native images = %#v", prompt.Images)
	}
	if !strings.Contains(prompt.Message, document.Path) {
		t.Fatalf("generic file was not path-only guidance: %s", data)
	}
	history, err := h.CanonicalHistory(agent.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var historyImage *runtimecontract.Image
	attachments := make([]*runtimecontract.Attachment, 0, 1)
	for _, block := range history.Turns[0].Content {
		if block.Image != nil {
			historyImage = block.Image
		}
		if block.Attachment != nil {
			attachments = append(attachments, block.Attachment)
		}
	}
	if historyImage == nil || historyImage.ID != image.ID || historyImage.Ref != image.URL || len(attachments) != 1 || attachments[0].ID != document.ID {
		t.Fatalf("Pi history image=%#v attachments=%#v", historyImage, attachments)
	}
}

func TestPiAgentWaitsForAcceptedImagePromptToCreateNativeUserEntry(t *testing.T) {
	configureFakePiHubRPC(t, "image-entry-delayed")
	runtime := newPiAgentRuntime("agent-pi", t.TempDir(), "http://127.0.0.1:6123")
	defer runtime.Close()
	nativeRef, err := runtime.Create(nativeBindingRequest{Name: "Pi Vision", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(t.TempDir(), "diagram.png")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	nativeTurnID, err := runtime.StartTurn(nativeTurnRequest{
		NativeRef: nativeRef,
		Input: []nativeInput{
			{Kind: nativeInputText, Text: "Review this image"},
			{Kind: nativeInputLocalImage, Path: imagePath, MimeType: "image/png"},
		},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if nativeTurnID != "user-1" {
		t.Fatalf("native Turn ID = %q, want user-1", nativeTurnID)
	}
}

func TestPiProtocolFailureReconcilesDurableFailedTurn(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "malformed-after-prompt")
	h := testHub(st)
	h.stop = make(chan struct{})
	agent, err := h.CreateAgent(CreateParams{Name: "pi-failure", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.SendTask(agent.ID, "break protocol", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		view, _ := h.GetAgent(agent.ID)
		if view.Status == "idle" && view.LastTurn != nil {
			if view.LastTurn.TurnID != result.TurnID {
				t.Fatalf("failed Pi Turn ID = %q, want %q", view.LastTurn.TurnID, result.TurnID)
			}
			if view.LastTurn.Status != "failed" {
				t.Fatalf("Pi Turn status = %q, want failed", view.LastTurn.Status)
			}
			if !strings.Contains(view.LastError, "protocol") {
				t.Fatalf("Pi protocol failure = %q", view.LastError)
			}
			h.Shutdown()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Pi protocol failure did not reconcile the durable failed Turn")
}

func TestPiCleanProcessLossCreatesOneRecoveryTurnWithoutReplayingOriginalPrompt(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "crash-clean")
	h := testHub(st)
	h.stop = make(chan struct{})
	defer h.Shutdown()
	agent, err := h.CreateAgent(CreateParams{Name: "pi-recovery", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err := h.SendTask(agent.ID, "publish once", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(4 * time.Second)
	var marker TurnRecoveryMarker
	for time.Now().Before(deadline) {
		h.mu.Lock()
		marker = h.agents[agent.ID].TurnRecoveryMarkers[predecessor.TurnID]
		h.mu.Unlock()
		if marker.State == TurnRecoveryCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if marker.State != TurnRecoveryCompleted || marker.RecoveryTurnID == "" || marker.RecoveryTurnID == predecessor.TurnID {
		t.Fatalf("clean crash recovery marker = %#v", marker)
	}
	prompts, err := os.ReadFile(os.Getenv("FAKE_PI_PROMPTS_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(prompts), "\n--- prompt ---\n") != 2 || strings.Count(string(prompts), "publish once") != 2 || !strings.Contains(string(prompts), `<loom_turn_recovery`) {
		t.Fatalf("clean crash prompts = %q", prompts)
	}
	starts, err := os.ReadFile(os.Getenv("FAKE_PI_STARTS_FILE"))
	if err != nil || strings.Count(string(starts), "start\n") != 2 {
		t.Fatalf("Pi process starts = %q, err=%v", starts, err)
	}
	events, err := st.ReadEvents(agent.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var predecessorInterrupted, recoveryCompleted bool
	for _, event := range events {
		predecessorInterrupted = predecessorInterrupted || event.Type == "loom/turn-interrupted" && strings.Contains(string(event.Data), predecessor.TurnID)
		recoveryCompleted = recoveryCompleted || event.Type == "loom/turn-completed" && strings.Contains(string(event.Data), marker.RecoveryTurnID)
	}
	if !predecessorInterrupted || !recoveryCompleted {
		t.Fatalf("clean crash terminal events = %#v", events)
	}
}

func TestPiProcessLossWithUnfinishedToolCreatesOneNeedsYou(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "crash-ambiguous")
	h := testHub(st)
	h.stop = make(chan struct{})
	defer h.Shutdown()
	agent, err := h.CreateAgent(CreateParams{Name: "pi-ambiguous", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	predecessor, err := h.SendTask(agent.ID, "deploy once", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(4 * time.Second)
	var requests []HumanRequest
	var marker TurnRecoveryMarker
	for time.Now().Before(deadline) {
		requests, _ = h.ListHumanRequests(agent.ID, "all")
		h.mu.Lock()
		marker = h.agents[agent.ID].TurnRecoveryMarkers[predecessor.TurnID]
		h.mu.Unlock()
		if len(requests) == 1 && marker.State == TurnRecoveryDispatched {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(requests) != 1 || requests[0].SourceTurnID != predecessor.TurnID || requests[0].ThreadID != agent.ThreadID {
		t.Fatalf("ambiguous crash Needs You = %#v", requests)
	}
	if !strings.Contains(requests[0].Context, "deploy --prod") || !strings.Contains(requests[0].Context, "partially completed") {
		t.Fatalf("ambiguous crash context = %s", requests[0].Context)
	}
	if marker.Disposition != "needs_you" || marker.State != TurnRecoveryDispatched || marker.HumanRequestID != requests[0].ID || marker.RecoveryTurnID != "" {
		t.Fatalf("ambiguous crash marker = %#v", marker)
	}
	history, err := h.CanonicalHistory(agent.ID, 10, 0)
	if err != nil || len(history.Turns) != 1 {
		t.Fatalf("ambiguous crash history = %#v, err=%v", history, err)
	}
	if history.Turns[0].TurnID != predecessor.TurnID || history.Turns[0].State != runtimecontract.LifecycleInterrupted {
		t.Fatalf("ambiguous predecessor history = %#v, want interrupted", history.Turns[0])
	}
	prompts, err := os.ReadFile(os.Getenv("FAKE_PI_PROMPTS_FILE"))
	if err != nil || strings.Count(string(prompts), "\n--- prompt ---\n") != 1 {
		t.Fatalf("ambiguous crash prompts = %q, err=%v", prompts, err)
	}
	h.recoverPiInterruptedTurn(agent.ID, predecessor.TurnID)
	requests, _ = h.ListHumanRequests(agent.ID, "all")
	if len(requests) != 1 {
		t.Fatalf("duplicate ambiguous Needs You = %#v", requests)
	}

	t.Setenv("FAKE_PI_HUB_SCENARIO", "happy")
	if _, err := h.AnswerHumanRequest(requests[0].ID, AnswerHumanRequestParams{Answer: "The deployment did not complete; continue with verification first."}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(4 * time.Second)
	var delivered HumanRequest
	for time.Now().Before(deadline) {
		delivered, _ = h.GetHumanRequest(requests[0].ID)
		if delivered.DeliveryStatus == "delivered" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if delivered.DeliveryStatus != "delivered" || delivered.ResumedTurnID == "" || delivered.ResumedTurnID == predecessor.TurnID || delivered.AgentID != agent.ID || delivered.ThreadID != agent.ThreadID {
		t.Fatalf("eventual Needs You continuation = %#v", delivered)
	}
	history, err = h.CanonicalHistory(agent.ID, 10, 0)
	if err != nil || len(history.Turns) != 2 {
		t.Fatalf("continued ambiguous crash history = %#v, err=%v", history, err)
	}
	if history.Turns[0].TurnID != predecessor.TurnID || history.Turns[0].State != runtimecontract.LifecycleInterrupted {
		t.Fatalf("continued ambiguous predecessor history = %#v, want interrupted", history.Turns[0])
	}
	if len(history.Turns[0].Content) != 2 || history.Turns[0].Content[1].ToolCall == nil {
		t.Fatalf("continued ambiguous predecessor tool history = %#v, want unfinished tool call without synthetic success", history.Turns[0].Content)
	}
	detail, err := h.GetCanonicalTurn(predecessor.TurnID)
	if err != nil || detail.State != runtimecontract.LifecycleInterrupted || len(detail.Content) != 2 || detail.Content[1].ToolCall == nil {
		t.Fatalf("continued ambiguous predecessor Turn = %#v, err=%v", detail, err)
	}
}

func TestPiStoreRestartCreatesOneRecoveryTurnFromCleanDurableSession(t *testing.T) {
	configureFakePiHubRPC(t, "happy")
	dataDir := t.TempDir()
	sessionDir := filepath.Join(dataDir, "pi", "agent-restart")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(sessionDir, "session-agent-restart.jsonl")
	contents := strings.Join([]string{
		`{"type":"session","version":3,"id":"agent-restart","timestamp":"2026-08-10T01:00:00Z","cwd":"/tmp/work"}`,
		`{"type":"message","id":"user-before","parentId":null,"timestamp":"2026-08-10T01:00:01Z","message":{"role":"user","content":[{"type":"text","text":"publish after restart"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(sessionFile, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := now()
	if err := st.SaveAgents(map[string]*Agent{
		"agent-restart": {
			ID: "agent-restart", Name: "pi-restart", Cwd: t.TempDir(), ThreadID: "loom-thread-restart",
			RuntimeBinding: RuntimeBinding{Kind: "pi", NativeRef: sessionFile}, RuntimeTurnBindings: map[string]string{"turn-before": "user-before"},
			Status: "running", CurrentTurnID: "turn-before", CurrentTask: "publish after restart", CreatedAt: stamp, UpdatedAt: stamp,
		},
	}); err != nil {
		t.Fatal(err)
	}
	h, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	var marker TurnRecoveryMarker
	for time.Now().Before(deadline) {
		h.mu.Lock()
		marker = h.agents["agent-restart"].TurnRecoveryMarkers["turn-before"]
		h.mu.Unlock()
		if marker.State == TurnRecoveryCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if marker.State != TurnRecoveryCompleted || marker.RecoveryTurnID == "" {
		t.Fatalf("restart recovery marker = %#v", marker)
	}
	starts, err := os.ReadFile(os.Getenv("FAKE_PI_STARTS_FILE"))
	if err != nil || strings.Count(string(starts), "start\n") != 1 {
		t.Fatalf("first restart Pi starts = %q, err=%v", starts, err)
	}
	h.Shutdown()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := Open(reopened)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Shutdown()
	time.Sleep(100 * time.Millisecond)
	starts, err = os.ReadFile(os.Getenv("FAKE_PI_STARTS_FILE"))
	if err != nil || strings.Count(string(starts), "start\n") != 1 {
		t.Fatalf("second restart duplicated Pi recovery starts = %q, err=%v", starts, err)
	}
}

func TestPiCanonicalHistoryAfterStoreRestartKeepsUnmatchedToolInterrupted(t *testing.T) {
	dataDir := t.TempDir()
	sessionFile := filepath.Join(dataDir, "session.jsonl")
	contents := strings.Join([]string{
		`{"type":"session","version":3,"id":"agent-restart","timestamp":"2026-08-10T01:00:00Z","cwd":"/tmp/work"}`,
		`{"type":"message","id":"user-before","parentId":null,"timestamp":"2026-08-10T01:00:01Z","message":{"role":"user","content":[{"type":"text","text":"publish after restart"}]}}`,
		`{"type":"message","id":"assistant-before","parentId":"user-before","timestamp":"2026-08-10T01:00:02Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-before","name":"bash","arguments":{"command":"deploy --prod"}}],"stopReason":"toolUse"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(sessionFile, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	stamp := now()
	if err := st.SaveAgents(map[string]*Agent{"agent-restart": {
		ID: "agent-restart", Name: "pi-restart", Cwd: t.TempDir(), ThreadID: "loom-thread-restart",
		RuntimeBinding: RuntimeBinding{SchemaVersion: RuntimeBindingSchemaVersion, Kind: "pi", NativeRef: sessionFile}, RuntimeTurnBindings: map[string]string{"turn-before": "user-before"},
		TurnRecoveryMarkers: map[string]TurnRecoveryMarker{"turn-before": {PredecessorTurnID: "turn-before", State: TurnRecoveryCompleted, CreatedAt: stamp, UpdatedAt: stamp}},
		Status:              "idle", CreatedAt: stamp, UpdatedAt: stamp,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	h, err := Open(reopened)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown()
	history, err := h.CanonicalHistory("agent-restart", 10, 0)
	if err != nil || len(history.Turns) != 1 {
		t.Fatalf("canonical history=%#v err=%v", history, err)
	}
	predecessor := history.Turns[0]
	if predecessor.TurnID != "turn-before" || predecessor.State != runtimecontract.LifecycleInterrupted || len(predecessor.Content) < 2 || predecessor.Content[1].Kind != runtimecontract.ContentToolCall {
		t.Fatalf("restarted Pi predecessor = %#v", predecessor)
	}
	for _, content := range predecessor.Content {
		if content.Kind == runtimecontract.ContentToolResult && content.ToolResult != nil && content.ToolResult.Success {
			t.Fatalf("restart invented successful tool result: %#v", predecessor.Content)
		}
	}
}

func TestPiRuntimeNormalizesStreamingTextReasoningAndToolLifecycle(t *testing.T) {
	runtime := newPiAgentRuntime("agent-1", t.TempDir(), "")
	rawEvents := []string{
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","contentIndex":0,"delta":"checking"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":1,"delta":"hello"}}`,
		`{"type":"tool_execution_start","toolCallId":"call-1","toolName":"bash","args":{"command":"pwd"}}`,
		`{"type":"tool_execution_update","toolCallId":"call-1","toolName":"bash","args":{"command":"pwd"},"partialResult":{"content":[{"type":"text","text":"/tmp"}]}}`,
		`{"type":"tool_execution_end","toolCallId":"call-1","toolName":"bash","result":{"content":[{"type":"text","text":"/tmp/project"}]},"isError":false}`,
		`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"thinking","thinking":"checking files"},{"type":"text","text":"hello world"}]}}`,
	}
	var events []nativeEvent
	for _, raw := range rawEvents {
		events = append(events, runtime.NormalizeEvent("", json.RawMessage(raw))...)
	}
	if len(events) != 7 {
		t.Fatalf("normalized events = %#v, want 7", events)
	}
	wantKinds := []nativeEventKind{
		nativeReasoningDelta, nativeTextDelta, nativeToolStarted, nativeToolUpdated,
		nativeToolCompleted, nativeReasoningCompleted, nativeTextCompleted,
	}
	for i, want := range wantKinds {
		if events[i].Kind != want {
			t.Fatalf("event %d kind = %q, want %q (all=%#v)", i, events[i].Kind, want, events)
		}
	}
	if events[0].ItemID == "" || events[0].ItemID == events[1].ItemID || events[0].Text != "checking" || events[1].Text != "hello" {
		t.Fatalf("stream correlation = %#v", events[:2])
	}
	for _, index := range []int{2, 3, 4} {
		if events[index].ItemID != "call-1" {
			t.Fatalf("tool event %d correlation = %#v", index, events[index])
		}
	}
	if events[3].Item["aggregatedOutput"] != "/tmp" || events[4].Item["aggregatedOutput"] != "/tmp/project" || events[4].Item["status"] != "completed" {
		t.Fatalf("tool lifecycle = %#v", events[2:5])
	}
	if events[5].ItemID != events[0].ItemID || events[5].Text != "checking files" || events[6].ItemID != events[1].ItemID || events[6].Text != "hello world" {
		t.Fatalf("completed message correlation = %#v", events[5:])
	}
}

func TestPiRuntimeCapturesRedactedNativeDiagnosticBeforeNormalization(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := testHub(st)
	h.agents["agent-1"] = &Agent{ID: "agent-1", Name: "pi", RuntimeBinding: RuntimeBinding{Kind: "pi"}}
	runtime := newPiAgentRuntime("agent-1", t.TempDir(), "")
	runtime.SetRuntimeDiagnosticHandler(func(raw json.RawMessage) {
		h.mu.Lock()
		h.appendRuntimeDiagnosticLocked("agent-1", "pi/rpc-event", raw)
		h.mu.Unlock()
	})
	runtime.handleEvent(json.RawMessage(`{"type":"message_update","nativeEntryId":"entry-1","apiKey":"private","assistantMessageEvent":{"type":"text_delta","delta":"hello"}}`))
	events, err := h.ReadRuntimeDiagnosticEvents("agent-1", 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("diagnostic events = %#v, err=%v", events, err)
	}
	data := string(events[0].Data)
	if !strings.Contains(data, "entry-1") || !strings.Contains(data, "[redacted]") || strings.Contains(data, "private") {
		t.Fatalf("Pi diagnostic projection = %s", data)
	}
}

func TestPiAbortKeepsLoomTurnRunningUntilAbortedAgentSettles(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "abort")
	h := testHub(st)
	h.stop = make(chan struct{})
	defer h.Shutdown()
	agent, err := h.CreateAgent(CreateParams{Name: "pi-abort", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.SendTask(agent.ID, "stream until stopped", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitForPiFile(t, os.Getenv("FAKE_PI_ABORT_READY_FILE"))

	type interruptResult struct {
		result InterruptResult
		err    error
	}
	interruptCh := make(chan interruptResult, 1)
	go func() {
		interrupted, err := h.Interrupt(agent.ID, "Owner stopped Pi")
		interruptCh <- interruptResult{result: interrupted, err: err}
	}()

	waitForPiFile(t, os.Getenv("FAKE_PI_AGENT_END_FILE"))
	view, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "running" || view.CurrentTurnID != result.TurnID || view.LastTurn != nil {
		t.Fatalf("Loom settled before Pi agent_settled: %#v", view.Agent)
	}

	select {
	case stopped := <-interruptCh:
		if stopped.err != nil || !stopped.result.Interrupted {
			t.Fatalf("Interrupt() = (%#v, %v)", stopped.result, stopped.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Interrupt did not return after Pi settled")
	}
	view, err = h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "idle" || view.LastTurn == nil || view.LastTurn.Status != "interrupted" || view.LastTurn.TurnID != result.TurnID {
		t.Fatalf("settled aborted Pi Turn = %#v", view.Agent)
	}
	if len(h.agents[agent.ID].TurnRecoveryMarkers) != 0 || len(h.humanRequests) != 0 {
		t.Fatalf("Owner abort triggered crash recovery markers=%#v requests=%#v", h.agents[agent.ID].TurnRecoveryMarkers, h.humanRequests)
	}
	events, err := st.ReadEvents(agent.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	terminal := ""
	for _, event := range events {
		if event.Type == "loom/runtime-event" {
			var canonical runtimecontract.Event
			if json.Unmarshal(event.Data, &canonical) == nil && canonical.Kind == runtimecontract.EventContent &&
				canonical.Content != nil && canonical.Content.Kind == runtimecontract.ContentAssistantText &&
				canonical.ContentPhase == runtimecontract.ContentPhaseDelta {
				text += canonical.Content.Text
			}
		}
		switch event.Type {
		case "loom/turn-completed", "loom/turn-failed", "loom/turn-interrupted":
			terminal = event.Type
		}
	}
	if text != "before abort after abort" || terminal != "loom/turn-interrupted" {
		t.Fatalf("post-abort event stream text=%q terminal=%q events=%#v", text, terminal, events)
	}
}

func TestPiRetryAndCompactionContinuationSettleOnlyFinalAssistantState(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "retry-compaction")
	h := testHub(st)
	h.stop = make(chan struct{})
	defer h.Shutdown()
	agent, err := h.CreateAgent(CreateParams{Name: "pi-retry", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.SendTask(agent.ID, "recover transparently", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitForPiFile(t, os.Getenv("FAKE_PI_AGENT_END_FILE"))
	view, err := h.GetAgent(agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "running" || view.LastTurn != nil {
		t.Fatalf("Loom settled during retry/compaction continuation: %#v", view.Agent)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, _ = h.GetAgent(agent.ID)
		if view.LastTurn != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if view.LastTurn == nil || view.LastTurn.TurnID != result.TurnID || view.LastTurn.Status != "completed" {
		t.Fatalf("settled retry Turn = %#v", view.Agent)
	}
	events, err := st.ReadEvents(agent.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var deltas string
	terminals := 0
	for _, event := range events {
		if event.Type != "loom/runtime-event" {
			continue
		}
		var canonical runtimecontract.Event
		if json.Unmarshal(event.Data, &canonical) != nil {
			continue
		}
		if canonical.Kind == runtimecontract.EventContent && canonical.Content != nil && canonical.Content.Kind == runtimecontract.ContentAssistantText && canonical.ContentPhase == runtimecontract.ContentPhaseDelta {
			deltas += canonical.Content.Text
		}
		if canonical.Kind == runtimecontract.EventTerminal {
			terminals++
		}
	}
	if deltas != "first attemptrecovered" || terminals != 1 {
		t.Fatalf("retry stream deltas=%q terminals=%d events=%#v", deltas, terminals, events)
	}
}

func TestPiHistoryProjectsOnlyActiveBranchAndKeepsPreCompactionTurns(t *testing.T) {
	sessionFile := filepath.Join(t.TempDir(), "session.jsonl")
	contents := strings.Join([]string{
		`{"type":"session","version":3,"id":"session-1","timestamp":"2026-08-10T01:00:00.000Z","cwd":"/tmp/work"}`,
		`{"type":"message","id":"user-1","parentId":null,"timestamp":"2026-08-10T01:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"<loom_developer_context version=\"1\" complete=\"true\">latest profile</loom_developer_context>\n\nfirst task\n\n<loom_context version=\"1\"><loom_turn_context origin=\"owner\" kind=\"direct_input\" /></loom_context>"}]}}`,
		`{"type":"message","id":"assistant-1","parentId":"user-1","timestamp":"2026-08-10T01:00:02.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"inspect"},{"type":"text","text":"first answer"}],"stopReason":"stop","provider":"openai","model":"gpt-5","usage":{"input":10,"output":4,"cacheRead":2,"totalTokens":14}}}`,
		`{"type":"message","id":"abandoned-user","parentId":"assistant-1","timestamp":"2026-08-10T01:00:03.000Z","message":{"role":"user","content":[{"type":"text","text":"abandoned task"}]}}`,
		`{"type":"message","id":"abandoned-answer","parentId":"abandoned-user","timestamp":"2026-08-10T01:00:04.000Z","message":{"role":"assistant","content":[{"type":"text","text":"abandoned answer"}],"stopReason":"stop"}}`,
		`{"type":"compaction","id":"compact-1","parentId":"assistant-1","timestamp":"2026-08-10T01:00:05.000Z","summary":"first task completed","firstKeptEntryId":"user-1","tokensBefore":100}`,
		`{"type":"message","id":"user-2","parentId":"compact-1","timestamp":"2026-08-10T01:00:06.000Z","message":{"role":"user","content":[{"type":"text","text":"second task"}]}}`,
		`{"type":"message","id":"assistant-2","parentId":"user-2","timestamp":"2026-08-10T01:00:07.000Z","message":{"role":"assistant","content":[{"type":"text","text":"second answer"}],"stopReason":"stop"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(sessionFile, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	runtime := newPiAgentRuntime("agent-1", t.TempDir(), "")
	history, err := runtime.ReadHistory(sessionFile, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if history.Total != 2 || len(history.Turns) != 2 {
		t.Fatalf("Pi history = %#v, want two active-branch Turns", history)
	}
	if history.Turns[0].ID != "user-1" || history.Turns[0].Task != "first task" || strings.Contains(history.Turns[0].Task, "loom_") || history.Turns[0].Status != "completed" {
		t.Fatalf("pre-compaction Turn = %#v", history.Turns[0])
	}
	if history.Turns[1].ID != "user-2" || history.Turns[1].Task != "second task" || history.Turns[1].Status != "completed" {
		t.Fatalf("latest Turn = %#v", history.Turns[1])
	}
	encoded, _ := json.Marshal(history.Turns)
	if strings.Contains(string(encoded), "abandoned") || !strings.Contains(string(encoded), "first answer") || !strings.Contains(string(encoded), "second answer") {
		t.Fatalf("active branch projection = %s", encoded)
	}
	if got, err := os.ReadFile(sessionFile); err != nil || string(got) != contents {
		t.Fatalf("history read mutated native JSONL: err=%v\n%s", err, got)
	}

	latest, err := runtime.ReadHistory(sessionFile, 1, 0)
	if err != nil || latest.Total != 2 || len(latest.Turns) != 1 || latest.Turns[0].ID != "user-2" {
		t.Fatalf("latest history page = %#v, err=%v", latest, err)
	}
	older, err := runtime.ReadHistory(sessionFile, 1, 1)
	if err != nil || older.Total != 2 || len(older.Turns) != 1 || older.Turns[0].ID != "user-1" {
		t.Fatalf("older history page = %#v, err=%v", older, err)
	}
	turn, err := runtime.ReadTurn(sessionFile, "user-1")
	if err != nil || turn.Task != "first task" {
		t.Fatalf("ReadTurn = %#v, err=%v", turn, err)
	}
	last, err := runtime.LatestTurn(sessionFile)
	if err != nil || last == nil || last.ID != "user-2" {
		t.Fatalf("LatestTurn = %#v, err=%v", last, err)
	}
}

func TestPiHistoryKeepsCausalAgentMessageSteerInOriginalTurn(t *testing.T) {
	sessionFile := filepath.Join(t.TempDir(), "session.jsonl")
	contents := strings.Join([]string{
		`{"type":"session","version":3,"id":"session-1","timestamp":"2026-08-10T01:00:00.000Z","cwd":"/tmp/work"}`,
		`{"type":"message","id":"user-1","parentId":null,"timestamp":"2026-08-10T01:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"review Pi runtime"}]}}`,
		`{"type":"message","id":"assistant-1","parentId":"user-1","timestamp":"2026-08-10T01:00:02.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"bash","arguments":{"command":"sleep 30"}}],"stopReason":"toolUse"}}`,
		`{"type":"message","id":"result-1","parentId":"assistant-1","timestamp":"2026-08-10T01:00:03.000Z","message":{"role":"toolResult","toolCallId":"call-1","toolName":"bash","content":[{"type":"text","text":"done"}],"isError":false}}`,
		`{"type":"message","id":"steer-1","parentId":"result-1","timestamp":"2026-08-10T01:00:04.000Z","message":{"role":"user","content":[{"type":"text","text":"<agent_message version=\"1\" id=\"msg_reply\" response=\"required\" status=\"answered\">\n  <from>codex-agent</from>\n  <to>pi-agent</to>\n  <reply_to>msg_root</reply_to>\n  <body><![CDATA[review complete]]></body>\n</agent_message>"}]}}`,
		`{"type":"message","id":"assistant-2","parentId":"steer-1","timestamp":"2026-08-10T01:00:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"integrated review"}],"stopReason":"stop"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(sessionFile, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	runtime := newPiAgentRuntime("agent-1", t.TempDir(), "")
	history, err := runtime.ReadHistory(sessionFile, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if history.Total != 1 || len(history.Turns) != 1 {
		t.Fatalf("Pi causal-steer history = %#v, want one Turn", history)
	}
	turn := history.Turns[0]
	if turn.ID != "user-1" || turn.Status != "completed" {
		t.Fatalf("Pi causal-steer Turn = %#v", turn)
	}
	encoded, _ := json.Marshal(turn)
	if !strings.Contains(string(encoded), "msg_reply") || !strings.Contains(string(encoded), "integrated review") || strings.Contains(string(encoded), `"id":"steer-1"`) {
		t.Fatalf("Pi causal-steer projection = %s", encoded)
	}
}

func TestPiVisibleHistoryHidesManagedMessageAndTopicContextEnvelope(t *testing.T) {
	prompt := `<loom_developer_context version="1">profile</loom_developer_context>` + "\n\n" +
		`Review the incident.` + "\n\n" +
		`<loom_context version="1"><loom_turn_context origin="internal_agent" kind="agent_message" topic_id="topic-1"><payload><![CDATA[<agent_message><subject>Incident</subject><body>Review the incident.</body></agent_message>]]></payload></loom_turn_context></loom_context>`
	visible := piVisibleUserText(prompt)
	if visible != "Review the incident." {
		t.Fatalf("managed Pi history payload = %s", visible)
	}
}

func TestPiSessionResumesAfterStoreReopenWithStableLoomIdentityAndLatestContext(t *testing.T) {
	configureFakePiHubRPC(t, "persistence")
	dataDir := t.TempDir()
	workDir := t.TempDir()

	st1, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h1 := New(st1)
	worker, err := h1.CreateAgent(CreateParams{Name: "pi-worker", Cwd: workDir, RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := h1.CreateAgent(CreateParams{Name: "pi-peer", Cwd: workDir, RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h1.UpdateProfile(worker.ID, ProfileParams{Identity: "original identity", Domain: "original domain"}); err != nil {
		t.Fatal(err)
	}
	relationship, err := h1.CreateRelationship(RelationshipParams{From: peer.ID, To: worker.ID, Description: "original relationship"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := h1.SendTask(worker.ID, "first durable task", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitForPiTurn(t, h1, worker.ID, first.TurnID)
	nativeRef := h1.agents[worker.ID].RuntimeBinding.NativeRef
	loomThreadID := worker.ThreadID
	h1.Shutdown()
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h2 := New(st2)
	defer func() {
		h2.Shutdown()
		_ = st2.Close()
	}()
	reopened, err := h2.GetAgent(worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.ID != worker.ID || reopened.ThreadID != loomThreadID || h2.agents[worker.ID].RuntimeBinding.NativeRef != nativeRef {
		t.Fatalf("reopened Pi identity = %#v native=%q, want Agent %q Thread %q native %q", reopened.Agent, h2.agents[worker.ID].RuntimeBinding.NativeRef, worker.ID, loomThreadID, nativeRef)
	}
	reopenedHistory, err := h2.CanonicalHistory(worker.ID, 10, 0)
	if err != nil || reopenedHistory.Total != 1 || len(reopenedHistory.Turns) != 1 || reopenedHistory.Turns[0].TurnID != first.TurnID {
		t.Fatalf("history before Pi process resume = %#v, err=%v", reopenedHistory, err)
	}
	reopenedTurn, err := h2.GetCanonicalTurn(first.TurnID)
	if err != nil || reopenedTurn.TurnID != first.TurnID || reopenedTurn.AgentID != worker.ID || reopenedTurn.State != runtimecontract.LifecycleCompleted {
		t.Fatalf("Turn before Pi process resume = %#v, err=%v", reopenedTurn, err)
	}
	if _, err := h2.UpdateProfile(worker.ID, ProfileParams{Identity: "latest identity", Domain: "latest domain"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h2.UpdateRelationship(relationship.ID, RelationshipParams{Description: "latest relationship"}); err != nil {
		t.Fatal(err)
	}
	second, err := h2.SendTask(worker.ID, "continue after restart", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	waitForPiTurn(t, h2, worker.ID, second.TurnID)

	if second.TurnID == first.TurnID || h2.agents[worker.ID].RuntimeTurnBindings[first.TurnID] == "" || h2.agents[worker.ID].RuntimeTurnBindings[second.TurnID] == "" {
		t.Fatalf("durable Loom→Pi Turn bindings = %#v, first=%q second=%q", h2.agents[worker.ID].RuntimeTurnBindings, first.TurnID, second.TurnID)
	}
	resumeArgs, err := os.ReadFile(os.Getenv("FAKE_PI_RESUME_FILE"))
	if err != nil || !strings.Contains(string(resumeArgs), "--session\t"+nativeRef) {
		t.Fatalf("Pi resume invocation = %q, err=%v", resumeArgs, err)
	}
	prompts, err := os.ReadFile(os.Getenv("FAKE_PI_PROMPTS_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(prompts), "\n--- prompt ---\n")
	if len(parts) < 3 {
		t.Fatalf("persisted prompts = %q", prompts)
	}
	latestPrompt := parts[len(parts)-1]
	for _, want := range []string{"latest identity", "latest domain", "latest relationship", "continue after restart"} {
		if !strings.Contains(latestPrompt, want) {
			t.Fatalf("latest Pi prompt missing %q:\n%s", want, latestPrompt)
		}
	}
	for _, stale := range []string{"original identity", "original domain", "original relationship", "coverage_manifest", "epoch_id="} {
		if strings.Contains(latestPrompt, stale) {
			t.Fatalf("latest Pi prompt retained stale/Codex-only %q:\n%s", stale, latestPrompt)
		}
	}

	history, err := h2.CanonicalHistory(worker.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if history.Total != 2 || len(history.Turns) != 2 || history.Turns[0].TurnID != first.TurnID || history.Turns[1].TurnID != second.TurnID {
		t.Fatalf("reopened Pi history = %#v", history)
	}
	encoded, _ := json.Marshal(history.Turns)
	if strings.Contains(string(encoded), "loom_developer_context") || strings.Contains(string(encoded), "<loom_context") || !strings.Contains(string(encoded), "first durable task") || !strings.Contains(string(encoded), "continue after restart") {
		t.Fatalf("visible Pi history = %s", encoded)
	}
}

func TestPiSettledBeforeEntriesResponseStillPersistsLoomTurnBinding(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	configureFakePiHubRPC(t, "settle-before-entries")
	h := testHub(st)
	h.stop = make(chan struct{})
	defer h.Shutdown()

	agent, err := h.CreateAgent(CreateParams{Name: "pi-fast", Cwd: t.TempDir(), RuntimeKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.SendTask(agent.ID, "fast settled task", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if native := h.agents[agent.ID].RuntimeTurnBindings[result.TurnID]; native != "user-1" {
		t.Fatalf("fast-settled Loom→Pi binding = %q, want user-1 (all=%#v)", native, h.agents[agent.ID].RuntimeTurnBindings)
	}
	history, err := h.CanonicalHistory(agent.ID, 10, 0)
	if err != nil || len(history.Turns) != 1 || history.Turns[0].TurnID != result.TurnID {
		t.Fatalf("fast-settled history = %#v, err=%v", history, err)
	}
}

func configureFakePiHubRPC(t *testing.T, scenario string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "pi")
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = \"--version\" ] && { echo 0.84.1; exit 0; }\nexec %q -test.run=TestFakePiHubRPCProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_BIN", bin)
	t.Setenv("FAKE_PI_HUB_SCENARIO", scenario)
	t.Setenv("FAKE_PI_AGENT_END_FILE", filepath.Join(dir, "agent-end"))
	t.Setenv("FAKE_PI_PROMPT_FILE", filepath.Join(dir, "prompt"))
	t.Setenv("FAKE_PI_PROMPT_JSON_FILE", filepath.Join(dir, "prompt.json"))
	t.Setenv("FAKE_PI_STARTS_FILE", filepath.Join(dir, "starts"))
	t.Setenv("FAKE_PI_ABORT_READY_FILE", filepath.Join(dir, "abort-ready"))
	t.Setenv("FAKE_PI_PROMPTS_FILE", filepath.Join(dir, "prompts"))
	t.Setenv("FAKE_PI_RESUME_FILE", filepath.Join(dir, "resume"))
	t.Setenv("FAKE_PI_START_ARGS_FILE", filepath.Join(dir, "start-args"))
	t.Setenv("FAKE_PI_RUNTIME_ENV_FILE", filepath.Join(dir, "runtime-env"))
	t.Setenv("FAKE_PI_STEER_FILE", filepath.Join(dir, "steer"))
	t.Setenv("FAKE_PI_MODEL_FILE", filepath.Join(dir, "model"))
	t.Setenv("FAKE_PI_TOOL_EFFECT_FILE", filepath.Join(dir, "tool-effect"))
	t.Setenv("FAKE_PI_PID_FILE", filepath.Join(dir, "pid"))
	t.Setenv("FAKE_PI_RESUME_MISMATCH_MARKER", filepath.Join(dir, "resume-mismatch"))
}

func TestFakePiHubRPCProcess(t *testing.T) {
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "" {
		return
	}
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "--version" {
		fmt.Println("0.84.1")
		return
	}
	_ = os.WriteFile(os.Getenv("FAKE_PI_PID_FILE"), []byte(fmt.Sprint(os.Getpid())), 0o600)
	starts, _ := os.OpenFile(os.Getenv("FAKE_PI_STARTS_FILE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if starts != nil {
		_, _ = starts.WriteString("start\n")
		_ = starts.Close()
	}
	args := os.Args
	_ = os.WriteFile(os.Getenv("FAKE_PI_START_ARGS_FILE"), []byte(strings.Join(args, "\t")), 0o600)
	_ = os.WriteFile(os.Getenv("FAKE_PI_RUNTIME_ENV_FILE"), []byte(os.Getenv("CODEX_LOOM_AGENT_ID")+"\n"+os.Getenv("CODEX_LOOM_API_URL")), 0o600)
	sessionDir := argumentValue(args, "--session-dir")
	sessionID := argumentValue(args, "--session-id")
	resumedSession := argumentValue(args, "--session")
	if sessionDir == "" || (sessionID == "" && resumedSession == "") || !containsArgumentPair(args, "--mode", "rpc") {
		os.Exit(30)
	}
	if resumedSession != "" {
		_ = os.WriteFile(os.Getenv("FAKE_PI_RESUME_FILE"), []byte(strings.Join(args, "\t")), 0o600)
		sessionID = strings.TrimSuffix(strings.TrimPrefix(filepath.Base(resumedSession), "session-"), ".jsonl")
	}
	sessionFile := resumedSession
	if sessionFile == "" {
		sessionFile = filepath.Join(sessionDir, "session-"+sessionID+".jsonl")
	}
	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		_ = os.WriteFile(sessionFile, []byte(fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":"2026-08-10T01:00:00.000Z","cwd":"/tmp/work"}`, sessionID)+"\n"), 0o600)
	}
	reader := bufio.NewReader(os.Stdin)
	var command map[string]any
	readFakePiCommand(t, reader, &command)
	id, _ := command["id"].(string)
	reportedSessionFile := sessionFile
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "resume-mismatch-once" && resumedSession != "" {
		marker := os.Getenv("FAKE_PI_RESUME_MISMATCH_MARKER")
		if _, err := os.Stat(marker); os.IsNotExist(err) {
			_ = os.WriteFile(marker, []byte("mismatch returned"), 0o600)
			reportedSessionFile = filepath.Join(sessionDir, "session-wrong.jsonl")
		}
	}
	if scenario := os.Getenv("FAKE_PI_HUB_SCENARIO"); scenario == "image" || scenario == "image-entry-delayed" || scenario == "conformance" {
		fmt.Printf(`{"id":%q,"type":"response","command":"get_state","success":true,"data":{"sessionFile":%q,"sessionId":%q,"model":{"provider":"fixture","id":"vision","input":["text","image"]}}}`+"\n", id, reportedSessionFile, sessionID)
	} else {
		fmt.Printf(`{"id":%q,"type":"response","command":"get_state","success":true,"data":{"sessionFile":%q,"sessionId":%q}}`+"\n", id, reportedSessionFile, sessionID)
	}
	for {
		readFakePiCommand(t, reader, &command)
		id, _ = command["id"].(string)
		switch command["type"] {
		case "get_entries":
			respondFakePiEntries(command, sessionFile)
			goto entriesReady
		case "get_available_models":
			fmt.Printf(`{"id":%q,"type":"response","command":"get_available_models","success":true,"data":{"models":[{"provider":"fixture","id":"vision","input":["text","image"],"contextWindow":128000,"reasoning":true},{"provider":"fixture","id":"vision-next","input":["text","image"],"contextWindow":128000,"reasoning":true}]}}`+"\n", id)
		case "set_model":
			provider, _ := command["provider"].(string)
			model, _ := command["modelId"].(string)
			_ = os.WriteFile(os.Getenv("FAKE_PI_MODEL_FILE"), []byte(provider+"/"+model), 0o600)
			fmt.Printf(`{"id":%q,"type":"response","command":"set_model","success":true,"data":{"provider":%q,"id":%q,"input":["text","image"]}}`+"\n", id, provider, model)
		case "set_thinking_level":
			fmt.Printf(`{"id":%q,"type":"response","command":"set_thinking_level","success":true}`+"\n", id)
		default:
			os.Exit(34)
		}
	}

entriesReady:
	for {
		readFakePiCommand(t, reader, &command)
		id, _ = command["id"].(string)
		switch command["type"] {
		case "get_entries":
			respondFakePiEntries(command, sessionFile)
			continue
		case "get_available_models":
			fmt.Printf(`{"id":%q,"type":"response","command":"get_available_models","success":true,"data":{"models":[{"provider":"fixture","id":"vision","input":["text","image"],"contextWindow":128000,"reasoning":true},{"provider":"fixture","id":"vision-next","input":["text","image"],"contextWindow":128000,"reasoning":true}]}}`+"\n", id)
			continue
		case "set_model":
			provider, _ := command["provider"].(string)
			model, _ := command["modelId"].(string)
			_ = os.WriteFile(os.Getenv("FAKE_PI_MODEL_FILE"), []byte(provider+"/"+model), 0o600)
			fmt.Printf(`{"id":%q,"type":"response","command":"set_model","success":true,"data":{"provider":%q,"id":%q,"input":["text","image"]}}`+"\n", id, provider, model)
			continue
		case "set_thinking_level":
			fmt.Printf(`{"id":%q,"type":"response","command":"set_thinking_level","success":true}`+"\n", id)
			continue
		}
		break
	}
	id, _ = command["id"].(string)
	message, _ := command["message"].(string)
	_ = os.WriteFile(os.Getenv("FAKE_PI_PROMPT_FILE"), []byte(message), 0o600)
	if encoded, err := json.Marshal(command); err == nil {
		_ = os.WriteFile(os.Getenv("FAKE_PI_PROMPT_JSON_FILE"), encoded, 0o600)
	}
	if prompts := os.Getenv("FAKE_PI_PROMPTS_FILE"); prompts != "" {
		file, _ := os.OpenFile(prompts, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if file != nil {
			_, _ = file.WriteString("\n--- prompt ---\n" + message)
			_ = file.Close()
		}
	}
	answer, stopReason := "hello from Pi", "stop"
	scenario := os.Getenv("FAKE_PI_HUB_SCENARIO")
	if scenario == "accepted-no-entry" {
		fmt.Printf(`{"id":%q,"type":"response","command":"prompt","success":true}`+"\n", id)
		serveFakePiHistory(reader, sessionFile)
		return
	}
	switch scenario {
	case "abort":
		answer, stopReason = "before abort after abort", "aborted"
	case "conformance":
		answer, stopReason = "hello continued", "aborted"
	case "retry-compaction":
		answer = "recovered"
	case "malformed-after-prompt":
		answer, stopReason = "", "error"
	}
	crashInitial := scenario == "crash-clean" && !strings.Contains(message, "<loom_turn_recovery")
	if scenario == "image-entry-delayed" {
		// Pi may acknowledge the prompt before its session entry is observable.
	} else if crashInitial {
		appendFakePiInterruptedTurn(t, sessionFile, message, false)
	} else if scenario == "crash-ambiguous" {
		appendFakePiInterruptedTurn(t, sessionFile, message, true)
	} else {
		appendFakePiTurn(t, sessionFile, message, answer, stopReason)
	}
	if scenario == "conformance" {
		payload, _ := json.Marshal(map[string]any{
			"version": 1, "operation": "request_approval", "toolCallId": "call-conformance",
			"toolName": "bash", "input": map[string]any{"command": "printf conformance"},
		})
		event, _ := json.Marshal(map[string]any{
			"type": "extension_ui_request", "id": "ui-conformance", "method": "input",
			"title": "codex-loom:approval:v1", "placeholder": string(payload), "timeout": 5000,
		})
		fmt.Println(string(event))
		readFakePiCommand(t, reader, &command)
		if command["type"] != "extension_ui_response" || command["id"] != "ui-conformance" || command["value"] != "approve" {
			fmt.Fprintf(os.Stderr, "unexpected conformance approval response: %#v\n", command)
			os.Exit(36)
		}
		_ = os.WriteFile(os.Getenv("FAKE_PI_TOOL_EFFECT_FILE"), []byte("executed"), 0o600)
	}
	fmt.Printf(`{"id":%q,"type":"response","command":"prompt","success":true}`+"\n", id)
	if scenario == "image-entry-delayed" {
		serveOneFakePiEntries(t, reader, sessionFile)
		appendFakePiTurn(t, sessionFile, message, answer, stopReason)
		serveFakePiHistory(reader, sessionFile)
		return
	}
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "settle-before-entries" {
		var entriesCommand map[string]any
		readFakePiCommand(t, reader, &entriesCommand)
		fmt.Print("{\"type\":\"agent_start\"}\n")
		fmt.Print("{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n")
		fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":\"hello from Pi\"}]}}\n")
		fmt.Print("{\"type\":\"agent_settled\"}\n")
		respondFakePiEntries(entriesCommand, sessionFile)
		serveFakePiHistory(reader, sessionFile)
		return
	}
	serveOneFakePiEntries(t, reader, sessionFile)
	if crashInitial || scenario == "crash-ambiguous" {
		return
	}
	if strings.HasPrefix(scenario, "approval") {
		fmt.Print("{\"type\":\"agent_start\"}\n")
		fmt.Print("{\"type\":\"tool_execution_start\",\"toolCallId\":\"call-approval-1\",\"toolName\":\"bash\",\"args\":{\"command\":\"touch approval-effect\"}}\n")
		payload, _ := json.Marshal(map[string]any{
			"version": 1, "operation": "request_approval", "toolCallId": "call-approval-1",
			"toolName": "bash", "input": map[string]any{"command": "touch approval-effect"},
		})
		timeout := 5000
		if scenario == "approval-timeout" {
			timeout = 150
		}
		event, _ := json.Marshal(map[string]any{
			"type": "extension_ui_request", "id": "ui-approval-1", "method": "input",
			"title": "codex-loom:approval:v1", "placeholder": string(payload), "timeout": timeout,
		})
		fmt.Println(string(event))
		readFakePiCommand(t, reader, &command)
		if command["type"] != "extension_ui_response" || command["id"] != "ui-approval-1" {
			os.Exit(36)
		}
		decision, _ := command["value"].(string)
		answer := "tool approved"
		if decision == "approve" {
			_ = os.WriteFile(os.Getenv("FAKE_PI_TOOL_EFFECT_FILE"), []byte("executed"), 0o600)
			fmt.Print("{\"type\":\"tool_execution_end\",\"toolCallId\":\"call-approval-1\",\"toolName\":\"bash\",\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"executed\"}]},\"isError\":false}\n")
		} else if decision != "deny" && decision != "timeout" && decision != "abort" {
			os.Exit(37)
		} else {
			answer = "tool blocked: " + decision
			fmt.Printf("{\"type\":\"tool_execution_end\",\"toolCallId\":\"call-approval-1\",\"toolName\":\"bash\",\"result\":{\"content\":[{\"type\":\"text\",\"text\":%q}]},\"isError\":true}\n", answer)
		}
		fmt.Print("{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n")
		fmt.Printf("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n", answer)
		fmt.Print("{\"type\":\"agent_end\"}\n")
		fmt.Print("{\"type\":\"agent_settled\"}\n")
		serveFakePiHistory(reader, sessionFile)
		return
	}
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "steer" {
		fmt.Print("{\"type\":\"agent_start\"}\n")
		readFakePiCommand(t, reader, &command)
		id, _ = command["id"].(string)
		if command["type"] != "steer" {
			os.Exit(35)
		}
		_ = os.WriteFile(os.Getenv("FAKE_PI_STEER_FILE"), []byte(fmt.Sprint(command["message"])), 0o600)
		fmt.Printf(`{"id":%q,"type":"response","command":"steer","success":true}`+"\n", id)
		_, _ = reader.ReadString('\n')
		return
	}
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "conformance" {
		fmt.Print("{\"type\":\"agent_start\"}\n")
		fmt.Print("{\"type\":\"tool_execution_start\",\"toolCallId\":\"call-conformance\",\"toolName\":\"bash\",\"args\":{\"command\":\"printf conformance\"}}\n")
		fmt.Print("{\"type\":\"tool_execution_end\",\"toolCallId\":\"call-conformance\",\"toolName\":\"bash\",\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"executed\"}]},\"isError\":false}\n")
		fmt.Print("{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n")
		fmt.Print("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"hello \"}}\n")
		readFakePiCommand(t, reader, &command)
		id, _ = command["id"].(string)
		if command["type"] != "steer" {
			os.Exit(35)
		}
		_ = os.WriteFile(os.Getenv("FAKE_PI_STEER_FILE"), []byte(fmt.Sprint(command["message"])), 0o600)
		fmt.Printf(`{"id":%q,"type":"response","command":"steer","success":true}`+"\n", id)
		fmt.Print("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"continued\"}}\n")
		readFakePiCommand(t, reader, &command)
		id, _ = command["id"].(string)
		if command["type"] != "abort" {
			os.Exit(32)
		}
		fmt.Printf(`{"id":%q,"type":"response","command":"abort","success":true}`+"\n", id)
		fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"aborted\",\"content\":[{\"type\":\"text\",\"text\":\"hello continued\"}]}}\n")
		fmt.Print("{\"type\":\"agent_end\",\"willRetry\":false}\n")
		fmt.Print("{\"type\":\"agent_settled\"}\n")
		serveFakePiHistory(reader, sessionFile)
		return
	}
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "abort" {
		fmt.Print("{\"type\":\"agent_start\"}\n")
		fmt.Print("{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n")
		fmt.Print("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"before abort \"}}\n")
		_ = os.WriteFile(os.Getenv("FAKE_PI_ABORT_READY_FILE"), []byte("ready"), 0o600)
		readFakePiCommand(t, reader, &command)
		id, _ = command["id"].(string)
		if command["type"] != "abort" {
			os.Exit(32)
		}
		fmt.Printf(`{"id":%q,"type":"response","command":"abort","success":true}`+"\n", id)
		fmt.Print("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"after abort\"}}\n")
		fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"aborted\",\"content\":[{\"type\":\"text\",\"text\":\"before abort after abort\"}]}}\n")
		fmt.Print("{\"type\":\"agent_end\",\"willRetry\":false}\n")
		_ = os.WriteFile(os.Getenv("FAKE_PI_AGENT_END_FILE"), []byte("done"), 0o600)
		time.Sleep(250 * time.Millisecond)
		fmt.Print("{\"type\":\"agent_settled\"}\n")
		serveFakePiHistory(reader, sessionFile)
		return
	}
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "retry-compaction" {
		fmt.Print("{\"type\":\"agent_start\"}\n")
		fmt.Print("{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n")
		fmt.Print("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"first attempt\"}}\n")
		fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"error\",\"errorMessage\":\"transient overload\",\"content\":[]}}\n")
		fmt.Print("{\"type\":\"agent_end\",\"willRetry\":true}\n")
		fmt.Print("{\"type\":\"auto_retry_start\",\"attempt\":1,\"maxAttempts\":3}\n")
		_ = os.WriteFile(os.Getenv("FAKE_PI_AGENT_END_FILE"), []byte("done"), 0o600)
		time.Sleep(250 * time.Millisecond)
		fmt.Print("{\"type\":\"auto_retry_end\",\"success\":true,\"attempt\":1}\n")
		fmt.Print("{\"type\":\"compaction_start\",\"reason\":\"overflow\"}\n")
		fmt.Print("{\"type\":\"compaction_end\",\"reason\":\"overflow\",\"aborted\":false,\"willRetry\":true}\n")
		fmt.Print("{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n")
		fmt.Print("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"recovered\"}}\n")
		fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":\"recovered\"}]}}\n")
		fmt.Print("{\"type\":\"agent_end\",\"willRetry\":false}\n")
		time.Sleep(100 * time.Millisecond)
		fmt.Print("{\"type\":\"agent_settled\"}\n")
		serveFakePiHistory(reader, sessionFile)
		return
	}
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "malformed-after-prompt" {
		fmt.Print("not-json\n")
		return
	}
	fmt.Print("{\"type\":\"agent_start\"}\n")
	fmt.Print("{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n")
	fmt.Print("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"thinking_delta\",\"contentIndex\":0,\"delta\":\"checking\"}}\n")
	fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"toolUse\",\"content\":[{\"type\":\"thinking\",\"thinking\":\"checking\"}]}}\n")
	fmt.Print("{\"type\":\"tool_execution_start\",\"toolCallId\":\"call-1\",\"toolName\":\"bash\",\"args\":{\"command\":\"pwd\"}}\n")
	fmt.Print("{\"type\":\"tool_execution_update\",\"toolCallId\":\"call-1\",\"toolName\":\"bash\",\"args\":{\"command\":\"pwd\"},\"partialResult\":{\"content\":[{\"type\":\"text\",\"text\":\"/tmp\"}]}}\n")
	fmt.Print("{\"type\":\"tool_execution_end\",\"toolCallId\":\"call-1\",\"toolName\":\"bash\",\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"/tmp/project\"}]},\"isError\":false}\n")
	fmt.Print("{\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n")
	fmt.Print("{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"contentIndex\":0,\"delta\":\"hello from Pi\"}}\n")
	fmt.Print("{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":\"hello from Pi\"}]}}\n")
	fmt.Print("{\"type\":\"agent_end\"}\n")
	_ = os.WriteFile(os.Getenv("FAKE_PI_AGENT_END_FILE"), []byte("done"), 0o600)
	time.Sleep(250 * time.Millisecond)
	fmt.Print("{\"type\":\"agent_settled\"}\n")
	serveFakePiHistory(reader, sessionFile)
}

func appendFakePiTurn(t *testing.T, sessionFile, prompt, answer, stopReason string) {
	t.Helper()
	entries, leafID, err := readPiSessionEntries(sessionFile)
	if err != nil {
		os.Exit(33)
	}
	index := 1
	for _, entry := range entries {
		if entry.Type == "message" {
			var message struct {
				Role string `json:"role"`
			}
			_ = json.Unmarshal(entry.Message, &message)
			if message.Role == "user" {
				index++
			}
		}
	}
	userID := fmt.Sprintf("user-%d", index)
	assistantID := fmt.Sprintf("assistant-%d", index)
	user := map[string]any{
		"type": "message", "id": userID, "parentId": nil, "timestamp": fmt.Sprintf("2026-08-10T01:00:%02d.000Z", index*2),
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": prompt}}},
	}
	if leafID != "" {
		user["parentId"] = leafID
	}
	assistantMessage := map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": answer}}, "stopReason": stopReason, "model": "fake-pi"}
	if os.Getenv("FAKE_PI_HUB_SCENARIO") == "conformance" {
		assistantMessage["usage"] = map[string]any{"input": 10, "output": 4, "cacheRead": 2, "totalTokens": 14}
	}
	assistant := map[string]any{
		"type": "message", "id": assistantID, "parentId": userID, "timestamp": fmt.Sprintf("2026-08-10T01:00:%02d.500Z", index*2),
		"message": assistantMessage,
	}
	file, err := os.OpenFile(sessionFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(33)
	}
	defer file.Close()
	for _, entry := range []map[string]any{user, assistant} {
		line, _ := json.Marshal(entry)
		if _, err := file.Write(append(line, '\n')); err != nil {
			os.Exit(33)
		}
	}
}

func appendFakePiInterruptedTurn(t *testing.T, sessionFile, prompt string, unfinishedTool bool) {
	t.Helper()
	entries, leafID, err := readPiSessionEntries(sessionFile)
	if err != nil {
		os.Exit(33)
	}
	index := 1
	for _, entry := range entries {
		if entry.Type != "message" {
			continue
		}
		var message struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(entry.Message, &message)
		if message.Role == "user" {
			index++
		}
	}
	userID := fmt.Sprintf("user-%d", index)
	user := map[string]any{
		"type": "message", "id": userID, "parentId": nil, "timestamp": fmt.Sprintf("2026-08-10T01:00:%02d.000Z", index*2),
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": prompt}}},
	}
	if leafID != "" {
		user["parentId"] = leafID
	}
	toAppend := []map[string]any{user}
	if unfinishedTool {
		toAppend = append(toAppend, map[string]any{
			"type": "message", "id": fmt.Sprintf("assistant-%d", index), "parentId": userID,
			"timestamp": fmt.Sprintf("2026-08-10T01:00:%02d.500Z", index*2),
			"message": map[string]any{
				"role": "assistant", "stopReason": "toolUse",
				"content": []map[string]any{{"type": "toolCall", "id": "call-deploy", "name": "bash", "arguments": map[string]any{"command": "deploy --prod"}}},
			},
		})
	}
	file, err := os.OpenFile(sessionFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(33)
	}
	defer file.Close()
	for _, entry := range toAppend {
		line, _ := json.Marshal(entry)
		if _, err := file.Write(append(line, '\n')); err != nil {
			os.Exit(33)
		}
	}
}

func serveOneFakePiEntries(t *testing.T, reader *bufio.Reader, sessionFile string) {
	t.Helper()
	var command map[string]any
	readFakePiCommand(t, reader, &command)
	if command["type"] != "get_entries" {
		os.Exit(34)
	}
	respondFakePiEntries(command, sessionFile)
}

func serveFakePiHistory(reader *bufio.Reader, sessionFile string) {
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		var command map[string]any
		if json.Unmarshal([]byte(line), &command) != nil || command["type"] != "get_entries" {
			os.Exit(34)
		}
		respondFakePiEntries(command, sessionFile)
	}
}

func respondFakePiEntries(command map[string]any, sessionFile string) {
	entries, leafID, err := readPiSessionEntries(sessionFile)
	if err != nil {
		os.Exit(33)
	}
	data, _ := json.Marshal(map[string]any{"entries": entries, "leafId": leafID})
	id, _ := command["id"].(string)
	fmt.Printf(`{"id":%q,"type":"response","command":"get_entries","success":true,"data":%s}`+"\n", id, data)
}

func readFakePiCommand(t *testing.T, reader *bufio.Reader, command *map[string]any) {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil || json.Unmarshal([]byte(line), command) != nil {
		os.Exit(31)
	}
}

func argumentValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func containsArgumentPair(args []string, left, right string) bool {
	return argumentValue(args, left) == right
}

func waitForPiFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func waitForPiTurn(t *testing.T, h *Hub, agentID, turnID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, err := h.GetAgent(agentID)
		if err == nil && view.LastTurn != nil && view.LastTurn.TurnID == turnID {
			if view.LastTurn.Status != "completed" {
				t.Fatalf("Pi Turn %s settled as %#v", turnID, view.LastTurn)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for Pi Turn %s", turnID)
}
