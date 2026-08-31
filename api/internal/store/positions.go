package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Position is one row destined for the positions table, with the vehicle
// already resolved and the server's receive time already stamped.
type Position struct {
	ReadingID  uuid.UUID
	VehicleID  uuid.UUID
	RecordedAt time.Time
	ReceivedAt time.Time

	Lat float64
	Lon float64

	SpeedMPS    *float32
	HeadingDeg  *float32
	AccuracyM   *float32
	BatteryPct  *int16
	MotionState *string
}

// InsertPositions writes a flush of readings in one statement and reports how
// many rows were new.
//
// Two things are going on in the SQL:
//
//   - The rows are passed as eleven arrays and expanded with unnest(), rather
//     than as a VALUES list with eleven placeholders per row. Postgres caps a
//     statement at 65535 parameters, so a VALUES list would put a ceiling on
//     the flush size (~5900 rows here) and would also produce a differently
//     shaped query for every batch length, defeating prepared-statement reuse.
//     With unnest the parameter count is constant no matter how many rows go in.
//
//   - ON CONFLICT (reading_id) DO NOTHING is what makes ingestion idempotent.
//     A device that retries after a timeout resends readings the server already
//     has, and this turns that into a no-op instead of a duplicate or an error.
//
// The returned count is rows actually inserted, so inserted < len(positions)
// means the difference were duplicates — which is a useful signal, not a fault.
func (s *Store) InsertPositions(ctx context.Context, positions []Position) (int, error) {
	if len(positions) == 0 {
		return 0, nil
	}

	n := len(positions)
	var (
		readingIDs   = make([]uuid.UUID, n)
		vehicleIDs   = make([]uuid.UUID, n)
		recordedAts  = make([]time.Time, n)
		receivedAts  = make([]time.Time, n)
		lats         = make([]float64, n)
		lons         = make([]float64, n)
		speeds       = make([]*float32, n)
		headings     = make([]*float32, n)
		accuracies   = make([]*float32, n)
		batteries    = make([]*int16, n)
		motionStates = make([]*string, n)
	)

	for i, p := range positions {
		readingIDs[i] = p.ReadingID
		vehicleIDs[i] = p.VehicleID
		recordedAts[i] = p.RecordedAt
		receivedAts[i] = p.ReceivedAt
		lats[i] = p.Lat
		lons[i] = p.Lon
		speeds[i] = p.SpeedMPS
		headings[i] = p.HeadingDeg
		accuracies[i] = p.AccuracyM
		batteries[i] = p.BatteryPct
		motionStates[i] = p.MotionState
	}

	tag, err := s.pool.Exec(ctx, `
		INSERT INTO positions (
			reading_id, vehicle_id, recorded_at, received_at,
			lat, lon, speed_mps, heading_deg, accuracy_m, battery_pct, motion_state
		)
		SELECT * FROM unnest(
			$1::uuid[], $2::uuid[], $3::timestamptz[], $4::timestamptz[],
			$5::float8[], $6::float8[], $7::real[], $8::real[], $9::real[],
			$10::smallint[], $11::text[]
		)
		ON CONFLICT (reading_id) DO NOTHING
	`,
		readingIDs, vehicleIDs, recordedAts, receivedAts,
		lats, lons, speeds, headings, accuracies, batteries, motionStates,
	)
	if err != nil {
		return 0, fmt.Errorf("insert %d positions: %w", n, err)
	}

	return int(tag.RowsAffected()), nil
}

// CountPositions is used by tests and by the operational endpoints.
func (s *Store) CountPositions(ctx context.Context) (int64, error) {
	var count int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM positions`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count positions: %w", err)
	}
	return count, nil
}
