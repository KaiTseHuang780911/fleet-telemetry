package store

import (
	"context"
	"testing"
	"time"
)

// ListVehicles answers "which vehicle should a screen show by default", so its
// ordering is load-bearing rather than cosmetic.

func TestListVehiclesPutsTheMostRecentlyActiveFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Created first, reported long ago — the trap. Ordered by creation this
	// one comes first, and a client taking row zero shows a stale vehicle
	// while the live one is invisible. That is the bug this ordering fixes.
	stale, err := s.VehicleIDForDevice(ctx, "device-stale")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	live, err := s.VehicleIDForDevice(ctx, "device-live")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	seedPositions(t, s, stale, base, 3)
	seedPositions(t, s, live, base.Add(40*time.Hour), 3)

	vehicles, err := s.ListVehicles(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(vehicles) < 2 {
		t.Fatalf("got %d vehicles, want at least 2", len(vehicles))
	}
	if vehicles[0].ExternalID != "device-live" {
		t.Fatalf("first vehicle is %q, want device-live — a client taking row zero "+
			"would show the stale vehicle and the live one would be invisible",
			vehicles[0].ExternalID)
	}
}

func TestListVehiclesReportsWhenEachLastReported(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.VehicleIDForDevice(ctx, "device-reported")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	seedPositions(t, s, id, base, 5)

	vehicles, err := s.ListVehicles(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var found *Vehicle
	for i := range vehicles {
		if vehicles[i].ExternalID == "device-reported" {
			found = &vehicles[i]
		}
	}
	if found == nil {
		t.Fatal("vehicle not listed")
	}
	if found.LastReportedAt == nil {
		t.Fatal("last_reported_at is nil for a vehicle that has reported")
	}
	// Seeded one second apart, so the newest is base + 4s.
	if !found.LastReportedAt.Equal(base.Add(4 * time.Second)) {
		t.Errorf("last_reported_at = %v, want %v", found.LastReportedAt, base.Add(4*time.Second))
	}
}

// A vehicle that has never reported is a real row with nothing to show, so it
// sorts last rather than winning by virtue of a NULL.
func TestListVehiclesSortsSilentVehiclesLast(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.VehicleIDForDevice(ctx, "device-silent"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	active, err := s.VehicleIDForDevice(ctx, "device-active")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	seedPositions(t, s, active, time.Now().UTC().Add(-time.Hour).Truncate(time.Second), 2)

	vehicles, err := s.ListVehicles(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if vehicles[0].ExternalID != "device-active" {
		t.Fatalf("first vehicle is %q, want device-active", vehicles[0].ExternalID)
	}
	last := vehicles[len(vehicles)-1]
	if last.ExternalID != "device-silent" {
		t.Errorf("last vehicle is %q, want device-silent", last.ExternalID)
	}
	if last.LastReportedAt != nil {
		t.Errorf("a vehicle with no positions reported %v", last.LastReportedAt)
	}
}
