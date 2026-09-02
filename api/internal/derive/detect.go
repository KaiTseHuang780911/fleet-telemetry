// Package derive reconstructs trips and stop events from a vehicle's position
// stream, and reconciles them against what the device reported for itself.
//
// Derivation recomputes a whole time window rather than reacting to readings as
// they arrive. That is not a simplification — it is required. A device coming
// out of a signal dead zone delivers hours of backlogged readings at once, so a
// streaming detector would already have closed trips covering that period and
// then be handed the evidence afterwards. Recomputation is also idempotent,
// which makes retrying it free and testing it possible.
//
// Everything in this file is pure: samples in, events out, no database and no
// clock. The store layer handles persistence, and Reconcile compares the two
// sources. See ADR-004.
package derive

import (
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/store"
)

const earthRadiusM = 6_371_000

// Config holds the detection thresholds.
//
// These are untuned starting values, not measured ones. Real tuning needs real
// traces, and until then honest defaults with a knob beat confident-looking
// constants. Every one is overridable by environment variable.
type Config struct {
	// StopRadiusM: samples staying within this distance of the cluster's anchor
	// count as the same stop. Wide enough to absorb GPS noise while parked
	// (which is several metres even with a clear sky), narrow enough not to
	// swallow a slow crawl through a car park.
	StopRadiusM float64

	// StopMinDuration: how long the vehicle must stay inside StopRadiusM before
	// it counts as stopped. Filters out traffic lights, which would otherwise
	// dominate the stop count and drown the deliveries you care about.
	StopMinDuration time.Duration

	// TripGapDuration: a gap between consecutive samples longer than this ends
	// the trip. The device was off, out of battery, or out of coverage, and
	// pretending the vehicle travelled in a straight line across the gap would
	// invent distance that never happened.
	TripGapDuration time.Duration

	// MatchMaxTimeDelta / MatchMaxDistance: how far apart a client-reported and
	// a server-derived stop may be while still being considered the same event.
	MatchMaxTimeDelta time.Duration
	MatchMaxDistance  float64
}

// DefaultConfig returns the starting thresholds. See Config for the reasoning.
func DefaultConfig() Config {
	return Config{
		StopRadiusM:       50,
		StopMinDuration:   120 * time.Second,
		TripGapDuration:   10 * time.Minute,
		MatchMaxTimeDelta: 120 * time.Second,
		MatchMaxDistance:  100,
	}
}

// Result is one vehicle's derived view of a window.
type Result struct {
	Trips []store.Trip
	Stops []store.StopEvent
}

// Detect reconstructs trips and stops from samples ordered by recorded_at.
//
// The shape is a single pass that groups consecutive samples into stop
// clusters, then treats whatever lies between stops as movement.
func Detect(vehicleID uuid.UUID, samples []store.Sample, cfg Config) Result {
	out := Result{Trips: []store.Trip{}, Stops: []store.StopEvent{}}
	if len(samples) < 2 {
		// A single sample says where a vehicle was, never that it went
		// anywhere or stayed put for any length of time.
		return out
	}

	stops := detectStops(vehicleID, samples, cfg)
	trips := detectTrips(vehicleID, samples, stops, cfg)

	// Attach each stop to the trip that arrived at it: the latest trip ending
	// at or before the stop began.
	//
	// Note what this cannot be. Trips are the movement spans *between* stops,
	// so a stop's arrival never falls inside a trip's span — an earlier version
	// tested exactly that and consequently left every trip_id nil while looking
	// entirely reasonable. The link that means something is "the journey that
	// brought the vehicle here".
	//
	// A stop with no preceding trip keeps a nil trip_id. That is the honest
	// answer for the first stop in a window, where the arriving journey
	// happened before the data begins.
	for i := range stops {
		var best *store.Trip
		for j := range trips {
			endsAt := trips[j].EndedAt
			if endsAt == nil || endsAt.After(stops[i].ArrivedAt) {
				continue
			}
			if best == nil || endsAt.After(*best.EndedAt) {
				best = &trips[j]
			}
		}
		if best != nil {
			id := best.ID
			stops[i].TripID = &id
		}
	}

	out.Stops = stops
	out.Trips = trips
	return out
}

