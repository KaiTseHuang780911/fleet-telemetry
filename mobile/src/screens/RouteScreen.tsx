/**
 * The route map.
 *
 * Everything this project does well is currently invisible — the queue, the
 * reconciliation, the idempotency all live in tables. This is the one screen
 * that makes the work legible: where the vehicle went, and where it stopped.
 *
 * It reads from the server rather than from the local outbox on purpose. The
 * outbox holds only what has *not* been delivered yet, so drawing from it would
 * show an emptying route — the better the sync works, the less you would see.
 */

import { GoogleMaps } from 'expo-maps';
import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  ActivityIndicator,
  Platform,
  Pressable,
  RefreshControl,
  ScrollView,
  StyleSheet,
  Text,
  View,
} from 'react-native';

import {
  ApiError,
  listPositions,
  listStops,
  listVehicles,
  type Position,
  type StopEvent,
} from '../api/read';
import { cameraFor, thin } from '../route/geometry';

/** Windows the screen offers. Hours, because a shift is the unit that matters. */
const WINDOWS = [
  { label: '1h', hours: 1 },
  { label: '6h', hours: 6 },
  { label: '24h', hours: 24 },
  { label: '7d', hours: 24 * 7 },
] as const;

interface LoadedRoute {
  positions: Position[];
  stops: StopEvent[];
  truncated: boolean;
  vehicleLabel: string;
}

export function RouteScreen() {
  const [hours, setHours] = useState<number>(24);
  const [route, setRoute] = useState<LoadedRoute | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(
    async (signal?: AbortSignal) => {
      setError(null);
      try {
        const vehicles = await listVehicles({ signal });
        const vehicle = vehicles[0];
        if (!vehicle) {
          setRoute(null);
          setError('No vehicle has reported yet. Start tracking and drain the queue.');
          return;
        }

        const from = new Date(Date.now() - hours * 3_600_000);
        // Both reads in parallel: they are independent, and the screen cannot
        // render usefully until it has both anyway.
        const [page, stops] = await Promise.all([
          listPositions(vehicle.id, { from }, { signal }),
          // Client-detected stops only. Asking for both sources returns each
          // stop twice — once as detected on the device and once as derived by
          // the server — which would put two markers on every stop.
          listStops(vehicle.id, { from, source: 'client' }, { signal }),
        ]);

        setRoute({
          positions: page.positions,
          stops,
          truncated: page.truncated,
          vehicleLabel: vehicle.label ?? vehicle.external_id,
        });
      } catch (err) {
        if (err instanceof Error && err.name === 'AbortError') return;
        setError(err instanceof ApiError ? err.message : String(err));
        setRoute(null);
      } finally {
        setLoading(false);
      }
    },
    [hours],
  );

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    void load(controller.signal);
    // Aborting on unmount stops a slow reply from setting state on a screen
    // that is gone, and cancels the request rather than leaving it in flight.
    return () => controller.abort();
  }, [load]);

  // Thinned once per load rather than per render: a day of tracking is ~900
  // points, most of them stacked on top of each other while parked, and every
  // one costs a bridge crossing to the native map.
  const path = useMemo(() => thin(toPoints(route?.positions ?? [])), [route?.positions]);
  const camera = useMemo(
    () => cameraFor(path.length > 0 ? path : toPoints(route?.stops ?? [])),
    [path, route?.stops],
  );

  const markers = useMemo(
    () =>
      (route?.stops ?? []).map((stop) => ({
        id: stop.id,
        coordinates: { latitude: stop.lat, longitude: stop.lon },
        title: describeDwell(stop),
        snippet: new Date(stop.arrived_at).toLocaleString(),
        showCallout: true,
      })),
    [route?.stops],
  );

  const polylines = useMemo(
    () =>
      path.length >= 2
        ? [
            {
              id: 'route',
              coordinates: path.map((p) => ({ latitude: p.lat, longitude: p.lon })),
              color: '#58a6ff',
              width: 5,
            },
          ]
        : [],
    [path],
  );

  return (
    <View style={styles.screen}>
      <View style={styles.mapWrap}>
        {/* The map is keyed on the camera so a window change re-frames it.
            expo-maps treats cameraPosition as an initial value, not a
            controlled prop, so without this the view stays where it was. */}
        <GoogleMaps.View
          key={`${camera.latitude.toFixed(4)},${camera.longitude.toFixed(4)},${camera.zoom}`}
          style={StyleSheet.absoluteFill}
          cameraPosition={{
            coordinates: { latitude: camera.latitude, longitude: camera.longitude },
            zoom: camera.zoom,
          }}
          markers={markers}
          polylines={polylines}
          uiSettings={{ myLocationButtonEnabled: false }}
        />
        {loading ? (
          <View style={styles.overlay}>
            <ActivityIndicator color="#58a6ff" />
          </View>
        ) : null}
      </View>

      <View style={styles.windows}>
        {WINDOWS.map((w) => (
          <Pressable
            key={w.label}
            onPress={() => setHours(w.hours)}
            style={[styles.window, hours === w.hours && styles.windowOn]}
          >
            <Text style={[styles.windowText, hours === w.hours && styles.windowTextOn]}>
              {w.label}
            </Text>
          </Pressable>
        ))}
      </View>

      <ScrollView
        style={styles.panel}
        refreshControl={
          <RefreshControl refreshing={loading} onRefresh={() => void load()} tintColor="#8b949e" />
        }
      >
        {error ? <Text style={styles.error}>{error}</Text> : null}

        {route ? (
          <>
            <View style={styles.line}>
              <Text style={styles.dim}>vehicle</Text>
              <Text style={styles.mono}>{route.vehicleLabel}</Text>
            </View>
            <View style={styles.line}>
              <Text style={styles.dim}>positions</Text>
              <Text style={styles.mono}>
                {route.positions.length}
                {path.length !== route.positions.length ? ` (${path.length} drawn)` : ''}
              </Text>
            </View>
            <View style={styles.line}>
              <Text style={styles.dim}>stops</Text>
              <Text style={styles.mono}>{route.stops.length}</Text>
            </View>

            {/* A truncated route looks exactly like a short one once drawn, so
                the cap has to be said out loud rather than shown. */}
            {route.truncated ? (
              <Text style={styles.warn}>
                Showing the first {route.positions.length} positions — the window holds more.
                Narrow it to see the rest.
              </Text>
            ) : null}

            {route.stops.length > 0 ? (
              <>
                <Text style={styles.heading}>stops</Text>
                {route.stops.map((stop) => (
                  <View key={stop.id} style={styles.stopRow}>
                    <Text style={styles.mono}>{describeDwell(stop)}</Text>
                    <Text style={styles.dim}>
                      {new Date(stop.arrived_at).toLocaleTimeString()}
                    </Text>
                  </View>
                ))}
              </>
            ) : null}
          </>
        ) : null}

        {!loading && !error && route?.positions.length === 0 ? (
          <Text style={styles.dim}>
            Nothing recorded in this window. Try a longer one, or start tracking.
          </Text>
        ) : null}
      </ScrollView>
    </View>
  );
}

