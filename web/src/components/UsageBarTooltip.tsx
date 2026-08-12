import type { UsageDay } from "../types";
import { formatOptionalUsageMetric } from "../usage";

export function UsageBarTooltip({ day, align = "center" }: { day: UsageDay; align?: "start" | "center" | "end" }) {
  const position = align === "start" ? "left-0" : align === "end" ? "right-0" : "left-1/2 -translate-x-1/2";
  return (
    <div
      role="tooltip"
      data-usage-tooltip
      className={`pointer-events-none absolute top-1 z-20 hidden w-40 border border-border bg-popover px-2.5 py-2 text-popover-foreground shadow-float group-hover:block group-focus:block ${position}`}
    >
      <div className="border-b border-border pb-1 font-mono text-[9px] text-muted-foreground">{day.date}</div>
      <div className="mt-1 flex items-baseline justify-between gap-3">
        <span className="text-[9px] font-semibold uppercase text-muted-foreground">Total</span>
        <span className="font-mono text-[12px] font-semibold">{formatOptionalUsageMetric(day.usage, "totalTokens", exactNumber)}</span>
      </div>
      <dl className="mt-1 grid grid-cols-[1fr_auto] gap-x-3 gap-y-0.5 font-mono text-[9px]">
        <dt className="text-muted-foreground">Input</dt><dd>{formatOptionalUsageMetric(day.usage, "inputTokens", exactNumber)}</dd>
        <dt className="text-muted-foreground">Cached</dt><dd>{formatOptionalUsageMetric(day.usage, "cachedInputTokens", exactNumber)}</dd>
        <dt className="text-muted-foreground">Output</dt><dd>{formatOptionalUsageMetric(day.usage, "outputTokens", exactNumber)}</dd>
        <dt className="text-muted-foreground">Calls</dt><dd>{formatOptionalUsageMetric(day.usage, "calls", exactNumber)}</dd>
      </dl>
    </div>
  );
}

export function usageDayLabel(day: UsageDay) {
  return `${day.date}: ${formatOptionalUsageMetric(day.usage, "totalTokens", exactNumber)} total tokens, ${formatOptionalUsageMetric(day.usage, "inputTokens", exactNumber)} input, ${formatOptionalUsageMetric(day.usage, "cachedInputTokens", exactNumber)} cached, ${formatOptionalUsageMetric(day.usage, "outputTokens", exactNumber)} output, ${formatOptionalUsageMetric(day.usage, "calls", exactNumber)} calls`;
}

function exactNumber(value: number) {
  return Math.max(0, Number.isFinite(value) ? Math.round(value) : 0).toLocaleString();
}
