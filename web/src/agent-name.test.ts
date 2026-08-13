import { describe, expect, it } from "vitest";
import { agentNameError, agentNameHint } from "./agent-name";

describe("Agent name validation", () => {
	it("accepts names written with Unicode letters and numbers", () => {
		expect(agentNameError("研发助手-二号")).toBe("");
		expect(agentNameError("Développeur_2")).toBe("");
	});

	it("keeps identifier punctuation constraints explicit", () => {
		expect(agentNameError("研发 助手")).toBe(agentNameHint);
		expect(agentNameError("研发/助手")).toBe(agentNameHint);
		expect(agentNameError("  ")).toBe("Enter an Agent name.");
	});
});
