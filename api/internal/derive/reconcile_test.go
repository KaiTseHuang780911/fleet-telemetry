package derive

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/store"
)

func stop(source string, offset time.Duration, metresNorth float64) store.StopEvent {
	lat, lon := pointAt(metresNorth)
	return store.StopEvent{
		ID:        uuid.New(),
		VehicleID: testVehicle,
		Source:    source,
		ArrivedAt: base.Add(offset),
		Lat:       lat,
		Lon:       lon,
	}
}

func TestReconcileMatchesNearbyStops(t *testing.T) {
	cfg := DefaultConfig() // 120s, 100m

	client := []store.StopEvent{stop(store.SourceClient, 0, 0)}
	// The server derived the same stop 40 seconds later and 20m away.
	derived := []store.StopEvent{stop(store.SourceDerived, 40*time.Second, 20)}

	matches := Reconcile(client, derived, cfg)

	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	if matches[0].ClientEventID != client[0].ID || matches[0].DerivedEventID != derived[0].ID {
		t.Error("match pairs the wrong events")
	}
	// Signed: the client arrived 40s before the derived event, so the delta is
	// negative. Sign carries the bias information, which is the point.
	if matches[0].DeltaSeconds != -40 {
		t.Errorf("delta_seconds = %d, want -40", matches[0].DeltaSeconds)
	}
	if math.Abs(matches[0].DeltaMeters-20) > 1 {
		t.Errorf("delta_meters = %v, want ~20", matches[0].DeltaMeters)
	}
}

func TestReconcileRespectsTolerances(t *testing.T) {
	cfg := DefaultConfig() // 120s, 100m

	tests := []struct {
		name        string
		timeApart   time.Duration
		metresApart float64
		wantMatched bool
	}{
		{"close in both", 10 * time.Second, 10, true},
		{"at the time limit", 120 * time.Second, 10, true},
		{"just past the time limit", 121 * time.Second, 10, false},
		{"at the distance limit", 10 * time.Second, 100, true},
		{"past the distance limit", 10 * time.Second, 150, false},
		{"within time but far away", 5 * time.Second, 500, false},
		{"nearby but hours apart", 3 * time.Hour, 5, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := []store.StopEvent{stop(store.SourceClient, 0, 0)}
			derived := []store.StopEvent{stop(store.SourceDerived, tt.timeApart, tt.metresApart)}

			matches := Reconcile(client, derived, cfg)

			if got := len(matches) == 1; got != tt.wantMatched {
				t.Errorf("matched = %v, want %v (%d matches)", got, tt.wantMatched, len(matches))
			}
		})
	}
}

// Each event may be claimed once. Without this, one derived stop could absorb
// several client stops and the counts would stop meaning anything.
func TestReconcileMatchesEachEventAtMostOnce(t *testing.T) {
	cfg := DefaultConfig()

	client := []store.StopEvent{
		stop(store.SourceClient, 0, 0),
		stop(store.SourceClient, 30*time.Second, 5),
	}
	derived := []store.StopEvent{stop(store.SourceDerived, 10*time.Second, 2)}

	matches := Reconcile(client, derived, cfg)

	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1 — the single derived stop can only be claimed once", len(matches))
	}
	// Greedy on closeness, so the nearer client event wins: 10s away beats 20s.
	if matches[0].ClientEventID != client[0].ID {
		t.Error("expected the closest client stop to claim the derived one")
	}
}

// Greedy must pick the best pairing overall, not merely the first it stumbles
// on in input order.
func TestReconcilePrefersCloserPairings(t *testing.T) {
	cfg := DefaultConfig()

	// Two stops roughly 90s apart. Naively pairing by index would cross them.
	client := []store.StopEvent{
		stop(store.SourceClient, 0, 0),
		stop(store.SourceClient, 90*time.Second, 10),
	}
	derived := []store.StopEvent{
		stop(store.SourceDerived, 95*time.Second, 12),
		stop(store.SourceDerived, 5*time.Second, 2),
	}

	matches := Reconcile(client, derived, cfg)

	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(matches))
	}

	paired := map[uuid.UUID]uuid.UUID{}
	for _, m := range matches {
		paired[m.ClientEventID] = m.DerivedEventID
	}
	if paired[client[0].ID] != derived[1].ID {
		t.Error("first client stop should pair with the 5s derived stop, not the 95s one")
	}
	if paired[client[1].ID] != derived[0].ID {
		t.Error("second client stop should pair with the 95s derived stop")
	}
}

