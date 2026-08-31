// Package store owns all SQL. Nothing outside this package writes queries.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is a handle on the database.
type Store struct {
	pool *pgxpool.Pool

	// Vehicle ids are looked up by external_id on every batch. There are few
	// vehicles, they are never deleted, and their ids never change, so the
	// mapping is cached rather than hit for every request.
	vehicles *vehicleCache
}

// New opens a connection pool and verifies it can actually reach the database.
// pgxpool.New alone is lazy — it does not connect until first use — so a bad
// DSN would otherwise surface as a confusing failure on the first request
// rather than at startup.
func New(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	// The batch writer is a single goroutine, so the pool exists mainly to
	// serve read queries concurrently. Kept modest deliberately: Postgres
	// handles connections with processes, and an oversized pool moves the
	// bottleneck into the database rather than removing it.
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	return &Store{pool: pool, vehicles: newVehicleCache()}, nil
}

// Close releases the pool. Safe to call more than once.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Ping backs the readiness probe.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Vehicle is the API representation of a tracked vehicle.
type Vehicle struct {
	ID         uuid.UUID `json:"id"`
	ExternalID string    `json:"external_id"`
	Label      string    `json:"label"`
	CreatedAt  time.Time `json:"created_at"`
}

// ListVehicles returns every known vehicle, oldest first.
func (s *Store) ListVehicles(ctx context.Context) ([]Vehicle, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, external_id, label, created_at
		  FROM vehicles
		 ORDER BY created_at, external_id
	`)
	if err != nil {
		return nil, fmt.Errorf("query vehicles: %w", err)
	}
	defer rows.Close()

	// Non-nil empty slice so the handler encodes [] rather than null — a JSON
	// null where a client expects an array is a needless source of client bugs.
	out := make([]Vehicle, 0)
	for rows.Next() {
		var v Vehicle
		if err := rows.Scan(&v.ID, &v.ExternalID, &v.Label, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan vehicle: %w", err)
		}
		out = append(out, v)
	}
	// rows.Err reports failures that occurred mid-iteration; without this check
	// a truncated result set reads as a successful short one.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vehicles: %w", err)
	}
	return out, nil
}
