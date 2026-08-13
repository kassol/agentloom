import type { RuntimeConversationCandidate } from "./types";

export function defaultAdoptionWorkspace(candidate?: RuntimeConversationCandidate): string {
	return candidate?.cwd || "";
}

export function adoptionWorkspaceRequest(candidate: RuntimeConversationCandidate, cwd: string) {
	return {
		candidateId: candidate.id,
		expectedRevision: candidate.revision,
		cwd: cwd.trim(),
	};
}
