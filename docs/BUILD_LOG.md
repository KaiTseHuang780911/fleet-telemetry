# Build log

A few lines per session: what was delegated to Claude Code, what it got wrong, and
what had to be corrected by hand. This is the raw material for the "how do you work
with agentic AI tools" interview question — without it, in three weeks the honest
answer is "it went well" and nothing more specific.

Newest entries at the top.

---

## 2026-09-16 (later) — patching the dependency, and the patch that did nothing

**Delegated:** stop the crash that made the app unusable whenever device location was on.

Adding `RECEIVE_BOOT_COMPLETED` was correct and insufficient. The OS reported
`granted=true`, the permission appeared in the packaged manifest with no `maxSdkVersion`,
and JobScheduler threw the identical `IllegalArgumentException` an hour after the install.
OxygenOS 10 grants the permission and then declines to honour it. Nothing an app can do
about that from its own manifest.

Ruled out before touching a dependency: a `maxSdkVersion` narrowing the declaration; a stale
APK; AppOps background restrictions; `expo-task-manager@57.0.18`, byte-identical in this
file. Both of `LocationTaskConsumer`'s delivery paths end at the same `scheduleJob`, so no
Expo option avoids it.

**The observation that made the fix small.** `setPersisted(true)` is hardcoded, and buys
almost nothing: the job is one-shot, handing a batch of locations to JS within seconds of
being scheduled, so persistence only matters if the device reboots inside that gap. Expo
pays for that with a permission requirement that crash-loops the app on any ROM which
refuses it.

**The patch that did nothing, and how it was caught.** Patching the `.java` file and
rebuilding produced `BUILD SUCCESSFUL` and `patch-package ✔`. Both were true about their own
step, and the change reached nothing: **expo-task-manager ships a prebuilt AAR** in
`local-maven-repo`, and its `android/src/main/java` is shipped for reference and never
compiled. The only evidence anything was wrong was the APK's mtime being unchanged. Two
green checkmarks, a successful build, and an artifact that could not possibly contain the
fix — that is this project's recurring failure mode arriving in a new costume.

So the patch grew to three files, each forced by the last:

1. `TaskManagerUtils.java` — `setPersisted(false)`, the actual fix.
2. `expo-module.config.json` — drop the `publication` block, so autolinking builds the
   module from source instead of resolving the AAR.
3. `android/build.gradle` — `project(':unimodules-app-loader')` is a path that only exists
   inside Expo's monorepo, which is presumably *why* the module ships prebuilt. Replaced
   with the coordinate the same package publishes, `host.exp.exponent:org.unimodules.apploader`.

That is more invasive than a one-line change and should be recorded as such: one module now
builds from source, so its build wiring is this project's problem at every Expo upgrade.

**Cost, accepted deliberately:** background tasks no longer resume by themselves after a
reboot until the app is next opened. Confirmed acceptable before the change was made.

**Kept on purpose:** `RECEIVE_BOOT_COMPLETED` stays declared even though the patch removes
the need for it. `npm install --ignore-scripts` skips postinstall and therefore the patch,
silently; holding the permission keeps that failure benign on ROMs that honour it.

**Verified, not assumed:** the patch was reverted out of `node_modules` and `npx
patch-package` run against clean upstream files, reporting `expo-task-manager@57.0.17 ✔` — a
patch only ever seen already-applied has not been shown to apply. The fix itself was then
confirmed in **bytecode**, not in source: `javap -c` on the class Gradle actually compiled
shows `iconst_0` immediately before `setPersisted(Z)`. After the first silent no-op, the
source file's contents were no longer acceptable as evidence.

**Still unverified:** the phone is behind a secure lockscreen, so tracking cannot be started
from adb. Whether the crash is gone needs one tap on Start tracking.

---

## 2026-09-16 — the location pipeline had never run, and it crashed at step one

**Delegated:** fix the crash that made the app unusable the moment device location was
switched on.

**The crash.** From the crash buffer, not from guessing:

```
java.lang.IllegalArgumentException: Error: requested job be persisted
  without holding RECEIVE_BOOT_COMPLETED permission.
    at expo.modules.taskManager.TaskManagerUtils.updateOrScheduleJob
    at expo.modules.location.taskConsumers.LocationTaskConsumer.reportLocationsImmediately
```

`expo-task-manager` hands every background fix to JS through a **persisted** JobScheduler
job, and Android refuses to persist a job unless the app holds
`RECEIVE_BOOT_COMPLETED`. The app did not. So the first location fix ever delivered threw
on the main thread and killed the process — and since the fix was redelivered on every
relaunch, the app crash-looped until location was switched back off. One line in
`app.json`.

