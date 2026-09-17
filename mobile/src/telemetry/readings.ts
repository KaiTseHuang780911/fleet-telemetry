/**
 * Turns raw samples into outbox items.
 *
 * The id is generated exactly once, here, at capture time — never on retry.
 * That single property is what makes the whole pipeline idempotent: the server
 * deduplicates on this id, so a batch resent after a timeout is absorbed rather
 * than duplicated. Regenerating it anywhere downstream would silently defeat
 * every guarantee the queue claims to offer.
 */

// Must be imported before `uuid`. React Native has no crypto.getRandomValues,
// and uuid throws without it rather than falling back to a weak source — which
// is the right call, but it means the polyfill has to land first.
import 'react-native-get-random-values';
import { v7 as uuidv7 } from 'uuid';

import type { OutboxItem } from '../queue/types';

/**
 * A fresh client-generated id.
 *
 * Exported from this module rather than importing `uuid` at the call site
 * because the crypto polyfill above has to be loaded before `uuid` is touched,
 * and that is guaranteed here and nowhere else.
 */
export function newClientId(): string {
  return uuidv7();
}

/** A position sample, in the shape the server expects. */
export interface PositionSample {
  lat: number;
  lon: number;
  /** Omit rather than pass 0 when unknown — the server stores NULL, and NULL is
   *  genuinely different from "stationary". */
  speedMps?: number;
  headingDeg?: number;
  accuracyM?: number;
  batteryPct?: number;
  motionState?: 'still' | 'walking' | 'driving' | 'unknown';
  /** Defaults to now. Injectable so tests are deterministic. */
  recordedAt?: Date;
}

export function makePositionItem(sample: PositionSample): OutboxItem {
  const recordedAt = (sample.recordedAt ?? new Date()).toISOString();
  const id = uuidv7();

  return {
    id,
    kind: 'position',
    recordedAt,
    // snake_case here, not in the app's own types: this object goes on the wire
    // exactly as-is, so the mapping happens once, at the boundary.
    payload: pruneUndefined({
      reading_id: id,
      recorded_at: recordedAt,
      lat: sample.lat,
      lon: sample.lon,
      speed_mps: sample.speedMps,
      heading_deg: sample.headingDeg,
      accuracy_m: sample.accuracyM,
      battery_pct: sample.batteryPct,
      motion_state: sample.motionState,
    }),
    attempts: 0,
    lastError: null,
    createdAt: new Date().toISOString(),
  };
}

export interface StopSample {
  /**
   * Identifies the *stop*, and is deliberately supplied rather than generated
   * here: one stop is reported twice under this id, on arrival and again on
   * departure, and the server completes the first row from the second.
   */
  eventId: string;
  arrivedAt: Date;
  departedAt?: Date;
  lat: number;
  lon: number;
}

/**
 * Builds one report of a stop.
 *
 * Note that the outbox row id is freshly generated and is **not** the event id,
 * which is the opposite of `makePositionItem`. A position is reported once, so
 * one id serves both purposes; a stop is reported twice, and giving both
 * reports the same row id would make the queue's `INSERT OR IGNORE` discard the
 * departure as a duplicate — leaving the stop permanently open on the server.
 *
 * The transport translates between the two id spaces when the server names an
 * id it refused.
 */
export function makeStopItem(sample: StopSample): OutboxItem {
  const id = uuidv7();
  const arrivedAt = sample.arrivedAt.toISOString();

  return {
    id,
    kind: 'stop_event',
    // The arrival instant, for both reports. FIFO order in the queue then keeps
    // a stop's two reports in the order they happened.
    recordedAt: arrivedAt,
    payload: pruneUndefined({
      event_id: sample.eventId,
      arrived_at: arrivedAt,
      departed_at: sample.departedAt?.toISOString(),
      lat: sample.lat,
      lon: sample.lon,
    }),
    attempts: 0,
    lastError: null,
    createdAt: new Date().toISOString(),
  };
}

/**
 * Drops undefined keys so they are absent from the JSON rather than present as
 * null.
 *
 * The distinction matters. The server treats an absent optional field as "the
 * device did not report this" and stores NULL; an explicit null would mean the
 * same here, but sending `"speed_mps": null` where the field was simply never
 * measured is noise on a metered radio for no gain.
 */
function pruneUndefined<T extends Record<string, unknown>>(obj: T): T {
  const out = {} as T;
  for (const [key, value] of Object.entries(obj)) {
    if (value !== undefined) out[key as keyof T] = value as T[keyof T];
  }
  return out;
}
