package derive

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/store"
)

var (
	testVehicle = uuid.MustParse("018f3c4a-0000-7000-8000-00000000dead")
	base        = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	depot       = struct{ Lat, Lon float64 }{49.2827, -123.1207}
)

// pointAt returns a coordinate approximately metresNorth of the depot.
//
// Approximate on purpose: it uses the equatorial degree length, so at this
// latitude it is out by roughly 0.1%. That is irrelevant for spacing samples
// far enough apart to be inside or outside a 50m radius, which is all these
// tests need it for. Nothing asserts an exact distance through it.
func pointAt(metresNorth float64) (float64, float64) {
	const degPerMetre = 1.0 / 111_320.0
	return depot.Lat + metresNorth*degPerMetre, depot.Lon
}

type sampleSpec struct {
	offset      time.Duration
	metresNorth float64
}

func samples(specs ...sampleSpec) []store.Sample {
	out := make([]store.Sample, len(specs))
	for i, sp := range specs {
		lat, lon := pointAt(sp.metresNorth)
		out[i] = store.Sample{RecordedAt: base.Add(sp.offset), Lat: lat, Lon: lon}
	}
	return out
}

// A vehicle parked in one place for well over the dwell threshold is one stop.
func TestDetectFindsASimpleStop(t *testing.T) {
	cfg := DefaultConfig()

	var specs []sampleSpec
	for i := 0; i <= 10; i++ {
		specs = append(specs, sampleSpec{offset: time.Duration(i) * 30 * time.Second, metresNorth: 0})
	}

	got := Detect(testVehicle, samples(specs...), cfg)

	if len(got.Stops) != 1 {
		t.Fatalf("found %d stops, want 1", len(got.Stops))
	}
	if !got.Stops[0].ArrivedAt.Equal(base) {
		t.Errorf("arrived_at = %v, want %v", got.Stops[0].ArrivedAt, base)
	}
	if got.Stops[0].Source != store.SourceDerived {
		t.Errorf("source = %q, want derived", got.Stops[0].Source)
	}
	// Never moved, so there is no trip to report.
	if len(got.Trips) != 0 {
		t.Errorf("found %d trips for a stationary vehicle, want 0", len(got.Trips))
	}
}

// A pause shorter than StopMinDuration is a traffic light, not a delivery.
// Counting it would drown the stops that matter.
func TestDetectIgnoresBriefPauses(t *testing.T) {
	cfg := DefaultConfig() // 120s minimum

	got := Detect(testVehicle, samples(
		sampleSpec{0, 0},
		sampleSpec{30 * time.Second, 300},
		// 60s stationary — under the threshold.
		sampleSpec{60 * time.Second, 600},
		sampleSpec{90 * time.Second, 605},
		sampleSpec{120 * time.Second, 610},
		sampleSpec{150 * time.Second, 900},
		sampleSpec{180 * time.Second, 1200},
	), cfg)

	if len(got.Stops) != 0 {
		t.Errorf("found %d stops for a 60s pause under a 120s threshold, want 0", len(got.Stops))
	}
}

func TestDetectRespectsThresholds(t *testing.T) {
	tests := []struct {
		name      string
		dwell     time.Duration
		minDwell  time.Duration
		wantStops int
	}{
		{"dwell well over threshold", 10 * time.Minute, 2 * time.Minute, 1},
		{"dwell exactly at threshold", 2 * time.Minute, 2 * time.Minute, 1},
		{"dwell just under threshold", 90 * time.Second, 2 * time.Minute, 0},
		{"threshold relaxed to admit a short stop", 90 * time.Second, 30 * time.Second, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.StopMinDuration = tt.minDwell

			// Drive in, sit still for `dwell`, drive off.
			specs := []sampleSpec{
				{0, 0},
				{30 * time.Second, 400},
			}
			stopStart := 60 * time.Second
			for at := time.Duration(0); at <= tt.dwell; at += 30 * time.Second {
				specs = append(specs, sampleSpec{stopStart + at, 800})
			}
			specs = append(specs,
				sampleSpec{stopStart + tt.dwell + 30*time.Second, 1200},
				sampleSpec{stopStart + tt.dwell + 60*time.Second, 1600},
			)

			got := Detect(testVehicle, samples(specs...), cfg)
			if len(got.Stops) != tt.wantStops {
				t.Errorf("found %d stops, want %d", len(got.Stops), tt.wantStops)
			}
		})
	}
}

