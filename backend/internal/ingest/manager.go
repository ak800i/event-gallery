package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/store"
)

// SourceTerminator removes one upload from tusd's own storage. The app never
// unlinks tusd's files itself, so tusd's sidecars and locks stay consistent,
// and every deletion in the system goes through one implementation.
type SourceTerminator interface {
	Terminate(ctx context.Context, uploadID string) error
}

type Options struct {
	Workers           int
	DurabilityWorkers int
	ProcessingTimeout time.Duration
	MaxBackoff        time.Duration
	ReconcileInterval time.Duration
	JobRetention      time.Duration
	// IncompleteRetention is the incomplete-upload retention policy the
	// janitor enforces. The reconciler reads it as the age at which a file of
	// unknown standing has stopped being an upload in progress; see
	// adoptionWitnessed.
	IncompleteRetention time.Duration
	UploadDir           string
	MinFreeBytes        int64
	Terminator          SourceTerminator
}

// Manager owns the durable ingest queue. Its workers run on lifetime, a
// context created here and cancelled only by Stop. It deliberately accepts no
// context from callers: the original data loss happened because ingest ran on
// a request-derived context that tusd cancelled ten seconds after the
// upload's final PATCH, and a signature that cannot express that mistake is
// worth more than a comment warning against it.
type Manager struct {
	store     *store.Store
	processor *media.Processor
	health    *HealthGate
	opts      Options

	lifetime context.Context
	cancel   context.CancelFunc
	wake     chan struct{}
	wg       sync.WaitGroup
	ready    atomic.Bool

	// adoptWitness carries one reconcile pass's observation of each data file
	// that has neither a row nor a sidecar to vouch for it, so the next pass
	// can tell a finished file from one that is still changing.
	adoptMu      sync.Mutex
	adoptWitness map[string]adoptionObservation

	durability *durabilityRegistry
}

func New(st *store.Store, proc *media.Processor, opts Options) *Manager {
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	if opts.DurabilityWorkers <= 0 {
		opts.DurabilityWorkers = 2
	}
	if opts.ProcessingTimeout <= 0 {
		opts.ProcessingTimeout = time.Hour
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 15 * time.Minute
	}
	if opts.ReconcileInterval <= 0 {
		opts.ReconcileInterval = 15 * time.Second
	}
	m := &Manager{
		store:        st,
		processor:    proc,
		health:       NewHealthGate(st, proc, 8),
		opts:         opts,
		wake:         make(chan struct{}, 1),
		adoptWitness: make(map[string]adoptionObservation),
	}
	m.lifetime, m.cancel = context.WithCancel(context.Background())
	m.durability = newDurabilityRegistry(m)
	return m
}

// Health exposes the gate so HTTP handlers can refuse uploads while the
// circuit is open.
func (m *Manager) Health() *HealthGate { return m.health }

// Ready reports whether the startup inventory has finished. Upload routes must
// return a retryable 503 until it has.
func (m *Manager) Ready() bool { return m.ready.Load() }

// leaseDuration must exceed the attempt timeout, so a worker that runs its
// full budget still owns the job while it unwinds.
func (m *Manager) leaseDuration() time.Duration {
	return m.opts.ProcessingTimeout + m.opts.ReconcileInterval
}

// Start runs the startup inventory and then launches the pool. It is
// deliberately synchronous: readiness is true the moment it returns, which is
// what the rollout requires — every valid pre-upgrade sidecar is adopted
// before the app reports ready. Callers must already be serving HTTP, since
// the inventory fsyncs every recovered source; main.go calls it directly --
// never in a new goroutine -- after the listener starts, so that a shutdown
// racing startup cannot observe an empty WaitGroup.
func (m *Manager) Start() {
	// Held for the whole of Start so Stop cannot observe an empty WaitGroup
	// while recovery is still using the database.
	m.wg.Add(1)
	defer m.wg.Done()

	m.startupRecovery()
	if m.lifetime.Err() != nil {
		return // Stop() arrived during the inventory; do not launch workers
	}

	for i := 0; i < m.opts.Workers; i++ {
		m.wg.Add(1)
		go func(worker int) {
			defer m.wg.Done()
			m.runWorker(worker)
		}(i)
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.runReconciler()
	}()
}

// Stop cancels the lifetime context and waits for in-flight work to yield,
// including detached durability operations. Without that wait, a promotion
// could still be writing when main closes the database.
func (m *Manager) Stop() {
	m.cancel()
	m.wg.Wait()
	m.durability.wait()
}

// Wake nudges a worker without blocking. A full channel already means "there
// is work to look at", so dropping the signal is correct.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) runWorker(worker int) {
	ticker := time.NewTicker(m.opts.ReconcileInterval)
	defer ticker.Stop()

	for {
		worked, err := m.claimAndRunOnce()
		if err != nil {
			// A busy database is the next tick's problem, not an operator's:
			// the worker retries and normally succeeds. Logged at ERROR, a
			// bulk purge on an idle app produced 141 of these in six minutes.
			if store.IsBusy(err) {
				slog.Warn("ingest worker iteration deferred, database busy", "operation", "worker_loop", "worker", worker, "error", err)
			} else {
				slog.Error("ingest worker iteration failed", "operation", "worker_loop", "worker", worker, "error", err)
			}
		}
		if worked {
			continue // drain the queue before sleeping
		}
		select {
		case <-m.lifetime.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
	}
}

