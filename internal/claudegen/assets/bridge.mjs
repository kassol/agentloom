import * as ClaudeAgentSDK from "@anthropic-ai/claude-agent-sdk";
import { readFile } from "node:fs/promises";
import { createHash, randomUUID } from "node:crypto";
import { createInterface } from "node:readline";

const {getSessionInfo, getSessionMessages, listSessions, query} = ClaudeAgentSDK;
const pkg = JSON.parse(await readFile(new URL("./node_modules/@anthropic-ai/claude-agent-sdk/package.json", import.meta.url), "utf8"));
const capabilities = ["interrupt", "approval", "hooks", "mcp", "session_resume"];
const identity = {
  protocolVersion: 1,
  bridgeBuild: "claude-bridge-v1",
  nodeVersion: process.versions.node,
  sdkVersion: pkg.version,
  claudeCodeVersion: pkg.claudeCodeVersion,
  capabilities
};

if (process.argv[2] === "--self-test") {
  process.stdout.write(JSON.stringify(identity) + "\n");
  process.exit(0);
}

const write = (frame) => process.stdout.write(JSON.stringify(frame) + "\n");
const respond = (frame, accepted, data = {}, error) => write({
  kind: "response", requestId: frame.requestId, turnId: frame.turnId,
  operation: frame.operation, accepted, data, ...(error ? {error} : {})
});
const emit = (frame, event, data) => write({
  kind: "event", class: "control", event, turnId: frame.turnId,
  operation: frame.operation, data
});

write({kind: "hello", ...identity, os: process.platform, arch: process.arch === "x64" ? "amd64" : process.arch});

let initialized = false;
let active;

const authenticationEnvironmentKeys = new Set([
  "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
  "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE",
  "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_WEB_IDENTITY_TOKEN_FILE",
  "AWS_ROLE_ARN", "AWS_ROLE_SESSION_NAME", "AWS_REGION", "AWS_DEFAULT_REGION",
  "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_QUOTA_PROJECT"
]);
const authenticationEnvironmentPrefixes = [
  "CLAUDE_CODE_USE_", "CLAUDE_CODE_SKIP_", "ANTHROPIC_AWS_", "ANTHROPIC_BEDROCK_",
  "ANTHROPIC_VERTEX_", "ANTHROPIC_GOOGLE_CLOUD_", "ANTHROPIC_FOUNDRY_", "AWS_CONTAINER_"
];
const hostAuthenticationEnvironment = Object.fromEntries(Object.entries(process.env).filter(([key]) =>
  authenticationEnvironmentKeys.has(key) || authenticationEnvironmentPrefixes.some((prefix) => key.startsWith(prefix))
));
for (const key of Object.keys(hostAuthenticationEnvironment)) delete process.env[key];

function stableJSON(value) {
  if (Array.isArray(value)) return `[${value.map(stableJSON).join(",")}]`;
  if (value && typeof value === "object") return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${stableJSON(value[key])}`).join(",")}}`;
  return JSON.stringify(value);
}

function safeSessionName(value) {
  const name = safeName(value);
  return name ? name.slice(0, 120) : "";
}

function sessionRevision(info, messages) {
  const publicMessages = (Array.isArray(messages) ? messages : []).map((message) => ({
    type: message?.type === "user" || message?.type === "assistant" ? message.type : "",
    uuid: typeof message?.uuid === "string" ? message.uuid : "",
    sessionId: typeof message?.session_id === "string" ? message.session_id : "",
    parentToolUseId: typeof message?.parent_tool_use_id === "string" ? message.parent_tool_use_id : null,
    parentAgentId: typeof message?.parent_agent_id === "string" ? message.parent_agent_id : null
  }));
  return "claude-session:" + createHash("sha256").update(stableJSON({
    sessionId: String(info?.sessionId || ""),
    cwd: String(info?.cwd || ""),
    lastModified: Number(info?.lastModified || 0),
    fileSize: Number(info?.fileSize || 0),
    createdAt: Number(info?.createdAt || 0),
    messages: publicMessages
  })).digest("hex");
}

