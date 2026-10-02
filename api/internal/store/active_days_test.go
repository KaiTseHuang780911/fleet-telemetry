package store

import (
	"context"
	"testing"
	"time"
)

// ListActiveDays backs the route screen's day picker, so its job is to offer
// only days that hold something.

func TestListActiveDaysGroupsByDayNewestFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	vehicleID, err := s.VehicleIDForDevice(ctx, "device-days")
	if err != nil {
		t.Fatalf("resolve vehicle: %v", err)
	}

	// Three days, with a gap in the middle that must not appear in the result.
	base := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	seedPositions(t, s, vehicleID, base, 3)
	seedPositions(t, s, vehicleID, base.AddDate(0, 0, 1), 5)
	seedPositions(t, s, vehicleID, base.AddDate(0, 0, 4), 2)

	days, err := s.ListActiveDays(ctx, vehicleID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(days) != 3 {
		t.Fatalf("got %d days, want 3 — empty days should not be offered", len(days))
	}
	for i := 1; i < len(days); i++ {
		if !days[i].Day.Before(days[i-1].Day) {
			t.Fatalf("day %d is not older than the one before it", i)
		}
	}
	if days[0].Fixes != 2 {
		t.Errorf("newest day has %d fixes, want 2", days[0].Fixes)
	}
}

func TestListActiveDaysReportsTheSpanWithinEachDay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	vehicleID, err := s.VehicleIDForDevice(ctx, "device-days-span")
	if err != nil {
		t.Fatalf("resolve vehicle: %v", err)
	}

	base := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	seedPositions(t, s, vehicleID, base, 10) // 10 samples, one second apart

	days, err := s.ListActiveDays(ctx, vehicleID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("got %d days, want 1", len(days))
	}
	if !days[0].First.Equal(base) {
		t.Errorf("first = %v, want %v", days[0].First, base)
	}
	if !days[0].Last.Equal(base.Add(9 * time.Second)) {
		t.Errorf("last = %v, want %v", days[0].Last, base.Add(9*time.Second))
	}
}

func TestListActiveDaysIsScopedToOneVehicle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	mine, err := s.VehicleIDForDevice(ctx, "device-days-mine")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	theirs, err := s.VehicleIDForDevice(ctx, "device-days-theirs")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	base := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	seedPositions(t, s, mine, base, 3)
	seedPositions(t, s, theirs, base.AddDate(0, 0, 1), 3)

	days, err := s.ListActiveDays(ctx, mine, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("got %d days, want 1 — the other vehicle leaked in", len(days))
	}
}

func TestListActiveDaysReturnsEmptyForAVehicleWithNoData(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	vehicleID, err := s.VehicleIDForDevice(ctx, "device-days-silent")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	days, err := s.ListActiveDays(ctx, vehicleID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(days) != 0 {
		t.Fatalf("got %d days, want 0", len(days))
	}
}
