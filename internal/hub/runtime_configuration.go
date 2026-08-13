package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

const (
	RuntimeAuthConsole      = "console"
	RuntimeAuthSubscription = "subscription"
	RuntimeAuthCloud        = "cloud"
	RuntimeAuthGateway      = "gateway"
)

type RuntimeAuthentication struct {
	Category string `json:"category"`
	Source   string `json:"source"`
}

type RuntimeConfiguration struct {
	Configured     bool                  `json:"configured"`
	SettingSources []string              `json:"settingSources"`
	Authentication RuntimeAuthentication `json:"authentication,omitempty"`
}

type RuntimeAuthenticationEvidence struct {
	Category   string                               `json:"category"`
	Source     string                               `json:"source"`
	Validation string                               `json:"validation"`
	Evidence   []runtimecontract.CapabilityEvidence `json:"evidence,omitempty"`
}

type RuntimeConfigurationEvidence struct {
	SettingSources []string                      `json:"settingSources"`
	Authentication RuntimeAuthenticationEvidence `json:"authentication"`
}

type runtimeConfigurationEvidenceProvider interface {
	RuntimeConfigurationEvidence() (RuntimeConfigurationEvidence, bool)
}

type runtimeOwnerConfigurationInspector interface {
	InspectRuntimeOwnerConfiguration(context.Context, runtimecontract.Binding, string, RuntimeConfiguration) (RuntimeConfigurationEvidence, *runtimecontract.Failure)
}

type RuntimeConfigurationView struct {
	Configuration RuntimeConfiguration           `json:"configuration"`
	Evidence      RuntimeConfigurationEvidence   `json:"evidence"`
	Revision      string                         `json:"revision"`
	Descriptor    RuntimeConfigurationDescriptor `json:"descriptor"`
}

type RuntimeConfigurationParams struct {
	Configuration    RuntimeConfiguration `json:"configuration"`
	ExpectedRevision string               `json:"expectedRevision"`
}

type RuntimeConfigurationOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type RuntimeAuthenticationOption struct {
	Category    string                       `json:"category"`
	Label       string                       `json:"label"`
	Description string                       `json:"description,omitempty"`
	Sources     []RuntimeConfigurationOption `json:"sources"`
}

type RuntimeConfigurationDescriptor struct {
	SettingSources []RuntimeConfigurationOption  `json:"settingSources"`
	Authentication []RuntimeAuthenticationOption `json:"authentication"`
	Default        RuntimeConfiguration          `json:"default"`
}

type runtimeConfigurationDescriptorProvider interface {
	RuntimeConfigurationDescriptor() RuntimeConfigurationDescriptor
}

func claudeRuntimeConfigurationDescriptor() RuntimeConfigurationDescriptor {
	return RuntimeConfigurationDescriptor{
		SettingSources: []RuntimeConfigurationOption{
			{ID: "user", Label: "User", Description: "Your Claude user settings"},
			{ID: "project", Label: "Project", Description: "Shared project settings"},
			{ID: "local", Label: "Local", Description: "Local project overrides"},
		},
		Authentication: []RuntimeAuthenticationOption{
			{
				Category:    RuntimeAuthSubscription,
				Label:       "Claude subscription",
				Description: "Uses this Owner's existing local Claude.ai login for development. CodexLoom does not copy or store the OAuth credential.",
				Sources:     []RuntimeConfigurationOption{{ID: "claude_ai", Label: "Local Claude.ai login (OAuth)"}},
			},
			{
				Category: RuntimeAuthConsole,
				Label:    "Claude Console",
				Description: "API keys are supported. Claude credential helpers cannot be selected or verified through the public Agent SDK, " +
					"so helper authentication is unavailable.",
				Sources: []RuntimeConfigurationOption{{ID: "api_key", Label: "API key"}},
			},
			{Category: RuntimeAuthCloud, Label: "Cloud provider", Sources: []RuntimeConfigurationOption{
				{ID: "bedrock", Label: "Amazon Bedrock"}, {ID: "vertex", Label: "Google Vertex AI"}, {ID: "foundry", Label: "Microsoft Foundry"},
				{ID: "anthropic_aws", Label: "Anthropic on AWS"}, {ID: "anthropic_google_cloud", Label: "Anthropic on Google Cloud"}, {ID: "mantle", Label: "Mantle"},
			}},
			{Category: RuntimeAuthGateway, Label: "Gateway", Sources: []RuntimeConfigurationOption{{ID: "gateway", Label: "Managed gateway"}}},
		},
		Default: legacyClaudeRuntimeConfiguration(),
	}
}

