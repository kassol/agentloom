package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/yan5xu/codex-loom/internal/hub"
)

func (s *Server) registerContextRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/context/agent-prompt", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"prompt": s.hub.GetLoomAgentPrompt()})
	})
	mux.HandleFunc("PUT /api/context/agent-prompt", func(w http.ResponseWriter, r *http.Request) {
		var body hub.LoomAgentPromptParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		prompt, err := s.hub.UpdateLoomAgentPrompt(body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"prompt": prompt})
	})
	mux.HandleFunc("DELETE /api/context/agent-prompt", func(w http.ResponseWriter, r *http.Request) {
		var expectedVersion *int
		if value := strings.TrimSpace(r.URL.Query().Get("expectedVersion")); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "expectedVersion must be a non-negative integer"})
				return
			}
			expectedVersion = &parsed
		}
		prompt, err := s.hub.ClearLoomAgentPrompt(expectedVersion)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"prompt": prompt})
	})
	mux.HandleFunc("GET /api/agents/{key}/context/explain", func(w http.ResponseWriter, r *http.Request) {
		view, err := s.hub.ExplainContext(r.PathValue("key"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"context": view})
	})
	mux.HandleFunc("GET /api/agents/{key}/context/coverage", func(w http.ResponseWriter, r *http.Request) {
		ledger, err := s.hub.ContextCoverage(r.PathValue("key"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"coverage": ledger})
	})
}
