package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
	"github.com/yan5xu/codex-loom/internal/store"
)

type runtimeConformanceFixture struct {
	driver               RuntimeHostDriver
	agentID              string
	createCwd            string
	prepareBinding       func(runtimecontract.Binding) runtimecontract.Binding
	emitAfterStart       func(runtimecontract.Binding, runtimecontract.Outcome)
	emitAfterStop        func(runtimecontract.Binding, runtimecontract.Outcome)
	emitAfterReopenStart func(runtimecontract.Binding, runtimecontract.Outcome)
	emitAfterReopenStop  func(runtimecontract.Binding, runtimecontract.Outcome)
	expectedUsageTotal   int64
	verifyEffects        func(runtimecontract.Binding, runtimecontract.Outcome)
	reopen               func(context.Context, AgentHost, runtimecontract.Binding) (RuntimeHostDriver, AgentHost, runtimecontract.Contract, runtimecontract.Outcome)
	verifyClosed         func(AgentHost)
	cleanup              func()
}

func TestRuntimeContractConformanceCodexPiAndMinimalFake(t *testing.T) {
	tests := []struct {
		name string
		new  func(*testing.T) runtimeConformanceFixture
	}{
		{name: "codex", new: newCodexConformanceFixture},
		{name: "pi", new: newPiConformanceFixture},
		{name: "minimal fake", new: newMinimalConformanceFixture},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runRuntimeContractConformance(t, test.new(t))
		})
	}
}

