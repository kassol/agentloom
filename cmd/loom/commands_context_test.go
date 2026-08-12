package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestContextExplainCLISelectsCanonicalTurn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents/agent/context/explain" || r.URL.Query().Get("turnId") != "turn one" {
			t.Errorf("request URL = %s", r.URL.String())
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"context": map[string]any{"agentName": "Agent", "turnId": "turn one", "state": "proven", "sources": []any{}}})
	}))
	defer server.Close()
	previousBase, previousColor := base, useColor
	base, useColor = server.URL, false
	defer func() { base, useColor = previousBase, previousColor }()
	cmdContext(args{positional: []string{"explain", "agent"}, flags: map[string]string{"turn": "turn one", "json": "true"}})
}

func TestTurnUsageMetricDistinguishesUnavailableFromObservedZero(t *testing.T) {
	if _, available := usageMetricValue(map[string]any{"available": false, "source": "runtime_unavailable"}); available {
		t.Fatal("unavailable typed metric was rendered as zero")
	}
	if value, available := usageMetricValue(map[string]any{"available": true, "value": float64(0), "source": "claude_agent_sdk"}); !available || value != 0 {
		t.Fatalf("observed zero = %v, %v", value, available)
	}
}

func TestTurnUsageSummaryShowsClaudeModelAndUnavailableTotal(t *testing.T) {
	turn := map[string]any{
		"usage":        map[string]any{"totalTokens": map[string]any{"available": false, "source": "runtime_unavailable"}},
		"usageDetails": map[string]any{"models": []any{map[string]any{"model": map[string]any{"available": true, "value": "claude-sonnet", "source": "claude_agent_sdk"}}}},
	}
	if line := turnModelUsageLine(turn); line != "model: claude-sonnet · tokens unavailable" {
		t.Fatalf("Claude Turn usage line = %q", line)
	}
	turn["usageDetails"] = map[string]any{"models": []any{map[string]any{"model": map[string]any{"available": true, "value": "claude-sonnet", "source": "claude_agent_sdk"}}, map[string]any{"model": map[string]any{"available": true, "value": "claude-haiku", "source": "claude_agent_sdk"}}}}
	if line := turnModelUsageLine(turn); line != "model: multiple models · tokens unavailable" {
		t.Fatalf("multi-model Claude Turn usage line = %q", line)
	}
	turn["usage"] = map[string]any{"totalTokens": map[string]any{"available": true, "value": float64(0), "source": "test"}}
	if line := turnModelUsageLine(turn); line != "model: multiple models · 0 tokens" {
		t.Fatalf("observed-zero Turn usage line = %q", line)
	}
}