function toPoints(items: { lat: number; lon: number }[]) {
  return items.map((i) => ({ lat: i.lat, lon: i.lon }));
}

/**
 * How long a stop lasted, or that it is still open.
 *
 * An open stop is normal rather than missing data — the vehicle is still there
 * — so it says so instead of showing a blank or a zero.
 */
function describeDwell(stop: StopEvent): string {
  if (!stop.departed_at) return 'still here';

  const ms = new Date(stop.departed_at).getTime() - new Date(stop.arrived_at).getTime();
  if (!Number.isFinite(ms) || ms < 0) return 'unknown';

  const minutes = Math.round(ms / 60_000);
  if (minutes < 1) return '<1 min';
  if (minutes < 60) return `${minutes} min`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}

const mono = Platform.select({ android: 'monospace', default: 'Menlo' });
const bold = Platform.select({
  android: { fontFamily: 'sans-serif-medium', fontWeight: 'normal' as const },
  default: { fontWeight: '600' as const },
});

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: '#0d1117' },
  mapWrap: { flex: 1, minHeight: 240 },
  overlay: {
    position: 'absolute',
    top: 0,
    left: 0,
    right: 0,
    bottom: 0,
    alignItems: 'center',
    justifyContent: 'center',
    backgroundColor: '#0d111788',
  },
  windows: { flexDirection: 'row', gap: 8, padding: 12 },
  window: {
    paddingVertical: 6,
    paddingHorizontal: 14,
    borderRadius: 8,
    backgroundColor: '#161b22',
  },
  windowOn: { backgroundColor: '#238636' },
  windowText: { color: '#8b949e', fontSize: 13, ...bold },
  windowTextOn: { color: '#ffffff' },
  panel: { maxHeight: 260, paddingHorizontal: 16 },
  line: {
    flexDirection: 'row',
    justifyContent: 'space-between',
    alignItems: 'center',
    paddingVertical: 4,
  },
  stopRow: {
    flexDirection: 'row',
    justifyContent: 'space-between',
    paddingVertical: 6,
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: '#21262d',
  },
  heading: { color: '#e6edf3', fontSize: 15, marginTop: 14, marginBottom: 4, ...bold },
  dim: { color: '#8b949e', fontSize: 13 },
  mono: { color: '#e6edf3', fontSize: 13, fontFamily: mono },
  warn: { color: '#d29922', fontSize: 13, lineHeight: 18, marginTop: 8 },
  error: { color: '#f85149', fontSize: 13, lineHeight: 18, marginVertical: 8 },
});