func runRuntimeContractConformance(t *testing.T, fixture runtimeConformanceFixture) {
	t.Helper()
	ctx := context.Background()
	driver := fixture.driver
	if err := driver.Preflight(ctx); err != nil {
		t.Fatalf("Driver Preflight: %v", err)
	}
	handle, err := driver.Acquire(ctx, AgentHostRequest{AgentID: fixture.agentID})
	if err != nil {
		t.Fatalf("Driver Acquire: %v", err)
	}
	defer func() {
		handle.Close()
		_ = driver.Shutdown(context.Background())
		if fixture.cleanup != nil {
			fixture.cleanup()
		}
	}()
	contract := handle.Contract()
	if got := contract.ContractVersion(); got != runtimecontract.Version {
		t.Fatalf("ContractVersion = %d, want %d", got, runtimecontract.Version)
	}

	createCwd := fixture.createCwd
	if createCwd == "" {
		createCwd = t.TempDir()
	}
	binding, outcome := contract.CreateBinding(ctx, runtimecontract.BindingRequest{AgentID: "conformance", Name: "conformance", Cwd: createCwd})
	if err := runtimeLifecycleOutcomeError(outcome, runtimecontract.LifecycleAccepted, false); err != nil {
		t.Fatalf("CreateBinding: %v", err)
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("created binding: %v", err)
	}
	if fixture.prepareBinding != nil {
		binding = fixture.prepareBinding(binding)
	}
	if outcome := contract.ResumeBinding(ctx, binding); runtimeLifecycleOutcomeError(outcome, runtimecontract.LifecycleCompleted, false) != nil {
		t.Fatalf("ResumeBinding = %#v", outcome)
	}

	snapshot := contract.CapabilitySnapshot(ctx, binding)
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("CapabilitySnapshot: %v", err)
	}
	if err := validateRuntimeCapabilityHooks(contract, snapshot); err != nil {
		t.Fatal(err)
	}
	certifyAvailableRuntimeHooks(t, contract, binding, snapshot)
	if configured := contract.ResumeBinding(ctx, binding); runtimeLifecycleOutcomeError(configured, runtimecontract.LifecycleCompleted, false) != nil {
		t.Fatalf("ResumeBinding after capability configuration = %#v", configured)
	}

	events := make(chan runtimecontract.Event, 64)
	contract.SetEventHandler(func(event runtimecontract.Event) { events <- event })
	input := []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleUser, Text: "hello"}}
	if descriptor, ok := capabilityDescriptor(snapshot, runtimecontract.CapabilityContextDelivery); ok && descriptor.Availability == runtimecontract.CapabilityAvailable {
		input = append(input, runtimecontract.InputBlock{Kind: runtimecontract.InputText, Role: runtimecontract.InputRoleDeveloper, Text: "runtime-contract-context-sentinel"})
	}
	if descriptor, ok := capabilityDescriptor(snapshot, runtimecontract.CapabilityImageInput); ok && descriptor.Availability == runtimecontract.CapabilityAvailable {
		imagePath := filepath.Join(t.TempDir(), "conformance.png")
		if err := os.WriteFile(imagePath, []byte("conformance image"), 0o600); err != nil {
			t.Fatal(err)
		}
		input = append(input, runtimecontract.InputBlock{Kind: runtimecontract.InputImage, Role: runtimecontract.InputRoleUser, Ref: imagePath, MIMEType: "image/png"})
	}
	started := contract.StartTurn(ctx, runtimecontract.TurnRequest{
		Binding: binding, TurnID: "turn-conformance", Input: input,
	})
	if err := runtimeLifecycleOutcomeError(started, runtimecontract.LifecycleAccepted, true); err != nil {
		t.Fatalf("StartTurn: %v (%#v)", err, started)
	}
	if fixture.emitAfterStart != nil {
		fixture.emitAfterStart(binding, started)
	}
	continued := contract.ContinueTurn(ctx, runtimecontract.CausalInput{Binding: binding, TurnID: "turn-conformance", RuntimeTurnRef: started.RuntimeTurnRef, Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: "continue"}}})
	if err := runtimeLifecycleOutcomeError(continued, runtimecontract.LifecycleAccepted, false); err != nil {
		t.Fatalf("ContinueTurn: %v (%#v)", err, continued)
	}
	interrupted := contract.InterruptTurn(ctx, runtimecontract.TurnTarget{Binding: binding, TurnID: "turn-conformance", RuntimeTurnRef: started.RuntimeTurnRef})
	if err := runtimeLifecycleOutcomeError(interrupted, runtimecontract.LifecycleInterrupted, false); err != nil {
		t.Fatalf("InterruptTurn: %v (%#v)", err, interrupted)
	}
	if fixture.emitAfterStop != nil {
		fixture.emitAfterStop(binding, started)
	}
	stream := collectConformanceStream(t, events)
	if stream[0].Kind != runtimecontract.EventTurnStarted || stream[len(stream)-1].Kind != runtimecontract.EventTerminal {
		t.Fatalf("ordered typed stream = %#v", stream)
	}
	terminalCount, contentCount := 0, 0
	for _, event := range stream {
		if err := event.Validate(); err != nil || event.TurnID != "turn-conformance" {
			t.Fatalf("stream event = %#v, err=%v", event, err)
		}
		if event.Kind == runtimecontract.EventTerminal {
			terminalCount++
			if event.Outcome == nil || event.Outcome.State != runtimecontract.LifecycleInterrupted {
				t.Fatalf("terminal event = %#v", event)
			}
		}
		if event.Kind == runtimecontract.EventContent {
			contentCount++
		}
	}
	if terminalCount != 1 || contentCount == 0 {
		t.Fatalf("stream terminal/content cardinality = %d/%d; events=%#v", terminalCount, contentCount, stream)
	}

	deadline := time.Now().Add(2 * time.Second)
	var observed runtimecontract.HistoryTurn
	for {
		history, failure := contract.ReadHistory(ctx, runtimecontract.HistoryRequest{Binding: binding, Count: 10})
		if failure == nil && history.Validate() == nil {
			for _, turn := range history.Turns {
				if turn.TurnID == "turn-conformance" && turn.State == runtimecontract.LifecycleInterrupted && len(turn.Content) > 0 {
					observed = turn
					goto historyReady
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("ReadHistory = %#v, failure=%#v", history, failure)
		}
		time.Sleep(20 * time.Millisecond)
	}

historyReady:
	if observed.RuntimeTurnRef != started.RuntimeTurnRef || !conformanceHistoryMatchesStream(observed.Content, stream) {
		t.Fatalf("history did not preserve stream correlation/content: turn=%#v stream=%#v", observed, stream)
	}
	if fixture.expectedUsageTotal > 0 && conformanceUsageTotal(observed.Usage) != fixture.expectedUsageTotal {
		t.Fatalf("history usage did not remain correlated with the streamed Turn: got %#v, want total=%d", observed.Usage, fixture.expectedUsageTotal)
	}
	if fixture.verifyEffects != nil {
		fixture.verifyEffects(binding, started)
	}

	var reopened runtimecontract.Outcome
	driver, handle, contract, reopened = fixture.reopen(ctx, handle, binding)
	if runtimeLifecycleOutcomeError(reopened, runtimecontract.LifecycleCompleted, false) != nil {
		t.Fatalf("restart ResumeBinding = %#v", reopened)
	}
	if seeder, ok := contract.(runtimeTurnCorrelationSeeder); ok {
		seeder.seedTurnBindings(map[string]string{"turn-conformance": started.RuntimeTurnRef})
	}
	reopenedHistory, failure := contract.ReadHistory(ctx, runtimecontract.HistoryRequest{Binding: binding, Count: 10})
	if failure != nil || reopenedHistory.Validate() != nil || !conformanceHistoryHasTurn(reopenedHistory, "turn-conformance", started.RuntimeTurnRef) {
		t.Fatalf("history after true reopen = %#v, failure=%#v", reopenedHistory, failure)
	}
	causalEvents := make(chan runtimecontract.Event, 64)
	contract.SetEventHandler(func(event runtimecontract.Event) { causalEvents <- event })
	causal := contract.StartTurn(ctx, runtimecontract.TurnRequest{
		Binding: binding, TurnID: "turn-after-reopen",
		Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: "causal action after reading the reopened predecessor"}},
	})
	if err := runtimeLifecycleOutcomeError(causal, runtimecontract.LifecycleAccepted, true); err != nil {
		t.Fatalf("causal action after reopen: %v (%#v)", err, causal)
	}
	if fixture.emitAfterReopenStart != nil {
		fixture.emitAfterReopenStart(binding, causal)
	}
	causal = contract.ContinueTurn(ctx, runtimecontract.CausalInput{
		Binding: binding, TurnID: "turn-after-reopen", PredecessorTurnID: "turn-conformance", RuntimeTurnRef: causal.RuntimeTurnRef,
		Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: "continue from the reopened predecessor"}},
	})
	if err := runtimeLifecycleOutcomeError(causal, runtimecontract.LifecycleAccepted, false); err != nil {
		t.Fatalf("causal continuation after reopen: %v (%#v)", err, causal)
	}
	causalTerminal := contract.InterruptTurn(ctx, runtimecontract.TurnTarget{Binding: binding, TurnID: "turn-after-reopen", RuntimeTurnRef: causal.RuntimeTurnRef})
	if err := runtimeLifecycleOutcomeError(causalTerminal, runtimecontract.LifecycleInterrupted, false); err != nil {
		t.Fatalf("causal interrupt after reopen: %v (%#v)", err, causalTerminal)
	}
	if fixture.emitAfterReopenStop != nil {
		fixture.emitAfterReopenStop(binding, causal)
	}
	causalStream := collectConformanceStream(t, causalEvents)
	causalContent, causalTerminalObserved := false, false
	for _, event := range causalStream {
		if event.Kind == runtimecontract.EventContent && event.PredecessorTurnID == "turn-conformance" {
			causalContent = true
		}
		if event.Kind == runtimecontract.EventTerminal {
			causalTerminalObserved = event.PredecessorTurnID == "turn-conformance"
		}
	}
	if !causalContent || !causalTerminalObserved {
		t.Fatalf("causal stream after reopen lost predecessor-correlated content/terminal: %#v", causalStream)
	}
	causalHistory, failure := contract.ReadHistory(ctx, runtimecontract.HistoryRequest{Binding: binding, Count: 20})
	causalTurn, causalFound := conformanceHistoryTurn(causalHistory, "turn-after-reopen", causal.RuntimeTurnRef)
	if failure != nil || causalHistory.Validate() != nil || !causalFound {
		t.Fatalf("causal history after true reopen = %#v, failure=%#v", causalHistory, failure)
	}
	if causalTurn.State != runtimecontract.LifecycleInterrupted || causalTurn.PredecessorTurnID != "turn-conformance" || !conformanceHistoryMatchesStream(causalTurn.Content, causalStream) {
		t.Fatalf("causal history lost interrupted predecessor/content correlation: turn=%#v stream=%#v", causalTurn, causalStream)
	}
	if streamedUsage := conformanceStreamUsage(causalStream); streamedUsage != nil && conformanceUsageTotal(causalTurn.Usage) != conformanceUsageTotal(streamedUsage) {
		t.Fatalf("causal history usage did not match stream: turn=%#v stream=%#v", causalTurn.Usage, streamedUsage)
	}
	if !conformanceHistoryHasTurn(causalHistory, "turn-conformance", started.RuntimeTurnRef) {
		t.Fatalf("causal history overwrote the reopened predecessor: %#v", causalHistory)
	}
	certifyMandatoryFailurePhases(t, contract, binding)
	closed := contract.CloseBinding(ctx, binding)
	if err := runtimeLifecycleOutcomeError(closed, runtimecontract.LifecycleCompleted, false); err != nil {
		t.Fatalf("CloseBinding: %v (%#v)", err, closed)
	}
	if again := contract.CloseBinding(ctx, binding); runtimeLifecycleOutcomeError(again, runtimecontract.LifecycleCompleted, false) != nil {
		t.Fatalf("idempotent CloseBinding = %#v", again)
	}
	if fixture.verifyClosed != nil {
		fixture.verifyClosed(handle)
	}
}

