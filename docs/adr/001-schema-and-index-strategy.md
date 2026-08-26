# ADR-001: Telemetry schema and index strategy

- **Status:** accepted
- **Date:** 2026-08-25

## Context

`positions` is the only high-volume table in the system: every vehicle emits a reading
every few seconds, and readings arrive in batches, sometimes hours late when a device
drains an offline backlog. Every other table is small by comparison.

Two properties have to hold from the start, because retrofitting either is expensive:

1. **Ingestion must be idempotent.** The mobile client keeps a durable SQLite queue and
   retries on timeout, so the server will receive the same batch more than once. That is
   designed behaviour, not an edge case.
2. **Time-range queries per vehicle must stay fast** as the table grows — that is what
   both the dashboard and the eventual NL query layer are made of.

## Decision

**Client-generated UUIDv7 as `positions.reading_id`, and the primary key.**
Idempotency becomes `ON CONFLICT (reading_id) DO NOTHING` — replay costs a conflict check
and nothing more. v7 is time-ordered, so inserts land at the right-hand edge of the index
rather than scattering across it as UUIDv4 would. There is no column default: a reading
without a client-supplied id is a bug and should fail loudly rather than get a fresh id
and silently duplicate on retry.

**Two clocks, stored separately.** `recorded_at` is the device clock and is not trusted —
phones drift, and an offline backlog reports hours-old times. `received_at` is ours. Trip
and stop boundaries are never derived from `recorded_at` alone.

**Indexes on `positions`:**

- `(vehicle_id, recorded_at DESC)` B-tree — the workhorse. Serves per-vehicle time-range
  queries and, via `DISTINCT ON (vehicle_id)`, the live map's "latest position per vehicle".
- `BRIN (received_at)` — **not** `recorded_at`.

**Plain table, not partitioned.** A few million rows does not need it.

**`double precision` lat/lon, not PostGIS.** Haversine covers arrival detection and idle
time.

**`text` + `CHECK` instead of Postgres enums**, so adding a value is a one-line migration.

## Alternatives considered

**BRIN on `recorded_at`**, as originally sketched. Rejected. BRIN stores min/max per block
range, so it only helps when physical row order correlates with the indexed column. Rows
are written in `received_at` order, so that column correlates. `recorded_at` does not: the
offline queue is *designed* to drain hours of backlogged readings in one batch, dropping
old timestamps into freshly written pages. The index would degrade precisely when the
offline feature works as intended.

**Monthly range partitioning from day one.** Rejected for now. It makes retention a
`DROP PARTITION` instead of a slow `DELETE`, and it is where BRIN genuinely shines — but
it complicates every migration and needs a job to pre-create future partitions. That is
real operational burden for a side project.

**Server-generated `bigserial` primary key.** Rejected: idempotency would then need a
separate unique constraint on a client-supplied key anyway, so the surrogate key buys
nothing but an extra column.

**PostGIS.** Rejected for now — a large dependency for distance maths that is a dozen
lines of Go.

## Consequences

Replay is free and provably safe, which is what makes the offline queue tractable in
Phase 2. Per-vehicle time-range queries are served by one index.

The costs: UUID primary keys are 16 bytes against 8 for a bigint, which is a real storage
and index-size difference at scale. The `vehicles` foreign key on `positions` costs a
lookup per inserted row — negligible at this write volume, and worth revisiting if the
batch writer ever becomes the bottleneck. The BRIN index is close to free but is also not
doing much work at current row counts; it is there for retention sweeps and to make future
partitioning worthwhile, not because it makes anything fast today.

**Revisit when:** `positions` passes roughly 50M rows, or retention deletes start showing
up in slow-query logs. Both point at partitioning, which is the next ADR in this area.
