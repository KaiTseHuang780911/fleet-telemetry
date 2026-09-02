// Package trace generates plausible vehicle movement.
//
// The trace is deterministic given a seed: position, heading, speed, battery
// and stop timing are identical on every run and every machine, which lets
// tests and CI assert exact values instead of tolerating "roughly right".
//
// ReadingID is the deliberate exception. Each emission gets a fresh UUIDv7,
// because a reading id is an identity for a distinct observation — reusing one
// across runs would make the server treat a genuinely new reading as a replay
// and silently discard it via ON CONFLICT DO NOTHING. Assert on the trace, not
// on ids.
package trace

import (
	"math"
	"math/rand"
	"time"

	"github.com/google/uuid"

	"github.com/KaiTseHuang780911/fleet-telemetry/internal/wire"
)

const earthRadiusM = 6_371_000

// Movement parameters.
//
// All of these are expressed in real units — metres per second, seconds — and
// converted using the tick duration, never in "ticks".
//
// That distinction is load-bearing. An earlier version defined dwell as "30 to
// 150 ticks", which meant the tick rate silently changed how the vehicle
// behaved: running the simulator faster produced stops too short for a device
// to report, and the generated data quietly stopped exercising the code it was
// meant to. A fixture whose semantics depend on how fast you run it is not a
// fixture.
const (
	cruiseSpeedMPS = 11.0 // ~40 km/h, plausible for urban driving
	speedJitterMPS = 3.0

	// Expected stops per hour of driving. Converted to a per-tick probability
	// using dt, so the rate holds at any tick duration.
	stopsPerHour = 15.0

	// Dwell bounds. Chosen to straddle typical detection thresholds so the data
	// exercises both sides: some stops are long enough for every detector to
	// agree, others sit in the range where client and server disagree — which
	// is what the reconciliation pass is there to measure.
	minDwell = 50 * time.Second
	maxDwell = 240 * time.Second

	// Heading drift per second, scaled by dt. Roads bend; they do not teleport.
	headingDriftDegPerSec = 4.0

	batteryDrainPctPerSec = 0.004

	// How long a vehicle must be stationary before the device reports a stop
	// for itself.
	//
	// Deliberately shorter than the server's derivation threshold. A real
	// device has accelerometer and activity-recognition data and can call a
	// stop sooner and more confidently than a server squinting at GPS points,
	// so the two disagree — and that disagreement is precisely what the
	// reconciliation pass measures. Matching them would make the metric
	// meaningless by construction.
	clientMinStopDuration = 45 * time.Second
)

// Point is a WGS84 coordinate.
type Point struct {
	Lat float64
	Lon float64
}

// Config parameterises a fleet.
type Config struct {
	Seed     int64
	Vehicles int
	Origin   Point // vehicles start scattered around here
	SpreadM  float64
}

// Vehicle carries the simulation state for one device.
type Vehicle struct {
	DeviceID string

	rng        *rand.Rand
	pos        Point
	headingDeg float64
	speedMPS   float64
	battery    float64
	stopped    bool
	// dwellRemaining counts down in real time, not ticks.
	dwellRemaining time.Duration

	// On-device stop detection state.
	stopStartedAt time.Time
	stopLat       float64
	stopLon       float64
	pendingStops  []wire.StopEvent
}

// Fleet is a deterministic set of vehicles.
type Fleet struct {
	vehicles []*Vehicle
}

// NewFleet builds a fleet. Two calls with the same Config produce identical
// vehicles in identical starting states.
func NewFleet(cfg Config) *Fleet {
	if cfg.Vehicles <= 0 {
		cfg.Vehicles = 1
	}
	if cfg.SpreadM <= 0 {
		cfg.SpreadM = 3000
	}
	if cfg.Origin == (Point{}) {
		cfg.Origin = Point{Lat: 49.2827, Lon: -123.1207} // Vancouver
	}

	f := &Fleet{vehicles: make([]*Vehicle, cfg.Vehicles)}
	for i := range f.vehicles {
		// Each vehicle gets its own generator seeded from the fleet seed and
		// its index. Sharing one generator would make a vehicle's trace depend
		// on how many others exist and on goroutine interleaving — that is
		// exactly the kind of hidden coupling that makes a "deterministic"
		// simulator reproduce only by accident.
		rng := rand.New(rand.NewSource(cfg.Seed + int64(i)*7919))

		bearing := rng.Float64() * 360
		distance := rng.Float64() * cfg.SpreadM

		v := &Vehicle{
			DeviceID:   deviceID(i),
			rng:        rng,
			pos:        move(cfg.Origin, bearing, distance),
			headingDeg: rng.Float64() * 360,
			speedMPS:   cruiseSpeedMPS,
			battery:    60 + rng.Float64()*40,
		}
		f.vehicles[i] = v
	}
	return f
}

// Vehicles exposes the fleet for iteration.
func (f *Fleet) Vehicles() []*Vehicle { return f.vehicles }

// Tick advances the vehicle by dt and returns the reading it would report.
func (v *Vehicle) Tick(now time.Time, dt time.Duration) wire.Reading {
	wasStopped := v.stopped
	v.advance(dt)
	v.trackStop(now, wasStopped)

	// UUIDv7 is time-ordered, which is what keeps primary-key inserts at the
	// right edge of the B-tree server-side. Falling back to v4 rather than
	// failing: a simulator that halts because a UUID could not be generated
	// would be worse than one whose keys are slightly less ordered.
	id, err := uuid.NewV7()
	if err != nil {
		id = uuid.New()
	}

	motion := wire.MotionDriving
	if v.stopped {
		motion = wire.MotionStill
	}

	speed := float32(v.speedMPS)
	heading := float32(v.headingDeg)
	accuracy := float32(3 + v.rng.Float64()*7)
	battery := int16(v.battery)

	return wire.Reading{
		ReadingID:   id,
		RecordedAt:  now,
		Lat:         v.pos.Lat,
		Lon:         v.pos.Lon,
		SpeedMPS:    &speed,
		HeadingDeg:  &heading,
		AccuracyM:   &accuracy,
		BatteryPct:  &battery,
		MotionState: &motion,
	}
}