async function inspectSession(sessionRef, dir) {
  const info = await getSessionInfo(sessionRef, dir ? {dir} : undefined);
  if (!info || info.sessionId !== sessionRef) return undefined;
  const messages = await getSessionMessages(sessionRef, dir ? {dir} : undefined);
  if (!Array.isArray(messages) || messages.some((message) => message?.session_id !== sessionRef || typeof message?.uuid !== "string")) {
    throw new Error("Claude session returned invalid passive identity evidence");
  }
  return {
    sessionRef,
    name: safeSessionName(info.customTitle) || "Claude session",
    cwd: typeof info.cwd === "string" ? info.cwd : "",
    updatedAt: new Date(Number(info.lastModified || 0)).toISOString(),
    revision: sessionRevision(info, messages),
    compatible: Boolean(info.cwd)
  };
}

function ownerConfiguration(payload) {
  const raw = payload?.configuration;
  if (!raw || !Array.isArray(raw.settingSources) || !raw.authentication || typeof raw.authentication !== "object") {
    throw new Error("Claude Runtime owner configuration is required");
  }
  const settingSources = raw.settingSources.map((source) => String(source));
  if (settingSources.length === 0 || new Set(settingSources).size !== settingSources.length || settingSources.some((source) => !["user", "project", "local"].includes(source))) {
    throw new Error("Claude Runtime setting sources are invalid");
  }
  const category = String(raw.authentication.category || "");
  const source = String(raw.authentication.source || "");
  const supported = category === "console" && source === "api_key"
    || category === "gateway" && source === "gateway"
    || category === "cloud" && ["bedrock", "vertex", "foundry", "anthropic_aws", "anthropic_google_cloud", "mantle"].includes(source);
  if (!supported) throw new Error("Claude Runtime authentication source cannot be safely verified");
  return {settingSources, authentication: {category, source}};
}

function selectedAuthenticationEnvironment(configuration) {
  const source = configuration.authentication.source;
  const allowed = source === "api_key" ? ["ANTHROPIC_API_KEY"]
    : source === "bedrock" ? ["AWS_", "ANTHROPIC_BEDROCK_"]
    : source === "vertex" ? ["GOOGLE_", "ANTHROPIC_VERTEX_"]
    : source === "foundry" ? ["ANTHROPIC_FOUNDRY_"]
    : source === "anthropic_aws" ? ["AWS_", "ANTHROPIC_AWS_"]
    : source === "anthropic_google_cloud" ? ["GOOGLE_", "ANTHROPIC_GOOGLE_CLOUD_"]
    : source === "mantle" ? ["AWS_", "ANTHROPIC_BEDROCK_MANTLE_"]
    : source === "gateway" ? ["ANTHROPIC_AUTH_TOKEN"]
    : [];
  const env = {...process.env};
  for (const [key, value] of Object.entries(hostAuthenticationEnvironment)) {
    if (allowed.some((prefix) => key === prefix || key.startsWith(prefix))) env[key] = value;
  }
  const selector = {
    bedrock: "CLAUDE_CODE_USE_BEDROCK", vertex: "CLAUDE_CODE_USE_VERTEX",
    foundry: "CLAUDE_CODE_USE_FOUNDRY", anthropic_aws: "CLAUDE_CODE_USE_ANTHROPIC_AWS",
    anthropic_google_cloud: "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
    mantle: "CLAUDE_CODE_USE_MANTLE", gateway: "CLAUDE_CODE_USE_GATEWAY"
  }[source];
  if (selector) env[selector] = "1";
  return env;
}

async function verifyAuthentication(control, configuration) {
  const account = await control.accountInfo();
  const provider = account?.apiProvider;
  const source = configuration.authentication.source;
  const providerBySource = {
    bedrock: "bedrock", vertex: "vertex", foundry: "foundry", anthropic_aws: "anthropicAws",
    anthropic_google_cloud: "anthropicGoogleCloud", mantle: "mantle", gateway: "gateway"
  };
  const accepted = source === "api_key"
    ? provider === "firstParty" && typeof account?.apiKeySource === "string" && account.apiKeySource !== "oauth"
    : provider === providerBySource[source];
  if (!accepted) throw new Error("Claude Runtime authentication did not match the selected source");
  return {settingSources: [...configuration.settingSources], authentication: {...configuration.authentication, validation: "accepted"}};
}

function safeName(value) {
  if (typeof value !== "string") return "";
  const normalized = value.trim();
  return normalized && normalized.length <= 256 && !/[\r\n\0/\\]/.test(normalized) ? normalized : "";
}

