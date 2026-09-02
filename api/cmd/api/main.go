// Command api is the fleet telemetry ingestion and query service.
//
// Usage:
//
//	api                  serve HTTP
//	api migrate up       apply all pending migrations
//	api migrate down     roll back exactly one migration
//	api migrate status   list applied and pending migrations
//	api derive           recompute trips, stops, and reconciliation
//
// Migrations live in the same binary as the server so that a deploy and its
// schema change ship as one artifact.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/api"
	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/db"
	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/derive"
	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/ingest"
	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/store"
	"github.com/KaiTseHuang780911/fleet-telemetry/internal/config"
)

func main() {
	// slog is the standard library's structured logger (Go 1.21+). No logging
	// dependency is needed — this is deliberate, see CLAUDE.md.
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	// Local convenience only — real environment variables take precedence, and
	// a missing .env is not an error.
	if err := config.LoadDotEnv(".env"); err != nil {
		logger.Warn("could not read .env", "err", err)
	}

	if err := dispatch(logger); err != nil {
		logger.Error("exited with error", "err", err)
		os.Exit(1)
	}
}

// dispatch routes subcommands. Hand-rolled rather than using the flag package
// or a CLI library: there are three subcommands and no flags, so anything more
// would be ceremony.
func dispatch(logger *slog.Logger) error {
	if len(os.Args) < 2 {
		return run(logger)
	}

	switch os.Args[1] {
	case "migrate":
		ctx := context.Background()
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			return errors.New("DATABASE_URL is not set")
		}

		action := "up"
		if len(os.Args) > 2 {
			action = os.Args[2]
		}
		switch action {
		case "up":
			return db.Migrate(ctx, dsn, logger)
		case "down":
			return db.MigrateDown(ctx, dsn, logger)
		case "status":
			return db.MigrateStatus(ctx, dsn, logger)
		default:
			return fmt.Errorf("unknown migrate action %q: want up, down, or status", action)
		}
	case "derive":
		return runDerive(logger)

	default:
		return fmt.Errorf("unknown command %q: want migrate or derive, or no argument to serve", os.Args[1])
	}
}

// runDerive recomputes derived data over a window and exits.
//
// A subcommand rather than only a background loop, because derivation is a
// scheduled job in any real deployment: every API replica running its own copy
// would be duplicated work at best and contention at worst. As a subcommand it
// is a cron entry, a Fly.io scheduled machine, or a CI step, and it can be
// pointed at a chosen window to backfill.
func runDerive(logger *slog.Logger) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}

	ctx := context.Background()
	st, err := store.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	lookback := envDuration("DERIVE_LOOKBACK_MS", 24*time.Hour)
	from, to := derive.WindowFor(time.Now().UTC(), lookback)

	runner := derive.NewRunner(st, deriveConfig(), logger)
	sum, err := runner.Run(ctx, from, to)
	if err != nil {
		return err
	}

	logger.Info("derivation complete",
		"from", from.Format(time.RFC3339),
		"to", to.Format(time.RFC3339),
		"vehicles", sum.Vehicles,
		"trips", sum.Trips,
		"derived_stops", sum.DerivedStops,
		"client_stops", sum.ClientStops,
		"matched", sum.Matched,
		"client_only", sum.ClientOnly,
		"derived_only", sum.DerivedOnly,
		"duration_ms", sum.Duration.Milliseconds())
	return nil
}

// deriveConfig reads the detection thresholds from the environment, falling
// back to the documented defaults. See ADR-004 for why each exists and why the
// defaults are guesses rather than measurements.
func deriveConfig() derive.Config {
	cfg := derive.DefaultConfig()
	cfg.StopRadiusM = envFloat("DERIVE_STOP_RADIUS_M", cfg.StopRadiusM)
	cfg.StopMinDuration = envDuration("DERIVE_STOP_MIN_MS", cfg.StopMinDuration)
	cfg.TripGapDuration = envDuration("DERIVE_TRIP_GAP_MS", cfg.TripGapDuration)
	cfg.MatchMaxTimeDelta = envDuration("DERIVE_MATCH_MAX_DELTA_MS", cfg.MatchMaxTimeDelta)
	cfg.MatchMaxDistance = envFloat("DERIVE_MATCH_MAX_DISTANCE_M", cfg.MatchMaxDistance)
	return cfg
}

