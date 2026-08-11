package hub

// RuntimeTokenUsage is the Runtime-neutral token accounting vocabulary used
// by Loom control-plane usage, activity, and workload views.
type RuntimeTokenUsage struct {
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
	Calls                 int64 `json:"calls"`
}

func (u *RuntimeTokenUsage) Add(other RuntimeTokenUsage) {
	u.InputTokens += other.InputTokens
	u.CachedInputTokens += other.CachedInputTokens
	u.OutputTokens += other.OutputTokens
	u.ReasoningOutputTokens += other.ReasoningOutputTokens
	u.TotalTokens += other.TotalTokens
	u.Calls += other.Calls
}

func (u RuntimeTokenUsage) Empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0
}

type RuntimeUsageEvent struct {
	Timestamp string            `json:"timestamp"`
	TurnID    string            `json:"turnId,omitempty"`
	Model     string            `json:"model,omitempty"`
	Usage     RuntimeTokenUsage `json:"usage"`
}

type RuntimeTurnUsage struct {
	TurnID        string            `json:"turnId"`
	Model         string            `json:"model,omitempty"`
	Usage         RuntimeTokenUsage `json:"usage"`
	LastUpdatedAt string            `json:"lastUpdatedAt,omitempty"`
}

type RuntimeTurnActivity struct {
	TurnID      string `json:"turnId"`
	StartedAt   string `json:"startedAt"`
	EndedAt     string `json:"endedAt,omitempty"`
	Status      string `json:"status"`
	InferredEnd bool   `json:"inferredEnd,omitempty"`
}

type RuntimeUsageReport struct {
	Lifetime           RuntimeTokenUsage     `json:"lifetime"`
	Events             []RuntimeUsageEvent   `json:"events"`
	Turns              []RuntimeTurnUsage    `json:"turns"`
	Activity           []RuntimeTurnActivity `json:"activity"`
	LatestCall         RuntimeTokenUsage     `json:"latestCall"`
	LatestModel        string                `json:"latestModel,omitempty"`
	ContextInputTokens int64                 `json:"contextInputTokens"`
	ModelContextWindow int64                 `json:"modelContextWindow"`
	LastUpdatedAt      string                `json:"lastUpdatedAt,omitempty"`
}
