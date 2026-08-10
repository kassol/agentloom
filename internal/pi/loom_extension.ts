import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "@earendil-works/pi-ai";

type JsonRecord = Record<string, unknown>;

function environment(): { apiURL: string; agentID: string } {
	const apiURL = process.env.CODEX_LOOM_API_URL?.replace(/\/$/, "");
	const agentID = process.env.CODEX_LOOM_AGENT_ID;
	if (!apiURL || !agentID) throw new Error("Loom collaboration is unavailable in this Pi Runtime");
	return { apiURL, agentID };
}

async function request(path: string, signal: AbortSignal, init?: RequestInit): Promise<JsonRecord> {
	const { apiURL } = environment();
	const response = await fetch(`${apiURL}${path}`, {
		...init,
		signal,
		headers: { "content-type": "application/json", ...(init?.headers ?? {}) },
	});
	const value = (await response.json()) as JsonRecord;
	if (!response.ok) {
		const message = typeof value.error === "string" ? value.error : `${response.status} ${response.statusText}`;
		throw new Error(`CodexLoom request failed: ${message}`);
	}
	return value;
}

function result(value: unknown) {
	const text = JSON.stringify(value, null, 2);
	return { content: [{ type: "text" as const, text }], details: value };
}

export default function loomCollaboration(pi: ExtensionAPI) {
	pi.registerTool({
		name: "loom_agents_find",
		label: "Find Loom Agents",
		description: "Discover governed Loom Agents, their Runtime, Profile Domain and collaboration relationships.",
		promptGuidelines: ["Use loom_agents_find to identify the Agent whose Profile Domain owns required work before sending a Message."],
		parameters: Type.Object({ query: Type.Optional(Type.String({ description: "Optional name, Runtime, Domain, or Scope filter" })) }),
		async execute(_id, params, signal) {
			const [teamResponse, agentResponse] = await Promise.all([
				request("/api/team", signal),
				request("/api/agents", signal),
			]);
			const team = (teamResponse.team ?? {}) as JsonRecord;
			const runtimeByID = new Map(
				((agentResponse.agents ?? []) as JsonRecord[]).map((agent) => [agent.id, (agent.runtimeBinding as JsonRecord | undefined)?.kind]),
			);
			const query = params.query?.trim().toLowerCase();
			const agents = ((team.agents ?? []) as JsonRecord[])
				.map((agent) => ({ ...agent, runtimeKind: runtimeByID.get(agent.id) }))
				.filter((agent) => !query || JSON.stringify(agent).toLowerCase().includes(query));
			return result({
				agents,
				organizationLinks: team.organizationLinks ?? [],
				collaborationLinks: team.collaborationLinks ?? [],
			});
		},
	});

	pi.registerTool({
		name: "loom_message_send",
		label: "Send Loom Message",
		description: "Send a durable request or notification to another governed Loom Agent.",
		promptGuidelines: ["Use loom_message_send for Agent-to-Agent work; set response to required only when the current work needs a reply."],
		parameters: Type.Object({
			to: Type.String({ description: "Target Agent ID or name" }),
			subject: Type.String(),
			body: Type.String(),
			response: Type.Optional(Type.Union([Type.Literal("required"), Type.Literal("none")])),
			topicId: Type.Optional(Type.String()),
		}),
		async execute(_id, params, signal) {
			const { agentID } = environment();
			return result(await request("/api/comms/messages", signal, {
				method: "POST",
				body: JSON.stringify({
					from: agentID, to: params.to, subject: params.subject, body: params.body,
					response: params.response ?? "none", topicId: params.topicId,
				}),
			}));
		},
	});

	pi.registerTool({
		name: "loom_message_receive",
		label: "Receive Loom Messages",
		description: "Inspect durable Messages sent to or from this Agent, including delivery, handling, reply, source Turn, and Topic state.",
		parameters: Type.Object({
			messageId: Type.Optional(Type.String()),
			status: Type.Optional(Type.Union([Type.Literal("open"), Type.Literal("answered"), Type.Literal("closed")])),
			direction: Type.Optional(Type.Union([Type.Literal("in"), Type.Literal("out"), Type.Literal("both")])),
		}),
		async execute(_id, params, signal) {
			const { agentID } = environment();
			if (params.messageId) {
				const response = await request(`/api/comms/messages/${encodeURIComponent(params.messageId)}`, signal);
				const message = (response.message ?? {}) as JsonRecord;
				if (message.fromAgentId !== agentID && message.toAgentId !== agentID) {
					throw new Error("Loom Message does not belong to this Agent");
				}
				return result(response);
			}
			const query = new URLSearchParams({ agent: agentID });
			if (params.status) query.set("status", params.status);
			const response = await request(`/api/comms?${query}`, signal);
			const direction = params.direction ?? "in";
			const messages = ((response.messages ?? []) as JsonRecord[])
				.filter((message) => direction === "both" || (direction === "in" ? message.toAgentId === agentID : message.fromAgentId === agentID))
				.slice(0, 20);
			return result({ messages });
		},
	});

	pi.registerTool({
		name: "loom_message_reply",
		label: "Reply to Loom Message",
		description: "Reply to a response-required Loom Message while preserving its root Message, source Turn, Topic, delivery, and handling records.",
		promptGuidelines: ["Use loom_message_reply when handling a required Agent Message; return the result to the requesting Agent, not as an Owner action item."],
		parameters: Type.Object({
			messageId: Type.String({ description: "Root Message ID being answered" }),
			body: Type.String(),
			subject: Type.Optional(Type.String()),
		}),
		async execute(_id, params, signal) {
			const { agentID } = environment();
			return result(await request("/api/comms/messages", signal, {
				method: "POST",
				body: JSON.stringify({ from: agentID, replyTo: params.messageId, subject: params.subject, body: params.body }),
			}));
		},
	});
}
