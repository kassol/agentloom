import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NeedsYouPane } from "./NeedsYouPane";
import type { HumanRequest } from "./types";

const request: HumanRequest = {
  id: "hrq-release",
  agentId: "agent-release",
  agentName: "release-agent",
  threadId: "loom-thread-release",
  sourceTurnId: "turn-upload",
  sourceTask: "Upload the release",
  expectation: "required",
  question: "Did the upload complete?",
  context: "The Runtime stopped after starting the upload.",
  blockedWork: "Publish the verified release",
  state: "open",
  deliveryStatus: "waiting",
  createdAt: "2026-08-10T00:00:00Z",
  updatedAt: "2026-08-10T00:00:00Z",
};
let currentRequest = request;

describe("NeedsYouPane causal detail", () => {
  beforeEach(() => {
    currentRequest = request;
    window.history.replaceState(null, "", "#needs-you?request=hrq-release");
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
      const body = url.endsWith("/hrq-release") ? { request: currentRequest } : { requests: [currentRequest] };
      return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    }));
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    window.history.replaceState(null, "", "#");
  });

  it("shows the Agent, Thread, predecessor, question, context, and blocked work", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(
      <QueryClientProvider client={client}>
        <NeedsYouPane requests={[request]} onChanged={() => {}} onOpenAgent={() => {}} onError={() => {}} />
      </QueryClientProvider>,
    );

    expect(await view.findByRole("heading", { name: request.question })).toBeInTheDocument();
    expect(view.getAllByText(request.agentName).length).toBeGreaterThan(0);
    expect(view.getByText(request.context!)).toBeInTheDocument();
    expect(view.getByText(request.blockedWork!)).toBeInTheDocument();
    expect(view.getByText(request.threadId!)).toBeInTheDocument();
    expect(view.getByText(request.sourceTurnId!)).toBeInTheDocument();
  });

  it("shows the answer, delivery, and resumed Turn", async () => {
    currentRequest = {
      ...request,
      state: "answered",
      answer: "The upload is visible; continue verification.",
      deliveryStatus: "delivered",
      resumedTurnId: "turn-verify",
      answeredAt: "2026-08-10T00:03:00Z",
      deliveredAt: "2026-08-10T00:04:00Z",
    };
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(
      <QueryClientProvider client={client}>
        <NeedsYouPane requests={[currentRequest]} onChanged={() => {}} onOpenAgent={() => {}} onError={() => {}} />
      </QueryClientProvider>,
    );

    expect(await view.findByText(currentRequest.answer!)).toBeInTheDocument();
    expect(view.getByText("Delivered to a new Turn")).toBeInTheDocument();
    expect(view.getByText(currentRequest.resumedTurnId!)).toBeInTheDocument();
  });
});
