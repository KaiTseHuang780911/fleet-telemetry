// Package db owns the database connection and schema migrations.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/pressly/goose/v3"

	// Registers the "pgx" driver with database/sql. goose speaks database/sql,
	// whereas the rest of the service uses pgx's native interface directly —
	// the native one is faster and exposes CopyFrom, which the batch writer
	// will want. This blank import bridges the two for migrations only.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kaitsehuang780911/fleet-telemetry/api/migrations"
)

// Migrate applies all pending migrations. It is safe to run concurrently from
// multiple instances: goose takes a lock on its version table, so a rolling
// deploy where several containers start at once will not race.
func Migrate(ctx context.Context, dsn string, logger *slog.Logger) error {
	provider, cleanup, err := newProvider(dsn)
	if err != nil {
		return err
	}
	defer cleanup()

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	if len(results) == 0 {
		logger.Info("no pending migrations")
		return nil
	}
	for _, r := range results {
		logger.Info("applied migration", "version", r.Source.Version, "name", r.Source.Path, "duration", r.Duration)
	}
	return nil
}

// MigrateDown rolls back exactly one migration. Deliberately one at a time —
// an unbounded "roll everything back" is a destructive operation that should
// take deliberate effort to invoke.
func MigrateDown(ctx context.Context, dsn string, logger *slog.Logger) error {
	provider, cleanup, err := newProvider(dsn)
	if err != nil {
		return err
	}
	defer cleanup()

	result, err := provider.Down(ctx)
	if err != nil {
		return fmt.Errorf("roll back migration: %w", err)
	}
	logger.Info("rolled back migration", "version", result.Source.Version, "name", result.Source.Path)
	return nil
}

// MigrateStatus reports which migrations are applied and which are pending.
func MigrateStatus(ctx context.Context, dsn string, logger *slog.Logger) error {
	provider, cleanup, err := newProvider(dsn)
	if err != nil {
		return err
	}
	defer cleanup()

	status, err := provider.Status(ctx)
	if err != nil {
		return fmt.Errorf("read migration status: %w", err)
	}
	for _, s := range status {
		applied := "pending"
		if s.State == goose.StateApplied {
			applied = s.AppliedAt.Format("2006-01-02 15:04:05")
		}
		logger.Info("migration", "version", s.Source.Version, "name", s.Source.Path, "state", applied)
	}
	return nil
}

func newProvider(dsn string) (*goose.Provider, func(), error) {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("create migration provider: %w", err)
	}

	return provider, func() { _ = sqlDB.Close() }, nil
}
