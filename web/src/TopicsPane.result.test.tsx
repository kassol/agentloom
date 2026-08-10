import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { TopicsPane } from "./TopicsPane";
import type { Topic, TopicSummary } from "./types";

const initialBrief = {
  version: 1,
  summary: "Participant evidence is pending.",
  updatedBy: "pi-topic-lead",
  updatedAt: "2026-08-10T01:00:00Z",
};
const progressBrief = {
  version: 2,
  summary: "Codex participant evidence has been integrated into progress.",
  currentState: "Client boundary verified",
  nextStep: "Reconcile final limitations",
  updatedBy: "pi-topic-lead",
  updatedAt: "2026-08-10T01:02:00Z",
};
const resultBrief = {
  version: 3,
  summary: "Final integrated result: the client boundary is verified.",
  currentState: "Ready for Owner review",
  limitations: "Server rollout remains outside this Topic.",
  updatedBy: "pi-topic-lead",
  updatedAt: "2026-08-10T01:03:00Z",
};

const topic: Topic = {
  id: "tpc-result",
  title: "Integrated mixed Runtime result",
  purpose: "Integrate bounded participant evidence",
  completionBoundary: "The Responsible Agent publishes the reconciled result",
  status: "active",
  responsibleAgentId: "pi-lead",
  responsibleAgent: "pi-topic-lead",
  participants: [{
    agentId: "codex-edge", agent: "codex-participant", responsibility: "Return bounded client evidence", joinedAt: "2026-08-10T01:00:00Z",
  }],
  currentBrief: resultBrief,
  briefHistory: [initialBrief, progressBrief, resultBrief],
  events: [
    { seq: 1, type: "created", actor: "pi-topic-lead", summary: "Topic created", createdAt: "2026-08-10T01:00:00Z" },
    { seq: 2, type: "message_replied", actor: "codex-participant", agentId: "codex-edge", agent: "codex-participant", summary: "Participant evidence verified", ref: { type: "message", id: "msg-reply" }, createdAt: "2026-08-10T01:01:00Z" },
    { seq: 3, type: "brief_updated", actor: "pi-topic-lead", summary: progressBrief.summary, createdAt: progressBrief.updatedAt },
    { seq: 4, type: "result_published", actor: "pi-topic-lead", summary: resultBrief.summary, createdAt: resultBrief.updatedAt },
  ],
  links: [{ type: "message", id: "msg-reply", relation: "evidence", label: "Participant evidence", linkedBy: "pi-topic-lead", createdAt: progressBrief.updatedAt }],
  resultReadyVersion: 3,
  ownerSeenBriefVersion: 0,
  version: 3,
  createdBy: "pi-lead",
  createdAt: "2026-08-10T01:00:00Z",
  updatedAt: resultBrief.updatedAt,
  needsMeCount: 0,
  resultsReady: true,
  activeTurns: [],
};

const summary: TopicSummary = {
  id: topic.id,
  title: topic.title,
  purpose: topic.purpose,
  status: topic.status,
  responsibleAgentId: topic.responsibleAgentId,
  responsibleAgent: topic.responsibleAgent,
  currentBrief: topic.currentBrief,
  resultReadyVersion: topic.resultReadyVersion,
  ownerSeenBriefVersion: topic.ownerSeenBriefVersion,
  version: topic.version,
  createdAt: topic.createdAt,
  updatedAt: topic.updatedAt,
  needsMeCount: 0,
  resultsReady: true,
  activeTurns: [],
};

describe("Pi Topic result experience", () => {
  beforeEach(() => {
    window.history.replaceState(null, "", "#topics?topic=tpc-result&view=history");
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
      if (url.includes("/api/topics/tpc-result/artifacts")) {
        return new Response(JSON.stringify({ artifacts: [] }), { status: 200, headers: { "Content-Type": "application/json" } });
      }
      if (url.includes("/api/topics/tpc-result")) {
        return new Response(JSON.stringify({ topic }), { status: 200, headers: { "Content-Type": "application/json" } });
      }
      return new Response(JSON.stringify({ topics: [summary] }), { status: 200, headers: { "Content-Type": "application/json" } });
    }));
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    window.history.replaceState(null, "", "#");
  });

  it("shows the Pi Responsible Agent's integrated progress, final result, and participant evidence without Runtime-native IDs", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(
      <QueryClientProvider client={client}>
        <TopicsPane agents={[]} createRequest={null} onCreateRequestHandled={() => {}} onOpenAgent={() => {}} onError={() => {}} />
      </QueryClientProvider>,
    );

    expect((await view.findAllByText(resultBrief.summary)).length).toBeGreaterThan(0);
    expect(view.getAllByText(progressBrief.summary).length).toBeGreaterThan(0);
    expect(view.getAllByText("Participant evidence verified").length).toBeGreaterThan(0);
    expect(document.body.textContent).toContain("pi-topic-lead");
    expect(view.getByText("result ready")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("pi-session-native");
    expect(document.body.textContent).not.toContain("pi-user-entry-native");
  });
});
