# Build log

A few lines per session: what was delegated to Claude Code, what it got wrong, and
what had to be corrected by hand. This is the raw material for the "how do you work
with agentic AI tools" interview question — without it, in three weeks the honest
answer is "it went well" and nothing more specific.

Newest entries at the top.

---

## 2026-08-31 (later) — CI, and the two gaps it closed

**Delegated:** a GitHub Actions workflow to verify what a Windows machine cannot.

**Why it exists:** `-race` needs cgo, and Windows has no SIGTERM. Both were listed as
unverified for two sessions. Linux CI is not a workaround — it is the platform this
service actually deploys to, so it is the correct place to prove them.

**Result:** `api/cmd/api` passes in 2.8s under `-race` on Linux. The shutdown test starts
the built binary with flush thresholds high enough that nothing can flush on its own,
posts readings, asserts they are *still only in memory*, sends the signal, then checks
they reached Postgres. That middle assertion is what stops the test passing for the wrong
reason.

**What it got wrong — chasing a prebuilt linter:** golangci-lint failed with "the Go
language version (go1.24) used to build golangci-lint is lower than the targeted Go
version (1.26)". First reaction was to lower the `go` directive to 1.24; `go mod tidy`
immediately overrode that to 1.25.7, because **goose v3.27.3 requires 1.25.7** and the
directive must satisfy the highest any dependency demands. So the directive was never
mine to choose, and bumping the action version would only have deferred the same failure
to the next dependency bump.

The general rule worth keeping: **a Go analysis tool refuses to run against a module whose
`go` directive is newer than the Go the tool was built with.** Any prebuilt analyser is
therefore fragile against dependency bumps. Compiling staticcheck from source with the
repo's own toolchain (`go run honnef.co/go/tools/cmd/staticcheck@latest`) makes the
analyser always at least as new as what it analyses, and does not touch go.mod.

**What that immediately caught — a real security issue, mine:** chi's `middleware.RealIP`
was added reflexively in the router. It is deprecated and vulnerable to IP spoofing
(GHSA-3fxj-6jh8-hvhx): it rewrites `RemoteAddr` from `X-Forwarded-For` whether or not
anything upstream sets that header, so any client can forge its apparent source address.
Nothing in this service reads the client IP, so it was pure attack surface for zero
benefit. Removed, with a note on what reintroducing it safely would require.

Worth sitting with: that shipped through a review, a full test suite, and `go vet` without
being noticed. It took a tool whose whole job is knowing the ecosystem's deprecations.

---

## 2026-08-31 — Phase 1: ingestion pipeline, simulator, tests

**Delegated:** the whole ingestion path — wire types, handler, batch writer, store, and a
deterministic simulator.

**What it got wrong — and this one is embarrassing:** put the `.env` loader in
`api/internal/config`, then imported it from `sim/`. That is the *exact* Go `internal/`
scoping rule documented in the README two sessions earlier, violated while the explanation
was still sitting in the same repo. Build failed with "use of internal package not
allowed". Moved to root-level `internal/config`, matching `internal/wire`.

**What it got wrong — a bad test:** the first end-to-end backpressure test showed zero
503s and was nearly reported as "shedding verified". It was not. With `INGEST_BATCH_SIZE`
set to 10000 and a 60s flush interval, the writer never flushed, so it drained the channel
into memory faster than a single-threaded client could fill it — the buffer never filled,
so shedding never engaged. The config made the test meaningless rather than the code
wrong. Re-run with 40 concurrent vehicles and a one-slot buffer, shedding engaged properly:
258 readings shed, simulator backed off, **zero duplicates**.

Lesson worth keeping: a green result from a test that cannot fail is worse than a red one.

**What it got wrong — test setup:** the first integration-test helper opened a fresh
ten-connection pool inside *every* test. Eight tests tearing down and rebuilding pools back
to back raced the connect timeout once `go test ./...` ran packages in parallel, and one
test failed with "context deadline exceeded". The instinct to call that a flake and re-run
would have been wrong: it was a real defect in the harness. Moved to a single pool opened
in `TestMain` — correct, and the package went from 13.4s to 2.8s.

**What it got right:** flagged before writing that failing a whole batch on one malformed
reading creates a poison message — the client's durable queue would retry it forever and
never drain again. Partial acceptance was chosen because of that, and it is now the
behaviour a test asserts.

Also chose `unnest($1::uuid[], ...)` over a `VALUES` list for the insert. Postgres caps a
statement at 65535 parameters, so `VALUES` would have limited a flush to ~5900 rows and
produced a differently shaped query per batch length. Proven with an 8000-row test.

**Corrected by hand:** chose hybrid client+server detection in the previous session against
the recommendation; that decision shaped the `source` column and `stop_event_matches` table
this schema now carries.

**Verified, not assumed:**
- Idempotency under real retry pressure — 3088 rows, 0 duplicate `reading_id`s, after
  hundreds of shed-and-retry cycles.
- NULL versus 0 survives the round trip: an omitted `speed_mps` is NULL, an explicit 0 is 0.
- 8 store integration tests against real Postgres; they skip cleanly without
  `TEST_DATABASE_URL` so the suite stays green on a machine with no database.

**Still unverified:** the SIGTERM path. The writer's drain is now unit-tested
(`TestShutdownDrainsBufferedReadings`), but signal delivery itself has never run — Windows
force-kill sends no signal. `-race` also cannot run locally without a cgo toolchain. Both
need Linux CI, which is the next task.

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

## 2026-08-26 — Repo published, two self-inflicted detours

**Delegated:** correcting the Go module path, publishing the repo.

**What it got wrong — encoding:** rewrote three source files with PowerShell
`Get-Content -Raw` + `Set-Content -Encoding utf8` to change the module path casing. In
Windows PowerShell 5.1 that round trip reads UTF-8 files as ANSI and writes them back with
a BOM, so every em-dash in the comments became `â€"`. Caught it, restored from the last
commit, redid the change with `sed` — which operates on bytes and leaves multi-byte UTF-8
sequences alone. **Rule going forward: never round-trip source files through PowerShell
5.1's Get-Content/Set-Content.**

**What it got right:** noticed before publishing that the GitHub username is
`KaiTseHuang780911`, not the all-lowercase form used in `go.mod`. GitHub URLs are
case-insensitive but Go module paths are not — a consumer running `go get` with the
canonical casing would hit "module declares its path as X but was required as Y".

Also cleaned the module download cache after the goose detour: **603 MB → 39 MB**. The
orphaned `modernc.org` pure-Go SQLite implementation alone was 227 MB.

**Corrected by hand:** SSH. The push failed with `Permission denied (publickey)` — the key
is passphrase-protected and an agent session has no TTY to prompt on. Enabled Windows'
`ssh-agent` service (Automatic, so it survives reboots), pointed `core.sshCommand` at
Windows' OpenSSH since Git Bash's bundled ssh cannot talk to the Windows service agent,
and the passphrase was entered by hand once.

**Verified, not assumed:** GitHub's host-key fingerprints were checked against the
published values before being added to `known_hosts`, rather than accepting TOFU blindly.

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
