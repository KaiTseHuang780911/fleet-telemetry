/**
 * Reading telemetry back from the server.
 *
 * Everything in `transport.ts` pushes; this is the first code in the app that
 * pulls. The two are kept apart on purpose: the write path is the one with
 * durability guarantees, retry policy, and an outbox behind it, and none of
 * that applies to a screen fetching what it needs to draw. Merging them would
 * drag queue semantics into a read that should simply fail and let the user
 * pull to refresh.
 *
 * Shapes here mirror the Go types in `api/internal/store`. There is no code
 * generation keeping them in step — a deliberate tradeoff recorded in the
 * README — so a server change that renames a field surfaces as `undefined` at
 * the point of use rather than as a compile error. The parsing below is
 * defensive for that reason.
 */

import { API_BASE_URL } from '../config';

/** How long a read may take before it is abandoned. */
const DEFAULT_TIMEOUT_MS = 15_000;

export interface Vehicle {
  id: string;
  external_id: string;
  label: string | null;
  created_at: string;
}

export interface Position {
  reading_id: string;
  vehicle_id: string;
  recorded_at: string;
  received_at: string;
  lat: number;
  lon: number;
  speed_mps?: number;
  heading_deg?: number;
  accuracy_m?: number;
  battery_pct?: number;
  motion_state?: string;
}

export interface PositionPage {
  positions: Position[];
  /**
   * The server hit its row cap. Surfaced all the way to the UI because a
   * truncated route is indistinguishable from a short one once drawn.
   */
  truncated: boolean;
}

export interface StopEvent {
  id: string;
  vehicle_id: string;
  trip_id: string | null;
  source: 'client' | 'derived';
  arrived_at: string;
  /** Null while the vehicle is still there. An open stop is normal. */
  departed_at: string | null;
  lat: number;
  lon: number;
}

export interface Trip {
  id: string;
  vehicle_id: string;
  started_at: string;
  ended_at: string | null;
  distance_m: number;
}

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status?: number,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

export interface ReadOptions {
  baseUrl?: string;
  timeoutMs?: number;
  fetchImpl?: typeof fetch;
  signal?: AbortSignal;
}

/**
 * One GET, with a timeout and errors a person can read.
 *
 * The timeout is not optional politeness: `fetch` on React Native has no
 * default, so a request to a host that silently drops packets — which is what a
 * phone on the wrong Wi-Fi looks like — would otherwise hang until the screen
 * is closed, with a spinner and no explanation.
 */
async function getJson<T>(path: string, opts: ReadOptions = {}): Promise<T> {
  const baseUrl = (opts.baseUrl ?? API_BASE_URL).replace(/\/+$/, '');
  const doFetch = opts.fetchImpl ?? fetch;
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), opts.timeoutMs ?? DEFAULT_TIMEOUT_MS);

  // A caller-supplied signal (a screen unmounting) must also cancel the
  // request, so both are honoured.
  const onAbort = () => controller.abort();
  opts.signal?.addEventListener('abort', onAbort);

  let response: Response;
  try {
    response = await doFetch(`${baseUrl}${path}`, {
      method: 'GET',
      headers: { Accept: 'application/json' },
      signal: controller.signal,
    });
  } catch (err) {
    throw new ApiError(describeNetworkError(err, opts.timeoutMs ?? DEFAULT_TIMEOUT_MS));
  } finally {
    clearTimeout(timer);
    opts.signal?.removeEventListener('abort', onAbort);
  }

  if (!response.ok) {
    // The API reports failures as {"error": "..."}; prefer that to a bare code.
    let detail = `HTTP ${response.status}`;
    try {
      const body = (await response.json()) as { error?: unknown };
      if (typeof body?.error === 'string') detail = body.error;
    } catch {
      // Body was not JSON. The status is all we have and it is enough.
    }
    throw new ApiError(detail, response.status);
  }

  try {
    return (await response.json()) as T;
  } catch (err) {
    throw new ApiError(
      `the server sent a reply that is not JSON: ${err instanceof Error ? err.message : String(err)}`,
    );
  }
}

function describeNetworkError(err: unknown, timeoutMs: number): string {
  if (err instanceof Error && err.name === 'AbortError') {
    return `the server did not answer within ${Math.round(timeoutMs / 1000)}s`;
  }
  const detail = err instanceof Error ? err.message : String(err);
  // The overwhelmingly common cause in development, worth naming rather than
  // making someone recognise a stack trace.
  return `could not reach the server: ${detail}`;
}

export function listVehicles(opts?: ReadOptions): Promise<Vehicle[]> {
  return getJson<Vehicle[]>('/v1/vehicles', opts);
}

/** Builds ?from=&to=&limit=, omitting anything absent. */
function windowQuery(window: { from?: Date; to?: Date; limit?: number }): string {
  const params = new URLSearchParams();
  if (window.from) params.set('from', window.from.toISOString());
  if (window.to) params.set('to', window.to.toISOString());
  if (window.limit != null) params.set('limit', String(window.limit));
  const q = params.toString();
  return q ? `?${q}` : '';
}

export function listPositions(
  vehicleId: string,
  window: { from?: Date; to?: Date; limit?: number } = {},
  opts?: ReadOptions,
): Promise<PositionPage> {
  return getJson<PositionPage>(
    `/v1/vehicles/${encodeURIComponent(vehicleId)}/positions${windowQuery(window)}`,
    opts,
  );
}

export function listStops(
  vehicleId: string,
  window: { from?: Date; to?: Date; source?: 'client' | 'derived' } = {},
  opts?: ReadOptions,
): Promise<StopEvent[]> {
  const params = new URLSearchParams();
  if (window.from) params.set('from', window.from.toISOString());
  if (window.to) params.set('to', window.to.toISOString());
  // Defaults to both sources server-side, which double-counts every stop for a
  // caller who does not know the two exist. A screen drawing markers wants one.
  if (window.source) params.set('source', window.source);
  const q = params.toString();
  return getJson<StopEvent[]>(
    `/v1/vehicles/${encodeURIComponent(vehicleId)}/stops${q ? `?${q}` : ''}`,
    opts,
  );
}

export function listTrips(
  vehicleId: string,
  window: { from?: Date; to?: Date } = {},
  opts?: ReadOptions,
): Promise<Trip[]> {
  return getJson<Trip[]>(
    `/v1/vehicles/${encodeURIComponent(vehicleId)}/trips${windowQuery(window)}`,
    opts,
  );
}
