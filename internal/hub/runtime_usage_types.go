package hub

import (
	"sort"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

// RuntimeTokenUsage is the Runtime-neutral token accounting vocabulary used
// by Loom control-plane usage, activity, and workload views.
type RuntimeTokenUsage struct {
	InputTokens           int64                                 `json:"inputTokens"`
	CachedInputTokens     int64                                 `json:"cachedInputTokens"`
	OutputTokens          int64                                 `json:"outputTokens"`
	ReasoningOutputTokens int64                                 `json:"reasoningOutputTokens"`
	TotalTokens           int64                                 `json:"totalTokens"`
	Calls                 int64                                 `json:"calls"`
	CostMicros            int64                                 `json:"costMicros"`
	Metrics               map[string]RuntimeUsageMetricCoverage `json:"metrics"`
}

type RuntimeUsageMetricCoverage struct {
	Available bool     `json:"available"`
	Complete  bool     `json:"complete"`
	Sources   []string `json:"sources"`
}

func (u *RuntimeTokenUsage) Add(other RuntimeTokenUsage) {
	u.InputTokens += other.InputTokens
	u.CachedInputTokens += other.CachedInputTokens
	u.OutputTokens += other.OutputTokens
	u.ReasoningOutputTokens += other.ReasoningOutputTokens
	u.TotalTokens += other.TotalTokens
	u.Calls += other.Calls
	u.CostMicros += other.CostMicros
	if u.Metrics == nil {
		u.Metrics = map[string]RuntimeUsageMetricCoverage{}
	}
	for name, metric := range other.Metrics {
		current := u.Metrics[name]
		if _, initialized := u.Metrics[name]; !initialized {
			current = metric
		} else {
			current.Available = current.Available || metric.Available
			current.Complete = current.Complete && metric.Complete
			for _, source := range metric.Sources {
				if !containsUsageSource(current.Sources, source) {
					current.Sources = append(current.Sources, source)
				}
			}
		}
		sort.Strings(current.Sources)
		u.Metrics[name] = current
	}
}

func (u RuntimeTokenUsage) Empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0
}

type RuntimeUsageReport = runtimecontract.UsageReport
type RuntimeUsageEvent = runtimecontract.UsageEvent
type RuntimeTurnUsage = runtimecontract.TurnUsage
type RuntimeTurnActivity = runtimecontract.TurnActivity

func projectRuntimeTokenUsage(value runtimecontract.Usage) RuntimeTokenUsage {
	coverage := func(metric runtimecontract.UsageMetric) RuntimeUsageMetricCoverage {
		sources := []string{}
		if metric.Source != "" {
			sources = append(sources, metric.Source)
		}
		return RuntimeUsageMetricCoverage{Available: metric.Available, Complete: metric.Available, Sources: sources}
	}
	metrics := map[string]RuntimeUsageMetricCoverage{
		"inputTokens": coverage(value.InputTokens), "cachedInputTokens": coverage(value.CachedInputTokens),
		"outputTokens": coverage(value.OutputTokens), "reasoningOutputTokens": coverage(value.ReasoningOutputTokens),
		"totalTokens": coverage(value.TotalTokens), "calls": coverage(value.Calls), "costMicros": coverage(value.CostMicros),
	}
	return RuntimeTokenUsage{
		InputTokens: value.InputTokens.Value, CachedInputTokens: value.CachedInputTokens.Value,
		OutputTokens: value.OutputTokens.Value, ReasoningOutputTokens: value.ReasoningOutputTokens.Value,
		TotalTokens: value.TotalTokens.Value, Calls: value.Calls.Value, CostMicros: value.CostMicros.Value,
		Metrics: metrics,
	}
}

func containsUsageSource(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func emptyRuntimeTokenUsage(value runtimecontract.Usage) RuntimeTokenUsage {
	result := projectRuntimeTokenUsage(value)
	result.InputTokens, result.CachedInputTokens, result.OutputTokens = 0, 0, 0
	result.ReasoningOutputTokens, result.TotalTokens, result.Calls, result.CostMicros = 0, 0, 0, 0
	return result
}

func unavailableRuntimeTokenUsage(source string) RuntimeTokenUsage {
	metrics := map[string]RuntimeUsageMetricCoverage{}
	for _, name := range []string{"inputTokens", "cachedInputTokens", "outputTokens", "reasoningOutputTokens", "totalTokens", "calls", "costMicros"} {
		metrics[name] = RuntimeUsageMetricCoverage{Sources: []string{source}}
	}
	return RuntimeTokenUsage{Metrics: metrics}
}
