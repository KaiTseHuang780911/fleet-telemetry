/**
 * Location subscription configuration, and the one call that changes it.
 *
 * Split out of `service.ts` to break an import cycle. The background task needs
 * to change the sampling rate, `service.ts` imports the task for its
 * registration side effect, and a module cycle between them resolves only if
 * neither touches the other's bindings during evaluation — a condition that
 * holds today and would break silently the first time it did not. Every device
 * defect in this project has lived in the headless path; it is not the place to
 * rely on module-evaluation order.
 *
 * Nothing here imports `./task`.
 */

import * as Location from 'expo-location';

import { profileFor, type SamplingTier } from './sampling';

export const LOCATION_TASK = 'fleet-telemetry-location';

/**
 * Sampling configuration.
 *
 * High accuracy is GPS-backed, roughly 10m. That precision is not a luxury
 * here: the derivation pass clusters fixes within a 50m radius to detect a
 * stop, so a 100m error would put a parked vehicle outside its own stop and
 * make the thresholds meaningless.
 *
 * The two intervals do different jobs. distanceInterval stops a parked vehicle
 * emitting an identical fix every ten seconds; timeInterval ensures it still
 * emits *something* while parked, which is what proves it is still there
 * rather than simply gone. Either alone loses one of those.
 *
 * This is the heavy baseline. The battery-aware slice adapts these to motion
 * state and charge level, and measures against these numbers.
 */
export const LOCATION_OPTIONS: Location.LocationTaskOptions = {
  accuracy: Location.Accuracy.High,
  timeInterval: 10_000,
  distanceInterval: 20,

  foregroundService: {
    notificationTitle: 'Fleet Telemetry',
    notificationBody: 'Recording your route',
    notificationColor: '#238636',
    // Keep the service alive if the Activity is destroyed. Without this,
    // swiping the app away stops tracking mid-shift — which is precisely when
    // it needs to keep running.
    killServiceOnDestroy: false,
  },

  // Android shows a system dialog offering to turn on location services when
  // they are off, rather than the app failing with an error the driver cannot
  // act on.
  mayShowUserSettingsDialog: true,
};

/**
 * Re-applies location updates at a different sampling tier.
 *
 * expo-location has no way to adjust a running subscription, so changing the
 * rate means calling `startLocationUpdatesAsync` again with new options — which
 * tears the subscription down and rebuilds it. That is why the caller is
 * expected to have applied hysteresis first and to call this only when the tier
 * genuinely changed; doing it per fix would restart the subscription every ten
 * seconds and cost more than the reduced sampling saves.
 *
 * Does nothing when tracking is not running. Starting it here would turn a
 * battery policy into something that can switch tracking on behind the user's
 * back.
 */
export async function applySamplingTier(tier: SamplingTier): Promise<void> {
  // Checked here rather than taken on trust: starting updates from a battery
  // policy would switch tracking on behind the user's back.
  if (!(await Location.hasStartedLocationUpdatesAsync(LOCATION_TASK))) return;

  const profile = profileFor(tier);
  await Location.startLocationUpdatesAsync(LOCATION_TASK, {
    ...LOCATION_OPTIONS,
    timeInterval: profile.timeIntervalMs,
    distanceInterval: profile.distanceIntervalM,
  });
}
