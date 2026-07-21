import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { isChunkLoadError, WorkspaceErrorBoundary } from "./workspace-recovery";

function BrokenWorkspace(): never {
  throw new Error("Failed to fetch dynamically imported module: /assets/TopicsPane-old.js");
}

describe("workspace load recovery", () => {
  it("recognizes module failures reported by Chromium and Safari", () => {
    expect(isChunkLoadError(new Error("Failed to fetch dynamically imported module"))).toBe(true);
    expect(isChunkLoadError(new Error("Importing a module script failed"))).toBe(true);
    expect(isChunkLoadError(new Error("Failed to load module script: MIME type text/html"))).toBe(true);
    expect(isChunkLoadError(new Error("ordinary render failure"))).toBe(false);
  });

  it("shows a recovery action when an automatic reload was already attempted", () => {
    vi.spyOn(console, "error").mockImplementation(() => undefined);
    const reload = vi.fn(() => false);
    render(
      <WorkspaceErrorBoundary resetKey="topics" onReload={reload}>
        <BrokenWorkspace />
      </WorkspaceErrorBoundary>,
    );

    expect(screen.getByRole("alert")).toHaveTextContent("This workspace could not load");
    fireEvent.click(screen.getByRole("button", { name: "Reload workspace" }));
    expect(reload).toHaveBeenLastCalledWith(true);
  });
});
