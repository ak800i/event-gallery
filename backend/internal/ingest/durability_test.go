package ingest

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"event-gallery/backend/internal/store"
)

func seedCompleteUpload(t *testing.T, m *Manager, st *store.Store, uploadID string, payload []byte) {
	t.Helper()
	job := &store.UploadJob{
		UploadID:         uploadID,
		MediaID:          "media-" + uploadID,
		OriginalFilename: "a.jpg",
		ExpectedSize:     int64(len(payload)),
	}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := os.WriteFile(m.DataPath(uploadID), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	if err := os.WriteFile(m.InfoPath(uploadID), []byte(`{"ID":"`+uploadID+`"}`), 0o600); err != nil {
		t.Fatalf("write info: %v", err)
	}
}

func jobStatus(t *testing.T, st *store.Store, uploadID string) store.JobStatus {
	t.Helper()
	job, err := st.GetUploadJob(context.Background(), uploadID)
	if err != nil {
		t.Fatalf("get job %s: %v", uploadID, err)
	}
	if job == nil {
		t.Fatalf("no job row for %s", uploadID)
	}
	return job.Status
}

// stallStore reduces the pool to the one connection it then holds, so every
// later query parks in the driver instead of completing in microseconds. It
// returns the release func. This is a real stalled database, which is what a
// saturated or wedged durability operation looks like from the registry's
// side.
func stallStore(t *testing.T, st *store.Store) (release func()) {
	t.Helper()
	st.DB().SetMaxOpenConns(1)
	conn, err := st.DB().Conn(context.Background())
	if err != nil {
		t.Fatalf("take the only connection: %v", err)
	}
	var once sync.Once
	return func() { once.Do(func() { conn.Close() }) }
}

// awaitInFlight blocks until the registry has an operation registered for
// uploadID, so a test can act on a genuinely running operation rather than
// racing its registration.
func awaitInFlight(t *testing.T, m *Manager, uploadID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.durability.mu.Lock()
		_, ok := m.durability.inFlight[uploadID]
		m.durability.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no durability operation was ever registered for %s", uploadID)
}

func TestEnsureDurableCommitsPending(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))

	if err := m.EnsureDurable(context.Background(), "u1"); err != nil {
		t.Fatalf("ensure durable: %v", err)
	}

	job, _ := st.GetUploadJob(context.Background(), "u1")
	if job.Status != store.JobPending {
		t.Fatalf("status = %q, want pending", job.Status)
	}
	if job.SourceCompletedAt == nil {
		t.Error("source_completed_at must be committed")
	}
	// The barrier must not move, hash, or delete anything.
	if _, err := os.Stat(m.DataPath("u1")); err != nil {
		t.Errorf("data file must survive: %v", err)
	}
	if _, err := os.Stat(m.InfoPath("u1")); err != nil {
		t.Errorf("sidecar must survive: %v", err)
	}
}

func TestEnsureDurableIsIdempotent(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))
	for i := 0; i < 3; i++ {
		if err := m.EnsureDurable(context.Background(), "u1"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

// A repeated hook or the proxy fence arrives after the commit, not during it.
// It must still be told the upload is safe, and it must not disturb the row.
func TestEnsureDurableSucceedsForACallerThatArrivesAfterTheCommit(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))
	if err := m.EnsureDurable(context.Background(), "u1"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first, _ := st.GetUploadJob(context.Background(), "u1")

	if err := m.EnsureDurable(context.Background(), "u1"); err != nil {
		t.Fatalf("late caller: %v", err)
	}

	again, _ := st.GetUploadJob(context.Background(), "u1")
	if again.Status != store.JobPending {
		t.Fatalf("status = %q, want pending", again.Status)
	}
	if *again.SourceCompletedAt != *first.SourceCompletedAt {
		t.Errorf("source_completed_at moved from %d to %d; completion happens once",
			*first.SourceCompletedAt, *again.SourceCompletedAt)
	}
}

func TestEnsureDurableCommitsEvenAfterCallerGivesUp(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))

	// The caller's budget expires immediately. The work must not be wasted:
	// it continues on the manager's lifetime context and still commits.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.EnsureDurable(ctx, "u1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("an abandoned wait must report its own expiry, got %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, _ := st.GetUploadJob(context.Background(), "u1")
		if job.Status == store.JobPending {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("detached durability operation never committed")
}

