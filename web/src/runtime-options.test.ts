import { describe, expect, it } from "vitest";
import { createRuntimeOptions, defaultRuntimeConfiguration, runtimeConfigurationNote, runtimeConfigurationSpec, runtimeLabel } from "./runtime-options";

describe("Agent create Runtime options", () => {
	it("offers Claude Code while the catalog is loading", () => {
		expect(createRuntimeOptions([]).map((option) => option.runtimeKind)).toEqual(["codex", "pi", "claude"]);
		expect(runtimeLabel("claude")).toBe("Claude Code");
		expect(runtimeConfigurationNote()).toContain("native authentication");
	});

	it("drives explicit settings and authentication from the Runtime descriptor", () => {
		const configuration = {
			settingSources: [{ id: "project", label: "Project" }],
			authentication: [{ category: "console", label: "Console", sources: [{ id: "api_key", label: "API key" }] }],
			default: { configured: true, settingSources: ["project"], authentication: { category: "console", source: "api_key" } },
		};
		const catalog = { runtimeKind: "future", revision: "1", capabilities: [], configuration };
		expect(runtimeConfigurationSpec(catalog)).toBe(configuration);
		expect(runtimeConfigurationNote(configuration)).toContain("selected explicitly");
		expect(defaultRuntimeConfiguration(configuration)).toEqual(configuration.default);
		expect(runtimeConfigurationSpec({ runtimeKind: "native", revision: "1", capabilities: [] })).toBeNull();
	});
});
