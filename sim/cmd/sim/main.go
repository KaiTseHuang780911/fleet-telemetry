// Command sim generates plausible vehicle telemetry and posts it to the ingest
// API.
//
// Each vehicle runs in its own goroutine and maintains its own outbound queue,
// mirroring how real devices behave: independent, concurrent, and holding
// readings locally until the server confirms it has them. That last property is
// what lets the simulator exercise the server's backpressure path honestly —
// on a 503 it backs off and retries rather than dropping data, exactly as the
// mobile client's SQLite queue will.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/KaiTseHuang780911/fleet-telemetry/internal/config"
	"github.com/KaiTseHuang780911/fleet-telemetry/internal/wire"
	"github.com/KaiTseHuang780911/fleet-telemetry/sim/internal/trace"
)

type counters struct {
	posted    atomic.Int64
	accepted  atomic.Int64
	rejected  atomic.Int64
	shed      atomic.Int64 // 503 responses
	failed    atomic.Int64 // transport or 5xx other than 503
	queueHigh atomic.Int64 // deepest a vehicle's local queue ever got
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := config.LoadDotEnv(".env"); err != nil {
		logger.Warn("could not read .env", "err", err)
	}

	if err := run(logger); err != nil {
		logger.Error("simulator exited with error", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	var (
		apiURL    = envOr("SIM_API_URL", "http://localhost:8080")
		vehicles  = envInt("SIM_VEHICLES", 10)
		seed      = int64(envInt("SIM_SEED", 42))
		tick      = time.Duration(envInt("SIM_TICK_MS", 1000)) * time.Millisecond
		batchSize = envInt("SIM_BATCH_SIZE", 10)
	)

	logger.Info("starting simulator",
		"api", apiURL, "vehicles", vehicles, "seed", seed,
		"tick_ms", tick.Milliseconds(), "batch_size", batchSize)

	fleet := trace.NewFleet(trace.Config{Seed: seed, Vehicles: vehicles})

	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			// Every vehicle posts concurrently. The default of 2 idle
			// connections per host would serialise them behind a tiny pool and
			// make the simulator, rather than the server, the bottleneck.
			MaxIdleConns:        vehicles * 2,
			MaxIdleConnsPerHost: vehicles * 2,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		wg    sync.WaitGroup
		stats counters
	)

	for _, v := range fleet.Vehicles() {
		wg.Add(1)
		go func(v *trace.Vehicle) {
			defer wg.Done()
			runVehicle(ctx, v, client, apiURL, tick, batchSize, &stats, logger)
		}(v)
	}

	// Periodic summary so a long run is observable without reading every line.
	go reportPeriodically(ctx, &stats, logger)

	<-ctx.Done()
	logger.Info("shutdown signal received, stopping vehicles")
	wg.Wait()

	logger.Info("simulator stopped",
		"posted", stats.posted.Load(),
		"accepted", stats.accepted.Load(),
		"rejected", stats.rejected.Load(),
		"shed_503", stats.shed.Load(),
		"failed", stats.failed.Load(),
		"max_queue_depth", stats.queueHigh.Load())
	return nil
}

func runVehicle(
	ctx context.Context,
	v *trace.Vehicle,
	client *http.Client,
	apiURL string,
	tick time.Duration,
	batchSize int,
	stats *counters,
	logger *slog.Logger,
) {
	// Each vehicle's own generator, so backoff jitter stays deterministic per
	// vehicle and does not depend on scheduling order.
	rng := rand.New(rand.NewSource(int64(len(v.DeviceID)) + int64(v.DeviceID[len(v.DeviceID)-1])))

	// Stagger startup so ten vehicles do not post in lockstep forever after.
	select {
	case <-time.After(time.Duration(rng.Int63n(int64(tick)))):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	// The device's local queue. Readings stay here until the server accepts
	// them — this is the simulator's stand-in for the SQLite offline queue.
	queue := make([]wire.Reading, 0, batchSize*4)

	for {
		select {
		case <-ctx.Done():
			// Deliberately does not attempt a final flush. A device losing
			// power does not get to drain its queue either, and pretending
			// otherwise would hide exactly the data-loss question the offline
			// queue exists to answer.
			return

		case now := <-ticker.C:
			queue = append(queue, v.Tick(now.UTC(), tick))

			if depth := int64(len(queue)); depth > stats.queueHigh.Load() {
				stats.queueHigh.Store(depth)
			}
			if len(queue) < batchSize {
				continue
			}

			accepted, rejected, retryAfter, err := post(ctx, client, apiURL, v.DeviceID, queue)
			switch {
			case err != nil:
				stats.failed.Add(1)
				logger.Warn("post failed, will retry", "device", v.DeviceID, "queued", len(queue), "err", err)
				// Readings stay queued.

			case retryAfter > 0:
				// Server shed load. Hold everything and back off — with jitter,
				// so that every vehicle does not return at the same instant and
				// recreate the overload.
				stats.shed.Add(1)
				jitter := time.Duration(rng.Int63n(int64(retryAfter / 2)))
				select {
				case <-time.After(retryAfter + jitter):
				case <-ctx.Done():
					return
				}

			default:
				stats.posted.Add(1)
				stats.accepted.Add(int64(accepted))
				stats.rejected.Add(int64(len(rejected)))
				if len(rejected) > 0 {
					// The server named readings it will never accept. Dropping
					// them is the whole point of partial acceptance: keeping
					// them would block this queue forever.
					logger.Warn("server rejected readings, dropping them",
						"device", v.DeviceID, "count", len(rejected), "first_reason", rejected[0].Reason)
				}
				queue = queue[:0]
			}
		}
	}
}

// post sends a batch and reports what happened. A non-zero retryAfter means the
// server shed load and the batch must be retried.
func post(
	ctx context.Context,
	client *http.Client,
	apiURL, deviceID string,
	readings []wire.Reading,
) (accepted int, rejected []wire.Rejection, retryAfter time.Duration, err error) {
	body, err := json.Marshal(wire.Batch{
		DeviceID: deviceID,
		SentAt:   time.Now().UTC(),
		Readings: readings,
	})
	if err != nil {
		return 0, nil, 0, fmt.Errorf("encode batch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/v1/telemetry", bytes.NewReader(body))
	if err != nil {
		return 0, nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return 0, nil, 0, err
		}
		return 0, nil, 0, fmt.Errorf("post batch: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusServiceUnavailable {
		wait := 2 * time.Second
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, convErr := strconv.Atoi(v); convErr == nil && secs > 0 {
				wait = time.Duration(secs) * time.Second
			}
		}
		return 0, nil, wait, nil
	}

	if resp.StatusCode != http.StatusAccepted {
		return 0, nil, 0, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var out wire.IngestResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, nil, 0, fmt.Errorf("decode response: %w", err)
	}
	return out.Accepted, out.Rejected, 0, nil
}

func reportPeriodically(ctx context.Context, stats *counters, logger *slog.Logger) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logger.Info("progress",
				"batches", stats.posted.Load(),
				"readings_accepted", stats.accepted.Load(),
				"readings_rejected", stats.rejected.Load(),
				"shed_503", stats.shed.Load(),
				"failed", stats.failed.Load(),
				"max_queue_depth", stats.queueHigh.Load())
		}
	}
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
