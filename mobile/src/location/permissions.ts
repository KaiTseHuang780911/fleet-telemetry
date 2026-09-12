/**
 * Location permissions on Android, which are two separate grants rather than one.
 *
 * Foreground must be granted before background is even askable — Android
 * refuses the background request outright if the app does not already hold
 * foreground. And from Android 11 the background request **cannot show a
 * dialog at all**: the OS silently denies it and the only route is sending the
 * user into the app's settings page to choose "Allow all the time" themselves.
 *
 * Android 10 (API 29) still offers "Allow all the time" inline, so on that
 * version the request actually works. This module handles both, because
 * targetSdk is 36 and the app will meet newer devices.
 */

import * as Location from 'expo-location';
import { Linking, Platform } from 'react-native';

export type LocationGrant =
  /** Neither foreground nor background. Nothing can be tracked. */
  | 'none'
  /** Foreground only: tracking works while the app is visible, and stops when it is not. */
  | 'foreground'
  /** Both: tracking continues with the app backgrounded or the screen off. */
  | 'background';

export interface PermissionState {
  grant: LocationGrant;
  /**
   * True when the background grant was refused in a way that a further prompt
   * cannot change, so the UI must offer Settings instead of a retry button.
   */
  backgroundNeedsSettings: boolean;
}

export async function getPermissionState(): Promise<PermissionState> {
  const foreground = await Location.getForegroundPermissionsAsync();
  if (!foreground.granted) {
    return { grant: 'none', backgroundNeedsSettings: false };
  }

  const background = await Location.getBackgroundPermissionsAsync();
  if (background.granted) {
    return { grant: 'background', backgroundNeedsSettings: false };
  }

  return {
    grant: 'foreground',
    // canAskAgain false means the system will not prompt any more; Settings is
    // the only remaining path.
    backgroundNeedsSettings: !background.canAskAgain,
  };
}

/**
 * Requests foreground, then background, in that order.
 *
 * Deliberately sequential and deliberately tolerant of a background refusal:
 * foreground-only tracking is degraded but useful, so a refusal here is not an
 * error. What matters is that the caller can tell the difference, and say so.
 */
export async function requestLocationPermissions(): Promise<PermissionState> {
  const foreground = await Location.requestForegroundPermissionsAsync();
  if (!foreground.granted) {
    return { grant: 'none', backgroundNeedsSettings: false };
  }

  const background = await Location.requestBackgroundPermissionsAsync();
  if (background.granted) {
    return { grant: 'background', backgroundNeedsSettings: false };
  }

  return {
    grant: 'foreground',
    // On Android 11+ this request cannot prompt, so it returns denied without
    // the user having seen anything. Treating it as "needs Settings" is
    // correct there; on Android 10 the user genuinely chose "While using the
    // app" and canAskAgain tells us whether asking again is worthwhile.
    backgroundNeedsSettings:
      !background.canAskAgain || (Platform.OS === 'android' && Number(Platform.Version) >= 30),
  };
}

/** Opens this app's settings page, the only route to background on Android 11+. */
export async function openAppSettings(): Promise<void> {
  await Linking.openSettings();
}

export function describeGrant(state: PermissionState): string {
  switch (state.grant) {
    case 'background':
      return 'tracking continues in the background';
    case 'foreground':
      return 'foreground only — tracking pauses when the app is not in front';
    case 'none':
      return 'location permission not granted';
  }
}
