/**
 * Config invariants that only fail on a real device.
 *
 * Two field tests were lost in two days to settings in `app.json` that no unit
 * test, typecheck, or CI job could see:
 *
 *  1. A release build blocked plain HTTP, because Expo injects
 *     `usesCleartextTraffic` into debug builds only. Every upload failed.
 *  2. The first location fix ever delivered crashed the process, because
 *     expo-task-manager schedules a *persisted* JobScheduler job and Android
 *     refuses to persist one without RECEIVE_BOOT_COMPLETED. The app crashed
 *     on every re-open until location was switched back off.
 *
 * Neither is a code defect, which is exactly why both survived to a device.
 * These tests assert the *relationships* between config values — if background
 * location is on, the permission that makes it survivable must be there too —
 * rather than restating each value, which would only prove `app.json` parses.
 */

import appJson from '../../app.json';

const android = appJson.expo.android;
const plugins: unknown[] = appJson.expo.plugins;

/** Finds a plugin entry whether it is bare or `[name, config]`. */
function pluginConfig(name: string): Record<string, unknown> | null {
  for (const entry of plugins) {
    if (entry === name) return {};
    if (Array.isArray(entry) && entry[0] === name) {
      return (entry[1] as Record<string, unknown>) ?? {};
    }
  }
  return null;
}

describe('android permissions', () => {
  // The crash, stated as the rule that would have prevented it.
  //
  // expo-task-manager delivers every background fix through a persisted
  // JobScheduler job, and JobScheduler throws IllegalArgumentException on the
  // main thread when asked to persist a job without this permission. The
  // library's own manifest registers a BOOT_COMPLETED receiver but does not
  // declare the permission, so the app must.
  it('declares RECEIVE_BOOT_COMPLETED whenever background location is enabled', () => {
    const location = pluginConfig('expo-location');
    if (!location?.['isAndroidBackgroundLocationEnabled']) {
      return; // Background location is off; the job is never scheduled.
    }

    expect(android.permissions).toContain('RECEIVE_BOOT_COMPLETED');
  });

  it('declares the foreground service permissions the location plugin needs', () => {
    const location = pluginConfig('expo-location');
    if (!location?.['isAndroidForegroundServiceEnabled']) return;

    expect(android.permissions).toContain('FOREGROUND_SERVICE');
    expect(android.permissions).toContain('FOREGROUND_SERVICE_LOCATION');
  });

  // Background location without foreground location is not a weaker request,
  // it is a broken one: Android grants background only as an escalation of an
  // already-granted foreground grant.
  it('does not ask for background location without foreground location', () => {
    if (!android.permissions.includes('ACCESS_BACKGROUND_LOCATION')) return;

    expect(android.permissions).toContain('ACCESS_FINE_LOCATION');
  });
});

describe('cleartext traffic', () => {
  /**
   * Finds the android block of the expo-build-properties plugin, if any.
   */
  function buildProperties(): Record<string, unknown> | null {
    const config = pluginConfig('expo-build-properties');
    if (!config) return null;
    return (config['android'] as Record<string, unknown>) ?? {};
  }

  // The bug that cost the first field test. Expo injects
  // usesCleartextTraffic into *debug* builds and not release ones, so the
  // first APK built to run standalone could not reach an http:// API at all.
  // Every upload failed with UnknownServiceException while the queue, quite
  // correctly, kept everything and uploaded nothing.
  //
  // Tied to the configured URL rather than asserted flatly, so that the day
  // the API moves to https this test stops demanding a permission the app no
  // longer needs — and fails if someone removes TLS without restoring it.
  it('permits cleartext whenever the API is reached over plain http', () => {
    // The committed fallback, since mobile/.env is gitignored and absent in CI.
    const configuredUrl = process.env['EXPO_PUBLIC_API_URL'] ?? 'http://10.0.2.2:8080';
    if (!configuredUrl.startsWith('http://')) return;

    expect(buildProperties()?.['usesCleartextTraffic']).toBe(true);
  });

  // The inverse, stated so the cleanup is not forgotten: once every
  // configured URL is https, this flag is a liability rather than a
  // convenience and should be deleted rather than narrowed.
  it('is the only thing standing between this build and a production one', () => {
    const permitsCleartext = buildProperties()?.['usesCleartextTraffic'] === true;
    const configuredUrl = process.env['EXPO_PUBLIC_API_URL'] ?? 'http://10.0.2.2:8080';

    if (permitsCleartext) {
      expect(configuredUrl.startsWith('http://')).toBe(true);
    }
  });
});

describe('plugins', () => {
  // The duplicate that briefly existed: expo-build-properties appeared twice,
  // once bare and once configured. Harmless by luck — the configured entry ran
  // second — and the opposite order would have silently dropped the config.
  it('lists each plugin exactly once', () => {
    const names = plugins.map((entry) => (Array.isArray(entry) ? entry[0] : entry));
    expect(new Set(names).size).toBe(names.length);
  });
});
