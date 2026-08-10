package hub

import (
	"encoding/json"
	"time"
)

// AgentRuntime is the single execution boundary between Loom's durable Agent
// state and a Runtime-native conversation. Hub owns causality and Turn state;
// implementations own native lifecycle, events, and history.
type AgentRuntime interface {
	Alive() bool
	Create(RuntimeBindingRequest) (string, error)
	Resume(RuntimeBindingRequest, time.Duration) error
	InjectDeveloperContext(nativeRef, content string, timeout time.Duration) error
	StartTurn(RuntimeTurnRequest) (string, error)
	Steer(nativeRef, expectedNativeTurnID, input string, timeout time.Duration) (string, error)
	Interrupt(nativeRef, nativeTurnID string, timeout time.Duration) error
	NormalizeEvent(method string, params json.RawMessage) []RuntimeEvent
	ReadHistory(nativeRef string, count, offset int) (RuntimeHistory, error)
	ReadTurn(nativeRef, nativeTurnID string) (RuntimeHistoryTurn, error)
	LatestTurn(nativeRef string) (*RuntimeHistoryTurn, error)
	Capabilities() RuntimeCapabilities
	Close()
}

// RuntimeEventSource is implemented by per-Agent Runtimes that deliver
// normalized events directly instead of sharing CodexHost's notification
// connection. Hub remains the owner of Turn causality and terminal state.
type RuntimeEventSource interface {
	SetRuntimeEventHandlers(func(RuntimeEvent), func(error))
}

type RuntimeBindingRequest struct {
	NativeRef          string
	Name               string
	Cwd                string
	Sandbox            string
	ProviderID         string
	Model              string
	DisabledSkillPaths []string
}

type RuntimeTurnRequest struct {
	NativeRef      string
	Input          []RuntimeInput
	ApprovalPolicy string
	Sandbox        string
	Model          string
	Effort         string
	Timeout        time.Duration
}

type RuntimeInputKind string

const (
	RuntimeInputText       RuntimeInputKind = "text"
	RuntimeInputLocalImage RuntimeInputKind = "local_image"
)

type RuntimeInput struct {
	Kind RuntimeInputKind
	Text string
	Path string
}

type RuntimeCapabilities struct {
	History     bool `json:"history"`
	CausalSteer bool `json:"causalSteer"`
	Interrupt   bool `json:"interrupt"`
	Goal        bool `json:"goal"`
	Remote      bool `json:"remote"`
	Usage       bool `json:"usage"`
	Provider    bool `json:"provider"`
	Compaction  bool `json:"compaction"`
	Approval    bool `json:"approval"`
	Skills      bool `json:"skills"`
	Naming      bool `json:"naming"`
	Archive     bool `json:"archive"`
	Sandbox     bool `json:"sandbox"`
}

type RuntimeEventKind string

const (
	RuntimeTurnStarted        RuntimeEventKind = "turn_started"
	RuntimeUserInput          RuntimeEventKind = "user_input"
	RuntimeTextDelta          RuntimeEventKind = "text_delta"
	RuntimeTextCompleted      RuntimeEventKind = "text_completed"
	RuntimeReasoningDelta     RuntimeEventKind = "reasoning_delta"
	RuntimeReasoningCompleted RuntimeEventKind = "reasoning_completed"
	RuntimeToolStarted        RuntimeEventKind = "tool_started"
	RuntimeToolUpdated        RuntimeEventKind = "tool_updated"
	RuntimeToolCompleted      RuntimeEventKind = "tool_completed"
	RuntimeTurnCompleted      RuntimeEventKind = "turn_completed"
	RuntimeTurnFailed         RuntimeEventKind = "turn_failed"
	RuntimeTurnInterrupted    RuntimeEventKind = "turn_interrupted"
)

type RuntimeEvent struct {
	Kind         RuntimeEventKind
	NativeRef    string
	NativeTurnID string
	ItemID       string
	Text         string
	Item         map[string]any
	Status       string
	Error        string
}

type RuntimeHistoryTurn struct {
	ID             string
	Status         string
	Items          []map[string]any
	StartedAt      string
	CompletedAt    string
	Model          string
	Usage          *RuntimeTokenUsage
	UsageUpdatedAt string
	UpdatedAt      string
	Task           string
}

type RuntimeTokenUsage struct {
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
	Calls                 int64
}

type RuntimeHistory struct {
	Total int
	Turns []RuntimeHistoryTurn
}
