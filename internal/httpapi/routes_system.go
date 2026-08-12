package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yan5xu/codex-loom/internal/claudegen"
	"github.com/yan5xu/codex-loom/internal/hub"
)

func (s *Server) registerSystemRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"build": s.build})
	})

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		agents := s.hub.ListAgents()
		writeJSON(w, 200, map[string]any{
			"ok": true, "product": "CodexLoom", "dataDir": s.st.Dir(),
			"agents": len(agents), "sessions": len(agents), "build": s.build,
		})
	})

	mux.HandleFunc("GET /api/runtime-generations/claude", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"generation": s.claudeGenerations.Status(r.Context())})
	})
	mux.HandleFunc("POST /api/runtime-generations/claude/install", func(w http.ResponseWriter, r *http.Request) {
		if !allowClaudeGenerationAdminRequest(w, r) {
			return
		}
		if !s.beginGenerationMutation(w) {
			return
		}
		defer s.endGenerationMutation()
		var request struct {
			AcceptTerms bool `json:"acceptTerms"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
			return
		}
		status, err := s.claudeGenerations.Install(r.Context(), claudegen.InstallRequest{AcceptTerms: request.AcceptTerms})
		s.writeClaudeGenerationResult(w, "install", status, err)
	})
	mux.HandleFunc("POST /api/runtime-generations/claude/verify", func(w http.ResponseWriter, r *http.Request) {
		if !allowClaudeGenerationAdminRequest(w, r) {
			return
		}
		if !s.beginGenerationMutation(w) {
			return
		}
		defer s.endGenerationMutation()
		var request struct {
			Target string `json:"target"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil && err.Error() != "EOF" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
			return
		}
		status, err := s.claudeGenerations.Verify(r.Context(), request.Target)
		s.writeClaudeGenerationResult(w, "verify", status, err)
	})
	mux.HandleFunc("POST /api/runtime-generations/claude/activate", func(w http.ResponseWriter, r *http.Request) {
		if !allowClaudeGenerationAdminRequest(w, r) {
			return
		}
		if !s.beginGenerationMutation(w) {
			return
		}
		defer s.endGenerationMutation()
		status, err := s.claudeGenerations.Activate(r.Context())
		s.writeClaudeGenerationResult(w, "activate", status, err)
	})
	mux.HandleFunc("POST /api/runtime-generations/claude/rollback", func(w http.ResponseWriter, r *http.Request) {
		if !allowClaudeGenerationAdminRequest(w, r) {
			return
		}
		if !s.beginGenerationMutation(w) {
			return
		}
		defer s.endGenerationMutation()
		status, err := s.claudeGenerations.Rollback(r.Context())
		s.writeClaudeGenerationResult(w, "rollback", status, err)
	})

	mux.HandleFunc("POST /api/admin/restart", s.adminRestart)
	mux.HandleFunc("GET /api/admin/restart/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"restart": s.restartSnapshot()})
	})
	mux.HandleFunc("GET /api/admin/backups", s.adminListBackups)
	mux.HandleFunc("POST /api/admin/backup", s.adminBackup)
	mux.HandleFunc("POST /api/admin/backups/prune", s.adminPruneBackups)
	mux.HandleFunc("POST /api/skills/reload", func(w http.ResponseWriter, r *http.Request) {
		inventory, err := s.hub.ReloadSkills()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"inventory": inventory})
	})
	mux.HandleFunc("GET /api/usage", func(w http.ResponseWriter, r *http.Request) {
		start, endExclusive, explicit, err := calendarWindowFromRequest(r, time.Now())
		if err != nil {
			writeErr(w, &hub.HubError{Status: 400, Message: err.Error()})
			return
		}
		if explicit {
			writeJSON(w, 200, map[string]any{"usage": s.hub.TokenUsageOverviewRange(start, endExclusive)})
			return
		}
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		writeJSON(w, 200, map[string]any{"usage": s.hub.TokenUsageOverview(days)})
	})
	mux.HandleFunc("GET /api/workload", func(w http.ResponseWriter, r *http.Request) {
		start, endExclusive, explicit, err := calendarWindowFromRequest(r, time.Now())
		if err != nil {
			writeErr(w, &hub.HubError{Status: 400, Message: err.Error()})
			return
		}
		if explicit {
			writeJSON(w, 200, map[string]any{"workload": s.hub.WorkloadOverviewRange(start, endExclusive)})
			return
		}
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		writeJSON(w, 200, map[string]any{"workload": s.hub.WorkloadOverview(days)})
	})
	mux.HandleFunc("GET /api/activity/daily", func(w http.ResponseWriter, r *http.Request) {
		start, endExclusive, bucketMinutes, err := dailyActivityWindowFromRequest(r, time.Now())
		if err != nil {
			writeErr(w, &hub.HubError{Status: 400, Message: err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"activity": s.hub.DailyActivity(start, endExclusive, bucketMinutes)})
	})

	mux.HandleFunc("GET /api/remote", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"remote": s.hub.RemoteSnapshot()})
	})
	mux.HandleFunc("POST /api/remote/enable", func(w http.ResponseWriter, r *http.Request) {
		remote, err := s.hub.EnableRemote()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"remote": remote})
	})
	mux.HandleFunc("POST /api/remote/disable", func(w http.ResponseWriter, r *http.Request) {
		remote, err := s.hub.DisableRemote()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"remote": remote})
	})
	mux.HandleFunc("POST /api/remote/pairing", func(w http.ResponseWriter, r *http.Request) {
		pairing, err := s.hub.StartRemotePairing()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"pairing": pairing})
	})
	mux.HandleFunc("GET /api/remote/pairing", func(w http.ResponseWriter, r *http.Request) {
		pairing, err := s.hub.ReadRemotePairing()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"pairing": pairing})
	})
	mux.HandleFunc("GET /api/remote/devices", func(w http.ResponseWriter, r *http.Request) {
		devices, err := s.hub.ListRemoteDevices()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"devices": devices})
	})
	mux.HandleFunc("DELETE /api/remote/devices/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.hub.RevokeRemoteDevice(r.PathValue("id")); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"revoked": true})
	})

}