func conformanceHistoryMatchesStream(content []runtimecontract.ContentBlock, stream []runtimecontract.Event) bool {
	expected := ""
	for _, event := range stream {
		if event.Kind != runtimecontract.EventContent || event.Content == nil || event.Content.Kind != runtimecontract.ContentAssistantText {
			continue
		}
		if event.ContentPhase == runtimecontract.ContentPhaseCompleted {
			expected = event.Content.Text
		} else if event.ContentPhase == runtimecontract.ContentPhaseDelta {
			expected += event.Content.Text
		}
	}
	if expected == "" {
		return false
	}
	for _, block := range content {
		if block.Kind == runtimecontract.ContentAssistantText && block.Text == expected {
			return true
		}
	}
	return false
}

func conformanceUsageTotal(usage *runtimecontract.Usage) int64 {
	if usage == nil || !usage.TotalTokens.Available {
		return 0
	}
	return usage.TotalTokens.Value
}

func conformanceUsage(total int64) *runtimecontract.Usage {
	metric := runtimecontract.UsageMetric{Available: true, Value: total, Source: "runtime"}
	return &runtimecontract.Usage{TotalTokens: metric, Calls: runtimecontract.UsageMetric{Available: true, Value: 1, Source: "runtime"}}
}

func conformanceStreamUsage(stream []runtimecontract.Event) *runtimecontract.Usage {
	var usage *runtimecontract.Usage
	for _, event := range stream {
		if event.Kind == runtimecontract.EventUsage {
			usage = event.Usage
		}
	}
	return usage
}

func conformanceHistoryTurn(history runtimecontract.History, turnID, runtimeRef string) (runtimecontract.HistoryTurn, bool) {
	for _, turn := range history.Turns {
		if turn.TurnID == turnID && turn.RuntimeTurnRef == runtimeRef {
			return turn, true
		}
	}
	return runtimecontract.HistoryTurn{}, false
}

func conformanceHistoryHasTurn(history runtimecontract.History, turnID, runtimeRef string) bool {
	for _, turn := range history.Turns {
		if turn.TurnID == turnID && turn.RuntimeTurnRef == runtimeRef && len(turn.Content) > 0 {
			return true
		}
	}
	return false
}

