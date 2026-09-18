import { batteryPctFrom, motionFrom, normaliseHeading } from './mapping';

describe('normaliseHeading', () => {
  // The case that matters: Android reports -1 when it has no fix on direction,
  // which is common while stationary. Passing that through would fail the
  // server's CHECK (heading_deg BETWEEN 0 AND 360) and get the entire reading
  // rejected — so a parked vehicle would silently stop reporting.
  it('treats Android’s -1 sentinel as unknown, not as a heading', () => {
    expect(normaliseHeading(-1)).toBeUndefined();
  });

  it.each([
    ['null', null],
    ['undefined', undefined],
    ['any negative value', -0.5],
  ])('treats %s as unknown', (_name, input) => {
    expect(normaliseHeading(input)).toBeUndefined();
  });

  it.each([
    [0, 0],
    [90, 90],
    [359.9, 359.9],
  ])('passes a valid heading of %p through unchanged', (input, expected) => {
    expect(normaliseHeading(input)).toBeCloseTo(expected, 5);
  });

  // 360 is a legal value server-side, but wrapping keeps a single
  // representation for due north rather than two.
  it('wraps 360 and beyond into range', () => {
    expect(normaliseHeading(360)).toBe(0);
    expect(normaliseHeading(450)).toBe(90);
  });

  it('never returns a value the server would reject', () => {
    for (const input of [-1, -0.1, 0, 1, 180, 359, 360, 361, 719, 720, null, undefined]) {
      const out = normaliseHeading(input);
      if (out !== undefined) {
        expect(out).toBeGreaterThanOrEqual(0);
        expect(out).toBeLessThanOrEqual(360);
      }
    }
  });
});

describe('motionFrom', () => {
  it.each([
    ['no reading', null, 'unknown'],
    ['undefined', undefined, 'unknown'],
    ['a negative sentinel', -1, 'unknown'],
    ['stationary', 0, 'still'],
    ['barely moving', 0.4, 'still'],
    ['walking pace', 1.5, 'walking'],
    ['driving', 11, 'driving'],
  ])('reports %s as %s', (_name, speed, expected) => {
    expect(motionFrom(speed)).toBe(expected);
  });

  // Unknown and stationary must stay distinguishable. Collapsing "the sensor
  // said nothing" into "the vehicle was still" is the same mistake as storing a
  // missing speed as 0, and it would corrupt every idle-time aggregate.
  it('distinguishes an absent reading from a zero reading', () => {
    expect(motionFrom(null)).toBe('unknown');
    expect(motionFrom(0)).toBe('still');
  });

  it('only ever returns a value the server accepts', () => {
    const allowed = new Set(['still', 'walking', 'driving', 'unknown']);
    for (const speed of [null, undefined, -5, 0, 0.49, 0.5, 2.49, 2.5, 100, 1e9]) {
      expect(allowed.has(motionFrom(speed))).toBe(true);
    }
  });
});

describe('batteryPctFrom', () => {
  it('converts a fraction to a whole percentage', () => {
    expect(batteryPctFrom(0.5)).toBe(50);
    expect(batteryPctFrom(1)).toBe(100);
    expect(batteryPctFrom(0)).toBe(0);
  });

  // The heading bug again, in a new field. expo-battery reports -1 when the
  // level is unavailable, and the server rejects the entire reading for a
  // battery_pct outside [0, 100] -- so a sentinel passed through would cost a
  // position, which is the one thing here that actually matters.
  it('treats the unavailable sentinel as absent, not as a value', () => {
    expect(batteryPctFrom(-1)).toBeUndefined();
  });

  it('treats a missing level as absent', () => {
    expect(batteryPctFrom(null)).toBeUndefined();
    expect(batteryPctFrom(undefined)).toBeUndefined();
    expect(batteryPctFrom(Number.NaN)).toBeUndefined();
  });

  // A float fraction can land marginally outside its range; the column is a
  // smallint with a CHECK, so the clamp is what keeps a rounding artefact from
  // rejecting the reading.
  it('never produces a value the server would reject', () => {
    for (const level of [0, 0.001, 0.5, 0.999, 1, 1.0000001, 2]) {
      const pct = batteryPctFrom(level);
      expect(pct).toBeDefined();
      expect(pct).toBeGreaterThanOrEqual(0);
      expect(pct).toBeLessThanOrEqual(100);
      expect(Number.isInteger(pct)).toBe(true);
    }
  });

  // 0% is a real reading, not a missing one. `if (!pct)` would drop it.
  it('distinguishes an empty battery from an unknown one', () => {
    expect(batteryPctFrom(0)).toBe(0);
    expect(batteryPctFrom(null)).toBeUndefined();
  });
});
