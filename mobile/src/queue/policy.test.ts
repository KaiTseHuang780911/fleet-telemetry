import { backoffMs, planForOutcome, shouldDrain } from './policy';
import { DEFAULT_SYNC_CONFIG, type OutboxItem, type SyncConfig } from './types';

function item(id: string, attempts = 0): OutboxItem {
  return {
    id,
    kind: 'position',
    recordedAt: '2026-09-05T10:00:00.000Z',
    payload: {},
    attempts,
    lastError: null,
    createdAt: '2026-09-05T10:00:00.000Z',
  };
}

const cfg: SyncConfig = DEFAULT_SYNC_CONFIG;

describe('backoffMs', () => {
  it('returns no delay before the first attempt', () => {
    expect(backoffMs(0, cfg, () => 1)).toBe(0);
  });

  it('doubles the ceiling with each attempt', () => {
    // random() === 1 exposes the ceiling itself.
    const atMax = (attempt: number) => backoffMs(attempt, cfg, () => 1);
    expect(atMax(1)).toBe(cfg.baseBackoffMs);
    expect(atMax(2)).toBe(cfg.baseBackoffMs * 2);
    expect(atMax(3)).toBe(cfg.baseBackoffMs * 4);
    expect(atMax(4)).toBe(cfg.baseBackoffMs * 8);
  });

  it('never exceeds the maximum', () => {
    for (let attempt = 1; attempt <= 50; attempt++) {
      expect(backoffMs(attempt, cfg, () => 1)).toBeLessThanOrEqual(cfg.maxBackoffMs);
    }
  });

  // A large attempt count must not overflow the shift into a negative number,
  // which would produce a negative delay and retry instantly forever.
  it('stays non-negative at absurd attempt counts', () => {
    for (const attempt of [31, 32, 33, 64, 1000, Number.MAX_SAFE_INTEGER]) {
      const delay = backoffMs(attempt, cfg, () => 1);
      expect(delay).toBeGreaterThanOrEqual(0);
      expect(Number.isFinite(delay)).toBe(true);
    }
  });

  // Full jitter: the draw spans the whole interval, not the ceiling plus a
  // wobble. Without it a fleet released from a shared outage retries in
  // lockstep and recreates the overload.
  it('spreads retries across the whole interval', () => {
    expect(backoffMs(5, cfg, () => 0)).toBe(0);
    expect(backoffMs(5, cfg, () => 0.5)).toBe(Math.floor(cfg.baseBackoffMs * 16 * 0.5));
    expect(backoffMs(5, cfg, () => 1)).toBe(cfg.baseBackoffMs * 16);
  });
});