The library's own manifest registers a `BOOT_COMPLETED` receiver and schedules persisted
jobs but never declares the permission either of those needs, leaving it to the app. That
is an upstream gap, not a mistake in this repo — but it is this repo's crash.

**Why it took three field tests to find.** The location task had never once executed on a
real device. Slice 1's field test drove the queue from the manual Record buttons, which
never touch it. The walk on the 15th had the device's master location toggle off, so no fix
was ever delivered. The first time a fix arrived was the first time this code path ran, and
it failed immediately. Every prior "verified on device" result was real, and none of them
covered this.

**The pattern worth naming.** Three defects in two days — cleartext blocked in release
builds, tracking reported as running when no fix could arrive, and this — and none of them
were code. All three were configuration or environment, invisible to TypeScript, to 111
passing unit tests, and to a green CI run. The tests were not weak; they were aimed at a
layer that was not where the failures lived.

So the guard added here is deliberately not another logic test. `src/config/manifest.test.ts`
asserts *relationships between config values*: if background location is enabled, the
permission that makes it survivable must be declared; if the foreground service is enabled,
its permissions must be present; a plugin must not appear twice. Restating each value would
only prove `app.json` parses. The RECEIVE_BOOT_COMPLETED case was confirmed to fail against
the broken config before being trusted.

**Corrected by hand:** `local.properties` twice. `prebuild --clean` deletes it, and writing
it back with PowerShell's `Set-Content -Encoding utf8` prepends a BOM, so Gradle read the
first key as `﻿sdk.dir` and reported the SDK as missing. The same PowerShell encoding
trap this log already records, in a new place. Written with `printf` instead.

**Not verified, and it matters:** the phone is behind a secure lockscreen, so tracking could
not be started from adb. The permission is present in the packaged manifest and
`granted=true` on the device, and the app no longer crash-loops on open — but the delivery
path that actually threw has not been exercised. That needs one tap on Start tracking.

---

## 2026-09-15 — two bugs, and a walk that recorded nothing

**Delegated:** build a release APK so the app runs without Metro, then verify the offline
queue over a real 40-minute walk with no coverage.

**Bug one — cleartext.** Every drain failed with `java.net.UnknownServiceException:
CLEARTEXT communication to 192.168.1.68 not permitted by network security policy`. Android
has blocked plain HTTP by default since API 28; Expo's *debug* builds inject
`usesCleartextTraffic="true"` and release builds do not. So the first artifact built to run
standalone was also the first one that could not reach the dev server — a difference between
build types that no amount of testing on the debug build would have surfaced. I built that
APK and called it ready without checking that a release manifest still permits the transport
`EXPO_PUBLIC_API_URL` is configured to use.

**Bug two — the walk recorded zero fixes, and the UI said it was working.** The phone's
master Location toggle was off (`settings get secure location_mode` → `0`). Both runtime
permissions were granted, the foreground service was running, and the debug screen showed
`tracking: running` in green for the entire walk. It could not have been true.
`hasStartedLocationUpdatesAsync` answers "is a task registered", not "can this device
produce a fix", and the app treats the first as if it were the second. Nothing in the code
is wrong; the *reporting* is, and a status indicator that reads green through a total
failure is worse than no indicator. `Location.hasServicesEnabledAsync()` is the missing
check.

**What I got wrong, and how.** Seeing `remaining 151` after `sent 100 · accepted 0`, I
announced the walk data was safe on the phone. Both halves were wrong. `sent 100` is the
batch size, so the queue held 151 items, not 251 — and when those 151 finally drained they
were all stamped `2026-09-12 01:49`, spanning **24 milliseconds**, with `accuracy_m` null:
the `Record 250` debug button, not a walk. I read a queue depth and asserted what was in it
without looking. The check that settled it took one query, and I should have run it before
saying anything.

**What did hold up.** Once cleartext was fixed, 151 readings uploaded **without Drain being
touched** — the `foreground` trigger fired on launch. `duplicates 0`, `failures 0`,
`quarantined 0`, `dropped 0`. Through a total transport failure and a reinstall, the queue
lost nothing. That is the outbox and the auto-drain wiring both working end to end; the
walk just never put anything into them.

**A limitation this exposed:** `shouldAutoDrain` requires `active` for the `periodic`
trigger, so a backgrounded app never drains on the timer. Draining then depends on the
location task's own post-record drain — which is fine while fixes are arriving and is
nothing at all while they are not.