// The anchor must not drift. A vehicle creeping forward a few metres per sample
// would stay inside a *running centroid* forever and read as parked, which is
// why the cluster anchors on its first sample.
func TestDetectDoesNotTreatSlowCreepAsAStop(t *testing.T) {
	cfg := DefaultConfig() // 50m radius

	var specs []sampleSpec
	// 20m per sample for 20 samples = 400m travelled over 10 minutes.
	for i := 0; i <= 20; i++ {
		specs = append(specs, sampleSpec{time.Duration(i) * 30 * time.Second, float64(i) * 20})
	}

	got := Detect(testVehicle, samples(specs...), cfg)

	for _, s := range got.Stops {
		t.Errorf("a vehicle covering 400m was reported stopped at %v", s.ArrivedAt)
	}
}

func TestDetectSplitsTripsAtStops(t *testing.T) {
	cfg := DefaultConfig()

	specs := []sampleSpec{
		// Trip one.
		{0, 0}, {30 * time.Second, 400}, {60 * time.Second, 800},
	}
	// Stop for 5 minutes.
	for at := time.Duration(0); at <= 5*time.Minute; at += 30 * time.Second {
		specs = append(specs, sampleSpec{90*time.Second + at, 1000})
	}
	// Trip two.
	specs = append(specs,
		sampleSpec{7 * time.Minute, 1400},
		sampleSpec{7*time.Minute + 30*time.Second, 1800},
		sampleSpec{8 * time.Minute, 2200},
	)

	got := Detect(testVehicle, samples(specs...), cfg)

	if len(got.Stops) != 1 {
		t.Fatalf("found %d stops, want 1", len(got.Stops))
	}
	if len(got.Trips) != 2 {
		t.Fatalf("found %d trips, want 2 (one either side of the stop)", len(got.Trips))
	}
	if !got.Trips[0].EndedAt.Before(got.Trips[1].StartedAt) {
		t.Error("trips overlap; the first must end before the second begins")
	}
	for i, tr := range got.Trips {
		if tr.DistanceM <= 0 {
			t.Errorf("trip %d has distance %v, want a positive distance", i, tr.DistanceM)
		}
	}
}

// A coverage or power gap must break the trip. Drawing a straight line across
// it would invent distance the vehicle never travelled.
func TestDetectSplitsTripsAtCoverageGaps(t *testing.T) {
	cfg := DefaultConfig() // 10 minute gap threshold

	got := Detect(testVehicle, samples(
		sampleSpec{0, 0},
		sampleSpec{30 * time.Second, 400},
		sampleSpec{60 * time.Second, 800},
		// 30 minutes of silence — device off or out of coverage.
		sampleSpec{31 * time.Minute, 40000},
		sampleSpec{31*time.Minute + 30*time.Second, 40400},
		sampleSpec{32 * time.Minute, 40800},
	), cfg)

	if len(got.Trips) != 2 {
		t.Fatalf("found %d trips, want 2 (split at the gap)", len(got.Trips))
	}
	// The 40km jump must not appear in either trip's distance.
	for i, tr := range got.Trips {
		if tr.DistanceM > 5000 {
			t.Errorf("trip %d distance %v includes the gap; it should not", i, tr.DistanceM)
		}
	}
}

// A stop still in progress at the end of the window has no known departure.
// Inventing one at the last sample would report a vehicle as having left when
// it may still be sitting there.
func TestDetectLeavesAnOngoingStopOpen(t *testing.T) {
	cfg := DefaultConfig()

	var specs []sampleSpec
	specs = append(specs, sampleSpec{0, 0}, sampleSpec{30 * time.Second, 400})
	for at := time.Duration(0); at <= 10*time.Minute; at += 30 * time.Second {
		specs = append(specs, sampleSpec{60*time.Second + at, 800})
	}

	got := Detect(testVehicle, samples(specs...), cfg)

	if len(got.Stops) != 1 {
		t.Fatalf("found %d stops, want 1", len(got.Stops))
	}
	if got.Stops[0].DepartedAt != nil {
		t.Errorf("departed_at = %v, want nil for a stop still in progress", got.Stops[0].DepartedAt)
	}
}

func TestDetectHandlesDegenerateInput(t *testing.T) {
	cfg := DefaultConfig()

	tests := []struct {
		name    string
		samples []store.Sample
	}{
		{"no samples", nil},
		{"empty slice", []store.Sample{}},
		{"a single sample", samples(sampleSpec{0, 0})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect(testVehicle, tt.samples, cfg)
			if len(got.Trips) != 0 || len(got.Stops) != 0 {
				t.Errorf("got %d trips and %d stops, want none", len(got.Trips), len(got.Stops))
			}
			// Non-nil slices so callers and JSON encoders see [] not null.
			if got.Trips == nil || got.Stops == nil {
				t.Error("result slices must be non-nil")
			}
		})
	}
}

