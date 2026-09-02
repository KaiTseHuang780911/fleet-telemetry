# Fleet Telemetry Platform

[![CI](https://github.com/KaiTseHuang780911/fleet-telemetry/actions/workflows/ci.yml/badge.svg)](https://github.com/KaiTseHuang780911/fleet-telemetry/actions/workflows/ci.yml)

An end-to-end telematics system. An Android driver app produces position and stop
telemetry; a Go service ingests it into Postgres; a React dashboard and a
natural-language query layer read it back.

Built as one coherent system rather than four unrelated demos — the mobile app is what
generates the data everything else consumes.

> **Status:** Phase 1 complete. The simulator posts telemetry and its own stop events, the
> API buffers and batch-writes to Postgres, and a derivation pass reconstructs trips and
> stops from the position stream and reconciles them against what the device reported.
> Phases 2–5 (mobile app, platform layer, dashboard, NL query) are not started.

---

## Architecture

```
 ┌──────────────────────┐
 │  Driver App (RN+TS)  │  background location, offline queue,
 │  Android             │  route/stop list, arrival detection
 └──────────┬───────────┘
            │ HTTPS batch POST
            ▼
 ┌──────────────────────┐
 │  Ingest API (Go)     │  buffered channel → batch writer
 │  + Postgres          │  trips, positions, stop events
 └──────────┬───────────┘
            │ REST
     ┌──────┴───────┐
     ▼              ▼
┌──────────┐  ┌──────────────────┐
│ Dashboard│  │ NL Query (LLM    │  tool calling over the platform's
│ React+TS │  │ function calling)│  own API, plus an eval suite
└──────────┘  └──────────────────┘
```

## Layout

```
api/            Go ingest + REST API
  cmd/api/      service entrypoint
  internal/     handlers, ingestion pipeline, storage
  migrations/   SQL migrations
sim/            Vehicle telemetry simulator (Go)
internal/wire/  Telemetry wire format, shared by api/ and sim/
web/            Vite + React + TypeScript dashboard        (Phase 4)
mobile/         React Native + TypeScript driver app       (Phase 2)
scripts/        Database setup and reset SQL
docs/adr/       Architecture decision records
docs/BUILD_LOG.md
```

One Go module at the repo root covers `api/`, `sim/`, and `internal/wire/`. Go scopes an
`internal/` package to the subtree rooted at its parent, so a root-level `internal/wire/`
is importable from both services — `api/internal/wire/` would have been private to `api/`.

## Stack

| Layer | Choice | Why |
|---|---|---|
| Mobile | Expo dev build, TypeScript strict | Background location and native config plugins without ejecting; EAS keeps the Android toolchain off the critical path |
| API | Go, chi, pgx | Standard library first; no framework |
| Database | PostgreSQL 17, plain | A well-indexed `positions` table holds millions of rows at this scale. Timescale is a later ADR, not a day-one dependency |
| Ingestion | HTTP → buffered channel → batch writer | A real backpressure decision that can be explained line by line. A broker here would be resume-driven |
| Web | Vite, React, TypeScript, MapLibre GL, Recharts | — |
| E2E | Maestro | YAML flows, minimal setup, runs cleanly on Android CI |

## Prerequisites

- **Go 1.26+**
- **Node 20+**
- **PostgreSQL 17** running locally on port 5432

## Quickstart

```bash
git clone https://github.com/KaiTseHuang780911/fleet-telemetry
cd fleet-telemetry

cp .env.example .env      # adjust if your Postgres differs
npm install

npm run db:setup          # creates the `fleet` role and both databases
npm run db:migrate        # Phase 1
npm run dev               # API + simulator
```

## API

| Endpoint | |
|---|---|
| `POST /v1/telemetry` | Batch of readings. Returns `202` with `{accepted, rejected}`; `503` with `Retry-After` when shedding load |
| `GET /v1/vehicles` | All known vehicles |
| `GET /v1/vehicles/{id}/trips` | Trips overlapping `?from=`/`?to=` (RFC 3339, default last 24h) |
| `GET /v1/vehicles/{id}/stops` | Stop events; `?source=client` or `?source=derived` to narrow |
| `GET /v1/reconciliation` | How far the device's own stop detection and the server's derivation disagree |
| `GET /healthz` | Liveness — never touches the database |
| `GET /readyz` | Readiness — pings the database, reports ingest counters |

Ingestion is idempotent: readings carry a client-generated UUIDv7 primary key, so a
device that retries after a timeout costs a conflict check rather than a duplicate row.
A batch containing one malformed reading has that reading returned in `rejected` while
the rest are accepted — failing the whole batch would make it a poison message that
blocks the device's queue forever. See [ADR-003](docs/adr/003-ingestion-pipeline-and-backpressure.md).

| Command | Does |
|---|---|
| `npm run dev` | API and simulator together |
| `npm run derive` | Recompute trips, stops, and reconciliation over the lookback window |
| `npm run dev:api` / `dev:sim` / `dev:web` | one at a time |
| `npm test` | Go tests. Store integration tests skip unless `TEST_DATABASE_URL` is set |
| `npm run test:integration` | Store tests against a real Postgres |
| `npm run lint` | `go vet` plus a gofmt check |
| `npm run db:setup` | create role and databases (idempotent) |
| `npm run db:reset` | drop and recreate both databases — destructive |
| `npm run db:psql` | psql shell as the app user |

A `Makefile` wraps these so `make dev` works too.

## Deviations from the original brief

Recorded here rather than silently — each was a deliberate call.

- **npm scripts instead of `make` as the task runner.** Development happens on Windows,
  which has no GNU make by default. Node is already a dependency of the web and mobile
  workspaces, so npm is the one runner guaranteed to be present. The `Makefile` delegates
  to it for anyone on a Unix machine.
- **Native PostgreSQL instead of Docker.** WSL2 was not installed on the development
  machine, making Docker Desktop a multi-reboot setup for something a single MSI provides.
  Nothing in Phase 0–1 needs containers: CI uses GitHub Actions' own Postgres service, and
  Fly.io builds images on its remote builder. A compose file can be added when local image
  testing actually earns its keep.
- **BRIN index on `received_at`, not `recorded_at`.** BRIN only helps when physical row
  order correlates with the indexed column. `recorded_at` comes from the device clock, and
  the offline queue deliberately drains hours-old readings in a single batch — scattering
  old timestamps across fresh pages. The correlated column is `received_at`. Time-range
  queries by vehicle are served by a composite B-tree instead. See `docs/adr/`.

## Two sources, reconciled

Stops come from two places and both are kept. The device detects its own using sensor
context the server never sees; the server derives its own from the position stream, where
it can apply consistent thresholds across the fleet and cannot be misled by a buggy client.
Neither overwrites the other, and `stop_event_matches` records which pairs correspond and
how far apart they were.

The unmatched events are the point. A run over 8 simulated vehicles for an hour:

```
client stops 72   derived 52   matched 52   client-only 20   derived-only 0
mean signed delta +5.9s        mean distance 30.8m
```

Every figure there is explainable. The 20 client-only stops are the device reporting after
45s where the server requires 120s. Zero derived-only follows: anything long enough for the
server is long enough for the device. And the +5.9s is a *consistent bias*, not scatter —
the device times arrival from the moment motion ceases, while the server can only anchor on
the first sample already at rest. That is what a signed mean is for; an absolute mean would
report the same number for random disagreement.

Derivation recomputes a window rather than reacting to readings as they arrive, because a
device leaving a dead zone delivers hours-old readings that a streaming detector would
already have passed by. See [ADR-004](docs/adr/004-trip-and-stop-derivation.md).

## Documentation

- [`docs/adr/`](docs/adr/) — architecture decision records
- [`docs/BUILD_LOG.md`](docs/BUILD_LOG.md) — per-session notes on what was delegated to
  Claude Code, what it got wrong, and what was corrected by hand
