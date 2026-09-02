package api

import (
	"net/http"

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

func (s *Server) handleVehicleTrips(w http.ResponseWriter, r *http.Request) {
	vehicleID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "vehicle id must be a UUID")
		return
	}

	from, to, ok := parseWindow(w, r)
	if !ok {
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
