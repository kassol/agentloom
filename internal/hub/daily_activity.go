package hub

import (
	"sort"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/rollout"
)

type DailyActivityAgentBucket struct {
	ExecutingSeconds int64              `json:"executingSeconds"`
	TurnCount        int                `json:"turnCount"`
	Usage            rollout.TokenUsage `json:"usage"`
}

type DailyActivityBucket struct {
	StartedAt        string             `json:"startedAt"`
	EndedAt          string             `json:"endedAt"`
	ObservedSeconds  int64              `json:"observedSeconds"`
	ActiveAgents     int                `json:"activeAgents"`
	ExecutingSeconds int64              `json:"executingSeconds"`
	TurnCount        int                `json:"turnCount"`
	Usage            rollout.TokenUsage `json:"usage"`
}

type DailyAgentActivity struct {
	AgentID          string                     `json:"agentId"`
	AgentName        string                     `json:"agentName"`
	Status           string                     `json:"status"`
	ExecutingSeconds int64                      `json:"executingSeconds"`
	TurnCount        int                        `json:"turnCount"`
	Usage            rollout.TokenUsage         `json:"usage"`
	FirstActiveAt    string                     `json:"firstActiveAt,omitempty"`
	LastActiveAt     string                     `json:"lastActiveAt,omitempty"`
	Buckets          []DailyActivityAgentBucket `json:"buckets"`
}

type DailyActivityDataQuality struct {
	ActivityBasis string   `json:"activityBasis"`
	TokenBasis    string   `json:"tokenBasis"`
	Limitations   []string `json:"limitations"`
}

type DailyActivityOverview struct {
	Date             string                   `json:"date"`
	Timezone         string                   `json:"timezone"`
	GeneratedAt      string                   `json:"generatedAt"`
	Live             bool                     `json:"live"`
	BucketMinutes    int                      `json:"bucketMinutes"`
	ActiveAgents     int                      `json:"activeAgents"`
	InactiveAgents   int                      `json:"inactiveAgents"`
	TrackedAgents    int                      `json:"trackedAgents"`
	TotalAgents      int                      `json:"totalAgents"`
	ExecutingSeconds int64                    `json:"executingSeconds"`
	TurnCount        int                      `json:"turnCount"`
	Usage            rollout.TokenUsage       `json:"usage"`
	Buckets          []DailyActivityBucket    `json:"buckets"`
	Agents           []DailyAgentActivity     `json:"agents"`
	DataQuality      DailyActivityDataQuality `json:"dataQuality"`
}

func (h *Hub) DailyActivity(start, endExclusive time.Time, bucketMinutes int) DailyActivityOverview {
	now := time.Now().In(start.Location())
	h.mu.Lock()
	agents := make([]AgentView, 0, len(h.agents))
	for _, meta := range h.agents {
		agents = append(agents, h.viewLocked(meta))
	}
	h.mu.Unlock()

	reports := make(map[string]*rollout.UsageReport, len(agents))
	for _, agent := range agents {
		if strings.TrimSpace(agent.ThreadID) == "" {
			continue
		}
		if report, err := rollout.ReadUsage(agent.ThreadID); err == nil {
			reports[agent.ID] = report
		}
	}
	return buildDailyActivity(agents, reports, start, endExclusive, now, bucketMinutes)
}

