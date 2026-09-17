/**
 * On-device arrival and departure detection.
 *
 * ADR-002 decided that stops come from two independent detectors — this one,
 * and the server's pass over the position stream — which are then reconciled so
 * their disagreement is a measurable number rather than an opinion. The server
 * half shipped in Phase 1. This is the half that has been missing, which is why
 * `stop_event_matches` has never held a row.
 *
 * **Incremental, not batch.** The background task receives a handful of fixes
 * per delivery and the process can be killed between deliveries, so the
 * detector cannot hold a window of history in memory. Every call takes the
 * previous state and returns the next one, and that state is small enough to
 * persist as JSON beside the queue. A batch algorithm would be simpler to write
 * and would lose every stop that straddled a process death — which, for an app
 * Android is free to kill at any moment, is most of them.
 *
 * **Pure.** No clock, no storage, no network: the caller supplies the fixes and
 * writes the state back. Same reasoning as `policy.ts` and `mapping.ts` — this
 * is the part most likely to be subtly wrong, so it has to be testable
 * exhaustively and instantly rather than against a device on a driveway.
 */

/** One fix, reduced to what detection actually needs. */
export interface Fix {
  lat: number;
  lon: number;
  /** Epoch ms. The device clock; see the note on clock trust below. */
  at: number;
  /** Metres per second, or null when the platform did not report it. */
  speedMps: number | null;
  /** Reported accuracy in metres, or null. Used to reject junk fixes. */
  accuracyM: number | null;
}

export interface StopThresholds {
  /** At or below this speed the vehicle counts as stationary. */
  stillSpeedMps: number;
  /** How far a fix may drift from the anchor and still be the same stop. */
  radiusM: number;
  /** How long it must stay inside the radius before this is called a stop. */
  minDwellMs: number;
  /**
   * Fixes worse than this are ignored entirely.
   *
   * A 100 m fix cannot distinguish "parked" from "moved half a block", so
   * feeding it to a 50 m radius test produces confident nonsense in both
   * directions. Dropping it costs nothing: the next fix is seconds away, and a
   * stop is defined by minutes.
   */
  maxAccuracyM: number;
}

/**
 * Defaults, and the reasoning behind each — all four are guesses until the
 * reconciliation table has real data to tune them against.
 *
 * `radiusM` deliberately matches the server's derivation radius. The whole
 * point of running two detectors is comparing them, and a client using a
 * different radius would manufacture disagreement that says nothing about
 * either detector's quality.
 */
export const DEFAULT_STOP_THRESHOLDS: StopThresholds = {
  // Below walking pace. GPS speed hovers around 0.2-0.8 m/s when stationary
  // rather than reading a clean zero.
  stillSpeedMps: 1,
  radiusM: 50,
  // Long enough to exclude traffic lights and short queues, short enough that a
  // real delivery stop is not missed. This is the threshold most likely to be
  // wrong, and the one the matching pass will have the most to say about.
  minDwellMs: 90_000,
  maxAccuracyM: 50,
};

/**
 * Detector state between deliveries. Serialised to JSON and stored in SQLite,
 * so it must stay small and must survive a round trip through `JSON.parse`.
 */
export interface StopState {
  /** Where the vehicle has been sitting, and since when. */
  anchor: { lat: number; lon: number; at: number } | null;
  /** Last fix seen inside the anchor radius; this becomes the departure time. */
  lastInsideAt: number | null;
  /** Set once dwell passed the threshold and the arrival was reported. */
  openStop: { eventId: string; lat: number; lon: number; arrivedAt: number } | null;
}

export const INITIAL_STOP_STATE: StopState = {
  anchor: null,
  lastInsideAt: null,
  openStop: null,
};

/** What the detector wants reported, if anything. */
export type StopEmission =
  /** Dwell threshold passed: a stop has begun. `departedAt` is absent. */
  | { kind: 'arrived'; eventId: string; lat: number; lon: number; arrivedAt: number }
  /** The vehicle left a stop that was already reported. Same eventId. */
  | {
      kind: 'departed';
      eventId: string;
      lat: number;
      lon: number;
      arrivedAt: number;
      departedAt: number;
    };

export interface StopDetectionResult {
  state: StopState;
  emissions: StopEmission[];
}

const EARTH_RADIUS_M = 6_371_000;

/**
 * Great-circle distance. Haversine rather than the cheaper equirectangular
 * approximation: the cost is irrelevant at these volumes, and the approximation
 * degrades with latitude in a way that would quietly change the effective
 * radius depending on where the vehicle is.
 */
