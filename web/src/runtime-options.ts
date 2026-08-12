import type { RuntimeConversationCapabilities } from "./types";

export type CreateRuntimeKind = "codex" | "pi" | "claude";

export function createRuntimeOptions(catalogs: RuntimeConversationCapabilities[]): Array<{ runtimeKind: string }> {
	return catalogs.length ? catalogs : [{ runtimeKind: "codex" }, { runtimeKind: "pi" }, { runtimeKind: "claude" }];
}

export function runtimeLabel(kind: string): string {
	return kind === "codex" ? "Codex" : kind === "pi" ? "Pi" : kind === "claude" ? "Claude Code" : kind;
}

export function runtimeConfigurationNote(kind: CreateRuntimeKind): string {
	return kind === "claude"
		? "Claude Code runs from the pinned Claude Runtime generation and uses its native authentication and settings. Optional controls follow the Runtime capability snapshot."
		: "Pi inherits its native model, authentication, settings, skills, and extensions.";
}
