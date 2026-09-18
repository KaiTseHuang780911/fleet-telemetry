import {
  isHealthy,
  needsUserAction,
  trackingLabel,
  trackingState,
  type TrackingInputs,
  type TrackingState,
} from './status';

function inputs(over: Partial<TrackingInputs> = {}): TrackingInputs {
  return { registered: true, servicesEnabled: true, lastFixAt: 1_000_000, ...over };
}

describe('trackingState', () => {
  // The regression. On a real device this combination — task registered,
  // permissions granted, foreground service alive, device location master
  // switch off — rendered as "running" in green for forty minutes while
  // recording nothing. Any change that lets this return 'running' again is
  // reintroducing that bug.
  it('does not claim to be running when the device has location switched off', () => {
    const state = trackingState(inputs({ registered: true, servicesEnabled: false }));

    expect(state).toBe('blocked');
    expect(state).not.toBe('running');
    expect(isHealthy(state)).toBe(false);
  });

  // The same failure with the detail that made it convincing: fixes had been
  // recorded earlier, so a naive "have we ever seen a fix" check would still
  // say yes. Services being off has to win over that history.
  it('reports blocked even when fixes were recorded before location was switched off', () => {
    expect(trackingState(inputs({ servicesEnabled: false, lastFixAt: 1_000_000 }))).toBe('blocked');
  });

  it.each<[string, Partial<TrackingInputs>, TrackingState]>([
    ['nothing is registered', { registered: false }, 'off'],
    ['registered but services are off', { servicesEnabled: false }, 'blocked'],
    ['registered, permitted, no fix yet', { lastFixAt: null }, 'awaiting-fix'],
    ['registered, permitted, a fix has arrived', {}, 'running'],
  ])('reports %s', (_name, over, want) => {
    expect(trackingState(inputs(over))).toBe(want);
  });

  // "Not registered" outranks everything. A stopped task is stopped whatever
  // the device's settings say, and reporting it as blocked would send the user
  // to Settings to fix something that is not broken.
  it('reports off rather than blocked when nothing is registered', () => {
    expect(trackingState(inputs({ registered: false, servicesEnabled: false }))).toBe('off');
    expect(trackingState(inputs({ registered: false, servicesEnabled: false, lastFixAt: null }))).toBe(
      'off',
    );
  });

  // Deliberately not a staleness check. A parked vehicle does keep emitting —
  // measured at a 25s mean gap on a real drive — but irregularly, and the same
  // drive saw 408s between fixes while legitimately stationary. A timeout
  // short enough to catch a real fault would flag that as one, which is the
  // original bug with the sign flipped.
  it('keeps reporting running however old the last fix is', () => {
    expect(trackingState(inputs({ lastFixAt: 0 }))).toBe('running');
    expect(trackingState(inputs({ lastFixAt: Number.MIN_SAFE_INTEGER }))).toBe('running');
  });

  // A zero timestamp is a real epoch value, not "absent". Only null means
  // never. `if (!lastFixAt)` would get this wrong.
  it('treats a zero timestamp as a fix, not as the absence of one', () => {
    expect(trackingState(inputs({ lastFixAt: 0 }))).toBe('running');
    expect(trackingState(inputs({ lastFixAt: null }))).toBe('awaiting-fix');
  });
});

describe('the indicator', () => {
  const all: TrackingState[] = ['off', 'blocked', 'awaiting-fix', 'running'];

  // Green is the claim that everything works. It is the thing that was wrong,
  // so it is pinned to exactly one state.
  it('shows green only when a fix has actually been recorded', () => {
    expect(all.filter(isHealthy)).toEqual(['running']);
  });

  it('sends the user to settings only when a system setting is what is wrong', () => {
    expect(all.filter(needsUserAction)).toEqual(['blocked']);
  });

  // The label is what a person actually reads, so "location off" must never be
  // reachable as anything reassuring.
  it('labels every state distinctly', () => {
    const labels = all.map(trackingLabel);
    expect(new Set(labels).size).toBe(all.length);
    expect(trackingLabel('blocked')).toBe('location off');
    expect(trackingLabel('awaiting-fix')).not.toBe(trackingLabel('running'));
  });
});