function resource(kind, name, extra = {}) {
  const safe = safeName(name);
  return safe ? {id: `${kind}:${safe}`, name: safe, kind, path: "", source: "claude_agent_sdk_reload", enabled: true, ...extra} : undefined;
}

function resourceInventory(skillState, pluginState) {
  const skills = new Set((Array.isArray(skillState?.skills) ? skillState.skills : []).map((skill) => safeName(skill?.name)).filter(Boolean));
  const resources = [];
  for (const name of [...skills].sort()) resources.push(resource("skill", name));
  for (const command of Array.isArray(pluginState?.commands) ? pluginState.commands : []) {
    const name = safeName(command?.name);
    if (name && !skills.has(name)) resources.push(resource("command", name));
  }
  for (const plugin of Array.isArray(pluginState?.plugins) ? pluginState.plugins : []) {
    const item = resource("extension", plugin?.name);
    if (item) resources.push(item);
  }
  for (const server of Array.isArray(pluginState?.mcpServers) ? pluginState.mcpServers : []) {
    const status = ["connected", "failed", "needs-auth", "pending", "disabled"].includes(server?.status) ? server.status : "unknown";
    const item = resource("mcp", server?.name, {enabled: status !== "disabled", status});
    if (item) resources.push(item);
  }
  return resources.filter(Boolean).sort((left, right) => left.id.localeCompare(right.id));
}

function requestPermission(turn, toolName, input, options) {
  const callbackId = `${options.requestId}:${options.toolUseID}`;
  const fingerprint = stableJSON({toolName, input});
  const existing = turn.callbacks.get(callbackId);
  if (existing) {
    if (existing.fingerprint !== fingerprint) process.exit(70);
    return existing.promise;
  }
  let settle;
  const promise = new Promise((resolve) => { settle = resolve; });
	const callback = {callbackId, toolCallId: options.toolUseID, input, fingerprint, promise, settle, settled: false};
  turn.callbacks.set(callbackId, callback);
  const questions = toolName === "AskUserQuestion" && Array.isArray(input?.questions) ? input.questions : undefined;
  emit(turn.frame, questions ? "needs_you" : "approval", questions
    ? {callbackId, toolCallId: options.toolUseID, questions}
    : {callbackId, toolCallId: options.toolUseID, toolName, input});
  options.signal?.addEventListener("abort", () => {
	if (!callback.settled) {
	  callback.settled = true;
	  settle({behavior: "deny", message: "Runtime callback aborted", interrupt: true, toolUseID: options.toolUseID});
	}
  }, {once: true});
  return promise;
}

function textInput(input) {
  const blocks = Array.isArray(input) ? input : [];
  return blocks.filter((block) => block?.kind === "text" && block.role !== "developer")
    .map((block) => block.text).filter(Boolean).join("\n\n");
}

function developerInput(input) {
  const blocks = Array.isArray(input) ? input : [];
  return blocks.filter((block) => block?.kind === "text" && block.role === "developer")
    .map((block) => block.text).filter(Boolean).join("\n\n");
}

const generationImageModels = new Set([
	"claude-3-haiku-20240307", "claude-3-opus-20240229", "claude-3-5-haiku-20241022", "claude-3-5-sonnet-20240620", "claude-3-5-sonnet-20241022", "claude-3-7-sonnet-20250219",
  "claude-opus-4-20250514", "claude-opus-4-1-20250805", "claude-opus-4-5-20251101", "claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8", "claude-opus-5",
  "claude-sonnet-4-20250514", "claude-sonnet-4-5-20250929", "claude-sonnet-4-6", "claude-sonnet-5", "claude-haiku-4-5-20251001", "claude-fable-5", "claude-mythos-5"
]);

function modelDescriptor(raw) {
  const levels = ["default", ...(raw?.supportsEffort && Array.isArray(raw.supportedEffortLevels) ? raw.supportedEffortLevels : [])];
  return {
    provider: "anthropic", id: String(raw?.value || ""), displayName: String(raw?.displayName || raw?.value || ""),
    reasoning: Boolean(raw?.supportsAdaptiveThinking || raw?.supportsEffort), thinkingLevels: [...new Set(levels)],
    defaultThinkingLevel: "default", imageInput: typeof raw?.resolvedModel === "string" && generationImageModels.has(raw.resolvedModel)
  };
}

