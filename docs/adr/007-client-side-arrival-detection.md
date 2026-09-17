# ADR-007: Client-side arrival detection, and reporting a stop twice

- **Status:** accepted
- **Date:** 2026-09-17

## Context

ADR-002 decided that stops come from two independent detectors — the device and
the server — reconciled through `stop_event_matches` so their disagreement is a
measurable number rather than an opinion. Phase 1 built the server half. **The
client half has never existed**, which is why `stop_event_matches` has never
held a row and why the most interesting output the system was designed to
produce has never been produced.

Two constraints shape the client detector, and neither applies to the server's.

**The process dies mid-stop.** Android delivers a handful of fixes at a time and
is free to kill the app between deliveries. A detector holding a window of fixes
in memory loses every stop that spans a death — which, over a shift, is most of
them.

**Arrival is worth more than departure, and arrives first.** "The driver is at
the customer now" is the event a fleet acts on. It is known the moment the dwell
threshold passes, and the departure may be an hour away.

The server's insert path did not support that second point. It was
`ON CONFLICT (id) DO NOTHING`, with a comment stating that arrival and departure
would be reported as *different events with different ids* — which would put two
rows in the table for one stop and leave consumers to pair them up.

## Decision

**An incremental detector.** `detectStops(state, fixes, nextEventId)` is pure:
it takes the previous state and a batch of fixes, and returns the next state
plus whatever should be reported. State is a small JSON object — anchor,
last-inside timestamp, open stop — persisted in the SQLite `settings` table
beside the queue, so a stop spanning a process death is still detected.

**A stop is reported twice under one `event_id`:** on arrival with
`departed_at` absent, and again on departure with it set. The server's insert
becomes a narrow upsert:

```sql
ON CONFLICT (id) DO UPDATE
   SET departed_at = EXCLUDED.departed_at
 WHERE stop_events.departed_at IS NULL
   AND EXCLUDED.departed_at IS NOT NULL
```

The `WHERE` clause is the load-bearing part. At-least-once delivery means the
*open* version can arrive after the completed one — a batch that failed, queued,
and drained after the departure had already been reported. Without the guard
that replay silently reopens a finished stop, and the vehicle never leaves.

**Thresholds:** stationary at ≤1 m/s, 50 m radius, 90 s minimum dwell, fixes
worse than 50 m accuracy discarded. The radius deliberately matches the server's
derivation — two detectors using different radii would manufacture disagreement
that says nothing about either one's quality. All four are guesses until
`stop_event_matches` has data to tune them against.

**The outbox row id is not the event id.** This is the opposite of positions,
where one id serves both, and it is forced: the queue deduplicates on row id
with `INSERT OR IGNORE`, so two reports sharing one id would see the departure
silently discarded and the stop left open forever. The transport therefore
matches the server's rejections in wire-id space and translates back to row ids.

**Detection runs after positions are queued**, and its failures are swallowed.
A bug in stop detection must be able to cost stops and never a position reading,
which is the record everything else derives from.

**Detector state is written after emissions are queued**, never before. A death
in between re-detects the stop under a new event id — a duplicate that
reconciliation can see. The other order loses the stop silently, which it
cannot.

## Alternatives considered

**Report once, on departure.** Simplest, one row per stop, and it needs no
server change. Rejected because a driver parked at a customer for forty minutes
generates nothing for forty minutes, which discards the most valuable property
of on-device detection — that it knows about the arrival immediately, offline.

**Report arrival and departure as two separate events with two ids**, as the
original server comment envisaged. Rejected: it doubles the rows, and pairing
them afterwards means re-deriving the very relationship the client already knew.

**`DO UPDATE` without the `WHERE` guard.** Rejected, and the test that proves
why fails loudly against it: a late replay of an open stop erases a recorded
departure.

**A batch detector over the whole fix history.** Simpler to write and to test.
Rejected: it cannot survive process death, which is the normal case rather than
the exception.

**Trusting `motion_state` instead of running a radius test.** Rejected — that
field is explicitly documented as a crude hint the server does not trust for
anything load-bearing, and a stop is exactly load-bearing.

## Consequences

**Two rows in the queue per stop**, and they must drain in order for the arrival
to land before the completion. FIFO ordering by `created_at` gives that, but the
guarded upsert is what makes the out-of-order case safe rather than the ordering
itself.

**The device clock decides stop boundaries**, which ADR-001 warns against.
That is answered by the architecture rather than by the detector: client
detections are stored as `source = 'client'` and reconciled against the server's
own derivation from received positions, so a skewed clock shows up as
disagreement in `stop_event_matches` — the one place where it is useful.

**Duplicate stops are possible** when the process dies between queueing an
arrival and writing detector state. Chosen deliberately over the alternative
failure, which is losing the stop with no trace.

**The thresholds are untuned**, so the first real data will probably show the
90 s dwell is wrong in one direction or the other. That is what the matching
pass is for; until a drive produces stops, every number here is a guess stated
in public.

**We would revisit this** if activity recognition (`expo-sensors`, or the
platform's own) became available to the detector — it would make the stationary
test far better than a speed threshold — or if the reconciliation data showed
the two detectors agreeing so closely that running both stopped being worth the
cost.
