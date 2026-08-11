package hub

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yan5xu/codex-loom/internal/runtimecontract"
)

func TestPiUsageInspectionReadsAllConsumedBranchesWithoutStartingRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	data := `{"type":"message","id":"user-1","timestamp":"2026-08-10T01:00:00Z","message":{"role":"user","content":"first"}}
{"type":"message","id":"assistant-active","parentId":"user-1","timestamp":"2026-08-10T01:00:01Z","message":{"role":"assistant","provider":"provider-a","model":"pi-model","content":"answer","stopReason":"stop","usage":{"input":10,"output":4,"cacheRead":2,"cacheWrite":3,"totalTokens":19,"reasoning":1,"cost":{"input":0.00001,"output":0.000004,"cacheRead":0.000002,"cacheWrite":0.000003,"total":0.000019}}}}
{"type":"message","id":"assistant-abandoned","parentId":"user-1","timestamp":"2026-08-10T01:00:02Z","message":{"role":"assistant","provider":"provider-a","model":"pi-model","content":"abandoned","stopReason":"aborted","usage":{"input":20,"output":5,"cacheRead":7,"cacheWrite":11,"totalTokens":43,"reasoning":2,"cost":{"total":0.000043}}}}
{"type":"compaction","id":"compact-1","parentId":"assistant-active","timestamp":"2026-08-10T01:00:03Z","summary":"compact","firstKeptEntryId":"user-1","tokensBefore":100,"usage":{"input":1,"output":1,"cacheRead":1,"cacheWrite":1,"totalTokens":4,"reasoning":0,"cost":{"total":0.000004}}}
{"type":"branch_summary","id":"summary-1","parentId":"compact-1","timestamp":"2026-08-10T01:00:04Z","fromId":"assistant-active","summary":"branch summary","usage":{"input":1,"output":2,"cacheRead":3,"cacheWrite":4,"reasoning":1,"cost":{"total":0.00001}}}
{"type":"message","id":"assistant-active","parentId":"user-1","timestamp":"2026-08-10T01:00:01Z","message":{"role":"assistant","provider":"provider-a","model":"pi-model","content":"duplicate","stopReason":"stop","usage":{"input":10,"output":4,"cacheRead":2,"cacheWrite":3,"totalTokens":19}}}
{"type":"message","id":"partial"`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	contract := newPiRuntimeContract("pi-passive", &piAgentRuntime{})
	report, failure := contract.InspectUsage(context.Background(), runtimecontract.Binding{
		SchemaVersion: runtimecontract.BindingSchemaVersion,
		RuntimeKind:   "pi",
		NativeRef:     path,
	})
	if failure != nil {
		t.Fatalf("InspectUsage: %v", failure)
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("usage report validation: %v", err)
	}
	if report.Lifetime.InputTokens.Value != 64 || report.Lifetime.CachedInputTokens.Value != 13 || report.Lifetime.OutputTokens.Value != 12 || report.Lifetime.TotalTokens.Value != 76 {
		t.Fatalf("lifetime = %#v", report.Lifetime)
	}
	if report.Events[0].Usage.InputTokens.Value != 15 || report.Events[0].Usage.TotalTokens.Value != 19 {
		t.Fatalf("real Pi fixture projection = %#v", report.Events[0].Usage)
	}
	if report.Lifetime.Calls.Available || report.Lifetime.CostMicros.Value != 76 || report.Lifetime.ReasoningOutputTokens.Value != 4 {
		t.Fatalf("native optional metrics = %#v", report.Lifetime)
	}
	if len(report.Events) != 4 || len(report.Turns) != 1 || len(report.Activity) != 1 {
		t.Fatalf("report rows = events %d turns %d activity %d", len(report.Events), len(report.Turns), len(report.Activity))
	}
	if report.Events[0].Timestamp.Value > report.Events[1].Timestamp.Value {
		t.Fatalf("events are not stable chronological order: %#v", report.Events)
	}
	if contract.native.rpc != nil {
		t.Fatal("passive usage inspection started Pi RPC")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`,"parentId":"assistant-active","timestamp":"2026-08-10T01:00:05Z","message":{"role":"toolResult","provider":"provider-b","model":"pi-model-2","usage":{"input":2,"output":1,"cacheRead":1,"cacheWrite":1,"totalTokens":5,"reasoning":0,"cost":{"total":0.000005}}}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	completed, failure := contract.InspectUsage(context.Background(), runtimecontract.Binding{SchemaVersion: 2, RuntimeKind: "pi", NativeRef: path})
	if failure != nil || len(completed.Events) != 5 || completed.Lifetime.TotalTokens.Value != 81 {
		t.Fatalf("completed trailing entry = %#v failure=%v", completed, failure)
	}
	again, failure := contract.InspectUsage(context.Background(), runtimecontract.Binding{SchemaVersion: 2, RuntimeKind: "pi", NativeRef: path})
	if failure != nil || len(again.Events) != 5 || again.Lifetime.TotalTokens.Value != 81 {
		t.Fatalf("re-read duplicated completed trailing entry = %#v failure=%v", again, failure)
	}

	history, historyFailure := contract.ReadHistory(context.Background(), runtimecontract.HistoryRequest{Binding: runtimecontract.Binding{SchemaVersion: 2, RuntimeKind: "pi", NativeRef: path}, Count: 20})
	if historyFailure != nil || len(history.Turns) != 1 {
		t.Fatalf("visible history must remain active-branch only: history=%#v failure=%v", history, historyFailure)
	}
}

func TestPiUsageKeepsAppendOrderOnTimestampTiesAndPersistedProviders(t *testing.T) {
	entries := []piSessionEntry{
		{ID: "user-z", Timestamp: "2026-08-10T01:00:00Z", Message: json.RawMessage(`{"role":"user"}`)},
		{ID: "answer-z", ParentID: "user-z", Timestamp: "2026-08-10T01:00:01Z", Message: json.RawMessage(`{"role":"assistant","provider":"provider-before","model":"same","stopReason":"stop","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2}}`)},
		{ID: "user-a", Timestamp: "2026-08-10T01:00:00Z", Message: json.RawMessage(`{"role":"user"}`)},
		{ID: "answer-a", ParentID: "user-a", Timestamp: "2026-08-10T01:00:01Z", Message: json.RawMessage(`{"role":"assistant","provider":"provider-after","model":"same","stopReason":"stop","usage":{"input":2,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":3}}`)},
	}
	report := projectPiUsage(entries)
	if len(report.Events) != 2 || report.Events[0].TurnID.Value != "user-z" || report.Events[1].TurnID.Value != "user-a" {
		t.Fatalf("equal timestamps reordered append records: %#v", report.Events)
	}
	if report.LatestCall.TotalTokens.Value != 3 || report.LatestProvider.Value != "provider-after" {
		t.Fatalf("latest usage ignored append order/provider: %#v", report)
	}
	agent := AgentView{Agent: Agent{ProviderID: "wrong-current", ProviderHistory: []ProviderBindingChange{{PreviousProviderID: "wrong-old", ProviderID: "wrong-current", SwitchedAt: "2026-08-10T01:00:01Z"}}, RuntimeBinding: RuntimeBinding{Kind: "pi"}}}
	usage := buildAgentUsageRange(agent, time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC), &report)
	if len(usage.Models) != 2 || usage.Models[0].ProviderID != "provider-after" || usage.Models[1].ProviderID != "provider-before" {
		t.Fatalf("Pi provider attribution used Agent configuration: %#v", usage.Models)
	}
}

