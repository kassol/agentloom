import type { Topic, TopicBrief, TopicEvent } from "./types";

export function topicBriefTimeline(topic: Pick<Topic, "briefHistory" | "currentBrief">): TopicBrief[] {
  const byVersion = new Map<number, TopicBrief>();
  for (const brief of topic.briefHistory || []) byVersion.set(brief.version, brief);
  byVersion.set(topic.currentBrief.version, topic.currentBrief);
  return [...byVersion.values()].sort((left, right) => right.version - left.version);
}

export function cleanTopicDisplayText(value: string): string {
  return value.replaceAll("\uFFFD", "").replace(/\s+/g, " ").trim();
}

export function topicAuditSummary(event: TopicEvent): string {
  switch (event.type) {
    case "turn_started":
      return `${event.agent || "Agent"} started Topic work`;
    case "turn_completed":
      return `${event.agent || "Agent"} completed Topic work`;
    case "turn_interrupted":
      return `${event.agent || "Agent"} work was interrupted`;
    case "trigger_created":
      return "A Trigger was attached to this Topic";
    default:
      return cleanTopicDisplayText(event.summary);
  }
}
