import { useQuery } from "@tanstack/react-query";
import { CalendarDays, ChevronLeft, ChevronRight } from "lucide-react";
import { useEffect, useMemo, useState, type PointerEvent as ReactPointerEvent } from "react";
import { Button } from "./components/ui/button";
import { Input } from "./components/ui/input";
import { activityIntensity, shiftCalendarDate, type DailyActivityOverview, type DailyAgentActivity } from "./daily-activity";
import { api } from "./types";
import { browserTimezone, formatOptionalUsageMetric, todayDate } from "./usage";

const AGENT_COLUMN = 176;
const SUMMARY_COLUMN = 136;
const BUCKET_COLUMN = 14;

type Selection = { agent: DailyAgentActivity; bucketIndex: number };
type HoverDetail = { left: number; top: number; below: boolean; title: string; lines: string[] };

export function DailyActivityTimeline({ onSelectAgent }: { onSelectAgent: (id: string) => void }) {
  const [date, setDate] = useState(todayDate);
  const [selection, setSelection] = useState<Selection | null>(null);
  const [hover, setHover] = useState<HoverDetail | null>(null);
  const timezone = useMemo(browserTimezone, []);
  const query = useQuery<DailyActivityOverview>({
    queryKey: ["daily-activity", date, timezone],
    queryFn: async () => (await api("GET", `/api/activity/daily?${new URLSearchParams({ date, tz: timezone, bucket: "30" })}`)).activity,
    refetchInterval: (current) => current.state.data?.live ? 30_000 : false,
  });
  const activity = query.data;
  const today = todayDate();

  useEffect(() => {
    setSelection(null);
    setHover(null);
  }, [date]);

  if (query.isPending) {
    return <section className="mt-7 border-y border-border py-12 text-center text-[11px] text-muted-foreground">Loading daily activity…</section>;
  }
  if (query.isError || !activity) {
    return <section className="mt-7 border-y border-border py-10 text-center text-[11px] text-destructive">Daily activity is unavailable.</section>;
  }

  const gridTemplateColumns = `${AGENT_COLUMN}px repeat(${activity.buckets.length}, minmax(${BUCKET_COLUMN}px, 1fr)) ${SUMMARY_COLUMN}px`;
  const gridMinWidth = AGENT_COLUMN + activity.buckets.length * BUCKET_COLUMN + SUMMARY_COLUMN;
  const maxBucketTokens = Math.max(0, ...activity.buckets.map((bucket) => bucket.usage.totalTokens));
  const labelEvery = Math.max(1, Math.round(120 / activity.bucketMinutes));

  const showHover = (event: ReactPointerEvent<HTMLElement>, title: string, lines: string[]) => {
    if (event.pointerType === "touch") return;
    const rect = event.currentTarget.getBoundingClientRect();
    const width = 236;
    setHover({
      left: Math.max(8, Math.min(rect.left + rect.width / 2 - width / 2, window.innerWidth - width - 8)),
      top: rect.top > 150 ? rect.top - 8 : rect.bottom + 8,
      below: rect.top <= 150,
      title,
      lines,
    });
  };

  return (
    <section className="mt-7 min-w-0" aria-labelledby="daily-activity-title">
      <div className="mb-3 flex flex-col items-stretch gap-3 sm:flex-row sm:flex-wrap sm:items-end">
        <div className="min-w-0 sm:flex-1">
          <div className="flex items-center gap-2">
            <CalendarDays className="size-3.5 text-primary" />
            <h2 id="daily-activity-title" className="text-[12px] font-semibold uppercase text-muted-foreground">Daily activity</h2>
            {activity.live ? <span className="font-mono text-[8.5px] uppercase text-success">live</span> : null}
          </div>
          <p className="mt-1 text-[10.5px] text-muted-foreground">
            {activity.activeAgents} active {activity.activeAgents === 1 ? "Agent" : "Agents"} · {formatDuration(activity.executingSeconds)} execution · {formatTokens(activity.usage.totalTokens)} tokens · {formatTurnCount(activity.turnCount)}
          </p>
        </div>
        <div className="flex w-full items-center gap-1 sm:w-auto">
          <Button type="button" variant="outline" size="icon-sm" onClick={() => setDate(shiftCalendarDate(date, -1))} title="Previous day" aria-label="Previous day"><ChevronLeft className="size-3.5" /></Button>
          <Input type="date" value={date} max={today} onChange={(event) => event.target.value && setDate(event.target.value)} aria-label="Activity date" className="h-8 min-w-0 flex-1 px-2 font-mono text-[10px] sm:w-[138px] sm:flex-none" />
          <Button type="button" variant="outline" size="icon-sm" disabled={date >= today} onClick={() => setDate(shiftCalendarDate(date, 1))} title="Next day" aria-label="Next day"><ChevronRight className="size-3.5" /></Button>
          <Button type="button" variant="ghost" size="sm" disabled={date === today} onClick={() => setDate(today)} className="h-8 px-2 text-[10px]">Today</Button>
        </div>
      </div>

      <div className="overflow-x-auto border-y border-border" data-daily-activity>
        <div style={{ minWidth: gridMinWidth }}>
          <div className="grid h-7 items-center border-b border-border/70 bg-muted/25" style={{ gridTemplateColumns }}>
            <div className="sticky left-0 z-10 h-full border-r border-border bg-background px-2.5 py-1.5 font-mono text-[8.5px] uppercase text-muted-foreground">{formatDateHeading(activity.date)}</div>
            {activity.buckets.map((bucket, index) => (
              <div key={bucket.startedAt} className={`min-w-0 border-l ${index % labelEvery === 0 ? "border-border/70" : "border-transparent"}`}>
                {index % labelEvery === 0 ? <span className="block -translate-x-1/2 whitespace-nowrap font-mono text-[8px] text-muted-foreground">{formatBucketTime(bucket.startedAt, timezone)}</span> : null}
              </div>
            ))}
            <div className="h-full border-l border-border px-2 py-1.5 text-right font-mono text-[8.5px] uppercase text-muted-foreground">day total</div>
          </div>

          <div className="grid h-14 items-end border-b border-border bg-muted/10" style={{ gridTemplateColumns }}>
            <div className="sticky left-0 z-10 flex h-full items-center border-r border-border bg-background px-2.5">
              <div><div className="text-[10.5px] font-semibold">Token volume</div><div className="mt-0.5 font-mono text-[8.5px] text-muted-foreground">same 30m axis</div></div>
            </div>
            {activity.buckets.map((bucket, index) => {
              const height = maxBucketTokens > 0 && bucket.usage.totalTokens > 0 ? Math.max(2, bucket.usage.totalTokens / maxBucketTokens * 38) : 0;
              return (
                <button
                  key={bucket.startedAt}
                  type="button"
                  className={`relative flex h-full min-w-0 items-end justify-center border-l outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring/40 ${index % labelEvery === 0 ? "border-border/60" : "border-border/20"}`}
                  onPointerEnter={(event) => showHover(event, `${formatBucketRange(bucket.startedAt, bucket.endedAt, timezone)} · team`, [
                    `${formatTokens(bucket.usage.totalTokens)} tokens · ${formatOptionalUsageMetric(bucket.usage, "calls", formatTokens)} calls`,
                    `${bucket.activeAgents} active Agents · ${formatDuration(bucket.executingSeconds)} execution`,
                  ])}
                  onPointerLeave={() => setHover(null)}
                  aria-label={`${formatBucketRange(bucket.startedAt, bucket.endedAt, timezone)}: ${formatTokens(bucket.usage.totalTokens)} tokens, ${bucket.activeAgents} active Agents`}
                >
                  <span className="w-[70%] bg-[var(--loom-blue)]/65" style={{ height }} />
                </button>
              );
            })}
            <div className="flex h-full items-center justify-end border-l border-border px-2 font-mono text-[10px] font-semibold">{formatTokens(activity.usage.totalTokens)}</div>
          </div>

          {activity.agents.map((agent) => (
            <div key={agent.agentId} className="grid h-9 items-stretch border-b border-border/60 last:border-b-0" style={{ gridTemplateColumns }}>
              <button type="button" onClick={() => onSelectAgent(agent.agentId)} className="sticky left-0 z-10 flex min-w-0 items-center gap-2 border-r border-border bg-background px-2.5 text-left hover:bg-muted/35">
                <span className={`size-1.5 shrink-0 rounded-full ${agent.status === "running" ? "bg-success" : "bg-muted-foreground/35"}`} />
                <span className="truncate text-[10.5px] font-semibold">{agent.agentName}</span>
              </button>
              {agent.buckets.map((bucket, index) => {
                const timelineBucket = activity.buckets[index];
                const intensity = activityIntensity(bucket.executingSeconds);
                const active = bucket.executingSeconds > 0 || bucket.turnCount > 0 || bucket.usage.totalTokens > 0;
                const selected = selection?.agent.agentId === agent.agentId && selection.bucketIndex === index;
                return (
                  <button
                    key={`${agent.agentId}:${timelineBucket.startedAt}`}
                    type="button"
                    disabled={timelineBucket.observedSeconds === 0}
                    onClick={() => active && setSelection({ agent, bucketIndex: index })}
                    onPointerEnter={(event) => showHover(event, `${agent.agentName} · ${formatBucketRange(timelineBucket.startedAt, timelineBucket.endedAt, timezone)}`, [
                      `${formatDuration(bucket.executingSeconds)} execution · ${formatTurnCount(bucket.turnCount)}`,
                      `${formatTokens(bucket.usage.totalTokens)} tokens · ${formatOptionalUsageMetric(bucket.usage, "calls", formatTokens)} calls`,
                    ])}
                    onPointerLeave={() => setHover(null)}
                    aria-label={`${agent.agentName}, ${formatBucketRange(timelineBucket.startedAt, timelineBucket.endedAt, timezone)}: ${formatDuration(bucket.executingSeconds)} execution, ${formatTokens(bucket.usage.totalTokens)} tokens`}
                    aria-pressed={selected}
                    className={`relative min-w-0 border-l outline-none focus-visible:z-10 focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring/45 disabled:cursor-default ${index % labelEvery === 0 ? "border-border/60" : "border-border/20"} ${selected ? "ring-2 ring-inset ring-foreground/70" : ""}`}
                    style={{ background: cellBackground(intensity, timelineBucket.observedSeconds, activity.bucketMinutes) }}
                  >
                    {bucket.usage.totalTokens > 0 && intensity === 0 ? <span className="absolute left-1/2 top-1/2 size-1 -translate-x-1/2 -translate-y-1/2 rounded-full bg-[var(--loom-blue)]" /> : null}
                    {timelineBucket.observedSeconds > 0 && timelineBucket.observedSeconds < activity.bucketMinutes * 60 ? <span className="absolute inset-x-0 bottom-0 h-px bg-foreground/35" /> : null}
                  </button>
                );
              })}
              <button type="button" onClick={() => onSelectAgent(agent.agentId)} className="flex min-w-0 items-center justify-end border-l border-border px-2 text-right hover:bg-muted/35">
                <span className="truncate font-mono text-[8.5px] text-muted-foreground"><span className="font-semibold text-foreground">{formatDuration(agent.executingSeconds)}</span> · {formatTokens(agent.usage.totalTokens)}</span>
              </button>
            </div>
          ))}
          {activity.agents.length === 0 ? <div className="py-9 text-center text-[10.5px] text-muted-foreground">No Turn or token activity was recorded for this day.</div> : null}
        </div>
      </div>

      <div className="mt-2 flex flex-wrap items-center justify-between gap-2 font-mono text-[8.5px] text-muted-foreground">
        <div className="flex items-center gap-3"><Legend color="activity-low" label="&lt;10m" /><Legend color="activity-medium" label="10–20m" /><Legend color="activity-high" label="20–30m" /><Legend color="token-dot" label="token event only" /></div>
        <span>{activity.inactiveAgents} inactive · {activity.trackedAgents}/{activity.totalAgents} tracked · {activity.timezone}</span>
      </div>

      {selection ? <SelectedBucket selection={selection} activity={activity} timezone={timezone} onOpenAgent={() => onSelectAgent(selection.agent.agentId)} /> : null}
      <details className="mt-3 border-t border-border pt-2 text-[9px] text-muted-foreground">
        <summary className="cursor-pointer select-none font-semibold uppercase text-foreground">How to read this</summary>
        <div className="mt-2 space-y-1.5 leading-relaxed">{activity.dataQuality.limitations.map((limitation) => <p key={limitation}>{limitation}</p>)}</div>
      </details>
      {hover ? <HoverCard detail={hover} /> : null}
    </section>
  );
}

