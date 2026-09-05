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
import { StatusBar } from 'expo-status-bar';
import { useCallback, useEffect, useRef, useState } from 'react';
import {
  ActivityIndicator,
  Pressable,
  ScrollView,
  StyleSheet,
  Switch,
  Text,
  View,
} from 'react-native';
import { v7 as uuidv7 } from 'uuid';

import { HttpTransport } from './src/api/transport';
import { API_BASE_URL, API_URL_IS_FALLBACK, DEVICE_ID_SETTING } from './src/config';
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

  const storeRef = useRef<SqliteOutbox | null>(null);
  const engineRef = useRef<SyncEngine | null>(null);
  const transportRef = useRef<ToggleableTransport | null>(null);

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
        note(`ready — api ${API_BASE_URL}`);
      } catch (err) {
        if (!cancelled) setFatal(err instanceof Error ? err.message : String(err));
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [note]);

  // Real connectivity, distinct from the simulated toggle above.
  useEffect(() => NetInfo.addEventListener((s) => setNetworkOnline(Boolean(s.isConnected))), []);

  useEffect(() => {
    if (transportRef.current) transportRef.current.online = !simulateOffline;
  }, [simulateOffline]);

  const refresh = useCallback(async () => {
    const engine = engineRef.current;
    if (engine) setStatus(await engine.status());
  }, []);

  useEffect(() => {
    if (!ready) return;
    void refresh();
    const timer = setInterval(() => void refresh(), 1000);
    return () => clearInterval(timer);
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

  const drain = useCallback(async () => {
    const engine = engineRef.current;
    if (!engine) return;
    setBusy(true);
    try {
      const result = await engine.drain();
      setLastDrain(result);
      note(
        result.sent === 0
          ? 'drain: nothing queued'
          : `drain: sent ${result.sent}, accepted ${result.accepted}` +
              (result.rejected ? `, rejected ${result.rejected}` : '') +
              (result.error ? ` — ${result.error}` : ''),
      );
      await refresh();
    } catch (err) {
      note(`drain threw: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setBusy(false);
    }
  }, [note, refresh]);

  const clear = useCallback(async () => {
    await storeRef.current?.clear();
    setLastDrain(null);
    note('cleared queue and quarantine');
    await refresh();
  }, [note, refresh]);

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

        {backoffRemaining > 0 ? (
          <Text style={styles.dim}>backing off for {(backoffRemaining / 1000).toFixed(1)}s</Text>
        ) : null}

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

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: '#0d1117' },
  centre: { alignItems: 'center', justifyContent: 'center', gap: 8 },
  content: { padding: 20, paddingTop: 60, gap: 12 },
  title: { color: '#e6edf3', fontSize: 28, fontWeight: '600' },
  dim: { color: '#8b949e', fontSize: 13 },
  mono: { color: '#8b949e', fontSize: 12, fontFamily: 'monospace' },
  good: { color: '#3fb950', fontSize: 13, fontWeight: '600' },
  bad: { color: '#f85149', fontSize: 13, fontWeight: '600' },
  error: { color: '#f85149', fontSize: 18, fontWeight: '600' },
  row: { flexDirection: 'row', gap: 12, marginTop: 8 },
  stat: {
    flex: 1,
    backgroundColor: '#161b22',
    borderRadius: 10,
    padding: 12,
    alignItems: 'center',
    gap: 2,
  },
  statValue: { color: '#e6edf3', fontSize: 24, fontWeight: '700' },
  line: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between' },
  buttons: { flexDirection: 'row', flexWrap: 'wrap', gap: 8, marginTop: 8 },
  button: {
    backgroundColor: '#21262d',
    borderRadius: 8,
    paddingVertical: 10,
    paddingHorizontal: 14,
  },
  buttonPrimary: { backgroundColor: '#238636' },
  buttonDim: { opacity: 0.5 },
  buttonText: { color: '#e6edf3', fontWeight: '600' },
  card: { backgroundColor: '#161b22', borderRadius: 10, padding: 12, gap: 6 },
  cardTitle: { color: '#e6edf3', fontSize: 15, fontWeight: '600', marginTop: 8 },
  warning: { backgroundColor: '#3d2c00', borderRadius: 8, padding: 10 },
  warningText: { color: '#e3b341', fontSize: 12, lineHeight: 17 },
  logLine: { color: '#8b949e', fontSize: 11, fontFamily: 'monospace' },
});
