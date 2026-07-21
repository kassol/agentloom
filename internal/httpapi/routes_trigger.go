package httpapi

import (
	"net/http"
	"strings"

	"github.com/yan5xu/codex-loom/internal/hub"
)

func (s *Server) registerTriggerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/triggers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"triggers": s.hub.ListTriggers(r.URL.Query().Get("agent"), r.URL.Query().Get("state"))})
	})
	mux.HandleFunc("POST /api/triggers", func(w http.ResponseWriter, r *http.Request) {
		if s.isRestartPending() {
			writeErr(w, &hub.HubError{Status: 409, Message: "restart pending; wait for Loom to restart before creating Triggers"})
			return
		}
		var body hub.TriggerParams
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		trigger, err := s.hub.CreateTrigger(body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"trigger": trigger})
	})
	mux.HandleFunc("GET /api/triggers/sources", func(w http.ResponseWriter, r *http.Request) {
		connections := []hub.PlatformConnection{}
		for _, connection := range s.hub.ListConnections() {
			if connection.ArchivedAt != "" {
				continue
			}
			for _, capability := range connection.Capabilities {
				if strings.HasPrefix(capability, "trigger:") {
					connections = append(connections, connection)
					break
				}
			}
		}
		writeJSON(w, 200, map[string]any{"connections": connections})
	})
	mux.HandleFunc("POST /api/triggers/poll", func(w http.ResponseWriter, r *http.Request) {
		s.hub.PollTriggersNow()
		writeJSON(w, 200, map[string]any{"triggers": s.hub.ListTriggers("", "")})
	})
	mux.HandleFunc("GET /api/triggers/{id}", func(w http.ResponseWriter, r *http.Request) {
		trigger, err := s.hub.GetTrigger(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"trigger": trigger})
	})
	for action, operation := range map[string]func(string) (hub.Trigger, error){
		"pause": s.hub.PauseTrigger, "resume": s.hub.ResumeTrigger, "cancel": s.hub.CancelTrigger,
	} {
		action, operation := action, operation
		mux.HandleFunc("POST /api/triggers/{id}/"+action, func(w http.ResponseWriter, r *http.Request) {
			if action == "resume" && s.isRestartPending() {
				writeErr(w, &hub.HubError{Status: 409, Message: "restart pending; wait for Loom to restart before resuming Triggers"})
				return
			}
			trigger, err := operation(r.PathValue("id"))
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, 200, map[string]any{"trigger": trigger})
		})
	}
}
