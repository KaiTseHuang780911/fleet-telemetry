package store

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/db"
)

// These tests need a real Postgres. They exercise the parts that unit tests
// with a fake store cannot reach: the unnest insert, the ON CONFLICT clause
// that makes ingestion idempotent, pgx's encoding of nullable columns, and the
// overlap semantics of the trips query.
//
// Without TEST_DATABASE_URL they skip rather than fail, so `go test ./...`
// stays green on a machine with no database — while CI, which sets the
// variable, runs them for real.

// shared is opened once for the whole package. An earlier version built a fresh
// pool inside every test; tearing down and rebuilding eight ten-connection
// pools back to back raced the connect timeout, especially when `go test ./...`
// runs packages in parallel. One pool is both correct and considerably faster.
var shared *Store

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		// Leave `shared` nil; testStore turns that into a skip.
		os.Exit(m.Run())
	}

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Migrate rather than assume: a contributor running these for the first
	// time should not have to know to run migrations by hand first.
	if err := db.Migrate(ctx, dsn, logger); err != nil {
		fmt.Fprintf(os.Stderr, "migrate test database: %v\n", err)
		os.Exit(1)
	}

	s, err := New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect to test database: %v\n", err)
		os.Exit(1)
	}
	shared = s

	code := m.Run()

	s.Close()
	os.Exit(code)
}

