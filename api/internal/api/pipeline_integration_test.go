package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/api"
	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/db"
	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/ingest"
	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/store"
	"github.com/KaiTseHuang780911/fleet-telemetry/internal/wire"
)

// End-to-end tests over the real pipeline: HTTP handler, ingest writer, store,
// and a live Postgres. Every other test in this package uses fakes.
//
// They exist because of a defect none of those fakes could have caught. The API
// answered 202 Accepted for three days while storing nothing: a cached
// vehicle_id had outlived the row it referred to, every insert violated its
// foreign key, and the batch was discarded after the client had already been
// told it was safe. Unit tests passed throughout, because the seam between
// "accepted" and "stored" was exactly where the fake sat.
//
// The invariant under test is therefore the one that broke, stated plainly:
//
//	**if the API answers 202, that data must reach the database.**
//
// Anything that violates it — a stale cache, a constraint nobody anticipated, a
// writer that drops on error — fails these tests regardless of cause.

func pipelineStore(t *testing.T) *store.Store {
	t.Helper()

	dsn := pipelineDSN()
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping pipeline integration tests")
	}

	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, dsn, quiet); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	s, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(s.Close)

	if err := s.TruncateAll(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

// pipelineDSN points these tests at their own database.
//
// `go test ./...` runs packages in parallel, and the store package's suite
// truncates between every test. Sharing one database means the two suites
// delete each other's rows mid-run, which surfaces as an intermittent failure
// that looks like flakiness and is not. Separate databases remove the shared
// state; `-p 1` would also work but only for whoever remembers to pass it.
//
// Derived from TEST_DATABASE_URL rather than introducing another environment
// variable, so there is one thing to configure. `npm run db:setup` creates it.
func pipelineDSN() string {
	dsn := os.Getenv("PIPELINE_TEST_DATABASE_URL")
	if dsn != "" {
		return dsn
	}
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		return ""
	}
	return strings.Replace(base, "/fleet_test", "/fleet_pipeline_test", 1)
}

// harness wires the real handler to the real writer to the real store.
type harness struct {
	server *httptest.Server
	store  *store.Store
	writer *ingest.Writer
}

func newHarness(t *testing.T, s *store.Store) *harness {
	t.Helper()

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	// A short flush interval so tests do not wait on a production-sized timer.
	writer := ingest.New(s, ingest.Config{
		BufferSize:    64,
		BatchSize:     50,
		FlushInterval: 25 * time.Millisecond,
	}, quiet)

	ctx, cancel := context.WithCancel(context.Background())
	go writer.Run(ctx)
	t.Cleanup(func() {
		cancel()
		_ = writer.Shutdown(context.Background())
	})

	server := httptest.NewServer(api.NewServer(s, writer, quiet).Routes())
	t.Cleanup(server.Close)

	return &harness{server: server, store: s, writer: writer}
}

