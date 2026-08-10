import { cleanup, fireEvent, render, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Agent } from "./types";

const virtualizerHarness = vi.hoisted(() => ({
  options: [] as Array<Record<string, unknown>>,
  instance: {
    getTotalSize: vi.fn(() => 0),
    getVirtualItems: vi.fn((): any[] => []),
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
  runtimeCapabilities: {
    history: true, causalSteer: true, interrupt: true, goal: true, remote: true,
    usage: true, provider: true, compaction: true, approval: true, skills: true,
    naming: true, archive: true, sandbox: true, imageInput: true,
  },
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
	virtualizerHarness.instance.getVirtualItems.mockReturnValue([]);
	virtualizerHarness.instance.getTotalSize.mockReturnValue(0);
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

  it("shows Pi capability limits and disables unsupported Runtime controls", () => {
	const onError = vi.fn();
	vi.mocked(fetch).mockImplementation(async (input) => {
	  const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
	  if (url.includes("/runtime/models")) return new Response(JSON.stringify({
		current: { provider: "openai-codex", id: "gpt-5.4-mini", reasoning: true },
		models: [
		  { provider: "openai-codex", id: "gpt-5.4-mini", reasoning: true },
		  { provider: "xai", id: "grok-4.5", reasoning: true },
		],
	  }), { status: 200, headers: { "Content-Type": "application/json" } });
	  const body = url.includes("/thread/history") ? { total: 0, turns: [] } : url.includes("/config") ? { agent: piAgent } : { artifacts: [] };
	  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
	});
    const piAgent: Agent = {
      ...testAgent,
      runtimeBinding: { kind: "pi" },
      runtimeCapabilities: {
        history: true, causalSteer: false, interrupt: true, goal: false, remote: false,
        usage: false, provider: true, compaction: false, approval: true, skills: false,
        naming: false, archive: false, sandbox: false, imageInput: false,
      },
    };
    const view = render(<AgentPane {...props} agent={piAgent} onError={onError} active configRequestNonce={1} />);

    expect(view.getByText("Runtime capabilities")).toBeInTheDocument();
    expect(view.getByText("Image input").nextElementSibling).toHaveTextContent("Unavailable");
    expect(view.getByText("History").nextElementSibling).toHaveTextContent("Available");
    expect(view.getByText("Goal support").nextElementSibling).toHaveTextContent("Unavailable");
    expect(view.getByDisplayValue("agent-scroll")).toBeEnabled();
    expect(view.getByText("Provider switching").nextElementSibling).toHaveTextContent("Available");
	const provider = view.getByText("Provider").closest("label")?.querySelector("select") as HTMLSelectElement;
	const model = view.getByText("Model").closest("label")?.querySelector("select") as HTMLSelectElement;
    expect(view.getByText("Sandbox").closest("label")?.querySelector("select")).toBeDisabled();
	expect(view.getByText("Approval Policy").closest("label")?.querySelector("select")).toBeEnabled();
	expect(view.getByText("Sandbox isolation is unsupported for the pi Runtime.")).toBeInTheDocument();
	expect(view.getByText(/Approval controls individual tool actions only/)).toBeInTheDocument();
    expect(view.getByRole("button", { name: "Usage" })).toBeDisabled();
    expect(view.queryByText("History is unavailable for the pi Runtime.")).toBeNull();
    expect(view.getByText("Goal is unavailable for the pi Runtime.")).toBeInTheDocument();
	expect(view.queryByRole("button", { name: "Set a Goal" })).toBeNull();

	return waitFor(() => {
	  expect(provider).toBeEnabled();
	  expect(model).toBeEnabled();
	  expect(provider).toHaveValue("openai-codex");
	}).then(async () => {
	  fireEvent.change(provider, { target: { value: "xai" } });
	  expect(model).toHaveValue("grok-4.5");
	  fireEvent.click(view.getByRole("button", { name: "Save" }));
	  await waitFor(() => expect(vi.mocked(fetch).mock.calls).toContainEqual([
		"/api/agents/agent-scroll/runtime/model",
		expect.objectContaining({ method: "POST", body: JSON.stringify({ provider: "xai", model: "grok-4.5" }) }),
	  ]));

	  const task = view.getByRole("textbox", { name: "task message" });
	  fireEvent.change(task, { target: { value: "/compact" } });
	  fireEvent.click(view.getByRole("button", { name: "send task" }));
	  expect(onError).toHaveBeenCalledWith("Manual compaction is unavailable for the pi Runtime");

	  const requestedURLs = vi.mocked(fetch).mock.calls.map(([input]) => typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url);
	  expect(requestedURLs.some((url) => url.includes("/thread/history"))).toBe(true);
	  expect(requestedURLs.some((url) => url.includes("/compact"))).toBe(false);
	});
  });

  it("restores a Pi Approval card from the Agent snapshot without Codex wording", async () => {
	virtualizerHarness.instance.getVirtualItems.mockReturnValue([
	  { index: 0, key: "approval:ap-agent-scroll-a1", start: 0, size: 120, end: 120, lane: 0 },
	]);
	virtualizerHarness.instance.getTotalSize.mockReturnValue(120);
	const piAgent: Agent = {
	  ...testAgent,
	  runtimeBinding: { kind: "pi" },
	  pendingApprovals: [{
		approvalId: "ap-agent-scroll-a1", agentId: "agent-scroll", turnId: "turn-1", runtimeKind: "pi",
		method: "tool/bash", params: { command: "pwd" }, status: "pending", requestedAt: "2026-08-10T00:00:00Z",
	  }],
	};
	const view = render(<AgentPane {...props} agent={piAgent} active />);

	expect(await view.findByText("pi Runtime requests approval")).toBeInTheDocument();
	expect(view.queryByText("codex requests approval")).toBeNull();
	fireEvent.click(view.getByRole("button", { name: "approve" }));
	fireEvent.click(view.getByRole("button", { name: "reject" }));
	await waitFor(() => {
	  const calls = vi.mocked(fetch).mock.calls;
	  expect(calls).toContainEqual([
		"/api/agents/agent-scroll/thread/approvals/ap-agent-scroll-a1",
		expect.objectContaining({ method: "POST", body: JSON.stringify({ decision: "approve" }) }),
	  ]);
	  expect(calls).toContainEqual([
		"/api/agents/agent-scroll/thread/approvals/ap-agent-scroll-a1",
		expect.objectContaining({ method: "POST", body: JSON.stringify({ decision: "deny" }) }),
	  ]);
	});
  });
});
