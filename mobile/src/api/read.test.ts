import { ApiError, listPositions, listStops, listVehicles } from './read';

function jsonResponse(status: number, body: unknown) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** Captures the URL a call requested, so query building can be asserted. */
function spyFetch(response: Response) {
  const calls: string[] = [];
  const impl = (async (url: string) => {
    calls.push(String(url));
    return response;
  }) as unknown as typeof fetch;
  return { calls, impl };
}

const base = { baseUrl: 'http://api.test' };

describe('reading positions', () => {
  it('requests the window it was given', async () => {
    const { calls, impl } = spyFetch(jsonResponse(200, { positions: [], truncated: false }));

    await listPositions(
      'veh-1',
      { from: new Date('2026-09-18T00:00:00Z'), to: new Date('2026-09-19T00:00:00Z'), limit: 500 },
      { ...base, fetchImpl: impl },
    );

    expect(calls[0]).toContain('/v1/vehicles/veh-1/positions');
    expect(calls[0]).toContain('from=2026-09-18T00%3A00%3A00.000Z');
    expect(calls[0]).toContain('limit=500');
  });

  // The flag exists because a truncated route looks exactly like a short one
  // once drawn. Losing it in parsing would put the failure back out of sight.
  it('carries the truncation flag through to the caller', async () => {
    const { impl } = spyFetch(jsonResponse(200, { positions: [], truncated: true }));

    const page = await listPositions('veh-1', {}, { ...base, fetchImpl: impl });

    expect(page.truncated).toBe(true);
  });

  it('omits query parameters that were not supplied', async () => {
    const { calls, impl } = spyFetch(jsonResponse(200, { positions: [], truncated: false }));

    await listPositions('veh-1', {}, { ...base, fetchImpl: impl });

    expect(calls[0]).toBe('http://api.test/v1/vehicles/veh-1/positions');
  });

  // A vehicle id is a UUID today, but the path is built from a value the
  // caller supplies and must not be able to escape its segment.
  it('encodes the vehicle id into the path', async () => {
    const { calls, impl } = spyFetch(jsonResponse(200, { positions: [], truncated: false }));

    await listPositions('a/b?c', {}, { ...base, fetchImpl: impl });

    expect(calls[0]).toContain('/v1/vehicles/a%2Fb%3Fc/positions');
  });
});

describe('reading stops', () => {
  // Omitting source returns both client and derived, which double-counts every
  // stop for a caller who does not know the two exist.
  it('narrows to one source when asked', async () => {
    const { calls, impl } = spyFetch(jsonResponse(200, []));

    await listStops('veh-1', { source: 'client' }, { ...base, fetchImpl: impl });

    expect(calls[0]).toContain('source=client');
  });

  it('does not ask for a source when none was given', async () => {
    const { calls, impl } = spyFetch(jsonResponse(200, []));

    await listStops('veh-1', {}, { ...base, fetchImpl: impl });

    expect(calls[0]).not.toContain('source=');
  });
});

describe('failures', () => {
  // The API reports problems as {"error": "..."}. Showing "HTTP 400" instead
  // would throw away the only part a person can act on.
  it('surfaces the server’s own error message', async () => {
    const { impl } = spyFetch(jsonResponse(400, { error: 'from must be earlier than to' }));

    await expect(listVehicles({ ...base, fetchImpl: impl })).rejects.toThrow(
      /from must be earlier than to/,
    );
  });

  it('falls back to the status when the body is not JSON', async () => {
    const impl = (async () => new Response('<html>502</html>', { status: 502 })) as unknown as typeof fetch;

    await expect(listVehicles({ ...base, fetchImpl: impl })).rejects.toThrow(/HTTP 502/);
  });

  it('reports the status code on the error', async () => {
    const { impl } = spyFetch(jsonResponse(404, { error: 'no such vehicle' }));

    await expect(listVehicles({ ...base, fetchImpl: impl })).rejects.toMatchObject({
      name: 'ApiError',
      status: 404,
    });
  });

  // fetch has no default timeout on React Native, so a host that drops packets
  // — a phone on the wrong Wi-Fi — would hang with a spinner and no reason.
  it('gives up after the timeout and says so in seconds', async () => {
    const impl = ((_url: string, init?: RequestInit) =>
      new Promise((_resolve, reject) => {
        init?.signal?.addEventListener('abort', () => {
          const err = new Error('Aborted');
          err.name = 'AbortError';
          reject(err);
        });
      })) as unknown as typeof fetch;

    await expect(
      listVehicles({ ...base, fetchImpl: impl, timeoutMs: 20 }),
    ).rejects.toThrow(/did not answer within/);
  });

  it('names an unreachable server rather than leaking a stack trace', async () => {
    const impl = (async () => {
      throw new TypeError('Network request failed');
    }) as unknown as typeof fetch;

    await expect(listVehicles({ ...base, fetchImpl: impl })).rejects.toThrow(
      /could not reach the server/,
    );
  });

  it('reports a non-JSON success body as such', async () => {
    const impl = (async () => new Response('not json', { status: 200 })) as unknown as typeof fetch;

    await expect(listVehicles({ ...base, fetchImpl: impl })).rejects.toThrow(/not JSON/);
  });

  it('throws ApiError rather than a bare Error', async () => {
    const { impl } = spyFetch(jsonResponse(500, { error: 'boom' }));

    await expect(listVehicles({ ...base, fetchImpl: impl })).rejects.toBeInstanceOf(ApiError);
  });
});
