package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yan5xu/codex-loom/internal/pi"
	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

var errPiNativeTurnNotFound = errors.New("Pi native Turn not found")

type piAgentRuntime struct {
	agentID string
	dataDir string
	apiURL  string

	mu                sync.Mutex
	rpc               *pi.RPC
	onEvent           func(nativeEvent)
	onFailure         func(error)
	onDiagnostic      func(json.RawMessage)
	onApproval        func(runtimecontract.ApprovalProposal)
	approvalResponses map[string]func(string) error
	developerContext  string
	approvalPolicy    string
	messageSequence   uint64
	currentMessage    uint64
	pendingTerminal   nativeEvent
	settled           chan struct{}
	activeNativeTurn  string
	abortRequested    bool
	imageInput        bool
	currentModel      RuntimeModel
	availableModels   []RuntimeModel
	thinkingLevel     string
}

func newPiAgentRuntime(agentID, dataDir, apiURL string) *piAgentRuntime {
	if strings.TrimSpace(apiURL) == "" {
		apiURL = "http://127.0.0.1:4870"
	}
	return &piAgentRuntime{agentID: agentID, dataDir: dataDir, apiURL: strings.TrimRight(apiURL, "/")}
}

func (r *piAgentRuntime) SetRuntimeEventHandlers(onEvent func(nativeEvent), onFailure func(error)) {
	r.mu.Lock()
	r.onEvent, r.onFailure = onEvent, onFailure
	r.mu.Unlock()
}

func (r *piAgentRuntime) SetRuntimeDiagnosticHandler(onDiagnostic func(json.RawMessage)) {
	r.mu.Lock()
	r.onDiagnostic = onDiagnostic
	r.mu.Unlock()
}

func (r *piAgentRuntime) SetApprovalHandler(onApproval func(runtimecontract.ApprovalProposal)) {
	r.mu.Lock()
	r.onApproval = onApproval
	r.mu.Unlock()
}

func (r *piAgentRuntime) ResolveApproval(_ context.Context, proposalID string, decision runtimecontract.ApprovalDecision) error {
	r.mu.Lock()
	respond := r.approvalResponses[proposalID]
	delete(r.approvalResponses, proposalID)
	r.mu.Unlock()
	if respond == nil {
		return fmt.Errorf("Runtime Approval proposal %s is unavailable", proposalID)
	}
	return respond(string(decision))
}

func (r *piAgentRuntime) Alive() bool {
	r.mu.Lock()
	rpc := r.rpc
	r.mu.Unlock()
	return rpc != nil && rpc.Alive()
}

func (r *piAgentRuntime) Create(request nativeBindingRequest) (string, error) {
	state, err := r.start(request, "")
	if err != nil {
		return "", err
	}
	if state.SessionFile == "" {
		return "", errors.New("Pi get_state returned no sessionFile")
	}
	return state.SessionFile, nil
}

func (r *piAgentRuntime) Resume(request nativeBindingRequest, timeout time.Duration) error {
	if r.Alive() {
		return nil
	}
	state, err := r.start(request, request.NativeRef)
	if err != nil {
		return err
	}
	if filepath.Clean(state.SessionFile) != filepath.Clean(request.NativeRef) {
		r.Close()
		return fmt.Errorf("Pi resumed session %q, expected %q", state.SessionFile, request.NativeRef)
	}
	return nil
}

type piSessionState struct {
	SessionFile   string          `json:"sessionFile"`
	SessionID     string          `json:"sessionId"`
	Model         *piRuntimeModel `json:"model,omitempty"`
	ThinkingLevel string          `json:"thinkingLevel"`
}

type piRuntimeModel struct {
	Provider         string             `json:"provider"`
	ID               string             `json:"id"`
	Input            []string           `json:"input"`
	ContextWindow    int                `json:"contextWindow"`
	Reasoning        bool               `json:"reasoning"`
	ThinkingLevelMap map[string]*string `json:"thinkingLevelMap"`
}

