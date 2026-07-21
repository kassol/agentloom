import type { CollaborationGroup, TeamRelationship } from "./types";

export function relationshipContractAriaLabel(kind: "organization" | "collaboration", from: string, to: string, description: string) {
  return `${kind}: ${from} to ${to}. ${description}`;
}

export function projectCollaborationGroup(group: CollaborationGroup, relationships: TeamRelationship[]) {
  const byID = new Map(relationships.map((relationship) => [relationship.id, relationship]));
  const includedRelationships = group.relationshipIds.map((id) => byID.get(id)).filter(Boolean) as TeamRelationship[];
  const connectedAgentIDs = new Set(includedRelationships.flatMap((relationship) => [relationship.fromAgentId, relationship.toAgentId]));
  return {
    memberAgentIDs: [...group.memberAgentIds],
    includedRelationships,
    isolatedMemberAgentIDs: group.memberAgentIds.filter((id) => !connectedAgentIDs.has(id)),
    missingRelationshipIDs: group.relationshipIds.filter((id) => !byID.has(id)),
  };
}
