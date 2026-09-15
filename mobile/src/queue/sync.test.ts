import { at } from '../testing';
import { InMemoryOutbox } from './memory';
import { SyncEngine, type SyncEvent } from './sync';
import {
  DEFAULT_SYNC_CONFIG,
  type OutboxItem,
  type SendOutcome,
  type SyncConfig,
  type Transport,
} from './types';

/** Records what was sent and replies with whatever the test queues up. */
class FakeTransport implements Transport {
  sent: OutboxItem[][] = [];
  private outcomes: SendOutcome[] = [];
  throwOnce = false;

  queue(...outcomes: SendOutcome[]): this {
    this.outcomes.push(...outcomes);
    return this;
  }

  /** Discards any queued outcomes, so send() falls back to accepting. */
  drainQueuedOutcomes(): void {
    this.outcomes = [];
  }

  async send(_deviceId: string, items: OutboxItem[]): Promise<SendOutcome> {
    this.sent.push(items);

    if (this.throwOnce) {
      this.throwOnce = false;
      throw new Error('transport exploded');
    }

    const next = this.outcomes.shift();
    if (next) return next;

    // Default: acknowledge exactly what was offered.
    return { kind: 'accepted', acceptedIds: items.map((i) => i.id), rejectedIds: [] };
  }
}

let seq = 0;
function makeItem(overrides: Partial<OutboxItem> = {}): OutboxItem {
  seq += 1;
  // Zero-padded so lexical ordering matches insertion order, which is what the
  // store sorts on.
  const stamp = new Date(Date.UTC(2026, 8, 5, 10, 0, 0) + seq * 1000).toISOString();
  return {
    id: `id-${String(seq).padStart(5, '0')}`,
    kind: 'position',
    recordedAt: stamp,
    payload: { lat: 49.28, lon: -123.12 },
    attempts: 0,
    lastError: null,
    createdAt: stamp,
    ...overrides,
  };
}

function makeEngine(
  cfg: Partial<SyncConfig> = {},
  opts: { transport?: FakeTransport; events?: SyncEvent[] } = {},
) {
  const store = new InMemoryOutbox();
  const transport = opts.transport ?? new FakeTransport();
  const events = opts.events ?? [];
  let clock = 1_000_000;

  const engine = new SyncEngine(
    store,
    transport,
    'test-device',
    { ...DEFAULT_SYNC_CONFIG, ...cfg },
    {
      now: () => clock,
      random: () => 0.5,
      onEvent: (e) => events.push(e),
    },
  );

  return {
    store,
    transport,
    engine,
    events,
    advance: (ms: number) => {
      clock += ms;
    },
    clockNow: () => clock,
  };
}

beforeEach(() => {
  seq = 0;
});

describe('enqueue and drain', () => {
  it('sends queued items and removes those the server accepted', async () => {
    const { engine, store, transport } = makeEngine();
    await engine.enqueue([makeItem(), makeItem(), makeItem()]);

    const result = await engine.drain();

    expect(transport.sent).toHaveLength(1);
    expect(result.sent).toBe(3);
    expect(result.accepted).toBe(3);
    expect(await store.count()).toBe(0);
  });

  it('does nothing when the queue is empty', async () => {
    const { engine, transport } = makeEngine();
    const result = await engine.drain();

    expect(transport.sent).toHaveLength(0);
    expect(result).toMatchObject({ sent: 0, accepted: 0, remaining: 0 });
  });

  it('sends at most one batch per drain', async () => {
    const { engine, store, transport } = makeEngine({ batchSize: 2 });
    await engine.enqueue([makeItem(), makeItem(), makeItem(), makeItem(), makeItem()]);

    await engine.drain();

    expect(at(transport.sent, 0, 'sent batches')).toHaveLength(2);
    expect(await store.count()).toBe(3);
  });

  it('drains oldest first', async () => {
    const { engine, transport } = makeEngine({ batchSize: 2 });
    const first = makeItem();
    const second = makeItem();
    await engine.enqueue([first, second, makeItem(), makeItem()]);

    await engine.drain();

    expect(at(transport.sent, 0, 'sent batches').map((i) => i.id)).toEqual([first.id, second.id]);
  });
});

