import { shouldAutoDrain, type AutoDrainState, type DrainTrigger } from './autodrain';

const base: AutoDrainState = {
  online: true,
  active: true,
  queueDepth: 10,
  draining: false,
  nextAttemptAt: null,
  now: 1_000_000,
};

const allTriggers: DrainTrigger[] = ['reconnect', 'periodic', 'foreground', 'manual'];

describe('shouldAutoDrain', () => {
  it.each(allTriggers)('%s drains when there is work and nothing blocking', (trigger) => {
    expect(shouldAutoDrain(trigger, base)).toBe(true);
  });

  it.each(allTriggers)('%s does nothing when the queue is empty', (trigger) => {
    expect(shouldAutoDrain(trigger, { ...base, queueDepth: 0 })).toBe(false);
  });

  // Concurrent drains send the same rows twice. Harmless server-side thanks to
  // idempotency, but it wastes the radio, which is the scarce resource here.
  it.each(allTriggers)('%s refuses while a drain is already running', (trigger) => {
    expect(shouldAutoDrain(trigger, { ...base, draining: true })).toBe(false);
  });

  // A server that just asked us to wait does not become ready because the user
  // opened the app.
  it.each(allTriggers)('%s respects an active backoff', (trigger) => {
    expect(shouldAutoDrain(trigger, { ...base, nextAttemptAt: base.now + 5000 })).toBe(false);
  });

  it.each(allTriggers)('%s proceeds once the backoff has elapsed', (trigger) => {
    expect(shouldAutoDrain(trigger, { ...base, nextAttemptAt: base.now })).toBe(true);
  });

  describe('connectivity', () => {
    // The periodic trigger fires repeatedly, so a wrong "online" flag would
    // mean a pointless radio wake-up every interval.
    it('periodic waits for connectivity', () => {
      expect(shouldAutoDrain('periodic', { ...base, online: false })).toBe(false);
    });

    it('periodic only runs while the app is in the foreground', () => {
      expect(shouldAutoDrain('periodic', { ...base, active: false })).toBe(false);
    });

    // These two are edge events meaning "something just changed". NetInfo is
    // not reliable about reachability — a captive portal reports connected, and
    // a network switch reports disconnected for a moment — so refusing to try
    // would skip the moment most likely to succeed. A wrong guess costs one
    // failed request, which the queue absorbs.
    it.each<DrainTrigger>(['reconnect', 'foreground'])(
      '%s tries even when connectivity is reported as down',
      (trigger) => {
        expect(shouldAutoDrain(trigger, { ...base, online: false })).toBe(true);
      },
    );

    it('manual always tries, because the user asked', () => {
      expect(shouldAutoDrain('manual', { ...base, online: false, active: false })).toBe(true);
    });
  });

  // The regression this module exists for: a queue filled while offline must
  // not sit forever once tracking stops. Coming back into coverage has to be
  // enough on its own.
  it('drains a queue stranded after tracking stopped, on reconnect alone', () => {
    const stranded: AutoDrainState = {
      ...base,
      online: true,
      active: true,
      queueDepth: 240, // forty minutes of fixes at ten-second intervals
      nextAttemptAt: null,
    };
    expect(shouldAutoDrain('reconnect', stranded)).toBe(true);
  });
});