function SelectedBucket({ selection, activity, timezone, onOpenAgent }: { selection: Selection; activity: DailyActivityOverview; timezone: string; onOpenAgent: () => void }) {
  const bucket = selection.agent.buckets[selection.bucketIndex];
  const timelineBucket = activity.buckets[selection.bucketIndex];
  return (
    <div className="mt-3 flex flex-wrap items-center gap-x-5 gap-y-2 border-y border-border px-2 py-3">
      <div className="min-w-[180px] flex-1"><div className="text-[11px] font-semibold">{selection.agent.agentName}</div><div className="mt-0.5 font-mono text-[9px] text-muted-foreground">{formatBucketRange(timelineBucket.startedAt, timelineBucket.endedAt, timezone)}</div></div>
      <MiniMetric label="Execution" value={formatDuration(bucket.executingSeconds)} />
      <MiniMetric label="Turns" value={String(bucket.turnCount)} />
      <MiniMetric label="Tokens" value={formatTokens(bucket.usage.totalTokens)} />
      <MiniMetric label="Calls" value={formatOptionalUsageMetric(bucket.usage, "calls", formatTokens)} />
      <Button type="button" variant="outline" size="sm" onClick={onOpenAgent} className="h-7 text-[9.5px]">Open Agent</Button>
    </div>
  );
}