func certifyMandatoryFailurePhases(t *testing.T, contract runtimecontract.Contract, binding runtimecontract.Binding) {
	t.Helper()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	turn := runtimecontract.TurnRequest{Binding: binding, TurnID: "turn-failure", Input: []runtimecontract.InputBlock{{Kind: runtimecontract.InputText, Text: "fail"}}}
	target := runtimecontract.TurnTarget{Binding: binding, TurnID: "turn-failure", RuntimeTurnRef: "native-failure"}
	causal := runtimecontract.CausalInput{Binding: binding, TurnID: target.TurnID, RuntimeTurnRef: target.RuntimeTurnRef, Input: turn.Input}
	_, create := contract.CreateBinding(cancelled, runtimecontract.BindingRequest{AgentID: "failure", Name: "failure", Cwd: "/tmp"})
	checks := []struct {
		phase   runtimecontract.FailurePhase
		outcome runtimecontract.Outcome
	}{
		{runtimecontract.FailurePhaseBindingCreate, create},
		{runtimecontract.FailurePhaseBindingResume, contract.ResumeBinding(cancelled, binding)},
		{runtimecontract.FailurePhaseTurnStart, contract.StartTurn(cancelled, turn)},
		{runtimecontract.FailurePhaseTurnContinue, contract.ContinueTurn(cancelled, causal)},
		{runtimecontract.FailurePhaseTurnInterrupt, contract.InterruptTurn(cancelled, target)},
		{runtimecontract.FailurePhaseClose, contract.CloseBinding(cancelled, binding)},
	}
	for _, check := range checks {
		if check.outcome.Failure == nil || check.outcome.Failure.Phase != check.phase || check.outcome.Validate() != nil {
			t.Fatalf("cancelled %s outcome = %#v", check.phase, check.outcome)
		}
	}
	if _, failure := contract.ReadHistory(cancelled, runtimecontract.HistoryRequest{Binding: binding, Count: 1}); failure == nil || failure.Phase != runtimecontract.FailurePhaseHistory || failure.Validate() != nil {
		t.Fatalf("cancelled history failure = %#v", failure)
	}
}

func collectConformanceStream(t *testing.T, events <-chan runtimecontract.Event) []runtimecontract.Event {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	stream := []runtimecontract.Event{}
	for {
		select {
		case event := <-events:
			stream = append(stream, event)
			if event.Kind == runtimecontract.EventTerminal {
				select {
				case duplicate := <-events:
					stream = append(stream, duplicate)
					t.Fatalf("event arrived after terminal: %#v", stream)
				case <-time.After(25 * time.Millisecond):
				}
				return stream
			}
		case <-deadline.C:
			t.Fatalf("typed stream never reached a terminal: %#v", stream)
		}
	}
}

func certifyAvailableRuntimeHooks(t *testing.T, contract runtimecontract.Contract, binding runtimecontract.Binding, snapshot runtimecontract.CapabilitySnapshot) {
	t.Helper()
	for _, descriptor := range snapshot.Capabilities {
		if descriptor.Availability != runtimecontract.CapabilityAvailable {
			if descriptor.Reason == "" || descriptor.Alternative == "" {
				t.Fatalf("unavailable capability %q has no reason/alternative", descriptor.ID)
			}
			continue
		}
		switch descriptor.ID {
		case runtimecontract.CapabilitySandboxConfiguration:
			contract.(runtimeSandboxConfiguration).SetRuntimeSandbox("danger-full-access")
		case runtimecontract.CapabilityProviderConfiguration:
			contract.(runtimeProviderConfiguration).SetRuntimeProvider("fixture-provider", "fixture-model")
		case runtimecontract.CapabilityApprovalPolicy:
			contract.(runtimeApprovalConfiguration).SetRuntimeApprovalPolicy("on-request")
			capability := contract.(runtimecontract.ApprovalCapability)
			capability.SetApprovalHandler(func(proposal runtimecontract.ApprovalProposal) {
				_ = capability.ResolveApproval(context.Background(), proposal.ID, runtimecontract.ApprovalApprove)
			})
		case runtimecontract.CapabilitySkillsPolicy:
			contract.(runtimeSkillsConfiguration).SetRuntimeDisabledSkills([]string{"/fixture/disabled-skill"})
		case runtimecontract.CapabilityContextDelivery:
			_ = contract.(runtimecontract.ContextDeliveryPolicy).ContextDeliveryMode()
		case runtimecontract.CapabilityNativeRename:
			if outcome := contract.(runtimecontract.BindingNameCapability).UpdateBindingName(context.Background(), binding, "conformance renamed"); runtimeLifecycleOutcomeError(outcome, runtimecontract.LifecycleCompleted, false) != nil {
				t.Fatalf("native rename = %#v", outcome)
			}
		case runtimecontract.CapabilityNativeArchive:
			if outcome := contract.(runtimecontract.BindingArchiveCapability).ArchiveBinding(context.Background(), binding); runtimeLifecycleOutcomeError(outcome, runtimecontract.LifecycleCompleted, false) != nil {
				t.Fatalf("native archive = %#v", outcome)
			}
		case runtimecontract.CapabilityGoal:
			capability := contract.(runtimeGoalCapability)
			objective := "conformance goal"
			if current, err := capability.RuntimeGoal(context.Background(), binding); err != nil || current == nil || current.Objective != "initial conformance goal" {
				t.Fatalf("goal read = %#v, err=%v", current, err)
			}
			if updated, err := capability.UpdateRuntimeGoal(context.Background(), binding, GoalUpdateParams{Objective: &objective}); err != nil || updated == nil || updated.Objective != objective {
				t.Fatalf("goal update = %#v, err=%v", updated, err)
			}
			if cleared, err := capability.ClearRuntimeGoal(context.Background(), binding); err != nil || !cleared {
				t.Fatalf("goal clear = %v, err=%v", cleared, err)
			}
		case runtimecontract.CapabilityUsageReporting:
			if report, err := contract.(runtimeUsageCapability).RuntimeUsage(context.Background(), binding); err != nil || report == nil || report.Lifetime.TotalTokens != 17 {
				t.Fatalf("usage report = %#v, err=%v", report, err)
			}
		case runtimecontract.CapabilityModelConfiguration:
			capability := contract.(runtimeModelCatalogCapability)
			state, err := capability.RuntimeModels(context.Background(), binding)
			if err != nil || len(state.Models) == 0 {
				t.Fatalf("model catalog = %#v, err=%v", state, err)
			}
			model := state.Models[len(state.Models)-1]
			selection := RuntimeModelSelection{Provider: model.Provider, Model: model.ID}
			if _, err := capability.SwitchRuntimeModel(context.Background(), binding, selection); err != nil {
				t.Fatalf("model switch: %v", err)
			}
		case runtimecontract.CapabilityManualCompaction:
			if err := contract.(runtimeCompactionCapability).CompactRuntimeBinding(context.Background(), binding); err != nil {
				t.Fatalf("manual compaction: %v", err)
			}
		case runtimecontract.CapabilityImageInput:
			if failure := contract.(runtimeInputCapability).ValidateRuntimeInput(context.Background(), binding, []runtimecontract.InputBlock{{Kind: runtimecontract.InputImage, Ref: "data:image/png;base64,iVBORw0KGgo=", MIMEType: "image/png"}}); failure != nil {
				t.Fatalf("image input rejected: %#v", failure)
			}
		default:
			t.Fatalf("available capability %q is not exercised by the shared conformance suite", descriptor.ID)
		}
	}
}

