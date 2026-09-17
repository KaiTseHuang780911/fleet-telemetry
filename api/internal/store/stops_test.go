package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Client stop events are reported twice under one id: once on arrival, with
// departed_at NULL, and again when the vehicle leaves. The upsert that supports
// that has to be exactly as narrow as it sounds — it must complete an open stop
// and refuse everything else — because at-least-once delivery means the client
// will, in normal operation, resend the open version after the completed one
// has already landed.

// stopFixture returns a store, a vehicle, and a stop event ready to insert.
func stopFixture(t *testing.T) (*Store, context.Context, StopEvent) {
	t.Helper()

	s := testStore(t)
	ctx := context.Background()

	vehicleID, err := s.VehicleIDForDevice(ctx, "device-stops")
	if err != nil {
		t.Fatalf("resolve vehicle: %v", err)
	}

	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}

	return s, ctx, StopEvent{
		ID:        id,
		VehicleID: vehicleID,
		Source:    SourceClient,
		ArrivedAt: time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Millisecond),
		Lat:       49.2827,
		Lon:       -123.1207,
	}
}

// fetchStop reads one stop event back by id.
func fetchStop(t *testing.T, s *Store, ctx context.Context, id uuid.UUID) StopEvent {
	t.Helper()

	var got StopEvent
	err := s.pool.QueryRow(ctx,
		`SELECT id, vehicle_id, source, arrived_at, departed_at, lat, lon
		   FROM stop_events WHERE id = $1`, id).
		Scan(&got.ID, &got.VehicleID, &got.Source, &got.ArrivedAt, &got.DepartedAt, &got.Lat, &got.Lon)
	if err != nil {
		t.Fatalf("fetch stop %s: %v", id, err)
	}
	return got
}

func countStops(t *testing.T, s *Store, ctx context.Context) int {
	t.Helper()

	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM stop_events`).Scan(&n); err != nil {
		t.Fatalf("count stops: %v", err)
	}
	return n
}

func TestOpenStopIsCompletedByTheDepartureReport(t *testing.T) {
	s, ctx, open := stopFixture(t)

	if _, err := s.InsertClientStopEvents(ctx, []StopEvent{open}); err != nil {
		t.Fatalf("insert open stop: %v", err)
	}
	if got := fetchStop(t, s, ctx, open.ID); got.DepartedAt != nil {
		t.Fatalf("a freshly reported arrival already has departed_at = %v", got.DepartedAt)
	}

	departed := open.ArrivedAt.Add(12 * time.Minute)
	complete := open
	complete.DepartedAt = &departed

	if _, err := s.InsertClientStopEvents(ctx, []StopEvent{complete}); err != nil {
		t.Fatalf("insert completed stop: %v", err)
	}

	// One stop, not two: the same id updated in place.
	if n := countStops(t, s, ctx); n != 1 {
		t.Fatalf("reporting one stop twice produced %d rows, want 1", n)
	}
	got := fetchStop(t, s, ctx, open.ID)
	if got.DepartedAt == nil {
		t.Fatal("the departure report did not complete the open stop")
	}
	if !got.DepartedAt.Equal(departed) {
		t.Errorf("departed_at = %v, want %v", got.DepartedAt, departed)
	}
}

// The reason the WHERE clause exists.
//
// At-least-once delivery resends batches, so the open version of a stop can
// arrive after the completed one — a queued batch that failed, then drained
// once the departure had already been reported over a different connection.
// Accepting it would silently reopen a finished stop and lose the departure.
func TestALateReplayOfAnOpenStopCannotEraseTheDeparture(t *testing.T) {
	s, ctx, open := stopFixture(t)

	departed := open.ArrivedAt.Add(8 * time.Minute)
	complete := open
	complete.DepartedAt = &departed

	if _, err := s.InsertClientStopEvents(ctx, []StopEvent{complete}); err != nil {
		t.Fatalf("insert completed stop: %v", err)
	}

	// The stale open version turns up afterwards.
	if _, err := s.InsertClientStopEvents(ctx, []StopEvent{open}); err != nil {
		t.Fatalf("replay open stop: %v", err)
	}

	got := fetchStop(t, s, ctx, open.ID)
	if got.DepartedAt == nil {
		t.Fatal("a stale replay of the open stop erased the recorded departure; " +
			"this stop is now permanently open and the vehicle never left")
	}
	if !got.DepartedAt.Equal(departed) {
		t.Errorf("departed_at = %v, want %v", got.DepartedAt, departed)
	}
}

func TestReplayingAStopEventIsANoOp(t *testing.T) {
	s, ctx, open := stopFixture(t)

	departed := open.ArrivedAt.Add(5 * time.Minute)
	complete := open
	complete.DepartedAt = &departed

	for i := 0; i < 4; i++ {
		if _, err := s.InsertClientStopEvents(ctx, []StopEvent{complete}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	if n := countStops(t, s, ctx); n != 1 {
		t.Fatalf("four identical reports produced %d rows, want 1", n)
	}
	got := fetchStop(t, s, ctx, open.ID)
	if got.DepartedAt == nil || !got.DepartedAt.Equal(departed) {
		t.Errorf("departed_at = %v, want %v", got.DepartedAt, departed)
	}
}

// The upsert touches departed_at and nothing else. If the other fields differ
// between the two reports, the same id is describing a different stop — a
// client bug, and one worth being able to see rather than absorbing silently.
func TestCompletingAStopDoesNotRewriteWhereOrWhenItStarted(t *testing.T) {
	s, ctx, open := stopFixture(t)

	if _, err := s.InsertClientStopEvents(ctx, []StopEvent{open}); err != nil {
		t.Fatalf("insert open stop: %v", err)
	}

	departed := open.ArrivedAt.Add(3 * time.Minute)
	tampered := open
	tampered.DepartedAt = &departed
	tampered.ArrivedAt = open.ArrivedAt.Add(-2 * time.Hour)
	tampered.Lat = 51.5074
	tampered.Lon = -0.1278

	if _, err := s.InsertClientStopEvents(ctx, []StopEvent{tampered}); err != nil {
		t.Fatalf("insert tampered completion: %v", err)
	}

	got := fetchStop(t, s, ctx, open.ID)
	if !got.ArrivedAt.Equal(open.ArrivedAt) {
		t.Errorf("arrived_at was rewritten to %v, want %v", got.ArrivedAt, open.ArrivedAt)
	}
	if got.Lat != open.Lat || got.Lon != open.Lon {
		t.Errorf("position was rewritten to (%v, %v), want (%v, %v)",
			got.Lat, got.Lon, open.Lat, open.Lon)
	}
	if got.DepartedAt == nil {
		t.Error("the stop was not completed")
	}
}

// A stop short enough to be reported once, already complete, must still work:
// the client only splits the report when the vehicle is still stationary at
// delivery time.
func TestAStopReportedCompleteInOneGoIsStored(t *testing.T) {
	s, ctx, open := stopFixture(t)

	departed := open.ArrivedAt.Add(2 * time.Minute)
	complete := open
	complete.DepartedAt = &departed

	inserted, err := s.InsertClientStopEvents(ctx, []StopEvent{complete})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if inserted != 1 {
		t.Fatalf("inserted = %d, want 1", inserted)
	}

	got := fetchStop(t, s, ctx, open.ID)
	if got.Source != SourceClient {
		t.Errorf("source = %q, want %q", got.Source, SourceClient)
	}
	if got.DepartedAt == nil || !got.DepartedAt.Equal(departed) {
		t.Errorf("departed_at = %v, want %v", got.DepartedAt, departed)
	}
}