export function distanceM(
  a: { lat: number; lon: number },
  b: { lat: number; lon: number },
): number {
  const toRad = (deg: number) => (deg * Math.PI) / 180;
  const dLat = toRad(b.lat - a.lat);
  const dLon = toRad(b.lon - a.lon);
  const lat1 = toRad(a.lat);
  const lat2 = toRad(b.lat);

  const h =
    Math.sin(dLat / 2) ** 2 + Math.sin(dLon / 2) ** 2 * Math.cos(lat1) * Math.cos(lat2);
  return 2 * EARTH_RADIUS_M * Math.asin(Math.min(1, Math.sqrt(h)));
}

/** Whether a fix says the vehicle is stationary. */
function isStationary(fix: Fix, thresholds: StopThresholds): boolean {
  // A null speed is unknown, not zero — but unlike the motion_state mapping,
  // treating it as "possibly stationary" is right here: the radius test is what
  // actually decides, and a device that never reports speed would otherwise
  // never record a stop at all.
  return fix.speedMps === null || fix.speedMps <= thresholds.stillSpeedMps;
}

/**
 * Fold fixes into the detector state, returning whatever should be reported.
 *
 * `nextEventId` is injected rather than generated here so the function stays
 * pure and tests can assert on exact ids. It is called at most once per
 * arrival.
 *
 * **On clock trust.** Everything here uses the device clock, and ADR-001 is
 * explicit that trip and stop boundaries must never be derived from it alone.
 * That rule is satisfied by the architecture rather than by this function: what
 * the device reports is stored as `source = 'client'` and reconciled against
 * the server's own derivation, which uses the position stream the server
 * received. A skewed client clock shows up as disagreement in
 * `stop_event_matches`, which is exactly where it should show up.
 */
export function detectStops(
  state: StopState,
  fixes: Fix[],
  nextEventId: () => string,
  thresholds: StopThresholds = DEFAULT_STOP_THRESHOLDS,
): StopDetectionResult {
  let next: StopState = state;
  const emissions: StopEmission[] = [];

  // Oldest first. A delivery can batch fixes, and Android does not promise
  // order; processing them out of order would move the anchor backwards in
  // time and corrupt every dwell calculation after it.
  const ordered = [...fixes].sort((a, b) => a.at - b.at);

  for (const fix of ordered) {
    if (fix.accuracyM !== null && fix.accuracyM > thresholds.maxAccuracyM) continue;

    // A fix older than what has already been folded in tells us nothing new and
    // would rewind the dwell window.
    if (next.lastInsideAt !== null && fix.at < next.lastInsideAt) continue;

    next = foldFix(next, fix, nextEventId, thresholds, emissions);
  }

  return { state: next, emissions };
}

function foldFix(
  state: StopState,
  fix: Fix,
  nextEventId: () => string,
  thresholds: StopThresholds,
  emissions: StopEmission[],
): StopState {
  const anchor = state.anchor;

  // Still near the anchor: the stop continues, or begins.
  if (anchor && distanceM(anchor, fix) <= thresholds.radiusM) {
    const dwell = fix.at - anchor.at;

    if (!state.openStop && dwell >= thresholds.minDwellMs) {
      const eventId = nextEventId();
      emissions.push({
        kind: 'arrived',
        eventId,
        lat: anchor.lat,
        lon: anchor.lon,
        arrivedAt: anchor.at,
      });
      return {
        anchor,
        lastInsideAt: fix.at,
        openStop: { eventId, lat: anchor.lat, lon: anchor.lon, arrivedAt: anchor.at },
      };
    }

    return { ...state, lastInsideAt: fix.at };
  }

  // Outside the radius, or there was no anchor. Either way this fix starts a
  // new candidate — but first, close whatever was open.
  if (state.openStop) {
    emissions.push({
      kind: 'departed',
      eventId: state.openStop.eventId,
      lat: state.openStop.lat,
      lon: state.openStop.lon,
      arrivedAt: state.openStop.arrivedAt,
      // The last fix seen *inside* the radius, not this one. This fix is
      // already elsewhere, so using its timestamp would stretch the stop
      // across however long the vehicle spent driving away — or across a
      // coverage gap, which can be hours.
      departedAt: state.lastInsideAt ?? state.openStop.arrivedAt,
    });
  }

  // A moving fix is not a candidate anchor. Anchoring on it would start a dwell
  // window at a spot the vehicle is already leaving, and the next fix would
  // simply clear it — harmless, but it makes the state churn and the traces
  // harder to read.
  if (!isStationary(fix, thresholds)) {
    return { anchor: null, lastInsideAt: null, openStop: null };
  }

  return {
    anchor: { lat: fix.lat, lon: fix.lon, at: fix.at },
    lastInsideAt: fix.at,
    openStop: null,
  };
}