// Determinism matters because derivation replaces a window on every run. If the
// same input produced different boundaries, every pass would churn the data.
func TestDetectIsDeterministic(t *testing.T) {
	cfg := DefaultConfig()

	specs := []sampleSpec{{0, 0}, {30 * time.Second, 400}, {60 * time.Second, 800}}
	for at := time.Duration(0); at <= 5*time.Minute; at += 30 * time.Second {
		specs = append(specs, sampleSpec{90*time.Second + at, 1000})
	}
	specs = append(specs, sampleSpec{7 * time.Minute, 1400}, sampleSpec{8 * time.Minute, 1800})

	a := Detect(testVehicle, samples(specs...), cfg)
	b := Detect(testVehicle, samples(specs...), cfg)

	if len(a.Trips) != len(b.Trips) || len(a.Stops) != len(b.Stops) {
		t.Fatalf("counts differ between runs: %d/%d vs %d/%d",
			len(a.Trips), len(a.Stops), len(b.Trips), len(b.Stops))
	}
	for i := range a.Stops {
		if !a.Stops[i].ArrivedAt.Equal(b.Stops[i].ArrivedAt) {
			t.Errorf("stop %d arrived_at differs between runs", i)
		}
	}
	for i := range a.Trips {
		if !a.Trips[i].StartedAt.Equal(b.Trips[i].StartedAt) ||
			math.Abs(a.Trips[i].DistanceM-b.Trips[i].DistanceM) > 1e-9 {
			t.Errorf("trip %d differs between runs", i)
		}
	}
}

func TestDistanceM(t *testing.T) {
	tests := []struct {
		name           string
		lat1, lon1     float64
		lat2, lon2     float64
		wantM, toleran float64
	}{
		{"identical points", 49.28, -123.12, 49.28, -123.12, 0, 0.001},
		// Reference values, independent of the pointAt helper: that helper uses
		// the equatorial degree length and is only meant for spacing samples
		// roughly, not for asserting distances to the metre.
		{"one degree of latitude at the equator", 0, 0, 1, 0, 111_195, 100},
		{"a hundredth of a degree of latitude", 0, 0, 0.01, 0, 1111.95, 1},
		{"one degree of longitude at the equator", 0, 0, 0, 1, 111_195, 100},
		// Longitude lines converge toward the poles, so the same angular step
		// spans far less ground at 60 degrees north — half, by cos(60).
		{"one degree of longitude at 60 north", 60, 0, 60, 1, 55_597, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := distanceM(tt.lat1, tt.lon1, tt.lat2, tt.lon2)
			if math.Abs(got-tt.wantM) > tt.toleran {
				t.Errorf("distance = %v, want %v (+/- %v)", got, tt.wantM, tt.toleran)
			}
		})
	}
}

// A stop must be linked to the journey that arrived at it.
//
// Regression test: the original condition asked whether the stop's arrival fell
// *inside* a trip's span. Trips are the movement between stops, so that is
// never true, and every trip_id came out nil while the code read plausibly.
func TestDetectLinksStopsToTheArrivingTrip(t *testing.T) {
	cfg := DefaultConfig()

	specs := []sampleSpec{{0, 0}, {30 * time.Second, 400}, {60 * time.Second, 800}}
	// First stop.
	for at := time.Duration(0); at <= 5*time.Minute; at += 30 * time.Second {
		specs = append(specs, sampleSpec{90*time.Second + at, 1000})
	}
	// Drive on.
	specs = append(specs,
		sampleSpec{7 * time.Minute, 1400},
		sampleSpec{7*time.Minute + 30*time.Second, 1800})
	// Second stop.
	for at := time.Duration(0); at <= 5*time.Minute; at += 30 * time.Second {
		specs = append(specs, sampleSpec{8*time.Minute + at, 2000})
	}

	got := Detect(testVehicle, samples(specs...), cfg)

	if len(got.Stops) != 2 {
		t.Fatalf("found %d stops, want 2", len(got.Stops))
	}
	if len(got.Trips) < 2 {
		t.Fatalf("found %d trips, want at least 2", len(got.Trips))
	}

	for i, s := range got.Stops {
		if s.TripID == nil {
			t.Errorf("stop %d has no arriving trip; both stops here follow a journey", i)
			continue
		}

		var linked *store.Trip
		for j := range got.Trips {
			if got.Trips[j].ID == *s.TripID {
				linked = &got.Trips[j]
				break
			}
		}
		if linked == nil {
			t.Errorf("stop %d references a trip that is not in the result", i)
			continue
		}
		if linked.EndedAt == nil || linked.EndedAt.After(s.ArrivedAt) {
			t.Errorf("stop %d is linked to a trip that had not ended when it began", i)
		}
	}

	// Each stop should follow its own trip, not share one.
	if *got.Stops[0].TripID == *got.Stops[1].TripID {
		t.Error("both stops linked to the same trip; each follows a separate journey")
	}
}