func newCodexConformanceFixture(t *testing.T) runtimeConformanceFixture {
	t.Helper()
	logPath := installFakeSharedCodexHost(t)
	rolloutDir := t.TempDir()
	writeTestRollout(t, rolloutDir, "thr-stale", time.Now().UTC().Format(time.RFC3339Nano))
	rolloutPath := filepath.Join(rolloutDir, "2026", "07", "08", "rollout-2026-07-08T10-00-00-thr-stale.jsonl")
	seed, err := os.OpenFile(rolloutPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = seed.WriteString(`{"timestamp":"2026-08-09T00:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":12,"output_tokens":5,"total_tokens":17},"last_token_usage":{"input_tokens":12,"output_tokens":5,"total_tokens":17},"model_context_window":1000}}}` + "\n")
	_ = seed.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SESSIONS_DIR", rolloutDir)
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	driver := newCodexRuntimeHostDriver(testHub(st))
	otherHandle, err := driver.Acquire(context.Background(), AgentHostRequest{AgentID: "codex-conformance-other"})
	if err != nil {
		t.Fatal(err)
	}
	return runtimeConformanceFixture{
		driver: driver, agentID: "codex-conformance", createCwd: "/tmp/one", expectedUsageTotal: 17,
		prepareBinding: func(binding runtimecontract.Binding) runtimecontract.Binding {
			binding.NativeRef = "thr-stale"
			return binding
		},
		emitAfterStart: func(binding runtimecontract.Binding, started runtimecontract.Outcome) {
			driver.dispatchNativeEvent("codex-conformance", "turn/started", []byte(`{"threadId":"thr-stale","turn":{"id":"`+started.RuntimeTurnRef+`","status":"inProgress"}}`))
		},
		emitAfterStop: func(binding runtimecontract.Binding, started runtimecontract.Outcome) {
			file, openErr := os.OpenFile(rolloutPath, os.O_APPEND|os.O_WRONLY, 0o600)
			if openErr != nil {
				t.Fatal(openErr)
			}
			_, writeErr := file.WriteString("\n" + `{"timestamp":"2026-08-10T00:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + started.RuntimeTurnRef + `"}}` + "\n" +
				`{"timestamp":"2026-08-10T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"hello"}}` + "\n" +
				`{"timestamp":"2026-08-10T00:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"continued"}}` + "\n" +
				`{"timestamp":"2026-08-10T00:00:02.500Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":24,"output_tokens":10,"total_tokens":34},"last_token_usage":{"input_tokens":12,"output_tokens":5,"total_tokens":17},"model_context_window":1000}}}` + "\n" +
				`{"timestamp":"2026-08-10T00:00:03Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"` + started.RuntimeTurnRef + `"}}` + "\n")
			_ = file.Close()
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			driver.dispatchNativeEvent("codex-conformance", "item/agentMessage/delta", []byte(`{"threadId":"thr-stale","turnId":"`+started.RuntimeTurnRef+`","itemId":"answer","delta":"continued"}`))
			driver.dispatchNativeEvent("codex-conformance", "turn/completed", []byte(`{"threadId":"thr-stale","turn":{"id":"`+started.RuntimeTurnRef+`","status":"interrupted"}}`))
		},
		emitAfterReopenStart: func(binding runtimecontract.Binding, started runtimecontract.Outcome) {
			driver.dispatchNativeEvent("codex-conformance", "turn/started", []byte(`{"threadId":"thr-stale","turn":{"id":"`+started.RuntimeTurnRef+`","status":"inProgress"}}`))
		},
		emitAfterReopenStop: func(binding runtimecontract.Binding, started runtimecontract.Outcome) {
			file, openErr := os.OpenFile(rolloutPath, os.O_APPEND|os.O_WRONLY, 0o600)
			if openErr != nil {
				t.Fatal(openErr)
			}
			_, writeErr := file.WriteString(`{"timestamp":"2026-08-10T00:01:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + started.RuntimeTurnRef + `"}}` + "\n" +
				`{"timestamp":"2026-08-10T00:01:01Z","type":"event_msg","payload":{"type":"user_message","message":"causal action after reading the reopened predecessor"}}` + "\n" +
				`{"timestamp":"2026-08-10T00:01:02Z","type":"event_msg","payload":{"type":"agent_message","message":"causal continued"}}` + "\n" +
				`{"timestamp":"2026-08-10T00:01:03Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"` + started.RuntimeTurnRef + `"}}` + "\n")
			_ = file.Close()
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			driver.dispatchNativeEvent("codex-conformance", "item/agentMessage/delta", []byte(`{"threadId":"thr-stale","turnId":"`+started.RuntimeTurnRef+`","itemId":"causal-answer","delta":"causal continued"}`))
			driver.dispatchNativeEvent("codex-conformance", "turn/completed", []byte(`{"threadId":"thr-stale","turn":{"id":"`+started.RuntimeTurnRef+`","status":"interrupted"}}`))
		},
		verifyEffects: func(runtimecontract.Binding, runtimecontract.Outcome) {
			driver.mu.Lock()
			active := driver.handles["codex-conformance"]
			driver.mu.Unlock()
			if active == nil || !active.contract.handleNativeServerRequest(active.host.client, []byte(`991`), "item/commandExecution/requestApproval", []byte(`{"toolName":"bash","command":"printf conformance"}`)) {
				t.Fatal("Codex typed Approval proposal was not routed")
			}
			deadline := time.Now().Add(time.Second)
			for {
				requests, readErr := os.ReadFile(logPath)
				if readErr == nil && strings.Contains(string(requests), `"decision":"accept"`) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("Codex typed Approval decision did not reach native response: %s, err=%v", requests, readErr)
				}
				time.Sleep(10 * time.Millisecond)
			}
			requests, readErr := os.ReadFile(logPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			value := string(requests)
			for _, expected := range []string{`"modelProvider":"fixture-provider"`, `"model":"fixture-model"`, `"sandbox":"danger-full-access"`, `/fixture/disabled-skill`, `"approvalPolicy":"on-request"`, `"sandboxPolicy":{"type":"dangerFullAccess"}`, `runtime-contract-context-sentinel`, `"type":"localImage"`, `conformance.png`} {
				if !strings.Contains(value, expected) {
					t.Fatalf("configured Codex capability did not reach native request %q: %s", expected, value)
				}
			}
			for _, method := range []string{"thread/name/set", "thread/archive", "thread/goal/get", "thread/goal/set", "thread/goal/clear", "thread/compact/start"} {
				if countRequestMethod(t, logPath, method) != 1 {
					t.Fatalf("Codex capability %s had no single observable native effect: %s", method, value)
				}
			}
		},
		reopen: func(ctx context.Context, handle AgentHost, binding runtimecontract.Binding) (RuntimeHostDriver, AgentHost, runtimecontract.Contract, runtimecontract.Outcome) {
			persistConformanceBinding(t, st, "codex-conformance", binding)
			handle.Close()
			handle.Close()
			otherHandle.Close()
			_ = driver.Shutdown(ctx)
			_ = driver.Shutdown(ctx)
			_ = st.Close()
			st = reopenConformanceStore(t, dataDir, "codex-conformance", binding)
			driver = newCodexRuntimeHostDriver(testHub(st))
			var acquireErr error
			otherHandle, acquireErr = driver.Acquire(ctx, AgentHostRequest{AgentID: "codex-conformance-other"})
			if acquireErr != nil {
				return driver, handle, handle.Contract(), conformanceFailure(acquireErr, runtimecontract.FailurePhaseBindingResume)
			}
			newHandle, acquireErr := driver.Acquire(ctx, AgentHostRequest{AgentID: "codex-conformance"})
			if acquireErr != nil {
				return driver, handle, handle.Contract(), conformanceFailure(acquireErr, runtimecontract.FailurePhaseBindingResume)
			}
			contract := newHandle.Contract()
			return driver, newHandle, contract, contract.ResumeBinding(ctx, binding)
		},
		verifyClosed: func(handle AgentHost) {
			if handle.Alive() {
				t.Fatal("Codex Agent handle remained alive after CloseBinding")
			}
			if !otherHandle.Alive() {
				t.Fatal("closing one Codex binding killed another Agent handle")
			}
		},
		cleanup: func() { otherHandle.Close(); _ = st.Close() },
	}
}

func newPiConformanceFixture(t *testing.T) runtimeConformanceFixture {
	t.Helper()
	configureFakePiHubRPC(t, "conformance")
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	driver := newPiRuntimeHostDriver(testHub(st))
	return runtimeConformanceFixture{
		driver: driver, agentID: "pi-conformance", expectedUsageTotal: 14,
		verifyEffects: func(runtimecontract.Binding, runtimecontract.Outcome) {
			waitForPiFile(t, os.Getenv("FAKE_PI_TOOL_EFFECT_FILE"))
			model, modelErr := os.ReadFile(os.Getenv("FAKE_PI_MODEL_FILE"))
			if modelErr != nil || strings.TrimSpace(string(model)) != "fixture/vision-next" {
				t.Fatalf("Pi model capability effect = %q, err=%v", model, modelErr)
			}
			prompt, promptErr := os.ReadFile(os.Getenv("FAKE_PI_PROMPT_JSON_FILE"))
			if promptErr != nil || !strings.Contains(string(prompt), `"images"`) || !strings.Contains(string(prompt), `"mimeType":"image/png"`) || !strings.Contains(string(prompt), `"data":"Y29uZm9ybWFuY2UgaW1hZ2U="`) || !strings.Contains(string(prompt), `runtime-contract-context-sentinel`) {
				t.Fatalf("Pi image capability effect = %s, err=%v", prompt, promptErr)
			}
		},
		reopen: func(ctx context.Context, handle AgentHost, binding runtimecontract.Binding) (RuntimeHostDriver, AgentHost, runtimecontract.Contract, runtimecontract.Outcome) {
			persistConformanceBinding(t, st, "pi-conformance", binding)
			handle.Close()
			handle.Close()
			_ = driver.Shutdown(ctx)
			_ = driver.Shutdown(ctx)
			_ = st.Close()
			st = reopenConformanceStore(t, dataDir, "pi-conformance", binding)
			driver = newPiRuntimeHostDriver(testHub(st))
			newHandle, acquireErr := driver.Acquire(ctx, AgentHostRequest{AgentID: "pi-conformance"})
			if acquireErr != nil {
				return driver, handle, handle.Contract(), conformanceFailure(acquireErr, runtimecontract.FailurePhaseBindingResume)
			}
			contract := newHandle.Contract()
			return driver, newHandle, contract, contract.ResumeBinding(ctx, binding)
		},
		verifyClosed: func(handle AgentHost) {
			if handle.Alive() {
				t.Fatal("Pi process remained alive after CloseBinding")
			}
		},
		cleanup: func() { _ = st.Close() },
	}
}

func newMinimalConformanceFixture(t *testing.T) runtimeConformanceFixture {
	t.Helper()
	driver := &minimalConformanceDriver{}
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return runtimeConformanceFixture{
		driver: driver, agentID: "minimal-conformance", expectedUsageTotal: 7,
		reopen: func(ctx context.Context, handle AgentHost, binding runtimecontract.Binding) (RuntimeHostDriver, AgentHost, runtimecontract.Contract, runtimecontract.Outcome) {
			persistConformanceBinding(t, st, "minimal-conformance", binding)
			history, _ := handle.Contract().ReadHistory(ctx, runtimecontract.HistoryRequest{Binding: binding, Count: 10})
			handle.Close()
			handle.Close()
			_ = driver.Shutdown(ctx)
			_ = driver.Shutdown(ctx)
			_ = st.Close()
			st = reopenConformanceStore(t, dataDir, "minimal-conformance", binding)
			driver = &minimalConformanceDriver{history: history}
			newHandle, acquireErr := driver.Acquire(ctx, AgentHostRequest{AgentID: "minimal-conformance"})
			if acquireErr != nil {
				return driver, handle, handle.Contract(), conformanceFailure(acquireErr, runtimecontract.FailurePhaseBindingResume)
			}
			contract := newHandle.Contract()
			return driver, newHandle, contract, contract.ResumeBinding(ctx, binding)
		},
		verifyClosed: func(handle AgentHost) {
			if handle.Alive() {
				t.Fatal("minimal Agent handle remained alive after CloseBinding")
			}
		},
		cleanup: func() { _ = st.Close() },
	}
}

func persistConformanceBinding(t *testing.T, st *store.Store, agentID string, binding runtimecontract.Binding) {
	t.Helper()
	agent := &Agent{
		ID: agentID, Name: agentID, ThreadID: "thread-" + agentID, Status: "idle",
		RuntimeBinding: RuntimeBinding{SchemaVersion: binding.SchemaVersion, Kind: binding.RuntimeKind, NativeRef: binding.NativeRef},
		CreatedAt:      now(), UpdatedAt: now(),
	}
	if err := st.SaveAgents(map[string]*Agent{agentID: agent}); err != nil {
		t.Fatal(err)
	}
}

func reopenConformanceStore(t *testing.T, dataDir, agentID string, binding runtimecontract.Binding) *store.Store {
	t.Helper()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string]*Agent{}
	if err := st.LoadAgents(&agents); err != nil {
		t.Fatal(err)
	}
	agent := agents[agentID]
	if agent == nil || agent.RuntimeBinding.SchemaVersion != binding.SchemaVersion || agent.RuntimeBinding.Kind != binding.RuntimeKind || agent.RuntimeBinding.NativeRef != binding.NativeRef {
		t.Fatalf("reopened Runtime binding changed: %#v", agent)
	}
	return st
}

