package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Trip is a period of movement for one vehicle.
//
// Source distinguishes a device-reported trip from a server-derived one. Both
// are stored and neither overwrites the other — see ADR-002.
type Trip struct {
	ID         uuid.UUID  `json:"id"`
	VehicleID  uuid.UUID  `json:"vehicle_id"`
	Source     string     `json:"source"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	DistanceM  float64    `json:"distance_m"`
	ReceivedAt time.Time  `json:"received_at"`
}

// ListTripsForVehicle returns trips overlapping [from, to).
//
// Overlap, not containment: a trip that began before the window and is still
// running belongs in the result. Filtering on started_at alone would hide
// exactly the trip a "what is happening right now" query is looking for.
//
// An open trip has ended_at NULL, which is treated as "extends to now".
func (s *Store) ListTripsForVehicle(ctx context.Context, vehicleID uuid.UUID, from, to time.Time) ([]Trip, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, vehicle_id, source, started_at, ended_at, distance_m, received_at
		  FROM trips
		 WHERE vehicle_id = $1
		   AND started_at < $3
		   AND coalesce(ended_at, now()) >= $2
		 ORDER BY started_at DESC
	`, vehicleID, from, to)
	if err != nil {
		return nil, fmt.Errorf("query trips: %w", err)
	}
	defer rows.Close()

	out := make([]Trip, 0)
	for rows.Next() {
		var t Trip
		if err := rows.Scan(&t.ID, &t.VehicleID, &t.Source, &t.StartedAt,
			&t.EndedAt, &t.DistanceM, &t.ReceivedAt); err != nil {
			return nil, fmt.Errorf("scan trip: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trips: %w", err)
	}
	return out, nil
}
