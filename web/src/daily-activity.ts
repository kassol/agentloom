import type { TokenUsage } from "./types";

export type DailyActivityAgentBucket = {
  executingSeconds: number;
  turnCount: number;
  usage: TokenUsage;
};

export type DailyActivityBucket = DailyActivityAgentBucket & {
  startedAt: string;
  endedAt: string;
  observedSeconds: number;
  activeAgents: number;
};

export type DailyAgentActivity = {
  agentId: string;
  agentName: string;
  status: string;
  executingSeconds: number;
  turnCount: number;
  usage: TokenUsage;
  firstActiveAt?: string;
  lastActiveAt?: string;
  buckets: DailyActivityAgentBucket[];
};

export type DailyActivityOverview = {
  date: string;
  timezone: string;
  generatedAt: string;
  live: boolean;
  bucketMinutes: number;
  activeAgents: number;
  inactiveAgents: number;
  trackedAgents: number;
  totalAgents: number;
  executingSeconds: number;
  turnCount: number;
  usage: TokenUsage;
  buckets: DailyActivityBucket[];
  agents: DailyAgentActivity[];
  dataQuality: {
    activityBasis: string;
    tokenBasis: string;
    limitations: string[];
  };
};

export function shiftCalendarDate(value: string, days: number) {
  const [year, month, day] = value.split("-").map(Number);
  const date = new Date(year, month - 1, day, 12, 0, 0, 0);
  date.setDate(date.getDate() + days);
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
}

export function activityIntensity(executingSeconds: number) {
  const minutes = executingSeconds / 60;
  if (minutes <= 0) return 0;
  if (minutes < 10) return 1;
  if (minutes < 20) return 2;
  return 3;
}