describe('durability', () => {
  // The property the whole design rests on: nothing leaves the queue until the
  // server confirms it, so a crash mid-request costs a resend, not data.
  it('keeps items when the transport throws', async () => {
    const transport = new FakeTransport();
    transport.throwOnce = true;
    const { engine, store } = makeEngine({}, { transport });

    await engine.enqueue([makeItem(), makeItem()]);
    await expect(engine.drain()).rejects.toThrow('transport exploded');

    expect(await store.count()).toBe(2);
  });

  // A transport that throws must not wedge the engine into a permanently
  // "draining" state, which would silently stop syncing for good.
  it('recovers and drains normally after a transport throws', async () => {
    const transport = new FakeTransport();
    transport.throwOnce = true;
    const { engine, store } = makeEngine({}, { transport });

    await engine.enqueue([makeItem()]);
    await expect(engine.drain()).rejects.toThrow();

    const result = await engine.drain();
    expect(result.accepted).toBe(1);
    expect(await store.count()).toBe(0);
  });

  it('re-enqueueing an item already queued does not reset its attempt count', async () => {
    const { engine, store } = makeEngine();
    const item = makeItem();

    await engine.enqueue([item]);
    await store.recordFailure([item.id], 'network down');
    await engine.enqueue([{ ...item, attempts: 0 }]);

    const stored = at(await store.peek(1), 0, 'queue');
    expect(stored.attempts).toBe(1);
  });
});

describe('failure handling', () => {
  it('keeps items and backs off when the server is unavailable', async () => {
    const transport = new FakeTransport().queue({ kind: 'unavailable', reason: 'network down' });
    const { engine, store } = makeEngine({}, { transport });
    await engine.enqueue([makeItem()]);

    const result = await engine.drain();

    expect(result.accepted).toBe(0);
    expect(result.error).toBe('network down');
    expect(await store.count()).toBe(1);
    expect(await engine.ready(true)).toBe(false); // backing off
  });

  it('becomes ready again once the backoff elapses', async () => {
    const transport = new FakeTransport().queue({ kind: 'unavailable', reason: 'network down' });
    const { engine, advance } = makeEngine(
      { baseBackoffMs: 1000, maxBackoffMs: 60_000 },
      { transport },
    );
    await engine.enqueue([makeItem()]);
    await engine.drain();

    expect(await engine.ready(true)).toBe(false);
    advance(60_000);
    expect(await engine.ready(true)).toBe(true);
  });

  // Uses `rejected`, not `unavailable`. A network failure is never quarantined
  // — see the long-outage tests below for why.
  it('quarantines a rejected item after the attempt limit so the queue can drain', async () => {
    const transport = new FakeTransport();
    for (let i = 0; i < 3; i++) {
      transport.queue({ kind: 'rejected', reason: 'HTTP 400: malformed batch' });
    }

    const { engine, store, advance, events } = makeEngine(
      { maxAttempts: 3, baseBackoffMs: 1 },
      { transport },
    );
    const stuck = makeItem();
    await engine.enqueue([stuck]);

    for (let i = 0; i < 3; i++) {
      await engine.drain();
      advance(10_000);
    }

    expect(await store.count()).toBe(0);
    expect(await store.deadCount()).toBe(1);
    expect(at(store.deadItems(), 0, 'dead items').item.id).toBe(stuck.id);
    expect(events.some((e) => e.type === 'quarantined')).toBe(true);
  });

  // A stuck item must not hold up everything behind it.
  it('lets later items through once a poison item is quarantined', async () => {
    const transport = new FakeTransport();
    for (let i = 0; i < 2; i++) {
      transport.queue({ kind: 'rejected', reason: 'HTTP 400: malformed batch' });
    }

    const { engine, store, advance } = makeEngine(
      { maxAttempts: 2, batchSize: 1, baseBackoffMs: 1 },
      { transport },
    );
    const poison = makeItem();
    const good = makeItem();
    await engine.enqueue([poison, good]);

    await engine.drain();
    advance(10_000);
    await engine.drain(); // poison hits the limit here
    advance(10_000);
    const third = await engine.drain(); // now the good item goes

    expect(third.accepted).toBe(1);
    expect(await store.count()).toBe(0);
    expect(await store.deadCount()).toBe(1);
  });
});

