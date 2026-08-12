// Package runtimecontract defines Loom's runtime-neutral execution boundary.
// Native protocol DTOs belong in adapters, never in this package.
package runtimecontract

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

const (
	Version              = 2
	BindingSchemaVersion = 2
)

// Contract is the mandatory Runtime Contract v2 core. Optional product
// behavior is negotiated through CapabilitySnapshot and capability-scoped
// interfaces rather than added here.
type Contract interface {
	ContractVersion() int
	CreateBinding(context.Context, BindingRequest) (Binding, Outcome)
	ResumeBinding(context.Context, Binding) Outcome
	StartTurn(context.Context, TurnRequest) Outcome
	ContinueTurn(context.Context, CausalInput) Outcome
	InterruptTurn(context.Context, TurnTarget) Outcome
	SetEventHandler(func(Event))
	ReadHistory(context.Context, HistoryRequest) (History, *Failure)
	CapabilitySnapshot(context.Context, Binding) CapabilitySnapshot
	CloseBinding(context.Context, Binding) Outcome
}

// BindingNameCapability is an optional native convenience. Loom Agent names
// remain authoritative, so callers invoke this only after committing the Loom
// rename and never use its outcome to roll product state back.
type BindingNameCapability interface {
	UpdateBindingName(context.Context, Binding, string) Outcome
}

// BindingArchiveCapability is an optional native consequence of a committed
// Loom archive. Failure cannot resurrect or invalidate the Loom-owned Agent
// archive; CloseBinding remains the mandatory resource-release operation.
type BindingArchiveCapability interface {
	ArchiveBinding(context.Context, Binding) Outcome
}

// ApprovalCapability lets a Runtime propose a side effect in Loom vocabulary
// and later receive the Owner's typed decision. Native request IDs, protocol
// payloads, and response handles remain private to the adapter.
type ApprovalCapability interface {
	SetApprovalHandler(func(ApprovalProposal))
	ResolveApproval(context.Context, string, ApprovalDecision) error
}

type ApprovalDecision string

const (
	ApprovalApprove ApprovalDecision = "approve"
	ApprovalDeny    ApprovalDecision = "deny"
	ApprovalTimeout ApprovalDecision = "timeout"
	ApprovalAbort   ApprovalDecision = "abort"
)

type ApprovalArgument struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type ApprovalProposal struct {
	ID         string             `json:"id"`
	ToolCallID string             `json:"-"`
	TurnID     string             `json:"turnId,omitempty"`
	ToolName   string             `json:"toolName"`
	Action     string             `json:"action,omitempty"`
	Arguments  []ApprovalArgument `json:"arguments,omitempty"`
	Timeout    time.Duration      `json:"-"`
}

type NeedsYouOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type NeedsYouProposal struct {
	ID          string           `json:"id"`
	TurnID      string           `json:"turnId,omitempty"`
	Question    string           `json:"question"`
	Context     string           `json:"context,omitempty"`
	BlockedWork string           `json:"blockedWork,omitempty"`
	Options     []NeedsYouOption `json:"options,omitempty"`
}

// NeedsYouCapability projects a Runtime question into Loom's durable Human
// Request workflow. The handler must return only after persistence succeeds;
// the adapter then releases the live callback and interrupts the source Turn.
type NeedsYouCapability interface {
	SetNeedsYouHandler(func(NeedsYouProposal) error)
}

type Binding struct {
	SchemaVersion int    `json:"schemaVersion"`
	RuntimeKind   string `json:"runtimeKind"`
	NativeRef     string `json:"nativeRef"`
}

func (b Binding) Validate() error {
	if b.SchemaVersion != BindingSchemaVersion {
		return fmt.Errorf("unsupported Runtime binding schema version %d", b.SchemaVersion)
	}
	if b.RuntimeKind == "" {
		return fmt.Errorf("Runtime binding kind is required")
	}
	if b.NativeRef == "" {
		return fmt.Errorf("Runtime binding native reference is required")
	}
	return nil
}

type BindingRequest struct {
	AgentID string `json:"agentId"`
	Name    string `json:"name"`
	Cwd     string `json:"cwd"`
}

type InputKind string

type InputRole string

const (
	InputText          InputKind = "text"
	InputImage         InputKind = "image"
	InputRoleUser      InputRole = "user"
	InputRoleDeveloper InputRole = "developer"
)

type InputBlock struct {
	Kind     InputKind `json:"kind"`
	Role     InputRole `json:"role,omitempty"`
	Text     string    `json:"text,omitempty"`
	Ref      string    `json:"ref,omitempty"`
	MIMEType string    `json:"mimeType,omitempty"`
}

type ContextDeliveryMode string

const (
	ContextDeliveryEpochIncremental ContextDeliveryMode = "epoch_incremental"
	ContextDeliveryFullPerTurn      ContextDeliveryMode = "full_per_turn"
)

// ContextDeliveryPolicy selects Loom's existing delivery strategy without
// teaching control-plane consumers about a concrete Runtime kind.
type ContextDeliveryPolicy interface {
	ContextDeliveryMode() ContextDeliveryMode
}

// ContextEvidenceCapability passively explains what Loom context a Runtime
// can prove for one canonical Turn. Runtime-native identifiers stay inside the
// adapter call and must never be copied into the returned evidence.
type ContextEvidenceCapability interface {
	ContextDeliveryPolicy
	InspectContextEvidence(context.Context, Binding, ContextEvidenceQuery) (ContextEvidence, *Failure)
}

