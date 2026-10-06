import {
  INITIAL_SAMPLING_STATE,
  LOW_BATTERY_PCT,
  MIN_TIER_DWELL_MS,
  TIERS,
  decideSampling,
  profileFor,
  tierFor,
  type SamplingInputs,
  type SamplingState,
  type SamplingTier,
} from './sampling';

function inputs(over: Partial<SamplingInputs> = {}): SamplingInputs {
  return { motion: 'driving', batteryPct: 80, charging: false, ...over };
}

describe('tierFor', () => {
  it.each<[string, Partial<SamplingInputs>, SamplingTier]>([
    ['driving', { motion: 'driving' }, 'active'],
    ['walking', { motion: 'walking' }, 'active'],
    ['stationary with charge', { motion: 'still' }, 'idle'],
    ['stationary and low', { motion: 'still', batteryPct: 10 }, 'saver'],
  ])('chooses %s', (_name, over, want) => {
    expect(tierFor(inputs(over))).toBe(want);
  });

  // Treating "the platform did not say" as stationary is the same error as
  // storing a missing speed as zero, and it fails expensively: a vehicle
  // driving at the idle tier produces a route that cannot be reconstructed.
  it('treats unknown motion as moving, not as stationary', () => {
    expect(tierFor(inputs({ motion: 'unknown' }))).toBe('active');
    expect(tierFor(inputs({ motion: 'unknown', batteryPct: 5 }))).toBe('active');
  });

  // There is no reason to conserve power from a wall socket.
  it('stays active while charging however flat the battery is', () => {
    expect(tierFor(inputs({ motion: 'still', batteryPct: 1, charging: true }))).toBe('active');
  });

  // A null reading is unknown, not empty. Dropping to the floor on a failed
  // read would be a sampling policy driven by a diagnostic bug.
  it('does not drop to saver when the battery level is unreadable', () => {
    expect(tierFor(inputs({ motion: 'still', batteryPct: null }))).toBe('idle');
  });

  it('switches to saver exactly at the threshold, not above it', () => {
    expect(tierFor(inputs({ motion: 'still', batteryPct: LOW_BATTERY_PCT }))).toBe('idle');
    expect(tierFor(inputs({ motion: 'still', batteryPct: LOW_BATTERY_PCT - 1 }))).toBe('saver');
  });
});

describe('tier profiles', () => {
  // Changing the baseline at the same time as introducing the policy would make
  // the before-and-after comparison uninterpretable.
  it('leaves the active tier identical to the slice-2 baseline', () => {
    expect(TIERS.active).toEqual({ timeIntervalMs: 10_000, distanceIntervalM: 20 });
  });

  it('samples progressively less from active to saver', () => {
    expect(TIERS.idle.timeIntervalMs).toBeGreaterThan(TIERS.active.timeIntervalMs);
    expect(TIERS.saver.timeIntervalMs).toBeGreaterThan(TIERS.idle.timeIntervalMs);
    expect(TIERS.idle.distanceIntervalM).toBeGreaterThan(TIERS.active.distanceIntervalM);
    expect(TIERS.saver.distanceIntervalM).toBeGreaterThan(TIERS.idle.distanceIntervalM);
  });

  // The floor is a floor, not an off switch. Sparse data still looks like
  // evidence, which makes it worse than no data.
  it('keeps the saver tier frequent enough to reconstruct a route', () => {
    expect(TIERS.saver.timeIntervalMs).toBeLessThanOrEqual(5 * 60_000);
    expect(TIERS.saver.distanceIntervalM).toBeLessThanOrEqual(150);
  });

  it('returns the profile for each tier', () => {
    for (const tier of ['active', 'idle', 'saver'] as const) {
      expect(profileFor(tier)).toEqual(TIERS[tier]);
    }
  });
});