describe('backpressure', () => {
  // Shedding is the server asking for patience, not reporting bad data. If it
  // counted against attempts, a busy server would quarantine good readings.
  it('does not count a shed response against the attempt limit', async () => {
    const transport = new FakeTransport();
    for (let i = 0; i < 10; i++) transport.queue({ kind: 'shed', retryAfterMs: 2000 });

    const { engine, store, advance } = makeEngine({ maxAttempts: 3 }, { transport });
    await engine.enqueue([makeItem()]);

    for (let i = 0; i < 10; i++) {
      await engine.drain();
      advance(60_000);
    }

    expect(await store.deadCount()).toBe(0);
    expect(await store.count()).toBe(1);
  });

  it('waits at least as long as the server asked', async () => {
    const transport = new FakeTransport().queue({ kind: 'shed', retryAfterMs: 5000 });
    const { engine, advance } = makeEngine({}, { transport });
    await engine.enqueue([makeItem()]);

    await engine.drain();

    advance(4_999);
    expect(await engine.ready(true)).toBe(false);
    // 5000 plus jitter (random 0.5 of half the interval = 1250).
    advance(2_000);
    expect(await engine.ready(true)).toBe(true);
  });

  it('drops the server-rejected items and keeps going', async () => {
    const { engine, store } = makeEngine({}, {});
    const good = makeItem();
    const bad = makeItem();
    await engine.enqueue([good, bad]);

    const transport = new FakeTransport().queue({
      kind: 'accepted',
      acceptedIds: [good.id],
      rejectedIds: [bad.id],
    });
    const engine2 = new SyncEngine(store, transport, 'dev', DEFAULT_SYNC_CONFIG, {
      now: () => 1,
      random: () => 0.5,
    });

    const result = await engine2.drain();

    expect(result.accepted).toBe(1);
    expect(result.rejected).toBe(1);
    expect(await store.count()).toBe(0);
    // Rejected items are dropped, not quarantined: the server has already said
    // they can never be accepted, so there is nothing to inspect later.
    expect(await store.deadCount()).toBe(0);
  });
});

describe('queue cap', () => {
  it('drops the oldest items past the cap and reports the loss', async () => {
    const { engine, store, events } = makeEngine({ maxQueueSize: 5 });

    const items = Array.from({ length: 8 }, () => makeItem());
    await engine.enqueue(items);

    expect(await store.count()).toBe(5);

    const remaining = await store.peek(10);
    // The three oldest were sacrificed; recent telemetry is more useful.
    expect(remaining.map((i) => i.id)).toEqual(items.slice(3).map((i) => i.id));

    const dropEvent = events.find((e) => e.type === 'dropped');
    expect(dropEvent).toEqual({ type: 'dropped', count: 3 });
  });

  it('enforces the cap on enqueue, not only on drain', async () => {
    // The scenario needing the cap is a device offline for days, where drains
    // never succeed and would never run the trim.
    const { engine, store } = makeEngine({ maxQueueSize: 3 });

    for (let i = 0; i < 10; i++) {
      await engine.enqueue([makeItem()]);
      expect(await store.count()).toBeLessThanOrEqual(3);
    }
  });
});

describe('status', () => {
  it('reports depth, dead count, and backoff state', async () => {
    const transport = new FakeTransport().queue({ kind: 'unavailable', reason: 'down' });
    const { engine, clockNow } = makeEngine({ baseBackoffMs: 1000 }, { transport });
    await engine.enqueue([makeItem(), makeItem()]);

    await engine.drain();
    const status = await engine.status();

    expect(status.depth).toBe(2);
    expect(status.dead).toBe(0);
    expect(status.draining).toBe(false);
    expect(status.consecutiveFailures).toBe(1);
    expect(status.nextAttemptAt).toBeGreaterThan(clockNow() - 1);
  });
});

