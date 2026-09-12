/**
 * The background location task.
 *
 * `TaskManager.defineTask` is called at **module scope**, on purpose. Android
 * can restart this app headlessly to deliver a location update — no Activity,
 * no React tree, no mounted component. The JS bundle is loaded and the task is
 * looked up by name. If it were registered inside a component effect it would
 * simply not exist at that moment, and the fix would be dropped silently.
 *
 * The same fact rules out touching anything React owns: there is no engine ref
 * and no component state to reach for here. The handler opens its own store,
 * writes, drains, and closes.
 */

import * as TaskManager from 'expo-task-manager';
import type { LocationObject } from 'expo-location';

import { HttpTransport } from '../api/transport';
import { API_BASE_URL, DEVICE_ID_SETTING } from '../config';
import { SqliteOutbox } from '../queue/sqlite';
import { SyncEngine } from '../queue/sync';
import { DEFAULT_SYNC_CONFIG } from '../queue/types';
import { makePositionItem } from '../telemetry/readings';
import { motionFrom, normaliseHeading } from './mapping';

export const LOCATION_TASK = 'fleet-telemetry-location';

/** Written by the task, read by the UI, so the debug screen can show progress. */
export const LAST_FIX_SETTING = 'last_fix';
export const FIX_COUNT_SETTING = 'fix_count';

interface LocationTaskData {
  locations: LocationObject[];
}

TaskManager.defineTask<LocationTaskData>(LOCATION_TASK, async ({ data, error }) => {
  if (error) {
    // Nothing useful can be done here — there is no UI to show it in. Logging
    // is the honest option; the next fix will either work or not.
    console.error('[location task]', error.message);
    return;
  }
  await recordLocations(data?.locations ?? []);
});

/**
 * Turns fixes into queued readings and attempts one drain.
 *
 * Extracted from the task handler so the mock route driver can call exactly
 * this, rather than a parallel implementation that could drift from it. What
 * the mock therefore covers is everything from a fix onwards: the wire mapping,
 * the queue, the drain, and the server. What it cannot cover is Android
 * actually producing the fix — GPS delivery, the foreground service, and the
 * headless restart are only exercised by the real provider.
 */
export async function recordLocations(locations: LocationObject[]): Promise<void> {
  if (locations.length === 0) return;

  // A fresh store per invocation rather than a module-level singleton. The
  // headless context may be torn down between deliveries, and a handle held
  // across that boundary is exactly the use-after-close that produced a
  // NullPointerException earlier in this project.
  let store: SqliteOutbox | null = null;
  try {
    store = await SqliteOutbox.open();

    const deviceId = await store.getSetting(DEVICE_ID_SETTING);
    if (!deviceId) {
      // The UI mints the device id on first launch. If it is missing the app
      // has never been opened, so there is nothing sensible to attribute these
      // fixes to. Dropping them beats inventing a second identity for the same
      // device, which would show up server-side as a phantom vehicle.
      console.warn('[location] no device id yet; dropping', locations.length, 'fix(es)');
      return;
    }

    const items = locations.map((fix) =>
      makePositionItem({
        lat: fix.coords.latitude,
        lon: fix.coords.longitude,
        // Nullable on purpose: absent means "not reported", which is different
        // from zero. A null speed is unknown; a zero speed is stationary.
        speedMps: fix.coords.speed ?? undefined,
        headingDeg: normaliseHeading(fix.coords.heading),
        accuracyM: fix.coords.accuracy ?? undefined,
        motionState: motionFrom(fix.coords.speed),
        // The device clock at the moment of the fix, not now. These can arrive
        // in a batch well after the fact.
        recordedAt: new Date(fix.timestamp),
      }),
    );

    const engine = new SyncEngine(
      store,
      new HttpTransport({
        baseUrl: API_BASE_URL,
        // Shorter than the UI's timeout. Android gives a headless task a
        // limited window and kills it without ceremony; a request still
        // hanging when that happens costs the whole invocation, so it is
        // better to give up early and let the queue retry.
        timeoutMs: 10_000,
      }),
      deviceId,
      DEFAULT_SYNC_CONFIG,
    );

    await engine.enqueue(items);

    const last = locations[locations.length - 1];
    if (last) {
      await store.setSetting(
        LAST_FIX_SETTING,
        JSON.stringify({
          lat: Number(last.coords.latitude.toFixed(5)),
          lon: Number(last.coords.longitude.toFixed(5)),
          accuracy: last.coords.accuracy == null ? null : Math.round(last.coords.accuracy),
          at: new Date(last.timestamp).toISOString(),
        }),
      );
      const seen = Number((await store.getSetting(FIX_COUNT_SETTING)) ?? '0');
      await store.setSetting(FIX_COUNT_SETTING, String(seen + locations.length));
    }

    // One batch, not a loop until empty. The task's time budget is short and
    // unenforced from here; draining everything could be interrupted anywhere,
    // whereas one bounded attempt either completes or leaves the queue exactly
    // as it was.
    await engine.drain();
  } catch (err) {
    console.error('[location] failed:', err instanceof Error ? err.message : String(err));
  } finally {
    // Always close. This runs repeatedly over a shift, and a handle leaked per
    // delivery is a handle leaked per ten seconds.
    await store?.close();
  }
}
