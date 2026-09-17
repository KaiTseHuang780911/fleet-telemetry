# ADR-006: Android and Expo platform accommodations

- **Status:** accepted
- **Date:** 2026-09-17

## Context

Background location on Android is not a library call, it is a negotiation with the
platform. Between 2026-09-14 and 2026-09-16 the app failed three times on a real device —
a OnePlus 5 running Android 10 / OxygenOS 10.0.1 — and **none of the three failures was a
code defect.** All three were configuration or platform behaviour, invisible to TypeScript,
to the unit suite, and to a green CI run.

They are recorded together because they share a shape, and because each carries an ongoing
cost that a future maintainer has to know about.

1. **Cleartext HTTP is blocked in release builds.** Expo injects
   `usesCleartextTraffic="true"` into debug builds and not release ones. The first APK
   built to run without Metro therefore could not reach `EXPO_PUBLIC_API_URL` at all. Every
   upload failed with `UnknownServiceException`; the queue correctly kept everything and
   uploaded nothing.

2. **The first background fix crashed the process.** `expo-task-manager` delivers every fix
   through a *persisted* `JobScheduler` job. Android refuses to persist a job without
   `RECEIVE_BOOT_COMPLETED` and signals that refusal by throwing
   `IllegalArgumentException` on the main thread. Declaring the permission was necessary
   and **not sufficient**: OxygenOS reports it as `granted=true` and JobScheduler still
   refuses. The fix was redelivered on every relaunch, so the app crash-looped until device
   location was switched off.

3. **The background task closed the UI's database.** `expo-sqlite` caches one native
   connection per database name — "create new connection even if connection with the same
   database name exists in cache, **default false**". The location task opened a store per
   fix and closed it in a `finally`, believing the handle was its own. It was the UI's.
   Every fix released the native database under the running screen, and the next poll
   failed with `NativeDatabase.prepareAsync ... NullPointerException`.

## Decision

**1. `usesCleartextTraffic: true` via `expo-build-properties`, as a development-only
accommodation.** It is the only way to reach a plain-HTTP dev server from a release build
under config control.

**2. Patch `expo-task-manager` to `setPersisted(false)`, via `patch-package`.** Persistence
buys almost nothing here: the job is one-shot, handing a batch of locations to JS within
seconds of being scheduled, so it only matters if the device reboots inside that gap. Expo
pays for that with a permission requirement that bricks the app on any ROM which declines
the grant.

The patch is **three files**, and the reason matters:

- `TaskManagerUtils.java` — the actual change.
- `expo-module.config.json` — drop the `publication` block. The package ships a **prebuilt
  AAR**, and `android/src/main/java` is shipped for reference and never compiled. Patching
  the Java alone produced `BUILD SUCCESSFUL`, a green `patch-package ✔`, and an APK that
  could not contain the fix.
- `android/build.gradle` — building from source then needs
  `project(':unimodules-app-loader')`, a path that exists only inside Expo's monorepo
  (presumably why the module ships prebuilt). Replaced with the coordinate the same package
  publishes.

`RECEIVE_BOOT_COMPLETED` **stays declared** even though the patch removes the need for it:
`npm install --ignore-scripts` skips postinstall and therefore the patch, silently, and
holding the permission keeps that failure benign on ROMs that honour it.

**3. The background task opens an isolated SQLite connection** (`useNewConnection: true`),
exposed as an `isolated` option so each call site states what it wants. The UI keeps the
long-lived shared connection; the task takes its own. Two connections on one file are safe
here — WAL allows a reader alongside a writer, and `busy_timeout` makes the loser of a
write race wait rather than fail.

**4. Config invariants are tested as relationships, not values.** `src/config/manifest.test.ts`
asserts that background location being enabled implies the permissions that make it
survivable. Restating each value would only prove `app.json` parses.

## Alternatives considered

**For cleartext: run the dev server over TLS with a self-signed certificate.** Rejected for
now — it moves the problem to trusting a local CA on the device, which is its own
multi-step accommodation, for a server that is not reachable outside a home LAN.

**For the crash: enable OxygenOS "auto-launch" for the app.** The leading hypothesis, and
the setting could not be found on this device. Rejected as a fix in any case: a build that
only runs after an undocumented OEM toggle is not a build that can be handed to anyone.

**For the crash: upgrade the dependency.** `expo-task-manager@57.0.18` is byte-identical in
this file. Not available.

**For the crash: abandon `expo-task-manager` for foreground-only `watchPositionAsync`.**
Rejected — it discards headless restart, which is the entire point of background tracking.

**For the database: reference-count connections inside `SqliteOutbox`.** Rejected as
reimplementing, badly, what `useNewConnection` already provides at the layer that owns the
cache.

## Consequences

**One module now builds from source, and its build wiring is this project's problem.**
Every Expo upgrade may break the patch, and `patch-package` will fail loudly at install
when it does — which is the right failure, but it is now a step in every upgrade.

**Background tasks no longer resume by themselves after a reboot** until the app is next
opened. Accepted deliberately for a project with no reboot-survival requirement.

**`usesCleartextTraffic` permits plaintext to any host, not just the dev machine.** This
must be **deleted, not narrowed**, before anything ships, alongside TLS on the API and a
real release keystore — release currently signs with the debug keystore.

**Verification cannot be trusted to the source file.** Twice now, a green build reported
success over a change that reached nothing. The `setPersisted` fix was confirmed with
`javap -c` on the class Gradle actually compiled (`iconst_0` immediately before
`setPersisted(Z)`), and the cleartext flag in the *packaged* manifest rather than the source
one. That standard should hold for anything touching the build.

**We would revisit all of this** when the app moves to EAS Build and a signed release for
Play submission — at which point cleartext goes, the keystore becomes real, and the patch
should be re-tested against whatever Expo SDK that build uses. If upstream ever makes
persistence configurable, the patch should be dropped rather than carried.
