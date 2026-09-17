import {
  DEFAULT_STOP_THRESHOLDS,
  INITIAL_STOP_STATE,
  detectStops,
  distanceM,
  type Fix,
  type StopEmission,
  type StopState,
} from './detect';

const BASE = 1_760_000_000_000; // arbitrary fixed epoch, so failures are readable
const DEPOT = { lat: 49.2827, lon: -123.1207 };

/** Moves a point north by `metres`, which is all these tests need. */
function north(from: { lat: number; lon: number }, metres: number) {
  return { lat: from.lat + metres / 111_320, lon: from.lon };
}

function fix(over: Partial<Fix> & { at: number }): Fix {
  return { ...DEPOT, speedMps: 0, accuracyM: 8, ...over };
}

/** Fixes every `everyMs` from `fromMs` to `toMs` inclusive, at one place. */
function parked(fromMs: number, toMs: number, everyMs = 10_000, at = DEPOT): Fix[] {
  const out: Fix[] = [];
  for (let t = fromMs; t <= toMs; t += everyMs) {
    out.push(fix({ at: BASE + t, ...at }));
  }
  return out;
}

function ids() {
  let n = 0;
  return () => `event-${++n}`;
}

function run(fixes: Fix[], state: StopState = INITIAL_STOP_STATE) {
  return detectStops(state, fixes, ids());
}

function kinds(emissions: StopEmission[]) {
  return emissions.map((e) => e.kind);
}

describe('distanceM', () => {
  it('measures a short offset accurately enough for a 50m radius', () => {
    expect(distanceM(DEPOT, north(DEPOT, 100))).toBeGreaterThan(95);
    expect(distanceM(DEPOT, north(DEPOT, 100))).toBeLessThan(105);
  });

  it('is zero for the same point', () => {
    expect(distanceM(DEPOT, DEPOT)).toBe(0);
  });
});

describe('detectStops', () => {
  it('reports an arrival once the dwell threshold passes', () => {
    const { emissions, state } = run(parked(0, 120_000));

    expect(kinds(emissions)).toEqual(['arrived']);
    expect(emissions[0]).toMatchObject({ kind: 'arrived', arrivedAt: BASE });
    expect(state.openStop).not.toBeNull();
  });

  // The threshold is a minimum, not a target: a vehicle that has been still for
  // 89 seconds has not yet stopped as far as this detector is concerned.
  it('stays silent until the dwell threshold is actually reached', () => {
    const { emissions, state } = run(parked(0, 80_000));

    expect(emissions).toEqual([]);
    expect(state.anchor).not.toBeNull();
    expect(state.openStop).toBeNull();
  });

  // The case that makes a naive detector useless in a city.
  it('does not call a 45-second traffic light a stop', () => {
    const lights = [
      fix({ at: BASE, speedMps: 12 }),
      ...parked(5_000, 50_000),
      fix({ at: BASE + 60_000, speedMps: 11, ...north(DEPOT, 300) }),
    ];

    expect(run(lights).emissions).toEqual([]);
  });

  it('reports a departure when the vehicle leaves a reported stop', () => {
    const trip = [
      ...parked(0, 300_000),
      fix({ at: BASE + 360_000, speedMps: 9, ...north(DEPOT, 400) }),
    ];
    const { emissions } = run(trip);

    expect(kinds(emissions)).toEqual(['arrived', 'departed']);
    const [arrived, departed] = emissions;
    expect(departed).toMatchObject({ kind: 'departed', eventId: arrived?.eventId });
  });

  // The departure time is the last fix seen *inside* the radius, not the fix
  // that proved the vehicle had left. Using the latter would stretch every stop
  // by however long the vehicle spent driving away — or, across a coverage gap,
  // by hours.
  it('dates the departure from the last fix inside the radius', () => {
    const trip = [
      ...parked(0, 300_000),
      // Nothing for two hours — a tunnel, a dead battery, a killed process.
      fix({ at: BASE + 7_500_000, speedMps: 9, ...north(DEPOT, 800) }),
    ];
    const { emissions } = run(trip);
    const departed = emissions.find((e) => e.kind === 'departed');

    expect(departed).toMatchObject({ departedAt: BASE + 300_000 });
  });

  // GPS does not sit still while the vehicle does. Scatter inside the radius
  // has to read as one stop, not as a departure and a fresh arrival.
  it('treats GPS scatter around a parked vehicle as a single stop', () => {
    const scattered: Fix[] = [];
    for (let t = 0; t <= 300_000; t += 10_000) {
      const drift = (t / 10_000) % 2 === 0 ? 20 : -15;
      scattered.push(fix({ at: BASE + t, ...north(DEPOT, drift) }));
    }

    expect(kinds(run(scattered).emissions)).toEqual(['arrived']);
  });

  it('starts a new stop after a real departure', () => {
    const twoStops = [
      ...parked(0, 200_000),
      fix({ at: BASE + 260_000, speedMps: 10, ...north(DEPOT, 500) }),
      ...parked(300_000, 500_000, 10_000, north(DEPOT, 1_000)),
    ];
    const { emissions } = run(twoStops);

    expect(kinds(emissions)).toEqual(['arrived', 'departed', 'arrived']);
    expect(emissions[0]?.eventId).not.toBe(emissions[2]?.eventId);
  });
});