function MiniMetric({ label, value }: { label: string; value: string }) {
  return <div className="min-w-[64px]"><div className="font-mono text-[8px] uppercase text-muted-foreground">{label}</div><div className="mt-0.5 font-mono text-[10px] font-semibold">{value}</div></div>;
}

function HoverCard({ detail }: { detail: HoverDetail }) {
  return <div className="pointer-events-none fixed z-50 w-[236px] border border-border bg-popover px-3 py-2 text-popover-foreground shadow-float" style={{ left: detail.left, top: detail.top, transform: detail.below ? undefined : "translateY(-100%)" }}><div className="truncate text-[10.5px] font-semibold">{detail.title}</div>{detail.lines.map((line) => <div key={line} className="mt-1 font-mono text-[8.5px] text-muted-foreground">{line}</div>)}</div>;
}

function Legend({ color, label }: { color: "activity-low" | "activity-medium" | "activity-high" | "token-dot"; label: string }) {
  const style = color === "token-dot" ? undefined : { background: cellBackground(color === "activity-low" ? 1 : color === "activity-medium" ? 2 : 3, 1800, 30) };
  return <span className="flex items-center gap-1.5"><span className={color === "token-dot" ? "size-1.5 rounded-full bg-[var(--loom-blue)]" : "size-2.5 border border-border/50"} style={style} />{label}</span>;
}

