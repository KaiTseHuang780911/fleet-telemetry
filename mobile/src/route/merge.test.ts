import { describeSource, mergePositions, type MergeablePosition } from './merge';

function p(id: string, at: string, lat = 49.06, lon = -122.78): MergeablePosition {
  return { reading_id: id, recorded_at: at, lat, lon };
}

describe('mergePositions', () => {
  it('returns the union of both sources', () => {
    const result = mergePositions(
      [p('a', '2026-10-06T17:00:00Z')],
      [p('b', '2026-10-06T16:00:00Z')],
    );

    expect(result.positions.map((x) => x.reading_id)).toEqual(['b', 'a']);
  });

  // The same fix exists in both places by design: every reading is written to
  // local history and queued for upload. Showing it twice would double every
  // point on the route once a drive had drained.
  it('counts a fix present in both sources once', () => {
    const shared = p('a', '2026-10-06T17:00:00Z');
    const result = mergePositions([shared], [shared]);

    expect(result.positions).toHaveLength(1);
  });

  // A merge of two sorted lists is not sorted, and a route drawn from an
  // unsorted list is a scribble rather than a path.
  it('orders by time regardless of the order either source arrived in', () => {
    const local = [p('c', '2026-10-06T18:00:00Z'), p('a', '2026-10-06T16:00:00Z')];
    const server = [p('d', '2026-10-06T19:00:00Z'), p('b', '2026-10-06T17:00:00Z')];

    const result = mergePositions(local, server);

    expect(result.positions.map((x) => x.reading_id)).toEqual(['a', 'b', 'c', 'd']);
  });

  // The number that matters offline: how much the device holds that a server
  // round trip would not have returned.
  it('reports how many fixes exist only locally', () => {
    const shared = p('a', '2026-10-06T17:00:00Z');
    const result = mergePositions(
      [shared, p('b', '2026-10-06T17:01:00Z'), p('c', '2026-10-06T17:02:00Z')],
      [shared],
    );

    expect(result.localOnly).toBe(2);
  });

  it('reports no local-only fixes when everything has uploaded', () => {
    const rows = [p('a', '2026-10-06T17:00:00Z'), p('b', '2026-10-06T17:01:00Z')];

    expect(mergePositions(rows, rows).localOnly).toBe(0);
  });

  it.each([
    ['both', [p('a', '2026-10-06T17:00:00Z')], [p('b', '2026-10-06T16:00:00Z')]],
    ['server', [], [p('b', '2026-10-06T16:00:00Z')]],
    ['local', [p('a', '2026-10-06T17:00:00Z')], []],
  ])('reports the source as %s', (want, local, server) => {
    expect(mergePositions(local, server).source).toBe(want);
  });

  it('handles both sources being empty', () => {
    const result = mergePositions([], []);

    expect(result.positions).toEqual([]);
    expect(result.localOnly).toBe(0);
  });
});

describe('describeSource', () => {
  const synced = { source: 'both' as const, localOnly: 0 };

  // The screen used to print raw exception text at the driver:
  // "java.net.ConnectException: Failed to connect to /192.168.1.68:8080".
  // That is unactionable, and worse, it implied nothing was being shown when
  // the device had the data all along.
  it('says the device is offline rather than naming an exception', () => {
    const line = describeSource('offline', synced, true);

    expect(line).toBe('offline — showing what this device recorded');
    expect(line).not.toMatch(/Exception|192\.168|fetch/);
  });

  it('distinguishes an unreachable server from being offline', () => {
    expect(describeSource('online', synced, true)).toMatch(/server unreachable/);
  });

  it('says plainly when everything is synced', () => {
    expect(describeSource('online', synced, false)).toBe('synced');
  });

  // The useful case while driving: some of what is drawn has not uploaded yet.
  it('reports how much has not uploaded', () => {
    expect(describeSource('online', { source: 'both', localOnly: 12 }, false)).toBe(
      'synced, plus 12 not yet uploaded',
    );
  });

  it('never mentions a network error when nothing failed', () => {
    for (const network of ['online'] as const) {
      expect(describeSource(network, synced, false)).not.toMatch(/unreachable|offline/);
    }
  });
});
