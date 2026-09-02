package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Sample is one position reduced to what derivation actually needs.
type Sample struct {
	RecordedAt time.Time
	Lat        float64
	Lon        float64
	SpeedMPS   *float32
}

// StopEventMatch records that a client-reported stop and a server-derived stop
// are believed to describe the same real-world event, and how far apart the two
// sources were.
type StopEventMatch struct {
	ClientEventID  uuid.UUID
	DerivedEventID uuid.UUID
	DeltaSeconds   int
	DeltaMeters    float64
}

// VehiclesWithPositionsIn returns the vehicles that have any position in the
// window, so derivation only walks vehicles that could have changed.
func (s *Store) VehiclesWithPositionsIn(ctx context.Context, from, to time.Time) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT vehicle_id
		  FROM positions
		 WHERE recorded_at >= $1 AND recorded_at < $2
	`, from, to)
	if err != nil {
		return nil, fmt.Errorf("query vehicles with positions: %w", err)
	}
	defer rows.Close()

	out := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan vehicle id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vehicle ids: %w", err)
	}
	return out, nil
}

// SamplesForDerivation returns a vehicle's positions in the window, ordered by
// the device clock.
//
// Ordered by recorded_at, not received_at: derivation reconstructs what the
// vehicle did, and that happened in device-clock order. A drained backlog
// arrives in a received_at order that has nothing to do with the route. This is
// exactly why derivation recomputes a window instead of streaming.
func (s *Store) SamplesForDerivation(ctx context.Context, vehicleID uuid.UUID, from, to time.Time) ([]Sample, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT recorded_at, lat, lon, speed_mps
		  FROM positions
		 WHERE vehicle_id = $1
		   AND recorded_at >= $2 AND recorded_at < $3
		 ORDER BY recorded_at
	`, vehicleID, from, to)
	if err != nil {
		return nil, fmt.Errorf("query samples: %w", err)
	}
	defer rows.Close()

	out := make([]Sample, 0)
	for rows.Next() {
		var sm Sample
		if err := rows.Scan(&sm.RecordedAt, &sm.Lat, &sm.Lon, &sm.SpeedMPS); err != nil {
			return nil, fmt.Errorf("scan sample: %w", err)
		}
		out = append(out, sm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate samples: %w", err)
	}
	return out, nil
}

