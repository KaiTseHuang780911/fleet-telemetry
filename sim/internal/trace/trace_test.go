package trace

import (
	"math"
	"testing"
	"time"
)

var simStart = time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)

// traceOf runs a fleet for n ticks and returns each vehicle's observable state,
// deliberately excluding reading ids — those are unique per emission by design.
func traceOf(cfg Config, ticks int) [][]Point {
	fleet := NewFleet(cfg)
	out := make([][]Point, len(fleet.Vehicles()))
	dt := time.Second

	for i, v := range fleet.Vehicles() {
		path := make([]Point, 0, ticks)
		for tick := 0; tick < ticks; tick++ {
			r := v.Tick(simStart.Add(time.Duration(tick)*dt), dt)
			path = append(path, Point{Lat: r.Lat, Lon: r.Lon})
		}
		out[i] = path
	}
	return out
}

// The property CI depends on: same seed, same trace.
func TestSameSeedProducesIdenticalTraces(t *testing.T) {
	cfg := Config{Seed: 42, Vehicles: 3}

	a := traceOf(cfg, 200)
	b := traceOf(cfg, 200)

	for v := range a {
		if len(a[v]) != len(b[v]) {
			t.Fatalf("vehicle %d: trace lengths differ: %d vs %d", v, len(a[v]), len(b[v]))
		}
		for i := range a[v] {
			// Exact equality, not approximate. The same arithmetic on the same
			// inputs must give bit-identical results; tolerating drift here
			// would hide real nondeterminism.
			if a[v][i] != b[v][i] {
				t.Fatalf("vehicle %d tick %d: %v != %v", v, i, a[v][i], b[v][i])
			}
		}
	}
}

func TestDifferentSeedsProduceDifferentTraces(t *testing.T) {
	a := traceOf(Config{Seed: 42, Vehicles: 2}, 100)
	b := traceOf(Config{Seed: 43, Vehicles: 2}, 100)

	identical := true
	for v := range a {
		for i := range a[v] {
			if a[v][i] != b[v][i] {
				identical = false
				break
			}
		}
	}
	if identical {
		t.Fatal("different seeds produced identical traces; the seed is not being used")
	}
}

// A vehicle's trace must not depend on how many other vehicles exist. If it
// did, the simulator would only be reproducible at one fleet size.
func TestVehicleTraceIsIndependentOfFleetSize(t *testing.T) {
	small := traceOf(Config{Seed: 7, Vehicles: 2}, 150)
	large := traceOf(Config{Seed: 7, Vehicles: 20}, 150)

	for v := 0; v < 2; v++ {
		for i := range small[v] {
			if small[v][i] != large[v][i] {
				t.Fatalf("vehicle %d tick %d differs between fleet sizes: %v vs %v",
					v, i, small[v][i], large[v][i])
			}
		}
	}
}

// Every generated reading must satisfy the server's validation, or the
// simulator would spend its life being rejected.
func TestGeneratedReadingsPassServerValidation(t *testing.T) {
	fleet := NewFleet(Config{Seed: 99, Vehicles: 5})
	dt := time.Second

	for _, v := range fleet.Vehicles() {
		for tick := 0; tick < 500; tick++ {
			now := simStart.Add(time.Duration(tick) * dt)
			r := v.Tick(now, dt)
			if err := r.Validate(now); err != nil {
				t.Fatalf("vehicle %s tick %d produced an invalid reading: %v", v.DeviceID, tick, err)
			}
		}
	}
}

// Coordinates must stay in range over a long run — an unbounded random walk
// would eventually wander past the poles.
func TestCoordinatesStayInRangeOverLongRun(t *testing.T) {
	fleet := NewFleet(Config{Seed: 1234, Vehicles: 3})
	dt := time.Second

	for _, v := range fleet.Vehicles() {
		for tick := 0; tick < 5000; tick++ {
			r := v.Tick(simStart.Add(time.Duration(tick)*dt), dt)
			if r.Lat < -90 || r.Lat > 90 {
				t.Fatalf("latitude %v out of range at tick %d", r.Lat, tick)
			}
			if r.Lon < -180 || r.Lon > 180 {
				t.Fatalf("longitude %v out of range at tick %d", r.Lon, tick)
			}
		}
	}
}