func legacyClaudeRuntimeConfiguration() RuntimeConfiguration {
	return RuntimeConfiguration{
		Configured:     true,
		SettingSources: []string{"user", "project", "local"},
		Authentication: RuntimeAuthentication{Category: RuntimeAuthConsole, Source: "api_key"},
	}
}

func validateRuntimeConfigurationEvidence(configuration RuntimeConfiguration, evidence RuntimeConfigurationEvidence) error {
	if !configuration.Configured {
		return fmt.Errorf("Claude Runtime owner configuration is not configured")
	}
	if evidence.Authentication.Validation != "accepted" {
		return fmt.Errorf("Claude Runtime authentication was not accepted")
	}
	if evidence.Authentication.Category != configuration.Authentication.Category || evidence.Authentication.Source != configuration.Authentication.Source {
		return fmt.Errorf("Claude Runtime authentication evidence does not match the selected configuration")
	}
	if strings.Join(evidence.SettingSources, "\x00") != strings.Join(configuration.SettingSources, "\x00") {
		return fmt.Errorf("Claude Runtime setting source evidence does not match the selected configuration")
	}
	return nil
}

func normalizeRuntimeConfiguration(runtimeKind string, configuration RuntimeConfiguration) (RuntimeConfiguration, error) {
	if runtimeKind != "claude" {
		if configuration.Configured || len(configuration.SettingSources) != 0 || configuration.Authentication.Category != "" || configuration.Authentication.Source != "" {
			return RuntimeConfiguration{}, errf(400, "this Runtime does not accept Claude setting or authentication configuration")
		}
		return RuntimeConfiguration{}, nil
	}
	if !configuration.Configured {
		return RuntimeConfiguration{}, errf(400, "Claude Runtime configuration must explicitly select settingSources and authentication")
	}
	if len(configuration.SettingSources) == 0 {
		return RuntimeConfiguration{}, errf(400, "Claude settingSources must select at least one source")
	}
	sources := make([]string, len(configuration.SettingSources))
	seen := map[string]bool{}
	for index, rawSource := range configuration.SettingSources {
		source := strings.TrimSpace(rawSource)
		if source != "user" && source != "project" && source != "local" {
			return RuntimeConfiguration{}, errf(400, "Claude settingSources must contain only user, project, or local")
		}
		if seen[source] {
			return RuntimeConfiguration{}, errf(400, "Claude settingSources contains duplicate %q", source)
		}
		seen[source] = true
		sources[index] = source
	}
	order := map[string]int{"user": 0, "project": 1, "local": 2}
	sort.SliceStable(sources, func(i, j int) bool {
		return order[sources[i]] < order[sources[j]]
	})
	auth := configuration.Authentication
	auth.Category, auth.Source = strings.TrimSpace(auth.Category), strings.TrimSpace(auth.Source)
	switch auth.Category {
	case RuntimeAuthSubscription:
		if auth.Source != "claude_ai" {
			return RuntimeConfiguration{}, errf(400, "Claude subscription authentication source must be claude_ai")
		}
	case RuntimeAuthConsole:
		if auth.Source != "api_key" {
			return RuntimeConfiguration{}, errf(400, "Claude Console authentication source must be api_key")
		}
	case RuntimeAuthCloud:
		switch auth.Source {
		case "bedrock", "vertex", "foundry", "anthropic_aws", "anthropic_google_cloud", "mantle":
		default:
			return RuntimeConfiguration{}, errf(400, "Claude cloud authentication source is unsupported")
		}
	case RuntimeAuthGateway:
		if auth.Source != "gateway" {
			return RuntimeConfiguration{}, errf(400, "Claude gateway authentication source must be gateway")
		}
	default:
		return RuntimeConfiguration{}, errf(400, "Claude authentication category must be subscription, console, cloud, or gateway")
	}
	return RuntimeConfiguration{Configured: true, SettingSources: sources, Authentication: auth}, nil
}

func runtimeConfigurationRevision(meta *Agent) string {
	if meta == nil {
		return ""
	}
	encoded, _ := json.Marshal(struct {
		Binding       runtimecontract.Binding `json:"binding"`
		Cwd           string                  `json:"cwd"`
		Configuration RuntimeConfiguration    `json:"configuration"`
	}{runtimeContractBinding(meta), meta.Cwd, meta.RuntimeConfiguration})
	return "runtime-config:" + sha256Hex(encoded)[:16]
}