function cellBackground(intensity: number, observedSeconds: number, bucketMinutes: number) {
  if (observedSeconds === 0) return "color-mix(in oklch, var(--muted) 40%, var(--background))";
  if (intensity === 1) return "color-mix(in oklch, var(--loom-teal) 20%, var(--background))";
  if (intensity === 2) return "color-mix(in oklch, var(--loom-teal) 38%, var(--background))";
  if (intensity === 3) return "color-mix(in oklch, var(--loom-teal) 62%, var(--background))";
  if (observedSeconds < bucketMinutes * 60) return "color-mix(in oklch, var(--muted) 25%, var(--background))";
  return "var(--background)";
}

function formatBucketTime(value: string, timezone: string) {
  return new Date(value).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZone: timezone });
}

function formatBucketRange(start: string, end: string, timezone: string) {
  return `${formatBucketTime(start, timezone)}–${formatBucketTime(end, timezone)}`;
}

function formatDateHeading(value: string) {
  const [year, month, day] = value.split("-").map(Number);
  return new Date(year, month - 1, day, 12).toLocaleDateString([], { month: "short", day: "numeric", weekday: "short" });
}

function formatDuration(seconds: number) {
  const minutes = Math.max(0, Math.round(seconds / 60));
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const remainder = minutes % 60;
  return remainder ? `${hours}h ${remainder}m` : `${hours}h`;
}

function formatTokens(value: number) {
  const amount = Number.isFinite(value) ? Math.max(0, value) : 0;
  if (amount >= 1_000_000_000) return `${trimNumber(amount / 1_000_000_000)}B`;
  if (amount >= 1_000_000) return `${trimNumber(amount / 1_000_000)}M`;
  if (amount >= 1_000) return `${trimNumber(amount / 1_000)}K`;
  return Math.round(amount).toLocaleString();
}

function formatTurnCount(value: number) {
  return `${value} ${value === 1 ? "Turn" : "Turns"}`;
}

function trimNumber(value: number) {
  return value >= 100 ? value.toFixed(0) : value >= 10 ? value.toFixed(1) : value.toFixed(2);
}
