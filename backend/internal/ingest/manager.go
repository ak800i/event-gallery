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
	UploadDir         string
	MinFreeBytes      int64
	Terminator        SourceTerminator
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
		store:     st,
		processor: proc,
		health:    NewHealthGate(st, proc, 8),
		opts:      opts,
		wake:      make(chan struct{}, 1),
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
// the inventory fsyncs every recovered source (see main.go, which runs this in
// a goroutine after the listener starts).
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
			slog.Error("ingest worker iteration failed", "operation", "worker_loop", "worker", worker, "error", err)
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
				// Startup did not complete. Retry the whole prerequisite,
				// including the lease reset: opening readiness without it would
				// leave interrupted jobs holding pre-crash leases for a full
				// lease duration.
				m.startupRecovery()
			}
			m.expireTerminalJobs()
		}
	}
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

// TODO(task-12): temporary stub so the package compiles; the real reconcile
// implementation replaces it.
func (m *Manager) reconcileOnce() error { return nil }

// The health gate starts closed, so something must prove the media volume is
// mounted before uploads are admitted. Task 12's real implementation does the
// same check as its first step.
func (m *Manager) startupRecovery() {
	_ = m.health.Check(m.lifetime)
	m.ready.Store(true)
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
