/**
 * Types for the durable outbox.
 *
 * The queue is the reason Phase 1 put a client-generated UUIDv7 on the server's
 * primary key. Delivery here is at-least-once: rows are removed only after the
 * server confirms them, so a process killed mid-request resends. Combined with
 * the server's `ON CONFLICT (reading_id) DO NOTHING`, that yields
 * effectively-once without this side ever tracking in-flight state — and it is
 * exactly that in-flight state, written to disk between "sending" and "sent",
 * that loses data when an app is killed at the wrong moment.
 */

/** What an outbox row carries. */
export type OutboxKind = 'position' | 'stop_event';

export interface OutboxItem {
  /**
   * Client-generated UUIDv7. This is the id the server deduplicates on, so it
   * must be generated once at enqueue time and never regenerated on retry —
   * a fresh id on every attempt would turn every retry into a duplicate row.
   */
  id: string;
  kind: OutboxKind;
  /** Device clock at capture, ISO 8601. */
  recordedAt: string;
  /** The wire object, already shaped for the server. */
  payload: unknown;
  attempts: number;
  lastError: string | null;
  /** Device clock at enqueue, ISO 8601. Drives FIFO ordering and the age cap. */
  createdAt: string;
}

/** What the server said, reduced to what the queue needs to decide. */
export type SendOutcome =
  | { kind: 'accepted'; acceptedIds: string[]; rejectedIds: string[] }
  /** Server is shedding load. Nothing was stored; retry after the delay. */
  | { kind: 'shed'; retryAfterMs: number }
  /**
   * The request itself was malformed — a bug on this side, not a transient
   * fault. Retrying identical bytes cannot help, so these items burn attempts
   * and eventually quarantine rather than looping forever.
   */
  | { kind: 'rejected'; reason: string }
  /** Network failure, timeout, 5xx. Transient; retry with backoff. */
  | { kind: 'unavailable'; reason: string };

/** Storage the sync engine needs. Implemented over SQLite, and in memory for tests. */
export interface OutboxStore {
  enqueue(items: OutboxItem[]): Promise<void>;
  /** Oldest first. FIFO bounds how stale the queue's head can get. */
  peek(limit: number): Promise<OutboxItem[]>;
  remove(ids: string[]): Promise<void>;
  /** Increment attempts and record why, for items that failed transiently. */
  recordFailure(ids: string[], error: string): Promise<void>;
  /** Move past-the-limit items out of the way so the queue can drain. */
  quarantine(ids: string[], reason: string): Promise<void>;
  count(): Promise<number>;
  deadCount(): Promise<number>;
  /** Enforce the size cap by dropping the oldest. Returns how many were dropped. */
  trimToCap(cap: number): Promise<number>;
}

/** Sends a batch. The transport owns HTTP; the engine owns policy. */
export interface Transport {
  send(deviceId: string, items: OutboxItem[]): Promise<SendOutcome>;
}

export interface SyncConfig {
  /** Rows per request. Bounded by the server's MaxReadingsPerBatch. */
  batchSize: number;
  /**
   * Attempts before an item is quarantined. Without a limit, one permanently
   * unacceptable row blocks every row behind it forever — the same
   * poison-message failure the server guards against, at the other end of the
   * wire.
   */
  maxAttempts: number;
  /** Queue size cap. Past this, the oldest rows are dropped and counted. */
  maxQueueSize: number;
  baseBackoffMs: number;
  maxBackoffMs: number;
}

export const DEFAULT_SYNC_CONFIG: SyncConfig = {
  batchSize: 100,
  maxAttempts: 5,
  // Roughly a week of position samples at a 10-second interval. Chosen to
  // outlast any plausible offline stretch while staying well inside the storage
  // an Android app may hold.
  maxQueueSize: 50_000,
  baseBackoffMs: 1_000,
  maxBackoffMs: 5 * 60_000,
};

/** Outcome of one drain pass, surfaced on the debug screen. */
export interface DrainResult {
  sent: number;
  accepted: number;
  /** Server named these as permanently unacceptable; dropped locally. */
  rejected: number;
  quarantined: number;
  /** Dropped to keep the queue under its cap. This is data loss — count it. */
  dropped: number;
  remaining: number;
  retryAfterMs: number | null;
  error: string | null;
}

export const EMPTY_DRAIN: DrainResult = {
  sent: 0,
  accepted: 0,
  rejected: 0,
  quarantined: 0,
  dropped: 0,
  remaining: 0,
  retryAfterMs: null,
  error: null,
};
