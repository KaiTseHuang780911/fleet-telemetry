package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/KaiTseHuang780911/fleet-telemetry/api/internal/store"
	"github.com/KaiTseHuang780911/fleet-telemetry/internal/wire"
)

// maxBodyBytes caps a telemetry request. Comfortably above a full
// MaxReadingsPerBatch payload, and low enough that a malformed or hostile
// client cannot make the server allocate without bound.
const maxBodyBytes = 4 << 20 // 4 MiB

// retryAfterSeconds is what the server suggests when it sheds load.
//
// Deliberately short: the device holds the batch in durable storage, so a quick
// retry is cheap. The client is responsible for adding jitter — if every device
// obeys the same interval exactly, the retry wave arrives together and recreates
// the overload that caused the 503.
const retryAfterSeconds = "2"

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var batch wire.Batch
	// Unknown fields are tolerated on purpose. Mobile clients update on their
	// own schedule, and a newer app sending a field this server does not know
	// yet must not have its entire queue rejected.
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge,
				"request body exceeds 4 MiB; split the batch")
			return
		}
		writeError(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}

	// Batch-level problems are the client's fault in a way it can fix without
	// guessing, so they are a hard 400. Reading-level problems are handled
	// per-reading below.
	if err := batch.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	vehicleID, err := s.store.VehicleIDForDevice(ctx, batch.DeviceID)
	if err != nil {
		s.logger.Error("resolve vehicle", "device_id", batch.DeviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not resolve device")
		return
	}

	receivedAt := time.Now().UTC()

	// The difference between the device's send time and ours is its clock
	// offset. Logged rather than stored for now: acting on it needs a column
	// that does not exist yet, and inventing one before the derivation work in
	// ADR-002 needs it would be speculative.
	if !batch.SentAt.IsZero() {
		s.logger.Debug("clock offset",
			"device_id", batch.DeviceID,
			"offset_ms", receivedAt.Sub(batch.SentAt).Milliseconds())
	}

	positions := make([]store.Position, 0, len(batch.Readings))
	var rejected []wire.Rejection

	for _, rd := range batch.Readings {
		if err := rd.Validate(receivedAt); err != nil {
			rejected = append(rejected, wire.Rejection{
				ID:     rd.ReadingID.String(),
				Reason: err.Error(),
			})
			continue
		}
		positions = append(positions, store.Position{
			ReadingID:   rd.ReadingID,
			VehicleID:   vehicleID,
			RecordedAt:  rd.RecordedAt.UTC(),
			ReceivedAt:  receivedAt,
			Lat:         rd.Lat,
			Lon:         rd.Lon,
			SpeedMPS:    rd.SpeedMPS,
			HeadingDeg:  rd.HeadingDeg,
			AccuracyM:   rd.AccuracyM,
			BatteryPct:  rd.BatteryPct,
			MotionState: rd.MotionState,
		})
	}

	if len(positions) > 0 && !s.writer.Enqueue(positions) {
		// Shed load. Nothing is dropped — the device still holds these readings
		// and will resend. See ADR-003 for why this beats blocking or dropping.
		//
		// Deliberately before the stop-event write: if the readings could not
		// be taken, the client resends the whole batch, and doing the stop
		// write first would leave it done twice. It is idempotent, so that
		// would be harmless — but skipping it is both cheaper and easier to
		// reason about.
		w.Header().Set("Retry-After", retryAfterSeconds)
		writeError(w, http.StatusServiceUnavailable, "ingest buffer full, retry shortly")
		return
	}

	acceptedStops, rejectedStops := s.storeStopEvents(ctx, w, batch, vehicleID, receivedAt)
	if acceptedStops < 0 {
		return // storeStopEvents already wrote the error response
	}

	// 202, not 200: the readings are queued, not yet durably written. Claiming
	// otherwise would be a lie the client acts on by deleting its only copy.
	writeJSON(w, http.StatusAccepted, wire.IngestResponse{
		Accepted:      len(positions),
		AcceptedStops: acceptedStops,
		Rejected:      rejected,
		RejectedStops: rejectedStops,
	})
}

// storeStopEvents validates and writes the device-reported stops in a batch.
//
// These are written synchronously rather than going through the ingest buffer.
// Stops are orders of magnitude rarer than positions — a vehicle makes tens a
// day, not thousands an hour — so they do not need batching, and writing them
// inline means a 202 genuinely does mean "stored" for this half of the payload.
//
// Returns -1 if it has already written an error response.
func (s *Server) storeStopEvents(
	ctx context.Context,
	w http.ResponseWriter,
	batch wire.Batch,
	vehicleID uuid.UUID,
	receivedAt time.Time,
) (int, []wire.Rejection) {
	if len(batch.StopEvents) == 0 {
		return 0, nil
	}

	events := make([]store.StopEvent, 0, len(batch.StopEvents))
	var rejected []wire.Rejection

	for _, se := range batch.StopEvents {
		if err := se.Validate(receivedAt); err != nil {
			rejected = append(rejected, wire.Rejection{
				ID:     se.EventID.String(),
				Reason: err.Error(),
			})
			continue
		}
		events = append(events, store.StopEvent{
			ID:         se.EventID,
			VehicleID:  vehicleID,
			Source:     store.SourceClient,
			ArrivedAt:  se.ArrivedAt.UTC(),
			DepartedAt: se.DepartedAt,
			Lat:        se.Lat,
			Lon:        se.Lon,
		})
	}

	if len(events) == 0 {
		return 0, rejected
	}

	inserted, err := s.store.InsertClientStopEvents(ctx, events)
	if err != nil {
		s.logger.Error("store client stop events", "device_id", batch.DeviceID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not store stop events")
		return -1, nil
	}

	// inserted counts genuinely new rows, so a replayed batch reports fewer
	// than were offered. The client is told how many it sent that were valid,
	// because "you already sent me that" is not something it needs to act on —
	// but the gap is worth seeing when diagnosing a device that retries too
	// eagerly.
	if dupes := len(events) - inserted; dupes > 0 {
		s.logger.Debug("client stop events already present",
			"device_id", batch.DeviceID, "duplicates", dupes)
	}
	return len(events), rejected
}
