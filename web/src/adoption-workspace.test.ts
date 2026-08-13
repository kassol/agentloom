import { describe, expect, it } from "vitest";
import { adoptionWorkspaceRequest, defaultAdoptionWorkspace } from "./adoption-workspace";
import type { RuntimeConversationCandidate } from "./types";

const candidate: RuntimeConversationCandidate = {
	id: "candidate-1",
	revision: "revision-1",
	runtimeKind: "codex",
	name: "Existing conversation",
	cwd: "/source/workspace",
	updatedAt: "2026-08-13T00:00:00Z",
	compatible: true,
};

describe("conversation adoption workspace", () => {
	it("defaults to the source workspace without conflating it with the selected stable workspace", () => {
		expect(defaultAdoptionWorkspace(candidate)).toBe("/source/workspace");
		expect(adoptionWorkspaceRequest(candidate, "  /stable/workspace  ")).toEqual({
			candidateId: "candidate-1",
			expectedRevision: "revision-1",
			cwd: "/stable/workspace",
		});
		expect(candidate.cwd).toBe("/source/workspace");
	});
});