func TestEnsureDurableRefusesCancelledUpload(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))
	if err := st.RequestCancellation(context.Background(), "u1", store.NowMicros()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := m.EnsureDurable(context.Background(), "u1"); err == nil {
		t.Fatal("promotion must fail closed after cancellation")
	}
}

// Reporting success while the discard worker is reclaiming the bytes would
// tell the browser its upload is safe moments before it disappears.
func TestEnsureDurableRefusesDiscardingUpload(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))
	if err := st.ClaimUploadingForDiscard(context.Background(), "u1", store.NowMicros()); err != nil {
		t.Fatalf("claim for discard: %v", err)
	}
	if err := m.EnsureDurable(context.Background(), "u1"); err == nil {
		t.Fatal("a discarding upload must never be reported durable")
	}
	if got := jobStatus(t, st, "u1"); got != store.JobDiscarding {
		t.Errorf("status = %q, want discarding", got)
	}
}

func TestEnsureDurableRefusesUnknownUpload(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	if err := m.EnsureDurable(context.Background(), "never-admitted"); err == nil {
		t.Fatal("an upload with no row must not be reported durable")
	}
}

// The size check is the difference between a durable upload and a truncated
// one: everything downstream trusts that a pending row means whole bytes.
func TestEnsureDurableRefusesSourceThatDoesNotMatchTheAdmittedSize(t *testing.T) {
	cases := map[string]func(t *testing.T, m *Manager, uploadID string){
		"truncated": func(t *testing.T, m *Manager, uploadID string) {
			if err := os.WriteFile(m.DataPath(uploadID), []byte("pay"), 0o600); err != nil {
				t.Fatalf("truncate: %v", err)
			}
		},
		"missing": func(t *testing.T, m *Manager, uploadID string) {
			if err := os.Remove(m.DataPath(uploadID)); err != nil {
				t.Fatalf("remove: %v", err)
			}
		},
		"not a regular file": func(t *testing.T, m *Manager, uploadID string) {
			if err := os.Remove(m.DataPath(uploadID)); err != nil {
				t.Fatalf("remove: %v", err)
			}
			if err := os.Mkdir(m.DataPath(uploadID), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		},
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			st, proc := newIngestFixture(t)
			m := New(st, proc, testOptions(t))
			defer m.Stop()

			seedCompleteUpload(t, m, st, "u1", []byte("payload"))
			corrupt(t, m, "u1")

			if err := m.EnsureDurable(context.Background(), "u1"); err == nil {
				t.Fatal("a source that is not the admitted file must not be promoted")
			}
			if got := jobStatus(t, st, "u1"); got != store.JobUploading {
				t.Errorf("status = %q, want uploading: a refused barrier commits nothing", got)
			}
		})
	}
}

func TestEnsureDurableWakesTheQueueOnlyOnSuccess(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))
	seedCompleteUpload(t, m, st, "u2", []byte("payload"))
	if err := os.Remove(m.DataPath("u2")); err != nil {
		t.Fatalf("remove u2 source: %v", err)
	}

	before := m.WakeCount()
	if err := m.EnsureDurable(context.Background(), "u1"); err != nil {
		t.Fatalf("ensure durable: %v", err)
	}
	if got := m.WakeCount(); got != before+1 {
		// Nothing else nudges the queue at this point, so a promoted job would
		// sit until the next reconcile tick.
		t.Errorf("wake count %d, want %d: a promotion must nudge the workers", got, before+1)
	}

	before = m.WakeCount()
	if err := m.EnsureDurable(context.Background(), "u2"); err == nil {
		t.Fatal("u2 has no source and must not be promoted")
	}
	if got := m.WakeCount(); got != before {
		t.Errorf("wake count %d, want %d: a failed barrier queued nothing", got, before)
	}
}

// The client retries after a refusal, so the registry must not hand the next
// caller a cached failure, and the refused operation must give its slot back.
func TestEnsureDurableRetriesAfterAFailedAttempt(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	opts.DurabilityWorkers = 1
	m := New(st, proc, opts)
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))
	if err := os.Remove(m.DataPath("u1")); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	if err := m.EnsureDurable(context.Background(), "u1"); err == nil {
		t.Fatal("a missing source must not be promoted")
	}

	if err := os.WriteFile(m.DataPath("u1"), []byte("payload"), 0o600); err != nil {
		t.Fatalf("restore source: %v", err)
	}
	if err := m.EnsureDurable(context.Background(), "u1"); err != nil {
		t.Fatalf("retry after a failed attempt: %v", err)
	}
	if got := jobStatus(t, st, "u1"); got != store.JobPending {
		t.Errorf("status = %q, want pending", got)
	}
}