func testStore(t *testing.T) *Store {
	t.Helper()

	if shared == nil {
		t.Skip("TEST_DATABASE_URL not set; skipping integration tests")
	}

	// Truncate before each test so ordering between tests cannot matter.
	if _, err := shared.pool.Exec(context.Background(),
		`TRUNCATE positions, stop_event_matches, stop_events, trips, vehicles RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// The vehicle cache outlives the truncate, so it would hand back ids for
	// rows that no longer exist. Resetting it keeps each test genuinely
	// independent.
	shared.vehicles = newVehicleCache()

	return shared
}

func ptr[T any](v T) *T { return &v }

func TestVehicleIDForDeviceRegistersAndIsStable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first, err := s.VehicleIDForDevice(ctx, "device-alpha")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if first == uuid.Nil {
		t.Fatal("expected a real vehicle id")
	}

	// Second call hits the cache; must return the same id.
	second, err := s.VehicleIDForDevice(ctx, "device-alpha")
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if first != second {
		t.Errorf("id changed between calls: %v then %v", first, second)
	}

	// A fresh Store with an empty cache must still resolve to the same row —
	// this is what proves the upsert is doing the work, not the cache.
	s2 := &Store{pool: s.pool, vehicles: newVehicleCache()}
	third, err := s2.VehicleIDForDevice(ctx, "device-alpha")
	if err != nil {
		t.Fatalf("resolve with cold cache: %v", err)
	}
	if third != first {
		t.Errorf("cold-cache resolve gave %v, want %v", third, first)
	}

	vehicles, err := s.ListVehicles(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(vehicles) != 1 {
		t.Errorf("expected exactly 1 vehicle after repeated resolves, got %d", len(vehicles))
	}
}

func TestInsertPositionsIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	vehicleID, err := s.VehicleIDForDevice(ctx, "device-idempotent")
	if err != nil {
		t.Fatalf("resolve vehicle: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	positions := make([]Position, 3)
	for i := range positions {
		positions[i] = Position{
			ReadingID:  uuid.New(),
			VehicleID:  vehicleID,
			RecordedAt: now.Add(time.Duration(i) * time.Second),
			ReceivedAt: now,
			Lat:        49.28,
			Lon:        -123.12,
		}
	}

	inserted, err := s.InsertPositions(ctx, positions)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if inserted != 3 {
		t.Errorf("first insert reported %d rows, want 3", inserted)
	}

	// Replaying the identical batch is what a client retry looks like.
	again, err := s.InsertPositions(ctx, positions)
	if err != nil {
		t.Fatalf("replay insert: %v", err)
	}
	if again != 0 {
		t.Errorf("replay reported %d new rows, want 0", again)
	}

	count, err := s.CountPositions(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Errorf("table holds %d rows after a replay, want 3", count)
	}
}

// A batch where only some readings are new — the common case when a client
// resends after a partial acknowledgement.
func TestInsertPositionsPartialOverlap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	vehicleID, _ := s.VehicleIDForDevice(ctx, "device-overlap")
	now := time.Now().UTC().Truncate(time.Microsecond)

	mk := func(n int) []Position {
		out := make([]Position, n)
		for i := range out {
			out[i] = Position{
				ReadingID:  uuid.New(),
				VehicleID:  vehicleID,
				RecordedAt: now.Add(time.Duration(i) * time.Second),
				ReceivedAt: now,
				Lat:        49.28,
				Lon:        -123.12,
			}
		}
		return out
	}

	first := mk(3)
	if _, err := s.InsertPositions(ctx, first); err != nil {
		t.Fatalf("seed insert: %v", err)
	}

	// Two already-seen readings plus two new ones.
	mixed := append(append([]Position{}, first[:2]...), mk(2)...)
	inserted, err := s.InsertPositions(ctx, mixed)
	if err != nil {
		t.Fatalf("mixed insert: %v", err)
	}
	if inserted != 2 {
		t.Errorf("inserted %d rows from a 4-row batch with 2 duplicates, want 2", inserted)
	}
}

// The distinction ADR-001 rests on: an absent reading must be NULL in the
// database, not zero.
func TestInsertPositionsPreservesNullVersusZero(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	vehicleID, _ := s.VehicleIDForDevice(ctx, "device-nulls")
	now := time.Now().UTC().Truncate(time.Microsecond)

	explicitZero := uuid.New()
	absent := uuid.New()

	_, err := s.InsertPositions(ctx, []Position{
		{
			ReadingID: explicitZero, VehicleID: vehicleID,
			RecordedAt: now, ReceivedAt: now, Lat: 49.28, Lon: -123.12,
			SpeedMPS: ptr(float32(0)), BatteryPct: ptr(int16(0)),
			MotionState: ptr("still"),
		},
		{
			ReadingID: absent, VehicleID: vehicleID,
			RecordedAt: now, ReceivedAt: now, Lat: 49.28, Lon: -123.12,
		},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	var (
		speedNull, batteryNull, motionNull bool
		speed                              *float32
	)
	if err := s.pool.QueryRow(ctx,
		`SELECT speed_mps IS NULL, battery_pct IS NULL, motion_state IS NULL, speed_mps
		   FROM positions WHERE reading_id = $1`, explicitZero,
	).Scan(&speedNull, &batteryNull, &motionNull, &speed); err != nil {
		t.Fatalf("read explicit-zero row: %v", err)
	}
	if speedNull || batteryNull || motionNull {
		t.Error("explicitly reported zero values must not be stored as NULL")
	}
	if speed == nil || *speed != 0 {
		t.Errorf("speed_mps = %v, want 0", speed)
	}

	if err := s.pool.QueryRow(ctx,
		`SELECT speed_mps IS NULL, battery_pct IS NULL, motion_state IS NULL
		   FROM positions WHERE reading_id = $1`, absent,
	).Scan(&speedNull, &batteryNull, &motionNull); err != nil {
		t.Fatalf("read absent-fields row: %v", err)
	}
	if !speedNull || !batteryNull || !motionNull {
		t.Error("omitted fields must be stored as NULL, not zero")
	}
}

// The unnest insert must handle a batch far larger than a VALUES list could,
// without the parameter count growing with the row count.
func TestInsertPositionsHandlesLargeBatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	vehicleID, _ := s.VehicleIDForDevice(ctx, "device-large")
	now := time.Now().UTC().Truncate(time.Microsecond)

	const n = 8000 // more rows than 65535/11 would allow as bound parameters
	positions := make([]Position, n)
	for i := range positions {
		positions[i] = Position{
			ReadingID:  uuid.New(),
			VehicleID:  vehicleID,
			RecordedAt: now.Add(time.Duration(i) * time.Millisecond),
			ReceivedAt: now,
			Lat:        49.28,
			Lon:        -123.12,
			SpeedMPS:   ptr(float32(i % 30)),
		}
	}

	inserted, err := s.InsertPositions(ctx, positions)
	if err != nil {
		t.Fatalf("large insert: %v", err)
	}
	if inserted != n {
		t.Errorf("inserted %d rows, want %d", inserted, n)
	}
}

func TestInsertPositionsEmptyIsNoop(t *testing.T) {
	s := testStore(t)

	inserted, err := s.InsertPositions(context.Background(), nil)
	if err != nil {
		t.Fatalf("empty insert should not error: %v", err)
	}
	if inserted != 0 {
		t.Errorf("inserted %d, want 0", inserted)
	}
}

// Trips are selected by overlap, not containment: a trip that started before
// the window and has not ended must still appear.
func TestListTripsForVehicleUsesOverlapSemantics(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	vehicleID, _ := s.VehicleIDForDevice(ctx, "device-trips")
	base := time.Now().UTC().Truncate(time.Second)

	type seed struct {
		name    string
		started time.Time
		ended   *time.Time
	}
	seeds := []seed{
		{"wholly before the window", base.Add(-10 * time.Hour), ptr(base.Add(-9 * time.Hour))},
		{"overlapping the start", base.Add(-3 * time.Hour), ptr(base.Add(-1 * time.Hour))},
		{"inside the window", base.Add(-90 * time.Minute), ptr(base.Add(-80 * time.Minute))},
		{"still open, started long ago", base.Add(-8 * time.Hour), nil},
		{"wholly after the window", base.Add(2 * time.Hour), ptr(base.Add(3 * time.Hour))},
	}

	ids := make(map[string]uuid.UUID, len(seeds))
	for _, sd := range seeds {
		id := uuid.New()
		ids[sd.name] = id
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO trips (id, vehicle_id, source, started_at, ended_at)
			 VALUES ($1, $2, 'derived', $3, $4)`,
			id, vehicleID, sd.started, sd.ended,
		); err != nil {
			// Only one open trip per (vehicle, source) is allowed, which is
			// itself worth knowing if this ever fails.
			t.Fatalf("seed %q: %v", sd.name, err)
		}
	}

	from := base.Add(-4 * time.Hour)
	to := base

	trips, err := s.ListTripsForVehicle(ctx, vehicleID, from, to)
	if err != nil {
		t.Fatalf("list trips: %v", err)
	}

	got := make(map[uuid.UUID]bool, len(trips))
	for _, tr := range trips {
		got[tr.ID] = true
	}

	want := map[string]bool{
		"wholly before the window":     false,
		"overlapping the start":        true,
		"inside the window":            true,
		"still open, started long ago": true,
		"wholly after the window":      false,
	}
	for name, shouldAppear := range want {
		if got[ids[name]] != shouldAppear {
			t.Errorf("trip %q: present=%v, want %v", name, got[ids[name]], shouldAppear)
		}
	}
}

// The partial unique index is the database enforcing "one open trip per vehicle
// per source" rather than the application hoping for it.
func TestOnlyOneOpenTripPerVehicleAndSource(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	vehicleID, _ := s.VehicleIDForDevice(ctx, "device-open-trips")
	now := time.Now().UTC()

	insertOpen := func(source string) error {
		_, err := s.pool.Exec(ctx,
			`INSERT INTO trips (id, vehicle_id, source, started_at, ended_at)
			 VALUES ($1, $2, $3, $4, NULL)`,
			uuid.New(), vehicleID, source, now)
		return err
	}

	if err := insertOpen("client"); err != nil {
		t.Fatalf("first open client trip should be allowed: %v", err)
	}
	// A different source may also have an open trip — that is the whole point
	// of storing both.
	if err := insertOpen("derived"); err != nil {
		t.Fatalf("open derived trip alongside a client one should be allowed: %v", err)
	}
	if err := insertOpen("client"); err == nil {
		t.Error("a second open client trip must be rejected by the partial unique index")
	}
}
