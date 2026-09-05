# mobile/

React Native + TypeScript driver app (Expo SDK 57, Android target).

> **Slice 1 of Phase 2.** The offline sync queue and a debug screen to observe it.
> Background location, arrival detection, battery-aware sampling, and the route UI
> are later slices.

## What exists

The **durable outbox** — the piece everything else in the app depends on.

```
capture → SQLite outbox → drain → POST /v1/telemetry
                            ↑ delete only on confirmed 202
```

The design claim it rests on: **at-least-once delivery plus an idempotent server is
effectively-once.** Rows are removed only after the server confirms them, so a process
killed mid-request resends and the server's `ON CONFLICT (reading_id) DO NOTHING`
absorbs the duplicate. This is why Phase 1 put a client-generated UUIDv7 on the
server's primary key.

It also means the queue holds **no in-flight state** — and it is exactly that state,
written to disk between "sending" and "sent", that loses data when Android kills an app
at the wrong moment.

| Concern | How it's handled |
|---|---|
| Server rejects an item | Deleted, not retried — it will never be accepted, and keeping it blocks the queue |
| Server sheds load (`503`) | Backoff honouring `Retry-After`, **without** counting against attempts |
| Network failure | Exponential backoff with full jitter |
| Item fails repeatedly | Quarantined to `outbox_dead` after N attempts, so the queue drains |
| Queue grows unbounded | Capped; oldest dropped, and the loss is counted and surfaced |
| Response omits an item | Kept, not assumed sent |

## Layout

```
src/queue/policy.ts    pure decisions: backoff, quarantine, response interpretation
src/queue/sync.ts      the engine: orchestrates store, transport, policy
src/queue/sqlite.ts    durable store (expo-sqlite)
src/queue/memory.ts    same contract, in memory, for tests
src/api/transport.ts   HTTP; owns status codes and nothing else
src/telemetry/         sample -> outbox item, where the UUIDv7 is minted
App.tsx                debug screen
```

The split is deliberate: everything likely to be *wrong* — backoff, quarantine, what a
response means — is pure and runs as a fast unit test with no device involved.

## Running it

```bash
cp .env.example .env     # set EXPO_PUBLIC_API_URL to your machine's LAN address
npm install
npm start                # then press 'a', or scan with a dev build
```

`localhost` will not work from a phone — it resolves to the phone. See `.env.example`.

```bash
npm test            # unit tests, no device needed
npm run typecheck   # tsc --noEmit, strict + noUncheckedIndexedAccess
```

## Verified vs not

**Verified:** 50 unit tests covering the policy layer, the sync engine against an
in-memory store, and the HTTP transport. The app bundles for Android (650 modules).

**Not yet verified:** `SqliteOutbox` has no test coverage. The engine is tested through
the same `OutboxStore` interface the SQLite store implements, so the *contract* is
covered — but the SQL itself has never run. That needs a device or emulator, and it is
the first thing to check when one is available.
