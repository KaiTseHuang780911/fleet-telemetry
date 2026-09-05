/**
 * SQLite-backed outbox. This is the durability the queue's guarantees rest on.
 *
 * Uses the async expo-sqlite API (SDK 57): `openDatabaseAsync`, `runAsync`,
 * `getAllAsync`, `withExclusiveTransactionAsync`. The older callback-based
 * `transaction()` API is gone.
 */

import * as SQLite from 'expo-sqlite';

import type { OutboxItem, OutboxStore } from './types';

const DB_NAME = 'fleet-outbox.db';

/**
 * Schema version, tracked in SQLite's own `user_version` pragma.
 *
 * A hand-rolled migration counter rather than a migration library: there is one
 * table and the app is the only writer. If this grows past a handful of steps,
 * that is the moment to reach for something with rollback support.
 */
const SCHEMA_VERSION = 1;

interface OutboxRow {
  id: string;
  kind: string;
  recorded_at: string;
  payload: string;
  attempts: number;
  last_error: string | null;
  created_at: string;
}

export class SqliteOutbox implements OutboxStore {
  private constructor(private readonly db: SQLite.SQLiteDatabase) {}

  static async open(name: string = DB_NAME): Promise<SqliteOutbox> {
    const db = await SQLite.openDatabaseAsync(name);

    // WAL lets a read proceed while a write is in flight. The app records
    // positions on a timer while a drain may be reading the queue, and the
    // default rollback journal would make those two block each other.
    //
    // foreign_keys is off by default in SQLite and has to be asked for
    // per-connection, which is a common silent source of orphaned rows.
    await db.execAsync('PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON;');

    await SqliteOutbox.migrate(db);
    return new SqliteOutbox(db);
  }

  private static async migrate(db: SQLite.SQLiteDatabase): Promise<void> {
    const row = await db.getFirstAsync<{ user_version: number }>('PRAGMA user_version');
    const current = row?.user_version ?? 0;
    if (current >= SCHEMA_VERSION) return;

    await db.withExclusiveTransactionAsync(async (txn) => {
      if (current < 1) {
        await txn.execAsync(`
          CREATE TABLE IF NOT EXISTS outbox (
            id          TEXT PRIMARY KEY NOT NULL,
            kind        TEXT NOT NULL,
            recorded_at TEXT NOT NULL,
            payload     TEXT NOT NULL,
            attempts    INTEGER NOT NULL DEFAULT 0,
            last_error  TEXT,
            created_at  TEXT NOT NULL
          );

          -- Every read is "oldest first", so this index is the queue's whole
          -- access pattern rather than an optimisation.
          CREATE INDEX IF NOT EXISTS outbox_created_idx ON outbox (created_at, id);

          -- Small key/value table for things that must persist but are not
          -- queue items: the device id, chiefly. Kept in the same database so
          -- there is one file to reason about and one transaction boundary.
          CREATE TABLE IF NOT EXISTS settings (
            key   TEXT PRIMARY KEY NOT NULL,
            value TEXT NOT NULL
          );

          -- Quarantined items. Kept rather than deleted so a device that keeps
          -- failing can be inspected instead of silently losing the data.
          CREATE TABLE IF NOT EXISTS outbox_dead (
            id            TEXT PRIMARY KEY NOT NULL,
            kind          TEXT NOT NULL,
            recorded_at   TEXT NOT NULL,
            payload       TEXT NOT NULL,
            attempts      INTEGER NOT NULL,
            reason        TEXT NOT NULL,
            created_at    TEXT NOT NULL,
            quarantined_at TEXT NOT NULL
          );
        `);
      }
      await txn.execAsync(`PRAGMA user_version = ${SCHEMA_VERSION}`);
    });
  }

  async enqueue(items: OutboxItem[]): Promise<void> {
    if (items.length === 0) return;

    await this.db.withExclusiveTransactionAsync(async (txn) => {
      for (const item of items) {
        // INSERT OR IGNORE, not REPLACE. Re-enqueueing an id already present
        // must not reset its attempt count, or an item that keeps being
        // re-recorded could never reach the quarantine threshold.
        await txn.runAsync(
          `INSERT OR IGNORE INTO outbox
             (id, kind, recorded_at, payload, attempts, last_error, created_at)
           VALUES (?, ?, ?, ?, ?, ?, ?)`,
          [
            item.id,
            item.kind,
            item.recordedAt,
            JSON.stringify(item.payload),
            item.attempts,
            item.lastError,
            item.createdAt,
          ],
        );
      }
    });
  }

