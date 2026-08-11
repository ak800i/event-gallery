package ingest

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"event-gallery/backend/internal/store"
)

// logRecorder captures what the package actually emits. The queue summary and
// the shutdown log level are both operator-facing contracts, and the log line
// is the only place either of them exists.
type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec.Clone())
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

// find returns the most recent record with this message.
func (r *logRecorder) find(message string) (slog.Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.records) - 1; i >= 0; i-- {
		if r.records[i].Message == message {
			return r.records[i], true
		}
	}
	return slog.Record{}, false
}

func (r *logRecorder) errorMessages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var msgs []string
	for _, rec := range r.records {
		if rec.Level >= slog.LevelError {
			msgs = append(msgs, rec.Message)
		}
	}
	return msgs
}

func captureLogs(t *testing.T) *logRecorder {
	t.Helper()
	recorder := &logRecorder{}
	previous := slog.Default()
	slog.SetDefault(slog.New(recorder))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return recorder
}

func attrValue(rec slog.Record, key string) (slog.Value, bool) {
	var found slog.Value
	var ok bool
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found, ok = a.Value, true
			return false
		}
		return true
	})
	return found, ok
}

// A permanently failing job emits one WARN per attempt at up to fifteen-minute
// intervals and nothing else: /readyz never reflects queue state, last_error is
// written to the row and surfaced nowhere, and there is no depth or age signal
// at all. One aggregated line per pass is the difference between reading a log
// and opening the SQLite file.
func TestReconcilerLogsAQueueSummary(t *testing.T) {
	recorder := captureLogs(t)
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	// Pending, but held under an unexpired lease so the real workers cannot
	// claim it and change the counts under the assertions.
	job := &store.UploadJob{UploadID: "stuck", MediaID: "media-stuck", OriginalFilename: "a.jpg", ExpectedSize: 4}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.DB().Exec(
		`UPDATE upload_jobs
		    SET status = 'pending', processing_failures = 7, next_attempt_at = ?,
		        lease_token = 'held', lease_until = ?
		  WHERE upload_id = ?`,
		store.NowMicros()-(10*time.Minute).Microseconds(),
		store.NowMicros()+time.Hour.Microseconds(), "stuck"); err != nil {
		t.Fatalf("wedge the job: %v", err)
	}

	m.Start()
	defer m.Stop()

	awaitCondition(t, "the reconciler reports the queue", func() bool {
		rec, ok := recorder.find("ingest queue")
		if !ok {
			return false
		}
		pending, hasPending := attrValue(rec, "pending")
		failures, hasFailures := attrValue(rec, "max_processing_failures")
		age, hasAge := attrValue(rec, "oldest_pending_age_seconds")
		return hasPending && pending.Int64() == 1 &&
			hasFailures && failures.Int64() == 7 &&
			hasAge && age.Int64() >= 600
	})
}

// The line is meant to be greppable, so its presence has to mean something. An
// installation that is simply idle would otherwise print one every reconcile
// interval forever, and the signal would be buried in its own noise.
func TestQueueSummaryIsSilentWhileNothingIsInFlight(t *testing.T) {
	recorder := captureLogs(t)
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	// A terminal row is not work: it is waiting out its retention.
	job := &store.UploadJob{UploadID: "done", MediaID: "media-done", OriginalFilename: "a.jpg", ExpectedSize: 4}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.DB().Exec(
		`UPDATE upload_jobs SET status = 'complete', terminal_at = ? WHERE upload_id = ?`,
		store.NowMicros(), "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}

	m.logQueueSummary()

	if _, logged := recorder.find("ingest queue"); logged {
		t.Error("an idle queue reported itself")
	}
}

// A clean shutdown with work in flight always fails to record its retry,
// because the lifetime the write runs on is the thing that was cancelled. At
// ERROR that produces a false page on every deploy, and an operator who then
// stops paging on ERROR has no signal left for the case that matters.
func TestShutdownDoesNotLogIngestFailuresAtErrorLevel(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, m *Manager, st *store.Store)
	}{
		{"processing", func(t *testing.T, m *Manager, st *store.Store) {
			job, err := st.ClaimNextJob(context.Background(), store.JobPending, store.JobProcessing, store.NowMicros(), time.Minute)
			if err != nil || job == nil {
				t.Fatalf("claim for processing: %+v %v", job, err)
			}
			m.Stop()
			m.runProcessing(job)
		}},
		{"cleanup", func(t *testing.T, m *Manager, st *store.Store) {
			if worked, err := m.claimAndRunOnce(); !worked || err != nil {
				t.Fatalf("publication pass: worked=%v err=%v", worked, err)
			}
			job, err := st.ClaimNextJob(context.Background(), store.JobCleanup, store.JobCleanup, store.NowMicros(), time.Minute)
			if err != nil || job == nil {
				t.Fatalf("claim for cleanup: %+v %v", job, err)
			}
			m.Stop()
			m.runCleanup(job)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, proc := newIngestFixture(t)
			m := New(st, proc, testOptions(t))
			defer m.Stop()

			seedCompleteUpload(t, m, st, "u1", jpegFixture(t))
			if err := m.EnsureDurable(context.Background(), "u1"); err != nil {
				t.Fatalf("ensure durable: %v", err)
			}

			recorder := captureLogs(t)
			tc.run(t, m, st)

			if msgs := recorder.errorMessages(); len(msgs) > 0 {
				t.Errorf("a clean shutdown logged at ERROR: %v", msgs)
			}
		})
	}
}
