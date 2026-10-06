/**
 * How often to ask Android for a fix.
 *
 * Slice 2 sampled at one fixed setting forever. ADR-008 measured what that
 * costs — 5.58 %/h, roughly 17.9 hours from full — and, more usefully, that
 * drain scales with *fix rate* rather than with driving: restricted to
 * stationary hours, ~25 fixes/h cost 2.86 %/h against ~137 fixes/h at
 * 11.43 %/h. Sampling rate is the lever, so this is what pulls it.
 *
 * Pure, like `policy.ts` and `detect.ts`. The tier decision is the part most
 * likely to be subtly wrong — a policy that flaps between tiers costs more than
 * it saves, and that failure is invisible except as a battery graph that did
 * not improve.
 */

import type { MotionState } from './mapping';

export type SamplingTier =
  /** The slice-2 baseline. What the vehicle gets while it is moving. */
  | 'active'
  /** Stationary: still enough to prove the vehicle is there, far less often. */
  | 'idle'
  /** Stationary and low on charge. The floor, not an absence of sampling. */
  | 'saver';

/** The knobs expo-location actually takes, minus the parts that never change. */
export interface SamplingProfile {
  timeIntervalMs: number;
  distanceIntervalM: number;
}

/**
 * The three tiers.
 *
 * `active` is deliberately unchanged from slice 2 so the before-and-after
 * comparison measures the *policy*, not a simultaneous retuning of the
 * baseline. Changing both at once would make the result uninterpretable.
 *
 * The `saver` numbers are a floor rather than an off switch. A shift that
 * records too little to reconstruct a route is worse than one that records
 * nothing, because sparse data still looks like evidence — and at 100 m there
 * is still enough to tell a parked vehicle from a departed one.
 */
export const TIERS: Record<SamplingTier, SamplingProfile> = {
  active: { timeIntervalMs: 10_000, distanceIntervalM: 20 },
  idle: { timeIntervalMs: 60_000, distanceIntervalM: 50 },
  saver: { timeIntervalMs: 180_000, distanceIntervalM: 100 },
};

export interface SamplingInputs {
  motion: MotionState;
  /** 0-100, or null when the level could not be read. */
  batteryPct: number | null;
  charging: boolean;
}

/** Below this, a stationary vehicle drops to the floor. */
export const LOW_BATTERY_PCT = 20;

/**
 * Which tier the current conditions call for, ignoring hysteresis.
 *
 * `unknown` motion counts as moving. The alternative — treating "the platform
 * did not say" as stationary — is the same error as storing a missing speed as
 * zero, and it fails in the expensive direction: a vehicle driving at the idle
 * tier produces a route that cannot be reconstructed, while a parked vehicle at
 * the active tier merely costs battery.
 *
 * Charging pins to `active` whatever else is true. There is no reason to
 * conserve from a wall socket, and a vehicle on a charger is usually one being
 * driven.
 */
export function tierFor(inputs: SamplingInputs): SamplingTier {
  if (inputs.charging) return 'active';

  const stationary = inputs.motion === 'still';
  if (!stationary) return 'active';

  // A null battery reading is unknown, not empty. Dropping to the floor on a
  // failed read would be a sampling policy driven by a diagnostic bug.
  if (inputs.batteryPct !== null && inputs.batteryPct < LOW_BATTERY_PCT) return 'saver';
  return 'idle';
}

/** What the caller remembers between evaluations. */
export interface SamplingState {
  tier: SamplingTier;
  /** When the current tier was adopted, epoch ms. */
  since: number;
  /**
   * A slower tier being considered, and when it was first wanted.
   *
   * The dwell has to measure how long the *wanted* tier has been stable, not
   * how long the current one has been in place. An earlier version timed the
   * latter, which meant a vehicle alternating between moving and stopped at a
   * junction accumulated dwell it had not earned and dropped to idle while
   * still driving. Cleared the moment conditions return to the current tier.
   */
  pending: { tier: SamplingTier; since: number } | null;
}

export const INITIAL_SAMPLING_STATE: SamplingState = {
  tier: 'active',
  since: 0,
  pending: null,
};

/**
 * Minimum time in a tier before another change is allowed.
 *
 * Hysteresis is mandatory here rather than a refinement. Changing the options
 * means calling `startLocationUpdatesAsync` again, which tears down and rebuilds
 * the location subscription; a policy that switches on every speed wobble would
 * restart it constantly, and the restarts plausibly cost more than the reduced
 * sampling saves. Two minutes is longer than a traffic light and shorter than
 * any real stop.
 */
export const MIN_TIER_DWELL_MS = 120_000;

export interface SamplingDecision {
  state: SamplingState;
  /** Set when the tier changed and the subscription must be restarted. */
  changedTo: SamplingTier | null;
}

/**
 * Decide whether to switch tiers, given how long the current one has held.
 *
 * Returns the next state and, only when a restart is actually needed, the tier
 * to apply. The caller restarts location updates if and only if `changedTo` is
 * set — which is what keeps a stable vehicle from restarting its subscription
 * every ten seconds.
 *
 * One asymmetry, deliberate: escalating *to* `active` skips the dwell check.
 * Dropping to a slower tier a couple of minutes late costs a little battery;
 * staying slow a couple of minutes after a vehicle starts moving costs the
 * start of the trip, and that data cannot be recovered.
 */
export function decideSampling(
  state: SamplingState,
  inputs: SamplingInputs,
  now: number,
): SamplingDecision {
  const wanted = tierFor(inputs);

  // Conditions match what is already running: abandon any slower tier that was
  // being considered, because it has not held.
  if (wanted === state.tier) {
    return { state: state.pending ? { ...state, pending: null } : state, changedTo: null };
  }

  // Escalating skips the wait entirely.
  if (wanted === 'active') {
    return { state: { tier: 'active', since: now, pending: null }, changedTo: 'active' };
  }

  // A different slower tier than the one being considered restarts the clock.
  if (state.pending?.tier !== wanted) {
    return { state: { ...state, pending: { tier: wanted, since: now } }, changedTo: null };
  }

  if (now - state.pending.since < MIN_TIER_DWELL_MS) {
    return { state, changedTo: null };
  }

  return { state: { tier: wanted, since: now, pending: null }, changedTo: wanted };
}

/** The profile for a tier. */
export function profileFor(tier: SamplingTier): SamplingProfile {
  return TIERS[tier];
}
