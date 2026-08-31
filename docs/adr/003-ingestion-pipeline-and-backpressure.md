# ADR-003: Ingestion pipeline and backpressure

- **Status:** accepted
- **Date:** 2026-08-31

## Context

Devices post batches of position readings over HTTP. Writes are far more
frequent than reads, they arrive in bursts, and a device coming out of a signal
dead zone drains hours of backlog at once. Three things have to hold:

1. Writes must not be lost, because the device's queue is the only other copy.
2. The server must degrade predictably when writes arrive faster than Postgres
   accepts them, rather than falling over.
3. Every part of it has to be explainable line by line in an interview.

## Decision

**HTTP handler → bounded channel → single batch-writer goroutine.**

The handler validates a batch, resolves the device to a vehicle, and offers the
readings to a buffered channel with a non-blocking send. The writer goroutine
accumulates readings until either `INGEST_BATCH_SIZE` rows or `INGEST_FLUSH_MS`
elapse, then writes them in one statement.

**When the buffer is full, shed load: `503` with `Retry-After`.**

**Writes go in as a single `INSERT ... SELECT FROM unnest(...) ON CONFLICT DO NOTHING`.**
Passing eleven arrays and expanding them server-side keeps the parameter count
constant regardless of batch size. A `VALUES` list would hit Postgres' 65535
parameter ceiling at about 5900 rows and would produce a differently shaped
query for every batch length, preventing prepared-statement reuse.

**Malformed readings are rejected individually, not by failing the batch.**
The response carries `accepted` and a `rejected` array naming each bad reading
and why.

**Shutdown stops HTTP first, then drains the writer**, each flush using a
context independent of any request context.

## Alternatives considered

**A message broker — Kafka, NATS, Redis Streams.** Rejected. There is one
producer type and one consumer, no replay requirement, and no second consumer.
A broker in a project this size is resume-driven development, and it would move
the interesting decision out of code I can explain and into someone else's
defaults. *What would change the answer:* a second independent consumer (the
Phase 5 query layer wanting a live feed), a replay requirement, or ingest
outliving the database's write capacity.

**Blocking when the buffer is full.** Rejected. It ties up an HTTP goroutine per
stalled request; under sustained load that cascades into connection exhaustion
and the server stops answering health checks too — a local slowdown becomes a
total outage.

**Dropping the oldest or newest buffered readings.** Rejected, and this is the
important one: the client has already been told `202`. Dropping after
acknowledging loses data *silently*, and silent loss is the worst possible
failure mode for a telemetry system, because nothing downstream can tell the
difference between "the vehicle did not move" and "we threw the readings away".

**Spilling to local disk.** Rejected. It rebuilds a durable queue that already
exists on the device, and adds a second source of truth to reconcile.

**Failing the whole batch on one bad reading.** Rejected — this is a poison
message. The client's durable queue would retry that batch forever and never
drain again, permanently breaking offline sync for that device. One corrupt
reading would take out everything behind it.

**`COPY` via pgx's `CopyFrom`.** Faster, but `COPY` cannot express
`ON CONFLICT`, and idempotent replay is the foundation the offline queue rests
on. Doing it with `COPY` means staging into an unlogged table and then
`INSERT ... SELECT`, which is more machinery than the current write volume
justifies. *What would change the answer:* flush latency becoming the
bottleneck under real load.

## Consequences

Backpressure is pushed to the edge, which is where a durable buffer already
lives. Nothing is dropped after acknowledgement: a `503` means the device keeps
the batch and resends it. Verified under load — with a one-slot buffer and 40
concurrent vehicles, 258 readings were shed, the simulator backed off and
retried, and **zero duplicate rows** resulted.

The costs, stated plainly:

- **A crash loses whatever is buffered.** Readings sit in memory between `202`
  and the flush. This is bounded and deliberate — the device has not deleted its
  copy, and it resends what was never durably confirmed — but a `202` genuinely
  does not mean "stored". That is why it is `202` and not `200`.
- **Validation is duplicated** between `wire.Reading.Validate` and the table's
  CHECK constraints. That duplication is load-bearing, not sloppiness: a flush is
  one statement, so a single constraint violation would abort the insert for
  every other row in the batch.
- **Ordering is not guaranteed** across concurrent submissions. Nothing depends
  on it — readings carry their own timestamps — but it would matter if
  derivation were ever done streaming rather than by query.
- **One writer goroutine is a deliberate ceiling.** Throughput is bounded by one
  connection's insert rate. Sharding by vehicle would lift it, at the cost of
  ordering guarantees that are currently free.

**Revisit when:** flush latency becomes the bottleneck (→ `COPY` into a staging
table), a second consumer appears (→ reconsider the broker), or buffered-on-crash
loss becomes unacceptable (→ acknowledge only after the flush, trading latency
for durability).
