import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { DailyActivityTimeline } from "./DailyActivityTimeline";

afterEach(() => {
  vi.restoreAllMocks();
});

describe("DailyActivityTimeline", () => {
  it("renders aligned activity even when the API timezone field is a display label", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ activity: {
      date: "2026-07-21",
      timezone: "Asia/Shanghai (UTC+08:00)",
      generatedAt: "2026-07-21T08:00:00Z",
      live: true,
      bucketMinutes: 30,
      activeAgents: 1,
      inactiveAgents: 1,
      trackedAgents: 2,
      totalAgents: 2,
      executingSeconds: 900,
      turnCount: 1,
      usage: usage(1200, 1),
      buckets: [{ startedAt: "2026-07-20T16:00:00Z", endedAt: "2026-07-20T16:30:00Z", observedSeconds: 1800, activeAgents: 1, executingSeconds: 900, turnCount: 1, usage: usage(1200, 1) }],
      agents: [{ agentId: "agent-1", agentName: "research", status: "idle", executingSeconds: 900, turnCount: 1, usage: usage(1200, 1), buckets: [{ executingSeconds: 900, turnCount: 1, usage: usage(1200, 1) }] }],
      dataQuality: { activityBasis: "turns", tokenBasis: "events", limitations: ["Example limitation"] },
    } }), { status: 200, headers: { "Content-Type": "application/json" } }));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    render(<QueryClientProvider client={client}><DailyActivityTimeline onSelectAgent={() => {}} /></QueryClientProvider>);

    await waitFor(() => expect(screen.getByText("research")).toBeInTheDocument());
    expect(screen.getByText("1 active Agent · 15m execution · 1.20K tokens · 1 Turn")).toBeInTheDocument();
    expect(document.body.textContent).toContain("Asia/Shanghai (UTC+08:00)");
  });
});

function usage(totalTokens: number, calls: number) {
  return { inputTokens: totalTokens, cachedInputTokens: 0, outputTokens: 0, reasoningOutputTokens: 0, totalTokens, calls };
}