func conformanceFailure(err error, phase runtimecontract.FailurePhase) runtimecontract.Outcome {
	return runtimecontract.Outcome{State: runtimecontract.LifecycleFailed, Failure: &runtimecontract.Failure{Code: "fixture_failure", Phase: phase, Message: err.Error(), Cause: err}}
}

type minimalConformanceContract struct {
	mu                sync.Mutex
	handler           func(runtimecontract.Event)
	history           runtimecontract.History
	predecessorByTurn map[string]string
	release           func()
}

func (c *minimalConformanceContract) ContractVersion() int { return runtimecontract.Version }
func (c *minimalConformanceContract) CreateBinding(ctx context.Context, _ runtimecontract.BindingRequest) (runtimecontract.Binding, runtimecontract.Outcome) {
	if failure := minimalConformanceContextFailure(ctx, runtimecontract.FailurePhaseBindingCreate); failure != nil {
		return runtimecontract.Binding{}, *failure
	}
	return runtimecontract.Binding{SchemaVersion: runtimecontract.BindingSchemaVersion, RuntimeKind: "fake", NativeRef: "fake-native"}, runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted}
}
func (c *minimalConformanceContract) ResumeBinding(ctx context.Context, _ runtimecontract.Binding) runtimecontract.Outcome {
	if failure := minimalConformanceContextFailure(ctx, runtimecontract.FailurePhaseBindingResume); failure != nil {
		return *failure
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}
func (c *minimalConformanceContract) UpdateBindingName(context.Context, runtimecontract.Binding, string) runtimecontract.Outcome {
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}
func (c *minimalConformanceContract) ArchiveBinding(context.Context, runtimecontract.Binding) runtimecontract.Outcome {
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}
func (c *minimalConformanceContract) StartTurn(ctx context.Context, request runtimecontract.TurnRequest) runtimecontract.Outcome {
	if failure := minimalConformanceContextFailure(ctx, runtimecontract.FailurePhaseTurnStart); failure != nil {
		return *failure
	}
	c.emit(runtimecontract.Event{Kind: runtimecontract.EventTurnStarted, TurnID: request.TurnID, RuntimeTurnRef: "fake-turn"})
	return runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: "fake-turn"}
}
func (c *minimalConformanceContract) ContinueTurn(ctx context.Context, request runtimecontract.CausalInput) runtimecontract.Outcome {
	if failure := minimalConformanceContextFailure(ctx, runtimecontract.FailurePhaseTurnContinue); failure != nil {
		return *failure
	}
	c.mu.Lock()
	if c.predecessorByTurn == nil {
		c.predecessorByTurn = map[string]string{}
	}
	c.predecessorByTurn[request.TurnID] = request.PredecessorTurnID
	c.mu.Unlock()
	c.emit(runtimecontract.Event{Kind: runtimecontract.EventContent, TurnID: request.TurnID, PredecessorTurnID: request.PredecessorTurnID, RuntimeTurnRef: request.RuntimeTurnRef, ContentPhase: runtimecontract.ContentPhaseCompleted, Content: &runtimecontract.ContentBlock{ID: "fake-answer", Kind: runtimecontract.ContentAssistantText, Text: "continued"}})
	return runtimecontract.Outcome{State: runtimecontract.LifecycleAccepted, RuntimeTurnRef: request.RuntimeTurnRef}
}
func (c *minimalConformanceContract) InterruptTurn(ctx context.Context, request runtimecontract.TurnTarget) runtimecontract.Outcome {
	if failure := minimalConformanceContextFailure(ctx, runtimecontract.FailurePhaseTurnInterrupt); failure != nil {
		return *failure
	}
	outcome := runtimecontract.Outcome{State: runtimecontract.LifecycleInterrupted, RuntimeTurnRef: request.RuntimeTurnRef}
	c.mu.Lock()
	predecessor := c.predecessorByTurn[request.TurnID]
	usage := conformanceUsage(7)
	c.history.Turns = append(c.history.Turns, runtimecontract.HistoryTurn{TurnID: request.TurnID, PredecessorTurnID: predecessor, RuntimeTurnRef: request.RuntimeTurnRef, State: runtimecontract.LifecycleInterrupted, Content: []runtimecontract.ContentBlock{{ID: "fake-answer", Kind: runtimecontract.ContentAssistantText, Text: "continued"}}, Usage: usage})
	c.history.Total = len(c.history.Turns)
	c.mu.Unlock()
	c.emit(runtimecontract.Event{Kind: runtimecontract.EventTerminal, TurnID: request.TurnID, PredecessorTurnID: predecessor, RuntimeTurnRef: request.RuntimeTurnRef, Outcome: &outcome})
	return outcome
}
func (c *minimalConformanceContract) SetEventHandler(handler func(runtimecontract.Event)) {
	c.mu.Lock()
	c.handler = handler
	c.mu.Unlock()
}
func (c *minimalConformanceContract) ReadHistory(ctx context.Context, _ runtimecontract.HistoryRequest) (runtimecontract.History, *runtimecontract.Failure) {
	if failure := minimalConformanceContextFailure(ctx, runtimecontract.FailurePhaseHistory); failure != nil {
		return runtimecontract.History{}, failure.Failure
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.history, nil
}

func minimalConformanceContextFailure(ctx context.Context, phase runtimecontract.FailurePhase) *runtimecontract.Outcome {
	if ctx.Err() == nil {
		return nil
	}
	return &runtimecontract.Outcome{State: runtimecontract.LifecycleFailed, Failure: &runtimecontract.Failure{Code: "context_cancelled", Phase: phase, Message: ctx.Err().Error(), Cause: ctx.Err()}}
}
func (c *minimalConformanceContract) CapabilitySnapshot(context.Context, runtimecontract.Binding) runtimecontract.CapabilitySnapshot {
	return controlPlaneCapabilitySnapshot("fake")
}
func (c *minimalConformanceContract) CloseBinding(ctx context.Context, _ runtimecontract.Binding) runtimecontract.Outcome {
	if failure := minimalConformanceContextFailure(ctx, runtimecontract.FailurePhaseClose); failure != nil {
		return *failure
	}
	if c.release != nil {
		c.release()
	}
	return runtimecontract.Outcome{State: runtimecontract.LifecycleCompleted}
}
func (c *minimalConformanceContract) emit(event runtimecontract.Event) {
	c.mu.Lock()
	handler := c.handler
	c.mu.Unlock()
	if handler != nil {
		handler(event)
	}
}

type minimalConformanceHost struct {
	mu       sync.Mutex
	alive    bool
	contract *minimalConformanceContract
}

func (h *minimalConformanceHost) Alive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.alive
}
func (h *minimalConformanceHost) Contract() runtimecontract.Contract { return h.contract }
func (h *minimalConformanceHost) SetFailureHandler(func(error))      {}
func (h *minimalConformanceHost) Close() {
	h.mu.Lock()
	h.alive = false
	h.mu.Unlock()
}

type minimalConformanceDriver struct {
	mu       sync.Mutex
	shutdown bool
	history  runtimecontract.History
}

func (d *minimalConformanceDriver) Preflight(context.Context) error { return nil }
func (d *minimalConformanceDriver) Acquire(context.Context, AgentHostRequest) (AgentHost, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.shutdown {
		return nil, errors.New("minimal Runtime Host Driver is shut down")
	}
	contract := &minimalConformanceContract{history: d.history}
	host := &minimalConformanceHost{alive: true, contract: contract}
	contract.release = host.Close
	return host, nil
}
func (d *minimalConformanceDriver) Shutdown(context.Context) error {
	d.mu.Lock()
	d.shutdown = true
	d.mu.Unlock()
	return nil
}
