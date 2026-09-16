/**
 * What the tracking indicator is allowed to claim.
 *
 * This exists because the debug screen showed `tracking: running` in green for
 * the whole of a 40-minute walk that recorded zero fixes. Nothing had crashed:
 * both runtime permissions were granted, the foreground service was alive, and
 * the task was registered. The device's master location toggle was off, so
 * Android had nothing to deliver and said so to no one.
 *
 * The bug was the reporting. `hasStartedLocationUpdatesAsync` answers "is a
 * task registered", which the app was treating as "fixes are arriving". Those
 * differ in exactly the case that matters.
 *
 * Pure on purpose, and in its own module rather than in `service.ts`: importing
 * that module pulls in expo-location and the whole native graph, which a unit
 * test cannot load. The same lesson as `mapping.ts`.
 */

export type TrackingState =
  /** No task registered. Nothing is expected to happen. */
  | 'off'
  /**
   * Registered, but the device's location services are off. This is the state
   * that used to render as "running". No fix can arrive until the user changes
   * a system setting, so it is the app's job to say so.
   */
  | 'blocked'
  /** Registered and permitted, but nothing has ever been recorded. */
  | 'awaiting-fix'
  /** Registered, permitted, and at least one fix has been recorded. */
  | 'running';

export interface TrackingInputs {
  /** `Location.hasStartedLocationUpdatesAsync` — a task is registered. */
  registered: boolean;
  /** `Location.hasServicesEnabledAsync` — the device's location master switch. */
  servicesEnabled: boolean;
  /** Epoch ms of the most recent recorded fix, or null if there has never been one. */
  lastFixAt: number | null;
}

/**
 * Collapse the three facts into one label.
 *
 * Deliberately **not** a staleness heuristic. "No fix for two minutes" is the
 * normal, correct behaviour of a parked vehicle: `distanceInterval` is a hard
 * filter on Android, not a hint, so a stationary device emits nothing at all
 * and a timeout would flag every legitimate stop as a fault. Claiming a
 * failure that is not happening would repeat the original bug with the sign
 * flipped, so the only negative claim made here is one the system has already
 * stated as fact — services being off.
 *
 * `awaiting-fix` carries the ambiguous case without resolving it: it is
 * expected for the first few seconds after starting, and it is a loud problem
 * after ten minutes of driving. The screen shows elapsed time next to it and
 * lets the person reading it decide, because they know whether the vehicle is
 * moving and this function cannot.
 */
export function trackingState(inputs: TrackingInputs): TrackingState {
  if (!inputs.registered) return 'off';
  if (!inputs.servicesEnabled) return 'blocked';
  if (inputs.lastFixAt === null) return 'awaiting-fix';
  return 'running';
}

/** Text for the indicator. */
export function trackingLabel(state: TrackingState): string {
  switch (state) {
    case 'off':
      return 'stopped';
    case 'blocked':
      return 'location off';
    case 'awaiting-fix':
      return 'no fix yet';
    case 'running':
      return 'running';
  }
}

/**
 * Whether the indicator may be green.
 *
 * Only `running` qualifies. `awaiting-fix` is explicitly not green: during the
 * walk that prompted all this, green was the entire problem.
 */
export function isHealthy(state: TrackingState): boolean {
  return state === 'running';
}

/**
 * Whether the state needs the user to do something the app cannot do itself.
 *
 * Only `blocked`. Turning on location services requires a system setting, so
 * this is the one state where the right response is to send the user to
 * Settings rather than to retry.
 */
export function needsUserAction(state: TrackingState): boolean {
  return state === 'blocked';
}