func (r *piAgentRuntime) start(request nativeBindingRequest, nativeRef string) (piSessionState, error) {
	sessionDir := filepath.Join(r.dataDir, "pi", r.agentID)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return piSessionState{}, fmt.Errorf("create Pi session directory: %w", err)
	}
	extensionPath, err := pi.MaterializeLoomExtension(r.dataDir)
	if err != nil {
		return piSessionState{}, err
	}
	args := []string{"--session-dir", sessionDir, "--approve", "--extension", extensionPath}
	if nativeRef == "" {
		args = append(args, "--session-id", r.agentID)
		if request.Name != "" {
			args = append(args, "--name", request.Name)
		}
	} else {
		args = append(args, "--session", nativeRef)
	}
	rpc, err := pi.SpawnRPC(pi.RPCOptions{
		Cwd: request.Cwd, Args: args,
		Env: map[string]string{
			"CODEX_LOOM_AGENT_ID": r.agentID, "CODEX_LOOM_API_URL": r.apiURL,
			// The process stays prepared to intercept side effects; the current
			// Turn policy below decides whether Loom needs Owner input.
			"CODEX_LOOM_APPROVAL_POLICY": "on-request",
		},
		OnEvent: r.handleEvent,
		OnFailure: func(err error) {
			r.mu.Lock()
			handler := r.onFailure
			r.mu.Unlock()
			if handler != nil {
				handler(err)
			}
		},
	})
	if err != nil {
		return piSessionState{}, err
	}
	r.mu.Lock()
	previous := r.rpc
	r.rpc = rpc
	r.mu.Unlock()
	if previous != nil {
		previous.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	response, err := rpc.Request(ctx, "get_state", nil)
	if err != nil {
		rpc.Close()
		return piSessionState{}, err
	}
	var state piSessionState
	if err := json.Unmarshal(response.Data, &state); err != nil || state.SessionFile == "" || state.SessionID == "" {
		rpc.Close()
		return piSessionState{}, fmt.Errorf("Pi get_state returned unsupported session state: %s", response.Data)
	}
	currentModel := RuntimeModel{}
	if state.Model != nil {
		currentModel = runtimeModel(*state.Model)
	}
	r.mu.Lock()
	r.imageInput = currentModel.ImageInput
	if state.Model != nil {
		r.currentModel = currentModel
	}
	r.thinkingLevel = state.ThinkingLevel
	r.mu.Unlock()
	return state, nil
}

func runtimeModel(model piRuntimeModel) RuntimeModel {
	imageInput := false
	for _, input := range model.Input {
		if input == "image" {
			imageInput = true
			break
		}
	}
	levels := []string{"off"}
	if model.Reasoning {
		levels = levels[:0]
		for _, level := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"} {
			mapped, present := model.ThinkingLevelMap[level]
			if (present && mapped == nil) || ((level == "xhigh" || level == "max") && !present) {
				continue
			}
			levels = append(levels, level)
		}
	}
	defaultLevel := levels[0]
	for _, level := range levels {
		if level == "medium" {
			defaultLevel = level
			break
		}
	}
	return RuntimeModel{Provider: model.Provider, ID: model.ID, ContextWindow: model.ContextWindow, Reasoning: model.Reasoning, ThinkingLevels: levels, DefaultThinkingLevel: defaultLevel, ImageInput: imageInput}
}

