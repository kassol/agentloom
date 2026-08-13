import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { OverviewPane } from "./OverviewPane";
import type { Agent } from "./types";

function agent(overrides: Partial<Agent>): Agent {
  return {
    id: "agent-1", name: "release", cwd: "/tmp", threadId: "thread-1", runtimeBinding: { kind: "pi" },
    capabilitySnapshot: { revision: "r1", capabilities: [] }, sandbox: "read-only", approvalPolicy: "never",
    status: "idle", currentTask: "", currentTurnId: "", lastError: "", createdAt: "2026-08-13T00:00:00Z",
    updatedAt: "2026-08-13T00:00:00Z", processAlive: true, pendingApprovals: [], lastSeq: 1,
    ...overrides,
  };
}

function renderOverview(agents: Agent[], callbacks: {
  onSelectAgent?: (id: string) => void;
  onOpenExternal?: () => void;
  onOpenMessages?: () => void;
  onOpenSchedules?: () => void;
  onOpenTopics?: (topicID?: string) => void;
} = {}) {
  vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
    const body = url.startsWith("/api/activity/daily") ? {
      activity: {
        date: "2026-08-13", timezone: "UTC", generatedAt: "2026-08-13T00:00:00Z", live: false,
        bucketMinutes: 30, activeAgents: 0, inactiveAgents: 0, trackedAgents: 0, totalAgents: 0,
        executingSeconds: 0, turnCount: 0, usage: {}, buckets: [], agents: [],
        dataQuality: { activityBasis: "test", tokenBasis: "test", limitations: [] },
      },
    } : { connections: [] };
    return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
  }));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><OverviewPane
    section="status" agents={agents} requests={[]} entries={[]} remote={null} onSectionChange={() => {}}
    onSelectAgent={callbacks.onSelectAgent || vi.fn()} onOpenNeedsYou={vi.fn()} onOpenExternal={callbacks.onOpenExternal || vi.fn()}
    onOpenMessages={callbacks.onOpenMessages || vi.fn()} onOpenSchedules={callbacks.onOpenSchedules || vi.fn()} onOpenTopics={callbacks.onOpenTopics || vi.fn()}
  /></QueryClientProvider>);
}

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe("Overview work disposition", () => {
  it("separates pending approvals, managed waits, and unclassified stops with direct guidance", async () => {
    const onSelectAgent = vi.fn();
    const onOpenMessages = vi.fn();
    renderOverview([
      agent({ id: "approval", name: "approver", pendingApprovals: [{ approvalId: "ap-1", agentId: "approval", runtimeKind: "pi", method: "shell", params: {}, status: "pending", requestedAt: "2026-08-13T00:00:00Z" }] }),
      agent({ id: "waiting", name: "researcher", workDisposition: { kind: "waiting_agent", threadId: "thread-1", turnId: "turn-1", recordedAt: "2026-08-13T00:00:00Z", wakeSources: [{ kind: "message", id: "msg-1", sourceTurnId: "turn-1", summary: "Review evidence" }] } }),
      agent({ id: "external", name: "release-monitor", workDisposition: { kind: "waiting_external", threadId: "thread-1", turnId: "turn-external", recordedAt: "2026-08-13T00:00:00Z", wakeSources: [{ kind: "trigger", id: "trg-1", sourceTurnId: "turn-external", summary: "Wait for deployment" }] } }),
      agent({ id: "stopped", name: "release", workDisposition: { kind: "unclassified", threadId: "thread-1", turnId: "turn-2", recordedAt: "2026-08-13T00:00:00Z", unfinished: [{ kind: "topic", id: "topic-1", summary: "Release remains active" }] } }),
    ], { onSelectAgent, onOpenMessages });

    expect(await screen.findByText("Pending approvals")).toBeInTheDocument();
    expect(screen.getByText("Waiting on Agent")).toBeInTheDocument();
    expect(screen.getByText("Missing durable next step")).toBeInTheDocument();
    expect(screen.getByText(/Create Needs You if Owner input is required/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Open Agent Message wait for researcher/ }));
    expect(onOpenMessages).toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: /Open external wait for release-monitor/ }));
    expect(onSelectAgent).toHaveBeenCalledWith("external");
    fireEvent.click(screen.getByRole("button", { name: /Open stopped Agent release/ }));
    expect(onSelectAgent).toHaveBeenCalledWith("stopped");
  });

  it("does not turn completed idle Agents or Needs You dispositions into duplicate attention items", async () => {
    renderOverview([
      agent({ id: "done", workDisposition: { kind: "completed", threadId: "thread-1", turnId: "turn-done", recordedAt: "2026-08-13T00:00:00Z" } }),
      agent({ id: "human", workDisposition: { kind: "needs_you", threadId: "thread-2", turnId: "turn-human", recordedAt: "2026-08-13T00:00:00Z", wakeSources: [{ kind: "human_request", id: "hrq-1", sourceTurnId: "turn-human" }] } }),
    ]);
    expect(await screen.findByText("No unclassified stopped work.")).toBeInTheDocument();
    expect(screen.queryByText("Missing durable next step")).not.toBeInTheDocument();
  });
});