// Vehicles must actually stop sometimes, or there will be no stop events to
// detect and ADR-002's reconciliation work has nothing to reconcile.
func TestVehiclesBothMoveAndStop(t *testing.T) {
	fleet := NewFleet(Config{Seed: 2024, Vehicles: 4})
	dt := time.Second

	movedAtAll, stoppedAtAll := false, false
	for _, v := range fleet.Vehicles() {
		for tick := 0; tick < 3000; tick++ {
			r := v.Tick(simStart.Add(time.Duration(tick)*dt), dt)
			if r.SpeedMPS == nil {
				t.Fatal("simulator should always report a speed")
			}
			if *r.SpeedMPS > 0 {
				movedAtAll = true
			} else {
				stoppedAtAll = true
			}
		}
	}
	if !movedAtAll {
		t.Error("no vehicle ever moved")
	}
	if !stoppedAtAll {
		t.Error("no vehicle ever stopped; there would be no stop events to detect")
	}
}

func TestMoveGeodesy(t *testing.T) {
	start := Point{Lat: 49.2827, Lon: -123.1207}

	tests := []struct {
		name      string
		bearing   float64
		distanceM float64
		check     func(t *testing.T, got Point)
	}{
		{
			name: "zero distance is a no-op",
			check: func(t *testing.T, got Point) {
				if got != start {
					t.Errorf("got %v, want the starting point unchanged", got)
				}
			},
		},
		{
			name:      "1000m due north increases latitude by ~0.009 degrees",
			bearing:   0,
			distanceM: 1000,
			check: func(t *testing.T, got Point) {
				deltaLat := got.Lat - start.Lat
				if math.Abs(deltaLat-0.008993) > 0.0001 {
					t.Errorf("latitude delta = %v, want ~0.008993", deltaLat)
				}
				if math.Abs(got.Lon-start.Lon) > 1e-9 {
					t.Errorf("longitude should not change when heading due north, got %v", got.Lon)
				}
			},
		},
		{
			name:      "1000m due east increases longitude and leaves latitude near constant",
			bearing:   90,
			distanceM: 1000,
			check: func(t *testing.T, got Point) {
				if got.Lon <= start.Lon {
					t.Errorf("longitude should increase heading east, got %v", got.Lon)
				}
				if math.Abs(got.Lat-start.Lat) > 0.0001 {
					t.Errorf("latitude drifted too far heading due east: %v", got.Lat)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, move(start, tt.bearing, tt.distanceM))
		})
	}
}

// move and DistanceM must agree, or trip distances computed from traces will be
// quietly wrong.
func TestMoveAndDistanceAreConsistent(t *testing.T) {
	start := Point{Lat: 49.2827, Lon: -123.1207}

	for _, distance := range []float64{10, 100, 1000, 10000} {
		for _, bearing := range []float64{0, 45, 90, 180, 270, 359} {
			end := move(start, bearing, distance)
			got := DistanceM(start, end)
			if math.Abs(got-distance) > distance*0.001 {
				t.Errorf("bearing %v distance %v: DistanceM reported %v", bearing, distance, got)
			}
		}
	}
}

func TestDeviceIDsAreZeroPaddedAndUnique(t *testing.T) {
	fleet := NewFleet(Config{Seed: 1, Vehicles: 12})

	seen := make(map[string]bool)
	for _, v := range fleet.Vehicles() {
		if seen[v.DeviceID] {
			t.Fatalf("duplicate device id %q", v.DeviceID)
		}
		seen[v.DeviceID] = true
	}

	if got := fleet.Vehicles()[0].DeviceID; got != "sim-vehicle-000" {
		t.Errorf("first device id = %q, want sim-vehicle-000", got)
	}
	// Zero padding is what keeps lexical and numeric ordering in agreement.
	if got := fleet.Vehicles()[11].DeviceID; got != "sim-vehicle-011" {
		t.Errorf("twelfth device id = %q, want sim-vehicle-011", got)
	}
}
