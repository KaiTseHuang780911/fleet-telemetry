import {
  FALLBACK_CAMERA,
  boundsOf,
  cameraFor,
  thin,
  zoomForSpan,
  type Point,
} from './geometry';

const DEPOT: Point = { lat: 49.0591, lon: -122.7809 };

/** Moves a point north by `metres`. */
function north(from: Point, metres: number): Point {
  return { lat: from.lat + metres / 111_320, lon: from.lon };
}

describe('boundsOf', () => {
  it('spans every point', () => {
    const bounds = boundsOf([
      { lat: 1, lon: 10 },
      { lat: 3, lon: 8 },
      { lat: 2, lon: 12 },
    ]);

    expect(bounds).toEqual({ minLat: 1, maxLat: 3, minLon: 8, maxLon: 12 });
  });

  it('has no bounds for no points', () => {
    expect(boundsOf([])).toBeNull();
  });

  // A NaN poisons every comparison silently, and the resulting camera points
  // nowhere with no error to explain it.
  it('ignores points that are not real coordinates', () => {
    const bounds = boundsOf([
      { lat: 1, lon: 10 },
      { lat: Number.NaN, lon: 10 },
      { lat: 3, lon: 12 },
    ]);

    expect(bounds).toEqual({ minLat: 1, maxLat: 3, minLon: 10, maxLon: 12 });
  });

  it('has no bounds when every point is unusable', () => {
    expect(boundsOf([{ lat: Number.NaN, lon: Number.NaN }])).toBeNull();
  });
});

describe('zoomForSpan', () => {
  it('zooms in for a small span and out for a large one', () => {
    expect(zoomForSpan(0.01, 0.01)).toBeGreaterThan(zoomForSpan(1, 1));
  });

  // Dividing by a zero span is the obvious way to zoom to infinity.
  it('returns a street-level zoom for a single point', () => {
    expect(zoomForSpan(0, 0)).toBe(16);
  });

  it('stays within a range the map can actually render', () => {
    for (const span of [0, 1e-9, 0.001, 1, 90, 360, 1000]) {
      const zoom = zoomForSpan(span, span);
      expect(zoom).toBeGreaterThanOrEqual(2);
      expect(zoom).toBeLessThanOrEqual(18);
      expect(Number.isFinite(zoom)).toBe(true);
    }
  });

  // The wider of the two dimensions is what has to fit; fitting the narrower
  // one clips the route along the other axis.
  it('fits the larger of the two spans', () => {
    expect(zoomForSpan(0.001, 1)).toBe(zoomForSpan(1, 1));
  });
});

describe('cameraFor', () => {
  it('falls back to a recognisable place when there is nothing to show', () => {
    expect(cameraFor([])).toEqual(FALLBACK_CAMERA);
  });

  // The centre is the middle of the bounding box, not the mean of the points.
  // A vehicle parked for twenty minutes contributes far more points than the
  // road it drove to get there, so a mean drags the camera onto the car park
  // and cuts off the route.
  it('centres on the bounding box rather than the crowd of points', () => {
    const parked = Array.from({ length: 100 }, () => ({ lat: 49.0, lon: -123.0 }));
    const oneFarPoint: Point = { lat: 49.2, lon: -123.0 };

    const camera = cameraFor([...parked, oneFarPoint]);

    expect(camera.latitude).toBeCloseTo(49.1, 5);
  });

  it('centres a single point on itself', () => {
    const camera = cameraFor([DEPOT]);

    expect(camera.latitude).toBeCloseTo(DEPOT.lat, 6);
    expect(camera.longitude).toBeCloseTo(DEPOT.lon, 6);
  });

  it('zooms out further for a longer route', () => {
    const short = cameraFor([DEPOT, north(DEPOT, 200)]);
    const long = cameraFor([DEPOT, north(DEPOT, 20_000)]);

    expect(long.zoom).toBeLessThan(short.zoom);
  });

  it('never produces a camera with a non-finite value', () => {
    const camera = cameraFor([{ lat: Number.NaN, lon: 5 }]);

    expect(Number.isFinite(camera.latitude)).toBe(true);
    expect(Number.isFinite(camera.longitude)).toBe(true);
    expect(Number.isFinite(camera.zoom)).toBe(true);
  });
});

describe('thin', () => {
  it('collapses a cluster of near-identical fixes', () => {
    const parked = Array.from({ length: 200 }, (_, i) => north(DEPOT, i * 0.05));

    expect(thin(parked, 15).length).toBeLessThan(10);
  });

  it('keeps points that are genuinely apart', () => {
    const spread = [DEPOT, north(DEPOT, 100), north(DEPOT, 200), north(DEPOT, 300)];

    expect(thin(spread, 15)).toHaveLength(4);
  });

  // The last point is where the vehicle is now, which is the one point a reader
  // looks for. Thinning must never be what removes it.
  it('always keeps the final point', () => {
    const points = [DEPOT, ...Array.from({ length: 50 }, () => north(DEPOT, 1000))];
    const thinned = thin(points, 15);

    expect(thinned[thinned.length - 1]).toEqual(points[points.length - 1]);
  });

  it('leaves a short route alone', () => {
    expect(thin([DEPOT, DEPOT], 15)).toHaveLength(2);
    expect(thin([DEPOT], 15)).toHaveLength(1);
    expect(thin([], 15)).toHaveLength(0);
  });

  it('drops points that are not real coordinates', () => {
    const points = [DEPOT, { lat: Number.NaN, lon: 0 }, north(DEPOT, 500)];

    expect(thin(points, 15).every((p) => Number.isFinite(p.lat))).toBe(true);
  });

  // Thinning is a rendering optimisation, so it must not reorder the route.
  it('preserves order', () => {
    const points = Array.from({ length: 20 }, (_, i) => north(DEPOT, i * 100));
    const thinned = thin(points, 15);

    for (let i = 1; i < thinned.length; i++) {
      expect(thinned[i]!.lat).toBeGreaterThan(thinned[i - 1]!.lat);
    }
  });
});
