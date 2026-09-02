package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/ingest"
	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/store"
	"github.com/KaiTseHuang780911/fleet-telemetry/internal/wire"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeStore struct {
	vehicleID  uuid.UUID
	vehicleErr error
	pingErr    error
	stopsErr   error

	insertedStops []store.StopEvent
}

func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

func (f *fakeStore) ListVehicles(context.Context) ([]store.Vehicle, error) {
	return []store.Vehicle{}, nil
}

func (f *fakeStore) ListTripsForVehicle(context.Context, uuid.UUID, time.Time, time.Time) ([]store.Trip, error) {
	return []store.Trip{}, nil
}

func (f *fakeStore) VehicleIDForDevice(context.Context, string) (uuid.UUID, error) {
	if f.vehicleErr != nil {
		return uuid.Nil, f.vehicleErr
	}
	return f.vehicleID, nil
}

func (f *fakeStore) ListStopEvents(context.Context, uuid.UUID, time.Time, time.Time, string) ([]store.StopEvent, error) {
	return []store.StopEvent{}, nil
}

func (f *fakeStore) InsertClientStopEvents(_ context.Context, events []store.StopEvent) (int, error) {
	if f.stopsErr != nil {
		return 0, f.stopsErr
	}
	f.insertedStops = append(f.insertedStops, events...)
	return len(events), nil
}

func (f *fakeStore) SummariseReconciliation(context.Context, time.Time, time.Time) (store.ReconciliationSummary, error) {
	return store.ReconciliationSummary{}, nil
}

type fakeWriter struct {
	accept   bool
	received [][]store.Position
}

func (f *fakeWriter) Enqueue(p []store.Position) bool {
	if !f.accept {
		return false
	}
	f.received = append(f.received, p)
	return true
}

func (f *fakeWriter) Stats() ingest.Stats { return ingest.Stats{} }

func newTestServer(t *testing.T, st *fakeStore, w *fakeWriter) http.Handler {
	t.Helper()
	if st.vehicleID == uuid.Nil && st.vehicleErr == nil {
		st.vehicleID = uuid.New()
	}
	return NewServer(st, w, discardLogger()).Routes()
}

func postTelemetry(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/telemetry", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func validReadingJSON(id string, offset time.Duration) string {
	return fmt.Sprintf(
		`{"reading_id":%q,"recorded_at":%q,"lat":49.2827,"lon":-123.1207}`,
		id, time.Now().UTC().Add(offset).Format(time.RFC3339Nano),
	)
}

func TestTelemetryAcceptsValidBatch(t *testing.T) {
	w := &fakeWriter{accept: true}
	h := newTestServer(t, &fakeStore{}, w)

	body := fmt.Sprintf(`{"device_id":"dev-1","sent_at":%q,"readings":[%s,%s]}`,
		time.Now().UTC().Format(time.RFC3339Nano),
		validReadingJSON(uuid.NewString(), -time.Minute),
		validReadingJSON(uuid.NewString(), -30*time.Second),
	)

	rec := postTelemetry(t, h, body)

	// 202 rather than 200: the readings are queued, not yet durably stored.
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body: %s", rec.Code, rec.Body.String())
	}

	var resp wire.IngestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Accepted != 2 {
		t.Errorf("Accepted = %d, want 2", resp.Accepted)
	}
	if len(resp.Rejected) != 0 {
		t.Errorf("Rejected = %v, want none", resp.Rejected)
	}
	if len(w.received) != 1 || len(w.received[0]) != 2 {
		t.Errorf("writer received %v, want one batch of 2", w.received)
	}
}

// The poison-message protection: one bad reading must not cost the other 499.
// Rejecting the whole batch would make the client retry it forever and its
// offline queue would never drain again.
func TestTelemetryAcceptsValidReadingsAndReportsRejectedOnes(t *testing.T) {
	w := &fakeWriter{accept: true}
	h := newTestServer(t, &fakeStore{}, w)

	badID := uuid.NewString()
	body := fmt.Sprintf(`{"device_id":"dev-1","readings":[%s,%s,%s]}`,
		validReadingJSON(uuid.NewString(), -time.Minute),
		// Latitude out of range.
		fmt.Sprintf(`{"reading_id":%q,"recorded_at":%q,"lat":991.0,"lon":-123.1}`,
			badID, time.Now().UTC().Format(time.RFC3339Nano)),
		validReadingJSON(uuid.NewString(), -10*time.Second),
	)

	rec := postTelemetry(t, h, body)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body: %s", rec.Code, rec.Body.String())
	}

	var resp wire.IngestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Accepted != 2 {
		t.Errorf("Accepted = %d, want 2", resp.Accepted)
	}
	if len(resp.Rejected) != 1 {
		t.Fatalf("Rejected = %v, want exactly 1", resp.Rejected)
	}
	// The client must be told *which* reading to discard, or it cannot make
	// progress.
	if resp.Rejected[0].ID != badID {
		t.Errorf("rejected reading_id = %q, want %q", resp.Rejected[0].ID, badID)
	}
	if !strings.Contains(resp.Rejected[0].Reason, "lat") {
		t.Errorf("reason = %q, want it to mention lat", resp.Rejected[0].Reason)
	}
}

