# CLAUDE.md

Context for Claude Code working in this repo. Keep this file current — when a decision
changes, update it here rather than re-explaining in every session.

# AI Rules and Guidelines

## Core Principles
* **Do not over complicate.** Write the simplest solution that works.
* **Do not make assumptions.** If a request is unclear, stop and ask questions.
* **Do not make unrequested changes.** Stick strictly to the task provided. No random refactoring or style changes.
* **Verify before finishing.** Double-check your work, run existing tests, and confirm functionality before stating a task is complete.

## What this is

A fleet telemetry platform. An Android driver app reports position and stop events; a Go
service ingests and stores them; a React dashboard and an LLM query layer read them back.

This is a portfolio project built to demonstrate production practices, not a toy. Prefer
the boring, explainable choice over the clever one — every decision here has to survive
being questioned in a job interview.

## Layout

```
/mobile          React Native + TypeScript (Expo dev build), Android target
/api             Go ingest + REST API
/web             Vite + React + TypeScript dashboard
/sim             Vehicle telemetry simulator (Go)
/internal/wire   Telemetry wire format, shared by /api and /sim
/scripts         Database setup and reset SQL
/docs/adr        Architecture decision records
```

One Go module at the repo root (`github.com/KaiTseHuang780911/fleet-telemetry`) covers
`/api`, `/sim`, and `/internal/wire`. Go scopes an `internal/` package to the subtree
rooted at its parent directory, so root-level `/internal/wire` is importable from both
services — `/api/internal/wire` would have been private to `/api`.

## Stack, fixed

- **Mobile:** Expo (dev build, not bare, not managed-only), TypeScript strict mode,
  expo-location + expo-task-manager for background tracking, expo-sqlite for the offline
  queue, TanStack Query for server state, Zustand for local state, Sentry for crash
  reporting, Maestro for E2E.
- **API:** Go 1.26 toolchain, but `go.mod` declares `go 1.25.7` — that is set by `go mod
  tidy` from goose's own requirement, not chosen, and it is also the minimum Go any static
  analyser must be built with to inspect this module. chi router, pgx for Postgres. Standard library first — do not add a
  framework.
- **Database:** PostgreSQL 17, installed natively (not Docker — see below). Plain, no
  Timescale for now. **BRIN goes on `positions.received_at`, not `recorded_at`** — BRIN
  only helps when physical row order correlates with the indexed column, and the offline
  queue deliberately drains hours-old readings in one batch, scattering `recorded_at`
  values across fresh pages. Time-range queries per vehicle are served by a composite
  B-tree on `(vehicle_id, recorded_at DESC)`.
- **Web:** Vite, React, TypeScript, MapLibre GL, Recharts, TanStack Query.
- **CI:** GitHub Actions. EAS Build for mobile artifacts.

## Development environment (Windows)

Worth knowing before suggesting commands — this machine is not a typical Unix setup.

- **Shell is PowerShell.** `&&` and `||` do not work in Windows PowerShell 5.1; chain with
  `;` or `if ($?) { ... }`.
- **No `make`.** The task runner is npm scripts in the root `package.json`. A `Makefile`
  exists but only delegates to npm, for anyone cloning on Unix.
- **No Docker, no WSL2.** Postgres 17 runs as a native Windows service. CI uses GitHub
  Actions' own Postgres service container; Fly.io builds images on its remote builder. Do
  not propose docker-compose or testcontainers without flagging that they need a WSL2
  install first.
- **Storage is split across two drives.** C: is a 250 GB SSD with limited headroom; E: is a
  1 TB spinning disk. Project source and Go caches (`GOMODCACHE`, `GOCACHE` →
  `E:\Claude\.gocache`) live on E:. Postgres data is at `E:\PostgreSQL\17\data`. In Phase 2
  the Android SDK and system images go on E:, but `GRADLE_USER_HOME` stays on C: — that is
  where the hot small-file build I/O happens.
- **git is 2.55** (upgraded 2026-08-27). Earlier sessions worked around 2.24 lacking
  `git init -b`, `git switch`, and `git restore`; those are all available now.

## Rules

1. **TypeScript strict, no `any`.** If a type is hard, model it properly or leave a `TODO`
   with a question — do not paper over it.
2. **Go: no panics in request paths.** Errors are returned and wrapped with context.
3. **Every non-obvious decision gets an ADR** in `/docs/adr`, numbered, in the format
   Context / Decision / Consequences. Short is fine — half a page.
4. **Tests alongside features, not after.** Go: table-driven tests. Mobile: unit tests for
   the sync queue and any pure logic; Maestro for flows.
5. **No secrets in the repo.** `.env.example` documents what is needed.
6. **Small commits with real messages.** The commit history is part of what this project is
   showing.

## Working style I want from you