// A retried PATCH, a repeated hook and the proxy fence can all arrive while
// one operation is running. They must join it, not queue behind it: with a
// single executor slot, a caller that did not join could only be refused.
func TestEnsureDurableJoinsTheOperationAlreadyInFlight(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	opts.DurabilityWorkers = 1
	m := New(st, proc, opts)
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))

	release := stallStore(t, st)
	defer release()

	results := make(chan error, 3)
	go func() { results <- m.EnsureDurable(context.Background(), "u1") }()
	awaitInFlight(t, m, "u1")

	for i := 0; i < 2; i++ {
		go func() { results <- m.EnsureDurable(context.Background(), "u1") }()
	}
	// Let the joiners park on the running operation before it can finish.
	time.Sleep(50 * time.Millisecond)
	release()

	for i := 0; i < 3; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("caller %d: %v", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a joined caller was never released")
		}
	}
	if got := jobStatus(t, st, "u1"); got != store.JobPending {
		t.Errorf("status = %q, want pending", got)
	}
}

// Saturation is backpressure, never success: the caller has to relay a 503 so
// the browser retries, and the row must stay exactly where it was.
func TestEnsureDurableReportsBusyWhenTheExecutorIsSaturated(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	opts.DurabilityWorkers = 1
	m := New(st, proc, opts)
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))
	seedCompleteUpload(t, m, st, "u2", []byte("payload"))

	release := stallStore(t, st)
	defer release()

	parked := make(chan error, 1)
	go func() { parked <- m.EnsureDurable(context.Background(), "u1") }()
	awaitInFlight(t, m, "u1")

	err := m.EnsureDurable(context.Background(), "u2")
	if !errors.Is(err, ErrDurabilityBusy) {
		t.Fatalf("second upload error = %v, want ErrDurabilityBusy", err)
	}

	release()
	select {
	case err := <-parked:
		if err != nil {
			t.Fatalf("the operation holding the slot failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked operation never finished")
	}
	if got := jobStatus(t, st, "u2"); got != store.JobUploading {
		t.Errorf("u2 status = %q, want uploading: a refused caller commits nothing", got)
	}
}

func TestConcurrentEnsureDurableCallsAllReportTheSameCommit(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	opts.DurabilityWorkers = 8
	m := New(st, proc, opts)
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))

	const callers = 8
	start := make(chan struct{})
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- m.EnsureDurable(context.Background(), "u1")
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	for err := range results {
		if err != nil {
			t.Fatalf("concurrent caller: %v", err)
		}
	}
	job, _ := st.GetUploadJob(context.Background(), "u1")
	if job.Status != store.JobPending {
		t.Fatalf("status = %q, want pending", job.Status)
	}
	if job.SourceCompletedAt == nil {
		t.Error("source_completed_at must be committed")
	}
}

// The detached operation outlives its callers, so nothing but its own budget
// can end it. Without that bound a wedged fsync would hold an executor slot
// for the life of the process.
func TestDetachedDurabilityWorkIsBounded(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	opts.ProcessingTimeout = time.Nanosecond
	m := New(st, proc, opts)
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))

	if err := m.EnsureDurable(context.Background(), "u1"); err == nil {
		t.Fatal("an operation past its own budget must not report success")
	}
	if got := jobStatus(t, st, "u1"); got != store.JobUploading {
		t.Errorf("status = %q, want uploading", got)
	}
}

// Shutdown must release a parked caller rather than stranding the request,
// and it must never let it report a durability the database did not record.
func TestStopReleasesAParkedCallerWithoutReportingSuccess(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	seedCompleteUpload(t, m, st, "u1", []byte("payload"))

	release := stallStore(t, st)
	defer release()

	parked := make(chan error, 1)
	go func() { parked <- m.EnsureDurable(context.Background(), "u1") }()
	awaitInFlight(t, m, "u1")

	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop never returned: a detached operation was not joined")
	}

	select {
	case err := <-parked:
		if err == nil {
			t.Fatal("a caller released by shutdown must not report success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown left a caller parked")
	}

	release()
	if got := jobStatus(t, st, "u1"); got != store.JobUploading {
		t.Errorf("status = %q, want uploading: shutdown committed nothing", got)
	}
}
