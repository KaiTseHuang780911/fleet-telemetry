/**
 * How the outbox opens its connection.
 *
 * Narrow on purpose: the rest of SqliteOutbox needs a real device, but the
 * choice made here is not about SQL at all. expo-sqlite caches one native
 * connection per database name and hands the same one to every caller, so
 * "open a store, use it, close it" is only safe if the connection is actually
 * yours.
 *
 * It was not. The background location task opened a store per fix and closed
 * it in a `finally`, believing the handle was its own. It was the UI's. Closing
 * it released the native database under the running screen, and every call
 * after that failed with "NativeDatabase.prepareAsync has been rejected ->
 * NullPointerException" until the app was restarted.
 */

import * as SQLite from 'expo-sqlite';

import { SqliteOutbox } from './sqlite';

jest.mock('expo-sqlite', () => ({ openDatabaseAsync: jest.fn() }));

const openDatabaseAsync = SQLite.openDatabaseAsync as jest.MockedFunction<
  typeof SQLite.openDatabaseAsync
>;

beforeEach(() => {
  openDatabaseAsync.mockReset();
  openDatabaseAsync.mockImplementation(async () => {
    const db = {
      execAsync: jest.fn(async () => undefined),
      // Report the schema as current so open() skips migration entirely; this
      // test is about the connection, not the DDL.
      getFirstAsync: jest.fn(async () => ({ user_version: 999 })),
      closeAsync: jest.fn(async () => undefined),
    };
    return db as unknown as SQLite.SQLiteDatabase;
  });
});

function optionsFromLastOpen(): SQLite.SQLiteOpenOptions {
  const call = openDatabaseAsync.mock.calls[0];
  if (!call) throw new Error('openDatabaseAsync was never called');
  return (call[1] ?? {}) as SQLite.SQLiteOpenOptions;
}

describe('SqliteOutbox.open', () => {
  // The regression. A caller that will close its store must not be handed the
  // connection somebody else is still using.
  it('asks for an independent connection when opened isolated', async () => {
    await SqliteOutbox.open('test.db', { isolated: true });

    expect(optionsFromLastOpen().useNewConnection).toBe(true);
  });

  // The UI deliberately shares the cached connection: it is the long-lived
  // owner, and a second connection would only add a writer to contend with.
  //
  // Asserted as falsy rather than exactly `false`, because expo-sqlite treats
  // absent and false identically. Pinning the literal would fail a refactor
  // that changed nothing a caller can observe.
  it('shares the cached connection by default', async () => {
    await SqliteOutbox.open('test.db');

    expect(optionsFromLastOpen().useNewConnection).toBeFalsy();
  });

  it('shares the cached connection when isolation is explicitly declined', async () => {
    await SqliteOutbox.open('test.db', { isolated: false });

    expect(optionsFromLastOpen().useNewConnection).toBeFalsy();
  });

  // Closing an isolated store must not be mistaken for closing the shared one.
  // Both are the same class; only the connection differs.
  it('closes only its own handle, and does so once', async () => {
    const store = await SqliteOutbox.open('test.db', { isolated: true });
    const db = await openDatabaseAsync.mock.results[0]?.value;

    await store.close();
    await store.close();

    expect(db.closeAsync).toHaveBeenCalledTimes(1);
  });
});
