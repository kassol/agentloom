import { getSessionInfo, query } from "@anthropic-ai/claude-agent-sdk";
import { readFile } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { createInterface } from "node:readline";

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

function stableJSON(value) {
  if (Array.isArray(value)) return `[${value.map(stableJSON).join(",")}]`;
  if (value && typeof value === "object") return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${stableJSON(value[key])}`).join(",")}}`;
  return JSON.stringify(value);
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

function sdkUserMessage(input, sessionRef, includeDeveloper = false) {
	const developer = includeDeveloper ? developerInput(input) : "";
	const user = textInput(input);
	const content = developer ? `<loom_developer_context>\n${developer}\n</loom_developer_context>\n\n${user}` : user;
	return {type: "user", message: {role: "user", content}, parent_tool_use_id: null, origin: {kind: "human"}, session_id: sessionRef};
}

async function* initialInput(input, sessionRef) {
	yield sdkUserMessage(input, sessionRef);
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
  const sessionRef = payload.sessionRef;
  const runtimeTurnRef = randomUUID();
  const developer = developerInput(payload.input);
  let sdkQuery, turn;
  try {
	if (!payload.resume && await getSessionInfo(sessionRef, {dir: payload.cwd})) {
	  respond(frame, false, {}, "Claude Runtime cannot create the reserved session");
	  return;
	}
    const options = {
      cwd: payload.cwd,
      ...(payload.resume ? {resume: sessionRef} : {sessionId: sessionRef}),
      ...(developer ? {systemPrompt: {type: "preset", preset: "claude_code", append: developer}} : {}),
      canUseTool: (toolName, input, callbackOptions) => requestPermission(turn, toolName, input, callbackOptions)
    };
    sdkQuery = query({prompt: initialInput(payload.input, sessionRef), options});
	turn = {frame, operations: [frame], query: sdkQuery, runtimeTurnRef, callbacks: new Map(), accepted: false, terminal: false, expectedResults: 1, observedResults: 0};
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
      const content = message?.type === "assistant" ? assistantContent(message) : message?.type === "user" ? toolResultContent(message) : [];
      for (const block of content) {
        emit(frame, "content", {runtimeTurnRef, phase: "completed", content: block});
      }
      if (message?.type === "result") {
		turn.observedResults++;
		const observedUsage = usageFrom(message);
        emit(frame, "usage", {runtimeTurnRef, ...observedUsage});
		if (turn.observedResults < turn.expectedResults) continue;
		turn.terminal = true;
		const interrupted = message.subtype !== "success" && turn.interruptRequested && ["aborted_streaming", "aborted_tools"].includes(message.terminal_reason);
		const terminal = message.subtype === "success" ? "turn_completed" : interrupted ? "turn_interrupted" : "turn_failed";
		emit(turn.frame, terminal, {
		  runtimeTurnRef, ...(terminal === "turn_failed" ? {message: "Claude Runtime Turn failed"} : {})
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
  case "resume_binding": {
	const info = await getSessionInfo(payload.sessionRef, {dir: payload.cwd});
	if (!info || info.sessionId !== payload.sessionRef || info.cwd !== payload.cwd) {
      respond(frame, false, {}, "Claude session was not found");
      return;
    }
    respond(frame, true);
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
		yield sdkUserMessage(payload.input, payload.sessionRef, true);
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
