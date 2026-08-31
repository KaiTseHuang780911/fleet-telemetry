package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeStore records every flush and can be told to fail or to report
// duplicates, so the writer's behaviour is testable without a database.
type fakeStore struct {
	mu       sync.Mutex
	flushes  [][]store.Position
	notify   chan struct{}
	err      error
	inserted func(n int) int // maps rows offered to rows reported inserted
}

func newFakeStore() *fakeStore {
	return &fakeStore{notify: make(chan struct{}, 64)}
}

func (f *fakeStore) InsertPositions(_ context.Context, positions []store.Position) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return 0, f.err
	}

	// The writer reuses its backing array between flushes, so the slice handed
	// in is only valid for the duration of the call. Copying is what makes
	// later assertions meaningful rather than racy.
	cp := make([]store.Position, len(positions))
	copy(cp, positions)
	f.flushes = append(f.flushes, cp)

	select {
	case f.notify <- struct{}{}:
	default:
	}

	if f.inserted != nil {
		return f.inserted(len(positions)), nil
	}
	return len(positions), nil
}

func (f *fakeStore) flushCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.flushes)
}

func (f *fakeStore) totalRows() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, fl := range f.flushes {
		n += len(fl)
	}
	return n
}

// waitForFlushes blocks until the fake has recorded at least n flushes, or
// fails the test. Polling a condition rather than sleeping a fixed duration
// keeps the suite fast and removes the usual source of flakiness.
func (f *fakeStore) waitForFlushes(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if f.flushCount() >= n {
			return
		}
		select {
		case <-f.notify:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %d flushes, saw %d", n, f.flushCount())
		}
	}
}

func makePositions(n int) []store.Position {
	out := make([]store.Position, n)
	for i := range out {
		out[i] = store.Position{
			ReadingID:  uuid.New(),
			VehicleID:  uuid.Nil,
			RecordedAt: time.Now().UTC(),
			ReceivedAt: time.Now().UTC(),
			Lat:        49.28,
			Lon:        -123.12,
		}
	}
	return out
}

// The backpressure path, which is the decision ADR-003 exists to justify.
//
// The writer goroutine is deliberately never started: with nothing draining it,
// the buffer fills deterministically and the test does not depend on timing.
func TestEnqueueShedsLoadWhenBufferIsFull(t *testing.T) {
	w := New(newFakeStore(), Config{BufferSize: 2, BatchSize: 100}, discardLogger())

	if !w.Enqueue(makePositions(1)) {
		t.Fatal("first enqueue should be accepted into an empty buffer")
	}
	if !w.Enqueue(makePositions(1)) {
		t.Fatal("second enqueue should fill the buffer but still be accepted")
	}

	if w.Enqueue(makePositions(5)) {
		t.Fatal("enqueue past buffer capacity must be refused, not blocked")
	}

	stats := w.Stats()
	if stats.Enqueued != 2 {
		t.Errorf("Enqueued = %d, want 2", stats.Enqueued)
	}
	// The count is readings, not requests: a refused batch of 5 sheds 5.
	if stats.Shed != 5 {
		t.Errorf("Shed = %d, want 5", stats.Shed)
	}
}

// Enqueue must never block, even under sustained overload — a blocking send
// would tie up an HTTP goroutine per stalled request and cascade into
// connection exhaustion, which is precisely what shedding avoids.
func TestEnqueueNeverBlocks(t *testing.T) {
	w := New(newFakeStore(), Config{BufferSize: 1, BatchSize: 100}, discardLogger())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			w.Enqueue(makePositions(1))
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Enqueue blocked; it must always return immediately")
	}

	if w.Stats().Shed == 0 {
		t.Error("expected some readings to be shed with a buffer of 1")
	}
}

