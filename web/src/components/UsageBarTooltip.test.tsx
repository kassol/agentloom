import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { UsageBarTooltip } from "./UsageBarTooltip";

describe("UsageBarTooltip", () => {
  it("shows exact token and call values", () => {
    render(<UsageBarTooltip day={{
      date: "2026-07-15",
      usage: {
        inputTokens: 10_000,
        cachedInputTokens: 9_000,
        outputTokens: 2_345,
        reasoningOutputTokens: 345,
        totalTokens: 12_345,
        calls: 3,
      },
    }} />);
    expect(screen.getByRole("tooltip")).toHaveTextContent("2026-07-15");
    expect(screen.getByText("12,345")).toBeInTheDocument();
    expect(screen.getByText("10,000")).toBeInTheDocument();
    expect(screen.getByText("9,000")).toBeInTheDocument();
    expect(screen.getByText("2,345")).toBeInTheDocument();
    expect(screen.getByText("3")).toBeInTheDocument();
  });

	 it("shows unavailable Runtime metrics as a dash instead of zero", () => {
	   render(<UsageBarTooltip day={{
	     date: "2026-08-11",
	     usage: {
	       inputTokens: 12,
	       cachedInputTokens: 2,
	       outputTokens: 3,
	       reasoningOutputTokens: 0,
	       totalTokens: 15,
	       calls: 0,
	       metrics: {
	         cachedInputTokens: { available: true, complete: true, sources: ["pi_session_usage"] },
	         calls: { available: false, complete: false, sources: ["runtime_unavailable"] },
	       },
	     },
	   }} />);
	   expect(screen.getAllByText("Calls").at(-1)?.nextSibling).toHaveTextContent("—");
	 });
});