func (h *Hub) inspectRuntimeConfiguration(key string, requested *RuntimeConfiguration) (RuntimeConfigurationView, error) {
	h.mu.Lock()
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return RuntimeConfigurationView{}, errf(404, "agent not found: %s", key)
	}
	if h.runtimeConfigurationApplying[meta.ID] {
		h.mu.Unlock()
		return RuntimeConfigurationView{}, errf(409, "Runtime configuration is being applied; retry after it settles")
	}
	agentID, cwd, binding := meta.ID, meta.Cwd, runtimeContractBinding(meta)
	driver, driverErr := h.runtimeHostDriverLocked(binding.RuntimeKind)
	if driverErr != nil {
		h.mu.Unlock()
		return RuntimeConfigurationView{}, driverErr
	}
	descriptorProvider, ok := driver.(runtimeConfigurationDescriptorProvider)
	if !ok {
		h.mu.Unlock()
		return RuntimeConfigurationView{}, errf(409, "this Runtime does not expose owner configuration")
	}
	descriptor := descriptorProvider.RuntimeConfigurationDescriptor()
	configuration := meta.RuntimeConfiguration
	if requested != nil {
		configuration = *requested
	}
	revision := runtimeConfigurationRevision(meta)
	rt, err := h.getRuntimeLocked(meta)
	h.mu.Unlock()
	if err != nil {
		return RuntimeConfigurationView{}, err
	}
	rt.startMu.Lock()
	defer rt.startMu.Unlock()
	if err := waitReady(rt); err != nil {
		return RuntimeConfigurationView{}, errf(500, "Runtime not ready: %s", err)
	}
	inspector, ok := rt.runtimeContract.(runtimeOwnerConfigurationInspector)
	if !ok {
		return RuntimeConfigurationView{}, errf(409, "this Runtime does not expose owner configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	evidence, failure := inspector.InspectRuntimeOwnerConfiguration(ctx, binding, cwd, configuration)
	cancel()
	if failure != nil {
		return RuntimeConfigurationView{}, errf(409, "validate Runtime configuration: %s", failure.Message)
	}
	if err := validateRuntimeConfigurationEvidence(configuration, evidence); err != nil {
		return RuntimeConfigurationView{}, errf(500, "invalid Runtime configuration evidence: %s", err)
	}
	h.mu.Lock()
	current := h.agents[agentID]
	stale := current == nil || current.Cwd != cwd || runtimeContractBinding(current) != binding || runtimeConfigurationRevision(current) != revision || h.runtimes[agentID] != rt
	h.mu.Unlock()
	if stale {
		return RuntimeConfigurationView{}, errf(409, "Agent Runtime configuration changed while it was inspected; retry")
	}
	configuration.SettingSources = append([]string(nil), configuration.SettingSources...)
	return RuntimeConfigurationView{Configuration: configuration, Evidence: evidence, Revision: revision, Descriptor: descriptor}, nil
}

func (h *Hub) GetRuntimeConfiguration(key string) (RuntimeConfigurationView, error) {
	return h.inspectRuntimeConfiguration(key, nil)
}

func (h *Hub) UpdateRuntimeConfiguration(key string, params RuntimeConfigurationParams) (RuntimeConfigurationView, error) {
	if strings.TrimSpace(params.ExpectedRevision) == "" {
		return RuntimeConfigurationView{}, errf(400, "expectedRevision is required")
	}
	h.runtimeConfigurationMu.Lock()
	defer h.runtimeConfigurationMu.Unlock()
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		return RuntimeConfigurationView{}, errf(409, "CodexLoom is shutting down; Runtime configuration cannot be changed")
	}
	meta := h.resolveLocked(key)
	if meta == nil {
		h.mu.Unlock()
		return RuntimeConfigurationView{}, errf(404, "agent not found: %s", key)
	}
	next, err := normalizeRuntimeConfiguration(meta.RuntimeBinding.Kind, params.Configuration)
	if err != nil {
		h.mu.Unlock()
		return RuntimeConfigurationView{}, err
	}
	if meta.Status == "running" {
		h.mu.Unlock()
		return RuntimeConfigurationView{}, errf(409, "agent %q is running; change Runtime configuration between Turns", meta.Name)
	}
	if meta.Source == "edge" {
		h.mu.Unlock()
		return RuntimeConfigurationView{}, errf(409, "edge Agent %q must be adopted before configuring its Runtime", meta.Name)
	}
	if revision := runtimeConfigurationRevision(meta); revision != params.ExpectedRevision {
		h.mu.Unlock()
		return RuntimeConfigurationView{}, errf(409, "Runtime configuration snapshot is stale; reopen configuration and retry")
	}
	if h.runtimeConfigurationApplying[meta.ID] {
		h.mu.Unlock()
		return RuntimeConfigurationView{}, errf(409, "Runtime configuration is already being applied")
	}
	if h.runtimeConfigurationApplying == nil {
		h.runtimeConfigurationApplying = map[string]bool{}
	}
	h.runtimeConfigurationApplying[meta.ID] = true
	agentID := meta.ID
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.runtimeConfigurationApplying, agentID)
		h.mu.Unlock()
	}()

	inspected, err := h.inspectRuntimeConfigurationForApply(agentID, next, params.ExpectedRevision)
	if err != nil {
		return RuntimeConfigurationView{}, err
	}
	h.mu.Lock()
	meta = h.agents[agentID]
	if meta == nil || meta.Status == "running" || h.stopping || runtimeConfigurationRevision(meta) != params.ExpectedRevision {
		h.mu.Unlock()
		return RuntimeConfigurationView{}, errf(409, "Agent state changed while Runtime configuration was validated; retry")
	}
	previous := meta.RuntimeConfiguration
	previousUpdatedAt := meta.UpdatedAt
	meta.RuntimeConfiguration = next
	meta.runtimeConfigurationPresent = true
	meta.UpdatedAt = now()
	if rt := h.runtimes[agentID]; rt != nil {
		configureRuntimeOwnerConfiguration(rt.runtimeContract, next)
	}
	if err := h.persistAgentsLocked(); err != nil {
		meta.RuntimeConfiguration, meta.UpdatedAt = previous, previousUpdatedAt
		if rt := h.runtimes[agentID]; rt != nil {
			configureRuntimeOwnerConfiguration(rt.runtimeContract, previous)
		}
		h.mu.Unlock()
		return RuntimeConfigurationView{}, errf(500, "save Runtime configuration: %s", err)
	}
	h.emitLocked(agentID, "loom/runtime-configuration-updated", map[string]any{
		"settingSources": append([]string(nil), next.SettingSources...),
		"authentication": next.Authentication,
		"validation":     inspected.Evidence.Authentication.Validation,
	})
	revision := runtimeConfigurationRevision(meta)
	h.mu.Unlock()
	inspected.Configuration = next
	inspected.Revision = revision
	return inspected, nil
}

