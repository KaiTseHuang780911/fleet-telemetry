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
**Phase 2, slice 1 in progress.** Phase 1 is complete (schema, ingestion with shed-load
backpressure, derivation, reconciliation; ADRs 001-004; CI green on Linux with the race
detector and a real SIGTERM test).

The mobile app now exists at `/mobile`: Expo SDK 57, RN 0.86, TypeScript strict plus
`noUncheckedIndexedAccess`. Slice 1 is the durable offline outbox and a debug screen to
observe it. 50 unit tests, app bundles for Android.

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

**Next slices:** background location with a foreground service, arrival detection,
battery-aware sampling, the route/stop UI, then EAS Build to a signed APK. Google Play
submission is deferred — no developer account yet.

**Verified on a real device (OnePlus 5, Android 10 / API 29):** the offline queue works
end to end. 250 readings queued to SQLite, held through a simulated outage, drained to the
server in 100-row batches; the process was then force-stopped with 250 more queued and they
survived the kill and drained after relaunch. 500 rows on the server, 500 distinct
`reading_id`, **0 duplicates**. Client ids confirmed genuine UUIDv7 (version nibble 7) and
time-ordered with no inversions, which is the property ADR-001 relies on.

**Known gaps, deliberately not hidden:**
- Detection thresholds in the derivation pass are untuned guesses.
- Derivation truncates trips straddling the window's start edge.
- Device auto-registration is a development convenience, not authentication.
- A crash between `202` and the server-side flush loses buffered readings. See ADR-003.