  async peek(limit: number): Promise<OutboxItem[]> {
    const rows = await this.db.getAllAsync<OutboxRow>(
      `SELECT id, kind, recorded_at, payload, attempts, last_error, created_at
         FROM outbox
        ORDER BY created_at, id
        LIMIT ?`,
      [limit],
    );
    return rows.map(toItem);
  }

  async remove(ids: string[]): Promise<void> {
    if (ids.length === 0) return;
    await this.db.runAsync(
      `DELETE FROM outbox WHERE id IN (${placeholders(ids.length)})`,
      ids,
    );
  }

  async recordFailure(ids: string[], error: string): Promise<void> {
    if (ids.length === 0) return;
    await this.db.runAsync(
      `UPDATE outbox
          SET attempts = attempts + 1, last_error = ?
        WHERE id IN (${placeholders(ids.length)})`,
      [error, ...ids],
    );
  }

  async quarantine(ids: string[], reason: string): Promise<void> {
    if (ids.length === 0) return;

    // Copy then delete, in one transaction. Doing it in two statements outside
    // a transaction risks a crash between them, which would either lose the
    // item or leave it in both tables.
    await this.db.withExclusiveTransactionAsync(async (txn) => {
      await txn.runAsync(
        `INSERT OR REPLACE INTO outbox_dead
           (id, kind, recorded_at, payload, attempts, reason, created_at, quarantined_at)
         SELECT id, kind, recorded_at, payload, attempts, ?, created_at, ?
           FROM outbox
          WHERE id IN (${placeholders(ids.length)})`,
        [reason, new Date().toISOString(), ...ids],
      );
      await txn.runAsync(
        `DELETE FROM outbox WHERE id IN (${placeholders(ids.length)})`,
        ids,
      );
    });
  }

  async count(): Promise<number> {
    const row = await this.db.getFirstAsync<{ n: number }>('SELECT count(*) AS n FROM outbox');
    return row?.n ?? 0;
  }

  async deadCount(): Promise<number> {
    const row = await this.db.getFirstAsync<{ n: number }>('SELECT count(*) AS n FROM outbox_dead');
    return row?.n ?? 0;
  }

  async trimToCap(cap: number): Promise<number> {
    const total = await this.count();
    const excess = total - cap;
    if (excess <= 0) return 0;

    // Delete by subquery rather than reading ids into JS first: it is one
    // statement, and it cannot race an insert happening between the read and
    // the delete.
    const result = await this.db.runAsync(
      `DELETE FROM outbox WHERE id IN (
         SELECT id FROM outbox ORDER BY created_at, id LIMIT ?
       )`,
      [excess],
    );
    return result.changes;
  }

  async getSetting(key: string): Promise<string | null> {
    const row = await this.db.getFirstAsync<{ value: string }>(
      'SELECT value FROM settings WHERE key = ?',
      [key],
    );
    return row?.value ?? null;
  }

  async setSetting(key: string, value: string): Promise<void> {
    await this.db.runAsync(
      'INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value',
      [key, value],
    );
  }

  /** Quarantined items, newest first, for the debug screen. */
  async deadItems(limit = 20): Promise<Array<{ id: string; reason: string; attempts: number }>> {
    return this.db.getAllAsync<{ id: string; reason: string; attempts: number }>(
      'SELECT id, reason, attempts FROM outbox_dead ORDER BY quarantined_at DESC LIMIT ?',
      [limit],
    );
  }

  /** Test and debug support. */
  async clear(): Promise<void> {
    await this.db.execAsync('DELETE FROM outbox; DELETE FROM outbox_dead;');
  }

  async close(): Promise<void> {
    await this.db.closeAsync();
  }
}

function toItem(row: OutboxRow): OutboxItem {
  return {
    id: row.id,
    kind: row.kind as OutboxItem['kind'],
    recordedAt: row.recorded_at,
    payload: JSON.parse(row.payload),
    attempts: row.attempts,
    lastError: row.last_error,
    createdAt: row.created_at,
  };
}

/**
 * Builds `?, ?, ?` for an IN clause.
 *
 * Batch sizes here are in the hundreds, well under SQLite's variable limit
 * (32766 on modern builds, 999 on older ones). The batch size cap in
 * SyncConfig is what keeps that true.
 */
function placeholders(n: number): string {
  return Array.from({ length: n }, () => '?').join(', ');
}
