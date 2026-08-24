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

One Go module at the repo root (`github.com/kaitsehuang780911/fleet-telemetry`) covers
`/api`, `/sim`, and `/internal/wire`. Go scopes an `internal/` package to the subtree
rooted at its parent directory, so root-level `/internal/wire` is importable from both
services — `/api/internal/wire` would have been private to `/api`.

## Stack, fixed

- **Mobile:** Expo (dev build, not bare, not managed-only), TypeScript strict mode,
  expo-location + expo-task-manager for background tracking, expo-sqlite for the offline
  queue, TanStack Query for server state, Zustand for local state, Sentry for crash
  reporting, Maestro for E2E.
- **API:** Go 1.26, chi router, pgx for Postgres. Standard library first — do not add a
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
- **git is 2.24** — old. `git init -b`, `git switch`, and `git restore` are unavailable.

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
**Phase 0 — repo foundation.** Monorepo skeleton, README, task runner, and database setup
scripts are in place. Go 1.26 and PostgreSQL 17 are installed locally.

Next: Phase 1 — Postgres schema and migrations, then `POST /v1/telemetry` with the buffered
channel and batch writer, then the simulator. ADR-001 covers the ingestion design.
