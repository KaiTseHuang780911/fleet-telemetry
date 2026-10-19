import { pickActiveVehicle } from './vehicle';

function v(id: string, lastReportedAt: string | null) {
  return { id, last_reported_at: lastReportedAt };
}

describe('pickActiveVehicle', () => {
  // The bug this exists to prevent. The list used to be ordered by creation
  // date, so taking the first row picked the *oldest* vehicle — exactly wrong
  // after a device re-registers, because the stale vehicle wins and the live
  // one is invisible. A week of drives went missing that way.
  it('picks the most recently active, not the first in the list', () => {
    const picked = pickActiveVehicle([
      v('stale', '2026-10-02T10:00:00Z'),
      v('live', '2026-10-18T19:00:00Z'),
    ]);

    expect(picked?.id).toBe('live');
  });

  it('picks the same one regardless of the order it arrives in', () => {
    const stale = v('stale', '2026-10-02T10:00:00Z');
    const live = v('live', '2026-10-18T19:00:00Z');

    expect(pickActiveVehicle([stale, live])?.id).toBe('live');
    expect(pickActiveVehicle([live, stale])?.id).toBe('live');
  });

  // A vehicle that has never reported has nothing to draw, so it loses to any
  // that has — whichever order they come in.
  it('prefers a vehicle that has reported over one that never has', () => {
    expect(pickActiveVehicle([v('silent', null), v('active', '2026-10-18T19:00:00Z')])?.id).toBe(
      'active',
    );
    expect(pickActiveVehicle([v('active', '2026-10-18T19:00:00Z'), v('silent', null)])?.id).toBe(
      'active',
    );
  });

  // But it should still be shown if it is all there is, rather than the screen
  // claiming no vehicle exists.
  it('falls back to a silent vehicle when nothing else is on offer', () => {
    expect(pickActiveVehicle([v('silent', null)])?.id).toBe('silent');
  });

  it('returns undefined when there are no vehicles', () => {
    expect(pickActiveVehicle([])).toBeUndefined();
  });

  it('handles every vehicle being silent', () => {
    expect(pickActiveVehicle([v('a', null), v('b', null)])?.id).toBe('a');
  });

  // ISO 8601 in UTC sorts correctly as a string, which is why this compares
  // without parsing. A mixed-offset format would break that, so the server
  // returning anything other than UTC instants would need this revisited.
  it('compares timestamps correctly across a month boundary', () => {
    const picked = pickActiveVehicle([
      v('sep', '2026-09-30T23:59:59Z'),
      v('oct', '2026-10-01T00:00:01Z'),
    ]);

    expect(picked?.id).toBe('oct');
  });
});
