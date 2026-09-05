/**
 * The sync engine: drains the outbox to the server.
 *
 * Holds no in-flight state. A row is deleted only once the server has confirmed
 * it, so a process killed mid-request simply resends — and the server's
 * `ON CONFLICT DO NOTHING` absorbs the duplicate. Writing "sending" to disk
 * before the request and "sent" after is the usual design, and it is precisely
 * that gap which loses data when the OS kills the app between the two.
 */

import { backoffMs, planForOutcome, shouldDrain } from './policy';
import {
  DEFAULT_SYNC_CONFIG,
  EMPTY_DRAIN,
  type DrainResult,
  type OutboxItem,
  type OutboxStore,
  type SyncConfig,
  type Transport,
} from './types';

export interface SyncDeps {
  /** Injected so tests control time instead of waiting for it. */
  now?: () => number;
  random?: () => number;
  onEvent?: (event: SyncEvent) => void;
}

export type SyncEvent =
  | { type: 'drained'; result: DrainResult }
  | { type: 'dropped'; count: number }
  | { type: 'quarantined'; count: number; reason: string };

export class SyncEngine {
  private draining = false;
  private nextAttemptAt: number | null = null;
  private consecutiveFailures = 0;
  private readonly now: () => number;
  private readonly random: () => number;
  private readonly onEvent: (event: SyncEvent) => void;

  constructor(
    private readonly store: OutboxStore,
    private readonly transport: Transport,
    private readonly deviceId: string,
    private readonly cfg: SyncConfig = DEFAULT_SYNC_CONFIG,
    deps: SyncDeps = {},
  ) {
    this.now = deps.now ?? Date.now;
    this.random = deps.random ?? Math.random;
    this.onEvent = deps.onEvent ?? (() => {});
  }

  /**
   * Add items to the queue, enforcing the size cap.
   *
   * The cap is enforced here rather than only at drain time, because the
   * scenario that needs it — a device offline for days — is one where drains
   * never succeed and would otherwise never run the trim.
   */
  async enqueue(items: OutboxItem[]): Promise<void> {
    if (items.length === 0) return;
    await this.store.enqueue(items);

    const dropped = await this.store.trimToCap(this.cfg.maxQueueSize);
    if (dropped > 0) {
      // Data loss. Reported rather than swallowed: a queue quietly discarding
      // readings looks identical to a vehicle that never moved.
      this.onEvent({ type: 'dropped', count: dropped });
    }
  }

  /** Whether a drain would run right now. */
  async ready(online: boolean): Promise<boolean> {
    return shouldDrain({
      now: this.now(),
      queueDepth: await this.store.count(),
      nextAttemptAt: this.nextAttemptAt,
      online,
      draining: this.draining,
    });
  }

  /**
   * Send one batch.
   *
   * One batch per call, not a loop until empty. A device draining a long
   * backlog should yield between batches so the UI stays responsive and the
   * caller can stop on connectivity loss; the scheduler decides how eagerly to
   * come back.
   */
  async drain(): Promise<DrainResult> {
    if (this.draining) return { ...EMPTY_DRAIN, error: 'already draining' };
    this.draining = true;

    try {
      const dropped = await this.store.trimToCap(this.cfg.maxQueueSize);
      if (dropped > 0) this.onEvent({ type: 'dropped', count: dropped });

      const batch = await this.store.peek(this.cfg.batchSize);
      if (batch.length === 0) {
        this.nextAttemptAt = null;
        this.consecutiveFailures = 0;
        return { ...EMPTY_DRAIN, dropped, remaining: await this.store.count() };
      }

      const outcome = await this.transport.send(this.deviceId, batch);
      const plan = planForOutcome(batch, outcome, this.cfg);

      if (plan.removeIds.length) await this.store.remove(plan.removeIds);
      if (plan.failIds.length) {
        await this.store.recordFailure(plan.failIds, plan.error ?? 'unknown error');
      }
      if (plan.quarantineIds.length) {
        await this.store.quarantine(plan.quarantineIds, plan.error ?? 'unknown error');
        this.onEvent({
          type: 'quarantined',
          count: plan.quarantineIds.length,
          reason: plan.error ?? 'unknown error',
        });
      }

      this.scheduleNextAttempt(outcome.kind, plan.retryAfterMs);

      const result: DrainResult = {
        sent: batch.length,
        accepted: plan.acceptedCount,
        rejected: plan.rejectedCount,
        quarantined: plan.quarantineIds.length,
        dropped,
        remaining: await this.store.count(),
        retryAfterMs: plan.retryAfterMs,
        error: plan.error,
      };
      this.onEvent({ type: 'drained', result });
      return result;
    } finally {
      // In a finally block so a transport that throws cannot wedge the engine
      // into a permanently "draining" state, which would silently stop all
      // syncing for the rest of the process's life.
      this.draining = false;
    }
  }

  private scheduleNextAttempt(kind: string, retryAfterMs: number | null): void {
    if (kind === 'accepted') {
      this.consecutiveFailures = 0;
      this.nextAttemptAt = null;
      return;
    }

    if (retryAfterMs !== null) {
      // The server told us when to come back. Honour it, but add jitter so a
      // fleet released from one outage does not return in lockstep.
      const jitter = Math.floor(this.random() * (retryAfterMs / 2));
      this.nextAttemptAt = this.now() + retryAfterMs + jitter;
      return;
    }

    this.consecutiveFailures += 1;
    this.nextAttemptAt = this.now() + backoffMs(this.consecutiveFailures, this.cfg, this.random);
  }

  /** Snapshot for the debug screen. */
  async status(): Promise<{
    depth: number;
    dead: number;
    draining: boolean;
    nextAttemptAt: number | null;
    consecutiveFailures: number;
  }> {
    return {
      depth: await this.store.count(),
      dead: await this.store.deadCount(),
      draining: this.draining,
      nextAttemptAt: this.nextAttemptAt,
      consecutiveFailures: this.consecutiveFailures,
    };
  }
}