// ContextMaintenanceCapability exposes one Runtime-native context maintenance
// action without pretending different Runtimes use the same algorithm. The
// inspection revision is opaque and only supports crash reconciliation.
type ContextMaintenanceCapability interface {
	InspectContextMaintenance(context.Context, Binding) (ContextMaintenanceInspection, *Failure)
	MaintainContext(context.Context, Binding) Outcome
}

type ContextMaintenanceInspection struct {
	Revision   string `json:"revision"`
	ObservedAt string `json:"observedAt,omitempty"`
}

func (i ContextMaintenanceInspection) Validate() error {
	if i.Revision == "" {
		return fmt.Errorf("Runtime context maintenance revision is required")
	}
	return nil
}

type ContextEvidenceState string

const (
	ContextEvidenceProven      ContextEvidenceState = "proven"
	ContextEvidenceUnknown     ContextEvidenceState = "unknown"
	ContextEvidenceUnavailable ContextEvidenceState = "unavailable"
)

type ContextEvidenceQuery struct {
	TurnID         string                 `json:"turnId"`
	RuntimeTurnRef string                 `json:"-"`
	Deliveries     []ContextDeliveryProbe `json:"deliveries,omitempty"`
}

type ContextDeliveryProbe struct {
	Role   string `json:"role"`
	Marker string `json:"marker"`
	Hash   string `json:"hash"`
}

type ContextEvidenceDelivery struct {
	Channel string `json:"channel"`
	Role    string `json:"role"`
	Hash    string `json:"hash"`
	Content string `json:"content"`
}

type ContextEvidenceSource struct {
	Key      string `json:"key"`
	Revision string `json:"revision,omitempty"`
	Hash     string `json:"hash,omitempty"`
	Channel  string `json:"channel"`
	State    string `json:"state"`
}

type ContextEvidence struct {
	State                 ContextEvidenceState      `json:"state"`
	TurnID                string                    `json:"turnId"`
	Mode                  ContextDeliveryMode       `json:"mode"`
	Reason                string                    `json:"reason,omitempty"`
	Sources               []ContextEvidenceSource   `json:"sources"`
	Deliveries            []ContextEvidenceDelivery `json:"deliveries"`
	UnsupportedDimensions []string                  `json:"unsupportedDimensions"`
	EpochID               string                    `json:"epochId,omitempty"`
	WindowNumber          int                       `json:"windowNumber,omitempty"`
	CompactedAt           string                    `json:"compactedAt,omitempty"`
	DeliveriesPersisted   bool                      `json:"deliveriesPersisted,omitempty"`
	PersistedDeliveryKeys map[string]bool           `json:"persistedDeliveryKeys,omitempty"`
}

func (e ContextEvidence) Validate() error {
	if e.TurnID == "" {
		return fmt.Errorf("context evidence Turn ID is required")
	}
	switch e.State {
	case ContextEvidenceProven, ContextEvidenceUnknown, ContextEvidenceUnavailable:
	default:
		return fmt.Errorf("unknown context evidence state %q", e.State)
	}
	switch e.Mode {
	case ContextDeliveryFullPerTurn, ContextDeliveryEpochIncremental:
	default:
		return fmt.Errorf("unknown context delivery mode %q", e.Mode)
	}
	seenSources := map[string]bool{}
	for _, source := range e.Sources {
		if source.Key == "" || source.Channel == "" || source.State == "" {
			return fmt.Errorf("context evidence source is incomplete")
		}
		key := source.Key + "\x00" + source.Channel
		if seenSources[key] {
			return fmt.Errorf("duplicate context evidence source %q", source.Key)
		}
		seenSources[key] = true
	}
	seenDeliveries := map[string]bool{}
	for _, delivery := range e.Deliveries {
		if delivery.Channel == "" || delivery.Role == "" || delivery.Hash == "" || delivery.Content == "" {
			return fmt.Errorf("context evidence delivery is incomplete")
		}
		if seenDeliveries[delivery.Channel] {
			return fmt.Errorf("duplicate context evidence delivery %q", delivery.Channel)
		}
		if fmt.Sprintf("%x", sha256.Sum256([]byte(delivery.Content))) != delivery.Hash {
			return fmt.Errorf("context evidence delivery %q hash does not match content", delivery.Channel)
		}
		seenDeliveries[delivery.Channel] = true
	}
	if e.State == ContextEvidenceProven && len(e.Deliveries) == 0 {
		return fmt.Errorf("proven context evidence requires a delivery")
	}
	return nil
}

// ModelControlCapability is the optional, Runtime-neutral model-control
// surface. Provider administration and credentials remain Host concerns; this
// capability only describes and selects models already available to one
// Runtime binding.
type ModelControlCapability interface {
	InspectModelControl(context.Context, Binding) (ModelControlState, *Failure)
	SelectModel(context.Context, Binding, ModelSelection) (ModelControlState, *Failure)
}

// InputCapability revalidates active-model input support at the execution
// boundary. Inspector previews use ModelControlState while the composer and
// Hub always use the committed active model.
type InputCapability interface {
	ValidateInput(context.Context, Binding, []InputBlock) *Failure
}