// When every reading is invalid the response is still 202, not 4xx: the batch
// itself was well formed, and a 4xx would make the client retry data that can
// never be accepted.
func TestTelemetryReturnsAcceptedWhenEveryReadingIsRejected(t *testing.T) {
	w := &fakeWriter{accept: true}
	h := newTestServer(t, &fakeStore{}, w)

	body := fmt.Sprintf(`{"device_id":"dev-1","readings":[{"reading_id":%q,"recorded_at":%q,"lat":991,"lon":0}]}`,
		uuid.NewString(), time.Now().UTC().Format(time.RFC3339Nano))

	rec := postTelemetry(t, h, body)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	var resp wire.IngestResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Accepted != 0 {
		t.Errorf("Accepted = %d, want 0", resp.Accepted)
	}
	if len(resp.Rejected) != 1 {
		t.Errorf("Rejected = %v, want 1", resp.Rejected)
	}
	if len(w.received) != 0 {
		t.Error("nothing should have been enqueued")
	}
}

// The shed-load path, end to end through the handler.
func TestTelemetryShedsLoadWhenBufferIsFull(t *testing.T) {
	w := &fakeWriter{accept: false}
	h := newTestServer(t, &fakeStore{}, w)

	body := fmt.Sprintf(`{"device_id":"dev-1","readings":[%s]}`,
		validReadingJSON(uuid.NewString(), -time.Minute))

	rec := postTelemetry(t, h, body)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	// Without Retry-After the client has to guess, and guessing badly under
	// load is how a retry storm forms.
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("503 must carry a Retry-After header")
	}
}

func TestTelemetryRejectsMalformedRequests(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantErr    string
	}{
		{
			name:       "not json at all",
			body:       `this is not json`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "malformed",
		},
		{
			name:       "truncated json",
			body:       `{"device_id":"dev-1","readings":[`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "malformed",
		},
		{
			name:       "wrong type for readings",
			body:       `{"device_id":"dev-1","readings":"not-an-array"}`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "malformed",
		},
		{
			name:       "missing device id",
			body:       `{"readings":[{"reading_id":"018f3c4a-0000-7000-8000-000000000001","recorded_at":"2026-08-31T12:00:00Z","lat":1,"lon":2}]}`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "device_id",
		},
		{
			name:       "empty readings array",
			body:       `{"device_id":"dev-1","readings":[]}`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "must contain readings or stop_events",
		},
		{
			name:       "readings omitted entirely",
			body:       `{"device_id":"dev-1"}`,
			wantStatus: http.StatusBadRequest,
			wantErr:    "must contain readings or stop_events",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestServer(t, &fakeStore{}, &fakeWriter{accept: true})
			rec := postTelemetry(t, h, tt.body)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d. body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if !strings.Contains(strings.ToLower(rec.Body.String()), tt.wantErr) {
				t.Errorf("body %q should mention %q", rec.Body.String(), tt.wantErr)
			}
		})
	}
}

// A newer client sending a field this build does not know must not have its
// entire queue rejected — mobile clients update on their own schedule.
func TestTelemetryToleratesUnknownFields(t *testing.T) {
	w := &fakeWriter{accept: true}
	h := newTestServer(t, &fakeStore{}, w)

	body := fmt.Sprintf(
		`{"device_id":"dev-1","future_field":"whatever","readings":[{"reading_id":%q,"recorded_at":%q,"lat":49.2,"lon":-123.1,"altitude_m":12.5}]}`,
		uuid.NewString(), time.Now().UTC().Format(time.RFC3339Nano))

	rec := postTelemetry(t, h, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body: %s", rec.Code, rec.Body.String())
	}
}

