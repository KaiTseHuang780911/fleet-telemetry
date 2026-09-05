/**
 * In-memory outbox.
 *
 * Used by the sync engine's tests and by the debug screen when running without
 * a database. It implements the same contract as the SQLite store, so the
 * engine is exercised through exactly the interface it uses in production —
 * the only untested seam is SQL, which is what the storage tests cover.
 */

import type { OutboxItem, OutboxStore } from './types';

export class InMemoryOutbox implements OutboxStore {
  private items = new Map<string, OutboxItem>();
  private dead = new Map<string, { item: OutboxItem; reason: string }>();

  async enqueue(items: OutboxItem[]): Promise<void> {
    for (const item of items) {
      // Same semantics as the SQLite store's INSERT OR IGNORE: enqueueing an id
      // that is already queued must not reset its attempt count, or a retry
      // loop that re-enqueues could keep an item alive forever.
      if (!this.items.has(item.id)) {
        this.items.set(item.id, { ...item });
      }
    }
  }

  async peek(limit: number): Promise<OutboxItem[]> {
    return [...this.items.values()]
      .sort((a, b) => (a.createdAt < b.createdAt ? -1 : a.createdAt > b.createdAt ? 1 : 0))
      .slice(0, limit)
      .map((i) => ({ ...i }));
  }

  async remove(ids: string[]): Promise<void> {
    for (const id of ids) this.items.delete(id);
  }

  async recordFailure(ids: string[], error: string): Promise<void> {
    for (const id of ids) {
      const item = this.items.get(id);
      if (item) {
        item.attempts += 1;
        item.lastError = error;
      }
    }
  }

  async quarantine(ids: string[], reason: string): Promise<void> {
    for (const id of ids) {
      const item = this.items.get(id);
      if (item) {
        this.dead.set(id, { item, reason });
        this.items.delete(id);
      }
    }
  }

  async count(): Promise<number> {
    return this.items.size;
  }

  async deadCount(): Promise<number> {
    return this.dead.size;
  }

  async trimToCap(cap: number): Promise<number> {
    const excess = this.items.size - cap;
    if (excess <= 0) return 0;

    // Oldest first: recent telemetry is more useful than stale telemetry, so
    // the head of the queue is what gets sacrificed.
    const doomed = [...this.items.values()]
      .sort((a, b) => (a.createdAt < b.createdAt ? -1 : a.createdAt > b.createdAt ? 1 : 0))
      .slice(0, excess);

    for (const item of doomed) this.items.delete(item.id);
    return doomed.length;
  }

  /** Test helper. Not part of OutboxStore. */
  deadItems(): Array<{ item: OutboxItem; reason: string }> {
    return [...this.dead.values()];
  }
}