describe('state across deliveries', () => {
  // The reason this detector is incremental at all. Android delivers a few
  // fixes at a time and may kill the process in between, so a stop that spans
  // deliveries must still be detected.
  it('detects a stop spanning several deliveries', () => {
    const nextId = ids();
    let state = INITIAL_STOP_STATE;
    const all: StopEmission[] = [];

    for (const batch of [parked(0, 30_000), parked(40_000, 70_000), parked(80_000, 110_000)]) {
      const result = detectStops(state, batch, nextId);
      state = result.state;
      all.push(...result.emissions);
    }

    expect(kinds(all)).toEqual(['arrived']);
  });

  // State is persisted as JSON between deliveries, so it has to survive the
  // round trip. A Date or a Map here would silently become something else.
  it('survives serialisation to JSON and back', () => {
    const first = detectStops(INITIAL_STOP_STATE, parked(0, 120_000), ids());
    const revived: StopState = JSON.parse(JSON.stringify(first.state));

    expect(revived).toEqual(first.state);

    const after = detectStops(
      revived,
      [fix({ at: BASE + 200_000, speedMps: 10, ...north(DEPOT, 600) })],
      ids(),
    );
    expect(kinds(after.emissions)).toEqual(['departed']);
  });
});

describe('junk fixes', () => {
  // A 100m fix cannot tell "parked" from "moved half a block", so letting it
  // near a 50m radius test produces confident nonsense in both directions.
  it('ignores fixes too imprecise to place the vehicle', () => {
    const noisy = [
      ...parked(0, 120_000),
      fix({ at: BASE + 130_000, accuracyM: 500, ...north(DEPOT, 2_000) }),
      ...parked(140_000, 200_000),
    ];

    expect(kinds(run(noisy).emissions)).toEqual(['arrived']);
  });

  it('accepts fixes with no reported accuracy', () => {
    const unknown = parked(0, 120_000).map((f) => ({ ...f, accuracyM: null }));

    expect(kinds(run(unknown).emissions)).toEqual(['arrived']);
  });

  // A device that never reports speed must still be able to record a stop; the
  // radius test is what actually decides.
  it('detects a stop when speed is never reported', () => {
    const noSpeed = parked(0, 120_000).map((f) => ({ ...f, speedMps: null }));

    expect(kinds(run(noSpeed).emissions)).toEqual(['arrived']);
  });

  it('processes a batch delivered out of order', () => {
    const shuffled = [...parked(0, 120_000)].reverse();

    expect(kinds(run(shuffled).emissions)).toEqual(['arrived']);
  });

  it('ignores a fix older than what has already been folded in', () => {
    const first = detectStops(INITIAL_STOP_STATE, parked(0, 120_000), ids());
    const stale = detectStops(
      first.state,
      [fix({ at: BASE - 500_000, speedMps: 10, ...north(DEPOT, 5_000) })],
      ids(),
    );

    expect(stale.emissions).toEqual([]);
    expect(stale.state.openStop).not.toBeNull();
  });

  it('does nothing with an empty batch', () => {
    const result = detectStops(INITIAL_STOP_STATE, [], ids());

    expect(result.emissions).toEqual([]);
    expect(result.state).toEqual(INITIAL_STOP_STATE);
  });
});

describe('thresholds', () => {
  it('honours a custom dwell threshold', () => {
    const quick = { ...DEFAULT_STOP_THRESHOLDS, minDwellMs: 20_000 };
    const result = detectStops(INITIAL_STOP_STATE, parked(0, 30_000), ids(), quick);

    expect(kinds(result.emissions)).toEqual(['arrived']);
  });

  // Matching the server's radius is the whole basis of reconciliation; if this
  // drifts, disagreement in stop_event_matches stops meaning anything.
  it('defaults to the 50m radius the server derivation uses', () => {
    expect(DEFAULT_STOP_THRESHOLDS.radiusM).toBe(50);
  });
});
