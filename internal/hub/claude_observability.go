package hub

import (
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

func claudeUsageFailure(err error) *runtimecontract.Failure {
	return &runtimecontract.Failure{Code: "usage_inspection_failed", Phase: runtimecontract.FailurePhaseUsageInspection, Message: "Claude usage could not be inspected", Diagnostic: err.Error(), Cause: err}
}

func projectClaudeUsage(turns []runtimecontract.HistoryTurn) runtimecontract.UsageReport {
	unavailableText := runtimecontract.UsageText{Source: "runtime_unavailable"}
	unavailableMetric := runtimecontract.UsageMetric{Source: "runtime_unavailable"}
	unavailableUsage := runtimecontract.Usage{InputTokens: unavailableMetric, CachedInputTokens: unavailableMetric, OutputTokens: unavailableMetric, ReasoningOutputTokens: unavailableMetric, TotalTokens: unavailableMetric, Calls: unavailableMetric, CostMicros: unavailableMetric}
	report := runtimecontract.UsageReport{Lifetime: unavailableUsage, LatestCall: unavailableUsage, LatestProvider: unavailableText, LatestModel: unavailableText, ContextInputTokens: unavailableMetric, ModelContextWindow: unavailableMetric, LastUpdatedAt: unavailableText, Events: []runtimecontract.UsageEvent{}, Turns: []runtimecontract.TurnUsage{}, Activity: []runtimecontract.TurnActivity{}}
	for _, turn := range turns {
		turnID := runtimecontract.UsageText{Available: true, Value: turn.TurnID, Source: "canonical_turn_ledger"}
		started := usageTime(turn.StartedAt, "canonical_turn_ledger")
		ended := usageTime(turn.CompletedAt, "canonical_turn_ledger")
		report.Activity = append(report.Activity, runtimecontract.TurnActivity{TurnID: turnID, StartedAt: started, EndedAt: ended, Status: runtimecontract.UsageText{Available: true, Value: string(turn.State), Source: "canonical_turn_ledger"}, InferredEnd: runtimecontract.UsageBool{Available: true, Source: "canonical_turn_ledger"}})
		if turn.Usage == nil {
			continue
		}
		observedAt := ended
		provider, model := unavailableText, unavailableText
		contextWindow := unavailableMetric
		if turn.UsageDetails != nil {
			if turn.UsageDetails.ObservedAt.Available {
				observedAt = turn.UsageDetails.ObservedAt
			}
			for _, item := range turn.UsageDetails.Models {
				report.Events = append(report.Events, runtimecontract.UsageEvent{Timestamp: observedAt, TurnID: turnID, Provider: item.Provider, Model: item.Model, Usage: item.Usage})
				if item.ContextWindow.Available && (!contextWindow.Available || item.ContextWindow.Value > contextWindow.Value) {
					contextWindow = item.ContextWindow
				}
			}
			if len(turn.UsageDetails.Models) == 1 {
				provider, model = turn.UsageDetails.Models[0].Provider, turn.UsageDetails.Models[0].Model
			}
		}
		if len(report.Events) == 0 || report.Events[len(report.Events)-1].TurnID.Value != turn.TurnID {
			report.Events = append(report.Events, runtimecontract.UsageEvent{Timestamp: observedAt, TurnID: turnID, Provider: provider, Model: model, Usage: *turn.Usage})
		}
		report.Lifetime.Add(*turn.Usage)
		report.LatestCall = *turn.Usage
		report.LatestProvider, report.LatestModel = provider, model
		report.ModelContextWindow = contextWindow
		report.LastUpdatedAt = observedAt
		report.Turns = append(report.Turns, runtimecontract.TurnUsage{TurnID: turnID, Provider: provider, Model: model, Usage: *turn.Usage, LastUpdatedAt: observedAt})
	}
	return report
}

func usageTime(value, source string) runtimecontract.UsageText {
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return runtimecontract.UsageText{Source: "runtime_unavailable"}
	}
	return runtimecontract.UsageText{Available: true, Value: value, Source: source}
}