// The scenario this queue exists for: a driver leaves coverage, keeps
// recording for a long time, and comes back.
//
// Written after a real near-miss. `unavailable` originally counted against
// maxAttempts, the same as a malformed request. With the background task
// draining every ten seconds, that quarantined the oldest readings about fifty
// seconds into any outage — and quarantined data never uploads, even once
// signal returns. A thirty-minute walk would have lost nearly all of it, and
// the queue would have looked healthy the whole time.
describe('a long outage', () => {
  function offlineTransport() {
    const t = new FakeTransport();
    for (let i = 0; i < 500; i++) {
      t.queue({ kind: 'unavailable', reason: 'Network request failed' });
    }
    return t;
  }

  it('never quarantines readings, however long the outage lasts', async () => {
    const transport = offlineTransport();
    const { engine, store, advance } = makeEngine(
      { maxAttempts: 5, batchSize: 10, baseBackoffMs: 1 },
      { transport },
    );

    await engine.enqueue(Array.from({ length: 30 }, () => makeItem()));

    // 200 drain attempts is a bit over half an hour at a ten-second cadence.
    for (let i = 0; i < 200; i++) {
      await engine.drain();
      advance(1000);
    }

    expect(await store.deadCount()).toBe(0);
    expect(await store.count()).toBe(30);
  });

  it('uploads everything once coverage returns', async () => {
    const transport = offlineTransport();
    const { engine, store, advance } = makeEngine(
      { maxAttempts: 5, batchSize: 10, baseBackoffMs: 1 },
      { transport },
    );

    // Record while offline, in bursts, as a device on a route would.
    for (let burst = 0; burst < 3; burst++) {
      await engine.enqueue(Array.from({ length: 10 }, () => makeItem()));
      for (let i = 0; i < 20; i++) {
        await engine.drain();
        advance(1000);
      }
    }
    expect(await store.count()).toBe(30);
    expect(await store.deadCount()).toBe(0);

    // Back in range: the queued outcomes run out, so the fake starts accepting.
    transport.drainQueuedOutcomes();

    for (let i = 0; i < 5; i++) {
      await engine.drain();
      advance(1000);
    }

    expect(await store.count()).toBe(0);
    expect(await store.deadCount()).toBe(0);
  });

  // The limit still applies where it belongs: a request the server refuses as
  // malformed will never succeed, so it must not block the queue forever.
  it('still quarantines a request the server rejects as malformed', async () => {
    const transport = new FakeTransport();
    for (let i = 0; i < 10; i++) {
      transport.queue({ kind: 'rejected', reason: 'HTTP 400: malformed batch' });
    }

    const { engine, store, advance } = makeEngine(
      { maxAttempts: 3, batchSize: 10, baseBackoffMs: 1 },
      { transport },
    );
    await engine.enqueue(Array.from({ length: 5 }, () => makeItem()));

    for (let i = 0; i < 5; i++) {
      await engine.drain();
      advance(1000);
    }

    expect(await store.deadCount()).toBe(5);
    expect(await store.count()).toBe(0);
  });

  // An outage longer than the queue can hold must lose the OLDEST data, and say
  // so — not the newest, and not silently.
  it('drops the stalest readings at the cap rather than the freshest', async () => {
    const transport = offlineTransport();
    const { engine, store, events } = makeEngine(
      { maxQueueSize: 20, batchSize: 10, baseBackoffMs: 1 },
      { transport },
    );

    const all = Array.from({ length: 50 }, () => makeItem());
    for (const item of all) await engine.enqueue([item]);

    expect(await store.count()).toBe(20);
    const remaining = (await store.peek(50)).map((i) => i.id);
    expect(remaining).toEqual(all.slice(30).map((i) => i.id));

    const dropped = events.filter((e) => e.type === 'dropped');
    expect(dropped.length).toBeGreaterThan(0);
  });
});