// UsageInspectionCapability exposes passive, Runtime-neutral accounting for a
// binding. Inspection must not start or acquire a Runtime host.
type UsageInspectionCapability interface {
	InspectUsage(context.Context, Binding) (UsageReport, *Failure)
}

// ResourceInventoryCapability exposes only resources discovered by the
// selected Runtime. Paths and source metadata retain Runtime-specific meaning.
type ResourceInventoryCapability interface {
	InspectResources(context.Context, ResourceInventoryRequest) (ResourceInventory, *Failure)
}

// ResourcePolicyCapability is intentionally narrower than inventory. A
// Runtime may expose native resources while declining Loom policy mutation.
type ResourcePolicyCapability interface {
	InspectResourcePolicy(context.Context, ResourcePolicyRequest) (ResourcePolicyState, *Failure)
}

type ResourceKind string

const (
	ResourceSkill     ResourceKind = "skill"
	ResourcePrompt    ResourceKind = "prompt"
	ResourceExtension ResourceKind = "extension"
)

type Resource struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Kind        ResourceKind `json:"kind"`
	Path        string       `json:"path"`
	Scope       string       `json:"scope,omitempty"`
	Source      string       `json:"source,omitempty"`
	Enabled     bool         `json:"enabled"`
}

type ResourceInventoryRequest struct {
	Binding Binding `json:"binding"`
	Cwd     string  `json:"cwd"`
}

type ResourceInventory struct {
	Revision  string     `json:"revision"`
	Semantics string     `json:"semantics"`
	Resources []Resource `json:"resources"`
}

func (i ResourceInventory) Validate() error {
	if i.Revision == "" {
		return fmt.Errorf("Runtime resource inventory revision is required")
	}
	if i.Semantics == "" {
		return fmt.Errorf("Runtime resource semantics are required")
	}
	seen := make(map[string]struct{}, len(i.Resources))
	for _, resource := range i.Resources {
		if resource.ID == "" || resource.Name == "" || resource.Path == "" {
			return fmt.Errorf("Runtime resource identity, name, and path are required")
		}
		if _, exists := seen[resource.ID]; exists {
			return fmt.Errorf("duplicate Runtime resource %q", resource.ID)
		}
		seen[resource.ID] = struct{}{}
		switch resource.Kind {
		case ResourceSkill, ResourcePrompt, ResourceExtension:
		default:
			return fmt.Errorf("Runtime resource %q has unknown kind %q", resource.ID, resource.Kind)
		}
	}
	return nil
}

type ResourcePolicyRequest struct {
	Binding       Binding  `json:"binding"`
	Cwd           string   `json:"cwd"`
	DisabledPaths []string `json:"disabledPaths"`
}

type ResourcePolicyState struct {
	Revision      string               `json:"revision"`
	DisabledPaths []string             `json:"disabledPaths"`
	Effective     bool                 `json:"effective"`
	Evidence      []CapabilityEvidence `json:"evidence"`
}

func (s ResourcePolicyState) Validate() error {
	if s.Revision == "" {
		return fmt.Errorf("Runtime resource policy revision is required")
	}
	seen := make(map[string]struct{}, len(s.DisabledPaths))
	for _, path := range s.DisabledPaths {
		if path == "" {
			return fmt.Errorf("Runtime resource policy contains an empty path")
		}
		if _, exists := seen[path]; exists {
			return fmt.Errorf("Runtime resource policy contains duplicate path %q", path)
		}
		seen[path] = struct{}{}
	}
	if s.Effective && len(s.Evidence) == 0 {
		return fmt.Errorf("effective Runtime resource policy requires native evidence")
	}
	return nil
}

type Model struct {
	Provider             string   `json:"provider"`
	ID                   string   `json:"id"`
	DisplayName          string   `json:"displayName,omitempty"`
	ContextWindow        int      `json:"contextWindow,omitempty"`
	Reasoning            bool     `json:"reasoning"`
	ThinkingLevels       []string `json:"thinkingLevels"`
	DefaultThinkingLevel string   `json:"defaultThinkingLevel,omitempty"`
	ImageInput           bool     `json:"imageInput"`
}

const ThinkingLevelDefault = "default"

type ModelControlState struct {
	Current       Model   `json:"current"`
	Models        []Model `json:"models"`
	ThinkingLevel string  `json:"thinkingLevel"`
}

type ModelSelection struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ThinkingLevel string `json:"thinkingLevel"`
}

func (s ModelControlState) Validate() error {
	if len(s.Models) == 0 {
		return fmt.Errorf("Runtime model catalog is empty")
	}
	seen := make(map[string]struct{}, len(s.Models))
	currentFound := false
	for _, model := range s.Models {
		if model.Provider == "" {
			return fmt.Errorf("Runtime model provider is required")
		}
		key := model.Provider + "\x00" + model.ID
		if _, exists := seen[key]; exists {
			return fmt.Errorf("Runtime model catalog contains duplicate %s/%s", model.Provider, model.ID)
		}
		seen[key] = struct{}{}
		if len(model.ThinkingLevels) == 0 {
			return fmt.Errorf("Runtime model %s/%s has no explicit thinking choices", model.Provider, model.ID)
		}
		defaultFound := false
		for _, level := range model.ThinkingLevels {
			if level == model.DefaultThinkingLevel {
				defaultFound = true
			}
		}
		if model.DefaultThinkingLevel == "" || !defaultFound {
			return fmt.Errorf("Runtime model %s/%s has no valid explicit default thinking level", model.Provider, model.ID)
		}
		if model.Provider == s.Current.Provider && model.ID == s.Current.ID {
			currentFound = true
			if !reflect.DeepEqual(model, s.Current) {
				return fmt.Errorf("active Runtime model descriptor disagrees with its catalog entry")
			}
		}
	}
	if !currentFound {
		return fmt.Errorf("active Runtime model %s/%s is absent from the catalog", s.Current.Provider, s.Current.ID)
	}
	return s.ValidateSelection(ModelSelection{Provider: s.Current.Provider, Model: s.Current.ID, ThinkingLevel: s.ThinkingLevel})
}