describe('decideSampling', () => {
  const stable: SamplingState = { tier: 'active', since: 1_000_000, pending: null };

  it('does nothing when the wanted tier is already in use', () => {
    const d = decideSampling(stable, inputs({ motion: 'driving' }), stable.since + 10_000);

    expect(d.changedTo).toBeNull();
    expect(d.state).toBe(stable);
  });

  // The whole point of hysteresis. A restart tears down and rebuilds the
  // location subscription; doing that on every speed wobble would cost more
  // than the reduced sampling saves.
  it('refuses to slow down before the dwell has elapsed', () => {
    const first = decideSampling(stable, inputs({ motion: 'still' }), stable.since);
    const d = decideSampling(
      first.state,
      inputs({ motion: 'still' }),
      stable.since + MIN_TIER_DWELL_MS - 1,
    );

    expect(d.changedTo).toBeNull();
    expect(d.state.tier).toBe('active');
  });

  it('slows down once the wanted tier has held for the dwell', () => {
    const first = decideSampling(stable, inputs({ motion: 'still' }), stable.since);
    const d = decideSampling(
      first.state,
      inputs({ motion: 'still' }),
      stable.since + MIN_TIER_DWELL_MS,
    );

    expect(d.changedTo).toBe('idle');
    expect(d.state.tier).toBe('idle');
    expect(d.state.pending).toBeNull();
  });

  // The flaw the flapping test exposed, stated directly: an interrupted stop
  // must not leave dwell credit behind for the next one.
  it('restarts the dwell when conditions return to the current tier', () => {
    const wantsIdle = decideSampling(stable, inputs({ motion: 'still' }), 0);
    const moved = decideSampling(wantsIdle.state, inputs({ motion: 'driving' }), 60_000);

    expect(moved.state.pending).toBeNull();

    const stopsAgain = decideSampling(moved.state, inputs({ motion: 'still' }), 70_000);
    const tooSoon = decideSampling(
      stopsAgain.state,
      inputs({ motion: 'still' }),
      70_000 + MIN_TIER_DWELL_MS - 1,
    );

    expect(tooSoon.changedTo).toBeNull();
  });

  // Deliberately asymmetric. Dropping to a slower tier late costs a little
  // battery; staying slow after a vehicle starts moving costs the start of the
  // trip, which cannot be recovered.
  it('speeds up immediately, without waiting for the dwell', () => {
    const idle: SamplingState = { tier: 'idle', since: 1_000_000, pending: null };
    const d = decideSampling(idle, inputs({ motion: 'driving' }), idle.since + 1);

    expect(d.changedTo).toBe('active');
  });

  it('escalates to active from saver without waiting either', () => {
    const saver: SamplingState = { tier: 'saver', since: 1_000_000, pending: null };
    const d = decideSampling(saver, inputs({ motion: 'driving', batteryPct: 5 }), saver.since + 1);

    expect(d.changedTo).toBe('active');
  });

  // A vehicle at a junction alternating still/driving must not restart the
  // subscription on every fix.
  it('does not flap when motion alternates at a boundary', () => {
    let state = INITIAL_SAMPLING_STATE;
    let restarts = 0;
    let now = 0;

    for (let i = 0; i < 40; i++) {
      now += 10_000; // a fix every ten seconds
      const motion = i % 2 === 0 ? 'still' : ('driving' as const);
      const d = decideSampling(state, inputs({ motion }), now);
      state = d.state;
      if (d.changedTo) restarts += 1;
    }

    // Each "still" is immediately followed by movement, so the dwell never
    // completes and nothing should ever be applied.
    expect(restarts).toBe(0);
    expect(state.tier).toBe('active');
  });

  // The sequence the policy exists for: drive, park for a while, drive again.
  it('drops to idle during a real stop and recovers on departure', () => {
    let state: SamplingState = { tier: 'active', since: 0, pending: null };
    const applied: SamplingTier[] = [];
    let now = 0;

    // Twenty minutes parked, a fix every minute.
    for (let i = 0; i < 20; i++) {
      now += 60_000;
      const d = decideSampling(state, inputs({ motion: 'still' }), now);
      state = d.state;
      if (d.changedTo) applied.push(d.changedTo);
    }
    // Then it drives away.
    now += 60_000;
    const away = decideSampling(state, inputs({ motion: 'driving' }), now);
    if (away.changedTo) applied.push(away.changedTo);

    expect(applied).toEqual(['idle', 'active']);
  });

  it('drops to saver when the battery falls during a long stop', () => {
    const idle: SamplingState = { tier: 'idle', since: 0, pending: null };
    const first = decideSampling(idle, inputs({ motion: 'still', batteryPct: 15 }), 0);
    const d = decideSampling(
      first.state,
      inputs({ motion: 'still', batteryPct: 15 }),
      MIN_TIER_DWELL_MS,
    );

    expect(d.changedTo).toBe('saver');
  });

  // Plugging in while parked should restore full sampling without a wait,
  // because charging is an escalation to active.
  it('returns to active immediately when plugged in', () => {
    const idle: SamplingState = { tier: 'idle', since: 1_000_000, pending: null };
    const d = decideSampling(idle, inputs({ motion: 'still', charging: true }), idle.since + 1);

    expect(d.changedTo).toBe('active');
  });
});