func TestFlushesWhenBatchSizeReached(t *testing.T) {
	fake := newFakeStore()
	// A long flush interval so that only the size threshold can trigger this.
	w := New(fake, Config{BufferSize: 16, BatchSize: 10, FlushInterval: time.Hour}, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	if !w.Enqueue(makePositions(10)) {
		t.Fatal("enqueue rejected")
	}
	fake.waitForFlushes(t, 1)

	if got := fake.totalRows(); got != 10 {
		t.Errorf("flushed %d rows, want 10", got)
	}
}

func TestFlushesOnTimerWhenBatchIsIncomplete(t *testing.T) {
	fake := newFakeStore()
	// Batch size far above what is enqueued, so only the timer can flush it.
	w := New(fake, Config{
		BufferSize:    16,
		BatchSize:     1000,
		FlushInterval: 20 * time.Millisecond,
	}, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	if !w.Enqueue(makePositions(3)) {
		t.Fatal("enqueue rejected")
	}
	fake.waitForFlushes(t, 1)

	if got := fake.totalRows(); got != 3 {
		t.Errorf("flushed %d rows, want 3", got)
	}
}

// A large submission must be split into batch-sized flushes rather than sent as
// one oversized statement.
func TestLargeSubmissionIsSplitIntoBatches(t *testing.T) {
	fake := newFakeStore()
	w := New(fake, Config{BufferSize: 8, BatchSize: 10, FlushInterval: 20 * time.Millisecond}, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	if !w.Enqueue(makePositions(25)) {
		t.Fatal("enqueue rejected")
	}
	fake.waitForFlushes(t, 3) // 10 + 10, then 5 on the timer

	if got := fake.totalRows(); got != 25 {
		t.Errorf("flushed %d rows total, want 25", got)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	for i, fl := range fake.flushes {
		if len(fl) > 10 {
			t.Errorf("flush %d had %d rows, exceeding the batch size of 10", i, len(fl))
		}
	}
}

// Shutdown must not lose buffered readings — that is the entire point of a
// graceful drain, and the path that has been untested until now.
func TestShutdownDrainsBufferedReadings(t *testing.T) {
	fake := newFakeStore()
	w := New(fake, Config{
		BufferSize:    16,
		BatchSize:     1000,      // never reached
		FlushInterval: time.Hour, // never fires
	}, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	for i := 0; i < 4; i++ {
		if !w.Enqueue(makePositions(5)) {
			t.Fatalf("enqueue %d rejected", i)
		}
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	if err := w.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if got := fake.totalRows(); got != 20 {
		t.Errorf("drained %d rows, want 20 — readings were lost on shutdown", got)
	}
}

func TestEnqueueAfterShutdownIsRefused(t *testing.T) {
	w := New(newFakeStore(), Config{BufferSize: 4, BatchSize: 10}, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Must refuse rather than panic on a send to a closed channel.
	if w.Enqueue(makePositions(1)) {
		t.Error("enqueue after shutdown should be refused")
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	w := New(newFakeStore(), Config{BufferSize: 4, BatchSize: 10}, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	for i := 0; i < 3; i++ {
		if err := w.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown call %d: %v", i, err)
		}
	}
}

// Rows the database already had are counted as duplicates, not as failures.
// This is the normal, expected outcome of a client retrying a batch.
func TestDuplicateRowsAreCountedSeparately(t *testing.T) {
	fake := newFakeStore()
	fake.inserted = func(n int) int { return n - 3 } // 3 of every flush already existed

	w := New(fake, Config{BufferSize: 8, BatchSize: 10, FlushInterval: time.Hour}, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	if !w.Enqueue(makePositions(10)) {
		t.Fatal("enqueue rejected")
	}
	fake.waitForFlushes(t, 1)

	// Shutdown first so the counters are guaranteed to be settled.
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	stats := w.Stats()
	if stats.Inserted != 7 {
		t.Errorf("Inserted = %d, want 7", stats.Inserted)
	}
	if stats.Duplicates != 3 {
		t.Errorf("Duplicates = %d, want 3", stats.Duplicates)
	}
	if stats.Failures != 0 {
		t.Errorf("Failures = %d, want 0 — duplicates are not failures", stats.Failures)
	}
}

func TestFlushFailureIsCountedAndDoesNotStopTheWriter(t *testing.T) {
	fake := newFakeStore()
	fake.err = errors.New("connection refused")

	w := New(fake, Config{BufferSize: 8, BatchSize: 5, FlushInterval: 20 * time.Millisecond}, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	if !w.Enqueue(makePositions(5)) {
		t.Fatal("first enqueue rejected")
	}

	// The writer must survive the failure and keep accepting work; a database
	// blip should not take the pipeline down with it.
	deadline := time.After(3 * time.Second)
	for w.Stats().Failures == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for a flush failure to be recorded")
		case <-time.After(5 * time.Millisecond):
		}
	}

	if !w.Enqueue(makePositions(5)) {
		t.Error("writer stopped accepting work after a flush failure")
	}
}