func (h *Hub) inspectRuntimeConfigurationForApply(agentID string, configuration RuntimeConfiguration, expectedRevision string) (RuntimeConfigurationView, error) {
	h.mu.Lock()
	meta := h.agents[agentID]
	if meta == nil || runtimeConfigurationRevision(meta) != expectedRevision {
		h.mu.Unlock()
		return RuntimeConfigurationView{}, errf(409, "Runtime configuration snapshot is stale; reopen configuration and retry")
	}
	cwd, binding := meta.Cwd, runtimeContractBinding(meta)
	rt, err := h.getRuntimeLockedForResourcePolicy(meta)
	h.mu.Unlock()
	if err != nil {
		return RuntimeConfigurationView{}, err
	}
	rt.startMu.Lock()
	defer rt.startMu.Unlock()
	if err := waitReady(rt); err != nil {
		return RuntimeConfigurationView{}, errf(500, "Runtime not ready: %s", err)
	}
	inspector, ok := rt.runtimeContract.(runtimeOwnerConfigurationInspector)
	if !ok {
		return RuntimeConfigurationView{}, errf(409, "this Runtime does not expose owner configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	evidence, failure := inspector.InspectRuntimeOwnerConfiguration(ctx, binding, cwd, configuration)
	cancel()
	if failure != nil {
		return RuntimeConfigurationView{}, errf(409, "apply Runtime configuration: %s", failure.Message)
	}
	if err := validateRuntimeConfigurationEvidence(configuration, evidence); err != nil {
		return RuntimeConfigurationView{}, errf(500, "invalid Runtime configuration evidence: %s", err)
	}
	h.mu.Lock()
	meta = h.agents[agentID]
	stale := meta == nil || meta.Cwd != cwd || runtimeContractBinding(meta) != binding || meta.Status == "running" || h.stopping || runtimeConfigurationRevision(meta) != expectedRevision || h.runtimes[agentID] != rt
	h.mu.Unlock()
	if stale {
		return RuntimeConfigurationView{}, errf(409, "Agent state changed while Runtime configuration was applied; retry")
	}
	h.mu.Lock()
	driver, driverErr := h.runtimeHostDriverLocked(binding.RuntimeKind)
	h.mu.Unlock()
	if driverErr != nil {
		return RuntimeConfigurationView{}, driverErr
	}
	provider, ok := driver.(runtimeConfigurationDescriptorProvider)
	if !ok {
		return RuntimeConfigurationView{}, errf(409, "this Runtime does not expose owner configuration")
	}
	return RuntimeConfigurationView{Configuration: configuration, Evidence: evidence, Revision: expectedRevision, Descriptor: provider.RuntimeConfigurationDescriptor()}, nil
}
