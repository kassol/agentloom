import { describe, expect, it } from "vitest";
import { emptyFeed, reduceFeed, summarizeTask } from "./feed";

function typedTurn(items: Array<{ type: string; text?: string; timestamp?: string }>) {
  return {
    turnId: `turn-${items[0]?.timestamp || "fixture"}`,
    state: "completed",
    startedAt: items[0]?.timestamp || "",
    content: items.map((item, index) => ({
      id: `content-${index}`,
      kind: item.type === "answer" ? "assistant_text" : item.type === "thinking" ? "reasoning" : "user_text",
      text: item.text || "",
    })),
  };
}

describe("Runtime-normalized live events", () => {
  it("renders one typed canonical event without a raw compatibility branch", () => {
    const normalized = reduceFeed(emptyFeed, {
      seq: 1,
      ts: "2026-08-10T00:00:00Z",
      type: "loom/runtime-event",
      data: { kind: "content", turnId: "turn-1", contentPhase: "delta", content: { id: "answer-1", kind: "assistant_text", text: "hello" } },
    });
    expect(normalized.blocks).toEqual([
      { kind: "agent", id: "answer-1", ts: "2026-08-10T00:00:00Z", text: "hello", streaming: true },
    ]);
    expect(normalized.blocks).toEqual([
      { kind: "agent", id: "answer-1", ts: "2026-08-10T00:00:00Z", text: "hello", streaming: true },
    ]);
  });

  it("correlates streamed reasoning, text, and failed tools and closes partial blocks on abort", () => {
    const events = [
      { type: "loom/runtime-event", data: { kind: "content", contentPhase: "delta", content: { id: "thinking-1", kind: "reasoning", text: "checking" } } },
      { type: "loom/runtime-event", data: { kind: "content", contentPhase: "delta", content: { id: "answer-1", kind: "assistant_text", text: "partial" } } },
      { type: "loom/runtime-event", data: { kind: "content", contentPhase: "started", content: { id: "call-1", kind: "tool_call", toolCall: { name: "bash", arguments: { command: "pwd" } } } } },
      { type: "loom/runtime-event", data: { kind: "content", contentPhase: "completed", content: { id: "result-1", kind: "tool_result", toolResult: { toolCallId: "call-1", success: false, text: "permission denied" } } } },
      { type: "loom/turn-interrupted", data: { error: "Pi Turn aborted" } },
    ];
    const state = events.reduce(
      (current, event, index) => reduceFeed(current, {
        seq: index + 1,
        ts: "2026-08-10T00:00:00Z",
        type: event.type,
        data: event.data,
      }),
      emptyFeed,
    );

    expect(state.blocks).toEqual([
      { kind: "think", id: "thinking-1", ts: "2026-08-10T00:00:00Z", text: "checking", done: true },
      { kind: "agent", id: "answer-1", ts: "2026-08-10T00:00:00Z", text: "partial", streaming: false },
      { kind: "command", id: "call-1", ts: "2026-08-10T00:00:00Z", command: "pwd", status: "failed", exitCode: null, durationMs: null, output: "permission denied" },
      { kind: "sys", ts: "2026-08-10T00:00:00Z", cls: "warn", text: "■ interrupted Pi Turn aborted" },
    ]);
  });

  it("keeps an exec purpose as the primary label while retaining the raw command", () => {
    const toolCall = { name: "exec_command", description: "Confirm the repository state", arguments: { command: "git status --short" } };
    const live = reduceFeed(emptyFeed, {
      seq: 1, ts: "2026-08-13T00:00:00Z", type: "loom/runtime-event",
      data: { kind: "content", contentPhase: "started", content: { id: "call-1", kind: "tool_call", toolCall } },
    });
    expect(live.blocks[0]).toMatchObject({ kind: "command", command: "git status --short", description: "Confirm the repository state" });

    const history = reduceFeed(emptyFeed, { seq: 0, ts: "", type: "__history__", data: { turns: [{
      turnId: "turn-history", state: "completed", content: [{ id: "call-history", kind: "tool_call", toolCall }],
    }] } });
    expect(history.blocks[0]).toMatchObject({ kind: "command", command: "git status --short", description: "Confirm the repository state" });
  });

  it("shows one control error for paired terminal events and never invents tool success", () => {
    const events = [
      { type: "loom/runtime-event", data: { kind: "content", turnId: "turn-1", contentPhase: "completed", content: { id: "call-1", kind: "tool_call", toolCall: { name: "bash", arguments: { command: "deploy --prod" } } } } },
      { type: "loom/turn-failed", data: { turnId: "turn-1", error: "safe failure" } },
      { type: "loom/runtime-event", data: { kind: "terminal", turnId: "turn-1", outcome: { state: "failed", failure: { message: "safe failure" } } } },
    ];
    const state = events.reduce((current, event, index) => reduceFeed(current, {
      seq: index + 1, ts: `t${index + 1}`, type: event.type, data: event.data,
    }), emptyFeed);
    expect(state.blocks.filter((block) => block.kind === "sys" && block.cls === "err")).toHaveLength(1);
    expect(state.blocks.find((block) => block.kind === "command")).toMatchObject({ status: "failed" });

    const history = reduceFeed(emptyFeed, { seq: 0, ts: "", type: "__history__", data: { turns: [{
      turnId: "turn-history", state: "completed", content: [{ id: "call-history", kind: "tool_call", toolCall: { name: "bash", arguments: { command: "deploy --prod" } } }],
    }] } });
    expect(history.blocks.find((block) => block.kind === "command")).toMatchObject({ status: "interrupted" });

	const failedHistory = reduceFeed(emptyFeed, { seq: 0, ts: "", type: "__history__", data: { turns: [{
		turnId: "turn-failed", state: "failed", error: "safe durable failure", source: { kind: "internal", id: "msg-1" },
		content: [{ id: "call-failed", kind: "tool_call", toolCall: { name: "bash", arguments: { command: "deploy --prod" } } }],
	}] } });
	expect(failedHistory.blocks.find((block) => block.kind === "command")).toMatchObject({ status: "failed" });
	expect(failedHistory.blocks.filter((block) => block.kind === "sys" && block.cls === "err")).toEqual([
		{ kind: "sys", ts: "", cls: "err", text: "✖ failed: safe durable failure" },
	]);
  });
});

