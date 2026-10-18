/**
 * Combining local history with what the server holds.
 *
 * The two overlap by design: every fix is written to local history *and*
 * queued for upload, so once a drive has drained, the same fix exists in both
 * places under the same client id. The map should show the union — the server's
 * archive reaching back further than the device keeps, and local history
 * covering what has not uploaded yet, which is exactly the part a driver
 * offline in a van most wants to see.
 */

/** The minimum a position needs to be drawable and de-duplicable. */
export interface MergeablePosition {
  /**
   * Client-generated id. The same value in both sources for the same fix.
   *
   * Field names are the wire shape rather than this app's usual camelCase,
   * because that is what both sources already produce — local history is read
   * back into it and the server returns it. An adapter in between would be a
   * mapping layer whose only job is to undo itself.
   */
  reading_id: string;
  /** ISO 8601. */
  recorded_at: string;
  lat: number;
  lon: number;
}

export type PositionSource = 'local' | 'server' | 'both';

export interface MergeResult<T extends MergeablePosition> {
  positions: T[];
  /** How many came only from local history — the part a server round trip misses. */
  localOnly: number;
  /** Where the drawn data came from, for the screen to say so honestly. */
  source: PositionSource;
}

/**
 * Union of both sources, oldest first, one row per client id.
 *
 * Server rows win a collision. Not because they are more accurate — they are
 * byte-identical, having come from this device — but because the server's copy
 * has been through validation, and preferring it means anything the two
 * disagree about surfaces as a server-side question rather than a local one.
 *
 * Sorted by `recorded_at` rather than trusting either source's order. Local
 * history is ordered by the device clock and the server's by the same field,
 * but a merge of two sorted lists is not sorted, and a route drawn from an
 * unsorted list is a scribble.
 */
export function mergePositions<T extends MergeablePosition>(
  local: T[],
  server: T[],
): MergeResult<T> {
  const byId = new Map<string, T>();

  for (const p of local) byId.set(p.reading_id, p);

  let overlapping = 0;
  for (const p of server) {
    if (byId.has(p.reading_id)) overlapping += 1;
    byId.set(p.reading_id, p);
  }

  const positions = [...byId.values()].sort((a, b) =>
    a.recorded_at < b.recorded_at ? -1 : a.recorded_at > b.recorded_at ? 1 : 0,
  );

  return {
    positions,
    localOnly: local.length - overlapping,
    source: sourceOf(local.length, server.length),
  };
}

function sourceOf(localCount: number, serverCount: number): PositionSource {
  if (localCount > 0 && serverCount > 0) return 'both';
  if (serverCount > 0) return 'server';
  return 'local';
}

/** Network state as the screen reports it. */
export type NetworkStatus = 'online' | 'offline';

/**
 * One short line describing where the drawn data came from.
 *
 * Deliberately short and non-technical. The screen previously printed raw
 * exception text — `java.net.ConnectException: Failed to connect to
 * /192.168.1.68:8080` — which tells a driver nothing they can act on and hides
 * the fact that the map is showing real data anyway.
 */
export function describeSource(
  network: NetworkStatus,
  result: { source: PositionSource; localOnly: number },
  serverFailed: boolean,
): string {
  // Kept short enough to sit on one line beside its label. The first version
  // read "offline — showing what this device recorded", which wrapped and
  // shouldered the label out of alignment. A status line that makes the layout
  // worse is not doing its job.
  if (network === 'offline') return 'offline · local only';
  if (serverFailed) return 'no server · local only';
  if (result.source === 'local') return 'local only';
  if (result.localOnly > 0) return `synced · ${result.localOnly} pending`;
  return 'synced';
}
