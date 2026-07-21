package hub

import (
	"sort"
	"strings"
)

const (
	CollaborationGroupStatusActive   = "active"
	CollaborationGroupStatusArchived = "archived"
)

// CollaborationGroup is a named, shared view over explicit collaboration
// relationships. It has no routing, permission, execution, or context effects.
type CollaborationGroup struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Status          string   `json:"status"`
	MemberAgentIDs  []string `json:"memberAgentIds"`
	RelationshipIDs []string `json:"relationshipIds"`
	Version         int      `json:"version"`
	CreatedAt       string   `json:"createdAt"`
	UpdatedAt       string   `json:"updatedAt"`
}

type CollaborationGroupParams struct {
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Status          string   `json:"status"`
	MemberAgentIDs  []string `json:"memberAgentIds"`
	RelationshipIDs []string `json:"relationshipIds"`
	ExpectedVersion *int     `json:"expectedVersion,omitempty"`
}

func (h *Hub) ListCollaborationGroups(status string) ([]CollaborationGroup, error) {
	status = strings.TrimSpace(strings.ToLower(status))
	if status != "" && status != CollaborationGroupStatusActive && status != CollaborationGroupStatusArchived {
		return nil, errf(400, "status must be active or archived")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]CollaborationGroup, 0, len(h.collaborationGroups))
	for _, group := range h.collaborationGroups {
		if status != "" && group.Status != status {
			continue
		}
		out = append(out, collaborationGroupCopy(group))
	}
	sortCollaborationGroups(out)
	return out, nil
}

func (h *Hub) GetCollaborationGroup(id string) (CollaborationGroup, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	group := h.collaborationGroups[strings.TrimSpace(id)]
	if group == nil {
		return CollaborationGroup{}, errf(404, "collaboration group not found: %s", id)
	}
	return collaborationGroupCopy(group), nil
}

func (h *Hub) CreateCollaborationGroup(params CollaborationGroupParams) (CollaborationGroup, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	normalized, err := h.normalizeCollaborationGroupParamsLocked(params, "")
	if err != nil {
		return CollaborationGroup{}, err
	}
	timestamp := now()
	group := &CollaborationGroup{
		ID:              "cgrp_" + strings.TrimPrefix(newRelationshipID(), "rel_"),
		Name:            normalized.Name,
		Description:     normalized.Description,
		Status:          normalized.Status,
		MemberAgentIDs:  normalized.MemberAgentIDs,
		RelationshipIDs: normalized.RelationshipIDs,
		Version:         1,
		CreatedAt:       timestamp,
		UpdatedAt:       timestamp,
	}
	h.collaborationGroups[group.ID] = group
	if err := h.st.SaveCollaborationGroups(h.collaborationGroups); err != nil {
		delete(h.collaborationGroups, group.ID)
		return CollaborationGroup{}, errf(500, "save collaboration groups: %s", err)
	}
	copy := collaborationGroupCopy(group)
	h.emitGlobalLocked("loom/collaboration-group-updated", map[string]any{"group": copy})
	return copy, nil
}

func (h *Hub) UpdateCollaborationGroup(id string, params CollaborationGroupParams) (CollaborationGroup, error) {
	id = strings.TrimSpace(id)
	h.mu.Lock()
	defer h.mu.Unlock()
	current := h.collaborationGroups[id]
	if current == nil {
		return CollaborationGroup{}, errf(404, "collaboration group not found: %s", id)
	}
	if params.ExpectedVersion != nil && *params.ExpectedVersion != current.Version {
		return CollaborationGroup{}, errf(409, "collaboration group version changed: expected %d, current %d", *params.ExpectedVersion, current.Version)
	}
	normalized, err := h.normalizeCollaborationGroupParamsLocked(params, id)
	if err != nil {
		return CollaborationGroup{}, err
	}
	if collaborationGroupEqual(current, normalized) {
		return collaborationGroupCopy(current), nil
	}
	previous := collaborationGroupCopy(current)
	current.Name = normalized.Name
	current.Description = normalized.Description
	current.Status = normalized.Status
	current.MemberAgentIDs = normalized.MemberAgentIDs
	current.RelationshipIDs = normalized.RelationshipIDs
	current.Version++
	current.UpdatedAt = now()
	if err := h.st.SaveCollaborationGroups(h.collaborationGroups); err != nil {
		*current = previous
		return CollaborationGroup{}, errf(500, "save collaboration groups: %s", err)
	}
	copy := collaborationGroupCopy(current)
	h.emitGlobalLocked("loom/collaboration-group-updated", map[string]any{"group": copy})
	return copy, nil
}

func (h *Hub) DeleteCollaborationGroup(id string, expectedVersion *int) (CollaborationGroup, error) {
	id = strings.TrimSpace(id)
	h.mu.Lock()
	defer h.mu.Unlock()
	group := h.collaborationGroups[id]
	if group == nil {
		return CollaborationGroup{}, errf(404, "collaboration group not found: %s", id)
	}
	if expectedVersion != nil && *expectedVersion != group.Version {
		return CollaborationGroup{}, errf(409, "collaboration group version changed: expected %d, current %d", *expectedVersion, group.Version)
	}
	copy := collaborationGroupCopy(group)
	delete(h.collaborationGroups, id)
	if err := h.st.SaveCollaborationGroups(h.collaborationGroups); err != nil {
		h.collaborationGroups[id] = group
		return CollaborationGroup{}, errf(500, "save collaboration groups: %s", err)
	}
	h.emitGlobalLocked("loom/collaboration-group-deleted", map[string]any{"group": copy})
	return copy, nil
}

