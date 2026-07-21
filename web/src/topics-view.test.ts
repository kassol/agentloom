import { describe, expect, it } from "vitest";
import type { TopicBrief, TopicEvent } from "./types";
import { cleanTopicDisplayText, topicAuditSummary, topicBriefTimeline } from "./topics-view";

function brief(version: number, summary: string): TopicBrief {
  return { version, summary, updatedBy: "lead", updatedAt: `2026-07-20T00:0${version}:00Z` };
}

describe("Topic history projection", () => {
  it("keeps every version once and places the current Brief first", () => {
    const timeline = topicBriefTimeline({ briefHistory: [brief(1, "created"), brief(2, "waiting")], currentBrief: brief(3, "ready") });
    expect(timeline.map((entry) => entry.version)).toEqual([3, 2, 1]);
  });

  it("keeps audit lifecycle events useful without exposing truncated XML", () => {
    const event: TopicEvent = { seq: 4, type: "turn_started", actor: "system", agent: "edge", summary: "<loom_topic_context>验��...", createdAt: "2026-07-20T00:00:00Z" };
    expect(topicAuditSummary(event)).toBe("edge started Topic work");
    expect(cleanTopicDisplayText("staging 验��...")).toBe("staging 验...");
  });
});
