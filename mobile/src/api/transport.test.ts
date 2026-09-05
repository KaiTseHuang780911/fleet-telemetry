import { HttpTransport } from './transport';
import type { OutboxItem } from '../queue/types';
import { at } from '../testing';

function item(id: string, kind: OutboxItem['kind'] = 'position'): OutboxItem {
  return {
    id,
    kind,
    recordedAt: '2026-09-05T10:00:00.000Z',
    payload: { reading_id: id, lat: 49.28, lon: -123.12 },
    attempts: 0,
    lastError: null,
    createdAt: '2026-09-05T10:00:00.000Z',
  };
}

function jsonResponse(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  });
}

describe('HttpTransport', () => {
  it('posts readings and stop events in one request', async () => {
    const calls: Array<{ url: string; init: RequestInit }> = [];
    const fetchImpl = (async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      return jsonResponse(202, { accepted: 1, accepted_stops: 1 });
    }) as unknown as typeof fetch;

    const transport = new HttpTransport({ baseUrl: 'http://api.test/', fetchImpl });
    await transport.send('device-1', [item('a'), item('b', 'stop_event')]);

    expect(calls).toHaveLength(1);
    expect(at(calls, 0, 'calls').url).toBe('http://api.test/v1/telemetry');

    const body = JSON.parse(at(calls, 0, 'calls').init.body as string);
    expect(body.device_id).toBe('device-1');
    expect(body.readings).toHaveLength(1);
    expect(body.stop_events).toHaveLength(1);
    // The device's send time is what lets the server compute clock offset.
    expect(typeof body.sent_at).toBe('string');
  });

  // The server reports which ids it refused, not which it took. Acceptance is
  // derived, so the server need not echo every id back.
  it('derives accepted ids from what was not rejected', async () => {
    const fetchImpl = (async () =>
      jsonResponse(202, {
        accepted: 2,
        accepted_stops: 0,
        rejected: [{ id: 'bad', reason: 'lat 991 out of range [-90, 90]' }],
      })) as unknown as typeof fetch;

    const transport = new HttpTransport({ baseUrl: 'http://api.test', fetchImpl });
    const outcome = await transport.send('d', [item('good1'), item('bad'), item('good2')]);

    expect(outcome).toEqual({
      kind: 'accepted',
      acceptedIds: ['good1', 'good2'],
      rejectedIds: ['bad'],
    });
  });

  it('collects rejections from both readings and stop events', async () => {
    const fetchImpl = (async () =>
      jsonResponse(202, {
        accepted: 0,
        accepted_stops: 0,
        rejected: [{ id: 'r1', reason: 'bad reading' }],
        rejected_stops: [{ id: 's1', reason: 'bad stop' }],
      })) as unknown as typeof fetch;

    const transport = new HttpTransport({ baseUrl: 'http://api.test', fetchImpl });
    const outcome = await transport.send('d', [item('r1'), item('s1', 'stop_event')]);

    expect(outcome.kind).toBe('accepted');
    if (outcome.kind === 'accepted') {
      expect(outcome.rejectedIds.sort()).toEqual(['r1', 's1']);
      expect(outcome.acceptedIds).toEqual([]);
    }
  });

  describe('503 backpressure', () => {
    const cases: Array<{ name: string; header: string | null; expected: number }> = [
      { name: 'seconds', header: '2', expected: 2000 },
      { name: 'zero', header: '0', expected: 0 },
      { name: 'missing', header: null, expected: 2000 },
      { name: 'nonsense', header: 'soon', expected: 2000 },
    ];

    it.each(cases)('reads Retry-After given as $name', async ({ header, expected }) => {
      const headers: Record<string, string> = header === null ? {} : { 'Retry-After': header };
      const fetchImpl = (async () =>
        new Response('{}', { status: 503, headers })) as unknown as typeof fetch;

      const transport = new HttpTransport({ baseUrl: 'http://api.test', fetchImpl });
      const outcome = await transport.send('d', [item('a')]);

      expect(outcome).toEqual({ kind: 'shed', retryAfterMs: expected });
    });
  });

  it('treats a 4xx as our bug rather than a transient fault', async () => {
    const fetchImpl = (async () =>
      new Response('{"error":"batch must contain readings or stop_events"}', {
        status: 400,
      })) as unknown as typeof fetch;

    const transport = new HttpTransport({ baseUrl: 'http://api.test', fetchImpl });
    const outcome = await transport.send('d', [item('a')]);

    expect(outcome.kind).toBe('rejected');
    if (outcome.kind === 'rejected') expect(outcome.reason).toMatch(/400/);
  });

  it('treats a 5xx as transient', async () => {
    const fetchImpl = (async () =>
      new Response('upstream exploded', { status: 502 })) as unknown as typeof fetch;

    const transport = new HttpTransport({ baseUrl: 'http://api.test', fetchImpl });
    const outcome = await transport.send('d', [item('a')]);

    expect(outcome.kind).toBe('unavailable');
  });

  it('treats a network failure as transient', async () => {
    const fetchImpl = (async () => {
      throw new TypeError('Network request failed');
    }) as unknown as typeof fetch;

    const transport = new HttpTransport({ baseUrl: 'http://api.test', fetchImpl });
    const outcome = await transport.send('d', [item('a')]);

    expect(outcome).toEqual({ kind: 'unavailable', reason: 'Network request failed' });
  });

  it('reports a timeout in terms a human reads', async () => {
    const fetchImpl = (async () => {
      const err = new Error('aborted');
      err.name = 'AbortError';
      throw err;
    }) as unknown as typeof fetch;

    const transport = new HttpTransport({ baseUrl: 'http://api.test', fetchImpl });
    const outcome = await transport.send('d', [item('a')]);

    expect(outcome).toEqual({ kind: 'unavailable', reason: 'request timed out' });
  });

  // Accepted, but we cannot tell which items landed. Treating it as transient
  // means a resend, which the server's idempotency absorbs — the safe direction
  // to be wrong in, since assuming success would delete data on a guess.
  it('retries when a 202 body cannot be parsed', async () => {
    const fetchImpl = (async () =>
      new Response('not json at all', {
        status: 202,
        headers: { 'Content-Type': 'application/json' },
      })) as unknown as typeof fetch;

    const transport = new HttpTransport({ baseUrl: 'http://api.test', fetchImpl });
    const outcome = await transport.send('d', [item('a')]);

    expect(outcome.kind).toBe('unavailable');
  });

  it('does not double up slashes in the URL', async () => {
    const urls: string[] = [];
    const fetchImpl = (async (url: string) => {
      urls.push(url);
      return jsonResponse(202, { accepted: 1, accepted_stops: 0 });
    }) as unknown as typeof fetch;

    for (const base of ['http://api.test', 'http://api.test/', 'http://api.test///']) {
      await new HttpTransport({ baseUrl: base, fetchImpl }).send('d', [item('a')]);
    }

    expect(urls).toEqual([
      'http://api.test/v1/telemetry',
      'http://api.test/v1/telemetry',
      'http://api.test/v1/telemetry',
    ]);
  });
});