- **Plan before writing.** For anything beyond a single file, outline the approach and wait
  for me to confirm. I need to understand this code well enough to defend it in an
  interview — a large diff I did not think about is worse than useless to me.
- **Explain unfamiliar idioms.** I have 13 years in C#/.NET and mobile, and I am newer to
  Go and to React Native. When you use an idiom specific to either, add a one-line comment
  on why it is done that way.
- **Flag tradeoffs out loud.** If there is a faster path and a more correct path, say so and
  let me choose.
- **Do not add dependencies without asking.** Every package is something I have to justify.
- **When I am wrong, say so.** Including about architecture.

## Current phase

<!-- Update this each session. -->
**Phase 2. Slices 1, 2, 2c and 2e are done and device-proven. 2d is next, then 2f.**
Phase 1 is complete (schema, ingestion with shed-load backpressure, derivation,
reconciliation; CI green on Linux with the race detector and a real SIGTERM test).

The mobile app is at `/mobile`: Expo SDK 57, RN 0.86, TypeScript strict plus
`noUncheckedIndexedAccess`. **183 unit tests across 12 files.** Slice 1 is the durable
offline outbox, 2 is background location through a foreground service, 2c is on-device
arrival detection, 2e is the route map.

ADRs 001-004 cover the server; **005** the mobile outbox and its delivery guarantees;
**006** the Android/Expo accommodations — not optional reading before touching the mobile
build; **007** client-side arrival detection; **008** the battery measurements behind 2d.

**Mobile toolchain notes (Windows):**
- JDK 17 (Temurin) is installed and `JAVA_HOME` points at it. `_JAVA_OPTIONS` is
  overridden at **user** level to `-Xmx4096M` because a machine-level `-Xmx1024M` would
  otherwise starve Gradle. An empty user value does NOT override a machine value.
- Android SDK is at **`E:\Android\android-sdk`** — an existing Xamarin-era SDK that
  Android Studio adopted, which is why it also holds API 15/21/22/23/25/27/28 and
  build-tools 23/25. `ANDROID_HOME`, `ANDROID_SDK_ROOT`, and PATH are set at user level.
  `GRADLE_USER_HOME` stays on C: (`C:\Users\KevinLocalAdmin\.gradle`) — that is where the
  hot small-file build I/O happens.
- **Expo SDK 57 compiles against API 36** (`compileSdkVersion 36`, `targetSdk 36`,
  `minSdk 24`, from `ExpoModulesCorePlugin.gradle`). Gradle auto-downloads a missing
  platform, so API 36 arrived on its own during the first build — no manual SDK Manager
  step was needed.
- First `assembleDebug` took 13 min; incremental ~3 min with a warm Gradle cache.
- `localhost` is unreachable from a phone. `EXPO_PUBLIC_API_URL` must be the machine's
  LAN address (currently `http://192.168.1.68:8080` in `mobile/.env`); the emulator uses
  `10.0.2.2`.
- Firewall rules allow inbound 8080 (API) and 8081 (Metro) on the **Private** profile, and
  the home Wi-Fi was reclassified Public -> Private so they apply. Verify rules with
  `netsh advfirewall firewall show rule name=all dir=in`, not `Get-NetFirewallRule` — the
  latter returns a stale view without admin and will claim rules are missing when they
  exist.
- App identity: `com.kaitsehuang.fleettelemetry`, name "Fleet Telemetry". `mobile/android/`
  is generated by `expo prebuild` and gitignored; regenerate with `--clean` after changing
  anything in `app.json`.
- `prebuild --clean` deletes `android/local.properties`, which holds `sdk.dir`. Write it
  back with `printf`, never PowerShell's `Set-Content -Encoding utf8` — that prepends a BOM
  and Gradle then reads the key as `﻿sdk.dir` and reports the SDK as missing.
- **A green Gradle build does not mean your change is in the APK.** Expo modules ship
  prebuilt AARs, so editing their `node_modules` sources compiles nothing. Check the APK's
  mtime, and verify build-affecting changes in the *packaged* manifest or in bytecode
  (`javap -c`), not in the source file.

**Next slices:** 2d battery-aware sampling, then 2f EAS Build to a signed APK. Google Play
submission is deferred — no developer account yet.

**Maps (slice 2e).** The route screen uses `expo-maps` (Google Maps, Android). The key is
read from `GOOGLE_MAPS_API_KEY` in `mobile/.env` by `app.config.js` and never committed;
`.env.example` documents obtaining and restricting it. Two gotchas cost an afternoon:
- The key must be restricted to **both** certificate fingerprints, because debug and
  release builds present different ones. Release currently signs with the *debug* keystore,
  so the debug SHA-1 is the one in use:
  `5E:8F:16:06:2E:A3:CD:2C:4A:0D:54:78:76:BA:A6:F3:8C:AB:F6:25`
