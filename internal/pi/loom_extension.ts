import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "@earendil-works/pi-ai";

type JsonRecord = Record<string, unknown>;

const LOOM_APPROVAL_TITLE = "codex-loom:approval:v1";
const LOOM_APPROVAL_TIMEOUT_MS = 5 * 60 * 1000;
const READ_ONLY_TOOLS = new Set(["read", "grep", "find", "ls"]);
const LOOM_TOOLS = new Set([
	"loom_agents_find",
	"loom_message_send",
	"loom_message_receive",
	"loom_message_reply",
	"loom_needs_you",
	"loom_topic_list",
	"loom_topic_get",
	"loom_topic_create",
	"loom_topic_participant_upsert",
	"loom_topic_participant_remove",
	"loom_topic_wait",
	"loom_topic_resume",
	"loom_topic_publish_progress",
	"loom_topic_publish_result",
]);

function approvalPolicy(): "never" | "on-request" {
	const value = process.env.CODEX_LOOM_APPROVAL_POLICY?.trim().toLowerCase();
	if (!value || value === "never") return "never";
	return "on-request";
}

function approvalBlockReason(decision: string | undefined, aborted: boolean): string {
	if (aborted || decision === "abort") return "Loom Approval aborted before the tool executed";
	if (decision === "timeout") return "Loom Approval timed out before the tool executed";
	if (decision === "deny") return "Loom Approval denied before the tool executed";
	if (decision === undefined) return "Loom Approval timed out or was cancelled before the tool executed";
	return "Loom Approval returned an unsupported decision before the tool executed";
}

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

function topicBelongsToAgent(topic: JsonRecord, agentID: string): boolean {
	if (topic.responsibleAgentId === agentID) return true;
	return ((topic.participants ?? []) as JsonRecord[]).some((participant) => participant.agentId === agentID);
}

