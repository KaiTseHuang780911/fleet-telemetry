/**
 * HTTP transport for the outbox.
 *
 * Owns the wire format and status-code interpretation, and nothing else. The
 * queue's decisions — backoff, quarantine, dropping — live in the policy layer,
 * so this file translates HTTP into a `SendOutcome` and stops there.
 */

import type { OutboxItem, SendOutcome, Transport } from '../queue/types';

/** Mirrors the server's wire.IngestResponse. */
interface IngestResponse {
  accepted: number;
  accepted_stops: number;
  rejected?: Array<{ id: string; reason: string }>;
  rejected_stops?: Array<{ id: string; reason: string }>;
}

export interface HttpTransportOptions {
  baseUrl: string;
  /** Guards against a stalled connection holding a drain open indefinitely. */
  timeoutMs?: number;
  fetchImpl?: typeof fetch;
}

export class HttpTransport implements Transport {
  private readonly baseUrl: string;
  private readonly timeoutMs: number;
  private readonly fetchImpl: typeof fetch;

  constructor(opts: HttpTransportOptions) {
    this.baseUrl = opts.baseUrl.replace(/\/+$/, '');
    this.timeoutMs = opts.timeoutMs ?? 20_000;
    this.fetchImpl = opts.fetchImpl ?? fetch;
  }

  async send(deviceId: string, items: OutboxItem[]): Promise<SendOutcome> {
    const readings = items.filter((i) => i.kind === 'position').map((i) => i.payload);
    const stopEvents = items.filter((i) => i.kind === 'stop_event').map((i) => i.payload);

    const body = JSON.stringify({
      device_id: deviceId,
      // The device's clock at send time. The server compares it against its own
      // to derive a per-batch offset — which is what makes the device's
      // recorded_at values interpretable at all.
      sent_at: new Date().toISOString(),
      readings,
      stop_events: stopEvents,
    });

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);

    let response: Response;
    try {
      response = await this.fetchImpl(`${this.baseUrl}/v1/telemetry`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body,
        signal: controller.signal,
      });
    } catch (err) {
      // Offline, DNS failure, timeout, TLS error. All transient as far as the
      // queue is concerned: keep the data and try again later.
      return { kind: 'unavailable', reason: describeError(err) };
    } finally {
      clearTimeout(timer);
    }

    if (response.status === 503) {
      return { kind: 'shed', retryAfterMs: parseRetryAfter(response.headers.get('Retry-After')) };
    }

    if (response.status === 202) {
      let parsed: IngestResponse;
      try {
        parsed = (await response.json()) as IngestResponse;
      } catch (err) {
        // The server accepted the batch but we could not read the reply, so we
        // do not know which items landed. Treating it as transient means a
        // resend, which the server's idempotency absorbs — the safe direction
        // to be wrong in.
        return { kind: 'unavailable', reason: `unreadable 202 body: ${describeError(err)}` };
      }

      const rejectedIds = [
        ...(parsed.rejected ?? []).map((r) => r.id),
        ...(parsed.rejected_stops ?? []).map((r) => r.id),
      ];
      const rejectedSet = new Set(rejectedIds);

      // The server reports counts and the ids it refused, not the ids it took.
      // Everything sent that was not refused was accepted, so acceptance is
      // derived here rather than requiring the server to echo every id back.
      const acceptedIds = items.map((i) => i.id).filter((id) => !rejectedSet.has(id));

      return { kind: 'accepted', acceptedIds, rejectedIds };
    }

    if (response.status >= 400 && response.status < 500) {
      // Our request was malformed. Retrying identical bytes cannot help, so the
      // policy layer burns attempts and quarantines rather than looping.
      return { kind: 'rejected', reason: `HTTP ${response.status}: ${await safeText(response)}` };
    }

    return { kind: 'unavailable', reason: `HTTP ${response.status}: ${await safeText(response)}` };
  }
}

/**
 * Retry-After is seconds in the server's responses, though the header also
 * permits an HTTP date. Both are handled; anything unparseable falls back to a
 * sane default rather than retrying immediately.
 */
function parseRetryAfter(header: string | null): number {
  const fallback = 2_000;
  if (!header) return fallback;

  const seconds = Number(header);
  if (Number.isFinite(seconds) && seconds >= 0) return seconds * 1000;

  const date = Date.parse(header);
  if (!Number.isNaN(date)) return Math.max(0, date - Date.now());

  return fallback;
}

function describeError(err: unknown): string {
  if (err instanceof Error) {
    // AbortError is what a timeout looks like; the raw name is not useful in a
    // log a human reads.
    return err.name === 'AbortError' ? 'request timed out' : err.message;
  }
  return String(err);
}

async function safeText(response: Response): Promise<string> {
  try {
    const text = await response.text();
    return text.slice(0, 200);
  } catch {
    return '<unreadable body>';
  }
}
