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
import * as Battery from 'expo-battery';
import type { LocationObject } from 'expo-location';

import { HttpTransport } from '../api/transport';
import { API_BASE_URL, DEVICE_ID_SETTING } from '../config';
import { SqliteOutbox } from '../queue/sqlite';
import { SyncEngine } from '../queue/sync';
import { DEFAULT_SYNC_CONFIG } from '../queue/types';
import { makePositionItem, makeStopItem, newClientId } from '../telemetry/readings';
import {
  INITIAL_STOP_STATE,
  detectStops,
  type Fix,
  type StopEmission,
  type StopState,
} from '../stops/detect';
import { batteryPctFrom, motionFrom, normaliseHeading } from './mapping';

export const LOCATION_TASK = 'fleet-telemetry-location';

/** Written by the task, read by the UI, so the debug screen can show progress. */
export const LAST_FIX_SETTING = 'last_fix';
export const FIX_COUNT_SETTING = 'fix_count';
/** Detector state, JSON. Persisted because the process dies between fixes. */
export const STOP_STATE_SETTING = 'stop_state';
/**
 * Counters for what the detector emitted, so the debug screen can show whether
 * a stop was detected at all — distinct from whether the server stored it.
 *
 * The device reported nine arrivals and zero departures while every unit test
 * on both sides was green. Without this, "the detector never emitted it" and
 * "it was emitted and lost downstream" look identical from the outside.
 */
export const STOP_DEBUG_SETTING = 'stop_debug';

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
  //
  // `isolated` is what makes "fresh" true. expo-sqlite caches one native
  // connection per database name, so without it this opened the *UI's* handle
  // and the `finally` below closed it — breaking the running screen on every
  // single location fix, with the same NullPointerException this comment was
  // written to prevent.
  let store: SqliteOutbox | null = null;
  try {
    store = await SqliteOutbox.open(undefined, { isolated: true });

    const deviceId = await store.getSetting(DEVICE_ID_SETTING);
    if (!deviceId) {
      // The UI mints the device id on first launch. If it is missing the app
      // has never been opened, so there is nothing sensible to attribute these
      // fixes to. Dropping them beats inventing a second identity for the same
      // device, which would show up server-side as a phantom vehicle.
      console.warn('[location] no device id yet; dropping', locations.length, 'fix(es)');
      return;
    }

    // Read once per delivery, not per fix: a batch spans seconds, the charge
    // does not move in that time, and this is a native call on the path that
    // runs every ten seconds for an entire shift.
    const batteryPct = await readBatteryPct();

    const items = locations.map((fix) =>
      makePositionItem({
        lat: fix.coords.latitude,
        lon: fix.coords.longitude,
        // Nullable on purpose: absent means "not reported", which is different
        // from zero. A null speed is unknown; a zero speed is stationary.
        speedMps: fix.coords.speed ?? undefined,
        headingDeg: normaliseHeading(fix.coords.heading),
        accuracyM: fix.coords.accuracy ?? undefined,
        batteryPct,
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

    // Arrival detection runs on the same fixes, after they are safely queued.
    // Ordered that way on purpose: a bug in detection must not be able to cost
    // a position reading, which is the record everything else is derived from.
    await detectAndQueueStops(store, engine, locations);

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


/**
 * The device's charge level, or undefined if it cannot be read.
 *
 * Swallows its own failure on purpose. Battery is context for tuning the
 * sampling policy; a position is the record everything downstream derives from.
 * Losing a fix because a diagnostic field was unavailable would be a bad trade,
 * and this runs on the hot path where that trade would be made repeatedly.
 */
async function readBatteryPct(): Promise<number | undefined> {
  try {
    return batteryPctFrom(await Battery.getBatteryLevelAsync());
  } catch (err) {
    console.warn('[battery] unavailable:', err instanceof Error ? err.message : String(err));
    return undefined;
  }
}

/**
 * Feeds fixes to the stop detector and queues whatever it wants reported.
 *
 * The detector is pure and holds nothing between calls, so its state lives in
 * the same SQLite file as the queue — which means a stop spanning a process
 * death is still detected, and a stop spanning a *reinstall* is not. The second
 * is an acceptable loss; the first happens constantly.
 *
 * Failures here are swallowed deliberately. A detector that cannot read its
 * state should cost us stops, not positions, and the caller has already queued
 * the readings by the time this runs.
 */
async function detectAndQueueStops(
  store: SqliteOutbox,
  engine: SyncEngine,
  locations: LocationObject[],
): Promise<void> {
  try {
    const previous = await readStopState(store);
    const fixes: Fix[] = locations.map((fix) => ({
      lat: fix.coords.latitude,
      lon: fix.coords.longitude,
      at: fix.timestamp,
      speedMps: fix.coords.speed ?? null,
      accuracyM: fix.coords.accuracy ?? null,
    }));

    const { state, emissions } = detectStops(previous, fixes, newClientId);

    if (emissions.length > 0) {
      await engine.enqueue(
        emissions.map((event) =>
          makeStopItem({
            eventId: event.eventId,
            arrivedAt: new Date(event.arrivedAt),
            departedAt: event.kind === 'departed' ? new Date(event.departedAt) : undefined,
            lat: event.lat,
            lon: event.lon,
          }),
        ),
      );
    }

    // Written after the emissions are queued, never before. If the process dies
    // in between, the same stop is detected again and reported under a new
    // event id — a duplicate stop, which reconciliation can see. Writing state
    // first would instead lose the stop silently, which it could not.
    await store.setSetting(STOP_STATE_SETTING, JSON.stringify(state));

    // Instrumentation, deliberately last: nothing above it may be affected by
    // a failure to record diagnostics.
    await recordStopDebug(store, state, emissions);
  } catch (err) {
    console.error('[stops] detection failed:', err instanceof Error ? err.message : String(err));
  }
}

/** Running totals of what the detector emitted, plus its current shape. */
async function recordStopDebug(
  store: SqliteOutbox,
  state: StopState,
  emissions: StopEmission[],
): Promise<void> {
  let arrived = 0;
  let departed = 0;
  const raw = await store.getSetting(STOP_DEBUG_SETTING);
  if (raw) {
    try {
      const prev = JSON.parse(raw) as { arrived?: number; departed?: number };
      arrived = prev.arrived ?? 0;
      departed = prev.departed ?? 0;
    } catch {
      // Corrupt diagnostics are worth less than the detection they describe.
    }
  }

  for (const event of emissions) {
    if (event.kind === 'arrived') arrived += 1;
    else departed += 1;
  }

  await store.setSetting(
    STOP_DEBUG_SETTING,
    JSON.stringify({
      arrived,
      departed,
      // Whether the detector is currently holding a candidate and an open stop.
      // If `open` is false straight after an arrival, state is not surviving
      // the round trip and the departure can never fire.
      anchored: state.anchor !== null,
      open: state.openStop !== null,
    }),
  );
}

/** Reads detector state, treating anything unparseable as a fresh start. */
async function readStopState(store: SqliteOutbox): Promise<StopState> {
  const raw = await store.getSetting(STOP_STATE_SETTING);
  if (!raw) return INITIAL_STOP_STATE;
  try {
    // Shape is not validated beyond this. A state written by an older build
    // that no longer parses into something usable will simply produce a wrong
    // stop or two before being overwritten, which is a cheaper failure than
    // refusing to detect anything.
    return JSON.parse(raw) as StopState;
  } catch {
    return INITIAL_STOP_STATE;
  }
}
