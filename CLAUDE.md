# CLAUDE.md

Context for Claude Code working in this repo. Keep this file current — when a decision
changes, update it here rather than re-explaining in every session.

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
**Phase 1 complete.** Schema and migrations, `POST /v1/telemetry` with a bounded channel
and batch writer, shed-load backpressure, a deterministic simulator, and server-side trip
and stop derivation reconciled against device-reported stops. ADR-001 (schema and
indexing), ADR-002 (hybrid reconciliation), ADR-003 (ingestion), ADR-004 (derivation) are
written.

**CI is green** on GitHub Actions (Linux, `postgres:17` service): gofmt, `go mod tidy`
verification, vet, staticcheck, and `go test -race ./...` including store integration tests
and a real SIGTERM shutdown test. Those last two cannot run on this machine — no cgo
toolchain, and Windows has no SIGTERM — and they skip with an explicit reason rather than
passing silently. Do not "fix" that by deleting the skip.

Verified under load: with a one-slot buffer and 40 concurrent vehicles, 258 readings were
shed with `503`, the simulator backed off and retried, and zero duplicate rows resulted.

Reconciliation over 8 simulated vehicles for an hour: 72 client stops, 52 derived, 52
matched, 20 client-only, 0 derived-only, mean signed delta +5.9s, mean distance 30.8m. The
client-only count is the device's 45s reporting threshold against the server's 120s; the
consistent positive bias is the device timing arrival from when motion ceased while the
server can only anchor on the first sample already at rest.

**Next: Phase 2 — the React Native driver app.** The Jobber-critical piece and the largest
remaining chunk. Android SDK and system images go on E:, `GRADLE_USER_HOME` stays on C:.

**Known gaps, deliberately not hidden:**
- Detection thresholds are untuned guesses. The reconciliation numbers describe agreement
  with a simulator whose stop model was written alongside them — a working harness, not a
  measurement of reality.
- Derivation truncates trips straddling the window's start edge. Mitigated by a generous
  lookback, not solved.
- Device auto-registration on first sight is a development convenience; a real deployment
  would require device authentication.
- A crash between `202` and the flush loses buffered readings. Bounded and deliberate — the
  device resends what it was never told was durable — but real. See ADR-003.