func (h *harness) post(t *testing.T, deviceID string, n int) (int, wire.IngestResponse) {
	t.Helper()

	readings := make([]string, n)
	now := time.Now().UTC()
	for i := range readings {
		id, err := uuid.NewV7()
		if err != nil {
			t.Fatalf("uuid: %v", err)
		}
		readings[i] = fmt.Sprintf(
			`{"reading_id":%q,"recorded_at":%q,"lat":49.28,"lon":-123.12}`,
			id, now.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano))
	}
	body := fmt.Sprintf(`{"device_id":%q,"sent_at":%q,"readings":[%s]}`,
		deviceID, now.Format(time.RFC3339Nano), strings.Join(readings, ","))

	resp, err := http.Post(h.server.URL+"/v1/telemetry", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded wire.IngestResponse
	if resp.StatusCode == http.StatusAccepted {
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp.StatusCode, decoded
}

// waitForRows polls until the expected count appears or the deadline passes.
// Polling rather than sleeping a fixed duration: the flush is asynchronous, and
// a fixed sleep is either flaky or slow.
func (h *harness) waitForRows(t *testing.T, want int64) int64 {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var got int64
	for time.Now().Before(deadline) {
		var err error
		got, err = h.store.CountPositions(context.Background())
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if got >= want {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	return got
}

// The invariant, in its simplest form.
func TestAcceptedReadingsReachTheDatabase(t *testing.T) {
	s := pipelineStore(t)
	h := newHarness(t, s)

	status, resp := h.post(t, "device-e2e", 20)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", status)
	}
	if resp.Accepted != 20 {
		t.Fatalf("accepted = %d, want 20", resp.Accepted)
	}

	if got := h.waitForRows(t, 20); got != 20 {
		t.Fatalf("API accepted 20 readings but the database holds %d", got)
	}

	stats := h.writer.Stats()
	if stats.Failures != 0 {
		t.Errorf("writer reported %d flush failures", stats.Failures)
	}
}

// The incident itself, driven through HTTP rather than through the store.
//
// This is the test that would have caught the original bug. A long-running
// process keeps serving requests while the vehicles table is emptied beneath
// it; the invariant says the data must still arrive, whatever the process is
// caching internally.
func TestAcceptedReadingsStillArriveAfterTheVehiclesTableIsEmptied(t *testing.T) {
	s := pipelineStore(t)
	h := newHarness(t, s)

	if status, _ := h.post(t, "device-survivor", 10); status != http.StatusAccepted {
		t.Fatalf("first post: status %d", status)
	}
	if got := h.waitForRows(t, 10); got != 10 {
		t.Fatalf("baseline: %d rows, want 10", got)
	}

	// The rug is pulled: every vehicle row disappears while the server keeps
	// running, exactly as happened in the incident.
	if err := s.TruncateAll(context.Background()); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// The batch in flight when the cache went stale is expected to be lost —
	// that much is acknowledged in ADR-003. What must NOT happen is the process
	// staying broken afterwards.
	if status, _ := h.post(t, "device-survivor", 10); status != http.StatusAccepted {
		t.Fatalf("post during stale cache: status %d", status)
	}
	time.Sleep(300 * time.Millisecond) // let that flush fail

	// The recovery that matters: the next batch must land.
	status, resp := h.post(t, "device-survivor", 10)
	if status != http.StatusAccepted {
		t.Fatalf("post after recovery: status %d", status)
	}
	if resp.Accepted != 10 {
		t.Fatalf("accepted = %d, want 10", resp.Accepted)
	}

	if got := h.waitForRows(t, 10); got < 10 {
		t.Fatalf("the server never recovered: %d rows after a further 10 were accepted. "+
			"This is the three-day silent-loss failure returning", got)
	}
}

// Silent loss is the failure mode ADR-003 calls the worst available. If a flush
// does fail, it must at least be visible in the counters, because that is all
// an operator has to go on.
func TestFlushFailuresAreVisibleInTheReadinessCounters(t *testing.T) {
	s := pipelineStore(t)
	h := newHarness(t, s)

	if status, _ := h.post(t, "device-counters", 5); status != http.StatusAccepted {
		t.Fatal("seed post failed")
	}
	h.waitForRows(t, 5)

	if err := s.TruncateAll(context.Background()); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	before := h.writer.Stats().Failures
	if status, _ := h.post(t, "device-counters", 5); status != http.StatusAccepted {
		t.Fatal("post during stale cache failed")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.writer.Stats().Failures == before {
		time.Sleep(20 * time.Millisecond)
	}

	if h.writer.Stats().Failures == before {
		t.Fatal("a batch was discarded without the failure counter moving; " +
			"this loss would be invisible to any monitoring")
	}

	// And /readyz must expose it, or the counter helps nobody.
	resp, err := http.Get(h.server.URL + "/readyz")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var payload struct {
		Ingest ingest.Stats `json:"ingest"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode readyz: %v", err)
	}
	if payload.Ingest.Failures == 0 {
		t.Error("/readyz reports zero failures after a batch was discarded")
	}
}

// Idempotency, proven through the full stack rather than at the store.
func TestReplayedBatchesDoNotDuplicateThroughTheWholePipeline(t *testing.T) {
	s := pipelineStore(t)
	h := newHarness(t, s)

	now := time.Now().UTC()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	body := fmt.Sprintf(
		`{"device_id":"device-replay","sent_at":%q,"readings":[{"reading_id":%q,"recorded_at":%q,"lat":49.28,"lon":-123.12}]}`,
		now.Format(time.RFC3339Nano), id, now.Format(time.RFC3339Nano))

	for i := 0; i < 5; i++ {
		resp, err := http.Post(h.server.URL+"/v1/telemetry", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("post %d: status %d", i, resp.StatusCode)
		}
	}

	h.waitForRows(t, 1)
	time.Sleep(200 * time.Millisecond) // give any duplicate a chance to appear

	got, err := s.CountPositions(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 1 {
		t.Fatalf("five identical posts produced %d rows, want 1", got)
	}
}
