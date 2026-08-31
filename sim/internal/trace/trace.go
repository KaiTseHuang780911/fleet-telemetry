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

// Movement parameters. Held as constants rather than configuration because
// they describe "what a delivery van does", not something an operator tunes.
const (
	cruiseSpeedMPS = 11.0 // ~40 km/h, plausible for urban driving
	speedJitterMPS = 3.0

	// Probability per tick of transitioning between moving and stopped. Tuned
	// so a vehicle stops every few minutes and dwells for a minute or two,
	// which is what generates useful stop_events downstream.
	chanceToStop  = 0.02
	chanceToStart = 0.10

	// A turn is applied every tick, but small — roads are mostly straight, and
	// a pure random walk looks nothing like driving.
	headingDriftDeg = 8.0

	batteryDrainPerTick = 0.004
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
	// stoppedTicks lets a dwell last a realistic while rather than ending on
	// the next coin flip.
	stoppedTicks int
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
	v.advance(dt)

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

func (v *Vehicle) advance(dt time.Duration) {
	if v.stopped {
		v.stoppedTicks--
		v.speedMPS = 0
		if v.stoppedTicks <= 0 && v.rng.Float64() < chanceToStart {
			v.stopped = false
			v.speedMPS = cruiseSpeedMPS
		}
	} else {
		if v.rng.Float64() < chanceToStop {
			v.stopped = true
			// 30 to 150 ticks of dwell.
			v.stoppedTicks = 30 + v.rng.Intn(120)
			v.speedMPS = 0
		} else {
			// Heading drifts a little each tick; roads bend, they do not
			// teleport.
			v.headingDeg = math.Mod(v.headingDeg+(v.rng.Float64()*2-1)*headingDriftDeg+360, 360)
			v.speedMPS = cruiseSpeedMPS + (v.rng.Float64()*2-1)*speedJitterMPS
			if v.speedMPS < 0 {
				v.speedMPS = 0
			}
			v.pos = move(v.pos, v.headingDeg, v.speedMPS*dt.Seconds())
		}
	}

	v.battery -= batteryDrainPerTick
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