func buildDailyActivity(agents []AgentView, reports map[string]*rollout.UsageReport, start, endExclusive, now time.Time, bucketMinutes int) DailyActivityOverview {
	if bucketMinutes <= 0 {
		bucketMinutes = 30
	}
	now = now.In(start.Location())
	windowEnd := endExclusive
	if now.Before(windowEnd) {
		windowEnd = now
	}
	live := !now.Before(start) && now.Before(endExclusive)
	bucketDuration := time.Duration(bucketMinutes) * time.Minute
	bucketCount := int((endExclusive.Sub(start) + bucketDuration - 1) / bucketDuration)

	result := DailyActivityOverview{
		Date: start.Format("2006-01-02"), Timezone: usageTimezoneLabel(now),
		GeneratedAt: now.UTC().Format(time.RFC3339Nano), Live: live, BucketMinutes: bucketMinutes,
		TotalAgents: len(agents), Buckets: make([]DailyActivityBucket, bucketCount), Agents: []DailyAgentActivity{},
		DataQuality: DailyActivityDataQuality{
			ActivityBasis: "codex_rollout_turn_intervals", TokenBasis: "codex_rollout_token_deltas",
			Limitations: []string{
				"Execution includes model, tool, network, and approval waiting inside a Turn.",
				"Team execution is summed Agent time, so parallel Agents can exceed wall-clock duration.",
				"Agents without a readable Codex rollout are not included in activity rows.",
				"Token events are assigned to the bucket in which Codex recorded each usage delta.",
			},
		},
	}
	for index := range result.Buckets {
		bucketStart := start.Add(time.Duration(index) * bucketDuration)
		bucketEnd := bucketStart.Add(bucketDuration)
		if bucketEnd.After(endExclusive) {
			bucketEnd = endExclusive
		}
		observedEnd := bucketEnd
		if windowEnd.Before(observedEnd) {
			observedEnd = windowEnd
		}
		observedSeconds := int64(0)
		if observedEnd.After(bucketStart) {
			observedSeconds = maxDurationSeconds(observedEnd.Sub(bucketStart))
		}
		result.Buckets[index] = DailyActivityBucket{
			StartedAt: bucketStart.Format(time.RFC3339), EndedAt: bucketEnd.Format(time.RFC3339), ObservedSeconds: observedSeconds,
		}
	}

	for _, agent := range agents {
		report := reports[agent.ID]
		if report == nil {
			continue
		}
		result.TrackedAgents++
		row := DailyAgentActivity{
			AgentID: agent.ID, AgentName: agent.Name, Status: agent.Status,
			Buckets: make([]DailyActivityAgentBucket, bucketCount),
		}
		intervals := workloadIntervals(report.Activity, start, windowEnd)
		row.ExecutingSeconds = intervalDurationSeconds(intervals)

		var firstActive, lastActive time.Time
		markActive := func(activeStart, activeEnd time.Time) {
			if activeStart.Before(start) {
				activeStart = start
			}
			if activeEnd.After(windowEnd) {
				activeEnd = windowEnd
			}
			if activeEnd.Before(activeStart) {
				activeEnd = activeStart
			}
			if firstActive.IsZero() || activeStart.Before(firstActive) {
				firstActive = activeStart
			}
			if lastActive.IsZero() || activeEnd.After(lastActive) {
				lastActive = activeEnd
			}
		}
		for _, interval := range intervals {
			markActive(interval.start, interval.end)
		}
		for index := range result.Buckets {
			bucketStart := start.Add(time.Duration(index) * bucketDuration)
			bucketEnd := bucketStart.Add(bucketDuration)
			if bucketEnd.After(windowEnd) {
				bucketEnd = windowEnd
			}
			if !bucketEnd.After(bucketStart) {
				continue
			}
			row.Buckets[index].ExecutingSeconds = intervalDurationSeconds(clipWorkloadIntervals(intervals, bucketStart, bucketEnd))
		}
		for _, activity := range report.Activity {
			started, ok := workloadTime(activity.StartedAt)
			if !ok || started.Before(start) || !started.Before(windowEnd) {
				continue
			}
			index := int(started.Sub(start) / bucketDuration)
			if index < 0 || index >= len(row.Buckets) {
				continue
			}
			row.TurnCount++
			row.Buckets[index].TurnCount++
			markActive(started, started)
		}
		for _, event := range report.Events {
			timestamp, err := time.Parse(time.RFC3339Nano, event.Timestamp)
			if err != nil || timestamp.Before(start) || !timestamp.Before(windowEnd) {
				continue
			}
			index := int(timestamp.Sub(start) / bucketDuration)
			if index < 0 || index >= len(row.Buckets) {
				continue
			}
			row.Usage.Add(event.Usage)
			row.Buckets[index].Usage.Add(event.Usage)
			markActive(timestamp, timestamp)
		}
		if row.ExecutingSeconds == 0 && row.TurnCount == 0 && row.Usage.Calls == 0 && row.Usage.TotalTokens == 0 {
			continue
		}
		if !firstActive.IsZero() {
			row.FirstActiveAt = firstActive.UTC().Format(time.RFC3339Nano)
			row.LastActiveAt = lastActive.UTC().Format(time.RFC3339Nano)
		}
		for index := range row.Buckets {
			bucket := row.Buckets[index]
			if bucket.ExecutingSeconds > 0 || bucket.TurnCount > 0 || bucket.Usage.Calls > 0 || bucket.Usage.TotalTokens > 0 {
				result.Buckets[index].ActiveAgents++
			}
			result.Buckets[index].ExecutingSeconds += bucket.ExecutingSeconds
			result.Buckets[index].TurnCount += bucket.TurnCount
			result.Buckets[index].Usage.Add(bucket.Usage)
		}
		result.ExecutingSeconds += row.ExecutingSeconds
		result.TurnCount += row.TurnCount
		result.Usage.Add(row.Usage)
		result.Agents = append(result.Agents, row)
	}

	result.ActiveAgents = len(result.Agents)
	result.InactiveAgents = result.TotalAgents - result.ActiveAgents
	sort.SliceStable(result.Agents, func(i, j int) bool {
		if result.Agents[i].FirstActiveAt != result.Agents[j].FirstActiveAt {
			return result.Agents[i].FirstActiveAt < result.Agents[j].FirstActiveAt
		}
		if result.Agents[i].ExecutingSeconds != result.Agents[j].ExecutingSeconds {
			return result.Agents[i].ExecutingSeconds > result.Agents[j].ExecutingSeconds
		}
		return result.Agents[i].AgentName < result.Agents[j].AgentName
	})
	return result
}
