package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func ptr[T any](v T) *T { return &v }

// A fixed reference time so nothing here depends on when the suite runs.
var testNow = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

func validReading() Reading {
	return Reading{
		ReadingID:  uuid.MustParse("018f3c4a-0000-7000-8000-000000000001"),
		RecordedAt: testNow.Add(-time.Minute),
		Lat:        49.2827,
		Lon:        -123.1207,
	}
}

func TestReadingValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Reading)
		wantErr string // substring; empty means the reading must be accepted
	}{
		{
			name:   "minimal valid reading",
			mutate: func(r *Reading) {},
		},
		{
			name: "all optional fields populated",
			mutate: func(r *Reading) {
				r.SpeedMPS = ptr(float32(12.5))
				r.HeadingDeg = ptr(float32(180))
				r.AccuracyM = ptr(float32(5))
				r.BatteryPct = ptr(int16(80))
				r.MotionState = ptr(MotionDriving)
			},
		},
		{
			// The offline queue exists to deliver these. Rejecting old readings
			// would break the feature the whole project is built around.
			name: "hours-old reading from a drained offline queue is accepted",
			mutate: func(r *Reading) {
				r.RecordedAt = testNow.Add(-8 * time.Hour)
			},
		},
		{
			name: "reading just inside the future skew allowance",
			mutate: func(r *Reading) {
				r.RecordedAt = testNow.Add(MaxClockSkewAhead - time.Minute)
			},
		},
		{
			name: "reading beyond the future skew allowance",
			mutate: func(r *Reading) {
				r.RecordedAt = testNow.Add(MaxClockSkewAhead + time.Minute)
			},
			wantErr: "future",
		},
		{
			name:    "missing reading id",
			mutate:  func(r *Reading) { r.ReadingID = uuid.Nil },
			wantErr: "reading_id",
		},
		{
			name:    "missing recorded_at",
			mutate:  func(r *Reading) { r.RecordedAt = time.Time{} },
			wantErr: "recorded_at",
		},
		{
			name:    "latitude above range",
			mutate:  func(r *Reading) { r.Lat = 90.1 },
			wantErr: "lat",
		},
		{
			name:    "latitude below range",
			mutate:  func(r *Reading) { r.Lat = -90.1 },
			wantErr: "lat",
		},
		{
			name:   "latitude exactly at the pole is valid",
			mutate: func(r *Reading) { r.Lat = 90 },
		},
		{
			name:    "longitude out of range",
			mutate:  func(r *Reading) { r.Lon = 180.5 },
			wantErr: "lon",
		},
		{
			name:   "longitude exactly at the antimeridian is valid",
			mutate: func(r *Reading) { r.Lon = -180 },
		},
		{
			name:    "negative speed",
			mutate:  func(r *Reading) { r.SpeedMPS = ptr(float32(-1)) },
			wantErr: "speed_mps",
		},
		{
			name:   "zero speed is valid and distinct from absent",
			mutate: func(r *Reading) { r.SpeedMPS = ptr(float32(0)) },
		},
		{
			name:    "heading beyond 360",
			mutate:  func(r *Reading) { r.HeadingDeg = ptr(float32(361)) },
			wantErr: "heading_deg",
		},
		{
			name:    "battery above 100",
			mutate:  func(r *Reading) { r.BatteryPct = ptr(int16(101)) },
			wantErr: "battery_pct",
		},
		{
			name:    "battery below zero",
			mutate:  func(r *Reading) { r.BatteryPct = ptr(int16(-1)) },
			wantErr: "battery_pct",
		},
		{
			name:    "unrecognised motion state",
			mutate:  func(r *Reading) { r.MotionState = ptr("teleporting") },
			wantErr: "motion_state",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validReading()
			tt.mutate(&r)

			err := r.Validate(testNow)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected reading to be accepted, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected rejection containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

// NaN and Inf survive some JSON producers and decode without complaint, but
// cannot be stored as a Postgres double precision. If they reach the batch
// writer they abort the insert for every other reading in the flush.
func TestReadingValidateRejectsNonFiniteCoordinates(t *testing.T) {
	inf := 1.0
	for i := 0; i < 400; i++ {
		inf *= 10 // overflows to +Inf without importing math
	}

	tests := []struct {
		name string
		lat  float64
		lon  float64
	}{
		{"infinite latitude", inf, -123.1},
		{"infinite longitude", 49.2, inf},
		{"negative infinite latitude", -inf, -123.1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validReading()
			r.Lat, r.Lon = tt.lat, tt.lon
			if err := r.Validate(testNow); err == nil {
				t.Fatal("expected non-finite coordinates to be rejected")
			}
		})
	}
}

func TestBatchValidate(t *testing.T) {
	tests := []struct {
		name    string
		batch   Batch
		wantErr string
	}{
		{
			name:  "valid batch",
			batch: Batch{DeviceID: "device-1", Readings: []Reading{validReading()}},
		},
		{
			name:    "missing device id",
			batch:   Batch{Readings: []Reading{validReading()}},
			wantErr: "device_id",
		},
		{
			name:    "no readings and no stop events",
			batch:   Batch{DeviceID: "device-1", Readings: []Reading{}},
			wantErr: "must contain readings or stop_events",
		},
		{
			name:    "nothing at all",
			batch:   Batch{DeviceID: "device-1"},
			wantErr: "must contain readings or stop_events",
		},
		{
			// A device with only a departure to report should not have to
			// invent a position to be allowed to send it.
			name: "stop events with no readings is valid",
			batch: Batch{
				DeviceID:   "device-1",
				StopEvents: []StopEvent{validStopEvent()},
			},
		},
		{
			name: "too many stop events",
			batch: Batch{
				DeviceID:   "device-1",
				Readings:   []Reading{validReading()},
				StopEvents: make([]StopEvent, MaxStopEventsPerBatch+1),
			},
			wantErr: "limit",
		},
		{
			name: "batch over the size limit",
			batch: Batch{
				DeviceID: "device-1",
				Readings: make([]Reading, MaxReadingsPerBatch+1),
			},
			wantErr: "limit",
		},
		{
			name: "batch exactly at the size limit",
			batch: Batch{
				DeviceID: "device-1",
				Readings: make([]Reading, MaxReadingsPerBatch),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.batch.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected batch to be accepted, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

// The pointer-vs-value distinction is the whole reason optional fields are
// pointers, so it is worth asserting rather than assuming.
func TestOptionalFieldsDistinguishAbsentFromZero(t *testing.T) {
	var absent, zero Reading

	if err := json.Unmarshal([]byte(`{"lat":1,"lon":2}`), &absent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"lat":1,"lon":2,"speed_mps":0}`), &zero); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if absent.SpeedMPS != nil {
		t.Error("omitted speed_mps should decode to nil, not a zero value")
	}
	if zero.SpeedMPS == nil {
		t.Fatal("explicit speed_mps of 0 should decode to a non-nil pointer")
	}
	if *zero.SpeedMPS != 0 {
		t.Errorf("expected 0, got %v", *zero.SpeedMPS)
	}
}

func validStopEvent() StopEvent {
	return StopEvent{
		EventID:   uuid.MustParse("018f3c4a-0000-7000-8000-0000000000aa"),
		ArrivedAt: testNow.Add(-10 * time.Minute),
		Lat:       49.2827,
		Lon:       -123.1207,
	}
}

func TestStopEventValidate(t *testing.T) {
	departed := testNow.Add(-5 * time.Minute)

	tests := []struct {
		name    string
		mutate  func(*StopEvent)
		wantErr string
	}{
		{
			name:   "minimal valid stop event",
			mutate: func(e *StopEvent) {},
		},
		{
			// Normal, not an error: the device reports arrival immediately and
			// departure later, so an open stop is the expected intermediate state.
			name:   "open stop with no departure",
			mutate: func(e *StopEvent) { e.DepartedAt = nil },
		},
		{
			name:   "closed stop",
			mutate: func(e *StopEvent) { e.DepartedAt = &departed },
		},
		{
			name:    "missing event id",
			mutate:  func(e *StopEvent) { e.EventID = uuid.Nil },
			wantErr: "event_id",
		},
		{
			name:    "missing arrival",
			mutate:  func(e *StopEvent) { e.ArrivedAt = time.Time{} },
			wantErr: "arrived_at",
		},
		{
			name: "departure before arrival",
			mutate: func(e *StopEvent) {
				before := e.ArrivedAt.Add(-time.Minute)
				e.DepartedAt = &before
			},
			wantErr: "before arrived_at",
		},
		{
			// Hours-old stops are exactly what a drained offline queue delivers.
			name:   "stop from hours ago is accepted",
			mutate: func(e *StopEvent) { e.ArrivedAt = testNow.Add(-9 * time.Hour) },
		},
		{
			name:    "arrival beyond the future skew allowance",
			mutate:  func(e *StopEvent) { e.ArrivedAt = testNow.Add(MaxClockSkewAhead + time.Hour) },
			wantErr: "future",
		},
		{
			name:    "latitude out of range",
			mutate:  func(e *StopEvent) { e.Lat = 91 },
			wantErr: "lat",
		},
		{
			name:    "longitude out of range",
			mutate:  func(e *StopEvent) { e.Lon = -181 },
			wantErr: "lon",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validStopEvent()
			tt.mutate(&e)

			err := e.Validate(testNow)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected acceptance, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}
