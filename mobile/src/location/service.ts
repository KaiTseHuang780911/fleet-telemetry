/**
 * Starting and stopping background location updates.
 *
 * Importing this module registers the background task as a side effect — see
 * `./task`, which must run at module scope so Android can find the task when
 * it restarts the app headlessly.
 */

import * as Location from 'expo-location';

import { LOCATION_TASK, LOCATION_OPTIONS } from './options';
// Imported for its registration side effect, not for a value. Without this the
// task name resolves to nothing when Android delivers a fix.
import './task';

export async function isTracking(): Promise<boolean> {
  return Location.hasStartedLocationUpdatesAsync(LOCATION_TASK);
}

/**
 * Whether the device's master location switch is on.
 *
 * Distinct from permissions, and the distinction is not academic: an app can
 * hold both location grants, register its task, and run its foreground service
 * while this is false — in which case Android delivers nothing, silently and
 * forever. A 40-minute walk was recorded that way, with the debug screen
 * showing "running" throughout. `isTracking()` alone cannot see it.
 */
export async function servicesEnabled(): Promise<boolean> {
  return Location.hasServicesEnabledAsync();
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
