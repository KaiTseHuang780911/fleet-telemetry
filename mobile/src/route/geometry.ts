/**
 * Pure geometry for the route map.
 *
 * The map component itself is native and untestable from here; this is the part
 * that decides *what* it shows, and it is the part that can be wrong in ways
 * nobody notices — a camera framed on the mean of two clusters puts the marker
 * in a field halfway between them, which looks like a deliberate choice rather
 * than a bug.
 */

/** The minimal shape the map needs from a position. */
export interface Point {
  lat: number;
  lon: number;
}

export interface Bounds {
  minLat: number;
  maxLat: number;
  minLon: number;
  maxLon: number;
}

export interface Camera {
  latitude: number;
  longitude: number;
  zoom: number;
}

/** Where to point the camera when there is nothing to show. */
export const FALLBACK_CAMERA: Camera = {
  // Vancouver, matching the simulator's origin, so an empty map is recognisable
  // as "no data" rather than as the middle of the Atlantic that (0, 0) gives.
  latitude: 49.2827,
  longitude: -123.1207,
  zoom: 11,
};

export function boundsOf(points: Point[]): Bounds | null {
  if (points.length === 0) return null;

  let minLat = Infinity;
  let maxLat = -Infinity;
  let minLon = Infinity;
  let maxLon = -Infinity;

  for (const p of points) {
    // A NaN would poison every comparison silently and produce a camera that
    // points nowhere, so bad points are skipped rather than trusted.
    if (!Number.isFinite(p.lat) || !Number.isFinite(p.lon)) continue;
    if (p.lat < minLat) minLat = p.lat;
    if (p.lat > maxLat) maxLat = p.lat;
    if (p.lon < minLon) minLon = p.lon;
    if (p.lon > maxLon) maxLon = p.lon;
  }

  if (minLat === Infinity) return null;
  return { minLat, maxLat, minLon, maxLon };
}

/**
 * A zoom level that fits the given span.
 *
 * Web-Mercator zoom doubles the scale each level, so the level that fits a span
 * is logarithmic in it. 360 degrees of longitude fill the tile width at zoom 0,
 * which gives `log2(360 / span)`.
 *
 * Latitude is compared against a 180-degree range *scaled by the viewport's
 * aspect*, approximated here as square. That is deliberately crude: a map that
 * is slightly too zoomed out is fine, one that clips the route is not, so the
 * result is rounded down and a margin is applied by the caller.
 */
export function zoomForSpan(latSpan: number, lonSpan: number): number {
  const MAX_ZOOM = 18;
  const MIN_ZOOM = 2;

  // A single point, or several at the same place, has no span to fit. Street
  // level is the useful answer; the alternative is dividing by zero and
  // zooming to infinity.
  const span = Math.max(latSpan, lonSpan);
  if (span <= 0) return 16;

  const zoom = Math.log2(360 / span);
  return Math.max(MIN_ZOOM, Math.min(MAX_ZOOM, Math.floor(zoom)));
}

/**
 * Frames the camera on a set of points.
 *
 * The centre is the middle of the *bounding box*, not the mean of the points.
 * Those differ whenever sampling is uneven, which it always is — a vehicle
 * parked for twenty minutes contributes far more points than the road it drove
 * to get there, so a mean would drag the camera onto the car park and cut off
 * the route.
 *
 * `margin` scales the fitted span so the route does not touch the edges;
 * 1.2 leaves roughly ten percent on each side.
 */
export function cameraFor(points: Point[], margin = 1.2): Camera {
  const bounds = boundsOf(points);
  if (!bounds) return FALLBACK_CAMERA;

  const latSpan = (bounds.maxLat - bounds.minLat) * margin;
  const lonSpan = (bounds.maxLon - bounds.minLon) * margin;

  return {
    latitude: (bounds.minLat + bounds.maxLat) / 2,
    longitude: (bounds.minLon + bounds.maxLon) / 2,
    zoom: zoomForSpan(latSpan, lonSpan),
  };
}

/**
 * Drops points closer together than `minMetres`, keeping the first and last.
 *
 * A route is mostly stops, and a stop is hundreds of near-identical fixes. The
 * map draws them as a single dot regardless, so sending them to the native
 * layer costs a bridge crossing per point and buys nothing. A 900-point day
 * typically thins to a couple of hundred.
 *
 * Distance uses an equirectangular approximation rather than haversine: at the
 * scale that matters here — metres, not continents — the error is far below the
 * GPS accuracy of the points themselves, and this runs over every point on
 * every render.
 */
export function thin<T extends Point>(points: T[], minMetres = 15): T[] {
  if (points.length <= 2) return [...points];

  const METRES_PER_DEGREE = 111_320;
  const kept: T[] = [];
  let last: T | null = null;

  for (const p of points) {
    if (!Number.isFinite(p.lat) || !Number.isFinite(p.lon)) continue;
    if (last === null) {
      kept.push(p);
      last = p;
      continue;
    }
    const dLat = (p.lat - last.lat) * METRES_PER_DEGREE;
    const dLon = (p.lon - last.lon) * METRES_PER_DEGREE * Math.cos((p.lat * Math.PI) / 180);
    if (Math.hypot(dLat, dLon) >= minMetres) {
      kept.push(p);
      last = p;
    }
  }

  // The final point is where the vehicle actually is, which is the one point a
  // reader looks for. Thinning must never be what removes it.
  const finalPoint = points[points.length - 1];
  if (finalPoint && kept[kept.length - 1] !== finalPoint) kept.push(finalPoint);

  return kept;
}
