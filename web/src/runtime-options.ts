import type { RuntimeConfigurationSpec, RuntimeConversationCapabilities, RuntimeOwnerConfiguration } from "./types";

export type CreateRuntimeKind = "codex" | "pi" | "claude";

export function createRuntimeOptions(catalogs: RuntimeConversationCapabilities[]): Array<{ runtimeKind: string }> {
	return catalogs.length ? catalogs : [{ runtimeKind: "codex" }, { runtimeKind: "pi" }, { runtimeKind: "claude" }];
}

export function runtimeLabel(kind: string): string {
	return kind === "codex" ? "Codex" : kind === "pi" ? "Pi" : kind === "claude" ? "Claude Code" : kind;
}

export function runtimeConfigurationNote(spec?: RuntimeConfigurationSpec): string {
	return spec
		? "Settings layers and authentication are selected explicitly for every Agent."
		: "This Runtime uses its native authentication, settings, and resources.";
}

export function runtimeConfigurationSpec(catalog?: RuntimeConversationCapabilities): RuntimeConfigurationSpec | null {
	return catalog?.configuration || null;
}

export function defaultRuntimeConfiguration(spec?: RuntimeConfigurationSpec): RuntimeOwnerConfiguration | undefined {
	if (!spec) return undefined;
	return {
		...spec.default,
		settingSources: [...spec.default.settingSources],
		authentication: { ...spec.default.authentication },
	};
}