func (s ModelControlState) ValidateSelection(selection ModelSelection) error {
	if selection.Provider == "" {
		return fmt.Errorf("Runtime model provider is required")
	}
	for _, model := range s.Models {
		if model.Provider != selection.Provider || model.ID != selection.Model {
			continue
		}
		if selection.ThinkingLevel == "" {
			return nil
		}
		for _, level := range model.ThinkingLevels {
			if level == selection.ThinkingLevel {
				return nil
			}
		}
		return fmt.Errorf("thinking level %q is unavailable for Runtime model %s/%s", selection.ThinkingLevel, selection.Provider, selection.Model)
	}
	return fmt.Errorf("Runtime model %s/%s is absent from the catalog", selection.Provider, selection.Model)
}

type TurnRequest struct {
	Binding Binding      `json:"binding"`
	TurnID  string       `json:"turnId"`
	Input   []InputBlock `json:"input"`
}

type CausalInput struct {
	Binding           Binding      `json:"binding"`
	TurnID            string       `json:"turnId"`
	PredecessorTurnID string       `json:"predecessorTurnId"`
	RuntimeTurnRef    string       `json:"runtimeTurnRef,omitempty"`
	Input             []InputBlock `json:"input"`
}

type TurnTarget struct {
	Binding        Binding `json:"binding"`
	TurnID         string  `json:"turnId"`
	RuntimeTurnRef string  `json:"runtimeTurnRef,omitempty"`
}

type LifecycleState string

const (
	LifecycleAccepted      LifecycleState = "accepted"
	LifecycleRejected      LifecycleState = "rejected"
	LifecycleFailed        LifecycleState = "failed"
	LifecycleInterrupted   LifecycleState = "interrupted"
	LifecycleCompleted     LifecycleState = "completed"
	LifecycleIndeterminate LifecycleState = "indeterminate"
)

type Outcome struct {
	State          LifecycleState `json:"state"`
	RuntimeTurnRef string         `json:"-"`
	Failure        *Failure       `json:"failure,omitempty"`
}

func (o Outcome) Validate() error {
	switch o.State {
	case LifecycleAccepted, LifecycleInterrupted, LifecycleCompleted:
		if o.Failure != nil {
			return fmt.Errorf("%s outcome cannot carry a failure", o.State)
		}
	case LifecycleRejected, LifecycleFailed, LifecycleIndeterminate:
		if o.Failure == nil {
			return fmt.Errorf("%s outcome requires a failure", o.State)
		}
	default:
		return fmt.Errorf("unknown lifecycle state %q", o.State)
	}
	if o.Failure != nil {
		if err := o.Failure.Validate(); err != nil {
			return err
		}
	}
	if o.State == LifecycleIndeterminate {
		if o.Failure == nil {
			return fmt.Errorf("indeterminate outcome requires a failure")
		}
		if o.Failure.Retryable {
			return fmt.Errorf("indeterminate outcome cannot be marked retryable")
		}
	}
	return nil
}

type FailurePhase string

const (
	FailurePhaseBindingCreate      FailurePhase = "binding_create"
	FailurePhaseBindingResume      FailurePhase = "binding_resume"
	FailurePhaseTurnStart          FailurePhase = "turn_start"
	FailurePhaseTurnContinue       FailurePhase = "turn_continue"
	FailurePhaseTurnInterrupt      FailurePhase = "turn_interrupt"
	FailurePhaseHistory            FailurePhase = "history"
	FailurePhaseClose              FailurePhase = "close"
	FailurePhaseBindingName        FailurePhase = "binding_name"
	FailurePhaseBindingArchive     FailurePhase = "binding_archive"
	FailurePhaseContextDelivery    FailurePhase = "context_delivery"
	FailurePhaseModelControl       FailurePhase = "model_control"
	FailurePhaseResourceInventory  FailurePhase = "resource_inventory"
	FailurePhaseResourcePolicy     FailurePhase = "resource_policy"
	FailurePhaseUsageInspection    FailurePhase = "usage_inspection"
	FailurePhaseContextMaintenance FailurePhase = "context_maintenance"
)

type Failure struct {
	Code       string       `json:"code"`
	Phase      FailurePhase `json:"phase"`
	Message    string       `json:"message"`
	Retryable  bool         `json:"retryable"`
	Diagnostic string       `json:"-"`
	Cause      error        `json:"-"`
}

