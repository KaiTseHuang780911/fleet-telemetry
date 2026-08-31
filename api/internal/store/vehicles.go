package store

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

type vehicleCache struct {
	mu sync.RWMutex
	m  map[string]uuid.UUID
}

func newVehicleCache() *vehicleCache {
	return &vehicleCache{m: make(map[string]uuid.UUID)}
}

// VehicleIDForDevice maps a device's external id to our internal vehicle id,
// registering the vehicle if it has not been seen before.
//
// Auto-registration is a development convenience: it lets the simulator and a
// freshly installed app post data with no setup step. A real deployment would
// replace it with device authentication, where an unknown device is rejected
// rather than silently enrolled — see ADR-003. Leaving it in without saying so
// would be the actual mistake.
func (s *Store) VehicleIDForDevice(ctx context.Context, externalID string) (uuid.UUID, error) {
	s.vehicles.mu.RLock()
	id, ok := s.vehicles.m[externalID]
	s.vehicles.mu.RUnlock()
	if ok {
		return id, nil
	}

	// ON CONFLICT DO NOTHING returns no row when the row already exists, which
	// would make this a two-round-trip dance. A no-op DO UPDATE always produces
	// a row, so RETURNING gives us the id whether we inserted or collided —
	// this is the standard Postgres idiom for "upsert and tell me the id".
	err := s.pool.QueryRow(ctx, `
		INSERT INTO vehicles (external_id, label)
		VALUES ($1, $1)
		ON CONFLICT (external_id)
		DO UPDATE SET external_id = EXCLUDED.external_id
		RETURNING id
	`, externalID).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve vehicle %q: %w", externalID, err)
	}

	s.vehicles.mu.Lock()
	s.vehicles.m[externalID] = id
	s.vehicles.mu.Unlock()

	return id, nil
}
