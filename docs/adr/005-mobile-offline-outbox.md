# ADR-005: The mobile offline outbox and its delivery guarantees

- **Status:** accepted
- **Date:** 2026-09-17

## Context

A driver's phone loses coverage. That is not an error case in this system, it is the
normal operating condition: underground loading bays, rural routes, lifts, tunnels, and
the ordinary dead spots of any city. A telemetry app that only reports while connected
reports nothing about exactly the parts of a shift that are hardest to account for.

So the device has to keep recording while offline and upload later, which makes the queue
the component every guarantee in the mobile app rests on. Three specific pressures shape
it:

- **The process dies without warning.** Android kills backgrounded apps, and the user
  force-stops them. Anything held only in memory between "recorded" and "confirmed stored"
  is lost.
- **The same batch will be sent twice.** A request that times out after the server
  committed it is indistinguishable, from the client, from one that never arrived.
- **An outage has no upper bound.** "Offline for six hours" and "offline for a week" are
  both real, and they need different answers.

An early version of this queue got the second and third of those wrong in ways that only
showed up on a device, which is why they are stated explicitly here.

## Decision

**At-least-once delivery from the client, idempotency on the server, which together give
effectively-once without either side tracking in-flight state.**

Rows are deleted from the outbox only after the server confirms them. A process killed
mid-request therefore resends. Every reading carries a client-generated UUIDv7 that is the
server's primary key, so the resend collides and `ON CONFLICT DO NOTHING` absorbs it.

The thing this deliberately does *not* do is write "sending" or "sent" state to disk
between the request and the response. That intermediate state is precisely what loses data
when the process is killed at the wrong moment — the row is marked in flight, nothing ever
confirms it, and it is neither retried nor delivered.

**Failures are classified by whether retrying can possibly help, and only one class counts
against the attempt limit.**

| Outcome | Meaning | Burns an attempt? |
|---|---|---|
| `accepted` | Stored, or named as permanently unacceptable | n/a — removed |
| `rejected` | The request was malformed; a bug on this side | **Yes** |
| `shed` | Server is applying backpressure | No |
| `unavailable` | Network failure, timeout, 5xx | No |

Only `rejected` quarantines, because retrying identical bytes the server has called
malformed cannot succeed, and one such row would otherwise block every row behind it
forever — the poison-message failure, at the client end of the wire.

`unavailable` and `shed` are environmental and resolve on their own. Counting them would
punish a reading for the crime of having been recorded during an outage.

**Queue growth is bounded by size, not by attempts.** Past `maxQueueSize` (50,000 rows,
roughly a week at a 10-second interval) the oldest rows are dropped and the count is
surfaced as data loss rather than swallowed. For "offline for a week", losing the stalest
data loudly is the right failure; quarantining the freshest data quietly is not.

**Backoff uses full jitter** — a uniform draw across the whole interval, not the interval
plus a wobble. Every device in a fleet is driven by the same server, so a shared outage
releases them all at once; without jitter they retry in lockstep and recreate the overload
that caused the failure.

**Drains are triggered, not merely schedulable.** Reconnect, foreground, and a periodic
sweep each consult the scheduling rules. This is stated as a decision because the rules
existed, fully tested, for a week before anything called them.

## Alternatives considered

**Count `unavailable` against the attempt limit, like any other failure.** This is what the
first version did, and it is wrong in a way that looks reasonable in code review. With a
drain every ten seconds, five attempts elapse in under a minute, so the oldest readings
were quarantined roughly fifty seconds into any outage — and quarantined data never
uploads, even once signal returns. A thirty-minute walk out of coverage would have
dead-lettered the entire walk. Three existing tests had encoded this behaviour as a
requirement and had to be changed with the fix.

**At-most-once: delete on send.** Removes the duplicate problem entirely and loses data on
every timeout. Rejected — for telemetry, a duplicate is free and a gap is permanent.

**Track in-flight state on disk.** The obvious way to avoid resending. Rejected because the
window it protects is smaller than the window it creates: a process killed after marking a
row in flight leaves that row in limbo, which is worse than sending it twice to a server
that deduplicates.

**Server-generated ids with a client correlation token.** Rejected: the client cannot then
deduplicate its own retries, and the id has to exist before the first send anyway for the
offline case to work at all.

**A general-purpose sync library.** Rejected under the "boring and explainable" rule. The
queue is roughly 300 lines, and every guarantee above has to survive being questioned in an
interview — which is easier when the code is readable in one sitting.

## Consequences

The server must stay idempotent forever. `ON CONFLICT DO NOTHING` on `reading_id` is not an
optimisation, it is load-bearing: remove it and at-least-once delivery becomes duplicate
rows. That coupling is why this ADR and ADR-001 have to be read together.

Duplicates are normal traffic, not an incident. The `/readyz` duplicate counter is expected
to be non-zero, and a device that reconnects mid-request will produce them. `duplicates: 2`
was observed on the first successful device run and is correct behaviour, not a defect.

Quarantined rows accumulate in `outbox_dead` and nothing currently drains or reports them.
They are kept rather than deleted so a failing device can be inspected, but a real fleet
needs them surfaced.

The size cap is a guess. 50,000 rows was chosen to outlast any plausible offline stretch
while staying well inside what an Android app may hold; it has never been reached in
testing, so the drop path is the least-exercised branch in the queue.

**We would revisit this if** the server ever needed to accept non-idempotent operations
from the device, or if stop events grew payloads large enough that a week of them stopped
fitting under a row-count cap — at which point the bound should become bytes, not rows.
