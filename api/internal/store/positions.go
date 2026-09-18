package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrStaleVehicleCache reports that an insert referenced a vehicle row that no
// longer exists, so the cached mapping was wrong and has been dropped.
//
// Surfaced as a distinct error because the caller's response differs: this one
// is self-healing on the next batch, whereas a connection failure is not.
var ErrStaleVehicleCache = errors.New("vehicle cache was stale; it has been invalidated")

// Position is one row destined for the positions table, with the vehicle
// already resolved and the server's receive time already stamped.
//
// The json tags matter because this type is now returned by the read API as
// well as written by the ingest path, and every other response in this service
// is snake_case. Optional fields are omitempty for the same reason they are
// pointers: absent and zero are different facts, and a speed of 0 that appears
// only because the device never reported one would corrupt any idle-time
// aggregate built on top of it.
type Position struct {
	ReadingID  uuid.UUID `json:"reading_id"`
	VehicleID  uuid.UUID `json:"vehicle_id"`
	RecordedAt time.Time `json:"recorded_at"`
	ReceivedAt time.Time `json:"received_at"`

	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`

	SpeedMPS    *float32 `json:"speed_mps,omitempty"`
	HeadingDeg  *float32 `json:"heading_deg,omitempty"`
	AccuracyM   *float32 `json:"accuracy_m,omitempty"`
	BatteryPct  *int16   `json:"battery_pct,omitempty"`
	MotionState *string  `json:"motion_state,omitempty"`
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
		// A foreign-key violation here means the vehicle_id these rows carry no
		// longer exists, which can only happen if the row was removed after the
		// id was cached. Dropping the cache lets the next batch re-resolve and
		// succeed instead of failing identically forever.
		//
		// 23503 is Postgres' foreign_key_violation. Matching on the code rather
		// than the message text because messages are localised and change
		// between versions.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			s.InvalidateVehicleCache()
			return 0, fmt.Errorf("insert %d positions: %w: %v", n, ErrStaleVehicleCache, err)
		}
		return 0, fmt.Errorf("insert %d positions: %w", n, err)
	}

	return int(tag.RowsAffected()), nil
}

// CountPositions is used by tests and by the operational endpoints.
// MaxPositionsPerQuery bounds one read.
//
// A vehicle reporting every ten seconds produces ~8,600 rows a day, so an
// unqualified day-long window is already large enough to matter for a phone
// parsing it. The cap is applied in SQL rather than trusted to the caller, and
// the response says whether it bit.
const MaxPositionsPerQuery = 5000

// PositionPage is a bounded slice of a vehicle's position history.
type PositionPage struct {
	Positions []Position `json:"positions"`
	// Truncated reports that the window held more rows than the cap allowed.
	// Surfaced rather than silently dropped: a route drawn from a truncated
	// stream looks like a route that simply ended, which is indistinguishable
	// from a device that stopped reporting.
	Truncated bool `json:"truncated"`
}

// ListPositions returns a vehicle's positions within [from, to), oldest first.
//
// Ordered ascending because every consumer wants it that way — a route is drawn
// forwards, and the derivation pass folds samples in time order. The composite
// index this rides on is (vehicle_id, recorded_at DESC), which serves either
// direction equally; ADR-001 created it for exactly this query, and until now
// nothing had used it.
//
// recorded_at, not received_at: the caller is asking when the vehicle was
// somewhere, not when the server heard about it. Those differ by hours whenever
// the offline queue drains a backlog.
func (s *Store) ListPositions(
	ctx context.Context,
	vehicleID uuid.UUID,
	from, to time.Time,
	limit int,
) (PositionPage, error) {
	if limit <= 0 || limit > MaxPositionsPerQuery {
		limit = MaxPositionsPerQuery
	}

	// One more than asked for, so "there were more" is answerable without a
	// second count query over the same range.
	rows, err := s.pool.Query(ctx, `
		SELECT reading_id, vehicle_id, recorded_at, received_at,
		       lat, lon, speed_mps, heading_deg, accuracy_m, battery_pct, motion_state
		  FROM positions
		 WHERE vehicle_id = $1
		   AND recorded_at >= $2
		   AND recorded_at < $3
		 ORDER BY recorded_at
		 LIMIT $4
	`, vehicleID, from, to, limit+1)
	if err != nil {
		return PositionPage{}, fmt.Errorf("query positions for %s: %w", vehicleID, err)
	}
	defer rows.Close()

	page := PositionPage{Positions: make([]Position, 0, limit)}
	for rows.Next() {
		var p Position
		if err := rows.Scan(
			&p.ReadingID, &p.VehicleID, &p.RecordedAt, &p.ReceivedAt,
			&p.Lat, &p.Lon, &p.SpeedMPS, &p.HeadingDeg, &p.AccuracyM,
			&p.BatteryPct, &p.MotionState,
		); err != nil {
			return PositionPage{}, fmt.Errorf("scan position: %w", err)
		}
		page.Positions = append(page.Positions, p)
	}
	if err := rows.Err(); err != nil {
		return PositionPage{}, fmt.Errorf("iterate positions: %w", err)
	}

	if len(page.Positions) > limit {
		page.Positions = page.Positions[:limit]
		page.Truncated = true
	}
	return page, nil
}

func (s *Store) CountPositions(ctx context.Context) (int64, error) {
	var count int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM positions`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count positions: %w", err)
	}
	return count, nil
}
