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
import { publishThreadEvent } from "./thread-events";

const testScope = { runtimeKind: "codex", bindingRevision: "binding", model: "model", configurationRevision: "config" };

function capabilitySnapshot(...ids: string[]): Agent["capabilitySnapshot"] {
  return {
    revision: "test-snapshot",
    capabilities: ids.map((id) => ({ id, availability: "available", revision: "test", scope: testScope })),
  };
}

const testAgent: Agent = {
  id: "agent-scroll",
  name: "agent-scroll",
  cwd: "/workspace/agent-scroll",
  threadId: "thread-scroll",
  runtimeBinding: { kind: "codex" },
  capabilitySnapshot: capabilitySnapshot("sandbox_configuration", "approval_policy", "model_configuration", "goal", "remote", "usage_reporting", "manual_compaction", "image_input"),
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

  it("lets the Owner take over scrolling while an unpinned Feed is settling", async () => {
	virtualizerHarness.instance.getVirtualItems.mockReturnValue([
	  { index: 0, key: "row-0", start: 0, size: 1_000, end: 1_000, lane: 0 },
	]);
	virtualizerHarness.instance.getTotalSize.mockReturnValue(1_000);
	vi.mocked(fetch).mockImplementation(async (input) => {
	  const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
	  const body = url.includes("/thread/history")
		? { total: 1, turns: [{ turnId: "turn-latest", state: "completed", content: [{ id: "content-latest", kind: "user_text", text: "Latest work" }] }] }
		: { artifacts: [] };
	  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
	});
	const view = render(<AgentPane {...props} active />);
	await view.findByText("Latest work");
	const feed = view.container.querySelector("main .overflow-y-auto") as HTMLDivElement;
	let scrollTop = 400;
	Object.defineProperties(feed, {
	  clientHeight: { configurable: true, value: 600 },
	  scrollHeight: { configurable: true, value: 2_000 },
	  scrollTop: {
		configurable: true,
		get: () => scrollTop,
		set: (value: number) => { scrollTop = value; },
	  },
	});
	fireEvent.scroll(feed);
	await new Promise((resolve) => window.setTimeout(resolve, 10));

	publishThreadEvent(testAgent.id, {
	  seq: 1,
	  ts: "2026-08-10T00:00:01Z",
	  type: "loom/runtime-event",
	  data: { kind: "content", turnId: "turn-latest", contentPhase: "delta", content: { id: "answer-1", kind: "assistant_text", text: "streaming" } },
	});
	await new Promise((resolve) => window.setTimeout(resolve, 10));
	scrollTop = 250;
	fireEvent.scroll(feed);
	await new Promise((resolve) => window.setTimeout(resolve, 180));

	expect(scrollTop).toBe(250);
  });

	it("reconciles the current history page after a Turn is interrupted", async () => {
	render(<AgentPane {...props} active />);
	await waitFor(() => expect(fetch).toHaveBeenCalledWith(expect.stringContaining("/thread/history?count=25&offset=0"), expect.anything()));
	vi.mocked(fetch).mockClear();

	publishThreadEvent(testAgent.id, {
	  seq: 2,
	  ts: "2026-08-10T00:00:02Z",
	  type: "loom/turn-interrupted",
	  data: { turnId: "turn-2" },
	});

	await waitFor(() => expect(fetch).toHaveBeenCalledWith(expect.stringContaining("/thread/history?count=25&offset=0"), expect.anything()));
  });

  it("shows the immutable Runtime kind in the Inspector", () => {
	const view = render(<AgentPane {...props} active configRequestNonce={1} />);
	expect(view.getByText("Runtime kind").nextElementSibling).toHaveTextContent("codex");
	expect(view.queryByDisplayValue("codex")).toBeNull();
  });

  it("shows Runtime-neutral context evidence for the relevant Loom Turn", async () => {
	vi.mocked(fetch).mockImplementation(async (input) => {
	  const url = typeof input === "string" ? input : input instanceof URL ? input.pathname + input.search : input.url;
	  const body = url.includes("/context/explain") ? { context: {
		agentId: testAgent.id, agentName: testAgent.name, threadId: testAgent.threadId, turnId: "turn-last",
		state: "proven", mode: "full_per_turn", policy: "Full delivery", reason: "", limitation: "No epoch evidence.",
		sources: [{ key: "loom_agent_profile", revision: "profile:2", channel: "developer", state: "delivered", covered: true }],
		deliveries: [], unsupportedDimensions: ["epoch", "replay"],
	  }} : url.includes("/thread/history") ? { total: 0, turns: [] } : { artifacts: [] };
	  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
	});
	const agent = { ...testAgent, lastTurn: { turnId: "turn-last", task: "done", status: "completed", completedAt: "2026-08-12T00:00:00Z" } };
	const view = render(<AgentPane {...props} agent={agent} active configRequestNonce={1} />);
	fireEvent.click(view.getByRole("button", { name: "Context" }));
	await view.findByText("loom_agent_profile");
	expect(view.getByText("proven")).toBeInTheDocument();
	expect(view.getByText(/full_per_turn · Turn turn-last/)).toBeInTheDocument();
	expect(fetch).toHaveBeenCalledWith(expect.stringContaining("/context/explain?turnId=turn-last"), expect.anything());
  });

  it("shows Pi capability limits and disables unsupported Runtime controls", () => {
	const onError = vi.fn();
	vi.mocked(fetch).mockImplementation(async (input) => {
	  const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
	  if (url.includes("/runtime/models")) return new Response(JSON.stringify({
		current: { provider: "openai-codex", id: "gpt-5.4-mini", reasoning: true },
		thinkingLevel: "medium",
		thinkingLevels: ["off", "minimal", "low", "medium", "high"],
		models: [
		  { provider: "openai-codex", id: "gpt-5.4-mini", reasoning: true, thinkingLevels: ["off", "minimal", "low", "medium", "high", "xhigh"], imageInput: false },
		  { provider: "xai", id: "grok-4.5", reasoning: true, thinkingLevels: ["low", "high"], defaultThinkingLevel: "high", imageInput: true },
		  { provider: "xai", id: "grok-build-0.1", reasoning: false, thinkingLevels: ["off"], imageInput: false },
		],
	  }), { status: 200, headers: { "Content-Type": "application/json" } });
	  const body = url.includes("/thread/history") ? { total: 0, turns: [] } : url.includes("/config") ? { agent: piAgent } : { artifacts: [] };
	  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
	});
    const piAgent: Agent = {
      ...testAgent,
      runtimeBinding: { kind: "pi" },
	  processAlive: false,
      capabilitySnapshot: capabilitySnapshot("approval_policy", "model_configuration", "context_delivery", "goal"),
    };
    const view = render(<AgentPane {...props} agent={piAgent} onError={onError} active configRequestNonce={1} />);

    expect(view.getByText("Runtime capabilities")).toBeInTheDocument();
    expect(view.getByText("Image input").nextElementSibling).toHaveTextContent("Checked on start");
    expect(view.getByText("History").nextElementSibling).toHaveTextContent("Available");
    expect(view.getByText("Goal support").nextElementSibling).toHaveTextContent("Available");
    expect(view.getByDisplayValue("agent-scroll")).toBeEnabled();
	expect(view.queryByText("Provider configuration")).toBeNull();
	const providerSelect = () => view.getByText("Provider").closest("label")?.querySelector("select") as HTMLSelectElement;
	const modelSelect = () => view.getByText("Model").closest("label")?.querySelector("select") as HTMLSelectElement;
	const thinkingSelect = () => view.getByText("Thinking Effort").closest("label")?.querySelector("select") as HTMLSelectElement;
    expect(view.getByText("Sandbox").closest("label")?.querySelector("select")).toBeDisabled();
	expect(view.getByText("Approval Policy").closest("label")?.querySelector("select")).toBeEnabled();
	expect(view.getByText("Sandbox isolation is unsupported for the pi Runtime.")).toBeInTheDocument();
	expect(view.getByText(/Approval controls individual tool actions only/)).toBeInTheDocument();
    expect(view.getByRole("button", { name: "Usage" })).toBeDisabled();
    expect(view.queryByText("History is unavailable for the pi Runtime.")).toBeNull();
    expect(view.queryByText("Goal is unavailable for the pi Runtime.")).toBeNull();
	expect(view.getByRole("button", { name: "Set a Goal" })).toBeInTheDocument();

	return waitFor(() => {
	  expect(providerSelect()).toBeEnabled();
	  expect(modelSelect()).toBeEnabled();
	  expect(thinkingSelect()).toBeEnabled();
	  expect(providerSelect()).toHaveValue("openai-codex");
	  expect(thinkingSelect()).toHaveValue("medium");
	}).then(async () => {
	  fireEvent.change(providerSelect(), { target: { value: "xai" } });
	  expect(modelSelect()).toHaveValue("grok-4.5");
	  expect(view.getByText("Image input").nextElementSibling).toHaveTextContent("Available after Save");
	  expect(Array.from(thinkingSelect().options).map((option) => option.value)).toEqual(["low", "high"]);
	  expect(thinkingSelect()).toHaveValue("high");
	  fireEvent.change(modelSelect(), { target: { value: "grok-build-0.1" } });
	  expect(view.getByText("Image input").nextElementSibling).toHaveTextContent("Unavailable after Save");
	  expect(Array.from(thinkingSelect().options).map((option) => option.value)).toEqual(["off"]);
	  expect(thinkingSelect()).toHaveValue("off");
	  fireEvent.change(modelSelect(), { target: { value: "grok-4.5" } });
	  expect(view.getByText("Image input").nextElementSibling).toHaveTextContent("Available after Save");
	  expect(Array.from(thinkingSelect().options).map((option) => option.value)).toEqual(["low", "high"]);
	  expect(thinkingSelect()).toHaveValue("high");
	  fireEvent.change(thinkingSelect(), { target: { value: "high" } });
	  fireEvent.click(view.getByRole("button", { name: "Save Runtime Model" }));
	  await waitFor(() => expect(vi.mocked(fetch).mock.calls).toContainEqual([
		"/api/agents/agent-scroll/runtime/model",
		expect.objectContaining({ method: "POST", body: JSON.stringify({ provider: "xai", model: "grok-4.5", thinkingLevel: "high" }) }),
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

  it("rejects unsupported images when they are attached but keeps other files", () => {
	const onError = vi.fn();
	const piAgent: Agent = {
	  ...testAgent,
	  runtimeBinding: { kind: "pi" },
	  capabilitySnapshot: capabilitySnapshot("approval_policy", "model_configuration", "context_delivery"),
	};
	const view = render(<AgentPane {...props} agent={piAgent} onError={onError} active />);
	const input = view.container.querySelector('input[type="file"]') as HTMLInputElement;
	const image = new File(["image"], "screen.png", { type: "image/png", lastModified: 1 });
	const text = new File(["notes"], "notes.txt", { type: "text/plain", lastModified: 2 });

	fireEvent.change(input, { target: { files: [image, text] } });

	expect(onError).toHaveBeenCalledWith("screen.png cannot be attached because the current pi model does not support image input");
	expect(view.queryByText("screen.png")).toBeNull();
	expect(view.getByText("notes.txt")).toBeInTheDocument();
  });

  it("refreshes an open Inspector from a live Capability Snapshot", async () => {
	const before: Agent = {
	  ...testAgent,
	  capabilitySnapshot: { revision: "revision-1", capabilities: [] },
	};
	const view = render(<AgentPane {...props} agent={before} active configRequestNonce={1} />);
	expect(await view.findByText("snapshot revision-1")).toBeInTheDocument();
	view.rerender(<AgentPane {...props} agent={{
	  ...before,
	  capabilitySnapshot: { revision: "revision-2", capabilities: [] },
	}} active configRequestNonce={1} />);
	expect(view.getByText("snapshot revision-2")).toBeInTheDocument();
	expect(view.getByText("Image input").nextElementSibling).toHaveTextContent("Unavailable");
  });

  it("refetches typed model control while the Runtime Inspector stays open", async () => {
	let modelReads = 0;
	vi.mocked(fetch).mockImplementation(async (input) => {
	  const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
	  if (url.includes("/runtime/models")) {
		modelReads++;
		const id = modelReads === 1 ? "model-a" : "model-b";
		return new Response(JSON.stringify({
		  current: { provider: "fixture", id, thinkingLevels: ["off"], imageInput: modelReads > 1 },
		  models: [
			{ provider: "fixture", id, thinkingLevels: ["off"], imageInput: modelReads > 1 },
			...(modelReads > 1 ? [{ provider: "fixture", id: "model-text", thinkingLevels: ["off"], imageInput: false }] : []),
		  ],
		  thinkingLevel: "off", thinkingLevels: ["off"],
		}), { status: 200, headers: { "Content-Type": "application/json" } });
	  }
	  const body = url.includes("/thread/history") ? { total: 0, turns: [] } : { artifacts: [] };
	  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
	});
	const snapshot = (revision: string, model: string): Agent["capabilitySnapshot"] => ({ revision, capabilities: [
	  { id: "model_configuration", availability: "available", revision, scope: { runtimeKind: "pi", bindingRevision: "b", model, configurationRevision: revision } },
	  { id: "image_input", availability: model === "model-b" ? "available" : "unavailable", reason: "text only", alternative: "choose vision", revision, scope: { runtimeKind: "pi", bindingRevision: "b", model, configurationRevision: revision } },
	] });
	const before: Agent = { ...testAgent, runtimeBinding: { kind: "pi" }, model: "model-a", capabilitySnapshot: snapshot("revision-a", "model-a") };
	const view = render(<AgentPane {...props} agent={before} active configRequestNonce={1} />);
	await waitFor(() => expect(view.getByText("Model").closest("label")?.querySelector("select")).toHaveValue("model-a"));
	view.rerender(<AgentPane {...props} agent={{ ...before, model: "model-b", capabilitySnapshot: snapshot("revision-b", "model-b") }} active configRequestNonce={1} />);
	await waitFor(() => {
	  expect(modelReads).toBe(2);
	  expect(view.getByText("Model").closest("label")?.querySelector("select")).toHaveValue("model-b");
	  expect(view.getByText("Image input").nextElementSibling).toHaveTextContent("Available");
	});
	const fileInput = view.container.querySelector('input[type="file"]') as HTMLInputElement;
	fireEvent.change(fileInput, { target: { files: [new File(["image"], "active.png", { type: "image/png", lastModified: 3 })] } });
	expect(view.getByText("active.png")).toBeInTheDocument();
	fireEvent.change(view.getByText("Model").closest("label")?.querySelector("select") as HTMLSelectElement, { target: { value: "model-text" } });
	expect(view.getByText("Remove attached images before saving this text-only model.")).toBeInTheDocument();
	expect(view.getByRole("button", { name: "Save Runtime Model" })).toBeDisabled();
  });

  it("shows Runtime-specific resources and patches policy with the inspected revision", async () => {
	const resourceAgent: Agent = {
	  ...testAgent,
	  processAlive: true,
	  capabilitySnapshot: capabilitySnapshot("resource_inventory", "resource_policy"),
	};
	const snapshot = {
	  agentId: resourceAgent.id, agentName: resourceAgent.name, runtimeKind: "codex", revision: "resources-1",
	  semantics: "Codex-native Skills; Runtime-specific paths and policy",
	  resources: [{ id: "skill:/tmp/review/SKILL.md", name: "review", kind: "skill", path: "/tmp/review/SKILL.md", enabled: true }],
	  policy: { available: true, mutable: true, effective: true, disabledPaths: [], evidence: [{ kind: "native_ack", summary: "exact policy acknowledged" }] },
	};
	vi.mocked(fetch).mockImplementation(async (input, init) => {
	  const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
	  if (url.endsWith("/skills") || url.endsWith("/skills/config")) {
		if (init?.method === "PATCH") {
		  expect(JSON.parse(String(init.body))).toEqual({ path: "/tmp/review/SKILL.md", enabled: false, expectedRevision: "resources-1" });
		  return new Response(JSON.stringify({ resources: { ...snapshot, revision: "resources-2", resources: [{ ...snapshot.resources[0], enabled: false }], policy: { ...snapshot.policy, mutable: false, reason: "binding is loaded", alternative: "restart and reopen Resources" } } }), { status: 200, headers: { "Content-Type": "application/json" } });
		}
		return new Response(JSON.stringify({ resources: snapshot }), { status: 200, headers: { "Content-Type": "application/json" } });
	  }
	  const body = url.includes("/thread/history") ? { total: 0, turns: [] } : { artifacts: [] };
	  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
	});
	const view = render(<AgentPane {...props} agent={resourceAgent} active configRequestNonce={1} />);
	fireEvent.click(view.getByRole("button", { name: "Resources" }));
	expect(await view.findByText("Codex-native Skills; Runtime-specific paths and policy")).toBeInTheDocument();
	expect(view.getByText("Runtime-specific semantics")).toBeInTheDocument();
	fireEvent.click(view.getByRole("button", { name: "Disable review" }));
	await waitFor(() => expect(view.getByText("disabled")).toBeInTheDocument());
	expect(view.getByText(/binding is loaded.*restart and reopen Resources/)).toBeInTheDocument();
	expect(view.getByRole("button", { name: "Enable review" })).toBeDisabled();
  });

  it("shows Pi unavailable policy reason from the typed snapshot", async () => {
	const resourceAgent: Agent = { ...testAgent, runtimeBinding: { kind: "pi" }, processAlive: false, capabilitySnapshot: capabilitySnapshot("resource_inventory") };
	vi.mocked(fetch).mockImplementation(async (input) => {
	  const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
	  if (url.endsWith("/skills")) return new Response(JSON.stringify({ resources: {
		agentId: resourceAgent.id, agentName: resourceAgent.name, runtimeKind: "pi", revision: "pi-resources-1", semantics: "Pi-native Runtime-specific resources",
		resources: [{ id: "skill:review", name: "review", kind: "skill", path: "/tmp/pi/review/SKILL.md", enabled: true }],
		policy: { available: false, mutable: false, effective: false, reason: "Pi policy unavailable", alternative: "use Pi settings" },
	  } }), { status: 200, headers: { "Content-Type": "application/json" } });
	  return new Response(JSON.stringify(url.includes("/thread/history") ? { total: 0, turns: [] } : { artifacts: [] }), { status: 200, headers: { "Content-Type": "application/json" } });
	});
	const view = render(<AgentPane {...props} agent={resourceAgent} active configRequestNonce={1} />);
	fireEvent.click(view.getByRole("button", { name: "Resources" }));
	expect(await view.findByText(/Pi policy unavailable.*use Pi settings/)).toBeInTheDocument();
	expect(view.getByRole("button", { name: "Disable review" })).toBeDisabled();
  });

  it("keeps Runtime model and Agent config saves independent on failure", async () => {
	const onError = vi.fn();
	vi.mocked(fetch).mockImplementation(async (input, init) => {
	  const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
	  if (url.includes("/runtime/models")) return new Response(JSON.stringify({
		current: { provider: "fixture", id: "vision", thinkingLevels: ["low"], defaultThinkingLevel: "low", imageInput: true },
		models: [
		  { provider: "fixture", id: "vision", thinkingLevels: ["low"], defaultThinkingLevel: "low", imageInput: true },
		  { provider: "fixture", id: "text", thinkingLevels: ["off"], defaultThinkingLevel: "off", imageInput: false },
		], thinkingLevel: "low",
	  }), { status: 200, headers: { "Content-Type": "application/json" } });
	  if (url.includes("/runtime/model") && init?.method === "POST") return new Response(JSON.stringify({ error: "native selection failed" }), { status: 409, headers: { "Content-Type": "application/json" } });
	  const body = url.includes("/thread/history") ? { total: 0, turns: [] } : { artifacts: [] };
	  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
	});
	const agent: Agent = { ...testAgent, providerId: "fixture", model: "vision", effort: "low", capabilitySnapshot: capabilitySnapshot("model_configuration", "image_input") };
	const view = render(<AgentPane {...props} agent={agent} onError={onError} active configRequestNonce={1} />);
	await waitFor(() => expect(view.getByText("Model").closest("label")?.querySelector("select")).toHaveValue("vision"));
	fireEvent.change(view.getByText("Model").closest("label")?.querySelector("select") as HTMLSelectElement, { target: { value: "text" } });
	fireEvent.click(view.getByRole("button", { name: "Save Runtime Model" }));
	await waitFor(() => expect(onError).toHaveBeenCalledWith("native selection failed"));
	expect(view.getByText("Runtime model save failed; Agent config was not changed.")).toBeInTheDocument();
	const calls = vi.mocked(fetch).mock.calls.map(([input, init]) => ({ url: typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url, method: init?.method }));
	expect(calls.some((call) => call.url.endsWith("/config") && call.method === "PATCH")).toBe(false);
  });

  it("gates Runtime controls from the typed Capability Snapshot instead of flat v1 flags", () => {
	const agent: Agent = {
	  ...testAgent,
	  capabilitySnapshot: { revision: "snapshot-data", capabilities: [
		{ id: "sandbox_configuration", availability: "unavailable", revision: "1", scope: { runtimeKind: "codex", bindingRevision: "b", model: "m", configurationRevision: "c" } },
		{ id: "approval_policy", availability: "available", revision: "1", scope: { runtimeKind: "codex", bindingRevision: "b", model: "m", configurationRevision: "c" } },
	  ] },
	};
	const view = render(<AgentPane {...props} agent={agent} active configRequestNonce={1} />);
	expect(view.getByText("Sandbox").closest("label")?.querySelector("select")).toBeDisabled();
	expect(view.getByText("Approval Policy").closest("label")?.querySelector("select")).toBeEnabled();
  });

  it("reconciles the current Goal after an optimistic revision conflict", async () => {
	const onAgentUpdated = vi.fn();
	const onError = vi.fn();
	const currentGoal = {
	  id: "goal-1", version: 1, threadId: testAgent.threadId, objective: "Ship", status: "active" as const,
	  tokenBudget: null, tokensUsed: 0, timeUsedSeconds: 0, createdAt: 1, updatedAt: 1, nativeSyncState: "synced",
	};
	const latestGoal = { ...currentGoal, version: 2, status: "paused" as const, updatedAt: 2 };
	vi.mocked(fetch).mockImplementation(async (input, init) => {
	  const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
	  if (url.endsWith("/goal") && init?.method === "PUT") {
		return new Response(JSON.stringify({ error: "Goal version changed" }), { status: 409, headers: { "Content-Type": "application/json" } });
	  }
	  if (url.endsWith("/goal") && init?.method === "GET") {
		return new Response(JSON.stringify({ goal: latestGoal, revision: 2 }), { status: 200, headers: { "Content-Type": "application/json" } });
	  }
	  const body = url.includes("/thread/history") ? { total: 0, turns: [] } : { artifacts: [] };
	  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
	});
	const view = render(<AgentPane {...props} agent={{ ...testAgent, goal: currentGoal, goalRevision: 1 }} onAgentUpdated={onAgentUpdated} onError={onError} active />);
	fireEvent.click(view.getByRole("button", { name: "Open active" }));
	expect(view.getByRole("button", { name: "Complete" })).toBeInTheDocument();
	fireEvent.click(view.getByRole("button", { name: "Pause" }));
	await waitFor(() => expect(onAgentUpdated).toHaveBeenCalledWith(expect.objectContaining({ goal: latestGoal, goalRevision: 2 })));
	expect(onError).toHaveBeenCalledWith("Goal version changed");
  });

  it("restores a Pi Approval card from the Agent snapshot without Codex wording", async () => {
	virtualizerHarness.instance.getVirtualItems.mockReturnValue([
	  { index: 0, key: "row-0", start: 0, size: 120, end: 120, lane: 0 },
	  { index: 1, key: "row-1", start: 120, size: 120, end: 240, lane: 0 },
	]);
	virtualizerHarness.instance.getTotalSize.mockReturnValue(240);
	vi.mocked(fetch).mockImplementation(async (input) => {
	  const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
	  const body = url.includes("/thread/history")
		? { total: 1, turns: [{ turnId: "turn-current", state: "completed", startedAt: "2026-08-10T00:00:00Z", content: [{ id: "content-current", kind: "user_text", text: "Current work" }] }] }
		: { artifacts: [] };
	  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
	});
	const piAgent: Agent = {
	  ...testAgent,
	  runtimeBinding: { kind: "pi" },
	  pendingApprovals: [{
		approvalId: "ap-agent-scroll-a1", agentId: "agent-scroll", turnId: "turn-1", runtimeKind: "pi",
		method: "tool/bash", params: { command: "pwd" }, status: "pending", requestedAt: "2026-08-10T00:00:00Z",
	  }],
	};
	const view = render(<AgentPane {...props} agent={piAgent} active />);

	const approval = await view.findByText("pi Runtime requests approval");
	const currentWork = await view.findByText("Current work");
	expect(currentWork.compareDocumentPosition(approval) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
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