// trackStop maintains the device's own view of when it started and finished
// being stationary, independent of anything the server later derives.
func (v *Vehicle) trackStop(now time.Time, wasStopped bool) {
	switch {
	case !wasStopped && v.stopped:
		// Just came to rest. The device knows immediately; it does not wait to
		// see whether the stop lasts.
		v.stopStartedAt = now
		v.stopLat, v.stopLon = v.pos.Lat, v.pos.Lon

	case wasStopped && !v.stopped:
		// Moved off. Report the stop only if it lasted long enough to be worth
		// reporting — otherwise every traffic light becomes a delivery.
		if !v.stopStartedAt.IsZero() && now.Sub(v.stopStartedAt) >= clientMinStopDuration {
			id, err := uuid.NewV7()
			if err != nil {
				id = uuid.New()
			}
			departed := now
			v.pendingStops = append(v.pendingStops, wire.StopEvent{
				EventID:    id,
				ArrivedAt:  v.stopStartedAt,
				DepartedAt: &departed,
				Lat:        v.stopLat,
				Lon:        v.stopLon,
			})
		}
		v.stopStartedAt = time.Time{}
	}
}

// TakeStopEvents returns the stop events completed since the last call and
// clears them.
//
// Take rather than peek: the caller queues them for upload and owns them from
// that point, so leaving a copy behind would risk reporting the same stop
// twice under a different event id — which the server could not deduplicate,
// because idempotency keys off the id.
func (v *Vehicle) TakeStopEvents() []wire.StopEvent {
	if len(v.pendingStops) == 0 {
		return nil
	}
	out := v.pendingStops
	v.pendingStops = nil
	return out
}

func (v *Vehicle) advance(dt time.Duration) {
	secs := dt.Seconds()

	if v.stopped {
		v.speedMPS = 0
		v.dwellRemaining -= dt
		if v.dwellRemaining <= 0 {
			v.stopped = false
			v.speedMPS = cruiseSpeedMPS
		}
	} else {
		// Probability of stopping during this tick, derived from the hourly
		// rate so it is independent of tick duration.
		if v.rng.Float64() < stopsPerHour/3600.0*secs {
			v.stopped = true
			v.dwellRemaining = minDwell + time.Duration(v.rng.Float64()*float64(maxDwell-minDwell))
			v.speedMPS = 0
		} else {
			v.headingDeg = math.Mod(
				v.headingDeg+(v.rng.Float64()*2-1)*headingDriftDegPerSec*secs+360, 360)
			v.speedMPS = cruiseSpeedMPS + (v.rng.Float64()*2-1)*speedJitterMPS
			if v.speedMPS < 0 {
				v.speedMPS = 0
			}
			v.pos = move(v.pos, v.headingDeg, v.speedMPS*secs)
		}
	}

	v.battery -= batteryDrainPctPerSec * secs
	if v.battery < 5 {
		v.battery = 100 // treat as plugged in overnight
	}
}

// move returns the point reached by travelling distanceM along bearingDeg from
// start, on a spherical earth.
//
// Spherical rather than ellipsoidal (Vincenty): the error is well under a metre
// over the distances a vehicle covers in one tick, and GPS accuracy is several
// metres anyway. Precision beyond the sensor's is false precision.
func move(start Point, bearingDeg, distanceM float64) Point {
	if distanceM == 0 {
		return start
	}

	angular := distanceM / earthRadiusM
	bearing := bearingDeg * math.Pi / 180
	lat1 := start.Lat * math.Pi / 180
	lon1 := start.Lon * math.Pi / 180

	sinLat2 := math.Sin(lat1)*math.Cos(angular) +
		math.Cos(lat1)*math.Sin(angular)*math.Cos(bearing)
	lat2 := math.Asin(sinLat2)

	y := math.Sin(bearing) * math.Sin(angular) * math.Cos(lat1)
	x := math.Cos(angular) - math.Sin(lat1)*sinLat2
	lon2 := lon1 + math.Atan2(y, x)

	// Normalise longitude into [-180, 180] so a vehicle crossing the
	// antimeridian does not produce values the server will reject.
	lonDeg := math.Mod(lon2*180/math.Pi+540, 360) - 180

	return Point{Lat: lat2 * 180 / math.Pi, Lon: lonDeg}
}

// DistanceM is the great-circle distance between two points. Used by tests and
// by the eventual server-side trip derivation.
func DistanceM(a, b Point) float64 {
	lat1 := a.Lat * math.Pi / 180
	lat2 := b.Lat * math.Pi / 180
	dLat := lat2 - lat1
	dLon := (b.Lon - a.Lon) * math.Pi / 180

	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(h)))
}

func deviceID(i int) string {
	const prefix = "sim-vehicle-"
	// Zero-padded so lexical and numeric ordering agree, which keeps test
	// output and dashboard listings sane past nine vehicles.
	digits := []byte{byte('0' + (i/100)%10), byte('0' + (i/10)%10), byte('0' + i%10)}
	return prefix + string(digits)
}
