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

import NetInfo from '@react-native-community/netinfo';
import { GoogleMaps } from 'expo-maps';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
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
  listActiveDays,
  listPositions,
  listStops,
  listVehicles,
  type ActiveDay,
  type Position,
  type StopEvent,
} from '../api/read';
import { cameraFor, thin } from '../route/geometry';
import { describeSource, mergePositions, type NetworkStatus } from '../route/merge';
import { SqliteOutbox } from '../queue/sqlite';

/**
 * What the screen is showing: the last N hours, or one specific day.
 *
 * Rolling windows answer "what is happening now" and a day answers "what
 * happened on the 18th". Both are wanted, and neither subsumes the other — a
 * 7-day window could not reach data from two weeks ago however far it stretched.
 */
type Selection = { kind: 'recent'; hours: number } | { kind: 'day'; day: ActiveDay };

const RECENT = [
  { label: '1h', hours: 1 },
  { label: '24h', hours: 24 },
] as const;

/** The window a selection covers. */
function rangeOf(selection: Selection): { from: Date; to: Date } {
  if (selection.kind === 'recent') {
    const to = new Date();
    return { from: new Date(to.getTime() - selection.hours * 3_600_000), to };
  }
  // Bounded by the day's own first and last fix rather than midnight to
  // midnight. The server groups in UTC and the reader is not in UTC, so a
  // local-midnight window would clip one end of the day it claims to show.
  const from = new Date(selection.day.first);
  const to = new Date(new Date(selection.day.last).getTime() + 1000);
  return { from, to };
}

