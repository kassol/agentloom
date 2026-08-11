// Package runtimecontract defines Loom's runtime-neutral execution boundary.
// Native protocol DTOs belong in adapters, never in this package.
package runtimecontract

import (
	"context"
	"encoding/json"
	"fmt"
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
	ID        string             `json:"id"`
	TurnID    string             `json:"turnId,omitempty"`
	ToolName  string             `json:"toolName"`
	Action    string             `json:"action,omitempty"`
	Arguments []ApprovalArgument `json:"arguments,omitempty"`
	Timeout   time.Duration      `json:"-"`
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
	FailurePhaseBindingCreate   FailurePhase = "binding_create"
	FailurePhaseBindingResume   FailurePhase = "binding_resume"
	FailurePhaseTurnStart       FailurePhase = "turn_start"
	FailurePhaseTurnContinue    FailurePhase = "turn_continue"
	FailurePhaseTurnInterrupt   FailurePhase = "turn_interrupt"
	FailurePhaseHistory         FailurePhase = "history"
	FailurePhaseClose           FailurePhase = "close"
	FailurePhaseBindingName     FailurePhase = "binding_name"
	FailurePhaseBindingArchive  FailurePhase = "binding_archive"
	FailurePhaseContextDelivery FailurePhase = "context_delivery"
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
		FailurePhaseContextDelivery:
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
	CapabilitySandboxConfiguration  = "sandbox_configuration"
	CapabilityProviderConfiguration = "provider_configuration"
	CapabilityApprovalPolicy        = "approval_policy"
	CapabilitySkillsPolicy          = "skills_policy"
	CapabilityContextDelivery       = "context_delivery"
	CapabilityNativeRename          = "native_rename"
	CapabilityNativeArchive         = "native_archive"
	CapabilityGoal                  = "goal"
	CapabilityRemote                = "remote"
	CapabilityUsageReporting        = "usage_reporting"
	CapabilityModelConfiguration    = "model_configuration"
	CapabilityManualCompaction      = "manual_compaction"
	CapabilityImageInput            = "image_input"
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
	Outcome           *Outcome      `json:"outcome,omitempty"`
}

func (e Event) Validate() error {
	if e.TurnID == "" {
		return fmt.Errorf("Runtime event Turn ID is required")
	}
	switch e.Kind {
	case EventTurnStarted:
		if e.Content != nil || e.Usage != nil || e.Outcome != nil {
			return fmt.Errorf("turn_started event cannot carry content, usage, or outcome")
		}
	case EventContent:
		if e.Content == nil || e.Usage != nil || e.Outcome != nil {
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
	case EventTerminal:
		if e.Outcome == nil || e.Content != nil || e.Usage != nil {
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

type HistoryRequest struct {
	Binding Binding `json:"binding"`
	Count   int     `json:"count"`
	Offset  int     `json:"offset"`
}

type HistoryTurn struct {
	TurnID            string          `json:"turnId"`
	PredecessorTurnID string          `json:"predecessorTurnId,omitempty"`
	RuntimeTurnRef    string          `json:"-"`
	State             LifecycleState  `json:"state"`
	Content           []ContentBlock  `json:"content"`
	Usage             *Usage          `json:"usage,omitempty"`
	StartedAt         string          `json:"startedAt,omitempty"`
	CompletedAt       string          `json:"completedAt,omitempty"`
	Diagnostic        json.RawMessage `json:"-"`
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
	}
	return nil
}
