import { describe, expect, it } from "vitest";
import { projectCollaborationGroup, relationshipContractAriaLabel } from "./collaboration-groups";
import type { CollaborationGroup, TeamRelationship } from "./types";

const relationships: TeamRelationship[] = [
  { id: "rel-platform-edge", fromAgentId: "platform", toAgentId: "edge", from: "platform", to: "edge", description: "API handoff", createdAt: "now", updatedAt: "now" },
  { id: "rel-lead-edge", fromAgentId: "lead", toAgentId: "edge", from: "lead", to: "edge", description: "Not included", createdAt: "now", updatedAt: "now" },
];

describe("projectCollaborationGroup", () => {
  it("keeps explicit members without inferring all-to-all relationships", () => {
    const group: CollaborationGroup = {
      id: "cgrp-parall", name: "Parall Development", description: "Stable interfaces", status: "active",
      memberAgentIds: ["lead", "platform", "edge"], relationshipIds: ["rel-platform-edge"],
      version: 1, createdAt: "now", updatedAt: "now",
    };
    const result = projectCollaborationGroup(group, relationships);
    expect(result.memberAgentIDs).toEqual(["lead", "platform", "edge"]);
    expect(result.includedRelationships.map((relationship) => relationship.id)).toEqual(["rel-platform-edge"]);
    expect(result.isolatedMemberAgentIDs).toEqual(["lead"]);
    expect(result.includedRelationships).not.toContainEqual(expect.objectContaining({ id: "rel-lead-edge" }));
  });

  it("preserves unavailable relationship ids for archived audit views", () => {
    const group: CollaborationGroup = {
      id: "cgrp-history", name: "Archived", description: "Historical view", status: "archived",
      memberAgentIds: ["lead"], relationshipIds: ["rel-removed"], version: 2, createdAt: "then", updatedAt: "now",
    };
    const result = projectCollaborationGroup(group, relationships);
    expect(result.includedRelationships).toEqual([]);
    expect(result.missingRelationshipIDs).toEqual(["rel-removed"]);
    expect(result.isolatedMemberAgentIDs).toEqual(["lead"]);
  });
});

describe("relationshipContractAriaLabel", () => {
  it("includes relationship type, endpoints and the complete contract", () => {
    expect(relationshipContractAriaLabel("collaboration", "research", "operator", "Keep evidence strength unchanged."))
      .toBe("collaboration: research to operator. Keep evidence strength unchanged.");
  });
});