func TestTelemetryRejectsOversizedBody(t *testing.T) {
	h := newTestServer(t, &fakeStore{}, &fakeWriter{accept: true})

	var buf bytes.Buffer
	buf.WriteString(`{"device_id":"dev-1","readings":[`)
	// Comfortably past the 4 MiB cap.
	for i := 0; i < 60000; i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(validReadingJSON(uuid.NewString(), -time.Minute))
	}
	buf.WriteString(`]}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/telemetry", &buf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413. body: %s", rec.Code, rec.Body.String())
	}
}

func TestTelemetryReturns500WhenVehicleCannotBeResolved(t *testing.T) {
	st := &fakeStore{vehicleErr: errors.New("database down")}
	h := newTestServer(t, st, &fakeWriter{accept: true})

	body := fmt.Sprintf(`{"device_id":"dev-1","readings":[%s]}`,
		validReadingJSON(uuid.NewString(), -time.Minute))

	rec := postTelemetry(t, h, body)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// The client must not learn what our database is doing.
	if strings.Contains(rec.Body.String(), "database down") {
		t.Error("internal error detail leaked to the client")
	}
}

func TestHealthzDoesNotDependOnTheDatabase(t *testing.T) {
	// Liveness must stay green while the database is unreachable, otherwise an
	// orchestrator restarts a process that is working fine and drops whatever
	// the ingest writer was holding.
	st := &fakeStore{pingErr: errors.New("database unreachable")}
	h := newTestServer(t, st, &fakeWriter{accept: true})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200 even with the database down", rec.Code)
	}
}

func TestReadyzFailsWhenTheDatabaseIsUnreachable(t *testing.T) {
	st := &fakeStore{pingErr: errors.New("database unreachable")}
	h := newTestServer(t, st, &fakeWriter{accept: true})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503 when the database is down", rec.Code)
	}
}

func TestVehicleTripsValidatesParameters(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{"valid uuid", "/v1/vehicles/" + uuid.NewString() + "/trips", http.StatusOK},
		{"not a uuid", "/v1/vehicles/not-a-uuid/trips", http.StatusBadRequest},
		{"valid range", "/v1/vehicles/" + uuid.NewString() + "/trips?from=2026-08-01T00:00:00Z&to=2026-08-02T00:00:00Z", http.StatusOK},
		{"unparseable from", "/v1/vehicles/" + uuid.NewString() + "/trips?from=yesterday", http.StatusBadRequest},
		{"inverted range", "/v1/vehicles/" + uuid.NewString() + "/trips?from=2026-08-02T00:00:00Z&to=2026-08-01T00:00:00Z", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestServer(t, &fakeStore{}, &fakeWriter{accept: true})
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d. body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// Empty result sets must encode as [] rather than null; a JSON null where a
// client expects an array is a needless source of client-side crashes.
func TestEmptyCollectionsEncodeAsArrays(t *testing.T) {
	h := newTestServer(t, &fakeStore{}, &fakeWriter{accept: true})

	req := httptest.NewRequest(http.MethodGet, "/v1/vehicles", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want []", got)
	}
}

func TestOptionalReadingFieldsReachTheWriter(t *testing.T) {
	w := &fakeWriter{accept: true}
	h := newTestServer(t, &fakeStore{}, w)

	body := fmt.Sprintf(
		`{"device_id":"dev-1","readings":[{"reading_id":%q,"recorded_at":%q,"lat":49.2,"lon":-123.1,"speed_mps":0,"battery_pct":42,"motion_state":"still"}]}`,
		uuid.NewString(), time.Now().UTC().Format(time.RFC3339Nano))

	if rec := postTelemetry(t, h, body); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body: %s", rec.Code, rec.Body.String())
	}

	got := w.received[0][0]
	if got.SpeedMPS == nil || *got.SpeedMPS != 0 {
		t.Errorf("SpeedMPS = %v, want a non-nil pointer to 0", got.SpeedMPS)
	}
	if got.BatteryPct == nil || *got.BatteryPct != 42 {
		t.Errorf("BatteryPct = %v, want 42", got.BatteryPct)
	}
	if got.MotionState == nil || *got.MotionState != wire.MotionStill {
		t.Errorf("MotionState = %v, want %q", got.MotionState, wire.MotionStill)
	}
	// Absent fields must stay nil so the column is NULL rather than 0.
	if got.HeadingDeg != nil {
		t.Errorf("HeadingDeg = %v, want nil for an omitted field", got.HeadingDeg)
	}
}