// detectStops groups consecutive samples that stay within StopRadiusM of the
// cluster anchor for at least StopMinDuration.
//
// The anchor is the first sample of the cluster rather than a running centroid.
// A centroid drifts: each new sample pulls it slightly, so a vehicle creeping
// forward can stay "within radius" indefinitely and a slow crawl reads as a
// stop. A fixed anchor cannot drift.
func detectStops(vehicleID uuid.UUID, samples []store.Sample, cfg Config) []store.StopEvent {
	stops := []store.StopEvent{}

	i := 0
	for i < len(samples) {
		anchor := samples[i]
		j := i + 1
		for j < len(samples) && distanceM(anchor.Lat, anchor.Lon, samples[j].Lat, samples[j].Lon) <= cfg.StopRadiusM {
			j++
		}

		// samples[i:j] all sit within the radius of the anchor.
		last := samples[j-1]
		dwell := last.RecordedAt.Sub(anchor.RecordedAt)

		if dwell >= cfg.StopMinDuration {
			departed := last.RecordedAt

			// The cluster ran to the end of the window, so the vehicle may
			// still be there. Leaving departed_at nil says "not known to have
			// left" rather than inventing a departure at the last sample.
			var departedAt *time.Time
			if j < len(samples) {
				departedAt = &departed
			}

			stops = append(stops, store.StopEvent{
				ID:         uuid.New(),
				VehicleID:  vehicleID,
				Source:     store.SourceDerived,
				ArrivedAt:  anchor.RecordedAt,
				DepartedAt: departedAt,
				Lat:        anchor.Lat,
				Lon:        anchor.Lon,
			})
			i = j
			continue
		}

		// Not a stop. Advance by one rather than to j: a cluster that was too
		// short starting here may still be long enough starting one sample
		// later, and skipping ahead would miss it.
		i++
	}

	return stops
}

// detectTrips treats the spans between stops as movement, splitting further
// wherever the sample gap exceeds TripGapDuration.
func detectTrips(vehicleID uuid.UUID, samples []store.Sample, stops []store.StopEvent, cfg Config) []store.Trip {
	trips := []store.Trip{}

	// A sample is "parked" if it falls inside any detected stop's span.
	parked := make([]bool, len(samples))
	for _, s := range stops {
		// An open stop has no departure, so it extends to the end of the data.
		end := samples[len(samples)-1].RecordedAt
		if s.DepartedAt != nil {
			end = *s.DepartedAt
		}
		for i := range samples {
			t := samples[i].RecordedAt
			if !t.Before(s.ArrivedAt) && !t.After(end) {
				parked[i] = true
			}
		}
	}

	var (
		current   []store.Sample
		flush     func()
		lastStamp time.Time
	)

	flush = func() {
		// Two samples minimum: one point is a location, not a journey.
		if len(current) < 2 {
			current = nil
			return
		}
		distance := 0.0
		for k := 1; k < len(current); k++ {
			distance += distanceM(current[k-1].Lat, current[k-1].Lon, current[k].Lat, current[k].Lon)
		}
		ended := current[len(current)-1].RecordedAt
		trips = append(trips, store.Trip{
			ID:        uuid.New(),
			VehicleID: vehicleID,
			Source:    store.SourceDerived,
			StartedAt: current[0].RecordedAt,
			EndedAt:   &ended,
			DistanceM: distance,
		})
		current = nil
	}

	for i, s := range samples {
		if parked[i] {
			flush()
			lastStamp = s.RecordedAt
			continue
		}
		if len(current) > 0 && s.RecordedAt.Sub(lastStamp) > cfg.TripGapDuration {
			// Coverage or power gap. End the trip here rather than drawing a
			// straight line across whatever happened in between.
			flush()
		}
		current = append(current, s)
		lastStamp = s.RecordedAt
	}
	flush()

	return trips
}

// distanceM is the great-circle distance in metres.
//
// Spherical, not ellipsoidal. The error is well under a metre at these
// distances, and GPS accuracy is several metres — precision beyond the sensor's
// is false precision.
func distanceM(lat1, lon1, lat2, lon2 float64) float64 {
	p1 := lat1 * math.Pi / 180
	p2 := lat2 * math.Pi / 180
	dPhi := p2 - p1
	dLambda := (lon2 - lon1) * math.Pi / 180

	h := math.Sin(dPhi/2)*math.Sin(dPhi/2) +
		math.Cos(p1)*math.Cos(p2)*math.Sin(dLambda/2)*math.Sin(dLambda/2)
	return 2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(h)))
}
