export const agentNameHint = "Use letters or numbers from any language, hyphens, or underscores.";

const agentNamePattern = /^[\p{L}\p{M}\p{N}_-]+$/u;

export function agentNameError(value: string): string {
	const name = value.trim();
	if (!name) return "Enter an Agent name.";
	if (!agentNamePattern.test(name)) return agentNameHint;
	return "";
}
