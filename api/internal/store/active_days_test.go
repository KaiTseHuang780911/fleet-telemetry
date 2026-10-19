package store

import (
	"context"
	"testing"
	"time"
)

// ListActiveDays backs the route screen's day picker, so its job is to offer
// only days that hold something -- in the caller's zone, because the picker
// labels each bucket with a local date.

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

	days, err := s.ListActiveDays(ctx, vehicleID, "UTC", 0)
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

	days, err := s.ListActiveDays(ctx, vehicleID, "UTC", 0)
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

	days, err := s.ListActiveDays(ctx, mine, "UTC", 0)
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

	days, err := s.ListActiveDays(ctx, vehicleID, "UTC", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(days) != 0 {
		t.Fatalf("got %d days, want 0", len(days))
	}
}

// The bug a field test found. Grouped in UTC, a viewer in UTC-7 gets days that
// run 5pm to 5pm: the bucket labelled for one date holds that evening plus all
// of the next day. A whole day's driving shows under the wrong date and the
// right one looks empty.
func TestListActiveDaysGroupsInTheCallersZone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	vehicleID, err := s.VehicleIDForDevice(ctx, "device-tz")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// 2026-10-18 02:00 UTC is still 2026-10-17 19:00 in Vancouver.
	evening := time.Date(2026, 10, 18, 2, 0, 0, 0, time.UTC)
	seedPositions(t, s, vehicleID, evening, 3)

	utcDays, err := s.ListActiveDays(ctx, vehicleID, "UTC", 0)
	if err != nil {
		t.Fatalf("list utc: %v", err)
	}
	if len(utcDays) != 1 || utcDays[0].Day.Format("2006-01-02") != "2026-10-18" {
		t.Fatalf("UTC grouping put it on %v, want 2026-10-18", utcDays[0].Day)
	}

	localDays, err := s.ListActiveDays(ctx, vehicleID, "America/Vancouver", 0)
	if err != nil {
		t.Fatalf("list local: %v", err)
	}
	if len(localDays) != 1 {
		t.Fatalf("got %d days, want 1", len(localDays))
	}
	if got := localDays[0].Day.Format("2006-01-02"); got != "2026-10-17" {
		t.Fatalf("Vancouver grouping put a 7pm local fix on %s, want 2026-10-17 — "+
			"this is the off-by-a-day that hid a whole day of driving", got)
	}
}

// Two fixes either side of local midnight must land on different days, which is
// the boundary a fixed UTC offset gets wrong half the year.
func TestListActiveDaysSplitsAtLocalMidnight(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	vehicleID, err := s.VehicleIDForDevice(ctx, "device-tz-split")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// 06:30 UTC = 23:30 previous day in Vancouver (PDT, UTC-7).
	seedPositions(t, s, vehicleID, time.Date(2026, 10, 18, 6, 30, 0, 0, time.UTC), 1)
	// 08:30 UTC = 01:30 the same day in Vancouver.
	seedPositions(t, s, vehicleID, time.Date(2026, 10, 18, 8, 30, 0, 0, time.UTC), 1)

	days, err := s.ListActiveDays(ctx, vehicleID, "America/Vancouver", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("got %d days, want 2 — fixes either side of local midnight", len(days))
	}
}

func TestListActiveDaysRejectsAnUnknownZone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	vehicleID, err := s.VehicleIDForDevice(ctx, "device-tz-bad")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	seedPositions(t, s, vehicleID, time.Now().UTC().Add(-time.Hour), 1)

	if _, err := s.ListActiveDays(ctx, vehicleID, "Mars/Olympus_Mons", 0); err == nil {
		t.Fatal("an unknown zone was accepted; it would silently group by some default")
	}
}