**Verified, not assumed:** `prebuild --clean` regenerates `android/` wholesale, so the debug
keystore was hashed before and after (identical) to confirm the new APK would install with
`-r` rather than needing an uninstall, which would have wiped the SQLite queue. The cleartext
flag was checked in the *packaged* manifest, not the source one, since merging is what
decides. The clean also deletes `local.properties`, where the SDK path lives — the first
build failed on it.

**Still development-only:** `usesCleartextTraffic` permits plaintext to any host. Before
anything ships: TLS on the API, a real release keystore (release currently signs with the
debug one), and this flag deleted rather than narrowed.

---

## 2026-09-14 (later) — a question caught the bug that would have ruined the field test

**Found by the user asking a question**, before any code ran: *"my phone won't have internet
if I leave the house — the app handles going offline and uploads later, right?"*

It would not have. `unavailable` (network failure) counted against `maxAttempts: 5`, exactly
like `rejected` (a malformed request). With the background task draining every ten seconds,
readings hit five failed attempts about **fifty seconds** into any outage and were
quarantined into the dead-letter table — where they stay. A thirty-minute walk would have
lost nearly everything, and **would never have uploaded it on returning to coverage**.

The reasoning error is specific and worth keeping. `503` was correctly exempted from the
attempt count, on the grounds that a busy server is not the reading's fault. Being out of
coverage is not the reading's fault either — but it got filed with "malformed request", the
one case where retrying genuinely cannot help. Two environmental conditions, treated
differently for no reason.

Now only `rejected` burns attempts. Unbounded growth during a long outage is bounded by
`maxQueueSize` instead, which drops the *oldest* and reports it: lose the stalest data
loudly rather than the freshest quietly.

**Three existing tests failed on the fix — and they were right to.** Each asserted that a
network failure quarantines after five attempts. They had encoded the bug as a requirement,
which is worth noticing as its own failure mode: a test can pin a defect in place and make
removing it look like a regression. Rewritten against `rejected`, where quarantine belongs.

**Verified by reintroducing the bug**, as is now the habit here. The new long-outage tests
failed with exactly the right numbers: `Expected: 0 dead, Received: 30` and `Expected: 30
queued, Received: 0`. That is the walk, in two assertions.

Worth sitting with: the offline queue is the centrepiece of this phase, it had 50 passing
tests, it had been verified on a real device — and it could not survive the one scenario it
exists for. None of the existing tests simulated an outage longer than a few attempts.

---

## 2026-09-14 — tests for the class of bug, not just the bug

**Delegated:** write tests so a defect of this shape gets caught next time.

**The shape worth guarding**, rather than "vehicles were deleted": **in-memory state that
outlives the thing it describes, with no path back** — and a failure that reports success.

Nine tests added across two levels.

`store/cache_test.go` covers the cache lifecycle: recovery from a TRUNCATE under a live
process, invalidation clearing *every* device rather than only the one that failed, the
inverse guard that a non-foreign-key error must **not** wipe the cache (over-invalidating
would be a quieter bug — a working but slow system is far harder to trace than an outright
failure), and concurrent resolve/invalidate under `-race`.

`api/pipeline_integration_test.go` is the one that matters. It is the first test spanning
handler → writer → store → live Postgres, and it asserts the invariant that actually broke:
**if the API answers 202, that data must reach the database.** Any cause — a stale cache, an
unanticipated constraint, a writer that drops on error — fails it.

**Proved the tests fail without the fix**, rather than assuming. Reverting the invalidation
produced exactly the right diagnosis: *"the server never recovered: 0 rows after a further
10 were accepted. This is the three-day silent-loss failure returning."* Given this project
has now produced a green result from a test that could not fail three separate times, a new
test is not finished until it has been seen red.

**Which immediately exposed a second defect, this time in the test setup.** The new suite
passed alone and failed under `go test ./...`. Not flakiness: packages run in parallel, and
the store suite and the pipeline suite both pointed at `fleet_test` and truncated it —
deleting each other's rows mid-run. Fixed by giving the pipeline tests their own database
rather than reaching for `-p 1`, which removes the shared state instead of depending on
whoever remembers the flag.

**Also changed the fixture to stop hiding things.** `testStore` now clears the cache through
the production `InvalidateVehicleCache` rather than swapping the field behind its back, so
the suite notices if that path ever breaks. `TruncateAll` deliberately does **not** invalidate
the cache — production has no such hook, and a helper that tidies up after itself would
recreate exactly the blind spot that let this run for three days.

---

## 2026-09-14 — a cache with no invalidation path, found by accident