func (s *Server) writeClaudeGenerationResult(w http.ResponseWriter, action string, status claudegen.Status, err error) {
	message := claudegen.PublicMessage(err)
	s.hub.EmitGlobal("loom/runtime-generation", map[string]any{"action": action, "generation": status, "succeeded": err == nil, "error": message})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": message, "generation": status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"generation": status})
}

func allowClaudeGenerationAdminRequest(w http.ResponseWriter, r *http.Request) bool {
	if allowAdminRequest(r) {
		return true
	}
	writeErr(w, &hub.HubError{Status: http.StatusForbidden, Message: "Claude Runtime generation changes are only allowed from localhost unless CODEX_LOOM_ADMIN_TOKEN is configured"})
	return false
}

func dailyActivityWindowFromRequest(r *http.Request, now time.Time) (time.Time, time.Time, int, error) {
	location := time.Local
	if timezone := strings.TrimSpace(r.URL.Query().Get("tz")); timezone != "" {
		var err error
		location, err = time.LoadLocation(timezone)
		if err != nil {
			return time.Time{}, time.Time{}, 0, fmt.Errorf("invalid timezone %q", timezone)
		}
	}
	date := strings.TrimSpace(r.URL.Query().Get("date"))
	if date == "" {
		date = now.In(location).Format("2006-01-02")
	}
	start, err := time.ParseInLocation("2006-01-02", date, location)
	if err != nil {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("invalid date %q", date)
	}
	today := now.In(location)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, location)
	if start.After(today) {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("date must not be in the future")
	}
	bucketMinutes := 30
	if value := strings.TrimSpace(r.URL.Query().Get("bucket")); value != "" {
		bucketMinutes, err = strconv.Atoi(value)
		if err != nil {
			return time.Time{}, time.Time{}, 0, fmt.Errorf("invalid bucket %q", value)
		}
	}
	if bucketMinutes != 15 && bucketMinutes != 30 && bucketMinutes != 60 {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("bucket must be 15, 30, or 60 minutes")
	}
	return start, start.AddDate(0, 0, 1), bucketMinutes, nil
}