func (f Failure) Validate() error {
	if f.Code == "" {
		return fmt.Errorf("Runtime failure code is required")
	}
	if f.Message == "" {
		return fmt.Errorf("Runtime failure message is required")
	}
	switch f.Phase {
	case FailurePhaseBindingCreate, FailurePhaseBindingResume, FailurePhaseTurnStart,
		FailurePhaseTurnContinue, FailurePhaseTurnInterrupt, FailurePhaseHistory,
		FailurePhaseClose, FailurePhaseBindingName, FailurePhaseBindingArchive,
		FailurePhaseContextDelivery, FailurePhaseModelControl,
		FailurePhaseResourceInventory, FailurePhaseResourcePolicy, FailurePhaseUsageInspection,
		FailurePhaseContextMaintenance:
		return nil
	default:
		return fmt.Errorf("Runtime failure has unknown phase %q", f.Phase)
	}
}

const FailureCodeBindingNotFound = "native_binding_not_found"

type CapabilityAvailability string

const (
	CapabilityAvailable   CapabilityAvailability = "available"
	CapabilityUnavailable CapabilityAvailability = "unavailable"
)

type CapabilityScope struct {
	RuntimeKind           string `json:"runtimeKind"`
	BindingRevision       string `json:"bindingRevision,omitempty"`
	Model                 string `json:"model,omitempty"`
	ConfigurationRevision string `json:"configurationRevision,omitempty"`
}

type CapabilityEvidence struct {
	Kind       string `json:"kind"`
	Summary    string `json:"summary"`
	ObservedAt string `json:"observedAt,omitempty"`
}

type CapabilityDescriptor struct {
	ID           string                 `json:"id"`
	Availability CapabilityAvailability `json:"availability"`
	Scope        CapabilityScope        `json:"scope"`
	Reason       string                 `json:"reason,omitempty"`
	Alternative  string                 `json:"alternative,omitempty"`
	Evidence     []CapabilityEvidence   `json:"evidence,omitempty"`
	Revision     string                 `json:"revision"`
}

type CapabilitySnapshot struct {
	Revision     string                 `json:"revision"`
	Capabilities []CapabilityDescriptor `json:"capabilities"`
}

func (s CapabilitySnapshot) Validate() error {
	if s.Revision == "" {
		return fmt.Errorf("capability snapshot revision is required")
	}
	seen := make(map[string]struct{}, len(s.Capabilities))
	for _, descriptor := range s.Capabilities {
		if descriptor.ID == "" {
			return fmt.Errorf("capability descriptor ID is required")
		}
		if _, exists := seen[descriptor.ID]; exists {
			return fmt.Errorf("duplicate capability descriptor %q", descriptor.ID)
		}
		seen[descriptor.ID] = struct{}{}
		if descriptor.Revision == "" {
			return fmt.Errorf("capability descriptor %q revision is required", descriptor.ID)
		}
		switch descriptor.Availability {
		case CapabilityAvailable:
		case CapabilityUnavailable:
			if descriptor.Reason == "" || descriptor.Alternative == "" {
				return fmt.Errorf("unavailable capability descriptor %q requires a reason and alternative", descriptor.ID)
			}
		default:
			return fmt.Errorf("capability descriptor %q has unknown availability %q", descriptor.ID, descriptor.Availability)
		}
	}
	return nil
}

const (
	CapabilitySandboxConfiguration = "sandbox_configuration"
	CapabilityApprovalPolicy       = "approval_policy"
	CapabilityResourceInventory    = "resource_inventory"
	CapabilityResourcePolicy       = "resource_policy"
	CapabilityContextDelivery      = "context_delivery"
	CapabilityNativeRename         = "native_rename"
	CapabilityNativeArchive        = "native_archive"
	CapabilityGoal                 = "goal"
	CapabilityRemote               = "remote"
	CapabilityUsageReporting       = "usage_reporting"
	CapabilityModelConfiguration   = "model_configuration"
	CapabilityManualCompaction     = "manual_compaction"
	CapabilityImageInput           = "image_input"
)

type ContentKind string

const (
	ContentUserText      ContentKind = "user_text"
	ContentAssistantText ContentKind = "assistant_text"
	ContentReasoning     ContentKind = "reasoning"
	ContentToolCall      ContentKind = "tool_call"
	ContentToolResult    ContentKind = "tool_result"
	ContentImage         ContentKind = "image"
	ContentAttachment    ContentKind = "attachment"
)

type ContentBlock struct {
	ID         string          `json:"id"`
	Kind       ContentKind     `json:"kind"`
	Text       string          `json:"text,omitempty"`
	ToolCall   *ToolCall       `json:"toolCall,omitempty"`
	ToolResult *ToolResult     `json:"toolResult,omitempty"`
	Image      *Image          `json:"image,omitempty"`
	Attachment *Attachment     `json:"attachment,omitempty"`
	Diagnostic json.RawMessage `json:"-"`
}

