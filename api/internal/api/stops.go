package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/store"
)

// handleVehicleStops returns a vehicle's stop events.
//
// ?source=client or ?source=derived narrows to one side; omitting it returns
// both. Returning both by default is deliberate — a caller who does not know
// the two sources exist will see the duplication immediately rather than
// silently reading only half the picture and wondering why the counts look odd.
func (s *Server) handleVehicleStops(w http.ResponseWriter, r *http.Request) {
	vehicleID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "vehicle id must be a UUID")
		return
	}

	from, to, ok := parseWindow(w, r)
	if !ok {
		return
	}

	source := r.URL.Query().Get("source")
	switch source {
	case "", store.SourceClient, store.SourceDerived:
	default:
		writeError(w, http.StatusBadRequest, `source must be "client" or "derived"`)
		return
	}

	stops, err := s.store.ListStopEvents(r.Context(), vehicleID, from, to, source)
	if err != nil {
		s.logger.Error("list stop events", "vehicle_id", vehicleID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not list stop events")
		return
	}
	writeJSON(w, http.StatusOK, stops)
}

// handleReconciliation reports how far the device's own stop detection and the
// server's derivation disagree over a window.
//
// This is the number ADR-002 exists to produce. "How often does on-device
// detection disagree with the server, and by how much" is answerable with a
// figure rather than an opinion.
func (s *Server) handleReconciliation(w http.ResponseWriter, r *http.Request) {
	from, to, ok := parseWindow(w, r)
	if !ok {
		return
	}

	summary, err := s.store.SummariseReconciliation(r.Context(), from, to)
	if err != nil {
		s.logger.Error("summarise reconciliation", "err", err)
		writeError(w, http.StatusInternalServerError, "could not summarise reconciliation")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// defaultWindow bounds an unqualified query. Without it, the first request
// against a year of data would try to serialise all of it.
const defaultWindow = 24 * time.Hour

// parseWindow reads ?from= and ?to= as RFC 3339, defaulting to the last 24
// hours. It writes the error response itself and reports false when the
// caller should stop.
//
// A bad timestamp is rejected rather than silently falling back to the default:
// a caller who mistyped a date should find out, not receive plausible-looking
// data for a window they did not ask for.
func parseWindow(w http.ResponseWriter, r *http.Request) (from, to time.Time, ok bool) {
	to = time.Now().UTC()
	from = to.Add(-defaultWindow)

	if v := r.URL.Query().Get("from"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "from must be an RFC 3339 timestamp")
			return from, to, false
		}
		from = parsed.UTC()
	}
	if v := r.URL.Query().Get("to"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "to must be an RFC 3339 timestamp")
			return from, to, false
		}
		to = parsed.UTC()
	}
	if !from.Before(to) {
		writeError(w, http.StatusBadRequest, "from must be earlier than to")
		return from, to, false
	}
	return from, to, true
}
