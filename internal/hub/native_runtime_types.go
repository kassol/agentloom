package hub

import "time"

// Native DTOs are adapter-private normalization values. They never cross the
// Runtime Contract or public HTTP/SSE boundary.
type nativeInputKind string

const (
	nativeInputText       nativeInputKind = "text"
	nativeInputLocalImage nativeInputKind = "local_image"
)

type nativeInput struct {
	Kind     nativeInputKind
	Text     string
	Path     string
	MimeType string
}

type nativeBindingRequest struct {
	NativeRef          string
	Name               string
	Cwd                string
	Sandbox            string
	ProviderID         string
	Model              string
	DisabledSkillPaths []string
}

type nativeTurnRequest struct {
	LoomTurnID     string
	NativeRef      string
	Input          []nativeInput
	ApprovalPolicy string
	Sandbox        string
	Model          string
	Effort         string
	Timeout        time.Duration
}

type nativeEventKind string

const (
	nativeTurnStarted        nativeEventKind = "turn_started"
	nativeUserInput          nativeEventKind = "user_input"
	nativeTextDelta          nativeEventKind = "text_delta"
	nativeTextCompleted      nativeEventKind = "text_completed"
	nativeReasoningDelta     nativeEventKind = "reasoning_delta"
	nativeReasoningCompleted nativeEventKind = "reasoning_completed"
	nativeToolStarted        nativeEventKind = "tool_started"
	nativeToolUpdated        nativeEventKind = "tool_updated"
	nativeToolCompleted      nativeEventKind = "tool_completed"
	nativeTurnCompleted      nativeEventKind = "turn_completed"
	nativeTurnFailed         nativeEventKind = "turn_failed"
	nativeTurnInterrupted    nativeEventKind = "turn_interrupted"
)

type nativeEvent struct {
	Kind         nativeEventKind
	LoomTurnID   string
	NativeRef    string
	NativeTurnID string
	ItemID       string
	Text         string
	Item         map[string]any
	Status       string
	Error        string
}

type nativeTokenUsage struct {
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
	Calls                 int64
}

type nativeHistoryTurn struct {
	ID             string
	Status         string
	Task           string
	Items          []map[string]any
	Model          string
	Usage          *nativeTokenUsage
	StartedAt      string
	CompletedAt    string
	UpdatedAt      string
	UsageUpdatedAt string
}

type nativeHistory struct {
	Turns []nativeHistoryTurn
	Total int
}