// run holds the real logic so that every failure path can return an error
// rather than calling os.Exit. Go idiom: main() stays trivial, which keeps the
// body testable and guarantees deferred cleanup actually runs — os.Exit skips
// deferred functions entirely.
func run(logger *slog.Logger) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}

	st, err := store.New(context.Background(), dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	writer := ingest.New(st, ingest.Config{
		BufferSize:    envInt("INGEST_BUFFER_SIZE", 1024),
		BatchSize:     envInt("INGEST_BATCH_SIZE", 500),
		FlushInterval: envDuration("INGEST_FLUSH_MS", 250*time.Millisecond),
		FlushTimeout:  envDuration("INGEST_FLUSH_TIMEOUT_MS", 10*time.Second),
	}, logger)

	// The writer's own context is independent of any request context, so that
	// cancelling in-flight requests never interrupts a flush mid-write.
	writerCtx, stopWriter := context.WithCancel(context.Background())
	defer stopWriter()
	go writer.Run(writerCtx)

	// Opt-in in-process derivation. Off unless DERIVE_INTERVAL_MS is set,
	// because in a multi-replica deployment every replica would run the same
	// pass. It exists so `npm run dev` shows live trips and reconciliation
	// without a second terminal or a scheduler.
	if interval := envDuration("DERIVE_INTERVAL_MS", 0); interval > 0 {
		lookback := envDuration("DERIVE_LOOKBACK_MS", 24*time.Hour)
		logger.Info("in-process derivation enabled",
			"interval_ms", interval.Milliseconds(), "lookback_ms", lookback.Milliseconds())
		go derive.NewRunner(st, deriveConfig(), logger).RunPeriodically(writerCtx, interval, lookback)
	}

	addr := ":" + envOr("PORT", "8080")
	srv := &http.Server{
		Addr:    addr,
		Handler: api.NewServer(st, writer, logger).Routes(),
		// Without these, a slow or hostile client can hold a connection open
		// indefinitely. net/http has no default timeouts, which is the single
		// most common way a Go service falls over in production.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Serve in a goroutine so main can block on the shutdown signal instead.
	// The buffered channel matters: if ListenAndServe fails before anyone
	// reads from it, an unbuffered send would block that goroutine forever.
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr)
		serveErr <- srv.ListenAndServe()
	}()

	// signal.NotifyContext gives a context that cancels on SIGINT/SIGTERM —
	// the tidiest way to plumb "we are shutting down" through to everything
	// that already takes a context.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serveErr:
		// ErrServerClosed is the expected result of a graceful Shutdown, so it
		// is not a failure.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(),
		envDuration("SHUTDOWN_TIMEOUT_MS", 10*time.Second))
	defer cancel()

	// Order matters. Stop accepting HTTP first so nothing new enters the
	// buffer, then drain what is already in it. Draining first would race
	// against requests still arriving.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", "err", err)
	}
	if err := writer.Shutdown(shutdownCtx); err != nil {
		logger.Error("ingest drain did not finish", "err", err)
	}

	logger.Info("shutdown complete", "ingest", writer.Stats())
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// envDuration reads a value expressed in milliseconds. The _MS suffix on the
// variable names is the contract; storing a bare number keeps the .env readable
// without teaching every consumer Go's duration syntax.
func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	ms, err := strconv.Atoi(v)
	// Negative is nonsense, but zero is meaningful: it is how the derivation
	// ticker is switched off explicitly rather than by omission.
	if err != nil || ms < 0 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return fallback
	}
	return f
}
