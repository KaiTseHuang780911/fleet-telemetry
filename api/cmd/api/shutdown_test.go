package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestGracefulShutdownDrainsBufferedReadings is the end-to-end proof that a
// SIGTERM does not lose acknowledged readings.
//
// The writer's drain is unit-tested in the ingest package, but that test calls
// Shutdown directly. This one exercises the wiring the unit test cannot reach:
// signal delivery, the HTTP-then-writer shutdown ordering, and the flush
// running on a context that is not already cancelled.
//
// It has to run on Linux. Windows has no SIGTERM — os.Process.Signal cannot
// deliver one — which is exactly why this path went unverified during
// development and why CI exists.
func TestGracefulShutdownDrainsBufferedReadings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot deliver SIGTERM; this runs on Linux CI")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	binary := buildAPI(t)
	port := freePort(t)
	deviceID := "shutdown-test-" + uuid.NewString()

	// Thresholds high enough that nothing can flush on its own: if the rows
	// reach Postgres, the drain is the only thing that could have put them
	// there. Without this the test would pass even with shutdown broken.
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+dsn,
		"PORT="+port,
		"INGEST_BATCH_SIZE=100000",
		"INGEST_FLUSH_MS=600000",
		"SHUTDOWN_TIMEOUT_MS=15000",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start api: %v", err)
	}
	// Make sure a failing test never leaves the process behind.
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	baseURL := "http://127.0.0.1:" + port
	waitForHealthz(t, baseURL)

	const readings = 25
	ids := postBatch(t, baseURL, deviceID, readings)

	// Confirm the readings really are still buffered, so the assertion after
	// shutdown means what it claims.
	if n := countReadings(t, dsn, ids); n != 0 {
		t.Fatalf("%d readings reached the database before shutdown; the test cannot prove anything", n)
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("send signal: %v", err)
	}

	waitForExit(t, cmd, 20*time.Second)

	if n := countReadings(t, dsn, ids); n != readings {
		t.Fatalf("after graceful shutdown %d of %d readings were persisted; the rest were lost", n, readings)
	}
}

// TestShutdownIsGracefulNotAbrupt guards the ordering: HTTP must stop first, so
// nothing new enters the buffer while it is being drained.
func TestShutdownRefusesNewWorkWhileDraining(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot deliver SIGTERM; this runs on Linux CI")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	binary := buildAPI(t)
	port := freePort(t)

	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+dsn,
		"PORT="+port,
		"SHUTDOWN_TIMEOUT_MS=10000",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start api: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	baseURL := "http://127.0.0.1:" + port
	waitForHealthz(t, baseURL)

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("send signal: %v", err)
	}
	waitForExit(t, cmd, 20*time.Second)

	// The listener must be gone once the process has exited.
	client := &http.Client{Timeout: 2 * time.Second}
	if resp, err := client.Get(baseURL + "/healthz"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("server still accepting connections after shutdown completed")
	}
}

func buildAPI(t *testing.T) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "api-under-test")
	// Build from the package directory so the test works regardless of where
	// `go test` was invoked from.
	cmd := exec.Command("go", "build", "-o", binary, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build api: %v\n%s", err, out)
	}
	return binary
}

// freePort asks the kernel for an unused port and immediately releases it.
// A hard-coded port would collide with a developer's running server or with a
// parallel job on the same CI runner.
func freePort(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer func() { _ = l.Close() }()

	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("parse reserved address: %v", err)
	}
	return port
}

func waitForHealthz(t *testing.T, baseURL string) {
	t.Helper()

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("api did not become healthy in time")
}

func postBatch(t *testing.T, baseURL, deviceID string, n int) []uuid.UUID {
	t.Helper()

	ids := make([]uuid.UUID, n)
	parts := make([]string, n)
	now := time.Now().UTC()

	for i := range ids {
		id, err := uuid.NewV7()
		if err != nil {
			t.Fatalf("generate uuid: %v", err)
		}
		ids[i] = id
		parts[i] = fmt.Sprintf(
			`{"reading_id":%q,"recorded_at":%q,"lat":49.28,"lon":-123.12}`,
			id, now.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano))
	}

	body := fmt.Sprintf(`{"device_id":%q,"sent_at":%q,"readings":[%s]}`,
		deviceID, now.Format(time.RFC3339Nano), strings.Join(parts, ","))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(baseURL+"/v1/telemetry", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post telemetry: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("post returned %d, want 202", resp.StatusCode)
	}
	return ids
}

func countReadings(t *testing.T, dsn string, ids []uuid.UUID) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var count int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM positions WHERE reading_id = ANY($1)`, ids).Scan(&count); err != nil {
		t.Fatalf("count readings: %v", err)
	}
	return count
}

func waitForExit(t *testing.T, cmd *exec.Cmd, within time.Duration) {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("process exited with error: %v", err)
		}
	case <-time.After(within):
		t.Fatalf("process did not exit within %s of the signal", within)
	}
}
