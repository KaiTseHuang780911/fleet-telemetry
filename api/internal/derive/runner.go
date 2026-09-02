package derive

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/store"
)

// Store is the slice of persistence the runner needs. Declared here, in the
// consumer, so the test suite can satisfy it without a database.
type Store interface {
	VehiclesWithPositionsIn(ctx context.Context, from, to time.Time) ([]uuid.UUID, error)
	SamplesForDerivation(ctx context.Context, vehicleID uuid.UUID, from, to time.Time) ([]store.Sample, error)
	ListStopEvents(ctx context.Context, vehicleID uuid.UUID, from, to time.Time, source string) ([]store.StopEvent, error)
	ReplaceDerived(ctx context.Context, vehicleID uuid.UUID, from, to time.Time, trips []store.Trip, stops []store.StopEvent) error
	ReplaceStopEventMatches(ctx context.Context, vehicleID uuid.UUID, from, to time.Time, matches []store.StopEventMatch) error
}

// Runner recomputes derived data over a window.
type Runner struct {
	store  Store
	cfg    Config
	logger *slog.Logger
}

func NewRunner(s Store, cfg Config, logger *slog.Logger) *Runner {
	return &Runner{store: s, cfg: cfg, logger: logger}
}

// RunSummary reports what one pass did.
type RunSummary struct {
	Vehicles     int
	Trips        int
	DerivedStops int
	ClientStops  int
	Matched      int
	ClientOnly   int
	DerivedOnly  int
	Duration     time.Duration
}

// Run recomputes trips, stops, and reconciliation for every vehicle with
// positions in [from, to).
//
// A failure on one vehicle is logged and the pass continues. One device with
// corrupt data must not stop the rest of the fleet being processed — and since
// each vehicle's write is its own transaction, a partial pass leaves consistent
// data rather than a half-applied mess.
func (r *Runner) Run(ctx context.Context, from, to time.Time) (RunSummary, error) {
	started := time.Now()
	var sum RunSummary

	vehicles, err := r.store.VehiclesWithPositionsIn(ctx, from, to)
	if err != nil {
		return sum, fmt.Errorf("list vehicles: %w", err)
	}
	sum.Vehicles = len(vehicles)

	for _, vehicleID := range vehicles {
		if err := ctx.Err(); err != nil {
			return sum, err
		}

		vs, err := r.runVehicle(ctx, vehicleID, from, to)
		if err != nil {
			r.logger.Error("derivation failed for vehicle", "vehicle_id", vehicleID, "err", err)
			continue
		}

		sum.Trips += vs.Trips
		sum.DerivedStops += vs.DerivedStops
		sum.ClientStops += vs.ClientStops
		sum.Matched += vs.Matched
		sum.ClientOnly += vs.ClientOnly
		sum.DerivedOnly += vs.DerivedOnly
	}

	sum.Duration = time.Since(started)
	return sum, nil
}

func (r *Runner) runVehicle(ctx context.Context, vehicleID uuid.UUID, from, to time.Time) (RunSummary, error) {
	var out RunSummary

	samples, err := r.store.SamplesForDerivation(ctx, vehicleID, from, to)
	if err != nil {
		return out, fmt.Errorf("read samples: %w", err)
	}

	result := Detect(vehicleID, samples, r.cfg)

	if err := r.store.ReplaceDerived(ctx, vehicleID, from, to, result.Trips, result.Stops); err != nil {
		return out, fmt.Errorf("persist derived: %w", err)
	}

	clientStops, err := r.store.ListStopEvents(ctx, vehicleID, from, to, store.SourceClient)
	if err != nil {
		return out, fmt.Errorf("read client stops: %w", err)
	}

	matches := Reconcile(clientStops, result.Stops, r.cfg)
	if err := r.store.ReplaceStopEventMatches(ctx, vehicleID, from, to, matches); err != nil {
		return out, fmt.Errorf("persist matches: %w", err)
	}

	agreement := Summarise(clientStops, result.Stops, matches)

	out.Trips = len(result.Trips)
	out.DerivedStops = len(result.Stops)
	out.ClientStops = agreement.ClientStops
	out.Matched = agreement.Matched
	out.ClientOnly = agreement.ClientOnly
	out.DerivedOnly = agreement.DerivedOnly

	r.logger.Debug("derived vehicle",
		"vehicle_id", vehicleID,
		"samples", len(samples),
		"trips", out.Trips,
		"derived_stops", out.DerivedStops,
		"client_stops", out.ClientStops,
		"matched", out.Matched,
		"mean_abs_delta_s", agreement.MeanAbsDeltaSeconds,
		"mean_signed_delta_s", agreement.MeanSignedDeltaSeconds,
		"mean_delta_m", agreement.MeanDeltaMeters)

	return out, nil
}

// RunPeriodically re-derives on an interval until ctx is cancelled.
//
// Opt-in, via DERIVE_INTERVAL_MS. Off by default because derivation belongs in
// a scheduler in any real deployment — running it inside the API process means
// every replica does the same work. It exists so that `npm run dev` shows live
// trips without a second terminal.
func (r *Runner) RunPeriodically(ctx context.Context, interval, lookback time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			from, to := WindowFor(now.UTC(), lookback)
			sum, err := r.Run(ctx, from, to)
			if err != nil {
				r.logger.Error("periodic derivation failed", "err", err)
				continue
			}
			if sum.Vehicles > 0 {
				r.logger.Info("derivation pass",
					"vehicles", sum.Vehicles,
					"trips", sum.Trips,
					"derived_stops", sum.DerivedStops,
					"client_stops", sum.ClientStops,
					"matched", sum.Matched,
					"client_only", sum.ClientOnly,
					"derived_only", sum.DerivedOnly,
					"duration_ms", sum.Duration.Milliseconds())
			}
		}
	}
}