**Found by the user**, not by a test: the simulator had been logging "post failed" for three
days. The API was healthy, `/healthz` was green, and every POST returned **202 Accepted**.
The rows were never stored.

`/readyz` told the real story: **enqueued 5,780,070, inserted 771,690, failures 13,128**,
against a table holding 601 rows.

**Cause.** During phone testing the `vehicles` table was truncated with CASCADE while the
API process kept running. The API caches `external_id -> vehicle_id` in memory and had a
population path but **no invalidation path**, so it went on handing out ids for rows that no
longer existed. Every insert violated the foreign key, the whole flush failed, and it
retried identically forever.

**Why it is worse than a failed write.** The handler answers 202 as soon as the batch is
buffered, so the client deleted its only copy. ADR-003 accepted that a *crash* between 202
and the flush loses a batch. It did not anticipate a *persistent* error discarding
everything, indefinitely, while continuing to report success. That is the silent loss the
same ADR calls the worst failure mode this system has — and it ran for three days without
anything noticing.

**Fix.** A foreign-key violation (Postgres 23503, matched on the code rather than the
message, since messages are localised) now invalidates the cache and returns a distinct
`ErrStaleVehicleCache`. The batch in flight is still lost, but the next one re-resolves and
succeeds instead of the process being poisoned until restart. Regression test reproduces the
exact sequence: resolve, delete the row underneath, assert the insert fails *and* clears the
cache, then assert recovery.

**What this says about the test suite.** Every unit test passed throughout. The integration
tests passed too — because `testStore` truncates *and* resets the cache before each test,
which is precisely the step production has no equivalent of. The harness was quietly
papering over the bug it should have caught. Worth remembering: a fixture that resets state
the real system cannot reset is a fixture that hides this class of defect.

**Also a design note worth keeping:** the cache was a premature optimisation. It saves one
indexed lookup per batch and bought a correctness bug in exchange.

---

## 2026-09-12 — the queue runs on real hardware

**Delegated:** getting the Android toolchain working and closing the `SqliteOutbox` gap.

**Result:** the durability claims hold on a OnePlus 5 running Android 10. 250 readings
queued to SQLite, held through a simulated outage (`remaining 250`, nothing discarded),
drained in 100-row batches. Then the process was force-stopped with 250 more queued — they
survived and drained after relaunch. 500 rows server-side, 500 distinct, **0 duplicates**.
The device id in the settings table survived the kill too.

Also confirmed what the unit tests could not: the client ids really are UUIDv7 (version
nibble 7, not v4) and sort identically to `recorded_at` with zero inversions. That ordering
is the whole reason ADR-001 chose v7.

**The SDK was not what it appeared to be.** Android Studio adopted an existing Xamarin-era
SDK at ``E:\Android\android-sdk``, which is why it carried API 15 through 28 and build-tools
23/25. Expo SDK 57 compiles against API 36 specifically, which was absent — but Gradle
auto-downloads a missing platform, so it arrived during the first build with no manual step.
Deleted 17.7 GB of obsolete emulator images afterwards.

**Three wrong diagnoses in a row, on one UI bug.** Bold text was clipped on device —
"Record 250" rendered as "Record", "Drain" as "Drai". First guess: synthetic bold, fixed
with 2px of padding. No effect. Second guess: flex shrinking squeezing the buttons, fixed
with `flexShrink: 0`. No effect either.

What finally identified it was reading the evidence properly rather than pattern-matching:
the loss **scaled with length** (one character on "Drain", four on "Record 250") and hit
only weights 600/700, while longer normal-weight strings like "simulate offline" were fine.
That is synthetic bold after all — Android measures with the regular face and draws with
widened glyphs — but it also means a fixed padding can never work, because the overflow is
proportional. The fix is to stop the synthesis: `sans-serif-medium` is a real Android family
with its own weight, so measurement matches rendering.

Worth keeping: the first diagnosis was *correct* and the first fix was still wrong. Being
right about the cause does not mean the obvious remedy addresses it.

**Also worth noting:** every screenshot before the phone was unlocked came back blank white,
which looked exactly like a rendering failure. It was the lockscreen — Android blanks
`screencap` behind a secure lockscreen, and `mWakefulness=Asleep` was the giveaway. Several
minutes went into debugging an app that was working perfectly.

---

## 2026-09-05 — Phase 2 slice 1: the offline sync queue

**Delegated:** Android toolchain setup, the Expo scaffold, and the durable outbox.

**Chosen against the recommendation:** building the queue before any UI. The stated
downside was that nothing is observable while it is built. Mitigated rather than argued
with — the slice ships a debug screen showing queue depth, quarantine count, last drain
result, and a simulated-offline toggle, so the behaviour can be watched instead of
inferred from logs.

