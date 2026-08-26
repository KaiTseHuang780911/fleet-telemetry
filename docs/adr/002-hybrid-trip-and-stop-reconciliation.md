# ADR-002: Hybrid trip and stop detection with reconciliation

- **Status:** accepted
- **Date:** 2026-08-25

## Context

Trips and stop events have to come from somewhere. Two sources are available and they know
different things.

The **device** has accelerometer and activity-recognition data, knows its own GPS accuracy
at each moment, and detects arrival locally — it has to, because arrival detection must
work offline. None of that context reaches the server.

The **server** sees the full position stream, including readings the device's own detector
may have discarded, and it can apply consistent thresholds across the whole fleet. It also
cannot be fooled by a buggy or tampered client.

Picking one source means throwing away what the other knows.

## Decision

Store both, and reconcile.

`trips` and `stop_events` each carry a `source` column constrained to `'client'` or
`'derived'`. The device reports its own detections; the server independently derives its
own from the position stream. Neither overwrites the other.

A `stop_event_matches` table records which client event corresponds to which derived
event, along with `delta_seconds` and `delta_meters` — how far apart the two sources were.
An event with no row in `stop_event_matches` is unmatched.

**The unmatched events are the point.** A client stop the server never derived means the
thresholds disagree; a derived stop the client missed means the on-device detector needs
tuning. Either way it is a measurable number rather than an opinion, which is the same
reason the Phase 5 eval suite exists.

Idempotency works as it does for positions: client-reported events carry client-generated
UUIDv7 ids, so replay is a no-op.

## Alternatives considered

**Client reports only.** Simplest, and the device genuinely knows more. Rejected because it
makes the server's data only as good as the weakest client build, and the simulator would
have to reimplement the on-device detector to produce any data at all in Phase 1.

**Server derives only.** Robust and testable in pure Go, and immune to client bugs.
Rejected because it discards the sensor context that makes on-device detection work
offline in the first place.

**Client reports, server silently corrects.** Rejected: it hides disagreement, which is the
most interesting signal the system produces.

## Consequences

Roughly double the detection work — two implementations plus a matching pass — and the
matching pass needs its own tuning (how close in time and space before two events are "the
same stop"?). Row counts for stops roughly double.

In exchange, the system can answer "how often does on-device arrival detection disagree
with the server, and by how much" with a number. That question is the honest version of the
battery-versus-fidelity tradeoff this project exists to demonstrate.

Queries that just want "the stops" must filter by `source` or they will double-count. That
is a real footgun; the read API should expose a single default source rather than leaking
the choice to every caller.

**Sequencing:** Phase 1 ships the schema and client-reported ingestion. Server-side
derivation and the matching pass follow within Phase 1, once the position stream exists to
derive from.

**Revisit when:** the two sources agree within tolerance often enough that reconciliation
stops producing signal — at which point deriving server-side becomes a validation job that
can run on a sample rather than on every event.
