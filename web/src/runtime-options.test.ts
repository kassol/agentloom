import { describe, expect, it } from "vitest";
import { createRuntimeOptions, defaultRuntimeConfiguration, runtimeConfigurationNote, runtimeConfigurationSpec, runtimeLabel } from "./runtime-options";

describe("Agent create Runtime options", () => {
	it("offers Claude Code while the catalog is loading", () => {
		expect(createRuntimeOptions([]).map((option) => option.runtimeKind)).toEqual(["codex", "pi", "claude"]);
		expect(runtimeLabel("claude")).toBe("Claude Code");
		expect(runtimeConfigurationNote("claude")).toContain("pinned Claude Runtime generation");
		expect(runtimeConfigurationNote("claude")).not.toContain("Pi");
	});

	it("requires explicit Claude settings and authentication while leaving shared Runtimes neutral", () => {
		expect(runtimeConfigurationSpec("codex")).toBeNull();
		expect(runtimeConfigurationSpec("pi")).toBeNull();
		expect(runtimeConfigurationSpec("claude")?.settingSources.map((source) => source.id)).toEqual(["user", "project", "local"]);
		expect(runtimeConfigurationSpec("claude")?.authentication.find((item) => item.category === "console")?.sources).toEqual([{ id: "api_key", label: "API key" }]);
		expect(defaultRuntimeConfiguration("claude")).toEqual({ configured: true, settingSources: ["user", "project", "local"], authentication: { category: "console", source: "api_key" } });
	});
});
