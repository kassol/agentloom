package hub

import (
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/rollout"
)

func TestBuildDailyActivitySplitsExecutionAndTokensIntoAlignedBuckets(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	start := time.Date(2026, 7, 21, 0, 0, 0, 0, location)
	endExclusive := start.AddDate(0, 0, 1)
	now := start.Add(75 * time.Minute)
	agents := []AgentView{
		{Agent: Agent{ID: "agent-a", Name: "alpha", Status: "running"}},
		{Agent: Agent{ID: "agent-b", Name: "beta", Status: "idle"}},
		{Agent: Agent{ID: "agent-c", Name: "inactive", Status: "idle"}},
	}
	reports := map[string]*rollout.UsageReport{
		"agent-a": {
			Activity: []rollout.TurnActivity{
				{TurnID: "turn-a1", StartedAt: start.Add(20 * time.Minute).UTC().Format(time.RFC3339Nano), EndedAt: start.Add(40 * time.Minute).UTC().Format(time.RFC3339Nano)},
				{TurnID: "turn-a2", StartedAt: start.Add(50 * time.Minute).UTC().Format(time.RFC3339Nano)},
			},
			Events: []rollout.UsageEvent{
				{Timestamp: start.Add(25 * time.Minute).UTC().Format(time.RFC3339Nano), Usage: rollout.TokenUsage{TotalTokens: 100, Calls: 1}},
				{Timestamp: start.Add(65 * time.Minute).UTC().Format(time.RFC3339Nano), Usage: rollout.TokenUsage{TotalTokens: 250, Calls: 1}},
			},
		},
		"agent-b": {
			Activity: []rollout.TurnActivity{{TurnID: "turn-b1", StartedAt: start.Add(10 * time.Minute).UTC().Format(time.RFC3339Nano), EndedAt: start.Add(70 * time.Minute).UTC().Format(time.RFC3339Nano)}},
		},
		"agent-c": {},
	}

	activity := buildDailyActivity(agents, reports, start, endExclusive, now, 30)
	if !activity.Live || activity.BucketMinutes != 30 || len(activity.Buckets) != 48 {
		t.Fatalf("activity header = %#v", activity)
	}
	if activity.ActiveAgents != 2 || activity.InactiveAgents != 1 || activity.TrackedAgents != 3 {
		t.Fatalf("agent counts = active %d inactive %d tracked %d", activity.ActiveAgents, activity.InactiveAgents, activity.TrackedAgents)
	}
	if activity.ExecutingSeconds != 105*60 {
		t.Fatalf("executing = %d, want 105 agent-minutes", activity.ExecutingSeconds)
	}
	if activity.TurnCount != 3 || activity.Usage.TotalTokens != 350 || activity.Usage.Calls != 2 {
		t.Fatalf("totals = turns %d usage %#v", activity.TurnCount, activity.Usage)
	}
	if activity.Buckets[0].ObservedSeconds != 30*60 || activity.Buckets[1].ObservedSeconds != 30*60 || activity.Buckets[2].ObservedSeconds != 15*60 || activity.Buckets[3].ObservedSeconds != 0 {
		t.Fatalf("observed buckets = %d %d %d %d", activity.Buckets[0].ObservedSeconds, activity.Buckets[1].ObservedSeconds, activity.Buckets[2].ObservedSeconds, activity.Buckets[3].ObservedSeconds)
	}
	if activity.Buckets[0].ExecutingSeconds != 30*60 || activity.Buckets[1].ExecutingSeconds != 50*60 || activity.Buckets[2].ExecutingSeconds != 25*60 {
		t.Fatalf("team bucket execution = %d %d %d", activity.Buckets[0].ExecutingSeconds, activity.Buckets[1].ExecutingSeconds, activity.Buckets[2].ExecutingSeconds)
	}
	if activity.Buckets[0].Usage.TotalTokens != 100 || activity.Buckets[2].Usage.TotalTokens != 250 {
		t.Fatalf("token buckets = %#v / %#v", activity.Buckets[0].Usage, activity.Buckets[2].Usage)
	}
	if activity.Agents[0].AgentID != "agent-b" || activity.Agents[1].AgentID != "agent-a" {
		t.Fatalf("agents should sort by first activity: %#v", activity.Agents)
	}
	alpha := activity.Agents[1]
	if alpha.Buckets[0].ExecutingSeconds != 10*60 || alpha.Buckets[1].ExecutingSeconds != 20*60 || alpha.Buckets[2].ExecutingSeconds != 15*60 {
		t.Fatalf("alpha execution buckets = %#v", alpha.Buckets[:3])
	}
}

func TestBuildDailyActivityHistoricalDayUsesFullWindow(t *testing.T) {
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	activity := buildDailyActivity(
		[]AgentView{{Agent: Agent{ID: "agent-a", Name: "alpha"}}},
		map[string]*rollout.UsageReport{"agent-a": {}},
		start,
		start.AddDate(0, 0, 1),
		start.AddDate(0, 0, 2),
		30,
	)
	if activity.Live || activity.Buckets[47].ObservedSeconds != 30*60 {
		t.Fatalf("historical activity = %#v", activity)
	}
}
