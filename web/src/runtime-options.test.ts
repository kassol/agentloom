import { describe, expect, it } from "vitest";
import { createRuntimeOptions, runtimeConfigurationNote, runtimeLabel } from "./runtime-options";

describe("Agent create Runtime options", () => {
	it("offers Claude Code while the catalog is loading", () => {
		expect(createRuntimeOptions([]).map((option) => option.runtimeKind)).toEqual(["codex", "pi", "claude"]);
		expect(runtimeLabel("claude")).toBe("Claude Code");
		expect(runtimeConfigurationNote("claude")).toContain("pinned Claude Runtime generation");
		expect(runtimeConfigurationNote("claude")).not.toContain("Pi");
	});
});
