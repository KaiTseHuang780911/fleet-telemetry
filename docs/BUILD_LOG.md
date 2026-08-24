# Build log

A few lines per session: what was delegated to Claude Code, what it got wrong, and
what had to be corrected by hand. This is the raw material for the "how do you work
with agentic AI tools" interview question — without it, in three weeks the honest
answer is "it went well" and nothing more specific.

Newest entries at the top.

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
