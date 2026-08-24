# mobile/

React Native + TypeScript driver app (Expo dev build, Android target).

**Scaffolded in Phase 2.** This directory is a placeholder so the monorepo layout is
visible from the first commit.

Planned:

- Background location reporting via `expo-location` + `expo-task-manager`, with an
  Android foreground service
- Offline-first SQLite sync queue that survives app restart and airplane mode, and is
  idempotent on the server side
- Today's route: stop list with status transitions (pending → en route → arrived → complete)
- Arrival detection by proximity and dwell time
- Battery-aware sampling interval, instrumented so the strategies can actually be compared
- Sentry crash and release monitoring; Maestro E2E; EAS Build to Google Play internal testing

See `../docs/` and the project brief for the full plan.