func (m *Manager) runReconciler() {
	ticker := time.NewTicker(m.opts.ReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.lifetime.Done():
			return
		case <-ticker.C:
			if err := m.health.Check(m.lifetime); err != nil {
				slog.Warn("storage health check failed", "operation", "reconcile", "error", err)
			}
			if m.Ready() {
				if err := m.reconcileOnce(); err != nil {
					slog.Warn("reconcile pass failed", "operation", "reconcile", "error", err)
				}
			} else {
				// Startup did not complete, so retry the inventory until it
				// does. Only the inventory: the startup requeue clears leases
				// unconditionally, and the workers launched by Start are
				// running by now, so repeating it here would take jobs away
				// from workers midway through an attempt -- orphaning a
				// full-size temporary each lap and letting a large job be
				// demoted before it can ever finish.
				m.recoverInventory()
			}
			m.logQueueSummary()
			m.expireTerminalJobs()
		}
	}
}

// logQueueSummary emits the one line an operator can grep for. Retries are
// indefinite by design, so a job that can never succeed produces only a WARN
// per attempt at up to MaxBackoff intervals; nothing else in the system
// reports queue depth, the age of the oldest queued work, or how many times
// the worst job has failed. It stays silent while nothing is in flight, so the
// line's presence is itself the signal.
func (m *Manager) logQueueSummary() {
	summary, err := m.store.SummarizeQueue(m.lifetime)
	if err != nil {
		if m.lifetime.Err() == nil {
			slog.Warn("could not summarize the ingest queue", "operation", "queue_summary", "error", err)
		}
		return
	}
	if summary.Active == 0 {
		return
	}
	attrs := []any{"operation", "queue_summary"}
	for _, status := range []store.JobStatus{
		store.JobUploading, store.JobPending, store.JobProcessing,
		store.JobCleanup, store.JobDiscarding,
	} {
		attrs = append(attrs, string(status), summary.Counts[status])
	}
	attrs = append(attrs, "max_processing_failures", summary.MaxProcessingFailures)
	if summary.OldestPendingAttemptAt != nil {
		attrs = append(attrs, "oldest_pending_age_seconds",
			(store.NowMicros()-*summary.OldestPendingAttemptAt)/int64(time.Second/time.Microsecond))
	}
	slog.Info("ingest queue", attrs...)
}

// backoffFor grows exponentially and then flattens. Retries are indefinite:
// no failure count may ever escalate into deleting a source.
func (m *Manager) backoffFor(failures int) time.Duration {
	if failures < 0 {
		failures = 0
	}
	if failures > 30 {
		return m.opts.MaxBackoff
	}
	delay := time.Duration(math.Pow(2, float64(failures))) * time.Second
	if delay <= 0 || delay > m.opts.MaxBackoff {
		return m.opts.MaxBackoff
	}
	return delay
}

func (m *Manager) expireTerminalJobs() {
	cutoff := store.NowMicros() - m.opts.JobRetention.Microseconds()
	if _, err := m.store.DeleteTerminalJobsBefore(m.lifetime, cutoff, 200); err != nil {
		slog.Warn("failed to expire terminal upload jobs", "operation", "expire_jobs", "error", err)
	}
}

// DataPath and InfoPath are the only two paths tusd's filestore derives from
// an upload id. Deriving them here keeps every absence check consistent.
func (m *Manager) DataPath(uploadID string) string {
	return filepath.Join(m.opts.UploadDir, uploadID)
}

func (m *Manager) InfoPath(uploadID string) string {
	return filepath.Join(m.opts.UploadDir, uploadID+".info")
}

// UploadPathsExist reports whether either derived path is already taken.
func (m *Manager) UploadPathsExist(uploadID string) bool {
	if _, err := os.Stat(m.DataPath(uploadID)); err == nil {
		return true
	}
	if _, err := os.Stat(m.InfoPath(uploadID)); err == nil {
		return true
	}
	return false
}

// AdmitCapacity refuses a create that would push either volume under the free
// space floor. It is deliberately coarse: running out of disk is a transient
// failure that retries, so approximate accounting is safe.
func (m *Manager) AdmitCapacity(ctx context.Context, size int64) error {
	if m.opts.MinFreeBytes <= 0 {
		return nil
	}
	for _, dir := range []string{m.opts.UploadDir, m.processor.MediaDir} {
		free, err := freeBytes(dir)
		if err != nil {
			slog.Warn("could not stat filesystem for admission", "operation", "admit_capacity", "dir", dir, "error", err)
			continue
		}
		if free-size < m.opts.MinFreeBytes {
			return fmt.Errorf("free space on %s would fall below the floor", dir)
		}
	}
	return nil
}