**Two environment landmines found before they could bite:**
- `JAVA_HOME` pointed at **JDK 1.8**. Android Gradle needs 17.
- `_JAVA_OPTIONS=-Xmx1024M` was set at **machine** level, so every JVM on the box
  silently capped its heap at 1 GB. Gradle wants several. This would have surfaced as
  inexplicable OOM build failures that look like Gradle bugs.

Both were machine-scoped on what is a work machine, so they were overridden at **user**
level rather than changed globally. Worth knowing: setting a user variable to an *empty*
string does not override a machine value — Windows treats empty as unset and falls back.
It has to be a non-empty value. The first attempt looked like it worked and did not; only
reading `MaxHeapSize` back out of a running JVM proved it.

**The design the slice rests on:** at-least-once delivery plus an idempotent server is
effectively-once. Rows leave the queue only after a confirmed 202, so a process killed
mid-request resends and `ON CONFLICT DO NOTHING` absorbs it. The queue therefore keeps
**no in-flight state** — and it is precisely that state, written between "sending" and
"sent", that loses data when Android kills an app at the wrong moment. Phase 1's
client-generated UUIDv7 primary key exists for this.

Client-side poison-message handling mirrors the server's: items the server names as
rejected are deleted rather than retried, and items that keep failing are quarantined to
a dead-letter table so the queue can drain. Same failure mode, the other end of the wire.

**Deliberate strictness that paid off immediately:** turning on `noUncheckedIndexedAccess`
surfaced five unchecked array accesses in the tests. Fixed with a helper that fails loudly
rather than with non-null assertions, which would have defeated the setting.

**Verified:** 50 unit tests; the app bundles for Android (650 modules).

**Not verified, and this is the honest gap:** `SqliteOutbox` has zero test coverage. The
engine is tested through the same `OutboxStore` interface, so the contract is covered, but
the SQL itself has never executed. That needs a device or emulator. Given this project's
recent history of green results from things that could not fail, it is worth saying plainly
rather than letting the passing suite imply otherwise.

---

## 2026-09-01 — Phase 1 finished: derivation and reconciliation

**Delegated:** server-side trip and stop detection, the reconciliation pass, client stop
reporting in the simulator, and the wiring for all of it.

**What it got right:** identified before writing anything that derivation cannot be
streaming. A device leaving a dead zone delivers hours of backlogged readings, so a
detector reacting to arrivals would already have closed trips covering that period. That
single constraint decided the whole architecture — recompute a window, replace it
transactionally — and everything else followed from it.

**What it got wrong — a condition that could never be true:** stops were linked to trips by
testing whether the stop's arrival fell *inside* a trip's time span. Trips are the movement
spans *between* stops, so that is never true. Every `trip_id` came out `NULL` while the code
read perfectly plausibly, and the unit tests passed because none of them asserted the link.
Caught only by looking at real API output. The fix links a stop to the latest trip ending
before it — "the journey that brought the vehicle here" — with a regression test.

**What it got wrong — a fixture whose meaning depended on its speed:** the simulator's
motion model was written in ticks ("dwell 30 to 150 ticks"). Running it at a 60ms tick to
generate data faster produced stops of 2-9 seconds, below the 45s the device needs before
reporting one, so **zero client stop events were generated** and the entire reconciliation
path silently had nothing to work on. Every test still passed. Rewritten in real units
(stops per hour, dwell in seconds, scaled by dt) with a regression test asserting behaviour
holds across 100ms, 1s, and 5s ticks.

That is the second time this project has produced a green result from something that could
not have failed. Worth noticing as a pattern rather than an incident.

**Also wrong, minor:** a `distanceM` unit test asserted an exact distance through a test
helper that uses the equatorial degree length, so it was off by 0.1% at Vancouver's
latitude. Replaced with reference values independent of the helper.

**Corrected by hand:** none this session — the design questions were settled up front.

**Added along the way:** `SIM_TIME_SCALE` and `SIM_BACKFILL_MS`, which decouple simulated
time from wall-clock time. Generating enough history to reconcile took hours at real speed;
now four real minutes produce an hour of fleet activity, with timestamps starting in the
past so they stay behind the server's clock. That also happens to exercise the offline
backlog path for real.

**Verified, not assumed:** derivation run three times over the same data produced identical
counts, no accumulation, no orphaned matches, no event claimed twice, and client-reported
rows untouched. All 52 derived stops linked to an arriving trip with zero ordering
violations.

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
