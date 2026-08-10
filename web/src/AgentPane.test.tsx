import { cleanup, fireEvent, render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Agent } from "./types";

const virtualizerHarness = vi.hoisted(() => ({
  options: [] as Array<Record<string, unknown>>,
  instance: {
    getTotalSize: vi.fn(() => 0),
    getVirtualItems: vi.fn(() => []),
    measureElement: vi.fn(),
    resizeItem: vi.fn(),
    scrollToIndex: vi.fn(),
  },
}));

vi.mock("@tanstack/react-virtual", () => ({
  useVirtualizer: (options: Record<string, unknown>) => {
    virtualizerHarness.options.push(options);
    return virtualizerHarness.instance;
  },
}));

import { AgentPane } from "./AgentPane";

const testAgent: Agent = {
  id: "agent-scroll",
  name: "agent-scroll",
  cwd: "/workspace/agent-scroll",
  threadId: "thread-scroll",
  runtimeBinding: { kind: "codex" },
  sandbox: "workspace-write",
  approvalPolicy: "on-request",
  status: "idle",
  currentTask: "",
  currentTurnId: "",
  lastError: "",
  createdAt: "2026-08-01T00:00:00Z",
  updatedAt: "2026-08-01T00:00:00Z",
  processAlive: true,
  pendingApprovals: [],
  lastSeq: 0,
};

const noOp = () => {};
const props = {
  agent: testAgent,
  modelProviders: [],
  configRequestNonce: 0,
  pendingWork: [],
  humanRequests: [],
  onOpenPendingWork: noOp,
  onOpenHumanRequest: noOp,
  onHumanRequestChanged: noOp,
  onPendingWorkChanged: noOp,
  onOpenUsage: noOp,
  onTrackTopic: noOp,
  onError: noOp,
  onAgentUpdated: noOp,
};

describe("AgentPane scroll restoration", () => {
  beforeEach(() => {
    virtualizerHarness.options.length = 0;
    vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => window.setTimeout(() => callback(performance.now()), 0));
    vi.stubGlobal("cancelAnimationFrame", (id: number) => window.clearTimeout(id));
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
      const body = url.includes("/thread/history") ? { total: 0, turns: [] } : { artifacts: [] };
      return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    }));
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    delete window.codexLoom;
    delete window.codexHub;
  });

  it("supplies the pane's last scrollTop when its virtualizer is re-enabled", () => {
    const view = render(<AgentPane {...props} active />);
    const feed = view.container.querySelector("main .overflow-y-auto") as HTMLDivElement;
    Object.defineProperties(feed, {
      clientHeight: { configurable: true, value: 600 },
      scrollHeight: { configurable: true, value: 2_000 },
    });
    feed.scrollTop = 900;
    fireEvent.scroll(feed);

    view.rerender(<AgentPane {...props} active={false} />);
    view.rerender(<AgentPane {...props} active />);

    const latestOptions = virtualizerHarness.options.at(-1);
    expect(latestOptions?.enabled).toBe(true);
    expect(latestOptions?.initialOffset).toBeTypeOf("function");
    expect((latestOptions?.initialOffset as () => number)()).toBe(900);
  });

  it("shows the immutable Runtime kind in the Inspector", () => {
	const view = render(<AgentPane {...props} active configRequestNonce={1} />);
	expect(view.getByText("Runtime kind").nextElementSibling).toHaveTextContent("codex");
	expect(view.queryByDisplayValue("codex")).toBeNull();
  });
});
