package derive

import (
	"math"
	"sort"
	"time"

	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/store"
)

// Reconcile pairs client-reported stops with server-derived ones.
//
// A pair is eligible when the two are within MatchMaxTimeDelta and
// MatchMaxDistance of each other. Among eligible pairs the algorithm is greedy
// on closeness: consider every candidate pair, sort by how well they agree, and
// take them in order, skipping any whose client or derived event has already
// been claimed.
//
// Greedy rather than optimal assignment (Hungarian). Stops are sparse and well
// separated in time — a vehicle does not make two deliveries ninety seconds
// apart — so the two agree in practice, and greedy is a fraction of the code
// with none of the "why is this matrix here" explanation cost. If real data
// ever shows dense clusters where greedy picks badly, that is the moment to
// reach for optimal assignment, and the moment it becomes justifiable.
//
// The unmatched events on either side are the interesting output, not a
// leftover: a client stop the server never derived means the thresholds
// disagree, and a derived stop the device missed means on-device detection
// needs tuning.
func Reconcile(clientStops, derivedStops []store.StopEvent, cfg Config) []store.StopEventMatch {
	if len(clientStops) == 0 || len(derivedStops) == 0 {
		return []store.StopEventMatch{}
	}

	type candidate struct {
		clientIdx  int
		derivedIdx int
		deltaSecs  float64
		deltaM     float64
		// score blends time and distance into one ordering. Distance is scaled
		// by the ratio of the two tolerances so that "half the allowed time
		// error" and "half the allowed distance error" weigh the same; without
		// scaling, metres would dominate seconds purely because the numbers are
		// bigger.
		score float64
	}

	timeTolSecs := cfg.MatchMaxTimeDelta.Seconds()
	candidates := make([]candidate, 0, len(clientStops))

	for ci, c := range clientStops {
		for di, d := range derivedStops {
			deltaSecs := c.ArrivedAt.Sub(d.ArrivedAt).Seconds()
			if math.Abs(deltaSecs) > timeTolSecs {
				continue
			}
			deltaM := distanceM(c.Lat, c.Lon, d.Lat, d.Lon)
			if deltaM > cfg.MatchMaxDistance {
				continue
			}
			candidates = append(candidates, candidate{
				clientIdx:  ci,
				derivedIdx: di,
				deltaSecs:  deltaSecs,
				deltaM:     deltaM,
				score:      math.Abs(deltaSecs)/timeTolSecs + deltaM/cfg.MatchMaxDistance,
			})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score < candidates[j].score
		}
		// Deterministic tie-break. Without it, two equally good pairings could
		// be ordered differently between runs and the output would not be
		// reproducible.
		if candidates[i].clientIdx != candidates[j].clientIdx {
			return candidates[i].clientIdx < candidates[j].clientIdx
		}
		return candidates[i].derivedIdx < candidates[j].derivedIdx
	})

	clientTaken := make([]bool, len(clientStops))
	derivedTaken := make([]bool, len(derivedStops))
	matches := make([]store.StopEventMatch, 0, min(len(clientStops), len(derivedStops)))

	for _, cand := range candidates {
		if clientTaken[cand.clientIdx] || derivedTaken[cand.derivedIdx] {
			continue
		}
		clientTaken[cand.clientIdx] = true
		derivedTaken[cand.derivedIdx] = true

		matches = append(matches, store.StopEventMatch{
			ClientEventID:  clientStops[cand.clientIdx].ID,
			DerivedEventID: derivedStops[cand.derivedIdx].ID,
			// Signed, not absolute: a consistent sign means the device reports
			// arrival systematically early or late, which is a tuning signal.
			// Averaging absolute values would hide that bias entirely.
			DeltaSeconds: int(math.Round(cand.deltaSecs)),
			DeltaMeters:  cand.deltaM,
		})
	}

	return matches
}

// Agreement summarises a set of matches. Kept here rather than in the store so
// it can be computed and asserted on without a database.
type Agreement struct {
	ClientStops  int
	DerivedStops int
	Matched      int
	ClientOnly   int
	DerivedOnly  int

	MeanAbsDeltaSeconds float64
	MeanDeltaMeters     float64
	// MeanSignedDeltaSeconds exposes systematic bias: near zero means the two
	// sources disagree randomly, consistently negative or positive means one
	// fires earlier than the other every time.
	MeanSignedDeltaSeconds float64
}

// Summarise computes agreement statistics for one vehicle's window.
func Summarise(clientStops, derivedStops []store.StopEvent, matches []store.StopEventMatch) Agreement {
	a := Agreement{
		ClientStops:  len(clientStops),
		DerivedStops: len(derivedStops),
		Matched:      len(matches),
		ClientOnly:   len(clientStops) - len(matches),
		DerivedOnly:  len(derivedStops) - len(matches),
	}
	if len(matches) == 0 {
		return a
	}

	var absSecs, signedSecs, metres float64
	for _, m := range matches {
		absSecs += math.Abs(float64(m.DeltaSeconds))
		signedSecs += float64(m.DeltaSeconds)
		metres += m.DeltaMeters
	}
	n := float64(len(matches))
	a.MeanAbsDeltaSeconds = absSecs / n
	a.MeanSignedDeltaSeconds = signedSecs / n
	a.MeanDeltaMeters = metres / n
	return a
}

// WindowFor returns the derivation window ending at now.
//
// The lookback is generous on purpose. Derivation recomputes a window, so a
// trip straddling the window's start edge would be truncated; looking back
// further than the data actually needs makes that edge fall in already-settled
// history rather than in the middle of live activity. It also has to exceed the
// longest plausible offline backlog, or a device that was dark for six hours
// would deliver readings the next run no longer looks at.
func WindowFor(now time.Time, lookback time.Duration) (from, to time.Time) {
	return now.Add(-lookback), now
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