const modelCatalog = (raw) => [{provider: "anthropic", id: "default", displayName: "SDK default", reasoning: true, thinkingLevels: ["default"], defaultThinkingLevel: "default", imageInput: false}, ...(Array.isArray(raw) ? raw : []).map(modelDescriptor).filter((model) => model.id)];

function modelState(models, requested) {
  const current = models.find((model) => model.provider === requested.provider && model.id === requested.model);
  if (!current) throw new Error("Claude model is absent from public generation evidence");
  const thinkingLevel = requested.thinkingLevel || "default";
  if (!current.thinkingLevels.includes(thinkingLevel)) throw new Error("Claude thinking level is unavailable for this model");
  return {current, models, thinkingLevel};
}

function imageBlock(block) {
  if (!block?.ref || !["image/png", "image/jpeg", "image/gif", "image/webp"].includes(block.mimeType)) {
    throw new Error("Claude image input is invalid");
  }
  return readFile(block.ref).then((data) => ({type: "image", source: {type: "base64", media_type: block.mimeType, data: data.toString("base64")}}));
}

async function sdkUserMessage(input, sessionRef, includeDeveloper = false) {
	const developer = includeDeveloper ? developerInput(input) : "";
	const user = textInput(input);
	const text = developer ? `<loom_developer_context>\n${developer}\n</loom_developer_context>\n\n${user}` : user;
	const content = [{type: "text", text}, ...await Promise.all((Array.isArray(input) ? input : []).filter((block) => block?.kind === "image").map(imageBlock))];
	return {type: "user", message: {role: "user", content}, parent_tool_use_id: null, origin: {kind: "human"}, session_id: sessionRef};
}

async function* initialInput(input, sessionRef, ready = Promise.resolve()) {
	await ready;
	yield await sdkUserMessage(input, sessionRef);
	await new Promise(() => {});
}

function usageFrom(result) {
  const source = "claude_agent_sdk";
  const unavailable = () => ({available: false, source});
  const number = (object, key, scale = 1) => {
    if (!object || !Object.hasOwn(object, key) || !Number.isFinite(object[key]) || object[key] < 0) return unavailable();
    return {available: true, value: Math.round(object[key] * scale), source};
  };
  const text = (value) => typeof value === "string" && value ? {available: true, value, source} : {available: false, source};
  const models = Object.entries(result?.modelUsage || {}).map(([model, raw]) => {
    const input = ["inputTokens", "cacheReadInputTokens", "cacheCreationInputTokens"].map((key) => number(raw, key));
    const inputTokens = input.every((item) => item.available) ? {available: true, value: input.reduce((sum, item) => sum + item.value, 0), source} : unavailable();
    return {provider: text(raw?.provider), model: text(model), usage: {
      inputTokens, cachedInputTokens: number(raw, "cacheReadInputTokens"), outputTokens: number(raw, "outputTokens"),
      reasoningOutputTokens: unavailable(), totalTokens: unavailable(), calls: unavailable(), costMicros: number(raw, "costUSD", 1_000_000)
    }, contextWindow: number(raw, "contextWindow")};
  });
  const sum = (key) => models.length && models.every((item) => item.usage[key].available)
    ? {available: true, value: models.reduce((total, item) => total + item.usage[key].value, 0), source} : unavailable();
  return {usage: {
    inputTokens: sum("inputTokens"), cachedInputTokens: sum("cachedInputTokens"), outputTokens: sum("outputTokens"),
    reasoningOutputTokens: unavailable(), totalTokens: unavailable(), calls: unavailable(), costMicros: sum("costMicros")
  }, details: {observedAt: {available: true, value: new Date().toISOString(), source: "canonical_turn_ledger"}, models}};
}

function certificationOptions(policy) {
  if (policy == null) return {};
  if (policy?.purpose !== "real_smoke" || !Array.isArray(policy.allowedTools) || policy.allowedTools.length !== 0
    || policy.maxTurns !== 1 || !Number.isFinite(policy.maxBudgetUsd) || policy.maxBudgetUsd <= 0 || policy.maxBudgetUsd > 0.05) {
    throw new Error("Claude certification Turn policy is invalid");
  }
  return {
    tools: [],
    allowedTools: [],
    permissionMode: "dontAsk",
    maxTurns: 1,
    maxBudgetUsd: policy.maxBudgetUsd,
    includePartialMessages: true
  };
}

