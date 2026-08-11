// Package runtimecontract defines Loom's runtime-neutral execution boundary.
// Native protocol DTOs belong in adapters, never in this package.
package runtimecontract

import (
	"context"
	"encoding/json"
	"fmt"
)

const (
	Version              = 2
	BindingSchemaVersion = 2
)

// Contract is the mandatory Runtime Contract v2 core. Optional product
// behavior is negotiated through CapabilitySnapshot and capability-scoped
// interfaces rather than added here. During migration the v1 compatibility
// shim continues to carry sandbox, Approval, Provider/model/effort, and Skill
// controls until their capability tickets move them off the mandatory core.
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

type Binding struct {
	SchemaVersion int    `json:"schemaVersion"`
	RuntimeKind   string `json:"runtimeKind"`
	NativeRef     string `json:"nativeRef"`
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

const (
	CapabilitySandboxConfiguration  = "sandbox_configuration"
	CapabilityProviderConfiguration = "provider_configuration"
	CapabilityApprovalPolicy        = "approval_policy"
	CapabilitySkillsPolicy          = "skills_policy"
	CapabilityContextDelivery       = "context_delivery"
	CapabilityNativeRename          = "native_rename"
	CapabilityNativeArchive         = "native_archive"
)

type ContentKind string

const (
	ContentUserText      ContentKind = "user_text"
	ContentAssistantText ContentKind = "assistant_text"
	ContentReasoning     ContentKind = "reasoning"
	ContentToolCall      ContentKind = "tool_call"
	ContentToolResult    ContentKind = "tool_result"
	ContentImage         ContentKind = "image"
)

type ContentBlock struct {
	ID         string          `json:"id"`
	Kind       ContentKind     `json:"kind"`
	Text       string          `json:"text,omitempty"`
	ToolCall   *ToolCall       `json:"toolCall,omitempty"`
	ToolResult *ToolResult     `json:"toolResult,omitempty"`
	Image      *Image          `json:"image,omitempty"`
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
	switch b.Kind {
	case ContentUserText, ContentAssistantText, ContentReasoning:
		if b.ToolCall != nil || b.ToolResult != nil || b.Image != nil {
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
	MIMEType string `json:"mimeType"`
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
	TurnID         string          `json:"turnId"`
	RuntimeTurnRef string          `json:"-"`
	State          LifecycleState  `json:"state"`
	Content        []ContentBlock  `json:"content"`
	Usage          *Usage          `json:"usage,omitempty"`
	StartedAt      string          `json:"startedAt,omitempty"`
	CompletedAt    string          `json:"completedAt,omitempty"`
	Diagnostic     json.RawMessage `json:"-"`
}

type History struct {
	Total int           `json:"total"`
	Turns []HistoryTurn `json:"turns"`
}
