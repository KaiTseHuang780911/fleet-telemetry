# ADR-004: Trip and stop derivation, and reconciliation

- **Status:** accepted
- **Date:** 2026-09-01
- **Implements:** the server-side half of [ADR-002](002-hybrid-trip-and-stop-reconciliation.md)

## Context

ADR-002 decided to store both device-reported and server-derived stops and to
measure their disagreement. This records how the server half actually works.

The input is a stream of positions carrying a device timestamp. The output is
trips, stop events, and a matching between the two sources.

## Decision

**Derivation recomputes a time window; it does not stream.**

This is the constraint everything else follows from. A device leaving a signal
dead zone delivers hours of backlogged readings in one batch, so a detector
reacting to arrivals would already have closed trips covering that period and
then be handed the evidence afterwards. Recomputation also makes retries free
and the whole thing testable, but correctness is the reason, not convenience.

Positions are read ordered by `recorded_at`, not `received_at`. Derivation
reconstructs what the vehicle did, and that happened in device-clock order.

**Stop detection: distance clustering with a fixed anchor.** Consecutive samples
within `StopRadiusM` of the cluster's *first* sample form a candidate stop; it
counts if it lasts at least `StopMinDuration`.

The anchor is fixed rather than a running centroid, and that matters. A centroid
drifts — each new sample tugs it — so a vehicle creeping forward at walking pace
stays "within radius" indefinitely and reads as parked. A fixed anchor cannot
drift, so a crawl eventually leaves the radius and correctly reads as movement.

**Trips are what is left over.** Samples not inside a stop form trips, split
further wherever the gap between consecutive samples exceeds
`TripGapDuration` — the device was off or out of coverage, and interpolating
across the gap would invent distance the vehicle never travelled.

**A stop links to the trip that arrived at it**: the latest trip ending at or
before the stop began.

**Reconciliation is greedy on closeness.** Every client/derived pair within both
tolerances is scored on normalised time and distance error, sorted, and taken in
order, skipping events already claimed. Distance is normalised by the ratio of
the two tolerances so metres do not dominate seconds purely by being bigger
numbers.

**Deltas are stored signed.** A consistent sign means one source systematically
fires earlier than the other, which is a tuning signal. Averaging absolute
values would hide that entirely, so both means are reported.

**Persistence replaces the window** in a transaction rather than upserting.

**Thresholds are environment-configurable**, defaulting to `StopRadiusM=50`,
`StopMinDuration=120s`, `TripGapDuration=10m`, `MatchMaxTimeDelta=120s`,
`MatchMaxDistance=100m`.

## Alternatives considered

**Streaming detection on ingest.** Rejected — see above. It is not a performance
trade-off; it produces wrong answers for exactly the devices the offline queue
exists to serve.

**Speed-threshold stop detection** (`speed < x` for `t` seconds). Rejected as
the primary signal: `speed_mps` is optional and nullable, and a stationary
vehicle with GPS drift can report several m/s. Position clustering uses data
that is always present. Speed remains available as a future corroborating
signal.

**Deterministic content-derived UUIDs plus upsert**, instead of replacing the
window. Rejected: derivation output depends on the thresholds it ran with, so
retuning shifts boundaries and changes how many events exist. Content-keyed
upserts would leave rows from the previous tuning stranded in the table with no
way to tell them apart.

**Optimal assignment (Hungarian) for matching.** Rejected for now. Stops are
sparse and well separated — a vehicle does not make two deliveries ninety
seconds apart — so greedy and optimal agree in practice, at a fraction of the
code and none of the explanation cost. *What would change the answer:* real data
showing dense clusters where greedy pairs badly.

**PostGIS.** Rejected: haversine is a dozen lines and precision beyond GPS
accuracy is false precision.

## Consequences

The reconciliation number exists. On a simulated fleet of 8 vehicles over an
hour: **72 client stops, 52 derived, 52 matched, 20 client-only, 0
derived-only**, mean signed delta **+5.9s**, mean distance **30.8m**.

Every one of those numbers is explainable, which is the point:

- **20 client-only** stops are the designed consequence of the device reporting
  after 45s while the server requires 120s. Short stops the device sees and the
  server declines to call.
- **0 derived-only** follows: any stop long enough for the server is long enough
  for the device.
- **The +5.9s bias is consistent**, not noise — every match fell between +3s and
  +6s. The device times arrival from the moment motion ceases; the server can
  only anchor on the first sample already at rest, which is up to one sampling
  interval later. This is precisely what a signed mean is for; an absolute mean
  would have shown the same magnitude for random disagreement.

Costs, stated plainly:

- **Window edges truncate.** A trip straddling the window's start is cut. The
  lookback default is generous to push that edge into settled history, but it is
  a real artefact, not a solved problem.
- **Thresholds are guesses.** They are not tuned against real driving, and the
  numbers above describe agreement with a simulator whose own stop model was
  written alongside them. Treat them as a working harness, not a measurement of
  reality.
- **Cost is linear in the window**, re-derived every pass. Fine at this scale;
  at fleet scale it wants incremental derivation keyed on which vehicles have
  new data.
- **Derivation is a scheduled job**, exposed as `api derive`. The in-process
  ticker is opt-in and development-only, since every replica running it would
  duplicate the work.

**Revisit when:** real traces exist to tune against; a vehicle's stops are dense
enough that greedy matching pairs badly; or the window re-derivation cost stops
being negligible.