function assistantContent(message) {
  const blocks = Array.isArray(message?.message?.content) ? message.message.content : [];
  return blocks.flatMap((block, index) => {
    const id = block?.id || `${message.uuid || message.message?.id || "assistant"}-${index}`;
    if (block?.type === "text" && block.text) return [{id, kind: "assistant_text", text: block.text}];
    if (block?.type === "thinking" && block.thinking) return [{id, kind: "reasoning", text: block.thinking}];
    if (block?.type === "tool_use" && block.name) return [{id, kind: "tool_call", toolCall: {name: block.name, arguments: block.input ?? {}}}];
    return [];
  });
}

function toolResultContent(message) {
  const raw = message?.message?.content;
  const blocks = Array.isArray(raw) ? raw : [];
  return blocks.flatMap((block, index) => {
    if (block?.type !== "tool_result" || !block.tool_use_id) return [];
    const content = typeof block.content === "string" ? block.content : Array.isArray(block.content)
      ? block.content.map((part) => part?.text || "").filter(Boolean).join("\n") : "";
    return [{
      id: `${message.uuid || "tool-result"}-${index}`, kind: "tool_result",
      toolResult: {toolCallId: block.tool_use_id, text: content, success: !block.is_error}
    }];
  });
}

async function runTurn(frame) {
  const payload = frame.payload || {};
  const configuration = ownerConfiguration(payload);
  const sessionRef = payload.sessionRef;
  const runtimeTurnRef = randomUUID();
  const developer = developerInput(payload.input);
  let sdkQuery, turn;
  try {
	if (payload.boundaryRevision) {
	  const boundary = await inspectSession(sessionRef, payload.cwd);
	  if (!boundary) {
		respond(frame, false, {}, "Claude session was not found");
		return;
	  }
	  if (boundary.revision !== payload.boundaryRevision) {
		respond(frame, false, {code: "native_conversation_divergence", actualRevision: boundary.revision}, "Claude native conversation changed outside Loom");
		return;
	  }
	}
	if (!payload.resume && await getSessionInfo(sessionRef, {dir: payload.cwd})) {
	  respond(frame, false, {}, "Claude Runtime cannot create the reserved session");
	  return;
	}
    const options = {
      cwd: payload.cwd,
      settingSources: configuration.settingSources,
      env: selectedAuthenticationEnvironment(configuration),
      ...(payload.resume ? {resume: sessionRef} : {sessionId: sessionRef}),
      ...(developer ? {systemPrompt: {type: "preset", preset: "claude_code", append: developer}} : {}),
      canUseTool: (toolName, input, callbackOptions) => requestPermission(turn, toolName, input, callbackOptions),
      ...certificationOptions(payload.certificationPolicy)
    };
	let releaseInput;
	const inputReady = new Promise((resolve) => { releaseInput = resolve; });
    sdkQuery = query({prompt: initialInput(payload.input, sessionRef, inputReady), options});
	await verifyAuthentication(sdkQuery, configuration);
	let expectedModel = "";
	if (payload.model) {
	  const available = await sdkQuery.supportedModels();
	  const rawCurrent = available.find((model) => model?.value === payload.model);
	  const state = modelState(modelCatalog(available), {provider: "anthropic", model: payload.model, thinkingLevel: payload.thinkingLevel || "default"});
	  expectedModel = rawCurrent?.resolvedModel || rawCurrent?.value || "";
	  await sdkQuery.setModel(state.current.id === "default" ? undefined : state.current.id);
	  await sdkQuery.applyFlagSettings({effortLevel: state.thinkingLevel === "default" ? null : state.thinkingLevel});
	  if ((Array.isArray(payload.input) ? payload.input : []).some((block) => block?.kind === "image") && !state.current.imageInput) {
		throw new Error("Claude model has no generation-proven image input");
	  }
	}
	releaseInput();
	turn = {frame, operations: [frame], query: sdkQuery, runtimeTurnRef, expectedModel, callbacks: new Map(), accepted: false, terminal: false, expectedResults: 1, observedResults: 0};
	active = turn;
    for await (const message of sdkQuery) {
      if (message?.session_id && message.session_id !== sessionRef) {
        throw new Error("Claude SDK initialized a different session");
      }
	  if (!turn.accepted) {
		if (message?.type !== "system" || message.subtype !== "init" || message.session_id !== sessionRef) {
          throw new Error("Claude SDK did not initialize the reserved session");
        }
		turn.accepted = true;
        emit(frame, "turn_started", {runtimeTurnRef});
		for (const [index, block] of (Array.isArray(payload.input) ? payload.input : []).filter((block) => block?.kind === "text" && block.role !== "developer").entries()) {
		  emit(frame, "content", {runtimeTurnRef, phase: "completed", content: {id: `user-${index}`, kind: "user_text", text: block.text || ""}});
		}
        respond(frame, true, {runtimeTurnRef});
        continue;
      }
	  if (message?.type === "assistant" && !message.parent_tool_use_id && turn.expectedModel && message.message?.model !== turn.expectedModel) {
		throw new Error("Claude SDK used a different model than the committed selection");
	  }
	  if (message?.type === "stream_event" && message.event?.type === "content_block_delta"
		&& message.event?.delta?.type === "text_delta" && message.event.delta.text) {
		turn.certificationStreamed = true;
		emit(frame, "content", {
		  runtimeTurnRef, phase: "delta",
		  content: {id: `certification-assistant-${message.event.index || 0}`, kind: "assistant_text", text: message.event.delta.text}
		});
		continue;
	  }
      const content = message?.type === "assistant" && !turn.certificationStreamed
		? assistantContent(message) : message?.type === "user" ? toolResultContent(message) : [];
      for (const block of content) {
        emit(frame, "content", {runtimeTurnRef, phase: "completed", content: block});
      }
      if (message?.type === "result") {
		turn.observedResults++;
		const observedUsage = usageFrom(message);
        emit(frame, "usage", {runtimeTurnRef, ...observedUsage});
		if (turn.observedResults < turn.expectedResults) continue;
		turn.terminal = true;
		const boundary = payload.boundaryRevision ? await inspectSession(sessionRef, payload.cwd) : undefined;
		if (payload.boundaryRevision && !boundary) {
		  throw new Error("Claude session boundary was unavailable after the Turn");
		}
		const interrupted = message.subtype !== "success" && turn.interruptRequested && ["aborted_streaming", "aborted_tools"].includes(message.terminal_reason);
		const terminal = message.subtype === "success" ? "turn_completed" : interrupted ? "turn_interrupted" : "turn_failed";
		emit(turn.frame, terminal, {
		  runtimeTurnRef, ...(boundary ? {nativeRevision: boundary.revision} : {}),
		  ...(terminal === "turn_failed" ? {message: "Claude Runtime Turn failed"} : {})
		});
		break;
      }
    }
	if (turn.accepted && !turn.terminal) process.exit(70);
  } catch {
	if (!active?.accepted) {
      respond(frame, false, {}, "Claude Runtime could not initialize the reserved session");
    } else {
	  process.exit(70);
    }
  } finally {
    if (active?.frame.operation === frame.operation) active = undefined;
    sdkQuery?.close();
  }
}