/** "Sep 18" — short, because these sit in a horizontal row. */
function dayLabel(day: ActiveDay): string {
  return new Date(day.first).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

interface LoadedRoute {
  positions: Position[];
  stops: StopEvent[];
  truncated: boolean;
  vehicleLabel: string;
  /** One short line saying where the drawn data came from. */
  sourceLine: string;
}

export interface RouteScreenProps {
  /** Sends detail to the Outbox screen's log, where exceptions belong. */
  onLog?: (line: string) => void;
}

export function RouteScreen({ onLog }: RouteScreenProps) {
  const [selection, setSelection] = useState<Selection>({ kind: 'recent', hours: 24 });
  const [days, setDays] = useState<ActiveDay[]>([]);
  const [route, setRoute] = useState<LoadedRoute | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [network, setNetwork] = useState<NetworkStatus>('online');
  const [loading, setLoading] = useState(true);

  // Held so the camera can be moved without remounting the map. See the effect
  // below for why that matters.
  const mapRef = useRef<GoogleMaps.MapView>(null);

  // Real connectivity, so an offline device stops asking a server it cannot
  // reach. Retrying into a dead network costs radio and returns the same
  // exception every time.
  useEffect(
    () =>
      NetInfo.addEventListener((state) =>
        setNetwork(state.isConnected ? 'online' : 'offline'),
      ),
    [],
  );

  const load = useCallback(
    async (signal?: AbortSignal) => {
      setError(null);
      const { from, to } = rangeOf(selection);

      // Local history first, always. The device just recorded these; asking a
      // server for them is a round trip to be told what we already know, and
      // out of coverage it is a round trip that cannot succeed. The map draws
      // from this whether or not anything else works.
      let local: Position[] = [];
      try {
        const store = await SqliteOutbox.open(undefined, { isolated: true });
        try {
          const rows = await store.localSince(from.toISOString());
          local = rows
            .filter((r) => new Date(r.recordedAt) <= to)
            .map((r) => ({
              reading_id: r.readingId,
              vehicle_id: '',
              recorded_at: r.recordedAt,
              received_at: r.recordedAt,
              lat: r.lat,
              lon: r.lon,
              speed_mps: r.speedMps,
              accuracy_m: r.accuracyM,
              battery_pct: r.batteryPct,
              motion_state: r.motionState,
            }));
        } finally {
          await store.close();
        }
      } catch (err) {
        onLog?.(`route: local history unreadable — ${describe(err)}`);
      }

      let server: Position[] = [];
      let stops: StopEvent[] = [];
      let truncated = false;
      let vehicleLabel = 'this device';
      let serverFailed = false;

      // Skipped entirely while offline. There is no point discovering the same
      // ConnectException once per window change.
      if (network === 'online') {
        try {
          const vehicles = await listVehicles({ signal });
          const vehicle = vehicles[0];
          if (vehicle) {
            vehicleLabel = vehicle.label ?? vehicle.external_id;
            const [page, serverStops, activeDays] = await Promise.all([
              listPositions(vehicle.id, { from, to }, { signal }),
              // Client-detected stops only. Both sources would put two markers
              // on every stop — the footgun ADR-002 predicted.
              listStops(vehicle.id, { from, to, source: 'client' }, { signal }),
              listActiveDays(vehicle.id, 30, { signal }),
            ]);
            server = page.positions;
            stops = serverStops;
            truncated = page.truncated;
            setDays(activeDays);
          }
        } catch (err) {
          if (err instanceof Error && err.name === 'AbortError') return;
          serverFailed = true;
          // The exception goes to the log, not at the driver. "fetch failed:
          // java.net.ConnectException" is unactionable on a screen and hides
          // the fact that the map is showing real data anyway.
          onLog?.(`route: server read failed — ${describe(err)}`);
        }
      }

      const merged = mergePositions(local, server);

      if (merged.positions.length === 0 && local.length === 0 && serverFailed) {
        setError('No route data on this device for this window.');
      }

      setRoute({
        positions: merged.positions,
        stops,
        truncated,
        vehicleLabel,
        sourceLine: describeSource(network, merged, serverFailed),
      });
      setLoading(false);
    },
    [selection, network, onLog],
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

  // Move the camera when the framing changes, rather than rebuilding the view.
  // Animated rather than snapped, so switching between days reads as the map
  // travelling there instead of flickering to a new place.
  useEffect(() => {
    mapRef.current?.setCameraPosition({
      coordinates: { latitude: camera.latitude, longitude: camera.longitude },
      zoom: camera.zoom,
      duration: 500,
    });
  }, [camera.latitude, camera.longitude, camera.zoom]);

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
        {/* Deliberately NOT keyed on the camera.
            `cameraPosition` is an initial value rather than a controlled prop,
            so an earlier version keyed the element on the camera to force a
            re-frame. That worked by remounting the entire native map on every
            date change, and remounting a Google map view repeatedly leaves it
            black. The camera is moved through the ref instead. */}
        <GoogleMaps.View
          ref={mapRef}
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

      <ScrollView
        horizontal
        showsHorizontalScrollIndicator={false}
        contentContainerStyle={styles.windows}
      >
        {RECENT.map((w) => {
          const on = selection.kind === 'recent' && selection.hours === w.hours;
          return (
            <Pressable
              key={w.label}
              onPress={() => setSelection({ kind: 'recent', hours: w.hours })}
              style={[styles.window, on && styles.windowOn]}
            >
              <Text style={[styles.windowText, on && styles.windowTextOn]}>{w.label}</Text>
            </Pressable>
          );
        })}

        {/* Only days the vehicle actually reported on. A picker offering an
            empty Tuesday is a picker that mostly produces disappointment. */}
        {days.map((d) => {
          const on = selection.kind === 'day' && selection.day.day === d.day;
          return (
            <Pressable
              key={d.day}
              onPress={() => setSelection({ kind: 'day', day: d })}
              style={[styles.window, on && styles.windowOn]}
            >
              <Text style={[styles.windowText, on && styles.windowTextOn]}>{dayLabel(d)}</Text>
            </Pressable>
          );
        })}
      </ScrollView>

      <ScrollView
        style={styles.panel}
        refreshControl={
          <RefreshControl refreshing={loading} onRefresh={() => void load()} tintColor="#8b949e" />
        }
      >
        {/* Two lines, always present, instead of an exception dump. One says
            whether the device can reach anything; the other says where the
            drawn data came from. Detail goes to the Outbox log. */}
        <View style={styles.line}>
          <Text style={[styles.dim, styles.lineLabel]}>network</Text>
          <Text
            style={[network === 'online' ? styles.good : styles.warnInline, styles.lineValue]}
          >
            {network === 'online' ? 'online' : 'offline'}
          </Text>
        </View>

        <View style={styles.line}>
          <Text style={[styles.dim, styles.lineLabel]}>data</Text>
          <Text style={[styles.mono, styles.lineValue]}>{route?.sourceLine ?? '—'}</Text>
        </View>

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

function describe(err: unknown): string {
  return err instanceof ApiError || err instanceof Error ? err.message : String(err);
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
  windows: { flexDirection: 'row', gap: 8, paddingHorizontal: 12, paddingVertical: 12 },
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
    // The label keeps its width and the value takes the rest, so a long value
    // wraps within its own column instead of squeezing the label out of
    // alignment. Belt to the braces of keeping the status strings short.
    gap: 12,
  },
  lineLabel: { flexShrink: 0 },
  lineValue: { flex: 1, textAlign: 'right' },
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
  warnInline: { color: '#d29922', fontSize: 13, ...bold },
  good: { color: '#3fb950', fontSize: 13, ...bold },
  error: { color: '#f85149', fontSize: 13, lineHeight: 18, marginVertical: 8 },
});