func (h *Hub) normalizeCollaborationGroupParamsLocked(params CollaborationGroupParams, currentID string) (CollaborationGroupParams, error) {
	params.Name = strings.TrimSpace(params.Name)
	params.Description = strings.TrimSpace(params.Description)
	params.Status = strings.TrimSpace(strings.ToLower(params.Status))
	if params.Status == "" {
		params.Status = CollaborationGroupStatusActive
	}
	if params.Name == "" {
		return CollaborationGroupParams{}, errf(400, "name is required")
	}
	if len(params.Name) > 160 {
		return CollaborationGroupParams{}, errf(400, "name must be at most 160 bytes")
	}
	if params.Description == "" {
		return CollaborationGroupParams{}, errf(400, "description is required")
	}
	if len(params.Description) > 16_000 {
		return CollaborationGroupParams{}, errf(400, "description must be at most 16000 bytes")
	}
	if params.Status != CollaborationGroupStatusActive && params.Status != CollaborationGroupStatusArchived {
		return CollaborationGroupParams{}, errf(400, "status must be active or archived")
	}
	for id, group := range h.collaborationGroups {
		if id == currentID || group.Status != CollaborationGroupStatusActive || params.Status != CollaborationGroupStatusActive {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(group.Name), params.Name) {
			return CollaborationGroupParams{}, errf(409, "active collaboration group already exists with name %q", group.Name)
		}
	}

	members := make([]string, 0, len(params.MemberAgentIDs))
	memberSet := map[string]bool{}
	for _, key := range params.MemberAgentIDs {
		key = strings.TrimSpace(key)
		agentID, ok := h.resolveAgentIDLocked(key)
		if !ok {
			if params.Status != CollaborationGroupStatusArchived || key == "" {
				return CollaborationGroupParams{}, errf(400, "collaboration group member agent not found: %s", key)
			}
			agentID = key
		}
		if memberSet[agentID] {
			continue
		}
		memberSet[agentID] = true
		members = append(members, agentID)
	}
	sort.Strings(members)
	if len(members) == 0 {
		return CollaborationGroupParams{}, errf(400, "at least one collaboration group member is required")
	}

	relationships := make([]string, 0, len(params.RelationshipIDs))
	relationshipSet := map[string]bool{}
	for _, key := range params.RelationshipIDs {
		id := strings.TrimSpace(key)
		if id == "" || relationshipSet[id] {
			continue
		}
		relationship := h.teamLinks[id]
		if relationship == nil {
			if params.Status == CollaborationGroupStatusArchived {
				relationshipSet[id] = true
				relationships = append(relationships, id)
				continue
			}
			return CollaborationGroupParams{}, errf(400, "collaboration relationship not found: %s", id)
		}
		if !memberSet[relationship.FromAgentID] || !memberSet[relationship.ToAgentID] {
			return CollaborationGroupParams{}, errf(400, "relationship %s endpoints must both be collaboration group members", id)
		}
		relationshipSet[id] = true
		relationships = append(relationships, id)
	}
	sort.Strings(relationships)
	params.MemberAgentIDs = members
	params.RelationshipIDs = relationships
	return params, nil
}

func (h *Hub) validateLoadedCollaborationGroupsLocked() error {
	for id, group := range h.collaborationGroups {
		if group == nil || strings.TrimSpace(group.ID) == "" || group.ID != id {
			return errf(500, "invalid collaboration group record: %s", id)
		}
		params, err := h.normalizeCollaborationGroupParamsLocked(CollaborationGroupParams{
			Name: group.Name, Description: group.Description, Status: group.Status,
			MemberAgentIDs: group.MemberAgentIDs, RelationshipIDs: group.RelationshipIDs,
		}, id)
		if err != nil {
			return err
		}
		group.Name = params.Name
		group.Description = params.Description
		group.Status = params.Status
		group.MemberAgentIDs = params.MemberAgentIDs
		group.RelationshipIDs = params.RelationshipIDs
		if group.Version < 1 || group.CreatedAt == "" || group.UpdatedAt == "" {
			return errf(500, "invalid collaboration group metadata: %s", id)
		}
	}
	return nil
}

func collaborationGroupCopy(group *CollaborationGroup) CollaborationGroup {
	copy := *group
	copy.MemberAgentIDs = append([]string(nil), group.MemberAgentIDs...)
	copy.RelationshipIDs = append([]string(nil), group.RelationshipIDs...)
	return copy
}

func collaborationGroupEqual(group *CollaborationGroup, params CollaborationGroupParams) bool {
	return group.Name == params.Name && group.Description == params.Description && group.Status == params.Status &&
		stringSlicesEqual(group.MemberAgentIDs, params.MemberAgentIDs) && stringSlicesEqual(group.RelationshipIDs, params.RelationshipIDs)
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sortCollaborationGroups(groups []CollaborationGroup) {
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Status != groups[j].Status {
			return groups[i].Status == CollaborationGroupStatusActive
		}
		left := strings.ToLower(groups[i].Name)
		right := strings.ToLower(groups[j].Name)
		if left != right {
			return left < right
		}
		return groups[i].ID < groups[j].ID
	})
}
