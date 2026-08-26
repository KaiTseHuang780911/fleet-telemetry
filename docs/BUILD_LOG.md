# Build log

A few lines per session: what was delegated to Claude Code, what it got wrong, and
what had to be corrected by hand. This is the raw material for the "how do you work
with agentic AI tools" interview question — without it, in three weeks the honest
answer is "it went well" and nothing more specific.

Newest entries at the top.

---

## 2026-08-25 — Phase 1: schema and migrations

**Delegated:** Postgres schema design, index strategy, and migration tooling.

**What it got right:** flagged the goose dependency problem unprompted. `go get -tool
github.com/pressly/goose/v3/cmd/goose` pulled **60+ indirect modules** — ClickHouse, MySQL,
MSSQL, Vertica, YDB, Turso, grpc, protobuf, OpenTelemetry — because `cmd/goose` imports
every dialect it supports. Using goose as a *library* with only pgx registered brings it to
**2 direct and 8 indirect**. Worth remembering as a general lesson: a tool's CLI entrypoint
and its library have very different dependency footprints, and `go get -tool` gives you the
CLI's.

**What it got wrong:**
- Recommended goose without checking its dependency tree first, then had to walk it back.
  The measurement should have come before the `go get`.
- Initially wrote the migration referencing an ADR number that did not match the plan's
  numbering. Renumbered: 001 is schema and indexing, 002 is reconciliation, ingestion moves
  to 003.

**Corrected by hand:** chose hybrid client+server trip/stop detection over server-only,
against the recommendation — the reconciliation delta between the two sources is the number
that makes the battery-versus-fidelity story concrete rather than anecdotal. Schema now
carries a `source` column and a `stop_event_matches` table.

**Verified, not assumed:** BRIN index confirmed as a real `brin` access method via
`pg_am`, not just present by name. Down-migration round-tripped to zero tables and back to
five. Both `fleet` and `fleet_test` migrated.

**Still unverified:** graceful shutdown on SIGTERM. Windows `Stop-Process -Force` does not
deliver a signal, so the drain path has never actually run. Needs a real test in Phase 1
once the batch writer has something to drain.

---

## 2026-08-24 — Phase 0: environment and repo foundation

**Delegated:** survey the machine's toolchain, plan Phase 0 + Phase 1, then set up the
monorepo.

**What it got right:** caught that the project folder name contained spaces before any
Gradle tooling existed to trip over it. Correctly identified that BRIN on `recorded_at`
would degrade precisely when the offline queue drains a backlog — the index has to go on
`received_at`, which actually correlates with physical row order.

**What it got wrong:**
- Claimed `api/internal/model` would be importable from `sim/`. It would not — Go's
  `internal/` rule scopes a package to the subtree rooted at `internal/`'s parent. Shared
  wire types moved to a root-level `internal/wire/`.
- Estimated the stale AVD cleanup would reclaim 30.9 GB; actual was 24.8 GB, because
  emulator images are sparse files and logical size overstates on-disk allocation.
- Recommended Docker Desktop for local Postgres without first checking whether WSL2 was
  installed. It was not, which changes the cost of that choice substantially.

**Corrected by hand:** storage layout. C: is a 250 GB SSD with limited headroom, E: is a
1 TB spinning disk. Split decided deliberately — hot small-file I/O (Gradle caches) on the
SSD, bulk cold storage (Android SDK, system images) on the HDD. Go module and build caches
redirected to `E:\Claude\.gocache`.

**Environment notes:** reclaimed 24.8 GB by deleting Xamarin-era AVDs (API 23, last touched
2018–2022) that no modern Expo build could have used anyway.