func (b ContentBlock) Validate() error {
	if b.ID == "" {
		return fmt.Errorf("content block id is required")
	}
	payloads := 0
	if b.Text != "" {
		payloads++
	}
	if b.ToolCall != nil {
		payloads++
	}
	if b.ToolResult != nil {
		payloads++
	}
	if b.Image != nil {
		payloads++
	}
	if b.Attachment != nil {
		payloads++
	}
	switch b.Kind {
	case ContentUserText, ContentAssistantText, ContentReasoning:
		if b.ToolCall != nil || b.ToolResult != nil || b.Image != nil || b.Attachment != nil {
			return fmt.Errorf("%s content requires only text", b.Kind)
		}
	case ContentToolCall:
		if b.ToolCall == nil || payloads != 1 {
			return fmt.Errorf("tool_call content requires only a tool call")
		}
	case ContentToolResult:
		if b.ToolResult == nil || payloads != 1 {
			return fmt.Errorf("tool_result content requires only a tool result")
		}
	case ContentImage:
		if b.Image == nil || payloads != 1 {
			return fmt.Errorf("image content requires only an image")
		}
		if b.Image.Ref == "" {
			return fmt.Errorf("image content requires a reference")
		}
	case ContentAttachment:
		if b.Attachment == nil || payloads != 1 {
			return fmt.Errorf("attachment content requires only an attachment")
		}
		if b.Attachment.Ref == "" {
			return fmt.Errorf("attachment content requires a reference")
		}
	default:
		return fmt.Errorf("unknown content kind %q", b.Kind)
	}
	return nil
}

type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type ToolResult struct {
	ToolCallID string `json:"toolCallId"`
	Text       string `json:"text,omitempty"`
	Success    bool   `json:"success"`
}

type Image struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Size     int64  `json:"size,omitempty"`
	MIMEType string `json:"mimeType"`
	Ref      string `json:"ref"`
}

