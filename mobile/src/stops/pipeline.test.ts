jest.mock('react-native-get-random-values', () => ({}));
jest.mock('uuid', () => {
  let seq = 0;
  return { v7: () => `row-${++seq}` };
});

import { makeStopItem } from '../telemetry/readings';
import { INITIAL_STOP_STATE, detectStops, type Fix, type StopState } from './detect';

/**
 * The client chain end to end: detector -> emission -> outbox item -> payload.
 *
 * The unit tests either side of this were both green while the device reported
 * nine arrivals and zero departures, because each covered one link. This covers
 * the joins, driving the detector with the mock route's own motion model — one
 * fix per call, state round-tripped through JSON, exactly as the background
 * task does it.
 *
 * It is also the only test `readings.ts` has ever had. `uuid` ships as ESM and
 * Jest cannot transform it, so anything importing that module failed to load
 * outright; mocking it here is what makes the wire payload assertable at all.
 */

const ORIGIN = { lat: 49.2827, lon: -123.1207 };
const EARTH_RADIUS_M = 6_371_000;
const TICK_SIM_MS = 20_000;
const CRUISE = 11;

function move(lat: number, lon: number, headingDeg: number, metres: number) {
  const angular = metres / EARTH_RADIUS_M;
  const bearing = (headingDeg * Math.PI) / 180;
  const lat1 = (lat * Math.PI) / 180;
  const lon1 = (lon * Math.PI) / 180;
  const sinLat2 =
    Math.sin(lat1) * Math.cos(angular) + Math.cos(lat1) * Math.sin(angular) * Math.cos(bearing);
  const lat2 = Math.asin(sinLat2);
  const y = Math.sin(bearing) * Math.sin(angular) * Math.cos(lat1);
  const x = Math.cos(angular) - Math.sin(lat1) * sinLat2;
  const lon2 = lon1 + Math.atan2(y, x);
  return { lat: (lat2 * 180) / Math.PI, lon: ((((lon2 * 180) / Math.PI + 540) % 360) - 180) };
}

it('reports one stop as an open arrival then a completion under the same id', () => {
  let { lat, lon } = ORIGIN;
  let at = 1_760_000_000_000;

  const script: Array<'drive' | 'stop'> = [
    ...Array(3).fill('drive'),
    ...Array(8).fill('stop'),
    // The mock does not move on the tick dwell expires: speed flips to cruise
    // while the position stays put, so the first moving fix is still inside the
    // radius. Reproduced here deliberately.
    ...Array(3).fill('drive'),
  ];

  let state: StopState = INITIAL_STOP_STATE;
  const payloads: Array<Record<string, unknown>> = [];
  let n = 0;

  for (const step of script) {
    if (step === 'drive') {
      const next = move(lat, lon, 45, CRUISE * (TICK_SIM_MS / 1000));
      lat = next.lat;
      lon = next.lon;
    }
    at += TICK_SIM_MS;

    const fix: Fix = {
      lat,
      lon,
      at,
      speedMps: step === 'stop' ? 0 : CRUISE,
      accuracyM: 10,
    };

    const result = detectStops(state, [fix], () => `e${++n}`);
    state = JSON.parse(JSON.stringify(result.state));

    // Exactly the mapping task.ts performs.
    for (const event of result.emissions) {
      const item = makeStopItem({
        eventId: event.eventId,
        arrivedAt: new Date(event.arrivedAt),
        departedAt: event.kind === 'departed' ? new Date(event.departedAt) : undefined,
        lat: event.lat,
        lon: event.lon,
      });
      payloads.push(item.payload as Record<string, unknown>);
    }
  }

  // eslint-disable-next-line no-console
  console.log(JSON.stringify(payloads, null, 2));

  expect(payloads).toHaveLength(2);
  expect(payloads[0]).not.toHaveProperty('departed_at');
  expect(payloads[1]).toHaveProperty('departed_at');
  // Both reports must name the same stop, or the server creates two rows.
  expect(payloads[1]?.event_id).toBe(payloads[0]?.event_id);
});
