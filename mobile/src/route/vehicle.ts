/**
 * Choosing which vehicle a single-vehicle screen shows.
 *
 * Pure, and in its own module rather than in `RouteScreen`, because importing
 * that file pulls in expo-maps and a unit test cannot load the native module.
 * The same reason `mapping.ts` and `detect.ts` exist — logic that can be wrong
 * should be testable without a device.
 */

/**
 * The vehicle a single-vehicle screen should show: the one that reported most
 * recently.
 *
 * Taking the first row of a list sorted by creation picks the *oldest*, which
 * is exactly wrong after a device re-registers — the stale vehicle wins and the
 * live one is invisible. That cost an evening.
 *
 * Vehicles that have never reported lose to any that have, and only win when
 * nothing else is on offer.
 */
export function pickActiveVehicle<T extends { last_reported_at: string | null }>(
  vehicles: T[],
): T | undefined {
  let best: T | undefined;
  for (const v of vehicles) {
    if (!best) {
      best = v;
      continue;
    }
    if (v.last_reported_at === null) continue;
    if (best.last_reported_at === null || v.last_reported_at > best.last_reported_at) {
      best = v;
    }
  }
  return best;
}
