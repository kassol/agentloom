import type { RuntimeConversationCapabilities, RuntimeOwnerConfiguration } from "./types";

export type CreateRuntimeKind = "codex" | "pi" | "claude";

export function createRuntimeOptions(catalogs: RuntimeConversationCapabilities[]): Array<{ runtimeKind: string }> {
	return catalogs.length ? catalogs : [{ runtimeKind: "codex" }, { runtimeKind: "pi" }, { runtimeKind: "claude" }];
}

export function runtimeLabel(kind: string): string {
	return kind === "codex" ? "Codex" : kind === "pi" ? "Pi" : kind === "claude" ? "Claude Code" : kind;
}

export function runtimeConfigurationNote(kind: CreateRuntimeKind): string {
	return kind === "claude"
		? "Claude Code runs from the pinned Claude Runtime generation. Settings layers and authentication are selected explicitly for every Agent."
		: "Pi inherits its native model, authentication, settings, skills, and extensions.";
}

export type RuntimeConfigurationSpec = {
	settingSources: Array<{ id: string; label: string; description: string }>;
	authentication: Array<{ category: string; label: string; sources: Array<{ id: string; label: string }> }>;
};

export const claudeRuntimeConfigurationSpec: RuntimeConfigurationSpec = {
	settingSources: [
		{ id: "user", label: "User", description: "Your Claude user settings" },
		{ id: "project", label: "Project", description: "Shared project settings" },
		{ id: "local", label: "Local", description: "Local project overrides" },
	],
	authentication: [
		{ category: "console", label: "Claude Console", sources: [{ id: "api_key", label: "API key" }] },
		{ category: "cloud", label: "Cloud provider", sources: [
			{ id: "bedrock", label: "Amazon Bedrock" }, { id: "vertex", label: "Google Vertex AI" }, { id: "foundry", label: "Microsoft Foundry" },
			{ id: "anthropic_aws", label: "Anthropic on AWS" }, { id: "anthropic_google_cloud", label: "Anthropic on Google Cloud" }, { id: "mantle", label: "Mantle" },
		] },
		{ category: "gateway", label: "Gateway", sources: [{ id: "gateway", label: "Managed gateway" }] },
	],
};

export function runtimeConfigurationSpec(kind: string): RuntimeConfigurationSpec | null {
	return kind === "claude" ? claudeRuntimeConfigurationSpec : null;
}

export function defaultRuntimeConfiguration(kind: string): RuntimeOwnerConfiguration | undefined {
	if (!runtimeConfigurationSpec(kind)) return undefined;
	return { configured: true, settingSources: ["user", "project", "local"], authentication: { category: "console", source: "api_key" } };
}
