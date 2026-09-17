package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Source values for trips and stop_events. Both sources are stored and neither
// overwrites the other — see ADR-002.
const (
	SourceClient  = "client"
	SourceDerived = "derived"
)

// StopEvent is a period a vehicle spent stationary.
type StopEvent struct {
	ID         uuid.UUID  `json:"id"`
	VehicleID  uuid.UUID  `json:"vehicle_id"`
	TripID     *uuid.UUID `json:"trip_id"`
	Source     string     `json:"source"`
	ArrivedAt  time.Time  `json:"arrived_at"`
	DepartedAt *time.Time `json:"departed_at"`
	Lat        float64    `json:"lat"`
	Lon        float64    `json:"lon"`
}

// InsertClientStopEvents stores stop events a device reported about itself.
//
// Same shape and same reasoning as InsertPositions: arrays expanded with
// unnest() so the parameter count does not grow with the row count.
//
// A stop is reported twice under the same event id: once on arrival, with
// departed_at NULL, and again once the vehicle leaves. That is why this is an
// upsert and not the DO NOTHING it used to be — arrival is the event a fleet
// wants promptly, and waiting for departure to report it would leave a driver
// parked at a customer invisible for as long as they stayed there.
//
// The WHERE clause is the important part. It admits the completion of an open
// stop and refuses everything else, so a replay of the *open* version arriving
// late — which at-least-once delivery makes normal, not exceptional — cannot
// erase a departure that has already been recorded. Replays are otherwise
// no-ops, and arrived_at/lat/lon are never updated: if those disagree between
// the two reports it is the same id describing a different stop, which is a
// client bug rather than something to silently accept.
func (s *Store) InsertClientStopEvents(ctx context.Context, events []StopEvent) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}

	n := len(events)
	var (
		ids         = make([]uuid.UUID, n)
		vehicleIDs  = make([]uuid.UUID, n)
		arrivedAts  = make([]time.Time, n)
		departedAts = make([]*time.Time, n)
		lats        = make([]float64, n)
		lons        = make([]float64, n)
	)
	for i, e := range events {
		ids[i] = e.ID
		vehicleIDs[i] = e.VehicleID
		arrivedAts[i] = e.ArrivedAt
		departedAts[i] = e.DepartedAt
		lats[i] = e.Lat
		lons[i] = e.Lon
	}

	tag, err := s.pool.Exec(ctx, `
		INSERT INTO stop_events (id, vehicle_id, source, arrived_at, departed_at, lat, lon)
		SELECT id, vehicle_id, 'client', arrived_at, departed_at, lat, lon
		  FROM unnest($1::uuid[], $2::uuid[], $3::timestamptz[], $4::timestamptz[],
		              $5::float8[], $6::float8[])
		    AS t(id, vehicle_id, arrived_at, departed_at, lat, lon)
		ON CONFLICT (id) DO UPDATE
		   SET departed_at = EXCLUDED.departed_at
		 WHERE stop_events.departed_at IS NULL
		   AND EXCLUDED.departed_at IS NOT NULL
	`, ids, vehicleIDs, arrivedAts, departedAts, lats, lons)
	if err != nil {
		return 0, fmt.Errorf("insert %d client stop events: %w", n, err)
	}
	return int(tag.RowsAffected()), nil
}

// ListStopEvents returns stop events for a vehicle overlapping [from, to),
// optionally filtered to one source. An empty source returns both.
//
// Overlap rather than containment, and an open stop (departed_at NULL) is
// treated as extending to now — the same reasoning as ListTripsForVehicle.
func (s *Store) ListStopEvents(
	ctx context.Context, vehicleID uuid.UUID, from, to time.Time, source string,
) ([]StopEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, vehicle_id, trip_id, source, arrived_at, departed_at, lat, lon
		  FROM stop_events
		 WHERE vehicle_id = $1
		   AND arrived_at < $3
		   AND coalesce(departed_at, now()) >= $2
		   AND ($4 = '' OR source = $4)
		 ORDER BY arrived_at
	`, vehicleID, from, to, source)
	if err != nil {
		return nil, fmt.Errorf("query stop events: %w", err)
	}
	defer rows.Close()

	out := make([]StopEvent, 0)
	for rows.Next() {
		var e StopEvent
		if err := rows.Scan(&e.ID, &e.VehicleID, &e.TripID, &e.Source,
			&e.ArrivedAt, &e.DepartedAt, &e.Lat, &e.Lon); err != nil {
			return nil, fmt.Errorf("scan stop event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stop events: %w", err)
	}
	return out, nil
}
