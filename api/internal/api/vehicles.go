package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (s *Server) handleListVehicles(w http.ResponseWriter, r *http.Request) {
	vehicles, err := s.store.ListVehicles(r.Context())
	if err != nil {
		s.logger.Error("list vehicles", "err", err)
		writeError(w, http.StatusInternalServerError, "could not list vehicles")
		return
	}
	writeJSON(w, http.StatusOK, vehicles)
}

// defaultTripWindow bounds an unqualified trips query. Without it, the first
// request against a year of data would try to serialise all of it.
const defaultTripWindow = 24 * time.Hour

func (s *Server) handleVehicleTrips(w http.ResponseWriter, r *http.Request) {
	vehicleID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "vehicle id must be a UUID")
		return
	}

	to := time.Now().UTC()
	from := to.Add(-defaultTripWindow)

	// RFC 3339 both ways. Rejecting a bad value rather than silently falling
	// back to the default: a caller who mistyped a timestamp should find out,
	// not receive plausible-looking data for the wrong window.
	if v := r.URL.Query().Get("from"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "from must be an RFC 3339 timestamp")
			return
		}
		from = parsed.UTC()
	}
	if v := r.URL.Query().Get("to"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "to must be an RFC 3339 timestamp")
			return
		}
		to = parsed.UTC()
	}
	if !from.Before(to) {
		writeError(w, http.StatusBadRequest, "from must be earlier than to")
		return
	}

	trips, err := s.store.ListTripsForVehicle(r.Context(), vehicleID, from, to)
	if err != nil {
		s.logger.Error("list trips", "vehicle_id", vehicleID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not list trips")
		return
	}
	writeJSON(w, http.StatusOK, trips)
}
