package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ListPositions is the read side of the highest-volume table, and the only
// consumer of the composite index ADR-001 created for it.

func seedPositions(t *testing.T, s *Store, vehicleID uuid.UUID, base time.Time, n int) {
	t.Helper()

	rows := make([]Position, n)
	for i := range rows {
		id, err := uuid.NewV7()
		if err != nil {
			t.Fatalf("uuid: %v", err)
		}
		rows[i] = Position{
			ReadingID:  id,
			VehicleID:  vehicleID,
			RecordedAt: base.Add(time.Duration(i) * time.Second),
			ReceivedAt: base.Add(time.Duration(i) * time.Second),
			Lat:        49.28 + float64(i)/100000,
			Lon:        -123.12,
		}
	}
	if _, err := s.InsertPositions(context.Background(), rows); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func positionFixture(t *testing.T, n int) (*Store, context.Context, uuid.UUID, time.Time) {
	t.Helper()

	s := testStore(t)
	ctx := context.Background()

	vehicleID, err := s.VehicleIDForDevice(ctx, "device-positions")
	if err != nil {
		t.Fatalf("resolve vehicle: %v", err)
	}
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	seedPositions(t, s, vehicleID, base, n)
	return s, ctx, vehicleID, base
}

// Ascending, because a route is drawn forwards and the derivation pass folds
// samples in time order. Descending would be silently wrong rather than
// obviously wrong: the map would render the same shape.
func TestListPositionsReturnsOldestFirst(t *testing.T) {
	s, ctx, vehicleID, base := positionFixture(t, 10)

	page, err := s.ListPositions(ctx, vehicleID, base.Add(-time.Minute), base.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Positions) != 10 {
		t.Fatalf("got %d positions, want 10", len(page.Positions))
	}
	for i := 1; i < len(page.Positions); i++ {
		if page.Positions[i].RecordedAt.Before(page.Positions[i-1].RecordedAt) {
			t.Fatalf("position %d is older than the one before it", i)
		}
	}
}

// The window is half-open, so consecutive windows tile without double-counting
// the boundary sample.
func TestListPositionsExcludesTheUpperBound(t *testing.T) {
	s, ctx, vehicleID, base := positionFixture(t, 10)

	// [base, base+5s) must hold exactly the samples at 0..4 seconds.
	page, err := s.ListPositions(ctx, vehicleID, base, base.Add(5*time.Second), 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Positions) != 5 {
		t.Fatalf("got %d positions, want 5", len(page.Positions))
	}
}

// A truncated route looks exactly like a vehicle that stopped moving, so the
// caller has to be told. This is the flag that stops a short read being read as
// a short trip.
func TestListPositionsReportsTruncation(t *testing.T) {
	s, ctx, vehicleID, base := positionFixture(t, 10)
	window := []time.Time{base.Add(-time.Minute), base.Add(time.Hour)}

	page, err := s.ListPositions(ctx, vehicleID, window[0], window[1], 4)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Positions) != 4 {
		t.Fatalf("got %d positions, want 4", len(page.Positions))
	}
	if !page.Truncated {
		t.Error("a capped read did not report itself as truncated")
	}

	// And the boundary case that an off-by-one would get wrong: exactly as many
	// rows as the limit is NOT truncation.
	exact, err := s.ListPositions(ctx, vehicleID, window[0], window[1], 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(exact.Positions) != 10 {
		t.Fatalf("got %d positions, want 10", len(exact.Positions))
	}
	if exact.Truncated {
		t.Error("a read that exactly filled the limit reported truncation")
	}
}

func TestListPositionsIsScopedToOneVehicle(t *testing.T) {
	s, ctx, vehicleID, base := positionFixture(t, 5)

	other, err := s.VehicleIDForDevice(ctx, "device-other")
	if err != nil {
		t.Fatalf("resolve other vehicle: %v", err)
	}
	seedPositions(t, s, other, base, 5)

	page, err := s.ListPositions(ctx, vehicleID, base.Add(-time.Minute), base.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Positions) != 5 {
		t.Fatalf("got %d positions, want 5 — the other vehicle leaked in", len(page.Positions))
	}
	for _, p := range page.Positions {
		if p.VehicleID != vehicleID {
			t.Fatalf("got a position for vehicle %s", p.VehicleID)
		}
	}
}

func TestListPositionsReturnsAnEmptyPageRatherThanNil(t *testing.T) {
	s, ctx, vehicleID, base := positionFixture(t, 3)

	// A window before anything was recorded.
	page, err := s.ListPositions(ctx, vehicleID, base.Add(-time.Hour), base.Add(-time.Minute), 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Positions == nil {
		t.Error("empty result encoded as nil; JSON would render null instead of []")
	}
	if len(page.Positions) != 0 {
		t.Errorf("got %d positions, want 0", len(page.Positions))
	}
}

// An absurd limit must not become an unbounded read of the largest table.
func TestListPositionsCapsAnOversizedLimit(t *testing.T) {
	s, ctx, vehicleID, base := positionFixture(t, 3)

	page, err := s.ListPositions(ctx, vehicleID, base.Add(-time.Minute), base.Add(time.Hour), 1_000_000)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Positions) != 3 {
		t.Fatalf("got %d positions, want 3", len(page.Positions))
	}
}
