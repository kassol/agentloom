import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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

describe("AgentPane", () => {
  beforeEach(() => {
    virtualizerHarness.options.length = 0;
	virtualizerHarness.instance.getVirtualItems.mockReturnValue([]);
	virtualizerHarness.instance.getTotalSize.mockReturnValue(0);
    vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => window.setTimeout(() => callback(performance.now()), 0));
    vi.stubGlobal("cancelAnimationFrame", (id: number) => window.clearTimeout(id));
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
      const body = url.includes("/thread/history")
        ? { total: 0, turns: [] }
        : url.endsWith("/profile")
          ? { profile: { identity: "", domain: "", scope: "", version: 1 } }
          : url.endsWith("/addresses")
            ? { addresses: [] }
            : url === "/api/integrations/connections"
              ? { connections: [] }
              : url.startsWith("/api/integrations/conversations")
                ? { memberships: [] }
                : init?.method === "PATCH" && url.endsWith("/config")
                  ? { agent: testAgent }
                  : { artifacts: [] };
      return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    }));
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: vi.fn().mockReturnValue(false),
    });
  });

  it("discloses the History Boundary and offers divergence recovery", async () => {
    render(<AgentPane
      {...props}
      agent={{
        ...testAgent,
        historyBoundary: {
          kind: "native_conversation_adoption",
          createdAt: "2026-08-13T00:00:00Z",
          importedTurns: 0,
          disclosure: "Existing native content remains outside Loom history.",
        },
        nativeConversationDivergence: {
          code: "native_conversation_divergence",
          detectedAt: "2026-08-13T00:01:00Z",
          summary: "The native conversation changed outside Loom.",
          recovery: "Accept the current native context.",
        },
      }}
      active
    />);

    expect(screen.getByText(/Existing native content remains outside Loom history/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Accept current context" }));
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      "/api/agents/agent-scroll/runtime/conversation/recover",
      expect.objectContaining({ method: "POST" }),
    ));
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    Reflect.deleteProperty(navigator, "clipboard");
    Reflect.deleteProperty(document, "execCommand");
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

	it("shows indeterminate recovery without offering replay", () => {
	  const agent: Agent = {
		...testAgent,
		status: "interrupted",
		lastTurn: { turnId: "turn-uncertain", task: "deploy", status: "interrupted", completedAt: "2026-08-12T00:00:00Z" },
		recovery: { predecessorTurnId: "turn-uncertain", runtimeKind: "claude", state: "dispatched", cause: "command_indeterminate", summary: "Runtime command outcome is indeterminate" },
	  };
	  const view = render(<AgentPane {...props} agent={agent} active />);
	  expect(view.getByText(/Outcome indeterminate/)).toBeInTheDocument();
	  expect(view.queryByRole("button", { name: /Continue/ })).toBeNull();
	});

  it("shows the immutable Runtime kind in the Inspector", () => {
	const view = render(<AgentPane {...props} active configRequestNonce={1} />);
	expect(view.getByText("Runtime kind").nextElementSibling).toHaveTextContent("codex");
	expect(view.queryByDisplayValue("codex")).toBeNull();
  });

	it("starts Runtime-neutral context maintenance and renders its canonical outcome", async () => {
	  const view = render(<AgentPane {...props} active />);
	  const task = view.getByRole("textbox", { name: "task message" });
	  fireEvent.change(task, { target: { value: "/compact" } });
	  fireEvent.click(view.getByRole("button", { name: "send task" }));
	  await waitFor(() => expect(fetch).toHaveBeenCalledWith(
		`/api/agents/${testAgent.id}/compact`,
		expect.objectContaining({ method: "POST" }),
	  ));
	  const operation = {
		id: "cmop-1", agentId: testAgent.id, threadId: testAgent.threadId, origin: "owner",
		state: "started", startedAt: "2026-08-12T00:00:00Z", baselineRevision: "maintenance:base", bindingRevision: "binding:base",
	  };
	  view.rerender(<AgentPane {...props} agent={{ ...testAgent, contextMaintenance: operation }} active />);
	  expect(view.getByText("Maintaining context")).toBeInTheDocument();
	  view.rerender(<AgentPane {...props} agent={{ ...testAgent, contextMaintenance: { ...operation, state: "completed", completedAt: "2026-08-12T00:00:01Z" } }} active />);
	  expect(view.getByText("Context maintenance completed")).toBeInTheDocument();
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

  it("applies descriptor-driven Runtime configuration with the inspected revision", async () => {
	const onAgentUpdated = vi.fn();
	const runtimeAgent: Agent = {
	  ...testAgent,
	  runtimeBinding: { kind: "future-runtime" },
	  capabilitySnapshot: capabilitySnapshot("runtime_configuration"),
	};
	const descriptor = {
	  settingSources: [
		{ id: "user", label: "User", description: "User settings" },
		{ id: "project", label: "Project", description: "Project settings" },
	  ],
	  authentication: [
		{ category: "console", label: "Console", sources: [{ id: "api_key", label: "API key" }] },
		{ category: "gateway", label: "Gateway", description: "Managed gateway credentials", sources: [{ id: "gateway", label: "Managed gateway" }] },
	  ],
	  default: { configured: true, settingSources: ["project"], authentication: { category: "console", source: "api_key" } },
	};
	const viewFor = (revision: string, settingSources: string[], category: string, source: string) => ({
	  configuration: { configured: true, settingSources, authentication: { category, source } },
	  descriptor,
	  evidence: { settingSources, authentication: { category, source, validation: "accepted" } },
	  revision,
	});
	vi.mocked(fetch).mockImplementation(async (input, init) => {
	  const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
	  if (url.endsWith("/runtime/configuration")) {
		if (init?.method === "PATCH") {
		  expect(JSON.parse(String(init.body))).toEqual({
			expectedRevision: "configuration-1",
			configuration: { configured: true, settingSources: ["project", "user"], authentication: { category: "gateway", source: "gateway" } },
		  });
		  return new Response(JSON.stringify({ configuration: viewFor("configuration-2", ["user", "project"], "gateway", "gateway") }), { status: 200, headers: { "Content-Type": "application/json" } });
		}
		return new Response(JSON.stringify({ configuration: viewFor("configuration-1", ["project"], "console", "api_key") }), { status: 200, headers: { "Content-Type": "application/json" } });
	  }
	  if (url === `/api/agents/${runtimeAgent.id}`) {
		return new Response(JSON.stringify({ agent: runtimeAgent }), { status: 200, headers: { "Content-Type": "application/json" } });
	  }
	  const body = url.includes("/thread/history")
		? { total: 0, turns: [] }
		: url.endsWith("/profile")
		  ? { profile: { identity: "", domain: "", scope: "", version: 1 } }
		  : { artifacts: [] };
	  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
	});

	const view = render(<AgentPane {...props} agent={runtimeAgent} onAgentUpdated={onAgentUpdated} active configRequestNonce={1} />);
	expect(await view.findByText("Owner configuration")).toBeInTheDocument();
	fireEvent.click(view.getByRole("checkbox", { name: /User/ }));
	fireEvent.change(view.getByLabelText("Authentication category"), { target: { value: "gateway" } });
	expect(view.getByLabelText("Authentication source")).toHaveValue("gateway");
	fireEvent.click(view.getByRole("button", { name: "Apply Runtime Configuration" }));

	expect(await view.findByText("Verified gateway / gateway · accepted")).toBeInTheDocument();
	expect(view.getByText("Managed gateway credentials")).toBeInTheDocument();
	await waitFor(() => expect(onAgentUpdated).toHaveBeenCalledWith(runtimeAgent));
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

	it("shows pathless Claude resources and configuration evidence without native paths", async () => {
	const resourceAgent: Agent = { ...testAgent, runtimeBinding: { kind: "claude" }, processAlive: true, capabilitySnapshot: capabilitySnapshot("resource_inventory") };
	vi.mocked(fetch).mockImplementation(async (input) => {
	  const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
	  if (url.endsWith("/skills")) return new Response(JSON.stringify({ resources: {
		agentId: resourceAgent.id, agentName: resourceAgent.name, runtimeKind: "claude", revision: "claude-resources-1", semantics: "Claude native resources",
		resources: [
		  { id: "command:review", name: "review", kind: "command", enabled: true, status: "ready", source: "project settings" },
		  { id: "mcp:github", name: "github", kind: "mcp", enabled: true, status: "connected", path: "/Users/owner/.claude/native.json", description: "Loaded from /Users/owner/.claude/native.json" },
		  { id: "extension:tools", name: "tools", kind: "extension", enabled: false, status: "disabled" },
		],
		configuration: { settingSources: ["user", "project"], authentication: { category: "console", source: "api_key", validation: "accepted", evidence: [{ kind: "native_ack", summary: "API key accepted" }] } },
		policy: { available: false, mutable: false, effective: false, reason: "Claude policy is native", alternative: "use Claude settings" },
	  } }), { status: 200, headers: { "Content-Type": "application/json" } });
	  return new Response(JSON.stringify(url.includes("/thread/history") ? { total: 0, turns: [] } : { artifacts: [] }), { status: 200, headers: { "Content-Type": "application/json" } });
	});
	const view = render(<AgentPane {...props} agent={resourceAgent} active configRequestNonce={1} />);
	fireEvent.click(view.getByRole("button", { name: "Resources" }));
	expect(await view.findByText("user · project")).toBeInTheDocument();
	expect(view.getByText("console / api_key · accepted")).toBeInTheDocument();
	expect(view.getByText(/API key accepted/)).toBeInTheDocument();
	expect(view.getByText("command")).toBeInTheDocument();
	expect(view.getByText("mcp")).toBeInTheDocument();
	expect(view.getByText("extension")).toBeInTheDocument();
	expect(view.container).not.toHaveTextContent("project settings");
	expect(view.container).not.toHaveTextContent("/Users/owner/.claude/native.json");
	expect(view.queryByRole("button", { name: "Disable review" })).not.toBeInTheDocument();
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

  it("allows a Unicode Agent name when saving config", async () => {
	const onAgentUpdated = vi.fn();
	vi.mocked(fetch).mockImplementation(async (input, init) => {
	  const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
	  if (url.endsWith("/config") && init?.method === "PATCH") {
		const body = JSON.parse(String(init.body));
		return new Response(JSON.stringify({ agent: { ...testAgent, name: body.name } }), { status: 200, headers: { "Content-Type": "application/json" } });
	  }
	  return new Response(JSON.stringify(url.includes("/thread/history") ? { total: 0, turns: [] } : { artifacts: [] }), { status: 200, headers: { "Content-Type": "application/json" } });
	});
	const view = render(<AgentPane {...props} onAgentUpdated={onAgentUpdated} active configRequestNonce={1} />);
	const nameInput = await waitFor(() => view.getByText("Name").closest("label")?.querySelector("input") as HTMLInputElement);
	fireEvent.change(nameInput, { target: { value: "产品负责人" } });
	fireEvent.click(view.getByRole("button", { name: "Save Agent Config" }));
	await waitFor(() => expect(onAgentUpdated).toHaveBeenCalledWith(expect.objectContaining({ name: "产品负责人" })));
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

  it("shows the configured working directory as read-only selectable text between Name and Provider", async () => {
    render(<AgentPane {...props} configRequestNonce={1} active />);

    const path = await screen.findByTestId("agent-working-directory");
    const name = screen.getByPlaceholderText("agent-name");
    const provider = screen.getByLabelText("Provider");

    expect(path).toHaveTextContent(testAgent.cwd);
    expect(path.tagName).toBe("CODE");
    expect(path).toHaveClass("select-text", "break-all", "whitespace-pre-wrap");
    expect(path.closest("input, textarea, [contenteditable='true']")).toBeNull();
    expect(name.compareDocumentPosition(path) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(path.compareDocumentPosition(provider) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("copies the exact working directory and reports success", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });
    render(<AgentPane {...props} configRequestNonce={1} active />);

    fireEvent.click(await screen.findByRole("button", { name: "Copy working directory" }));

    await waitFor(() => expect(screen.getByRole("button", { name: "Copied working directory" })).toBeInTheDocument());
    expect(writeText).toHaveBeenCalledWith(testAgent.cwd);
  });

  it("reports copy failure instead of claiming success", async () => {
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: vi.fn().mockRejectedValue(new DOMException("denied", "NotAllowedError")) },
    });
    vi.mocked(document.execCommand).mockReturnValue(false);
    render(<AgentPane {...props} configRequestNonce={1} active />);

    fireEvent.click(await screen.findByRole("button", { name: "Copy working directory" }));

    await waitFor(() => expect(screen.getByRole("button", { name: "Copy working directory failed" })).toBeInTheDocument());
    expect(screen.queryByRole("button", { name: "Copied working directory" })).not.toBeInTheDocument();
  });

  it("updates the path and resets copy feedback when the Agent changes", async () => {
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: vi.fn().mockResolvedValue(undefined) },
    });
    const view = render(<AgentPane {...props} configRequestNonce={1} active />);
    fireEvent.click(await screen.findByRole("button", { name: "Copy working directory" }));
    await screen.findByRole("button", { name: "Copied working directory" });

    const nextAgent = { ...testAgent, id: "agent-next", name: "agent-next", cwd: "/projects/next agent/with/a/very/long/project/path" };
    view.rerender(<AgentPane {...props} agent={nextAgent} configRequestNonce={1} active />);

    expect(await screen.findByTestId("agent-working-directory")).toHaveTextContent(nextAgent.cwd);
    expect(screen.queryByText(testAgent.cwd)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy working directory" })).toBeInTheDocument();
  });

  it("keeps cwd out of the config save request", async () => {
    const fetchMock = vi.mocked(fetch);
    render(<AgentPane {...props} configRequestNonce={1} active />);

    fireEvent.click(await screen.findByRole("button", { name: "Save Agent Config" }));

    await waitFor(() => {
      expect(fetchMock.mock.calls.some(([input, init]) => {
        const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
        return url.endsWith("/config") && init?.method === "PATCH";
      })).toBe(true);
    });
    const [, init] = fetchMock.mock.calls.find(([input, request]) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
      return url.endsWith("/config") && request?.method === "PATCH";
    })!;
    const body = JSON.parse(String(init?.body));
    expect(body).toMatchObject({
      name: testAgent.name,
      sandbox: testAgent.sandbox,
      approvalPolicy: testAgent.approvalPolicy,
    });
    expect(body).not.toHaveProperty("cwd");
  });
});
