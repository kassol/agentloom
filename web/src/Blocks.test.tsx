import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { BlockView } from "./Blocks";

describe("command blocks", () => {
  it("renders an interrupted command as terminal", () => {
    const view = render(<BlockView block={{
      kind: "command",
      id: "sleep-45",
      command: "sleep 45",
      status: "interrupted",
      exitCode: null,
      durationMs: null,
      output: "",
    }} />);

    expect(view.getByText("interrupted")).toBeInTheDocument();
    expect(view.queryByText("running")).toBeNull();
  });

  it("shows the command purpose first and keeps the raw command in details", () => {
    const view = render(<BlockView block={{
      kind: "command",
      id: "status",
      command: "git status --short",
      description: "Confirm the repository state",
      status: "completed",
      exitCode: 0,
      durationMs: 12,
      output: "",
    }} />);

    expect(view.container.querySelector("summary")).toHaveTextContent("Confirm the repository state");
    expect(view.getByText("git status --short")).toBeInTheDocument();
  });
});
