package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Tests for the vehicle cache's lifecycle.
//
// These exist because of a real incident. The vehicles table was truncated
// while the API kept running; the cache had a population path and no
// invalidation path, so it went on returning ids for rows that no longer
// existed. Every insert violated its foreign key, the flush failed, and the
// process stayed poisoned for three days — while the handler answered 202 and
// clients deleted their only copy of the data.
//
// The general shape worth guarding against is not "vehicles were deleted". It
// is **in-memory state that outlives the thing it describes, with no way back**.
// Each test below is written against that shape rather than the specific
// trigger.

func seedPosition(vehicleID uuid.UUID) Position {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return Position{
		ReadingID:  uuid.New(),
		VehicleID:  vehicleID,
		RecordedAt: now,
		ReceivedAt: now,
		Lat:        49.28,
		Lon:        -123.12,
	}
}

// The incident, reproduced exactly: TRUNCATE ... CASCADE under a live cache.
func TestCacheRecoversFromTruncateUnderALiveProcess(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	stale, err := s.VehicleIDForDevice(ctx, "device-truncated")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := s.InsertPositions(ctx, []Position{seedPosition(stale)}); err != nil {
		t.Fatalf("baseline insert: %v", err)
	}

	// Exactly what was run by hand during testing, with the API still up.
	if _, err := s.pool.Exec(ctx,
		`TRUNCATE positions, stop_event_matches, stop_events, trips, vehicles CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// The cache still holds the pre-truncate id, so this insert must fail — but
	// it must fail in a way that heals.
	_, err = s.InsertPositions(ctx, []Position{seedPosition(stale)})
	if !errors.Is(err, ErrStaleVehicleCache) {
		t.Fatalf("expected ErrStaleVehicleCache, got %v", err)
	}

	// The property that was missing, and the whole point of the fix: recovery
	// happens without anyone restarting the process.
	fresh, err := s.VehicleIDForDevice(ctx, "device-truncated")
	if err != nil {
		t.Fatalf("re-resolve: %v", err)
	}
	if fresh == stale {
		t.Fatal("re-resolve returned the id that was just truncated away")
	}
	if _, err := s.InsertPositions(ctx, []Position{seedPosition(fresh)}); err != nil {
		t.Fatalf("insert after recovery: %v", err)
	}
}

// A truncate removes every row, so evicting only the key that happened to fail
// would leave the rest of the fleet broken.
func TestInvalidationClearsEveryCachedDevice(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	devices := []string{"device-a", "device-b", "device-c"}
	ids := make(map[string]uuid.UUID, len(devices))
	for _, d := range devices {
		id, err := s.VehicleIDForDevice(ctx, d)
		if err != nil {
			t.Fatalf("resolve %s: %v", d, err)
		}
		ids[d] = id
	}
	if got := s.CachedVehicleCount(); got != len(devices) {
		t.Fatalf("cached %d devices, want %d", got, len(devices))
	}

	if _, err := s.pool.Exec(ctx, `TRUNCATE positions, stop_events, trips, vehicles CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// One device's insert fails...
	if _, err := s.InsertPositions(ctx, []Position{seedPosition(ids["device-a"])}); !errors.Is(err, ErrStaleVehicleCache) {
		t.Fatalf("expected ErrStaleVehicleCache, got %v", err)
	}
	// ...and every device is re-resolvable, not just that one.
	if got := s.CachedVehicleCount(); got != 0 {
		t.Errorf("cache holds %d entries after invalidation, want 0", got)
	}
	for _, d := range devices {
		id, err := s.VehicleIDForDevice(ctx, d)
		if err != nil {
			t.Fatalf("re-resolve %s: %v", d, err)
		}
		if id == ids[d] {
			t.Errorf("%s re-resolved to the stale id", d)
		}
	}
}

// The inverse guard. Over-invalidating would be a quieter bug: every malformed
// reading would dump the cache and cost a lookup per device afterwards, and the
// symptom — a slow but working system — is much harder to trace back than an
// outright failure.
func TestOnlyForeignKeyViolationsInvalidateTheCache(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	vehicleID, err := s.VehicleIDForDevice(ctx, "device-check-violation")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	cachedBefore := s.CachedVehicleCount()

	// A CHECK violation rather than a foreign-key one: the vehicle is fine, the
	// data is not. Validation should normally stop this reaching the database,
	// which is exactly why the store must not treat it as a cache problem.
	bad := seedPosition(vehicleID)
	bad.Lat = 991

	_, err = s.InsertPositions(ctx, []Position{bad})
	if err == nil {
		t.Fatal("expected the CHECK constraint to reject a latitude of 991")
	}
	if errors.Is(err, ErrStaleVehicleCache) {
		t.Error("a CHECK violation was misreported as a stale cache")
	}
	if got := s.CachedVehicleCount(); got != cachedBefore {
		t.Errorf("cache went from %d to %d entries on a non-cache error", cachedBefore, got)
	}

	// And the store still works afterwards.
	if _, err := s.InsertPositions(ctx, []Position{seedPosition(vehicleID)}); err != nil {
		t.Fatalf("insert after a rejected row: %v", err)
	}
}

// Recovery must not depend on the test fixture.
//
// The integration harness truncates *and* resets the cache before each test,
// and that reset is precisely the step production has no equivalent of — it is
// why every test passed while the real system was broken. This test deliberately
// does the truncate and then leaves the cache alone, so it fails if the system
// ever goes back to needing outside help.
func TestRecoveryDoesNotDependOnTheTestFixture(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.VehicleIDForDevice(ctx, "device-unassisted")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if _, err := s.pool.Exec(ctx, `TRUNCATE positions, stop_events, trips, vehicles CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// Note what is NOT done here: no InvalidateVehicleCache, no new cache
	// object. The only thing allowed to fix this is the production code path.

	if _, err := s.InsertPositions(ctx, []Position{seedPosition(id)}); !errors.Is(err, ErrStaleVehicleCache) {
		t.Fatalf("expected ErrStaleVehicleCache, got %v", err)
	}

	fresh, err := s.VehicleIDForDevice(ctx, "device-unassisted")
	if err != nil {
		t.Fatalf("re-resolve: %v", err)
	}
	if _, err := s.InsertPositions(ctx, []Position{seedPosition(fresh)}); err != nil {
		t.Fatalf("the system did not heal on its own: %v", err)
	}
}

// Invalidation happens on the writer goroutine while request handlers are
// reading the cache. This is the shape that produces a rare crash in production
// and never in a single-threaded test, so it is worth asserting under -race.
func TestCacheIsSafeUnderConcurrentUseAndInvalidation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers*2)
	stop := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := s.VehicleIDForDevice(ctx, "device-concurrent"); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.InvalidateVehicleCache()
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent resolve failed: %v", err)
	}
}
