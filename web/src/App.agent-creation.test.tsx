import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import App from "./App";
import { resetGlobalEventsForTests } from "./global-events";

function jsonResponse(body: unknown, status = 200) {
	return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

function renderApp() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(<QueryClientProvider client={client}><App /></QueryClientProvider>);
}

const claudeConfiguration = {
	settingSources: [
		{ id: "user", label: "User", description: "User settings" },
		{ id: "project", label: "Project", description: "Project settings" },
		{ id: "local", label: "Local", description: "Local settings" },
	],
	authentication: [
		{ category: "subscription", label: "Claude subscription", description: "Uses this Owner's existing local Claude.ai login for development. CodexLoom does not copy or store the OAuth credential.", sources: [{ id: "claude_ai", label: "Local Claude.ai login (OAuth)" }] },
		{ category: "console", label: "Claude Console", sources: [{ id: "api_key", label: "API key" }] },
		{ category: "gateway", label: "Gateway", sources: [{ id: "gateway", label: "Managed gateway" }] },
	],
	default: { configured: true, settingSources: ["user", "project", "local"], authentication: { category: "console", source: "api_key" } },
};

describe("Agent creation dialog", () => {
	beforeEach(() => {
		const storage = () => {
			const values = new Map<string, string>();
			return {
				getItem: (key: string) => values.get(key) ?? null,
				setItem: (key: string, value: string) => values.set(key, String(value)),
				removeItem: (key: string) => values.delete(key), clear: () => values.clear(),
				key: (index: number) => [...values.keys()][index] ?? null,
				get length() { return values.size; },
			};
		};
		vi.stubGlobal("localStorage", storage());
		vi.stubGlobal("sessionStorage", storage());
		vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
			const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
			if (init?.method === "POST" && url === "/api/agents") {
				const request = JSON.parse(String(init.body));
				return jsonResponse({ agent: {
					id: "agent-unicode", name: request.name, cwd: request.cwd, threadId: "thread-unicode",
					runtimeBinding: { kind: "codex" }, capabilitySnapshot: { revision: "", capabilities: [] },
					sandbox: "danger-full-access", approvalPolicy: "never", status: "idle", currentTask: "", currentTurnId: "", lastError: "",
					createdAt: "2026-08-13T00:00:00Z", updatedAt: "2026-08-13T00:00:00Z", processAlive: false, pendingApprovals: [], lastSeq: 0,
				} });
			}
			if (url.startsWith("/api/agents")) return jsonResponse({ agents: [] });
			if (url === "/api/remote") return jsonResponse({ remote: null });
			if (url === "/api/model-providers") return jsonResponse({ providers: [{ id: "openai", name: "OpenAI", configured: true, credentialConfigured: true, models: [], modelDetails: [] }] });
			if (url === "/api/runtimes") return jsonResponse({ runtimes: [{ runtimeKind: "codex", revision: "codex-1", capabilities: [] }] });
			if (url.startsWith("/api/inbox")) return jsonResponse({ entries: [] });
			if (url.startsWith("/api/human-requests")) return jsonResponse({ requests: [] });
			if (url.startsWith("/api/topics")) return jsonResponse({ topics: [] });
			if (url === "/api/admin/backups") return jsonResponse({ backups: [] });
			return jsonResponse({});
		}));
	});

	afterEach(() => {
		cleanup();
		resetGlobalEventsForTests();
		vi.unstubAllGlobals();
	});

	it("submits a Chinese Agent name", async () => {
		const view = renderApp();
		fireEvent.click(await view.findByRole("button", { name: "New agent" }));
		const dialog = await view.findByRole("dialog");
		fireEvent.change(within(dialog).getByLabelText("Agent name"), { target: { value: "研发助手" } });
		fireEvent.change(within(dialog).getByLabelText("Working directory"), { target: { value: "/workspace/project" } });
		fireEvent.click(within(dialog).getByRole("button", { name: "Create agent" }));

		await waitFor(() => expect(vi.mocked(fetch).mock.calls.some(([input, init]) => {
			const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
			return url === "/api/agents" && init?.method === "POST" && JSON.parse(String(init.body)).name === "研发助手";
		})).toBe(true));
	});

	it("keeps a name validation error in the creation dialog", async () => {
		const view = renderApp();
		fireEvent.click(await view.findByRole("button", { name: "New agent" }));
		const dialog = await view.findByRole("dialog");
		fireEvent.change(within(dialog).getByLabelText("Agent name"), { target: { value: "研发/助手" } });
		fireEvent.change(within(dialog).getByLabelText("Working directory"), { target: { value: "/workspace/project" } });
		fireEvent.click(within(dialog).getByRole("button", { name: "Create agent" }));

		expect(within(dialog).getByRole("alert")).toHaveTextContent("letters or numbers from any language");
		expect(vi.mocked(fetch).mock.calls.some(([input, init]) => input === "/api/agents" && init?.method === "POST")).toBe(false);
	});

	it("initializes explicit Runtime configuration when a delayed catalog arrives", async () => {
		let resolveRuntimes!: (response: Response) => void;
		const runtimes = new Promise<Response>((resolve) => { resolveRuntimes = resolve; });
		vi.mocked(fetch).mockImplementation(async (input, init) => {
			const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
			if (url === "/api/runtimes") return runtimes;
			if (init?.method === "POST" && url === "/api/agents") {
				const request = JSON.parse(String(init.body));
				return jsonResponse({ agent: {
					id: "agent-claude", name: request.name, cwd: request.cwd, threadId: "thread-claude",
					runtimeBinding: { kind: "claude" }, capabilitySnapshot: { revision: "", capabilities: [] },
					runtimeConfiguration: request.runtimeConfiguration,
					sandbox: "danger-full-access", approvalPolicy: "never", status: "idle", currentTask: "", currentTurnId: "", lastError: "",
					createdAt: "2026-08-13T00:00:00Z", updatedAt: "2026-08-13T00:00:00Z", processAlive: false, pendingApprovals: [], lastSeq: 0,
				} });
			}
			if (url.startsWith("/api/agents")) return jsonResponse({ agents: [] });
			if (url === "/api/remote") return jsonResponse({ remote: null });
			if (url === "/api/model-providers") return jsonResponse({ providers: [{ id: "openai", name: "OpenAI", configured: true, credentialConfigured: true, models: [], modelDetails: [] }] });
			if (url.startsWith("/api/inbox")) return jsonResponse({ entries: [] });
			if (url.startsWith("/api/human-requests")) return jsonResponse({ requests: [] });
			if (url.startsWith("/api/topics")) return jsonResponse({ topics: [] });
			if (url === "/api/admin/backups") return jsonResponse({ backups: [] });
			return jsonResponse({});
		});

		const view = renderApp();
		fireEvent.click(await view.findByRole("button", { name: "New agent" }));
		const dialog = await view.findByRole("dialog");
		fireEvent.change(within(dialog).getByLabelText("Runtime"), { target: { value: "claude" } });
		expect(within(dialog).getByRole("button", { name: "Create agent" })).toBeDisabled();

		resolveRuntimes(jsonResponse({ runtimes: [
			{ runtimeKind: "codex", revision: "codex-1", capabilities: [] },
			{ runtimeKind: "claude", revision: "claude-configuration-1", capabilities: [], configuration: claudeConfiguration },
		] }));

		expect(await within(dialog).findByText("Runtime settings")).toBeInTheDocument();
		expect(within(dialog).getByRole("checkbox", { name: /User/ })).toBeChecked();
		expect(within(dialog).getByRole("checkbox", { name: /Project/ })).toBeChecked();
		expect(within(dialog).getByRole("checkbox", { name: /Local/ })).toBeChecked();
		fireEvent.change(within(dialog).getByLabelText("Authentication category"), { target: { value: "subscription" } });
		expect(within(dialog).getByLabelText("Authentication source")).toHaveValue("claude_ai");
		expect(within(dialog).getByRole("option", { name: "Local Claude.ai login (OAuth)" })).toBeInTheDocument();
		expect(within(dialog).getByText(/does not copy or store the OAuth credential/)).toBeInTheDocument();
		fireEvent.change(within(dialog).getByLabelText("Agent name"), { target: { value: "claude-owner" } });
		fireEvent.change(within(dialog).getByLabelText("Working directory"), { target: { value: "/workspace/claude" } });
		fireEvent.click(within(dialog).getByRole("button", { name: "Create agent" }));

		await waitFor(() => expect(vi.mocked(fetch).mock.calls.some(([input, init]) => {
			const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
			if (url !== "/api/agents" || init?.method !== "POST") return false;
			const body = JSON.parse(String(init.body));
			return body.runtimeKind === "claude" &&
				body.runtimeConfiguration?.settingSources?.join(",") === "user,project,local" &&
				body.runtimeConfiguration?.authentication?.category === "subscription" &&
				body.runtimeConfiguration?.authentication?.source === "claude_ai";
		})).toBe(true));
	});

	it("adopts a discovered Codex Thread into an explicit stable workspace", async () => {
		const candidate = { id: "candidate-1", revision: "revision-1", runtimeKind: "codex", name: "Existing release", cwd: "/source/workspace", updatedAt: "2026-08-13T00:00:00Z", compatible: true };
		vi.mocked(fetch).mockImplementation(async (input, init) => {
			const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
			if (init?.method === "POST" && url === "/api/runtimes/codex/conversations/adopt") {
				const request = JSON.parse(String(init.body));
				return jsonResponse({ agent: {
					id: "agent-adopted", name: request.name, cwd: request.cwd, sourceCwd: candidate.cwd, threadId: "loom-thread",
					runtimeBinding: { kind: "codex" }, capabilitySnapshot: { revision: "", capabilities: [] },
					sandbox: "danger-full-access", approvalPolicy: "never", status: "idle", currentTask: "", currentTurnId: "", lastError: "",
					createdAt: candidate.updatedAt, updatedAt: candidate.updatedAt, processAlive: true, pendingApprovals: [], lastSeq: 0,
				} });
			}
			if (url === "/api/runtimes/codex/conversations") return jsonResponse({ candidates: [candidate] });
			if (url === `/api/runtimes/codex/conversations/${candidate.id}`) return jsonResponse({ candidate });
			if (url.startsWith("/api/agents")) return jsonResponse({ agents: [] });
			if (url === "/api/remote") return jsonResponse({ remote: null });
			if (url === "/api/model-providers") return jsonResponse({ providers: [{ id: "openai", name: "OpenAI", configured: true, credentialConfigured: true, models: [], modelDetails: [] }] });
			if (url === "/api/runtimes") return jsonResponse({ runtimes: [{ runtimeKind: "codex", revision: "catalog-1", capabilities: [{ id: "conversation_adoption", available: true }] }] });
			if (url.startsWith("/api/inbox")) return jsonResponse({ entries: [] });
			if (url.startsWith("/api/human-requests")) return jsonResponse({ requests: [] });
			if (url.startsWith("/api/topics")) return jsonResponse({ topics: [] });
			if (url === "/api/admin/backups") return jsonResponse({ backups: [] });
			return jsonResponse({});
		});

		const view = renderApp();
		fireEvent.click(await view.findByRole("button", { name: "New agent" }));
		const dialog = await view.findByRole("dialog");
		fireEvent.click(await within(dialog).findByRole("button", { name: "Adopt existing" }));
		const select = await within(dialog).findByLabelText("Native conversation");
		await within(dialog).findByRole("option", { name: /Existing release/ });
		fireEvent.change(select, { target: { value: candidate.id } });
		await within(dialog).findByText("Source conversation workspace");
		await within(dialog).findByText(/History Boundary: existing native content/);
		fireEvent.change(within(dialog).getByLabelText("Agent name"), { target: { value: "release-owner" } });
		fireEvent.change(within(dialog).getByPlaceholderText("/absolute/path/used-for-future-turns"), { target: { value: "/stable/workspace" } });
		fireEvent.click(within(dialog).getByRole("button", { name: "Adopt conversation" }));

		await waitFor(() => expect(vi.mocked(fetch).mock.calls.some(([input, init]) => {
			const url = typeof input === "string" ? input : input instanceof URL ? input.pathname : input.url;
			if (url !== "/api/runtimes/codex/conversations/adopt" || init?.method !== "POST") return false;
			const body = JSON.parse(String(init.body));
			return body.name === "release-owner" && body.cwd === "/stable/workspace" && body.candidateId === candidate.id && body.expectedRevision === candidate.revision;
		})).toBe(true));
	});
});
