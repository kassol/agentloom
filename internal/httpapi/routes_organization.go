package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/yan5xu/codex-loom/internal/hub"
)

func (s *Server) registerOrganizationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/comms", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"messages": s.hub.ListComms(r.URL.Query().Get("agent"), r.URL.Query().Get("status")),
		})
	})

	mux.HandleFunc("GET /api/team", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"team": s.hub.Team()})
	})

	mux.HandleFunc("GET /api/team/activity", func(w http.ResponseWriter, r *http.Request) {
		days := 7
		if raw := r.URL.Query().Get("days"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 || value > 3650 {
				writeJSON(w, 400, map[string]any{"error": "days must be between 0 and 3650"})
				return
			}
			days = value
		}
		writeJSON(w, 200, map[string]any{"days": days, "observedLinks": s.hub.TeamActivity(days)})
	})

	mux.HandleFunc("GET /api/team/relationships", func(w http.ResponseWriter, r *http.Request) {
		relationships, err := s.hub.ListRelationships(r.URL.Query().Get("agent"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"relationships": relationships})
	})

	mux.HandleFunc("GET /api/team/organization", func(w http.ResponseWriter, r *http.Request) {
		relationships, err := s.hub.ListOrganizationRelationships(r.URL.Query().Get("agent"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"relationships": relationships})
	})

	mux.HandleFunc("GET /api/team/collaboration-groups", func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		if status == "all" {
			status = ""
		}
		groups, err := s.hub.ListCollaborationGroups(status)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"groups": groups})
	})

	mux.HandleFunc("POST /api/team/collaboration-groups", func(w http.ResponseWriter, r *http.Request) {
		var body hub.CollaborationGroupParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		group, err := s.hub.CreateCollaborationGroup(body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"group": group})
	})

	mux.HandleFunc("GET /api/team/collaboration-groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		group, err := s.hub.GetCollaborationGroup(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"group": group})
	})

	mux.HandleFunc("PATCH /api/team/collaboration-groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name            *string   `json:"name"`
			Description     *string   `json:"description"`
			Status          *string   `json:"status"`
			MemberAgentIDs  *[]string `json:"memberAgentIds"`
			RelationshipIDs *[]string `json:"relationshipIds"`
			ExpectedVersion *int      `json:"expectedVersion"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		current, err := s.hub.GetCollaborationGroup(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		params := hub.CollaborationGroupParams{
			Name: current.Name, Description: current.Description, Status: current.Status,
			MemberAgentIDs: current.MemberAgentIDs, RelationshipIDs: current.RelationshipIDs,
			ExpectedVersion: body.ExpectedVersion,
		}
		if body.Name != nil {
			params.Name = *body.Name
		}
		if body.Description != nil {
			params.Description = *body.Description
		}
		if body.Status != nil {
			params.Status = *body.Status
		}
		if body.MemberAgentIDs != nil {
			params.MemberAgentIDs = *body.MemberAgentIDs
		}
		if body.RelationshipIDs != nil {
			params.RelationshipIDs = *body.RelationshipIDs
		}
		group, err := s.hub.UpdateCollaborationGroup(r.PathValue("id"), params)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"group": group})
	})

	mux.HandleFunc("DELETE /api/team/collaboration-groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		var expectedVersion *int
		if raw := strings.TrimSpace(r.URL.Query().Get("expectedVersion")); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 {
				writeJSON(w, 400, map[string]any{"error": "expectedVersion must be a positive integer"})
				return
			}
			expectedVersion = &value
		}
		group, err := s.hub.DeleteCollaborationGroup(r.PathValue("id"), expectedVersion)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"group": group})
	})

	mux.HandleFunc("POST /api/team/organization", func(w http.ResponseWriter, r *http.Request) {
		var body hub.OrganizationRelationshipParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		relationship, err := s.hub.CreateOrganizationRelationship(body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"relationship": relationship})
	})

	mux.HandleFunc("PATCH /api/team/organization/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body hub.OrganizationRelationshipParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		relationship, err := s.hub.UpdateOrganizationRelationship(r.PathValue("id"), body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"relationship": relationship})
	})

	mux.HandleFunc("DELETE /api/team/organization/{id}", func(w http.ResponseWriter, r *http.Request) {
		relationship, err := s.hub.DeleteOrganizationRelationship(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"relationship": relationship})
	})

	mux.HandleFunc("POST /api/team/relationships", func(w http.ResponseWriter, r *http.Request) {
		var body hub.RelationshipParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		relationship, err := s.hub.CreateRelationship(body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"relationship": relationship})
	})

	mux.HandleFunc("PATCH /api/team/relationships/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body hub.RelationshipParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		relationship, err := s.hub.UpdateRelationship(r.PathValue("id"), body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"relationship": relationship})
	})

	mux.HandleFunc("DELETE /api/team/relationships/{id}", func(w http.ResponseWriter, r *http.Request) {
		relationship, err := s.hub.DeleteRelationship(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"relationship": relationship})
	})

	mux.HandleFunc("GET /api/schedules", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"schedules": s.hub.ListSchedules()})
	})

	mux.HandleFunc("POST /api/schedules", func(w http.ResponseWriter, r *http.Request) {
		var body hub.ScheduleParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		schedule, err := s.hub.CreateSchedule(body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"schedule": schedule})
	})

	mux.HandleFunc("GET /api/schedules/{id}", func(w http.ResponseWriter, r *http.Request) {
		schedule, err := s.hub.GetSchedule(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"schedule": schedule})
	})

	mux.HandleFunc("POST /api/schedules/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		if s.isRestartPending() {
			writeErr(w, &hub.HubError{Status: 409, Message: "restart pending; wait for hub to restart before running schedules"})
			return
		}
		schedule, err := s.hub.RunSchedule(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 202, map[string]any{"schedule": schedule})
	})

	mux.HandleFunc("POST /api/schedules/{id}/enable", func(w http.ResponseWriter, r *http.Request) {
		schedule, err := s.hub.SetScheduleEnabled(r.PathValue("id"), true)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"schedule": schedule})
	})

	mux.HandleFunc("POST /api/schedules/{id}/disable", func(w http.ResponseWriter, r *http.Request) {
		schedule, err := s.hub.SetScheduleEnabled(r.PathValue("id"), false)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"schedule": schedule})
	})

	mux.HandleFunc("DELETE /api/schedules/{id}", func(w http.ResponseWriter, r *http.Request) {
		schedule, err := s.hub.DeleteSchedule(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"schedule": schedule})
	})

	mux.HandleFunc("POST /api/comms/messages", func(w http.ResponseWriter, r *http.Request) {
		var body hub.CommParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		if s.isRestartPending() && !isDrainCompletionMessage(body) {
			writeErr(w, &hub.HubError{Status: 409, Message: "restart pending; wait for hub to restart before sending new agent messages"})
			return
		}
		result, err := s.hub.SendAgentMessage(body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 202, result)
	})

	mux.HandleFunc("GET /api/comms/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		msg, err := s.hub.GetAgentMessage(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"message": msg})
	})

	mux.HandleFunc("POST /api/comms/messages/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		msg, err := s.hub.CancelAgentMessage(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"message": msg})
	})

	mux.HandleFunc("POST /api/comms/messages/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		msg, err := s.hub.RetryAgentMessage(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 202, map[string]any{"message": msg})
	})

	mux.HandleFunc("POST /api/comms/messages/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		var body hub.ResolveAgentMessageParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		msg, err := s.hub.ResolveAgentMessage(r.PathValue("id"), body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"message": msg})
	})

	mux.HandleFunc("POST /api/comms/messages/{id}/no-reply", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			From string `json:"from"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		msg, err := s.hub.NoReplyAgentMessage(r.PathValue("id"), body.From)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"message": msg})
	})

}

// A reply completes work already counted by graceful drain. Rejecting it
// while restart waits for the current Turn creates a reply/restart deadlock.
func isDrainCompletionMessage(params hub.CommParams) bool {
	return strings.TrimSpace(params.ReplyTo) != ""
}
