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
});