func TestPiUsageActivityStartsAtEveryUserAndEndsOnlyOnTerminalEvidence(t *testing.T) {
	entries := []piSessionEntry{
		{ID: "user-only", Timestamp: "2026-08-10T01:00:00Z", Message: json.RawMessage(`{"role":"user"}`)},
		{ID: "user-tool", Timestamp: "2026-08-10T02:00:00Z", Message: json.RawMessage(`{"role":"user"}`)},
		{ID: "tool-use", ParentID: "user-tool", Timestamp: "2026-08-10T02:01:00Z", Message: json.RawMessage(`{"role":"assistant","provider":"p","model":"m","stopReason":"toolUse","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2}}`)},
		{ID: "user-pending", Timestamp: "2026-08-10T03:00:00Z", Message: json.RawMessage(`{"role":"user"}`)},
		{ID: "pending", ParentID: "user-pending", Timestamp: "2026-08-10T03:01:00Z", Message: json.RawMessage(`{"role":"assistant","provider":"p","model":"m","stopReason":"pending","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2}}`)},
		{ID: "user-deferred", Timestamp: "2026-08-10T04:00:00Z", Message: json.RawMessage(`{"role":"user"}`)},
		{ID: "deferred", ParentID: "user-deferred", Timestamp: "2026-08-10T04:01:00Z", Message: json.RawMessage(`{"role":"assistant","provider":"p","model":"m","stopReason":"deferred","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2}}`)},
		{ID: "user-failed", Timestamp: "2026-08-10T05:00:00Z", Message: json.RawMessage(`{"role":"user"}`)},
		{ID: "failed", ParentID: "user-failed", Timestamp: "2026-08-10T05:01:00Z", Message: json.RawMessage(`{"role":"assistant","provider":"p","model":"m","stopReason":"error"}`)},
		{ID: "user-aborted", Timestamp: "2026-08-10T06:00:00Z", Message: json.RawMessage(`{"role":"user"}`)},
		{ID: "aborted", ParentID: "user-aborted", Timestamp: "2026-08-10T06:01:00Z", Message: json.RawMessage(`{"role":"assistant","provider":"p","model":"m","stopReason":"aborted"}`)},
	}
	report := projectPiUsage(entries)
	if len(report.Activity) != 6 {
		t.Fatalf("activity rows = %#v", report.Activity)
	}
	wantStatus := []string{"running", "running", "running", "running", "failed", "interrupted"}
	for index, activity := range report.Activity {
		if activity.Status.Value != wantStatus[index] {
			t.Fatalf("activity %d status = %#v, want %s", index, activity, wantStatus[index])
		}
		wantEnded := index >= 4
		if activity.EndedAt.Available != wantEnded {
			t.Fatalf("activity %d ended availability = %#v", index, activity)
		}
	}
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 10, 7, 0, 0, 0, time.UTC)
	daily := buildDailyActivity([]AgentView{{Agent: Agent{ID: "pi", Name: "pi"}}}, map[string]*RuntimeUsageReport{"pi": &report}, start, start.AddDate(0, 0, 1), now, 60)
	if daily.ActiveAgents != 1 || daily.TurnCount != 6 || daily.ExecutingSeconds == 0 {
		t.Fatalf("Pi open activity missing from Daily Activity: %#v", daily)
	}
	workload := buildAgentWorkload(Agent{ID: "pi", Name: "pi", CreatedAt: start.Format(time.RFC3339Nano)}, nil, start, 1, now, &report)
	if !workload.ActivityAvailable || workload.TurnCount != 6 || workload.OpenTurns != 4 || workload.ExecutingSeconds == 0 {
		t.Fatalf("Pi open activity missing from Workload: %#v", workload)
	}
}