describe("durable Approval projection", () => {
  it("restores pending Approvals from the Agent snapshot across history seeding", () => {
    const approval = {
      approvalId: "ap-agent-1-a1", agentId: "agent-1", turnId: "turn-1", runtimeKind: "pi",
      method: "tool/bash", params: { command: "pwd" }, status: "pending", requestedAt: "2026-08-10T00:00:00Z",
    };
    const restored = reduceFeed(emptyFeed, {
      seq: 0, ts: "", type: "__approvals_snapshot__", data: { approvals: [approval] },
    });
    const seeded = reduceFeed(restored, {
      seq: 0, ts: "", type: "__history__", data: { turns: [] },
    });

    expect(seeded.approvals).toEqual({ [approval.approvalId]: approval });
    const resolved = reduceFeed(seeded, {
      seq: 1, ts: "2026-08-10T00:01:00Z", type: "loom/approval-resolved",
      data: { approvalId: approval.approvalId, decision: "approve", status: "approved" },
    });
    expect(resolved.approvals).toEqual({});
  });
});

describe("rollout history projection", () => {
  it("keeps one realtime command block and preserves description when completion omits it", () => {
    const started = reduceFeed(emptyFeed, {
      seq: 1,
      ts: "2026-08-12T01:00:00Z",
      type: "loom/runtime-event",
      data: {
        kind: "content",
        contentPhase: "started",
        content: {
          id: "cmd-1",
          kind: "tool_call",
          toolCall: { name: "exec_command", description: "Run the isolated command probe", arguments: { command: "printf probe-ok" } },
        }
      },
    });
    const completedCall = reduceFeed(started, {
      seq: 2,
      ts: "2026-08-12T01:00:01Z",
      type: "loom/runtime-event",
      data: {
        kind: "content",
        contentPhase: "completed",
        content: { id: "cmd-1", kind: "tool_call", toolCall: { name: "exec_command", arguments: { command: "printf probe-ok" } } },
      },
    });
    const completed = reduceFeed(completedCall, {
      seq: 3,
      ts: "2026-08-12T01:00:02Z",
      type: "loom/runtime-event",
      data: {
        kind: "content",
        contentPhase: "completed",
        content: { id: "result-1", kind: "tool_result", toolResult: { toolCallId: "cmd-1", success: true, text: "probe-ok" } },
      },
    });

    expect(completed.blocks).toHaveLength(1);
    expect(completed.blocks[0]).toMatchObject({
      kind: "command",
      id: "cmd-1",
      description: "Run the isolated command probe",
      status: "completed",
      output: "probe-ok",
    });
  });

  it("updates description during realtime lifecycle without creating another block", () => {
    const started = reduceFeed(emptyFeed, {
      seq: 1,
      ts: "2026-08-12T01:00:00Z",
      type: "loom/runtime-event",
      data: {
        kind: "content",
        contentPhase: "started",
        content: { id: "cmd-2", kind: "tool_call", toolCall: { name: "exec_command", arguments: { command: "printf probe-ok" } } },
      },
    });
    const updated = reduceFeed(started, {
      seq: 2,
      ts: "2026-08-12T01:00:00Z",
      type: "loom/runtime-event",
      data: {
        kind: "content",
        contentPhase: "updated",
        content: {
          id: "cmd-2",
          kind: "tool_call",
          toolCall: { name: "exec_command", description: "Run the isolated command probe", arguments: { command: "printf probe-ok" } },
        }
      },
    });

    expect(updated.blocks).toHaveLength(1);
    expect(updated.blocks[0]).toMatchObject({ id: "cmd-2", description: "Run the isolated command probe" });
  });

  it("projects command history and keeps the raw command fallback", () => {
    const state = reduceFeed(emptyFeed, {
      seq: 0,
      ts: "",
      type: "__history__",
      data: {
        turns: [{
          state: "completed",
          content: [
            { id: "cmd-described", kind: "tool_call", toolCall: { name: "exec_command", description: "Historical command", arguments: { command: "printf probe-ok" } } },
            { id: "result-described", kind: "tool_result", toolResult: { toolCallId: "cmd-described", success: true, text: "canonical" } },
            { id: "cmd-legacy", kind: "tool_call", toolCall: { name: "exec_command", arguments: { command: "printf legacy" } } },
            { id: "result-legacy", kind: "tool_result", toolResult: { toolCallId: "cmd-legacy", success: true, text: "" } },
          ],
        }],
      },
    });

    expect(state.blocks).toHaveLength(2);
    expect(state.blocks[0]).toMatchObject({ kind: "command", description: "Historical command", output: "canonical" });
    expect(state.blocks[1]).toMatchObject({ kind: "command", command: "printf legacy" });
    expect((state.blocks[1] as { description?: string }).description).toBeUndefined();
  });

  it("preserves description across history reconcile and prepend", () => {
    const turn = (id: string, command: string, description: string) => ({
      turnId: id,
      state: "completed",
      content: [
        { id: `cmd-${id}`, kind: "tool_call", toolCall: { name: "exec_command", description, arguments: { command } } },
        { id: `result-${id}`, kind: "tool_result", toolResult: { toolCallId: `cmd-${id}`, success: true, text: "" } },
      ],
    });
    const current = reduceFeed(emptyFeed, {
      seq: 0,
      ts: "",
      type: "__history__",
      data: { turns: [turn("current", "printf current", "Current command")] },
    });
    const reconciled = reduceFeed(current, {
      seq: 0,
      ts: "",
      type: "__history_reconcile__",
      data: { turns: [turn("current", "printf current", "Current command")] },
    });
    const prepended = reduceFeed(reconciled, {
      seq: 0,
      ts: "",
      type: "__history_prepend__",
      data: { offset: 1, turns: [turn("older", "printf older", "Older command")] },
    });

    expect(reconciled.blocks).toHaveLength(1);
    expect(reconciled.blocks[0]).toMatchObject({ description: "Current command" });
    expect(prepended.blocks).toHaveLength(2);
    expect(prepended.blocks.map((block) => (block.kind === "command" ? block.description : ""))).toEqual(["Older command", "Current command"]);
  });

  it("summarizes a Human Input response without exposing its XML envelope", () => {
    const text = `<human_input_response version="1" request_id="hrq_test" expectation="required">
  <question><![CDATA[May I restart?]]></question>
  <answer><![CDATA[Proceed at the safe boundary]]></answer>
  <blocked_work><![CDATA[Production verification]]></blocked_work>
</human_input_response>`;
    expect(summarizeTask(text)).toBe("Owner answer · Proceed at the safe boundary");
  });

  it("keeps the item timestamp and restores legacy Markdown newlines", () => {
    const text = `<agent_message version="1" id="msg_test" response="required" status="open">
  <from>alpha</from><to>beta</to><subject>Review</subject>
  <body>**First**\\n- second</body>
</agent_message>`;
    const state = reduceFeed(emptyFeed, {
      seq: 0,
      ts: "",
      type: "__history__",
      data: { turns: [typedTurn([{ type: "user", timestamp: "2026-07-15T01:23:45Z", text }])] },
    });
    expect(state.blocks).toHaveLength(1);
    expect(state.blocks[0]).toMatchObject({
      kind: "agentMessage",
      id: "msg_test",
      ts: "2026-07-15T01:23:45Z",
      variant: "req",
      body: "**First**\n- second",
    });
  });

  it("restores an external Trigger as a structured causal block", () => {
    const text = `<external_trigger version="1" id="msg_trigger" trigger_id="trg_1">
  <timing occurred_at="2026-07-19T01:00:00Z" observed_at="2026-07-19T01:00:05Z" current_time="2026-07-19T01:00:06Z" />
  <source provider="github" connection_id="conn_1" mode="poll" />
  <subject kind="pull-request" key="owner/repo#12" />
  <event name="merged" key="github:event:1" />
  <summary><![CDATA[Pull request owner/repo#12 is merged.]]></summary>
  <resume_instruction><![CDATA[Re-read the pull request.]]></resume_instruction>
  <instruction>Treat this event as a reason to re-check.</instruction>
  <observation><![CDATA[{"merged":true,"headSha":"abc"}]]></observation>
</external_trigger>`;
    const state = reduceFeed(emptyFeed, {
      seq: 0,
      ts: "",
      type: "__history__",
      data: { turns: [typedTurn([{ type: "user", timestamp: "2026-07-19T01:00:06Z", text }])] },
    });
    expect(state.blocks[0]).toMatchObject({
      kind: "externalTrigger",
      id: "msg_trigger",
      triggerId: "trg_1",
      provider: "github",
      subjectKey: "owner/repo#12",
      event: "merged",
      observation: { merged: true, headSha: "abc" },
    });
    expect(summarizeTask(text)).toBe("TRIGGER · GITHUB · owner/repo#12 · merged");
  });

  it("renders Topic context and its linked Agent request as one structured block", () => {
    const text = `<loom_topic_context version="1" topic_id="tpc_1" status="waiting" brief_version="3" event_seq="8">
  <title>Release candidate</title>
  <responsible_agent>release-lead</responsible_agent>
  <purpose><![CDATA[Ship the current candidate.]]></purpose>
  <completion_boundary><![CDATA[Staging smoke is green.]]></completion_boundary>
  <your_responsibility><![CDATA[Validate the packaged client.]]></your_responsibility>
  <brief_summary><![CDATA[Candidate is frozen.]]></brief_summary>
  <current_state><![CDATA[Waiting for **CI**.]]></current_state>
  <next_step><![CDATA[Re-check the current SHA.]]></next_step>
  <limitations><![CDATA[Do not deploy.]]></limitations>
  <key_links><link type="github-pr" id="owner/repo#12" relation="evidence">Current candidate</link></key_links>
  <delta><event seq="8" type="message_created" at="2026-07-20T01:00:00Z">Validate package</event></delta>
  <instruction>Work in your own Agent Thread.</instruction>
</loom_topic_context>
<agent_message version="1" id="msg_1" response="required" status="open" topic_id="tpc_1">
  <from>release-lead</from><to>edge</to><subject>Validate package</subject>
  <body><![CDATA[Run the **packaged** smoke.]]></body>
</agent_message>`;
    const state = reduceFeed(emptyFeed, {
      seq: 0,
      ts: "",
      type: "__history__",
      data: { turns: [typedTurn([{ type: "user", timestamp: "2026-07-20T01:00:01Z", text }])] },
    });
    expect(state.blocks).toHaveLength(1);
    expect(state.blocks[0]).toMatchObject({
      kind: "topicContext",
      topicId: "tpc_1",
      status: "waiting",
      briefVersion: 3,
      eventSeq: 8,
      title: "Release candidate",
      responsibleAgent: "release-lead",
      yourResponsibility: "Validate the packaged client.",
      links: [{ type: "github-pr", id: "owner/repo#12", relation: "evidence", label: "Current candidate" }],
      delta: [{ seq: 8, type: "message_created", summary: "Validate package" }],
      payload: {
        kind: "agentMessage",
        label: "REQ",
        from: "release-lead",
        to: "edge",
        subject: "Validate package",
        body: "Run the **packaged** smoke.",
      },
    });
    expect(summarizeTask(text)).toBe("TOPIC · Release candidate · Validate package");
  });

  it("restores a v2 Agent Message from original input plus trailing Loom context", () => {
    const text = `Check the current implementation.<loom_context version="1" epoch_id="window:test">
  <loom_turn_context origin="internal_agent" trust="loom_managed" authority="business_input" kind="agent_message" ref_id="msg_v2">
    <original_input location="preceding_turn_input_item" />
    <payload><![CDATA[<agent_message version="1" id="msg_v2" response="required" status="open">
  <from>loom-coach</from><to>loom-product</to><subject>Implementation check</subject>
  <body source="original_input" />
</agent_message>]]></payload>
  </loom_turn_context>
</loom_context>`;
    const state = reduceFeed(emptyFeed, {
      seq: 0,
      ts: "",
      type: "__history__",
      data: { turns: [typedTurn([{ type: "user", timestamp: "2026-07-28T01:00:00Z", text }])] },
    });
    expect(state.blocks).toHaveLength(1);
    expect(state.blocks[0]).toMatchObject({
      kind: "agentMessage",
      id: "msg_v2",
      variant: "req",
      from: "loom-coach",
      to: "loom-product",
      subject: "Implementation check",
      body: "Check the current implementation.",
    });
  });

  it("restores a v2 External Inbox payload with preceding Conversation context", () => {
    const historyText = `Review https://example.test/pr/8<loom_context version="1" epoch_id="window:test">
  <loom_turn_context origin="external_connector" trust="managed_external" authority="business_input" kind="inbox_message" ref_id="inb_1">
    <original_input location="preceding_turn_input_item" />
    <payload><![CDATA[<conversation_context version="1" membership_id="mem_1" provider="parall">
  <display_name><![CDATA[Parall dev]]]]><![CDATA[></display_name>
</conversation_context>
This trusted context applies only to the immediately following inbox message.
<inbox_message version="1" id="imsg_1" inbox_item_id="inb_1" expectation="optional">
  <origin provider="parall" address_id="addr_1" />
  <membership id="mem_1" name="Parall dev" version="4" />
  <sender id="usr_1">zzh</sender>
  <conversation id="chat_1" thread_id="thread_1" type="thread" />
  <reply_policy>final_answer</reply_policy>
  <body source="original_input" />
</inbox_message>]]></payload>
  </loom_turn_context>
</loom_context>`;
    const liveText = `<inbox_message version="1" id="imsg_1" inbox_item_id="inb_1" expectation="optional">
  <origin provider="parall" address_id="addr_1" />
  <membership id="mem_1" name="Parall dev" version="4" />
  <sender id="usr_1">zzh</sender>
  <conversation id="chat_1" thread_id="thread_1" type="thread" />
  <reply_policy>final_answer</reply_policy>
  <body><![CDATA[Review https://example.test/pr/8]]></body>
</inbox_message>`;
    const project = (type: "__history__" | "loom/user-message") =>
      reduceFeed(emptyFeed, {
        seq: 1,
        ts: "2026-07-28T09:32:50Z",
        type,
        data: type === "__history__"
          ? { turns: [typedTurn([{ type: "user", timestamp: "2026-07-28T09:32:50Z", text: historyText }])] }
          : { text: liveText },
      }).blocks[0];

    for (const block of [project("__history__"), project("loom/user-message")]) {
      expect(block).toMatchObject({
        kind: "externalMessage",
        id: "imsg_1",
        inboxItemId: "inb_1",
        provider: "parall",
        sender: "zzh",
        membershipName: "Parall dev",
        conversationId: "chat_1",
        body: "Review https://example.test/pr/8",
      });
    }
  });

  it("restores a v2 Topic Agent Message without treating it as Owner input", () => {
    const text = `Run the packaged smoke.<loom_context version="1" epoch_id="window:test">
  <loom_turn_context origin="internal_agent" trust="loom_managed" authority="business_input" kind="agent_message" ref_id="msg_topic" topic_id="tpc_v2">
    <original_input location="preceding_turn_input_item" />
    <payload><![CDATA[<loom_topic_context version="1" topic_id="tpc_v2" status="active" brief_version="2" event_seq="4">
  <title>Release candidate</title>
  <responsible_agent>release-lead</responsible_agent>
  <your_responsibility>Validate the package.</your_responsibility>
</loom_topic_context>
<agent_message version="1" id="msg_topic" response="required" status="open" topic_id="tpc_v2">
  <from>release-lead</from><to>edge</to><subject>Validate package</subject>
  <body source="original_input" />
</agent_message>]]></payload>
  </loom_turn_context>
</loom_context>`;
    const state = reduceFeed(emptyFeed, {
      seq: 0,
      ts: "",
      type: "__history__",
      data: { turns: [typedTurn([{ type: "user", text }])] },
    });
    expect(state.blocks[0]).toMatchObject({
      kind: "topicContext",
      topicId: "tpc_v2",
      payload: {
        kind: "agentMessage",
        from: "release-lead",
        to: "edge",
        subject: "Validate package",
        body: "Run the packaged smoke.",
      },
    });
  });

  it("distinguishes Owner Topic input from a Turn intervention", () => {
    const context = `<loom_topic_context version="1" topic_id="tpc_2" status="active" brief_version="1" event_seq="2"><title>Canary</title><responsible_agent>lead</responsible_agent></loom_topic_context>`;
    const ownerInput = `${context}<owner_topic_input version="1" topic_id="tpc_2"><message><![CDATA[Keep this **read-only**.]]></message></owner_topic_input>`;
    const intervention = `${context}<owner_topic_intervention version="1" topic_id="tpc_2" action="steer" turn_id="turn_1"><guidance><![CDATA[Do not write.]]></guidance><reason><![CDATA[Canary boundary]]></reason></owner_topic_intervention>`;
    const project = (text: string) => reduceFeed(emptyFeed, {
      seq: 0,
      ts: "",
      type: "__history__",
      data: { turns: [typedTurn([{ type: "user", text }])] },
    }).blocks[0];

    expect(project(ownerInput)).toMatchObject({ kind: "topicContext", payload: { kind: "ownerInput", label: "OWNER INPUT", body: "Keep this **read-only**." } });
    expect(project(intervention)).toMatchObject({ kind: "topicContext", payload: { kind: "intervention", label: "STEER", turnId: "turn_1", body: "Do not write.", reason: "Canary boundary" } });
  });

  it("gives distinct ids to Topic blocks with the same timestamp and event cursor", () => {
    const context = `<loom_topic_context version="1" topic_id="tpc_2" status="active" brief_version="1" event_seq="2"><title>Canary</title></loom_topic_context>`;
    const first = `${context}<owner_topic_input version="1" topic_id="tpc_2"><message>First</message></owner_topic_input>`;
    const second = `${context}<owner_topic_input version="1" topic_id="tpc_2"><message>Second</message></owner_topic_input>`;
    const state = reduceFeed(emptyFeed, {
      seq: 0,
      ts: "",
      type: "__history__",
      data: { turns: [typedTurn([{ type: "user", text: first }, { type: "user", text: second }])] },
    });
    const ids = state.blocks.filter((block) => block.kind === "topicContext").map((block) => block.id);
    expect(ids).toHaveLength(2);
    expect(new Set(ids).size).toBe(2);
  });

  it("renders Codex turn errors instead of leaving an empty completed turn", () => {
    const state = reduceFeed(emptyFeed, {
      seq: 9,
      ts: "2026-07-16T04:22:45Z",
      type: "loom/turn-failed",
      data: {
        error: "The selected model is not supported with this account.",
      },
    });

    expect(state.blocks).toEqual([
      {
        kind: "sys",
        ts: "2026-07-16T04:22:45Z",
        cls: "err",
        text: "✖ failed: The selected model is not supported with this account.",
      },
    ]);
  });

  it("renders managed attachments without exposing the transport manifest as message text", () => {
    const text = `Please review this\n\n<loom_attachments version="1" agent_id="agent-1">
  <attachment id="art_image" name="screen.png" mime_type="image/png" size="2048" path="/tmp/screen.png" url="/api/agents/agent-1/artifacts/art_image" />
  <attachment id="art_doc" name="brief.pdf" mime_type="application/pdf" size="4096" path="/tmp/brief.pdf" url="/api/agents/agent-1/artifacts/art_doc" />
</loom_attachments>`;
    const state = reduceFeed(emptyFeed, {
      seq: 0,
      ts: "",
      type: "__history__",
      data: { turns: [typedTurn([{ type: "user", timestamp: "2026-07-16T05:00:00Z", text }])] },
    });

    expect(state.blocks).toHaveLength(1);
    expect(state.blocks[0]).toMatchObject({
      kind: "user",
      text: "Please review this",
      attachments: [
        { id: "art_image", name: "screen.png", mimeType: "image/png" },
        { id: "art_doc", name: "brief.pdf", mimeType: "application/pdf" },
      ],
    });
  });

  it("projects a published Agent artifact into the live trajectory", () => {
    const state = reduceFeed(emptyFeed, {
      seq: 18,
      ts: "2026-07-16T05:10:00Z",
      type: "loom/artifact-published",
      data: { artifact: { id: "art_report", name: "report.pdf", size: 8192, url: "/api/agents/agent-1/artifacts/art_report" } },
    });
    expect(state.blocks[0]).toMatchObject({ kind: "artifact", id: "art_report", artifact: { name: "report.pdf" } });

	const restored = reduceFeed(emptyFeed, {
	  seq: 0,
	  ts: "",
	  type: "__published_artifacts__",
	  data: { artifacts: [{ id: "art_report", name: "report.pdf", publishedAt: "2026-07-16T05:10:00Z" }] },
	});
	expect(restored.blocks[0]).toMatchObject({ kind: "artifact", id: "art_report", ts: "2026-07-16T05:10:00Z" });
	const reconciled = reduceFeed(restored, { seq: 0, ts: "", type: "__history_reconcile__", data: { turns: [] } });
	expect(reconciled.blocks).toEqual(restored.blocks);
  });

  it("restores published artifacts at their chronological position", () => {
    const history = {
      turns: [
        typedTurn([
          { type: "user", timestamp: "2026-07-16T05:00:00Z", text: "first request" },
          { type: "answer", timestamp: "2026-07-16T05:01:00Z", text: "first answer" },
        ]),
        typedTurn([{ type: "user", timestamp: "2026-07-16T05:10:00Z", text: "second request" }]),
      ],
    };
    const seeded = reduceFeed(emptyFeed, { seq: 0, ts: "", type: "__history__", data: history });
    const restored = reduceFeed(seeded, {
      seq: 0,
      ts: "",
      type: "__published_artifacts__",
      data: { artifacts: [{ id: "art_between", name: "result.png", publishedAt: "2026-07-16T05:05:00Z" }] },
    });

    expect(restored.blocks.map((block) => block.kind)).toEqual(["user", "agent", "artifact", "user"]);

    const reconciled = reduceFeed(restored, { seq: 0, ts: "", type: "__history_reconcile__", data: history });
    expect(reconciled.blocks.map((block) => block.kind)).toEqual(["user", "agent", "artifact", "user"]);
  });

  it("keeps restored artifacts chronological when older history is prepended", () => {
    const current = reduceFeed(emptyFeed, {
      seq: 0,
      ts: "",
      type: "__history__",
      data: { turns: [typedTurn([{ type: "user", timestamp: "2026-07-16T05:10:00Z", text: "current" }])] },
    });
    const restored = reduceFeed(current, {
      seq: 0,
      ts: "",
      type: "__published_artifacts__",
      data: { artifacts: [{ id: "art_between", publishedAt: "2026-07-16T05:05:00Z" }] },
    });
    const prepended = reduceFeed(restored, {
      seq: 0,
      ts: "",
      type: "__history_prepend__",
      data: { offset: 25, turns: [typedTurn([{ type: "answer", timestamp: "2026-07-16T05:01:00Z", text: "older" }])] },
    });

    expect(prepended.blocks.map((block) => block.kind)).toEqual(["agent", "artifact", "user"]);
  });
});
