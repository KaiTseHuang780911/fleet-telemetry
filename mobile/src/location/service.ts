/**
 * Starting and stopping background location updates.
 *
 * Importing this module registers the background task as a side effect — see
 * `./task`, which must run at module scope so Android can find the task when
 * it restarts the app headlessly.
 */

import * as Location from 'expo-location';

import { LOCATION_TASK } from './task';
// Imported for its registration side effect, not for a value. Without this the
// task name resolves to nothing when Android delivers a fix.
import './task';

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

export async function isTracking(): Promise<boolean> {
  return Location.hasStartedLocationUpdatesAsync(LOCATION_TASK);
}

/**
 * Starts location updates.
 *
 * Safe to call when already running: expo would otherwise stack registrations,
 * and the caller often cannot easily know the current state.
 */
export async function startTracking(): Promise<void> {
  if (await isTracking()) return;
  await Location.startLocationUpdatesAsync(LOCATION_TASK, LOCATION_OPTIONS);
}

export async function stopTracking(): Promise<void> {
  if (!(await isTracking())) return;
  await Location.stopLocationUpdatesAsync(LOCATION_TASK);
}

/**
 * One immediate fix, for the debug screen's "where am I now" button.
 *
 * Separate from the background stream on purpose — this is a user-initiated
 * question, and making it wait for the next scheduled update would make the
 * button feel broken.
 */
export async function getCurrentFix(): Promise<Location.LocationObject> {
  return Location.getCurrentPositionAsync({ accuracy: Location.Accuracy.High });
}
