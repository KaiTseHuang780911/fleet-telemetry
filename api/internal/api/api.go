// Package api wires HTTP routes onto the ingest pipeline and the store.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/ingest"
	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/store"
)

// Enqueuer is the part of the ingest writer the HTTP layer depends on.
type Enqueuer interface {
	Enqueue(positions []store.Position) bool
	Stats() ingest.Stats
}

// Store is the read side the handlers need.
type Store interface {
	Ping(ctx context.Context) error
	ListVehicles(ctx context.Context) ([]store.Vehicle, error)
	ListTripsForVehicle(ctx context.Context, vehicleID uuid.UUID, from, to time.Time) ([]store.Trip, error)
	VehicleIDForDevice(ctx context.Context, externalID string) (uuid.UUID, error)
}

// Server holds handler dependencies.
type Server struct {
	store  Store
	writer Enqueuer
	logger *slog.Logger
}

func NewServer(s Store, w Enqueuer, logger *slog.Logger) *Server {
	return &Server{store: s, writer: w, logger: logger}
}

// Routes builds the router.
//
// chi arrives here rather than in Phase 0 because this is the first route with
// a path parameter — /v1/vehicles/{id}/trips. Until then net/http's ServeMux
// was genuinely sufficient.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	// A panic in one handler must not take the process down and lose every
	// batch currently buffered in the writer. CLAUDE.md forbids panics in
	// request paths; this is the belt to that braces.
	r.Use(middleware.Recoverer)
	r.Use(s.requestLogger)

	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	r.Route("/v1", func(r chi.Router) {
		r.Post("/telemetry", s.handleTelemetry)
		r.Get("/vehicles", s.handleListVehicles)
		r.Get("/vehicles/{id}/trips", s.handleVehicleTrips)
	})

	return r
}

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		// Ingest is high-frequency; logging every accepted batch at info level
		// would drown everything else. Successful telemetry posts log at debug,
		// everything else at info.
		level := slog.LevelInfo
		if r.URL.Path == "/v1/telemetry" && ww.Status() < 400 {
			level = slog.LevelDebug
		}
		s.logger.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
		)
	})
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status and headers are already committed by the time encoding can
	// fail, so there is nothing useful left to say to the client. Swallowing it
	// here rather than pretending otherwise.
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}
