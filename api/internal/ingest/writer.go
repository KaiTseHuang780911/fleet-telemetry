// Package ingest buffers accepted telemetry and writes it to the database in
// batches.
//
// The shape is: HTTP handler -> bounded channel -> single writer goroutine.
// See ADR-003 for why this is a channel and not a message broker, and why a
// full buffer sheds load rather than blocking.
package ingest

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/store"
)

// PositionWriter is the slice of the store the writer actually needs.
//
// Declared here rather than in store because Go interfaces belong to the
// consumer: this package states what it requires, and the test suite can
// satisfy it without a database.
type PositionWriter interface {
	InsertPositions(ctx context.Context, positions []store.Position) (int, error)
}

// Stats is a snapshot of the writer's counters.
//
// Exposed on /readyz, so the JSON names match the rest of the API rather than
// leaking Go's exported-field casing.
type Stats struct {
	Enqueued   int64 `json:"enqueued"`   // readings accepted into the buffer
	Shed       int64 `json:"shed"`       // readings refused because the buffer was full
	Inserted   int64 `json:"inserted"`   // rows the database reported as newly written
	Duplicates int64 `json:"duplicates"` // rows already present, ignored via ON CONFLICT
	Flushes    int64 `json:"flushes"`
	Failures   int64 `json:"failures"` // flushes that returned an error
}

// Writer accumulates readings and flushes them in batches.
type Writer struct {
	ch            chan []store.Position
	batchSize     int
	flushInterval time.Duration
	flushTimeout  time.Duration
	store         PositionWriter
	logger        *slog.Logger

	// mu guards closed and, critically, serialises against close(ch). Enqueue
	// holds it for reading while it sends; Shutdown takes it for writing before
	// closing. Without this, a send racing a close panics on a closed channel.
	mu     sync.RWMutex
	closed bool

	done chan struct{}

	enqueued   atomic.Int64
	shed       atomic.Int64
	inserted   atomic.Int64
	duplicates atomic.Int64
	flushes    atomic.Int64
	failures   atomic.Int64
}

// Config parameterises the writer. Zero values fall back to defaults so tests
// can specify only what they care about.
type Config struct {
	BufferSize    int
	BatchSize     int
	FlushInterval time.Duration
	FlushTimeout  time.Duration
}

// New builds a writer. Call Run to start it.
func New(w PositionWriter, cfg Config, logger *slog.Logger) *Writer {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 1024
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 500
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 250 * time.Millisecond
	}
	if cfg.FlushTimeout <= 0 {
		cfg.FlushTimeout = 10 * time.Second
	}

	return &Writer{
		// The buffer counts submissions, not readings: one slot holds one
		// request's worth of positions. Memory is therefore bounded by
		// BufferSize x MaxReadingsPerBatch, which is what makes the batch-size
		// cap in the wire package load-bearing rather than cosmetic.
		ch:            make(chan []store.Position, cfg.BufferSize),
		batchSize:     cfg.BatchSize,
		flushInterval: cfg.FlushInterval,
		flushTimeout:  cfg.FlushTimeout,
		store:         w,
		logger:        logger,
		done:          make(chan struct{}),
	}
}

// Enqueue offers a batch to the writer. It never blocks.
//
// Returning false means the buffer is full and the caller should shed load —
// the handler turns that into 503 with Retry-After. Nothing is silently
// dropped: the reading stays in the device's durable queue and comes back.
// That is the whole argument for pushing backpressure to the edge, since the
// edge is where a durable buffer already exists.
func (w *Writer) Enqueue(positions []store.Position) bool {
	if len(positions) == 0 {
		return true
	}

	w.mu.RLock()
	defer w.mu.RUnlock()

	if w.closed {
		return false
	}

	select {
	case w.ch <- positions:
		w.enqueued.Add(int64(len(positions)))
		return true
	default:
		w.shed.Add(int64(len(positions)))
		return false
	}
}

// Run drives the writer until ctx is cancelled or Shutdown is called. It blocks,
// so callers run it in its own goroutine.
func (w *Writer) Run(ctx context.Context) {
	defer close(w.done)

	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	pending := make([]store.Position, 0, w.batchSize)

	for {
		select {
		case batch, ok := <-w.ch:
			if !ok {
				// A closed channel yields its buffered values before reporting
				// closed, so reaching here means everything queued has been
				// received. Flush the remainder and stop.
				w.flush(pending)
				return
			}
			pending = append(pending, batch...)
			for len(pending) >= w.batchSize {
				w.flush(pending[:w.batchSize])
				pending = pending[w.batchSize:]
			}
			// Re-slicing above keeps pointing into the original backing array,
			// so the memory behind already-flushed rows is never released.
			// Copying into a fresh slice once the remainder is small keeps the
			// writer's footprint flat over a long run.
			if cap(pending) > 4*w.batchSize {
				pending = append(make([]store.Position, 0, w.batchSize), pending...)
			}

		case <-ticker.C:
			if len(pending) > 0 {
				w.flush(pending)
				pending = make([]store.Position, 0, w.batchSize)
			}

		case <-ctx.Done():
			w.flush(pending)
			return
		}
	}
}

// Shutdown stops accepting work and waits for the writer to drain, or for ctx
// to expire. Safe to call more than once.
func (w *Writer) Shutdown(ctx context.Context) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		<-w.done
		return nil
	}
	w.closed = true
	close(w.ch)
	w.mu.Unlock()

	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats returns a snapshot of the counters.
func (w *Writer) Stats() Stats {
	return Stats{
		Enqueued:   w.enqueued.Load(),
		Shed:       w.shed.Load(),
		Inserted:   w.inserted.Load(),
		Duplicates: w.duplicates.Load(),
		Flushes:    w.flushes.Load(),
		Failures:   w.failures.Load(),
	}
}

func (w *Writer) flush(positions []store.Position) {
	if len(positions) == 0 {
		return
	}

	// A deliberately independent context. During shutdown the request context
	// is already cancelled, and inheriting it would abort the very drain we are
	// trying to complete — turning a graceful stop into data loss.
	ctx, cancel := context.WithTimeout(context.Background(), w.flushTimeout)
	defer cancel()

	w.flushes.Add(1)

	inserted, err := w.store.InsertPositions(ctx, positions)
	if err != nil {
		w.failures.Add(1)
		// The readings are gone from this process's memory. They are not lost
		// overall: the device never received a durable acknowledgement, so its
		// queue still holds them and will resend. Logged loudly because a
		// sustained failure rate here is the signal that the database, not the
		// pipeline, is the problem.
		w.logger.Error("batch flush failed", "readings", len(positions), "err", err)
		return
	}

	w.inserted.Add(int64(inserted))
	if dupes := len(positions) - inserted; dupes > 0 {
		w.duplicates.Add(int64(dupes))
	}
}
