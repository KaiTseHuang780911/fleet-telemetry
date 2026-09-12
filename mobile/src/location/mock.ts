/**
 * A synthetic route, for testing the pipeline without depending on GPS.
 *
 * **What this covers and what it does not.** The generated fixes go through
 * `recordLocations` — the exact function the real Android task calls — so
 * everything downstream of a fix is genuinely exercised: the wire mapping, the
 * outbox, the drain, the server, and the derivation pass that reads the result.
 *
 * It does not exercise Android producing the fix. GPS delivery, the foreground
 * service, and the headless restart are only proven by the real provider. This
 * is a pipeline test, not a substitute for walking outside with the phone, and
 * treating a green run here as "background location works" would be exactly the
 * kind of test-that-cannot-fail this project has already been caught by twice.
 *
 * The motion model mirrors the Go simulator's: real units scaled by elapsed
 * time, never "per tick", so changing the tick rate changes how often fixes are
 * emitted but not how the vehicle behaves.
 */

import type { LocationObject } from 'expo-location';

import { recordLocations } from './task';

const EARTH_RADIUS_M = 6_371_000;

/** Vancouver, matching the Go simulator's origin so both produce comparable data. */
const ORIGIN = { lat: 49.2827, lon: -123.1207 };

const CRUISE_SPEED_MPS = 11; // ~40 km/h, plausible urban driving
const SPEED_JITTER_MPS = 3;
const HEADING_DRIFT_DEG_PER_SEC = 4;

/** Expected stops per hour of driving, converted to a per-tick probability. */
const STOPS_PER_HOUR = 20;
const MIN_DWELL_MS = 60_000;
const MAX_DWELL_MS = 180_000;

/** Plausible GPS error, so the derivation pass sees realistic scatter. */
const ACCURACY_M = 8;

export interface MockRouteOptions {
  /** How often a fix is emitted, in real milliseconds. */
  tickMs?: number;
  /**
   * Simulated seconds per real second. Above 1, the route covers ground faster
   * than wall-clock so a stop long enough to be detected does not take three
   * real minutes to produce.
   */
  timeScale?: number;
  onFix?: (fix: LocationObject, stopped: boolean) => void;
  onError?: (message: string) => void;
}

export class MockRoute {
  private timer: ReturnType<typeof setInterval> | null = null;
  private lat = ORIGIN.lat;
  private lon = ORIGIN.lon;
  private headingDeg = Math.random() * 360;
  private speedMps = CRUISE_SPEED_MPS;
  private stopped = false;
  private dwellRemainingMs = 0;
  /** Simulated clock, which runs ahead of wall-clock when timeScale > 1. */
  private simNow = Date.now();
  private readonly tickMs: number;
  private readonly timeScale: number;
  private readonly onFix: (fix: LocationObject, stopped: boolean) => void;
  private readonly onError: (message: string) => void;

  constructor(opts: MockRouteOptions = {}) {
    this.tickMs = opts.tickMs ?? 2000;
    this.timeScale = opts.timeScale ?? 10;
    this.onFix = opts.onFix ?? (() => {});
    this.onError = opts.onError ?? (() => {});
  }

  get running(): boolean {
    return this.timer !== null;
  }

  start(): void {
    if (this.timer) return;
    // Start the simulated clock in the past so accelerated time stays behind
    // the server's clock. Readings from the future are rejected, correctly, and
    // a simulator that silently generates rejected data looks identical to one
    // that is not running.
    this.simNow = Date.now() - 60 * 60 * 1000;
    this.timer = setInterval(() => void this.tick(), this.tickMs);
  }

  stop(): void {
    if (!this.timer) return;
    clearInterval(this.timer);
    this.timer = null;
  }

  private async tick(): Promise<void> {
    const dtMs = this.tickMs * this.timeScale;
    this.advance(dtMs);

    // Never let the simulated clock overtake the server's.
    const wall = Date.now();
    if (this.simNow > wall) this.simNow = wall;

    const fix = this.toLocationObject();
    this.onFix(fix, this.stopped);

    try {
      // The same entry point the real Android task uses.
      await recordLocations([fix]);
    } catch (err) {
      this.onError(err instanceof Error ? err.message : String(err));
    }
  }

  private advance(dtMs: number): void {
    const seconds = dtMs / 1000;

    if (this.stopped) {
      this.speedMps = 0;
      this.dwellRemainingMs -= dtMs;
      if (this.dwellRemainingMs <= 0) {
        this.stopped = false;
        this.speedMps = CRUISE_SPEED_MPS;
      }
    } else if (Math.random() < (STOPS_PER_HOUR / 3600) * seconds) {
      this.stopped = true;
      this.dwellRemainingMs = MIN_DWELL_MS + Math.random() * (MAX_DWELL_MS - MIN_DWELL_MS);
      this.speedMps = 0;
    } else {
      this.headingDeg =
        (this.headingDeg + (Math.random() * 2 - 1) * HEADING_DRIFT_DEG_PER_SEC * seconds + 360) %
        360;
      this.speedMps = Math.max(0, CRUISE_SPEED_MPS + (Math.random() * 2 - 1) * SPEED_JITTER_MPS);
      this.move(this.speedMps * seconds);
    }

    this.simNow += dtMs;
  }

  /** Moves along the current heading, on a spherical earth. */
  private move(distanceM: number): void {
    if (distanceM === 0) return;

    const angular = distanceM / EARTH_RADIUS_M;
    const bearing = (this.headingDeg * Math.PI) / 180;
    const lat1 = (this.lat * Math.PI) / 180;
    const lon1 = (this.lon * Math.PI) / 180;

    const sinLat2 =
      Math.sin(lat1) * Math.cos(angular) + Math.cos(lat1) * Math.sin(angular) * Math.cos(bearing);
    const lat2 = Math.asin(sinLat2);
    const y = Math.sin(bearing) * Math.sin(angular) * Math.cos(lat1);
    const x = Math.cos(angular) - Math.sin(lat1) * sinLat2;
    const lon2 = lon1 + Math.atan2(y, x);

    this.lat = (lat2 * 180) / Math.PI;
    // Normalise into [-180, 180] so crossing the antimeridian cannot produce a
    // value the server rejects.
    this.lon = ((((lon2 * 180) / Math.PI + 540) % 360) - 180);
  }

  /** Shapes the state as expo-location would report it. */
  private toLocationObject(): LocationObject {
    return {
      coords: {
        latitude: this.lat,
        longitude: this.lon,
        altitude: 20,
        accuracy: ACCURACY_M + Math.random() * 4,
        altitudeAccuracy: 5,
        // Android reports -1 for heading when stationary and direction is
        // unknown. Reproducing that here is the point: it is what exercises
        // normaliseHeading, without which the server rejects the reading for
        // failing its 0..360 check.
        heading: this.stopped ? -1 : this.headingDeg,
        speed: this.speedMps,
      },
      timestamp: this.simNow,
    };
  }
}