// ReplaceDerived swaps a vehicle's derived trips and stops for the window in a
// single transaction.
//
// Replace rather than upsert. Derivation output depends on the thresholds it
// ran with, so retuning them shifts boundaries and changes how many events
// exist. Upserting by a content-derived id would strand rows from the previous
// tuning; replacing the window means the derived data always corresponds to one
// consistent run.
//
// Rows are matched by overlap with the window rather than containment, so a
// trip that began before the window and is still open gets recomputed instead
// of duplicated. Client-reported rows are never touched.
func (s *Store) ReplaceDerived(
	ctx context.Context, vehicleID uuid.UUID, from, to time.Time,
	trips []Trip, stops []StopEvent,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// Rollback after a successful Commit is a no-op, so this is safe as
	// unconditional cleanup and guarantees no transaction is left open on an
	// early return.
	defer func() { _ = tx.Rollback(ctx) }()

	// Stops first: stop_events.trip_id references trips.
	if _, err := tx.Exec(ctx, `
		DELETE FROM stop_events
		 WHERE vehicle_id = $1 AND source = 'derived'
		   AND arrived_at < $3
		   AND coalesce(departed_at, now()) >= $2
	`, vehicleID, from, to); err != nil {
		return fmt.Errorf("clear derived stops: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM trips
		 WHERE vehicle_id = $1 AND source = 'derived'
		   AND started_at < $3
		   AND coalesce(ended_at, now()) >= $2
	`, vehicleID, from, to); err != nil {
		return fmt.Errorf("clear derived trips: %w", err)
	}

	for _, t := range trips {
		if _, err := tx.Exec(ctx, `
			INSERT INTO trips (id, vehicle_id, source, started_at, ended_at, distance_m)
			VALUES ($1, $2, 'derived', $3, $4, $5)
		`, t.ID, vehicleID, t.StartedAt, t.EndedAt, t.DistanceM); err != nil {
			return fmt.Errorf("insert derived trip: %w", err)
		}
	}

	for _, e := range stops {
		if _, err := tx.Exec(ctx, `
			INSERT INTO stop_events (id, vehicle_id, trip_id, source, arrived_at, departed_at, lat, lon)
			VALUES ($1, $2, $3, 'derived', $4, $5, $6, $7)
		`, e.ID, vehicleID, e.TripID, e.ArrivedAt, e.DepartedAt, e.Lat, e.Lon); err != nil {
			return fmt.Errorf("insert derived stop: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ReplaceStopEventMatches swaps the reconciliation results for one vehicle's
// window. Matches are recomputed whenever either side changes, so the same
// replace-the-window reasoning applies.
func (s *Store) ReplaceStopEventMatches(
	ctx context.Context, vehicleID uuid.UUID, from, to time.Time, matches []StopEventMatch,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Scoped by joining through the client side of the match, so reconciling
	// one vehicle never disturbs another's rows.
	if _, err := tx.Exec(ctx, `
		DELETE FROM stop_event_matches m
		 USING stop_events e
		 WHERE m.client_event_id = e.id
		   AND e.vehicle_id = $1
		   AND e.arrived_at >= $2 AND e.arrived_at < $3
	`, vehicleID, from, to); err != nil {
		return fmt.Errorf("clear matches: %w", err)
	}

	if len(matches) > 0 {
		rows := make([][]any, len(matches))
		for i, m := range matches {
			rows[i] = []any{m.ClientEventID, m.DerivedEventID, m.DeltaSeconds, m.DeltaMeters}
		}
		// CopyFrom is usable here precisely because the delete above already
		// guaranteed there is nothing to conflict with. The positions path
		// cannot use it, because that one needs ON CONFLICT and COPY has no
		// equivalent.
		if _, err := tx.CopyFrom(ctx,
			pgx.Identifier{"stop_event_matches"},
			[]string{"client_event_id", "derived_event_id", "delta_seconds", "delta_meters"},
			pgx.CopyFromRows(rows),
		); err != nil {
			return fmt.Errorf("insert matches: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ReconciliationSummary counts how the two sources compared over a window.
// This is the number ADR-002 exists to produce.
type ReconciliationSummary struct {
	ClientStops     int     `json:"client_stops"`
	DerivedStops    int     `json:"derived_stops"`
	Matched         int     `json:"matched"`
	ClientOnly      int     `json:"client_only"`
	DerivedOnly     int     `json:"derived_only"`
	MeanDeltaSecs   float64 `json:"mean_delta_seconds"`
	MeanDeltaMeters float64 `json:"mean_delta_meters"`
}

// SummariseReconciliation reports agreement between the two sources across all
// vehicles in the window.
func (s *Store) SummariseReconciliation(ctx context.Context, from, to time.Time) (ReconciliationSummary, error) {
	var out ReconciliationSummary
	err := s.pool.QueryRow(ctx, `
		WITH windowed AS (
			SELECT id, source FROM stop_events
			 WHERE arrived_at >= $1 AND arrived_at < $2
		),
		matched AS (
			SELECT m.client_event_id, m.derived_event_id, m.delta_seconds, m.delta_meters
			  FROM stop_event_matches m
			  JOIN windowed w ON w.id = m.client_event_id
		)
		SELECT
			(SELECT count(*) FROM windowed WHERE source = 'client'),
			(SELECT count(*) FROM windowed WHERE source = 'derived'),
			(SELECT count(*) FROM matched),
			(SELECT count(*) FROM windowed w WHERE w.source = 'client'
			   AND NOT EXISTS (SELECT 1 FROM matched m WHERE m.client_event_id = w.id)),
			(SELECT count(*) FROM windowed w WHERE w.source = 'derived'
			   AND NOT EXISTS (SELECT 1 FROM matched m WHERE m.derived_event_id = w.id)),
			coalesce((SELECT avg(abs(delta_seconds)) FROM matched), 0),
			coalesce((SELECT avg(delta_meters) FROM matched), 0)
	`, from, to).Scan(
		&out.ClientStops, &out.DerivedStops, &out.Matched,
		&out.ClientOnly, &out.DerivedOnly,
		&out.MeanDeltaSecs, &out.MeanDeltaMeters,
	)
	if err != nil {
		return out, fmt.Errorf("summarise reconciliation: %w", err)
	}
	return out, nil
}
