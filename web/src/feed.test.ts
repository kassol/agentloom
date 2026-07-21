import { describe, expect, it } from "vitest";
import { emptyFeed, reduceFeed, summarizeTask } from "./feed";

describe("rollout history projection", () => {
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
      data: { turns: [{ items: [{ type: "user", timestamp: "2026-07-15T01:23:45Z", text }] }] },
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
      data: { turns: [{ items: [{ type: "user", timestamp: "2026-07-19T01:00:06Z", text }] }] },
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
      data: { turns: [{ items: [{ type: "user", timestamp: "2026-07-20T01:00:01Z", text }] }] },
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

  it("distinguishes Owner Topic input from a Turn intervention", () => {
    const context = `<loom_topic_context version="1" topic_id="tpc_2" status="active" brief_version="1" event_seq="2"><title>Canary</title><responsible_agent>lead</responsible_agent></loom_topic_context>`;
    const ownerInput = `${context}<owner_topic_input version="1" topic_id="tpc_2"><message><![CDATA[Keep this **read-only**.]]></message></owner_topic_input>`;
    const intervention = `${context}<owner_topic_intervention version="1" topic_id="tpc_2" action="steer" turn_id="turn_1"><guidance><![CDATA[Do not write.]]></guidance><reason><![CDATA[Canary boundary]]></reason></owner_topic_intervention>`;
    const project = (text: string) => reduceFeed(emptyFeed, {
      seq: 0,
      ts: "",
      type: "__history__",
      data: { turns: [{ items: [{ type: "user", text }] }] },
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
      data: { turns: [{ items: [{ type: "user", text: first }, { type: "user", text: second }] }] },
    });
    const ids = state.blocks.filter((block) => block.kind === "topicContext").map((block) => block.id);
    expect(ids).toHaveLength(2);
    expect(new Set(ids).size).toBe(2);
  });

  it("renders Codex turn errors instead of leaving an empty completed turn", () => {
    const state = reduceFeed(emptyFeed, {
      seq: 9,
      ts: "2026-07-16T04:22:45Z",
      type: "error",
      data: {
        error: {
          message: "The selected model is not supported with this account.",
        },
      },
    });

    expect(state.blocks).toEqual([
      {
        kind: "sys",
        ts: "2026-07-16T04:22:45Z",
        cls: "err",
        text: "The selected model is not supported with this account.",
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
      data: { turns: [{ items: [{ type: "user", timestamp: "2026-07-16T05:00:00Z", text, attachments: [{ path: "/tmp/screen.png", mimeType: "image/png" }] }] }] },
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
});