- **A rejected key logs nothing** on the Compose-based renderer. The map draws its frame,
  attribution and zoom controls, and simply omits the tiles. Do not treat a silent logcat
  as evidence the key is fine — verify the certificate in the APK with
  `apksigner verify --print-certs` instead. Restriction changes also need a force-stop,
  not just a reopen.

**Release keystore.** Generated and sitting in `mobile/credentials/` (gitignored), with its
password beside it. Deliberately **not** wired into `build.gradle`: changing the signing key
changes the app identity, which makes `adb install -r` fail and forces an uninstall —
taking the SQLite outbox, the queued readings and the device id with it. A fresh device id
registers a *second* vehicle and orphans the existing history. Switch deliberately, with an
empty queue. Release SHA-1 is
`83:A5:31:20:83:32:F0:45:8D:41:74:D1:DE:58:C5:EB:E9:4E:EB:24`.
**That folder is not backed up anywhere else — losing it means the app can never be updated.**

**Verified on a real device (OnePlus 5, Android 10 / OxygenOS 10.0.1):**

- *Slice 1, the queue.* 250 readings queued to SQLite, held through a simulated outage,
  drained in 100-row batches; the process was force-stopped with 250 more queued and they
  survived the kill and drained after relaunch. 500 rows, 500 distinct `reading_id`,
  **0 duplicates**. Client ids confirmed genuine UUIDv7 (version nibble 7), time-ordered
  with no inversions — the property ADR-001 relies on.
- *Slice 1 again, unplanned.* 151 readings survived a total transport failure (every upload
  rejected for ~90 minutes) with 0 quarantined and 0 dropped, then uploaded on their own via
  the reconnect trigger. A better test than the one that was planned.
- *Slice 2, background location.* Real Android GPS fixes — 16-100 m accuracy, across ~90
  minutes — reached Postgres through the background task, the outbox and the drain. The
  server reported `duplicates: 2`, which is at-least-once delivery working as designed, not
  a defect. **Still untested: sustained tracking while moving, and a long offline stretch
  with real fixes.**

- *Slice 2, sustained.* A 45-minute drive produced 101 real fixes (7.5-352 m accuracy, up to
  19.6 m/s) through the background task and the offline queue, plus three on-device stops of
  6m18s, 2m36s and 12m32s. Four days of tracking since: **3,200+ positions, 30 client stops**.
- *Slice 2c, reconciled.* `stop_event_matches` holds rows at last. On real data the device's
  detector and the server's independent derivation agree to **5.7 s and 8.0 m on average**
  (worst 16 s / 24 m), with `derived_only = 0` — the server found no stop the device missed,
  which is early evidence the 90 s dwell threshold is not too conservative.
- *Slice 1 under real failure.* A batch carrying both reports of one stop made the API answer
  500 for hours; the client kept everything and retried, producing **20,913 rejected
  duplicates against 101 stored rows and zero loss**. The guarantee ADR-005 claims is the one
  that held when everything around it did not.

Getting slice 2 to that point took three separate platform defects, none of them code. See
ADR-006 before assuming a green build means a working app.

**The recurring failure mode, stated plainly.** Five times now the code has been correct and
the *running artifact* has not: a patch edited into `node_modules` sources Expo never
compiles; a mock run against an APK built before the feature existed; a server binary three
days older than its own fix; an API process missing an endpoint added that morning; a map key
registered against the wrong certificate. Green tests and a successful build say nothing
about what is actually running. **Check the artifact** — APK mtime, packaged manifest,
bytecode, process start time, signing certificate.

**Known gaps, deliberately not hidden:**
- Detection thresholds in the derivation pass are untuned guesses.
- Derivation truncates trips straddling the window's start edge.
- Device auto-registration is a development convenience, not authentication.
- A crash between `202` and the server-side flush loses buffered readings. See ADR-003.
- A stop the vehicle arrives at and never leaves stays open forever, and nothing notices.
  One such row has been open since 2026-09-20. Real, visible in production data, unhandled.
- Detection thresholds (90 s dwell, 50 m radius) remain guesses, though now with
  reconciliation data to tune against rather than nothing.
- Quarantined rows accumulate in `outbox_dead` and nothing drains or reports them.
- The outbox size cap (50,000) has never been reached, so the drop path is the
  least-exercised branch in the queue.
- `usesCleartextTraffic` is on. It must be **deleted, not narrowed**, before anything
  ships, along with TLS on the API and a real release keystore — release currently signs
  with the **debug** keystore.
- `expo-task-manager` is patched and builds from source. Every Expo upgrade must re-test
  it; `patch-package` fails loudly at install if the patch stops applying. See ADR-006.