func (r *piAgentRuntime) models(timeout time.Duration) (RuntimeModelState, error) {
	r.mu.Lock()
	rpc, current, thinkingLevel := r.rpc, r.currentModel, r.thinkingLevel
	r.mu.Unlock()
	if rpc == nil {
		return RuntimeModelState{}, errors.New("Pi Runtime is not running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	response, err := rpc.Request(ctx, "get_available_models", nil)
	if err != nil {
		return RuntimeModelState{}, err
	}
	var data struct {
		Models []piRuntimeModel `json:"models"`
	}
	if err := json.Unmarshal(response.Data, &data); err != nil {
		return RuntimeModelState{}, fmt.Errorf("Pi get_available_models returned unsupported data: %w", err)
	}
	models := make([]RuntimeModel, 0, len(data.Models))
	for _, model := range data.Models {
		models = append(models, runtimeModel(model))
	}
	currentFound := false
	for _, model := range models {
		if model.Provider == current.Provider && model.ID == current.ID {
			current = model
			currentFound = true
			break
		}
	}
	if !currentFound && current.Provider != "" && current.ID != "" {
		level := thinkingLevel
		if level == "" {
			level = current.DefaultThinkingLevel
		}
		if level == "" {
			level = runtimecontract.ThinkingLevelDefault
		}
		foundLevel := false
		for _, candidate := range current.ThinkingLevels {
			if candidate == level {
				foundLevel = true
				break
			}
		}
		if !foundLevel {
			current.ThinkingLevels = append(current.ThinkingLevels, level)
		}
		if current.DefaultThinkingLevel == "" {
			current.DefaultThinkingLevel = level
		}
		models = append(models, current)
	}
	r.mu.Lock()
	r.availableModels = models
	r.currentModel = current
	r.imageInput = current.ImageInput
	r.mu.Unlock()
	return RuntimeModelState{Current: current, Models: models, ThinkingLevel: thinkingLevel}, nil
}

func (r *piAgentRuntime) resources(ctx context.Context) (runtimecontract.ResourceInventory, error) {
	r.mu.Lock()
	rpc := r.rpc
	r.mu.Unlock()
	if rpc == nil {
		return runtimecontract.ResourceInventory{}, errors.New("Pi Runtime is not running")
	}
	response, err := rpc.Request(ctx, "get_commands", nil)
	if err != nil {
		return runtimecontract.ResourceInventory{}, err
	}
	var data struct {
		Commands []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Source      string `json:"source"`
			Path        string `json:"path"`
			Location    string `json:"location"`
			SourceInfo  struct {
				Path   string `json:"path"`
				Source string `json:"source"`
				Scope  string `json:"scope"`
			} `json:"sourceInfo"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(response.Data, &data); err != nil {
		return runtimecontract.ResourceInventory{}, fmt.Errorf("Pi get_commands returned unsupported data: %w", err)
	}
	resources := make([]runtimecontract.Resource, 0, len(data.Commands))
	for _, command := range data.Commands {
		kind := runtimecontract.ResourceKind(command.Source)
		switch kind {
		case runtimecontract.ResourceSkill, runtimecontract.ResourcePrompt, runtimecontract.ResourceExtension:
		default:
			continue
		}
		name := strings.TrimPrefix(strings.TrimSpace(command.Name), "skill:")
		path := strings.TrimSpace(command.SourceInfo.Path)
		if path == "" {
			path = strings.TrimSpace(command.Path)
		}
		if name == "" || path == "" {
			continue
		}
		scope := command.SourceInfo.Scope
		if scope == "" {
			scope = command.Location
		}
		resources = append(resources, runtimecontract.Resource{
			ID: string(kind) + ":" + name + ":" + path, Name: name, Description: command.Description,
			Kind: kind, Path: path, Scope: scope, Source: command.SourceInfo.Source, Enabled: true,
		})
	}
	encoded, _ := json.Marshal(resources)
	return runtimecontract.ResourceInventory{
		Revision:  "pi:" + sha256Hex(encoded)[:16],
		Semantics: "Pi-native extensions, prompts, and skills; identifiers and paths are Runtime-specific",
		Resources: resources,
	}, nil
}

func (r *piAgentRuntime) switchModel(selection RuntimeModelSelection, timeout time.Duration) (RuntimeModelState, error) {
	preview, err := r.models(timeout)
	if err != nil {
		return RuntimeModelState{}, err
	}
	if err := preview.ValidateSelection(selection); err != nil {
		return RuntimeModelState{}, err
	}
	r.mu.Lock()
	rpc, current, currentThinking, imageInput := r.rpc, r.currentModel, r.thinkingLevel, r.imageInput
	r.mu.Unlock()
	if rpc == nil {
		return RuntimeModelState{}, errors.New("Pi Runtime is not running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	rollback := func(cause error) error {
		if current.Provider == "" || current.ID == "" {
			return cause
		}
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), timeout)
		defer rollbackCancel()
		if _, rollbackErr := rpc.Request(rollbackCtx, "set_model", map[string]any{"provider": current.Provider, "modelId": current.ID}); rollbackErr != nil {
			return piModelSelectionIndeterminate(fmt.Errorf("%v; restoring previous Pi model failed: %w", cause, rollbackErr))
		}
		if currentThinking != "" {
			if _, rollbackErr := rpc.Request(rollbackCtx, "set_thinking_level", map[string]any{"level": currentThinking}); rollbackErr != nil {
				return piModelSelectionIndeterminate(fmt.Errorf("%v; restoring previous Pi thinking level failed: %w", cause, rollbackErr))
			}
		}
		return cause
	}
	model := piRuntimeModel{Provider: current.Provider, ID: current.ID}
	modelChanged := current.Provider != selection.Provider || current.ID != selection.Model
	if modelChanged {
		response, err := rpc.Request(ctx, "set_model", map[string]any{"provider": selection.Provider, "modelId": selection.Model})
		if err != nil {
			return RuntimeModelState{}, piModelSelectionIndeterminate(fmt.Errorf("Pi set_model completion is unknown: %w", err))
		}
		if err := json.Unmarshal(response.Data, &model); err != nil || model.Provider == "" || model.ID == "" {
			return RuntimeModelState{}, rollback(fmt.Errorf("Pi set_model returned unsupported model: %s", response.Data))
		}
	}
	if selection.ThinkingLevel != "" {
		if _, err := rpc.Request(ctx, "set_thinking_level", map[string]any{"level": selection.ThinkingLevel}); err != nil {
			return RuntimeModelState{}, piModelSelectionIndeterminate(fmt.Errorf("Pi set_thinking_level completion is unknown: %w", err))
		}
	}
	nextModel := current
	if modelChanged {
		for _, candidate := range preview.Models {
			if candidate.Provider == selection.Provider && candidate.ID == selection.Model {
				nextModel = candidate
				break
			}
		}
		imageInput = nextModel.ImageInput
	}
	r.mu.Lock()
	r.currentModel = nextModel
	r.imageInput = imageInput
	if selection.ThinkingLevel != "" {
		r.thinkingLevel = selection.ThinkingLevel
	}
	thinkingLevel := r.thinkingLevel
	models := append([]RuntimeModel(nil), r.availableModels...)
	r.mu.Unlock()
	return RuntimeModelState{Current: nextModel, Models: models, ThinkingLevel: thinkingLevel}, nil
}

func piModelSelectionIndeterminate(cause error) error {
	return &runtimeIndeterminateError{failure: &runtimecontract.Failure{
		Code: "model_selection_indeterminate", Phase: runtimecontract.FailurePhaseModelControl,
		Message: "Pi model selection completion is indeterminate", Cause: cause,
	}}
}

func (r *piAgentRuntime) InjectDeveloperContext(_ string, content string, _ time.Duration) error {
	r.mu.Lock()
	r.developerContext = strings.TrimSpace(content)
	r.mu.Unlock()
	return nil
}

func (r *piAgentRuntime) StartTurn(request nativeTurnRequest) (string, error) {
	parts := make([]string, 0, len(request.Input)+1)
	type piImage struct {
		Type     string `json:"type"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	}
	images := make([]piImage, 0)
	r.mu.Lock()
	if r.developerContext != "" {
		parts = append(parts, r.developerContext)
		r.developerContext = ""
	}
	rpc := r.rpc
	imageInput := r.imageInput
	r.pendingTerminal = nativeEvent{}
	r.currentMessage = 0
	r.settled = make(chan struct{})
	r.abortRequested = false
	r.approvalPolicy = strings.ToLower(strings.TrimSpace(request.ApprovalPolicy))
	if r.approvalPolicy == "" {
		r.approvalPolicy = "never"
	}
	r.mu.Unlock()
	for _, input := range request.Input {
		switch input.Kind {
		case nativeInputText:
			if text := strings.TrimSpace(input.Text); text != "" {
				parts = append(parts, text)
			}
		case nativeInputLocalImage:
			if !imageInput {
				return "", errors.New("Pi Runtime active model does not support image input")
			}
			if !strings.HasPrefix(strings.ToLower(input.MimeType), "image/") {
				return "", fmt.Errorf("Pi image input %q has invalid MIME type %q", input.Path, input.MimeType)
			}
			data, err := os.ReadFile(input.Path)
			if err != nil {
				return "", fmt.Errorf("read Pi image input %q: %w", input.Path, err)
			}
			images = append(images, piImage{Type: "image", Data: base64.StdEncoding.EncodeToString(data), MimeType: input.MimeType})
		default:
			return "", fmt.Errorf("Pi Runtime does not support input kind %q", input.Kind)
		}
	}
	if rpc == nil || !rpc.Alive() {
		return "", errors.New("Pi RPC process is unavailable")
	}
	message := strings.Join(parts, "\n\n")
	if message == "" {
		return "", errors.New("Pi prompt text is required")
	}
	entriesBefore, leafBefore, err := r.piSessionEntries(request.NativeRef)
	if err != nil {
		return "", fmt.Errorf("snapshot Pi prompt entry: %w", err)
	}
	previousUserEntryID, err := latestPiUserEntryID(entriesBefore, leafBefore)
	if err != nil {
		return "", fmt.Errorf("snapshot Pi prompt entry: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), request.Timeout)
	defer cancel()
	prompt := map[string]any{"message": message}
	if len(images) > 0 {
		prompt["images"] = images
	}
	if _, err := rpc.Request(ctx, "prompt", prompt); err != nil {
		return "", err
	}
	var nativeUserEntryID string
	for nativeUserEntryID == "" || nativeUserEntryID == previousUserEntryID {
		entries, leafID, err := r.piSessionEntries(request.NativeRef)
		if err != nil {
			return "", fmt.Errorf("read Pi prompt entry: %w", err)
		}
		nativeUserEntryID, err = latestPiUserEntryID(entries, leafID)
		if err != nil {
			return "", fmt.Errorf("read Pi prompt entry: %w", err)
		}
		if nativeUserEntryID != "" && nativeUserEntryID != previousUserEntryID {
			break
		}
		select {
		case <-ctx.Done():
			return "", errPiPromptAcceptanceIndeterminate
		case <-time.After(25 * time.Millisecond):
		}
	}
	r.mu.Lock()
	if r.settled != nil {
		r.activeNativeTurn = nativeUserEntryID
	}
	r.mu.Unlock()
	return nativeUserEntryID, nil
}

func (r *piAgentRuntime) Steer(_ string, expectedNativeTurnID, input string, timeout time.Duration) (string, error) {
	r.mu.Lock()
	rpc := r.rpc
	activeNativeTurn := r.activeNativeTurn
	r.mu.Unlock()
	if rpc == nil || !rpc.Alive() {
		return "", errors.New("Pi RPC process is unavailable")
	}
	if expectedNativeTurnID == "" || activeNativeTurn != expectedNativeTurnID {
		return "", errors.New("Pi active native Turn changed before causal steer")
	}
	if strings.TrimSpace(input) == "" {
		return "", errors.New("Pi steer message is required")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := rpc.Request(ctx, "steer", map[string]any{"message": input}); err != nil {
		return "", err
	}
	return expectedNativeTurnID, nil
}

func (r *piAgentRuntime) Interrupt(_ string, _ string, timeout time.Duration) error {
	r.mu.Lock()
	rpc := r.rpc
	settled := r.settled
	if rpc != nil && settled != nil {
		r.abortRequested = true
	}
	r.mu.Unlock()
	if rpc == nil {
		return errors.New("Pi RPC process is unavailable")
	}
	if settled == nil {
		return errors.New("Pi Turn is not active")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := rpc.Request(ctx, "abort", nil); err != nil {
		return err
	}
	select {
	case <-settled:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for Pi aborted Turn settlement: %w", ctx.Err())
	}
}

func (r *piAgentRuntime) NormalizeEvent(_ string, raw json.RawMessage) []nativeEvent {
	r.mu.Lock()
	events, _ := r.normalizeEventLocked(raw)
	r.mu.Unlock()
	return events
}

func (r *piAgentRuntime) handleEvent(raw json.RawMessage) {
	r.mu.Lock()
	diagnostic := r.onDiagnostic
	r.mu.Unlock()
	if diagnostic != nil {
		diagnostic(append(json.RawMessage(nil), raw...))
	}
	if r.handleApprovalEvent(raw) {
		return
	}
	r.mu.Lock()
	events, terminal := r.normalizeEventLocked(raw)
	if terminal.Kind != "" {
		r.pendingTerminal = terminal
	}
	var typeOnly struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &typeOnly)
	var settled chan struct{}
	if typeOnly.Type == "agent_settled" {
		terminal = r.pendingTerminal
		r.pendingTerminal = nativeEvent{}
		if r.abortRequested && terminal.Kind != nativeTurnInterrupted {
			terminal = nativeEvent{Kind: nativeTurnFailed, Error: "Pi settled an aborted Turn without a final aborted assistant state"}
		} else if terminal.Kind == "" {
			terminal.Kind = nativeTurnCompleted
		}
		events = append(events, terminal)
		settled = r.settled
		r.settled = nil
		r.activeNativeTurn = ""
		r.abortRequested = false
	}
	handler := r.onEvent
	r.mu.Unlock()
	if handler != nil {
		for _, event := range events {
			handler(event)
		}
	}
	if settled != nil {
		close(settled)
	}
}

func (r *piAgentRuntime) handleApprovalEvent(raw json.RawMessage) bool {
	var envelope struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		Method      string `json:"method"`
		Title       string `json:"title"`
		Placeholder string `json:"placeholder"`
		Timeout     int64  `json:"timeout"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Type != "extension_ui_request" ||
		envelope.Method != "input" || envelope.Title != "codex-loom:approval:v1" {
		return false
	}
	var payload struct {
		Version    int             `json:"version"`
		Operation  string          `json:"operation"`
		ToolCallID string          `json:"toolCallId"`
		ToolName   string          `json:"toolName"`
		Input      json.RawMessage `json:"input"`
	}
	valid := json.Unmarshal([]byte(envelope.Placeholder), &payload) == nil && payload.Version == 1 &&
		payload.Operation == "request_approval" && strings.TrimSpace(payload.ToolCallID) != "" && strings.TrimSpace(payload.ToolName) != ""
	r.mu.Lock()
	rpc, policy, handler, failure := r.rpc, r.approvalPolicy, r.onApproval, r.onFailure
	r.mu.Unlock()
	respond := func(decision string) error {
		if rpc == nil {
			return errors.New("Pi RPC process is unavailable")
		}
		return rpc.RespondExtensionUI(pi.ExtensionUIResponse{ID: envelope.ID, Value: decision})
	}
	if !valid || envelope.ID == "" {
		if err := respond("abort"); err != nil && failure != nil {
			failure(err)
		}
		return true
	}
	if policy == "never" {
		if err := respond("approve"); err != nil && failure != nil {
			failure(err)
		}
		return true
	}
	if handler == nil {
		if err := respond("abort"); err != nil && failure != nil {
			failure(err)
		}
		return true
	}
	timeout := time.Duration(envelope.Timeout) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	proposalID := "runtime-approval-" + strings.TrimPrefix(newIntegrationID("proposal"), "proposal_")
	r.mu.Lock()
	if r.approvalResponses == nil {
		r.approvalResponses = map[string]func(string) error{}
	}
	r.approvalResponses[proposalID] = respond
	r.mu.Unlock()
	handler(runtimecontract.ApprovalProposal{
		ID: proposalID, ToolName: payload.ToolName, Action: "tool/" + payload.ToolName,
		Arguments: piApprovalArguments(envelope.Placeholder), Timeout: timeout,
	})
	return true
}

func piApprovalArguments(raw string) []runtimecontract.ApprovalArgument {
	var input map[string]any
	if json.Unmarshal([]byte(raw), &input) != nil {
		return nil
	}
	if nested, ok := input["input"].(string); ok {
		var actionable map[string]any
		if json.Unmarshal([]byte(nested), &actionable) == nil {
			for key, value := range actionable {
				input[key] = value
			}
			delete(input, "input")
		}
	}
	if actionable, ok := input["input"].(map[string]any); ok {
		for key, value := range actionable {
			input[key] = value
		}
		delete(input, "input")
	}
	arguments := make([]runtimecontract.ApprovalArgument, 0, len(input))
	for key, value := range input {
		if !approvalActionKey(key) {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			continue
		}
		arguments = append(arguments, runtimecontract.ApprovalArgument{Name: key, Value: strings.Trim(string(encoded), `"`)})
	}
	sort.Slice(arguments, func(i, j int) bool { return arguments[i].Name < arguments[j].Name })
	return arguments
}

func (r *piAgentRuntime) normalizeEventLocked(raw json.RawMessage) ([]nativeEvent, nativeEvent) {
	var envelope struct {
		Type    string `json:"type"`
		Message struct {
			Role         string `json:"role"`
			StopReason   string `json:"stopReason"`
			ErrorMessage string `json:"errorMessage"`
			Content      []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"content"`
		} `json:"message"`
		AssistantMessageEvent struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		} `json:"assistantMessageEvent"`
		ToolCallID    string         `json:"toolCallId"`
		ToolName      string         `json:"toolName"`
		Args          map[string]any `json:"args"`
		PartialResult map[string]any `json:"partialResult"`
		Result        map[string]any `json:"result"`
		IsError       bool           `json:"isError"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, nativeEvent{Kind: nativeTurnFailed, Error: "Pi emitted malformed event"}
	}
	switch envelope.Type {
	case "agent_start":
		return []nativeEvent{{Kind: nativeTurnStarted}}, nativeEvent{}
	case "message_start":
		if envelope.Message.Role == "assistant" {
			r.messageSequence++
			r.currentMessage = r.messageSequence
		}
		return nil, nativeEvent{}
	case "message_update":
		sequence := r.ensureCurrentMessageLocked()
		switch envelope.AssistantMessageEvent.Type {
		case "text_delta":
			return []nativeEvent{{Kind: nativeTextDelta, ItemID: piMessageItemID(sequence), Text: envelope.AssistantMessageEvent.Delta}}, nativeEvent{}
		case "thinking_delta":
			return []nativeEvent{{Kind: nativeReasoningDelta, ItemID: piReasoningItemID(sequence), Text: envelope.AssistantMessageEvent.Delta}}, nativeEvent{}
		default:
			return nil, nativeEvent{}
		}
	case "tool_execution_start", "tool_execution_update", "tool_execution_end":
		if envelope.ToolCallID == "" || envelope.ToolName == "" {
			return nil, nativeEvent{}
		}
		kind, status, result := nativeToolStarted, "running", map[string]any(nil)
		switch envelope.Type {
		case "tool_execution_update":
			kind, result = nativeToolUpdated, envelope.PartialResult
		case "tool_execution_end":
			kind, result = nativeToolCompleted, envelope.Result
			status = "completed"
			if envelope.IsError {
				status = "failed"
			}
		}
		output := piResultText(result)
		item := map[string]any{
			"id": envelope.ToolCallID, "type": "commandExecution", "command": piToolCommand(envelope.ToolName, envelope.Args),
			"toolName": envelope.ToolName, "args": envelope.Args, "status": status, "aggregatedOutput": output,
		}
		if envelope.IsError {
			item["error"] = output
		}
		return []nativeEvent{{Kind: kind, ItemID: envelope.ToolCallID, Item: item, Status: status, Error: func() string {
			if envelope.IsError {
				return output
			}
			return ""
		}()}}, nativeEvent{}
	case "message_end":
		if envelope.Message.Role != "assistant" {
			return nil, nativeEvent{}
		}
		sequence := r.ensureCurrentMessageLocked()
		r.currentMessage = 0
		var text, reasoning []string
		for _, content := range envelope.Message.Content {
			switch content.Type {
			case "text":
				text = append(text, content.Text)
			case "thinking":
				reasoning = append(reasoning, content.Thinking)
			}
		}
		answer := strings.Join(text, "")
		events := []nativeEvent{}
		if thought := strings.Join(reasoning, ""); thought != "" {
			itemID := piReasoningItemID(sequence)
			events = append(events, nativeEvent{
				Kind: nativeReasoningCompleted, ItemID: itemID, Text: thought,
				Item: map[string]any{"id": itemID, "type": "reasoning", "text": thought},
			})
		}
		if answer != "" {
			itemID := piMessageItemID(sequence)
			events = append(events, nativeEvent{
				Kind: nativeTextCompleted, ItemID: itemID, Text: answer,
				Item: map[string]any{"id": itemID, "type": "agentMessage", "text": answer},
			})
		}
		switch envelope.Message.StopReason {
		case "error":
			message := strings.TrimSpace(envelope.Message.ErrorMessage)
			if message == "" {
				message = "Pi assistant message ended with an error"
			}
			return events, nativeEvent{Kind: nativeTurnFailed, Error: message}
		case "aborted":
			return events, nativeEvent{Kind: nativeTurnInterrupted, Error: "Pi Turn aborted"}
		default:
			return events, nativeEvent{Kind: nativeTurnCompleted}
		}
	default:
		return nil, nativeEvent{}
	}
}

func (r *piAgentRuntime) ensureCurrentMessageLocked() uint64 {
	if r.currentMessage == 0 {
		r.messageSequence++
		r.currentMessage = r.messageSequence
	}
	return r.currentMessage
}

func piMessageItemID(sequence uint64) string { return fmt.Sprintf("pi-message-%d", sequence) }

func piReasoningItemID(sequence uint64) string { return fmt.Sprintf("pi-reasoning-%d", sequence) }

func piToolCommand(name string, args map[string]any) string {
	if name == "bash" {
		if command, _ := args["command"].(string); command != "" {
			return command
		}
	}
	raw, _ := json.Marshal(args)
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return name
	}
	return name + " " + string(raw)
}

func piResultText(result map[string]any) string {
	content, _ := result["content"].([]any)
	var parts []string
	for _, value := range content {
		item, _ := value.(map[string]any)
		if item["type"] == "text" {
			if text, _ := item["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "")
}

func (r *piAgentRuntime) ReadHistory(nativeRef string, count, offset int) (nativeHistory, error) {
	entries, leafID, err := r.piSessionEntries(nativeRef)
	if err != nil {
		return nativeHistory{}, err
	}
	history, err := projectPiHistory(entries, leafID)
	if err != nil {
		return nativeHistory{}, err
	}
	if count <= 0 {
		count = 10
	}
	if offset < 0 {
		offset = 0
	}
	end := history.Total - offset
	if end < 0 {
		end = 0
	}
	start := end - count
	if start < 0 {
		start = 0
	}
	history.Turns = history.Turns[start:end]
	return history, nil
}

func (r *piAgentRuntime) ReadTurn(nativeRef, nativeTurnID string) (nativeHistoryTurn, error) {
	entries, leafID, err := r.piSessionEntries(nativeRef)
	if err != nil {
		return nativeHistoryTurn{}, err
	}
	history, err := projectPiHistory(entries, leafID)
	if err != nil {
		return nativeHistoryTurn{}, err
	}
	for _, turn := range history.Turns {
		if turn.ID == nativeTurnID {
			return turn, nil
		}
	}
	return nativeHistoryTurn{}, fmt.Errorf("%w: %s", errPiNativeTurnNotFound, nativeTurnID)
}

func (r *piAgentRuntime) LatestTurn(nativeRef string) (*nativeHistoryTurn, error) {
	history, err := r.ReadHistory(nativeRef, 1, 0)
	if err != nil || len(history.Turns) == 0 {
		return nil, err
	}
	turn := history.Turns[0]
	return &turn, nil
}

func (r *piAgentRuntime) Close() {
	r.mu.Lock()
	rpc := r.rpc
	r.rpc = nil
	r.mu.Unlock()
	if rpc != nil {
		rpc.Close()
	}
}