func TestReconcileHandlesEmptyInput(t *testing.T) {
	cfg := DefaultConfig()
	one := []store.StopEvent{stop(store.SourceClient, 0, 0)}

	for _, tt := range []struct {
		name            string
		client, derived []store.StopEvent
	}{
		{"both empty", nil, nil},
		{"no derived stops", one, nil},
		{"no client stops", nil, one},
	} {
		t.Run(tt.name, func(t *testing.T) {
			matches := Reconcile(tt.client, tt.derived, cfg)
			if len(matches) != 0 {
				t.Errorf("got %d matches, want 0", len(matches))
			}
			if matches == nil {
				t.Error("result must be non-nil so it encodes as [] not null")
			}
		})
	}
}

// Reconciliation feeds a stored result that is replaced on every run, so an
// unstable pairing would churn the table on identical input.
func TestReconcileIsDeterministic(t *testing.T) {
	cfg := DefaultConfig()

	client := []store.StopEvent{
		stop(store.SourceClient, 0, 0),
		stop(store.SourceClient, 60*time.Second, 10),
		stop(store.SourceClient, 200*time.Second, 20),
	}
	derived := []store.StopEvent{
		stop(store.SourceDerived, 10*time.Second, 5),
		stop(store.SourceDerived, 70*time.Second, 12),
		stop(store.SourceDerived, 500*time.Second, 30),
	}

	first := Reconcile(client, derived, cfg)
	for i := 0; i < 20; i++ {
		again := Reconcile(client, derived, cfg)
		if len(again) != len(first) {
			t.Fatalf("run %d produced %d matches, first run produced %d", i, len(again), len(first))
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("run %d differs from the first at match %d", i, j)
			}
		}
	}
}

// The unmatched counts are the actual product of reconciliation — a client stop
// the server never saw means the two detectors disagree.
func TestSummariseReportsUnmatchedOnBothSides(t *testing.T) {
	cfg := DefaultConfig()

	client := []store.StopEvent{
		stop(store.SourceClient, 0, 0),
		stop(store.SourceClient, 10*time.Minute, 0), // server never saw this one
	}
	derived := []store.StopEvent{
		stop(store.SourceDerived, 20*time.Second, 10),
		stop(store.SourceDerived, 40*time.Minute, 0), // client never saw this one
	}

	matches := Reconcile(client, derived, cfg)
	got := Summarise(client, derived, matches)

	if got.ClientStops != 2 || got.DerivedStops != 2 {
		t.Errorf("counts = %d/%d, want 2/2", got.ClientStops, got.DerivedStops)
	}
	if got.Matched != 1 {
		t.Errorf("Matched = %d, want 1", got.Matched)
	}
	if got.ClientOnly != 1 {
		t.Errorf("ClientOnly = %d, want 1", got.ClientOnly)
	}
	if got.DerivedOnly != 1 {
		t.Errorf("DerivedOnly = %d, want 1", got.DerivedOnly)
	}
}

// Signed mean exposes systematic bias; absolute mean hides it. Both are
// reported because they answer different questions.
func TestSummariseDistinguishesBiasFromScatter(t *testing.T) {
	biased := []store.StopEventMatch{
		{DeltaSeconds: -30}, {DeltaSeconds: -28}, {DeltaSeconds: -32},
	}
	scattered := []store.StopEventMatch{
		{DeltaSeconds: -30}, {DeltaSeconds: 30}, {DeltaSeconds: -30}, {DeltaSeconds: 30},
	}

	b := Summarise(nil, nil, biased)
	if math.Abs(b.MeanSignedDeltaSeconds-(-30)) > 1 {
		t.Errorf("signed mean = %v, want ~-30 for a consistently early client", b.MeanSignedDeltaSeconds)
	}
	if math.Abs(b.MeanAbsDeltaSeconds-30) > 1 {
		t.Errorf("absolute mean = %v, want ~30", b.MeanAbsDeltaSeconds)
	}

	s := Summarise(nil, nil, scattered)
	if math.Abs(s.MeanSignedDeltaSeconds) > 1 {
		t.Errorf("signed mean = %v, want ~0 for random disagreement", s.MeanSignedDeltaSeconds)
	}
	// The absolute mean is identical to the biased case even though the
	// behaviour is completely different — which is exactly why signed is
	// reported alongside it.
	if math.Abs(s.MeanAbsDeltaSeconds-30) > 1 {
		t.Errorf("absolute mean = %v, want ~30", s.MeanAbsDeltaSeconds)
	}
}

func TestWindowFor(t *testing.T) {
	now := base
	from, to := WindowFor(now, 6*time.Hour)

	if !to.Equal(now) {
		t.Errorf("to = %v, want %v", to, now)
	}
	if got := to.Sub(from); got != 6*time.Hour {
		t.Errorf("window width = %v, want 6h", got)
	}
}
