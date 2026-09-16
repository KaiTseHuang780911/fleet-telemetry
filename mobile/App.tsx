/**
 * Slice 1 debug screen.
 *
 * The offline queue was built before any real UI, so this exists to make it
 * observable rather than inferred from logs: queue depth, quarantine count, the
 * last drain's outcome, and controls to force the situations that matter —
 * flooding the queue, simulating a dead network, draining on demand.
 *
 * It is scaffolding. The route and stop screens replace it in the next slice.
 */

import 'react-native-get-random-values';

import NetInfo from '@react-native-community/netinfo';
import { AppState } from 'react-native';
import { StatusBar } from 'expo-status-bar';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  ActivityIndicator,
  Platform,
  Pressable,
  ScrollView,
  StyleSheet,
  Switch,
  Text,
  View,
} from 'react-native';
import { v7 as uuidv7 } from 'uuid';

import { HttpTransport } from './src/api/transport';
import {
  describeGrant,
  getPermissionState,
  openAppSettings,
  requestLocationPermissions,
  type PermissionState,
} from './src/location/permissions';
import { MockRoute } from './src/location/mock';
import {
  getCurrentFix,
  isTracking,
  servicesEnabled,
  startTracking,
  stopTracking,
} from './src/location/service';
import {
  isHealthy,
  needsUserAction,
  trackingLabel,
  trackingState,
} from './src/location/status';
import { FIX_COUNT_SETTING, LAST_FIX_SETTING } from './src/location/task';
import { API_BASE_URL, API_URL_IS_FALLBACK, DEVICE_ID_SETTING } from './src/config';
import {
  PERIODIC_DRAIN_MS,
  shouldAutoDrain,
  type DrainTrigger,
} from './src/queue/autodrain';
import { SqliteOutbox } from './src/queue/sqlite';
import { SyncEngine, type SyncEvent } from './src/queue/sync';
import {
  DEFAULT_SYNC_CONFIG,
  type DrainResult,
  type OutboxItem,
  type SendOutcome,
  type Transport,
} from './src/queue/types';
import { makePositionItem } from './src/telemetry/readings';

/** Wraps the real transport so the UI can simulate a dead network. */
class ToggleableTransport implements Transport {
  online = true;
  constructor(private readonly inner: Transport) {}

  async send(deviceId: string, items: OutboxItem[]): Promise<SendOutcome> {
    if (!this.online) {
      return { kind: 'unavailable', reason: 'simulated offline' };
    }
    return this.inner.send(deviceId, items);
  }
}

interface Status {
  depth: number;
  dead: number;
  draining: boolean;
  nextAttemptAt: number | null;
  consecutiveFailures: number;
}