async function handleCommand(frame) {
  const payload = frame.payload || {};
  switch (frame.command) {
	case "discover_sessions": {
	  const sessions = await listSessions({limit: 200});
	  const candidates = [];
	  for (let index = 0; index < sessions.length; index += 8) {
		const inspected = await Promise.all(sessions.slice(index, index + 8).map((session) =>
		  session?.sessionId ? inspectSession(session.sessionId, session.cwd).catch(() => undefined) : undefined
		));
		candidates.push(...inspected.filter(Boolean));
	  }
	  respond(frame, true, {candidates});
	  return;
	}
	case "inspect_session": {
	  const inspected = await inspectSession(payload.sessionRef, payload.cwd);
	  if (!inspected) {
		respond(frame, false, {}, "Claude session was not found");
		return;
	  }
	  respond(frame, true, inspected);
	  return;
	}
	case "inspect_model_control": {
	  const configuration = ownerConfiguration(payload);
	  const requested = {provider: payload.provider || "anthropic", model: payload.model, thinkingLevel: payload.thinkingLevel || "default"};
	  if (!requested.model) {
		respond(frame, false, {}, "Claude model selection is not committed");
		return;
	  }
	  const control = query({prompt: (async function* () { await new Promise(() => {}); })(), options: {persistSession: false, settingSources: configuration.settingSources, env: selectedAuthenticationEnvironment(configuration)}});
	  try {
		await verifyAuthentication(control, configuration);
		const models = modelCatalog(await control.supportedModels());
		respond(frame, true, modelState(models, requested));
	  } finally { control.close(); }
	  return;
	}
	case "select_model": {
	  const configuration = ownerConfiguration(payload);
	  if (typeof payload.sessionRef !== "string" || typeof payload.cwd !== "string") {
		respond(frame, false, {}, "Claude Runtime model selection has no exact binding");
		return;
	  }
	  const control = query({prompt: (async function* () { await new Promise(() => {}); })(), options: {cwd: payload.cwd, persistSession: false, settingSources: configuration.settingSources, env: selectedAuthenticationEnvironment(configuration), ...(payload.resume ? {resume: payload.sessionRef} : {sessionId: payload.sessionRef})}});
	  try {
		await verifyAuthentication(control, configuration);
		const models = modelCatalog(await control.supportedModels());
		const previous = modelState(models, payload.current || {});
		const selected = modelState(models, payload.selection || {});
		try {
		  await control.setModel(selected.current.id === "default" ? undefined : selected.current.id);
		  await control.applyFlagSettings({effortLevel: selected.thinkingLevel === "default" ? null : selected.thinkingLevel});
		} catch (error) {
		  try {
			await control.setModel(previous.current.id === "default" ? undefined : previous.current.id);
			await control.applyFlagSettings({effortLevel: previous.thinkingLevel === "default" ? null : previous.thinkingLevel});
		  } catch { process.exit(70); }
		  respond(frame, false, {}, "Claude Runtime rejected the model selection");
		  return;
		}
		respond(frame, true, {...selected, effectEvidence: {binding: "exact", model: "sdk_ack", thinking: "sdk_ack"}});
	  } finally { control.close(); }
	  return;
	}
	case "inspect_resources": {
	  const configuration = ownerConfiguration(payload);
	  if (typeof payload.cwd !== "string") {
		respond(frame, false, {}, "Claude Runtime resource inspection has no working directory");
		return;
	  }
	  const control = query({prompt: (async function* () { await new Promise(() => {}); })(), options: {cwd: payload.cwd, persistSession: false, settingSources: configuration.settingSources, env: selectedAuthenticationEnvironment(configuration)}});
	  try {
		const verified = await verifyAuthentication(control, configuration);
		const [skillState, pluginState] = await Promise.all([
		  control.reloadSkills(), control.reloadPlugins()
		]);
		respond(frame, true, {resources: resourceInventory(skillState, pluginState), configuration: verified});
	  } finally { control.close(); }
	  return;
	}
	case "inspect_configuration": {
	  const configuration = ownerConfiguration(payload);
	  if (typeof payload.cwd !== "string") {
		respond(frame, false, {}, "Claude Runtime configuration inspection has no working directory");
		return;
	  }
	  const control = query({prompt: (async function* () { await new Promise(() => {}); })(), options: {cwd: payload.cwd, persistSession: false, settingSources: configuration.settingSources, env: selectedAuthenticationEnvironment(configuration)}});
	  try {
		respond(frame, true, await verifyAuthentication(control, configuration));
	  } finally { control.close(); }
	  return;
	}
  case "resume_binding": {
	const inspected = await inspectSession(payload.sessionRef, payload.cwd);
	if (inspected && payload.expectedRevision && inspected.revision !== payload.expectedRevision) {
      respond(frame, false, {code: "native_conversation_divergence", actualRevision: inspected.revision}, "Claude native conversation changed outside Loom");
      return;
    }
	if (!inspected || inspected.cwd !== payload.cwd) {
      respond(frame, false, {}, "Claude session was not found");
      return;
    }
    respond(frame, true, {revision: inspected.revision});
    emit(frame, "binding_resumed", {sessionRef: payload.sessionRef});
    return;
  }
  case "start_turn":
    if (active || typeof payload.sessionRef !== "string" || typeof payload.cwd !== "string") {
      respond(frame, false, {}, "Claude Runtime cannot start this Turn");
      return;
    }
    void runTurn(frame);
    return;
  case "continue_turn":
	if (!active?.accepted || active.terminal || active.runtimeTurnRef !== payload.runtimeTurnRef) {
      respond(frame, false, {}, "Claude Runtime has no matching active Turn");
      return;
    }
	{
	  const turn = active;
	  turn.operations.push(frame);
	  turn.expectedResults++;
    try {
      await turn.query.streamInput((async function* () {
		yield await sdkUserMessage(payload.input, payload.sessionRef, true);
		for (const [index, block] of (Array.isArray(payload.input) ? payload.input : []).filter((block) => block?.kind === "text" && block.role !== "developer").entries()) {
		  emit(frame, "content", {runtimeTurnRef: turn.runtimeTurnRef, phase: "completed", content: {id: `continue-user-${index}`, kind: "user_text", text: block.text || ""}});
		}
      })());
      respond(frame, true);
    } catch (error) {
	  turn.expectedResults--;
	  process.exit(70);
    }
	}
    return;
  case "interrupt_turn":
	if (!active?.accepted || active.terminal || active.runtimeTurnRef !== payload.runtimeTurnRef) {
      respond(frame, false, {}, "Claude Runtime has no matching active Turn");
      return;
    }
	{
	  const turn = active;
	  turn.operations.push(frame);
    try {
	  turn.interruptRequested = true;
	  await turn.query.interrupt();
	  emit(frame, "interrupt_receipt", {runtimeTurnRef: turn.runtimeTurnRef});
      respond(frame, true);
    } catch (error) {
	  process.exit(70);
    }
	}
    return;
  case "resolve_approval":
    if (!active?.accepted || active.terminal) {
      respond(frame, false, {}, "Claude Runtime has no matching active Turn");
      return;
    }
    {
      const callback = active.callbacks.get(payload.callbackId);
	  if (!callback || callback.settled) {
        respond(frame, false, {}, "Claude Runtime callback is unavailable");
        return;
      }
	  callback.settled = true;
      const allow = payload.decision === "approve";
	  callback.settle(allow
		? {behavior: "allow", updatedInput: callback.input, toolUseID: callback.toolCallId}
        : {behavior: "deny", message: "Owner did not authorize this tool", interrupt: false, toolUseID: callback.toolCallId});
      respond(frame, true);
    }
    return;
  case "resolve_needs_you":
    if (!active?.accepted || active.terminal) {
      respond(frame, false, {}, "Claude Runtime has no matching active Turn");
      return;
    }
    {
      const callback = active.callbacks.get(payload.callbackId);
	  if (!callback || callback.settled) {
        respond(frame, false, {}, "Claude Runtime callback is unavailable");
        return;
      }
	  callback.settled = true;
      callback.settle({behavior: "deny", message: payload.persisted ? "Waiting for Owner input" : "Owner request could not be persisted", interrupt: Boolean(payload.persisted), toolUseID: callback.toolCallId});
      if (payload.persisted) {
		active.interruptRequested = true;
		await active.query.interrupt();
	  }
      respond(frame, true);
    }
    return;
  default:
    respond(frame, false, {}, "command is not implemented by this bridge build");
  }
}

const lines = createInterface({input: process.stdin, crlfDelay: Infinity, terminal: false});
for await (const line of lines) {
  let frame;
  try {
    frame = JSON.parse(line);
  } catch {
    process.stderr.write("Claude bridge received malformed JSON\n");
    process.exit(65);
  }
  if (!initialized) {
    if (frame.kind !== "initialize" || typeof frame.requestId !== "string" || typeof frame.agentId !== "string") {
      process.stderr.write("Claude bridge expected initialize\n");
      process.exit(65);
    }
    initialized = true;
    write({kind: "ready", requestId: frame.requestId, capabilities});
    continue;
  }
  if (frame.kind === "close") {
    active?.query.close();
    process.exit(0);
  }
  if (frame.kind !== "command" || typeof frame.requestId !== "string" || typeof frame.operation !== "string") {
    process.stderr.write("Claude bridge received unknown control frame\n");
    process.exit(65);
  }
  void handleCommand(frame).catch(() => respond(frame, false, {}, "Claude Runtime command failed"));
}
