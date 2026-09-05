/**
 * Pure decision logic for the outbox.
 *
 * Nothing here touches SQLite, the network, or the clock unless it is passed
 * in. That is deliberate: backoff, quarantine, and response interpretation are
 * the parts most likely to be wrong, and keeping them free of I/O means they
 * can be tested exhaustively and instantly rather than against a device.
 */

import type { OutboxItem, SendOutcome, SyncConfig } from './types';

/**
 * Exponential backoff with full jitter.
 *
 * The jitter is not decoration. Every device in a fleet is driven by the same
 * server, so a shared outage releases them all at once; without jitter they
 * retry in lockstep and recreate the overload that caused the failure. Full
 * jitter — a uniform draw over the whole interval, rather than the interval
 * plus a small wobble — spreads them properly.
 *
 * `random` is injected so tests are deterministic.
 */
export function backoffMs(
  attempt: number,
  cfg: Pick<SyncConfig, 'baseBackoffMs' | 'maxBackoffMs'>,
  random: () => number = Math.random,
): number {
  if (attempt <= 0) return 0;

  // Cap the exponent before shifting. 1 << 31 overflows into negative numbers
  // in JS, which would produce a negative delay and retry instantly forever.
  const exponent = Math.min(attempt - 1, 20);
  const ceiling = Math.min(cfg.baseBackoffMs * 2 ** exponent, cfg.maxBackoffMs);
  return Math.floor(random() * ceiling);
}

/** How a drain pass should act on one server response. */
export interface OutcomePlan {
  /** Delete: either stored, or permanently unacceptable. */
  removeIds: string[];
  /** Transient failure — increment attempts and retry later. */
  failIds: string[];
  /** Past maxAttempts. Move aside so the rest of the queue can drain. */
  quarantineIds: string[];
  retryAfterMs: number | null;
  error: string | null;
  acceptedCount: number;
  rejectedCount: number;
}

/**
 * Decide what to do with a batch given the server's answer.
 *
 * The subtle case is `rejected` inside an accepted response. Those items are
 * removed, not retried: the server has stated they can never be accepted, so
 * keeping them would block everything behind them forever. Dropping them is
 * data loss, which is why the count is surfaced rather than swallowed.
 */
export function planForOutcome(
  batch: OutboxItem[],
  outcome: SendOutcome,
  cfg: Pick<SyncConfig, 'maxAttempts'>,
): OutcomePlan {
  const empty: OutcomePlan = {
    removeIds: [],
    failIds: [],
    quarantineIds: [],
    retryAfterMs: null,
    error: null,
    acceptedCount: 0,
    rejectedCount: 0,
  };

  switch (outcome.kind) {
    case 'accepted': {
      const acknowledged = new Set([...outcome.acceptedIds, ...outcome.rejectedIds]);

      // Anything the server did not mention stays queued. A response that
      // silently omits items must not be read as success for them — that would
      // discard data on a server bug.
      const unacknowledged = batch.filter((item) => !acknowledged.has(item.id));

      return {
        ...empty,
        removeIds: [...acknowledged],
        failIds: unacknowledged.map((i) => i.id),
        acceptedCount: outcome.acceptedIds.length,
        rejectedCount: outcome.rejectedIds.length,
        error: unacknowledged.length
          ? `${unacknowledged.length} item(s) missing from the server response`
          : null,
      };
    }

    case 'shed':
      // Backpressure, not failure. Nothing was stored and nothing is wrong with
      // the data, so attempts are deliberately NOT incremented — otherwise a
      // busy server would quarantine perfectly good readings.
      return { ...empty, retryAfterMs: outcome.retryAfterMs, error: 'server shed load' };

    case 'rejected': {
      // The request was malformed: a bug on this side. Retrying the same bytes
      // cannot help, so these burn attempts and quarantine quickly rather than
      // looping.
      const { toFail, toQuarantine } = splitByAttempts(batch, cfg.maxAttempts);
      return {
        ...empty,
        failIds: toFail,
        quarantineIds: toQuarantine,
        error: outcome.reason,
      };
    }

    case 'unavailable': {
      const { toFail, toQuarantine } = splitByAttempts(batch, cfg.maxAttempts);
      return {
        ...empty,
        failIds: toFail,
        quarantineIds: toQuarantine,
        error: outcome.reason,
      };
    }
  }
}

/**
 * Split a failed batch into "retry" and "give up".
 *
 * An item is quarantined once *this* failure takes it to maxAttempts, so
 * maxAttempts counts total tries rather than retries — five means five requests
 * were made, not six.
 */
function splitByAttempts(
  batch: OutboxItem[],
  maxAttempts: number,
): { toFail: string[]; toQuarantine: string[] } {
  const toFail: string[] = [];
  const toQuarantine: string[] = [];

  for (const item of batch) {
    if (item.attempts + 1 >= maxAttempts) {
      toQuarantine.push(item.id);
    } else {
      toFail.push(item.id);
    }
  }
  return { toFail, toQuarantine };
}

/**
 * Whether a drain should run now.
 *
 * Separated out so the scheduling rule is testable without waiting in real
 * time.
 */
export function shouldDrain(args: {
  now: number;
  queueDepth: number;
  nextAttemptAt: number | null;
  online: boolean;
  draining: boolean;
}): boolean {
  if (args.draining) return false;
  if (args.queueDepth === 0) return false;
  // Skipping while offline saves the radio wake-up and a certain failure. The
  // connectivity signal can be wrong, so a drain is also triggered by the
  // periodic timer rather than relying on it alone.
  if (!args.online) return false;
  if (args.nextAttemptAt !== null && args.now < args.nextAttemptAt) return false;
  return true;
}
