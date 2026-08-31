package api

import (
	"context"
	"net/http"
	"time"
)

// handleHealthz is liveness: is this process up and serving?
//
// It deliberately does not touch the database. Conflating liveness with
// dependency health means a slow database gets the process killed and
// restarted, which does nothing for the database and drops whatever the ingest
// writer was holding.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz is readiness: can this process actually serve traffic right now?
//
// This one does check the database, because a process that cannot reach
// Postgres should be taken out of the load balancer rotation — but not
// restarted.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		s.logger.Warn("readiness check failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable",
			"reason": "database unreachable",
		})
		return
	}

	stats := s.writer.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ready",
		"ingest": stats,
	})
}