type Attachment struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Size     int64  `json:"size,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	Ref      string `json:"ref"`
}

type EventKind string

const (
	EventTurnStarted EventKind = "turn_started"
	EventContent     EventKind = "content"
	EventUsage       EventKind = "usage"
	EventTerminal    EventKind = "terminal"
)

type ContentPhase string

const (
	ContentPhaseStarted   ContentPhase = "started"
	ContentPhaseDelta     ContentPhase = "delta"
	ContentPhaseUpdated   ContentPhase = "updated"
	ContentPhaseCompleted ContentPhase = "completed"
)

type Event struct {
	Kind              EventKind     `json:"kind"`
	TurnID            string        `json:"turnId"`
	PredecessorTurnID string        `json:"predecessorTurnId,omitempty"`
	RuntimeTurnRef    string        `json:"-"`
	ContentPhase      ContentPhase  `json:"contentPhase,omitempty"`
	Content           *ContentBlock `json:"content,omitempty"`
	Usage             *Usage        `json:"usage,omitempty"`
	UsageDetails      *UsageDetails `json:"usageDetails,omitempty"`
	Outcome           *Outcome      `json:"outcome,omitempty"`
}

func (e Event) Validate() error {
	if e.TurnID == "" {
		return fmt.Errorf("Runtime event Turn ID is required")
	}
	switch e.Kind {
	case EventTurnStarted:
		if e.Content != nil || e.Usage != nil || e.UsageDetails != nil || e.Outcome != nil {
			return fmt.Errorf("turn_started event cannot carry content, usage, or outcome")
		}
	case EventContent:
		if e.Content == nil || e.Usage != nil || e.UsageDetails != nil || e.Outcome != nil {
			return fmt.Errorf("content event requires only content")
		}
		switch e.ContentPhase {
		case ContentPhaseStarted, ContentPhaseDelta, ContentPhaseUpdated, ContentPhaseCompleted:
		default:
			return fmt.Errorf("content event has unknown phase %q", e.ContentPhase)
		}
		if err := e.Content.Validate(); err != nil {
			return fmt.Errorf("invalid Runtime event content: %w", err)
		}
	case EventUsage:
		if e.Usage == nil || e.Content != nil || e.Outcome != nil {
			return fmt.Errorf("usage event requires only usage")
		}
		if err := e.Usage.validate("usage event"); err != nil {
			return err
		}
		if e.UsageDetails != nil {
			if err := e.UsageDetails.validate("usage event"); err != nil {
				return err
			}
		}
	case EventTerminal:
		if e.Outcome == nil || e.Content != nil || e.Usage != nil || e.UsageDetails != nil {
			return fmt.Errorf("terminal event requires only an outcome")
		}
		if err := e.Outcome.Validate(); err != nil {
			return fmt.Errorf("invalid terminal outcome: %w", err)
		}
		if e.Outcome.State == LifecycleAccepted {
			return fmt.Errorf("terminal event cannot carry accepted outcome")
		}
	default:
		return fmt.Errorf("unknown Runtime event kind %q", e.Kind)
	}
	return nil
}

type UsageMetric struct {
	Available bool   `json:"available"`
	Value     int64  `json:"value,omitempty"`
	Source    string `json:"source"`
}

type Usage struct {
	InputTokens           UsageMetric `json:"inputTokens"`
	CachedInputTokens     UsageMetric `json:"cachedInputTokens"`
	OutputTokens          UsageMetric `json:"outputTokens"`
	ReasoningOutputTokens UsageMetric `json:"reasoningOutputTokens"`
	TotalTokens           UsageMetric `json:"totalTokens"`
	Calls                 UsageMetric `json:"calls"`
	CostMicros            UsageMetric `json:"costMicros"`
}

// Add combines only observed values. A field remains unavailable until at
// least one source reports it; missing Runtime metrics are never turned into
// zeroes by aggregation.
func (u *Usage) Add(other Usage) {
	addUsageMetric := func(target *UsageMetric, value UsageMetric) {
		if !value.Available {
			if target.Source == "" {
				target.Source = value.Source
			}
			return
		}
		wasAvailable := target.Available
		target.Available = true
		target.Value += value.Value
		if !wasAvailable || target.Source == "" {
			target.Source = value.Source
		} else if value.Source != "" && target.Source != value.Source {
			target.Source = "aggregate"
		}
	}
	addUsageMetric(&u.InputTokens, other.InputTokens)
	addUsageMetric(&u.CachedInputTokens, other.CachedInputTokens)
	addUsageMetric(&u.OutputTokens, other.OutputTokens)
	addUsageMetric(&u.ReasoningOutputTokens, other.ReasoningOutputTokens)
	addUsageMetric(&u.TotalTokens, other.TotalTokens)
	addUsageMetric(&u.Calls, other.Calls)
	addUsageMetric(&u.CostMicros, other.CostMicros)
}

type UsageText struct {
	Available bool   `json:"available"`
	Value     string `json:"value,omitempty"`
	Source    string `json:"source"`
}

// UsageDetails carries optional native-reported dimensions that are not
// numeric counters. Each field retains its own availability and source.
type UsageDetails struct {
	ObservedAt UsageText    `json:"observedAt"`
	Models     []ModelUsage `json:"models"`
}

func (d UsageDetails) validate(prefix string) error {
	if err := d.ObservedAt.validate(prefix + " observed at"); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, model := range d.Models {
		if err := model.Provider.validate(fmt.Sprintf("%s model %d provider", prefix, index)); err != nil {
			return err
		}
		if err := model.Model.validate(fmt.Sprintf("%s model %d name", prefix, index)); err != nil {
			return err
		}
		if !model.Model.Available {
			return fmt.Errorf("%s model %d has no model name", prefix, index)
		}
		if err := model.Usage.validate(fmt.Sprintf("%s model %d usage", prefix, index)); err != nil {
			return err
		}
		if err := model.ContextWindow.validate(fmt.Sprintf("%s model %d context window", prefix, index)); err != nil {
			return err
		}
		key := model.Provider.Value + "\x00" + model.Model.Value
		if seen[key] {
			return fmt.Errorf("%s has duplicate model %q", prefix, model.Model.Value)
		}
		seen[key] = true
	}
	return nil
}

type ModelUsage struct {
	Provider      UsageText   `json:"provider"`
	Model         UsageText   `json:"model"`
	Usage         Usage       `json:"usage"`
	ContextWindow UsageMetric `json:"contextWindow"`
}

type UsageBool struct {
	Available bool   `json:"available"`
	Value     bool   `json:"value,omitempty"`
	Source    string `json:"source"`
}

type UsageEvent struct {
	Timestamp UsageText `json:"timestamp"`
	TurnID    UsageText `json:"turnId"`
	Provider  UsageText `json:"provider"`
	Model     UsageText `json:"model"`
	Usage     Usage     `json:"usage"`
}

type TurnUsage struct {
	TurnID        UsageText `json:"turnId"`
	Provider      UsageText `json:"provider"`
	Model         UsageText `json:"model"`
	Usage         Usage     `json:"usage"`
	LastUpdatedAt UsageText `json:"lastUpdatedAt"`
}

type TurnActivity struct {
	TurnID      UsageText `json:"turnId"`
	StartedAt   UsageText `json:"startedAt"`
	EndedAt     UsageText `json:"endedAt"`
	Status      UsageText `json:"status"`
	InferredEnd UsageBool `json:"inferredEnd"`
}

type UsageReport struct {
	Lifetime           Usage          `json:"lifetime"`
	Events             []UsageEvent   `json:"events"`
	Turns              []TurnUsage    `json:"turns"`
	Activity           []TurnActivity `json:"activity"`
	LatestCall         Usage          `json:"latestCall"`
	LatestProvider     UsageText      `json:"latestProvider"`
	LatestModel        UsageText      `json:"latestModel"`
	ContextInputTokens UsageMetric    `json:"contextInputTokens"`
	ModelContextWindow UsageMetric    `json:"modelContextWindow"`
	LastUpdatedAt      UsageText      `json:"lastUpdatedAt"`
}

func (r UsageReport) Validate() error {
	if err := r.Lifetime.validate("lifetime"); err != nil {
		return err
	}
	for index, event := range r.Events {
		if err := event.Timestamp.validate(fmt.Sprintf("event %d timestamp", index)); err != nil {
			return err
		}
		if err := event.TurnID.validate(fmt.Sprintf("event %d Turn ID", index)); err != nil {
			return err
		}
		if err := event.Provider.validate(fmt.Sprintf("event %d provider", index)); err != nil {
			return err
		}
		if err := event.Model.validate(fmt.Sprintf("event %d model", index)); err != nil {
			return err
		}
		if err := event.Usage.validate(fmt.Sprintf("event %d usage", index)); err != nil {
			return err
		}
	}
	for index, turn := range r.Turns {
		for name, value := range map[string]UsageText{"Turn ID": turn.TurnID, "provider": turn.Provider, "model": turn.Model, "last updated": turn.LastUpdatedAt} {
			if err := value.validate(fmt.Sprintf("Turn %d %s", index, name)); err != nil {
				return err
			}
		}
		if err := turn.Usage.validate(fmt.Sprintf("Turn %d usage", index)); err != nil {
			return err
		}
	}
	for index, activity := range r.Activity {
		for name, value := range map[string]UsageText{"Turn ID": activity.TurnID, "started at": activity.StartedAt, "ended at": activity.EndedAt, "status": activity.Status} {
			if err := value.validate(fmt.Sprintf("activity %d %s", index, name)); err != nil {
				return err
			}
		}
		if err := activity.InferredEnd.validate(fmt.Sprintf("activity %d inferred end", index)); err != nil {
			return err
		}
	}
	for name, value := range map[string]UsageText{"latest provider": r.LatestProvider, "latest model": r.LatestModel, "last updated": r.LastUpdatedAt} {
		if err := value.validate(name); err != nil {
			return err
		}
	}
	if err := r.LatestCall.validate("latest call"); err != nil {
		return err
	}
	for name, metric := range map[string]UsageMetric{"context input tokens": r.ContextInputTokens, "model context window": r.ModelContextWindow} {
		if err := metric.validate(name); err != nil {
			return err
		}
	}
	return nil
}

func (u Usage) validate(prefix string) error {
	for name, metric := range map[string]UsageMetric{
		"input tokens": u.InputTokens, "cached input tokens": u.CachedInputTokens, "output tokens": u.OutputTokens,
		"reasoning output tokens": u.ReasoningOutputTokens, "total tokens": u.TotalTokens, "calls": u.Calls, "cost micros": u.CostMicros,
	} {
		if err := metric.validate(prefix + " " + name); err != nil {
			return err
		}
	}
	return nil
}

func (m UsageMetric) validate(name string) error {
	if m.Source == "" {
		return fmt.Errorf("%s has no source", name)
	}
	if !m.Available && m.Value != 0 {
		return fmt.Errorf("unavailable %s has value %d", name, m.Value)
	}
	return nil
}

func (m UsageText) validate(name string) error {
	if m.Source == "" {
		return fmt.Errorf("%s has no source", name)
	}
	if m.Available && m.Value == "" {
		return fmt.Errorf("available %s has no value", name)
	}
	if !m.Available && m.Value != "" {
		return fmt.Errorf("unavailable %s has value", name)
	}
	return nil
}

func (m UsageBool) validate(name string) error {
	if m.Source == "" {
		return fmt.Errorf("%s has no source", name)
	}
	if !m.Available && m.Value {
		return fmt.Errorf("unavailable %s has value", name)
	}
	return nil
}

type HistoryRequest struct {
	Binding Binding `json:"binding"`
	Count   int     `json:"count"`
	Offset  int     `json:"offset"`
}

type HistoryTurn struct {
	TurnID            string           `json:"turnId"`
	PredecessorTurnID string           `json:"predecessorTurnId,omitempty"`
	RuntimeTurnRef    string           `json:"-"`
	State             LifecycleState   `json:"state"`
	Content           []ContentBlock   `json:"content"`
	Usage             *Usage           `json:"usage,omitempty"`
	UsageDetails      *UsageDetails    `json:"usageDetails,omitempty"`
	ContextEvidence   *ContextEvidence `json:"contextEvidence,omitempty"`
	StartedAt         string           `json:"startedAt,omitempty"`
	CompletedAt       string           `json:"completedAt,omitempty"`
	Diagnostic        json.RawMessage  `json:"-"`
}

type History struct {
	Total int           `json:"total"`
	Turns []HistoryTurn `json:"turns"`
}

func (h History) Validate() error {
	if h.Total < 0 || h.Total < len(h.Turns) {
		return fmt.Errorf("Runtime history total %d is smaller than returned Turns", h.Total)
	}
	for turnIndex, turn := range h.Turns {
		if turn.TurnID == "" && turn.RuntimeTurnRef == "" {
			return fmt.Errorf("Runtime history Turn %d has no Turn identity", turnIndex)
		}
		switch turn.State {
		case LifecycleAccepted, LifecycleRejected, LifecycleFailed, LifecycleInterrupted, LifecycleCompleted, LifecycleIndeterminate:
		default:
			return fmt.Errorf("Runtime history Turn %q has unknown state %q", turn.TurnID, turn.State)
		}
		for contentIndex, content := range turn.Content {
			if err := content.Validate(); err != nil {
				return fmt.Errorf("Runtime history Turn %q content %d: %w", turn.TurnID, contentIndex, err)
			}
		}
		if turn.Usage != nil {
			if err := turn.Usage.validate(fmt.Sprintf("Runtime history Turn %q usage", turn.TurnID)); err != nil {
				return err
			}
		}
		if turn.UsageDetails != nil {
			if err := turn.UsageDetails.validate(fmt.Sprintf("Runtime history Turn %q usage details", turn.TurnID)); err != nil {
				return err
			}
		}
		if turn.ContextEvidence != nil {
			if err := turn.ContextEvidence.Validate(); err != nil {
				return fmt.Errorf("Runtime history Turn %q context evidence: %w", turn.TurnID, err)
			}
			if turn.ContextEvidence.TurnID != turn.TurnID {
				return fmt.Errorf("Runtime history Turn %q context evidence has Turn ID %q", turn.TurnID, turn.ContextEvidence.TurnID)
			}
		}
	}
	return nil
}