describe('planForOutcome', () => {
  it('removes everything the server acknowledged', () => {
    const batch = [item('a'), item('b'), item('c')];
    const plan = planForOutcome(
      batch,
      { kind: 'accepted', acceptedIds: ['a', 'b', 'c'], rejectedIds: [] },
      cfg,
    );

    expect(plan.removeIds.sort()).toEqual(['a', 'b', 'c']);
    expect(plan.failIds).toEqual([]);
    expect(plan.acceptedCount).toBe(3);
  });

  // The poison-message case. The server has said these can never be accepted,
  // so keeping them would block the queue permanently.
  it('drops rejected items rather than retrying them forever', () => {
    const batch = [item('good'), item('poison')];
    const plan = planForOutcome(
      batch,
      { kind: 'accepted', acceptedIds: ['good'], rejectedIds: ['poison'] },
      cfg,
    );

    expect(plan.removeIds.sort()).toEqual(['good', 'poison']);
    expect(plan.failIds).toEqual([]);
    expect(plan.acceptedCount).toBe(1);
    expect(plan.rejectedCount).toBe(1);
  });

  // A server bug that omits items must not be read as success for them.
  it('keeps items the server did not mention', () => {
    const batch = [item('a'), item('b'), item('ghost')];
    const plan = planForOutcome(
      batch,
      { kind: 'accepted', acceptedIds: ['a'], rejectedIds: ['b'] },
      cfg,
    );

    expect(plan.removeIds.sort()).toEqual(['a', 'b']);
    expect(plan.failIds).toEqual(['ghost']);
    expect(plan.error).toMatch(/missing from the server response/);
  });

  // Shedding is backpressure, not failure. Counting it against attempts would
  // let a busy server quarantine perfectly good readings.
  it('does not burn attempts when the server sheds load', () => {
    const batch = [item('a', 3), item('b', 4)];
    const plan = planForOutcome(batch, { kind: 'shed', retryAfterMs: 2000 }, cfg);

    expect(plan.removeIds).toEqual([]);
    expect(plan.failIds).toEqual([]);
    expect(plan.quarantineIds).toEqual([]);
    expect(plan.retryAfterMs).toBe(2000);
  });

  it('retries transient failures below the attempt limit', () => {
    const batch = [item('a', 0), item('b', 1)];
    const plan = planForOutcome(
      batch,
      { kind: 'unavailable', reason: 'network down' },
      { maxAttempts: 5 },
    );

    expect(plan.failIds.sort()).toEqual(['a', 'b']);
    expect(plan.quarantineIds).toEqual([]);
    expect(plan.error).toBe('network down');
  });

  it('quarantines items once this failure reaches the attempt limit', () => {
    const batch = [item('fresh', 0), item('tired', 4)];
    const plan = planForOutcome(
      batch,
      { kind: 'unavailable', reason: 'network down' },
      { maxAttempts: 5 },
    );

    expect(plan.failIds).toEqual(['fresh']);
    // 4 prior attempts plus this one is the fifth: the limit counts total
    // tries, not retries.
    expect(plan.quarantineIds).toEqual(['tired']);
  });

  it('treats a malformed request as a bug worth quarantining, not a transient fault', () => {
    const batch = [item('a', 4)];
    const plan = planForOutcome(
      batch,
      { kind: 'rejected', reason: 'batch must contain readings or stop_events' },
      { maxAttempts: 5 },
    );

    expect(plan.quarantineIds).toEqual(['a']);
    expect(plan.error).toMatch(/must contain readings/);
  });

  it('handles an empty batch without inventing work', () => {
    for (const outcome of [
      { kind: 'accepted' as const, acceptedIds: [], rejectedIds: [] },
      { kind: 'shed' as const, retryAfterMs: 1000 },
      { kind: 'unavailable' as const, reason: 'offline' },
    ]) {
      const plan = planForOutcome([], outcome, cfg);
      expect(plan.removeIds).toEqual([]);
      expect(plan.failIds).toEqual([]);
      expect(plan.quarantineIds).toEqual([]);
    }
  });
});

describe('shouldDrain', () => {
  const base = {
    now: 1_000_000,
    queueDepth: 10,
    nextAttemptAt: null as number | null,
    online: true,
    draining: false,
  };

  it('drains when there is work, connectivity, and no backoff pending', () => {
    expect(shouldDrain(base)).toBe(true);
  });

  it.each([
    ['already draining', { draining: true }],
    ['queue empty', { queueDepth: 0 }],
    ['offline', { online: false }],
    ['still backing off', { nextAttemptAt: 1_000_001 }],
  ])('does not drain when %s', (_name, override) => {
    expect(shouldDrain({ ...base, ...override })).toBe(false);
  });

  it('drains once the backoff deadline has passed', () => {
    expect(shouldDrain({ ...base, nextAttemptAt: base.now })).toBe(true);
    expect(shouldDrain({ ...base, nextAttemptAt: base.now - 1 })).toBe(true);
  });

  // Concurrent drains would send the same rows twice. Harmless server-side
  // thanks to idempotency, but it wastes the radio, which is the scarce
  // resource on a phone.
  it('refuses to start a second drain while one is running', () => {
    expect(shouldDrain({ ...base, draining: true, queueDepth: 5000 })).toBe(false);
  });
});