export default function loomCollaboration(pi: ExtensionAPI) {
	pi.on("tool_call", async (event, ctx) => {
		if (approvalPolicy() === "never" || READ_ONLY_TOOLS.has(event.toolName) || LOOM_TOOLS.has(event.toolName)) return;
		const decision = await ctx.ui.input(LOOM_APPROVAL_TITLE, JSON.stringify({
			version: 1,
			operation: "request_approval",
			toolCallId: event.toolCallId,
			toolName: event.toolName,
			input: event.input,
		}), { signal: ctx.signal, timeout: LOOM_APPROVAL_TIMEOUT_MS });
		if (decision === "approve") return;
		return { block: true, reason: approvalBlockReason(decision, ctx.signal?.aborted === true) };
	});

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

	pi.registerTool({
		name: "loom_needs_you",
		label: "Ask Loom Owner",
		description: "Create a durable Needs You request for an Owner fact, decision, or authorization that blocks the current work.",
		promptGuidelines: [
			"Use loom_needs_you only when current work requires Owner input; calling it ends the current Turn and Loom resumes the same Agent Thread after the Owner answers.",
		],
		parameters: Type.Object({
			question: Type.String({ description: "The specific question the Owner must answer" }),
			context: Type.Optional(Type.String({ description: "Concise context the Owner needs to decide" })),
			blockedWork: Type.Optional(Type.String({ description: "The work that cannot continue without this answer" })),
			expectation: Type.Optional(Type.Union([Type.Literal("required"), Type.Literal("optional")])),
			options: Type.Optional(Type.Array(Type.Object({
				label: Type.String(),
				description: Type.Optional(Type.String()),
			}))),
			topicId: Type.Optional(Type.String({ description: "Related Loom Topic ID, when this work belongs to a Topic" })),
		}),
		async execute(_id, params, signal) {
			const { agentID } = environment();
			const value = await request("/api/human-requests", signal, {
				method: "POST",
				body: JSON.stringify({
					agent: agentID,
					question: params.question,
					context: params.context,
					blockedWork: params.blockedWork,
					expectation: params.expectation ?? "required",
					options: params.options ?? [],
					topicId: params.topicId,
				}),
			});
			return { ...result(value), terminate: true };
		},
	});

	pi.registerTool({
		name: "loom_topic_list",
		label: "List Loom Topics",
		description: "List durable Topics in which this Agent is Responsible or a bounded Participant.",
		parameters: Type.Object({
			status: Type.Optional(Type.Union([
				Type.Literal("active"), Type.Literal("waiting"), Type.Literal("resolved"), Type.Literal("archived"),
			])),
		}),
		async execute(_id, params, signal) {
			const { agentID } = environment();
			const query = new URLSearchParams({ agent: agentID, view: "summary" });
			if (params.status) query.set("status", params.status);
			return result(await request(`/api/topics?${query}`, signal));
		},
	});

	pi.registerTool({
		name: "loom_topic_get",
		label: "Get Loom Topic",
		description: "Read the authoritative brief, responsibilities, waiting state, active work, and audit history for one Topic.",
		parameters: Type.Object({ topicId: Type.String() }),
		async execute(_id, params, signal) {
			const { agentID } = environment();
			const response = await request(`/api/topics/${encodeURIComponent(params.topicId)}`, signal);
			const topic = (response.topic ?? {}) as JsonRecord;
			if (!topicBelongsToAgent(topic, agentID)) throw new Error("Loom Topic does not belong to this Agent");
			return result(response);
		},
	});

	pi.registerTool({
		name: "loom_topic_create",
		label: "Create Loom Topic",
		description: "Create a durable Topic owned by this Agent, with a completion boundary and bounded Participants.",
		promptGuidelines: ["Create a Topic only for bounded shared coordination; remain accountable for integrating all participant work."],
		parameters: Type.Object({
			title: Type.String(),
			purpose: Type.String(),
			completionBoundary: Type.String(),
			initialSummary: Type.String(),
			currentState: Type.Optional(Type.String()),
			nextStep: Type.Optional(Type.String()),
			limitations: Type.Optional(Type.String()),
			participants: Type.Optional(Type.Array(Type.Object({
				agent: Type.String({ description: "Participant Agent ID or name" }),
				responsibility: Type.String({ description: "Bounded Topic responsibility" }),
			}))),
		}),
		async execute(_id, params, signal) {
			const { agentID } = environment();
			return result(await request("/api/topics", signal, {
				method: "POST",
				body: JSON.stringify({
					title: params.title, purpose: params.purpose, completionBoundary: params.completionBoundary,
					responsibleAgent: agentID, createdBy: agentID, participants: params.participants ?? [],
					initialBrief: {
						summary: params.initialSummary, currentState: params.currentState,
						nextStep: params.nextStep, limitations: params.limitations,
					},
				}),
			}));
		},
	});

	pi.registerTool({
		name: "loom_topic_participant_upsert",
		label: "Set Loom Topic Participant",
		description: "Add a Participant or replace that Participant's bounded responsibility. Only the Responsible Agent may do this.",
		parameters: Type.Object({ topicId: Type.String(), agent: Type.String(), responsibility: Type.String() }),
		async execute(_id, params, signal) {
			const { agentID } = environment();
			return result(await request(`/api/topics/${encodeURIComponent(params.topicId)}/participants`, signal, {
				method: "POST",
				body: JSON.stringify({ actor: agentID, agent: params.agent, responsibility: params.responsibility }),
			}));
		},
	});

	pi.registerTool({
		name: "loom_topic_participant_remove",
		label: "Remove Loom Topic Participant",
		description: "Remove a bounded Participant from a Topic. Only the Responsible Agent may do this.",
		parameters: Type.Object({ topicId: Type.String(), agent: Type.String() }),
		async execute(_id, params, signal) {
			const { agentID } = environment();
			const query = new URLSearchParams({ actor: agentID });
			return result(await request(`/api/topics/${encodeURIComponent(params.topicId)}/participants/${encodeURIComponent(params.agent)}?${query}`, signal, { method: "DELETE" }));
		},
	});

	pi.registerTool({
		name: "loom_topic_wait",
		label: "Wait Loom Topic",
		description: "Record the external condition a Topic is waiting on and the action required to resume it.",
		parameters: Type.Object({
			topicId: Type.String(), expectedVersion: Type.Optional(Type.Number()),
			kind: Type.String(), refId: Type.Optional(Type.String()), summary: Type.String(), resumeAction: Type.Optional(Type.String()),
		}),
		async execute(_id, params, signal) {
			const { agentID } = environment();
			return result(await request(`/api/topics/${encodeURIComponent(params.topicId)}`, signal, {
				method: "PATCH",
				body: JSON.stringify({
					actor: agentID, expectedVersion: params.expectedVersion,
					waitingOn: { kind: params.kind, refId: params.refId, summary: params.summary, resumeAction: params.resumeAction },
				}),
			}));
		},
	});

	pi.registerTool({
		name: "loom_topic_resume",
		label: "Resume Loom Topic",
		description: "Clear a Topic waiting condition and return it to the existing active coordination state.",
		parameters: Type.Object({ topicId: Type.String(), expectedVersion: Type.Optional(Type.Number()) }),
		async execute(_id, params, signal) {
			const { agentID } = environment();
			return result(await request(`/api/topics/${encodeURIComponent(params.topicId)}`, signal, {
				method: "PATCH",
				body: JSON.stringify({ actor: agentID, expectedVersion: params.expectedVersion, clearWaiting: true }),
			}));
		},
	});

	pi.registerTool({
		name: "loom_topic_publish_progress",
		label: "Publish Loom Topic Progress",
		description: "Publish an integrated progress Brief for a Topic. Only the Responsible Agent may update the shared Brief.",
		promptGuidelines: ["Integrate Participant replies into one shared progress update; keep individual replies as intermediate evidence."],
		parameters: Type.Object({
			topicId: Type.String(),
			expectedVersion: Type.Number({ description: "Current Topic version from loom_topic_get" }),
			summary: Type.String(),
			currentState: Type.Optional(Type.String()),
			nextStep: Type.Optional(Type.String()),
			limitations: Type.Optional(Type.String()),
			evidence: Type.Optional(Type.Array(Type.Object({
				type: Type.String(), id: Type.String(), label: Type.Optional(Type.String()),
			}))),
		}),
		async execute(_id, params, signal) {
			const { agentID } = environment();
			return result(await request(`/api/topics/${encodeURIComponent(params.topicId)}`, signal, {
				method: "PATCH",
				body: JSON.stringify({
					actor: agentID, expectedVersion: params.expectedVersion,
					brief: {
						summary: params.summary, currentState: params.currentState, nextStep: params.nextStep,
						limitations: params.limitations, evidence: params.evidence ?? [],
					},
				}),
			}));
		},
	});

	pi.registerTool({
		name: "loom_topic_publish_result",
		label: "Publish Loom Topic Result",
		description: "Publish the final integrated Topic result for Owner review. Only the Responsible Agent may cross this result boundary.",
		promptGuidelines: ["Publish only the integrated Topic result after reconciling Participant replies, evidence, limitations, and the completion boundary."],
		parameters: Type.Object({
			topicId: Type.String(),
			expectedVersion: Type.Number({ description: "Current Topic version from loom_topic_get" }),
			summary: Type.String(),
			currentState: Type.Optional(Type.String()),
			nextStep: Type.Optional(Type.String()),
			limitations: Type.Optional(Type.String()),
			evidence: Type.Optional(Type.Array(Type.Object({
				type: Type.String(), id: Type.String(), label: Type.Optional(Type.String()),
			}))),
		}),
		async execute(_id, params, signal) {
			const { agentID } = environment();
			return result(await request(`/api/topics/${encodeURIComponent(params.topicId)}`, signal, {
				method: "PATCH",
				body: JSON.stringify({
					actor: agentID, expectedVersion: params.expectedVersion, publishResult: true,
					brief: {
						summary: params.summary, currentState: params.currentState, nextStep: params.nextStep,
						limitations: params.limitations, evidence: params.evidence ?? [],
					},
				}),
			}));
		},
	});
}