export default function App() {
  const [ready, setReady] = useState(false);
  const [fatal, setFatal] = useState<string | null>(null);
  const [deviceId, setDeviceId] = useState('');
  const [status, setStatus] = useState<Status | null>(null);
  const [lastDrain, setLastDrain] = useState<DrainResult | null>(null);
  const [log, setLog] = useState<string[]>([]);
  const [simulateOffline, setSimulateOffline] = useState(false);
  const [networkOnline, setNetworkOnline] = useState(true);
  const [busy, setBusy] = useState(false);
  const [pollError, setPollError] = useState<string | null>(null);
  const [permission, setPermission] = useState<PermissionState | null>(null);
  const [tracking, setTracking] = useState(false);
  // The device's master location switch, polled alongside everything else.
  // Optimistic until the first poll answers, so the screen does not flash a
  // "location off" warning during startup.
  const [locationOn, setLocationOn] = useState(true);
  const [lastFix, setLastFix] = useState<string | null>(null);
  const [fixCount, setFixCount] = useState(0);
  const [mockRunning, setMockRunning] = useState(false);

  const storeRef = useRef<SqliteOutbox | null>(null);
  const engineRef = useRef<SyncEngine | null>(null);
  const transportRef = useRef<ToggleableTransport | null>(null);
  const mockRef = useRef<MockRoute | null>(null);
  // Read by the drain triggers. A ref rather than the state value because an
  // effect that closes over state sees whatever it was when the effect ran.
  const onlineRef = useRef(true);

  const note = useCallback((line: string) => {
    const stamp = new Date().toLocaleTimeString();
    setLog((prev) => [`${stamp}  ${line}`, ...prev].slice(0, 40));
  }, []);

  // One-time setup: open the database, mint or read the device id, build the engine.
  useEffect(() => {
    let cancelled = false;

    (async () => {
      try {
        const store = await SqliteOutbox.open();

        // A stable per-install id, generated once and stored, so the server
        // keeps mapping this device to the same vehicle across restarts.
        let id = await store.getSetting(DEVICE_ID_SETTING);
        if (!id) {
          id = `device-${uuidv7().slice(0, 8)}`;
          await store.setSetting(DEVICE_ID_SETTING, id);
        }

        const transport = new ToggleableTransport(new HttpTransport({ baseUrl: API_BASE_URL }));
        const engine = new SyncEngine(store, transport, id, DEFAULT_SYNC_CONFIG, {
          onEvent: (event: SyncEvent) => {
            if (event.type === 'dropped') {
              note(`DROPPED ${event.count} item(s) at the queue cap — data loss`);
            } else if (event.type === 'quarantined') {
              note(`quarantined ${event.count}: ${event.reason}`);
            }
          },
        });

        if (cancelled) return;
        storeRef.current = store;
        engineRef.current = engine;
        transportRef.current = transport;
        setDeviceId(id);
        setReady(true);
        setPermission(await getPermissionState());
        note(`ready — api ${API_BASE_URL}`);
      } catch (err) {
        if (!cancelled) setFatal(err instanceof Error ? err.message : String(err));
      }
    })();

    return () => {
      cancelled = true;
      // Close the database. Without this, every Fast Refresh opened another
      // handle and abandoned the previous one; the released native database
      // then failed every subsequent call with a NullPointerException, once
      // per poll, forever. A leaked handle is wrong regardless of whether a
      // production build ever hot-reloads.
      const store = storeRef.current;
      storeRef.current = null;
      engineRef.current = null;
      transportRef.current = null;
      void store?.close();
    };
  }, [note]);

  // Returns false when the store is no longer usable, so the caller can stop
  // polling instead of producing one unhandled rejection per second.
  const refresh = useCallback(async (): Promise<boolean> => {
    const engine = engineRef.current;
    if (!engine) return false;
    try {
      setStatus(await engine.status());

      const store = storeRef.current;
      if (store) {
        setLastFix(await store.getSetting(LAST_FIX_SETTING));
        setFixCount(Number((await store.getSetting(FIX_COUNT_SETTING)) ?? '0'));
      }
      setTracking(await isTracking());
      setLocationOn(await servicesEnabled());
      return true;
    } catch (err) {
      setPollError(err instanceof Error ? err.message : String(err));
      return false;
    }
  }, []);

  useEffect(() => {
    if (!ready) return;
    let stopped = false;

    const tick = async () => {
      if (stopped) return;
      const ok = await refresh();
      if (!ok) {
        // One visible failure beats a thousand identical red boxes. Whatever
        // broke the store will not fix itself on the next tick.
        stopped = true;
        clearInterval(timer);
      }
    };

    void tick();
    const timer = setInterval(() => void tick(), 1000);
    return () => {
      stopped = true;
      clearInterval(timer);
    };
  }, [ready, refresh]);

  const record = useCallback(
    async (count: number) => {
      const engine = engineRef.current;
      if (!engine) return;
      setBusy(true);
      try {
        const items = Array.from({ length: count }, () =>
          makePositionItem({
            // Vancouver, jittered. This slice is about the queue, not GPS.
            lat: 49.2827 + (Math.random() - 0.5) * 0.02,
            lon: -123.1207 + (Math.random() - 0.5) * 0.02,
            speedMps: Math.random() * 15,
            batteryPct: 80,
            motionState: 'driving',
          }),
        );
        await engine.enqueue(items);
        note(`queued ${count}`);
        await refresh();
      } finally {
        setBusy(false);
      }
    },
    [note, refresh],
  );

  const runDrain = useCallback(
    async (trigger: DrainTrigger) => {
      const engine = engineRef.current;
      if (!engine) return;

      // Consult the schedule for everything except an explicit tap. This is the
      // wiring that was missing: the rules existed and were tested, but nothing
      // ever asked them.
      if (trigger !== 'manual') {
        const status = await engine.status();
        const allowed = shouldAutoDrain(trigger, {
          online: onlineRef.current,
          active: AppState.currentState === 'active',
          queueDepth: status.depth,
          draining: status.draining,
          nextAttemptAt: status.nextAttemptAt,
          now: Date.now(),
        });
        if (!allowed) return;
      }

      if (trigger === 'manual') setBusy(true);
      try {
        const result = await engine.drain();
        setLastDrain(result);
        note(
          result.sent === 0
            ? `drain (${trigger}): nothing queued`
            : `drain (${trigger}): sent ${result.sent}, accepted ${result.accepted}` +
                (result.rejected ? `, rejected ${result.rejected}` : '') +
                (result.error ? ` — ${result.error}` : ''),
        );
        await refresh();
      } catch (err) {
        note(`drain threw: ${err instanceof Error ? err.message : String(err)}`);
      } finally {
        if (trigger === 'manual') setBusy(false);
      }
    },
    [note, refresh],
  );

  const drain = useCallback(() => runDrain('manual'), [runDrain]);

  // Real connectivity, distinct from the simulated toggle above.
  //
  // This listener used to do nothing but colour a label. Coming back into
  // coverage is the single most likely moment for a queued upload to succeed,
  // and it was going unused — so a queue filled while offline waited for a GPS
  // fix or a button press that might never come.
  useEffect(() => {
    return NetInfo.addEventListener((state) => {
      const online = Boolean(state.isConnected);
      const wasOffline = !onlineRef.current;
      onlineRef.current = online;
      setNetworkOnline(online);

      if (online && wasOffline) {
        note('back online — draining');
        void runDrain('reconnect');
      }
    });
  }, [note, runDrain]);

  // Trigger 2: the app coming to the foreground. A user who opens the app
  // expecting to see the queue clear should see it clear.
  useEffect(() => {
    const sub = AppState.addEventListener('change', (next) => {
      if (next === 'active') void runDrain('foreground');
    });
    return () => sub.remove();
  }, [runDrain]);

  // Trigger 3: a periodic sweep while the app is open, as a backstop for a
  // missed connectivity event. Gated by shouldAutoDrain, so an empty queue or a
  // device that believes it is offline costs nothing.
  useEffect(() => {
    if (!ready) return;
    // One immediately: the app may be opening after a long offline stretch with
    // a full queue, and nothing else would trigger a send until a fix arrived.
    void runDrain('foreground');
    const timer = setInterval(() => void runDrain('periodic'), PERIODIC_DRAIN_MS);
    return () => clearInterval(timer);
  }, [ready, runDrain]);

  useEffect(() => {
    if (transportRef.current) transportRef.current.online = !simulateOffline;
  }, [simulateOffline]);

  // Returns false when the store is no longer usable, so the caller can stop
  // polling instead of producing one unhandled rejection per second.

  const grantPermissions = useCallback(async () => {
    setBusy(true);
    try {
      const state = await requestLocationPermissions();
      setPermission(state);
      note(`location permission: ${describeGrant(state)}`);
    } finally {
      setBusy(false);
    }
  }, [note]);

  const toggleTracking = useCallback(async () => {
    setBusy(true);
    try {
      if (await isTracking()) {
        await stopTracking();
        note('tracking stopped');
      } else {
        await startTracking();
        // Checked after starting, not before: Android may show its own
        // "turn on location?" dialog during the call, so asking first would
        // report a state the user has just changed.
        const on = await servicesEnabled();
        setLocationOn(on);
        note(
          on
            ? 'tracking started'
            : 'tracking started but device location is OFF — no fix will arrive',
        );
      }
      setTracking(await isTracking());
    } catch (err) {
      note(`tracking: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setBusy(false);
    }
  }, [note]);

  const toggleMock = useCallback(() => {
    const existing = mockRef.current;
    if (existing?.running) {
      existing.stop();
      setMockRunning(false);
      note('mock route stopped');
      return;
    }

    const route = new MockRoute({
      // 2s ticks at 10x means a fix every 20 simulated seconds, so a two-minute
      // stop - long enough for the server to derive one - takes 12 real
      // seconds rather than two real minutes.
      tickMs: 2000,
      timeScale: 10,
      onError: (message) => note(`mock: ${message}`),
    });
    mockRef.current = route;
    route.start();
    setMockRunning(true);
    note('mock route started — synthetic fixes, not GPS');
  }, [note]);

  // Stop the generator if the screen goes away, or it keeps writing to a store
  // the cleanup has already closed.
  useEffect(() => () => mockRef.current?.stop(), []);

  const oneShotFix = useCallback(async () => {
    setBusy(true);
    try {
      const fix = await getCurrentFix();
      note(
        `fix ${fix.coords.latitude.toFixed(5)}, ${fix.coords.longitude.toFixed(5)}` +
          ` (±${Math.round(fix.coords.accuracy ?? 0)}m)`,
      );
    } catch (err) {
      note(`fix failed: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setBusy(false);
    }
  }, [note]);

  const clear = useCallback(async () => {
    await storeRef.current?.clear();
    setLastDrain(null);
    note('cleared queue and quarantine');
    await refresh();
  }, [note, refresh]);

  // When the last fix was recorded, or null if there has never been one.
  //
  // Parsed defensively: this string comes from SQLite, written by a previous
  // version of the app as often as by this one, so a shape change must degrade
  // to "no fix yet" rather than crash the screen that exists to diagnose
  // problems.
  //
  // Above the early returns because it is a hook: React requires the same hooks
  // to run in the same order on every render, and a useMemo below `if (fatal)`
  // would be skipped on the render where it matters least and present on every
  // other one.
  const lastFixAt = useMemo(() => {
    if (!lastFix) return null;
    try {
      const parsed: unknown = JSON.parse(lastFix);
      const at = (parsed as { at?: unknown }).at;
      return typeof at === 'string' ? Date.parse(at) : null;
    } catch {
      return null;
    }
  }, [lastFix]);

  const trackState = trackingState({
    registered: tracking,
    servicesEnabled: locationOn,
    // Date.parse yields NaN for an unparseable string, which is a number and
    // would therefore read as "a fix happened".
    lastFixAt: lastFixAt === null || Number.isNaN(lastFixAt) ? null : lastFixAt,
  });

  if (fatal) {
    return (
      <View style={[styles.screen, styles.centre]}>
        <Text style={styles.error}>Startup failed</Text>
        <Text style={styles.mono}>{fatal}</Text>
      </View>
    );
  }

  if (!ready) {
    return (
      <View style={[styles.screen, styles.centre]}>
        <ActivityIndicator />
        <Text style={styles.dim}>opening database…</Text>
      </View>
    );
  }

  const backoffRemaining =
    status?.nextAttemptAt != null ? Math.max(0, status.nextAttemptAt - Date.now()) : 0;

  return (
    <View style={styles.screen}>
      <StatusBar style="light" />
      <ScrollView contentContainerStyle={styles.content}>
        <Text style={styles.title}>Outbox</Text>
        <Text style={styles.dim}>{deviceId}</Text>

        {API_URL_IS_FALLBACK ? (
          <View style={styles.warning}>
            <Text style={styles.warningText}>
              EXPO_PUBLIC_API_URL is not set, so a default is in use. On a physical device that
              will always fail — it needs this machine&apos;s LAN address, not localhost.
            </Text>
          </View>
        ) : null}
        <Text style={styles.mono}>{API_BASE_URL}</Text>

        <View style={styles.row}>
          <Stat label="queued" value={status?.depth ?? 0} />
          <Stat
            label="quarantined"
            value={status?.dead ?? 0}
            tone={status?.dead ? 'bad' : undefined}
          />
          <Stat label="failures" value={status?.consecutiveFailures ?? 0} />
        </View>

        <View style={styles.line}>
          <Text style={styles.dim}>network</Text>
          <Text style={networkOnline ? styles.good : styles.bad}>
            {networkOnline ? 'connected' : 'offline'}
          </Text>
        </View>

        <View style={styles.line}>
          <Text style={styles.dim}>simulate offline</Text>
          <Switch value={simulateOffline} onValueChange={setSimulateOffline} />
        </View>

        {pollError ? (
          <View style={styles.warning}>
            <Text style={styles.warningText}>
              Status polling stopped: {pollError}
            </Text>
          </View>
        ) : null}

        {backoffRemaining > 0 ? (
          <Text style={styles.dim}>backing off for {(backoffRemaining / 1000).toFixed(1)}s</Text>
        ) : null}

        <Text style={styles.cardTitle}>location</Text>

        <View style={styles.line}>
          <Text style={styles.dim}>permission</Text>
          <Text style={permission?.grant === 'background' ? styles.good : styles.bad}>
            {permission ? permission.grant : '…'}
          </Text>
        </View>

        {permission && permission.grant !== 'background' ? (
          <View style={styles.warning}>
            <Text style={styles.warningText}>
              {describeGrant(permission)}
              {permission.backgroundNeedsSettings
                ? '. Android will not prompt for this again — it has to be changed in Settings under Permissions > Location > Allow all the time.'
                : ''}
            </Text>
          </View>
        ) : null}

        <View style={styles.line}>
          <Text style={styles.dim}>tracking</Text>
          <Text
            style={
              isHealthy(trackState)
                ? styles.good
                : needsUserAction(trackState)
                  ? styles.bad
                  : styles.dim
            }
          >
            {trackingLabel(trackState)}
          </Text>
        </View>

        {needsUserAction(trackState) ? (
          <Text style={styles.warn}>
            Location is switched off for this device, so no fix can arrive however long
            tracking runs. Turn it on in Android Settings.
          </Text>
        ) : null}

        <View style={styles.line}>
          <Text style={styles.dim}>fixes recorded</Text>
          <Text style={styles.mono}>{fixCount}</Text>
        </View>

        {lastFix ? <Text style={styles.mono}>{lastFix}</Text> : null}

        <View style={styles.buttons}>
          <Button label="Grant" onPress={() => void grantPermissions()} disabled={busy} />
          {permission?.backgroundNeedsSettings ? (
            <Button label="Settings" onPress={() => void openAppSettings()} disabled={busy} />
          ) : null}
          <Button
            label={tracking ? 'Stop tracking' : 'Start tracking'}
            onPress={() => void toggleTracking()}
            disabled={busy || permission?.grant === 'none'}
            primary={!tracking}
          />
          <Button label="One fix" onPress={() => void oneShotFix()} disabled={busy} />
          <Button
            label={mockRunning ? 'Stop mock' : 'Mock route'}
            onPress={toggleMock}
            disabled={busy}
          />
        </View>

        {mockRunning ? (
          <View style={styles.warning}>
            <Text style={styles.warningText}>
              Synthetic fixes, not GPS. This exercises the queue, the drain and the server, but
              proves nothing about Android actually delivering location in the background.
            </Text>
          </View>
        ) : null}

        <Text style={styles.cardTitle}>queue</Text>

        <View style={styles.buttons}>
          <Button label="Record 1" onPress={() => void record(1)} disabled={busy} />
          <Button label="Record 250" onPress={() => void record(250)} disabled={busy} />
          <Button label="Drain" onPress={() => void drain()} disabled={busy} primary />
          <Button label="Clear" onPress={() => void clear()} disabled={busy} />
        </View>

        {lastDrain ? (
          <View style={styles.card}>
            <Text style={styles.cardTitle}>last drain</Text>
            <Text style={styles.mono}>
              sent {lastDrain.sent} · accepted {lastDrain.accepted} · rejected {lastDrain.rejected}
              {'\n'}quarantined {lastDrain.quarantined} · dropped {lastDrain.dropped} · remaining{' '}
              {lastDrain.remaining}
            </Text>
            {lastDrain.error ? <Text style={styles.bad}>{lastDrain.error}</Text> : null}
          </View>
        ) : null}

        <Text style={styles.cardTitle}>log</Text>
        {log.map((line, i) => (
          <Text key={`${i}-${line}`} style={styles.logLine}>
            {line}
          </Text>
        ))}
      </ScrollView>
    </View>
  );
}

function Stat({ label, value, tone }: { label: string; value: number; tone?: 'bad' }) {
  return (
    <View style={styles.stat}>
      <Text style={[styles.statValue, tone === 'bad' ? styles.bad : null]}>{value}</Text>
      <Text style={styles.dim}>{label}</Text>
    </View>
  );
}

function Button({
  label,
  onPress,
  disabled,
  primary,
}: {
  label: string;
  onPress: () => void;
  disabled?: boolean;
  primary?: boolean;
}) {
  return (
    <Pressable
      onPress={onPress}
      disabled={disabled}
      style={({ pressed }) => [
        styles.button,
        primary ? styles.buttonPrimary : null,
        pressed || disabled ? styles.buttonDim : null,
      ]}
    >
      <Text style={styles.buttonText}>{label}</Text>
    </Pressable>
  );
}

/**
 * Android clips bold text, and the fix is to avoid synthetic bold entirely.
 *
 * The default font family has no real 600/700 face, so Android synthesises one:
 * it measures the string with the *regular* face, then draws it with widened
 * glyphs. The drawn text is therefore wider than the box reserved for it and
 * the tail is cut off — "Record 250" rendered as "Record", "Drain" as "Drai".
 *
 * The giveaway is that the loss scales with length (one character on "Drain",
 * four on "Record 250") and affects only bold text; "simulate offline" is
 * longer and renders fine at normal weight. That also rules out the two
 * plausible-looking wrong answers: a fixed padding cannot absorb a
 * proportional overflow, and flexShrink is irrelevant because the container is
 * already sized correctly — it is the glyphs that overflow it.
 *
 * `sans-serif-medium` is a real Android font family with its own weight, so
 * nothing is synthesised and measurement matches rendering. iOS has proper
 * weights for the system font and needs none of this.
 */
const bold = Platform.select({
  android: { fontFamily: 'sans-serif-medium', fontWeight: 'normal' as const },
  default: { fontWeight: '600' as const },
});

const heavy = Platform.select({
  android: { fontFamily: 'sans-serif-black', fontWeight: 'normal' as const },
  default: { fontWeight: '700' as const },
});

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: '#0d1117' },
  centre: { alignItems: 'center', justifyContent: 'center', gap: 8 },
  content: { padding: 20, paddingTop: 60, gap: 12 },
  title: { color: '#e6edf3', fontSize: 28, ...bold },
  dim: { color: '#8b949e', fontSize: 13 },
  mono: { color: '#8b949e', fontSize: 12, fontFamily: 'monospace' },
  good: { color: '#3fb950', fontSize: 13, ...bold },
  bad: { color: '#f85149', fontSize: 13, ...bold },
  // Wraps, unlike the single-line status values, because it explains rather
  // than labels.
  warn: { color: '#f85149', fontSize: 13, lineHeight: 18, marginTop: 6 },
  error: { color: '#f85149', fontSize: 18, ...bold },
  row: { flexDirection: 'row', gap: 12, marginTop: 8 },
  stat: {
    flex: 1,
    backgroundColor: '#161b22',
    borderRadius: 10,
    padding: 12,
    alignItems: 'center',
    gap: 2,
  },
  statValue: { color: '#e6edf3', fontSize: 24, ...heavy },
  line: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between' },
  buttons: { flexDirection: 'row', flexWrap: 'wrap', gap: 8, marginTop: 8 },
  button: {
    backgroundColor: '#21262d',
    borderRadius: 8,
    paddingVertical: 10,
    paddingHorizontal: 14,
    flexShrink: 0,
  },
  buttonPrimary: { backgroundColor: '#238636' },
  buttonDim: { opacity: 0.5 },
  buttonText: { color: '#e6edf3', ...bold },
  card: { backgroundColor: '#161b22', borderRadius: 10, padding: 12, gap: 6 },
  cardTitle: { color: '#e6edf3', fontSize: 15, marginTop: 8, ...bold },
  warning: { backgroundColor: '#3d2c00', borderRadius: 8, padding: 10 },
  warningText: { color: '#e3b341', fontSize: 12, lineHeight: 17 },
  logLine: { color: '#8b949e', fontSize: 11, fontFamily: 'monospace' },
});
