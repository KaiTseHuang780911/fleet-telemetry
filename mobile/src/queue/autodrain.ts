/**
 * Decides when the app should drain on its own.
 *
 * Exists because the scheduling logic in `policy.ts` was written, tested, and
 * then never called. `shouldDrain` had five passing tests and `engine.ready()`
 * — its only caller — appeared nowhere outside the test suite. The result was
 * an app that uploaded only when a GPS fix happened to arrive or the user
 * pressed a button, so a queue filled while offline could sit indefinitely once
 * tracking stopped.
 *
 * Tested logic that is not in the execution path is worse than no logic: it
 * reads as covered, and the coverage is real, but it governs nothing.
 *
 * This module is the wiring, kept pure so the triggering rules are testable
 * without React, a radio, or a clock.
 */

/** Why a drain was attempted. Surfaced in the debug log to make cause visible. */
export type DrainTrigger = 'reconnect' | 'periodic' | 'foreground' | 'manual';

export interface AutoDrainState {
  /** Connectivity as last reported. May be stale or simply wrong. */
  online: boolean;
  /** Whether the app is in the foreground. */
  active: boolean;
  queueDepth: number;
  draining: boolean;
  /** Set while backing off after a failure. */
  nextAttemptAt: number | null;
  now: number;
}

/**
 * Whether a trigger should actually result in a send.
 *
 * `reconnect` and `foreground` deliberately ignore the `online` flag. Both are
 * edge events that mean "something just changed"; NetInfo is not reliable about
 * reachability — a phone can report connected on a captive portal, or report
 * disconnected momentarily while switching networks — so refusing to try on the
 * strength of that flag would skip the one moment most likely to succeed. The
 * cost of being wrong is one failed request that the queue absorbs anyway.
 *
 * The periodic trigger does respect it, because that one fires repeatedly and a
 * wrong flag would otherwise mean a radio wake-up every interval for nothing.
 */
export function shouldAutoDrain(trigger: DrainTrigger, s: AutoDrainState): boolean {
  if (s.draining) return false;
  if (s.queueDepth === 0) return false;

  // Backoff applies to every trigger. A server that just told us to wait does
  // not become ready because the user opened the app.
  if (s.nextAttemptAt !== null && s.now < s.nextAttemptAt) return false;

  switch (trigger) {
    case 'manual':
      return true;
    case 'reconnect':
    case 'foreground':
      return true;
    case 'periodic':
      return s.online && s.active;
  }
}

/**
 * How often the periodic trigger fires while the app is open.
 *
 * Thirty seconds is a compromise: long enough that an idle app with an empty
 * queue costs nothing, short enough that a user watching the screen sees the
 * queue clear without reaching for a button. It only ever fires while the app
 * is in the foreground — background uploads are the location task's job, since
 * that already wakes for its own reasons and can piggyback.
 */
export const PERIODIC_DRAIN_MS = 30_000;
