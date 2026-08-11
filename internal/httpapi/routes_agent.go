package httpapi

import (
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/hub"
)

func (s *Server) registerAgentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agents/events", s.globalEvents)
	mux.HandleFunc("GET /api/agents", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("view") == "summary" {
			writeJSON(w, 200, map[string]any{"agents": s.hub.ListAgentSummaries()})
			return
		}
		writeJSON(w, 200, map[string]any{"agents": s.hub.ListAgents()})
	})
	mux.HandleFunc("POST /api/agents", func(w http.ResponseWriter, r *http.Request) {
		var p hub.CreateParams
		if err := readJSON(r, &p); err != nil {
			writeErr(w, err)
			return
		}
		agent, err := s.hub.CreateAgent(p)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"agent": agent})
	})
	mux.HandleFunc("POST /api/agents/restore", func(w http.ResponseWriter, r *http.Request) {
		var body hub.RestoreAgentParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		agent, err := s.hub.RestoreAgent(body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"agent": agent})
	})
	mux.HandleFunc("GET /api/agents/{key}", func(w http.ResponseWriter, r *http.Request) {
		agent, err := s.hub.GetAgent(r.PathValue("key"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"agent": agent})
	})
	mux.HandleFunc("GET /api/agents/{key}/runtime/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		diagnostics, err := s.hub.GetRuntimeDiagnostics(r.PathValue("key"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"diagnostics": diagnostics})
	})
	mux.HandleFunc("GET /api/agents/{key}/runtime/diagnostics/events", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
		if tail <= 0 {
			tail = 100
		}
		events, err := s.hub.ReadRuntimeDiagnosticEvents(r.PathValue("key"), since, tail)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]any{"events": events})
	})
	mux.HandleFunc("GET /api/turns/{turnId}", func(w http.ResponseWriter, r *http.Request) {
		turn, err := s.hub.GetCanonicalTurn(r.PathValue("turnId"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"turn": turn})
	})
	mux.HandleFunc("PATCH /api/agents/{key}/config", func(w http.ResponseWriter, r *http.Request) {
		var body hub.ConfigParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		agent, err := s.hub.UpdateAgentConfig(r.PathValue("key"), body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"agent": agent})
	})
	mux.HandleFunc("GET /api/agents/{key}/runtime/models", func(w http.ResponseWriter, r *http.Request) {
		models, err := s.hub.GetRuntimeModels(r.PathValue("key"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, models)
	})
	mux.HandleFunc("POST /api/agents/{key}/runtime/model", func(w http.ResponseWriter, r *http.Request) {
		var body hub.RuntimeModelSelection
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		models, err := s.hub.SwitchRuntimeModel(r.PathValue("key"), body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, models)
	})
	mux.HandleFunc("POST /api/agents/{key}/compact", func(w http.ResponseWriter, r *http.Request) {
		result, err := s.hub.CompactAgentThread(r.PathValue("key"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"compaction": result})
	})
	mux.HandleFunc("GET /api/agents/{key}/skills", func(w http.ResponseWriter, r *http.Request) {
		config, err := s.hub.GetAgentSkillConfig(r.PathValue("key"))
		if err != nil {
			writeErr(w, err)
			return
		}
		inventory, err := s.hub.AgentSkillInventory(r.PathValue("key"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"config": config, "inventory": inventory})
	})
	mux.HandleFunc("PATCH /api/agents/{key}/skills/config", func(w http.ResponseWriter, r *http.Request) {
		var body hub.AgentSkillConfigParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		config, err := s.hub.UpdateAgentSkillConfig(r.PathValue("key"), body)
		if err != nil {
			writeErr(w, err)
			return
		}
		inventory, err := s.hub.AgentSkillInventory(r.PathValue("key"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"config": config, "inventory": inventory})
	})
	mux.HandleFunc("GET /api/agents/{key}/profile", func(w http.ResponseWriter, r *http.Request) {
		profile, err := s.hub.GetProfile(r.PathValue("key"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"profile": profile})
	})
	mux.HandleFunc("GET /api/agents/{key}/usage", func(w http.ResponseWriter, r *http.Request) {
		start, endExclusive, explicit, rangeErr := calendarWindowFromRequest(r, time.Now())
		if rangeErr != nil {
			writeErr(w, &hub.HubError{Status: 400, Message: rangeErr.Error()})
			return
		}
		if explicit {
			usage, err := s.hub.AgentTokenUsageRange(r.PathValue("key"), start, endExclusive)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, 200, map[string]any{"usage": usage})
			return
		}
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		usage, err := s.hub.AgentTokenUsage(r.PathValue("key"), days)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"usage": usage})
	})
	mux.HandleFunc("GET /api/agents/{key}/workload", func(w http.ResponseWriter, r *http.Request) {
		start, endExclusive, explicit, rangeErr := calendarWindowFromRequest(r, time.Now())
		if rangeErr != nil {
			writeErr(w, &hub.HubError{Status: 400, Message: rangeErr.Error()})
			return
		}
		if explicit {
			workload, err := s.hub.AgentWorkloadRange(r.PathValue("key"), start, endExclusive)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, 200, map[string]any{"workload": workload})
			return
		}
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		workload, err := s.hub.AgentWorkload(r.PathValue("key"), days)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"workload": workload})
	})
	mux.HandleFunc("GET /api/agents/{key}/goal", func(w http.ResponseWriter, r *http.Request) {
		goal, revision, err := s.hub.GetGoalState(r.PathValue("key"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"goal": goal, "revision": revision})
	})
	mux.HandleFunc("PUT /api/agents/{key}/goal", func(w http.ResponseWriter, r *http.Request) {
		var body hub.GoalUpdateParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		if body.ExpectedVersion == nil {
			writeErr(w, &hub.HubError{Status: 400, Message: "expectedVersion is required"})
			return
		}
		goal, err := s.hub.UpdateGoal(r.PathValue("key"), body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"goal": goal})
	})
	mux.HandleFunc("DELETE /api/agents/{key}/goal", func(w http.ResponseWriter, r *http.Request) {
		var expectedVersion *int64
		if raw := strings.TrimSpace(r.URL.Query().Get("expectedVersion")); raw != "" {
			value, parseErr := strconv.ParseInt(raw, 10, 64)
			if parseErr != nil || value < 0 {
				writeErr(w, &hub.HubError{Status: 400, Message: "expectedVersion must be a non-negative integer"})
				return
			}
			expectedVersion = &value
		}
		if expectedVersion == nil {
			writeErr(w, &hub.HubError{Status: 400, Message: "expectedVersion is required"})
			return
		}
		cleared, err := s.hub.ClearGoalVersion(r.PathValue("key"), expectedVersion)
		if err != nil {
			writeErr(w, err)
			return
		}
		_, revision, _ := s.hub.GetGoalState(r.PathValue("key"))
		writeJSON(w, 200, map[string]any{"cleared": cleared, "revision": revision})
	})
	mux.HandleFunc("PUT /api/agents/{key}/profile", func(w http.ResponseWriter, r *http.Request) {
		var body hub.ProfileParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		profile, err := s.hub.UpdateProfile(r.PathValue("key"), body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"profile": profile})
	})
	mux.HandleFunc("DELETE /api/agents/{key}", func(w http.ResponseWriter, r *http.Request) {
		result, err := s.hub.ArchiveAgent(r.PathValue("key"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, result)
	})
	mux.HandleFunc("POST /api/agents/{key}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, hub.MaxThreadArtifactBytes+(1<<20))
		file, header, err := r.FormFile("file")
		if err != nil {
			writeErr(w, &hub.HubError{Status: 400, Message: "multipart file field is required"})
			return
		}
		defer file.Close()
		artifact, err := s.hub.StageThreadArtifact(r.PathValue("key"), header.Filename, header.Header.Get("Content-Type"), file)
		if err != nil {
			writeErr(w, err)
			return
		}
		if r.URL.Query().Get("publish") == "true" {
			artifact, err = s.hub.PublishThreadArtifact(r.PathValue("key"), artifact.ID)
			if err != nil {
				writeErr(w, err)
				return
			}
		}
		writeJSON(w, 201, map[string]any{"artifact": artifact})
	})
	mux.HandleFunc("GET /api/agents/{key}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		artifacts, err := s.hub.PublishedThreadArtifacts(r.PathValue("key"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"artifacts": artifacts})
	})
	mux.HandleFunc("GET /api/agents/{key}/artifacts/{artifactId}", func(w http.ResponseWriter, r *http.Request) {
		artifact, file, err := s.hub.OpenThreadArtifact(r.PathValue("key"), r.PathValue("artifactId"))
		if err != nil {
			writeErr(w, err)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			writeErr(w, err)
			return
		}
		disposition := threadArtifactDisposition(
			artifact.MimeType,
			r.URL.Query().Get("preview") == "1",
			r.URL.Query().Get("download") == "1",
		)
		w.Header().Set("Content-Type", artifact.MimeType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": artifact.Name}))
		w.Header().Set("Cache-Control", "private, max-age=300")
		w.Header().Set("Cross-Origin-Resource-Policy", threadArtifactResourcePolicy(artifact.MimeType))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, artifact.Name, info.ModTime(), file)
	})
	mux.HandleFunc("POST /api/agents/{key}/turns", func(w http.ResponseWriter, r *http.Request) {
		if s.isRestartPending() {
			writeErr(w, &hub.HubError{Status: 409, Message: "restart pending; wait for CodexLoom to restart before starting a Turn"})
			return
		}
		var body struct {
			Text        string   `json:"text"`
			ArtifactIDs []string `json:"artifactIds"`
			TimeoutSec  int      `json:"timeoutSec"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		result, err := s.hub.SendTaskWithArtifacts(r.PathValue("key"), body.Text, body.ArtifactIDs, time.Duration(body.TimeoutSec)*time.Second)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 202, result)
	})
	mux.HandleFunc("POST /api/agents/{key}/turns/current/interrupt", func(w http.ResponseWriter, r *http.Request) {
		result, err := s.hub.Interrupt(r.PathValue("key"), "")
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, result)
	})
	mux.HandleFunc("POST /api/agents/{key}/turns/interrupted/continue", func(w http.ResponseWriter, r *http.Request) {
		if s.isRestartPending() {
			writeErr(w, &hub.HubError{Status: 409, Message: "restart pending; wait for CodexLoom to restart before continuing a Turn"})
			return
		}
		var body struct {
			TimeoutSec int `json:"timeoutSec"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		result, err := s.hub.ContinueInterruptedTurn(r.PathValue("key"), time.Duration(body.TimeoutSec)*time.Second)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 202, result)
	})
	mux.HandleFunc("POST /api/agents/{key}/turns/interrupted/dismiss", func(w http.ResponseWriter, r *http.Request) {
		agent, err := s.hub.DismissInterruptedTurn(r.PathValue("key"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"agent": agent})
	})
	mux.HandleFunc("GET /api/agents/{key}/thread/history", func(w http.ResponseWriter, r *http.Request) {
		count, _ := strconv.Atoi(r.URL.Query().Get("count"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		hist, err := s.hub.CanonicalHistory(r.PathValue("key"), count, offset)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, hist)
	})
	mux.HandleFunc("POST /api/agents/{key}/thread/approvals/{approvalId}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Decision string `json:"decision"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		result, err := s.hub.ResolveApproval(r.PathValue("key"), r.PathValue("approvalId"), body.Decision)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, result)
	})
	mux.HandleFunc("GET /api/agents/{key}/thread/events", s.agentThreadEvents)

}

func threadArtifactDisposition(mimeType string, preview, download bool) string {
	if download {
		return "attachment"
	}
	normalized := strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	if preview {
		switch normalized {
		case "application/pdf", "image/png", "image/jpeg", "image/gif", "image/webp":
			return "inline"
		default:
			return "attachment"
		}
	}
	if strings.HasPrefix(normalized, "image/") {
		return "inline"
	}
	return "attachment"
}

func threadArtifactResourcePolicy(mimeType string) string {
	normalized := strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	switch normalized {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return "cross-origin"
	default:
		return "same-origin"
	}
}
