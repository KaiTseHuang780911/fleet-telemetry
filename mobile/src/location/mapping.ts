/**
 * Pure mapping from a platform location fix to the fields the server stores.
 *
 * Separate from `task.ts` for the same reason `policy.ts` is separate from
 * `sqlite.ts`: this is the part most likely to be subtly wrong, and it should
 * be testable without pulling in expo-sqlite and the rest of the native module
 * graph. Importing the task module into a unit test fails outright, which is a
 * useful signal rather than an inconvenience — it means the logic was in the
 * wrong place.
 */

export type MotionState = 'still' | 'walking' | 'driving' | 'unknown';

/**
 * Android reports heading as -1 when it has no fix on direction, which is
 * common while stationary, and expo passes that straight through.
 *
 * Storing -1 would fail the server's `heading_deg BETWEEN 0 AND 360` check and
 * get the whole reading rejected — so a parked vehicle would quietly stop
 * reporting at exactly the moment its stop was being recorded. Unknown is
 * expressed as absent, never as a sentinel.
 */
export function normaliseHeading(heading: number | null | undefined): number | undefined {
  if (heading == null || heading < 0) return undefined;
  return heading % 360;
}

/**
 * A coarse motion hint derived from speed alone.
 *
 * Deliberately crude: the server derives its own stops from the position
 * stream and does not trust this field for anything load-bearing. It exists so
 * a human reading raw rows can see roughly what was happening. Real activity
 * recognition is a later concern.
 *
 * The important distinction is between *unknown* and *still*. Collapsing "the
 * sensor reported nothing" into "the vehicle was stationary" is the same error
 * as storing a missing speed as zero, and it would corrupt every idle-time
 * aggregate downstream.
 */
export function motionFrom(speed: number | null | undefined): MotionState {
  if (speed == null || speed < 0) return 'unknown';
  if (speed < 0.5) return 'still';
  if (speed < 2.5) return 'walking';
  return 'driving';
}

/**
 * Converts a platform battery level to the percentage the server stores.
 *
 * expo-battery reports a fraction in [0, 1] and **-1 when the level is
 * unavailable**, which is the same sentinel-as-data trap as Android's heading.
 * The server constrains battery_pct to [0, 100] and rejects the whole reading
 * outside it, so passing -1 through would discard a perfectly good position for
 * the sake of a field nothing depends on.
 *
 * Rounded to an integer because the column is a smallint, and clamped because a
 * float fraction can land a hair outside its range and turn 1.0000001 into 100
 * by luck rather than by rule.
 */
export function batteryPctFrom(level: number | null | undefined): number | undefined {
  if (level == null || !Number.isFinite(level) || level < 0) return undefined;
  return Math.round(Math.min(1, level) * 100);
}
