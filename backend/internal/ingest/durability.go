package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/store"
)

// ErrDurabilityBusy means the fixed executor is saturated. Callers must relay
// backpressure to the browser, never report success.
var ErrDurabilityBusy = errors.New("durability executor is saturated")

// ErrDurabilityClosing means shutdown has begun, so no new operation may
// start. It is as transient as a refusal gets: the next process accepts the
// same retry.
var ErrDurabilityClosing = errors.New("durability executor is shutting down")

// ErrDurabilityFinal marks a deterministic refusal — the upload is not the one
// we admitted, or its row has left the lifecycle. Retrying can only fail the
// same way, so callers must tell the client to stop rather than to come back
// in five seconds. Every other failure is transient by omission.
var ErrDurabilityFinal = errors.New("upload can never be made durable")

// Indirected so a test can assert the barrier syncs before it commits.
var (
	fsyncFile = media.FsyncFile
	fsyncDir  = media.FsyncDir
)

type durabilityOp struct {
	done chan struct{}
	err  error
}

// durabilityRegistry keys one in-flight operation per upload id so that a
// retried PATCH, a repeated hook, and the proxy fence all join the same work
// instead of racing.
type durabilityRegistry struct {
	manager  *Manager
	mu       sync.Mutex
	inFlight map[string]*durabilityOp
	slots    chan struct{}
	wg       sync.WaitGroup
	closing  bool
}

func newDurabilityRegistry(m *Manager) *durabilityRegistry {
	workers := m.opts.DurabilityWorkers
	if workers <= 0 {
		workers = 2
	}
	return &durabilityRegistry{
		manager:  m,
		inFlight: make(map[string]*durabilityOp),
		slots:    make(chan struct{}, workers),
	}
}

// wait blocks until every detached operation has finished, so shutdown cannot
// close the database while a promotion is still committing. It first closes
// the registry under the same lock that guards wg.Add: without that, a hook
// arriving now could take the counter from zero to one underneath this Wait,
// which panics on the caller's shutdown goroutine.
func (r *durabilityRegistry) wait() {
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()
	r.wg.Wait()
}

// EnsureDurable fsyncs a completed upload's source and commits its pending
// row. Returning nil is the only thing that entitles tus to report success.
//
// The operation itself runs on the manager's lifetime context, so a caller
// whose HTTP budget expires abandons only its own wait — the bytes still
// become durable and the browser's retry finds the work already done.
func (m *Manager) EnsureDurable(ctx context.Context, uploadID string) error {
	return m.durability.ensure(ctx, uploadID)
}

func (r *durabilityRegistry) ensure(ctx context.Context, uploadID string) error {
	r.mu.Lock()
	op, joined := r.inFlight[uploadID]
	if !joined {
		if r.closing {
			r.mu.Unlock()
			return ErrDurabilityClosing
		}
		select {
		case r.slots <- struct{}{}:
		default:
			r.mu.Unlock()
			return ErrDurabilityBusy
		}
		op = &durabilityOp{done: make(chan struct{})}
		r.inFlight[uploadID] = op
		r.wg.Add(1)
		go r.run(uploadID, op)
	}
	r.mu.Unlock()

	select {
	case <-op.done:
		return op.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *durabilityRegistry) run(uploadID string, op *durabilityOp) {
	defer func() {
		// Both under one lock: dropping the map entry first leaves a caller
		// arriving in that window with nothing to join and no free slot, so it
		// would be refused as busy for no reason.
		r.mu.Lock()
		delete(r.inFlight, uploadID)
		<-r.slots
		r.mu.Unlock()
		close(op.done)
		r.wg.Done()
	}()

	// Bound the detached work so a stalled fsync cannot hold a slot forever.
	ctx, cancel := context.WithTimeout(r.manager.lifetime, r.manager.opts.ProcessingTimeout)
	defer cancel()

	op.err = r.manager.makeDurable(ctx, uploadID)
	if op.err != nil {
		// A busy database here is retried by the queue and the item still
		// publishes -- observed live during the overload stage, where an upload
		// lost both this barrier and its completion fence and published anyway.
		if store.IsBusy(op.err) {
			slog.Warn("durability barrier deferred, database busy", "operation", "durability", "upload_id", uploadID,
				"final", errors.Is(op.err, ErrDurabilityFinal), "error", op.err)
			return
		}
		slog.Error("durability barrier failed", "operation", "durability", "upload_id", uploadID,
			"final", errors.Is(op.err, ErrDurabilityFinal), "error", op.err)
		return
	}
	slog.Info("upload became durable", "operation", "durability", "upload_id", uploadID)
	r.manager.Wake()
}

// makeDurable performs the fsync barrier and the promotion. It never moves,
// hashes, decodes, or deletes anything: that work belongs to the workers,
// where it cannot delay or fail the client's request.
func (m *Manager) makeDurable(ctx context.Context, uploadID string) error {
	dataPath := m.DataPath(uploadID)
	// This is the operation that fsyncs and publishes whatever the id resolves
	// to, so containment is asserted here and not only at the HTTP boundary.
	if filepath.Dir(dataPath) != filepath.Clean(m.opts.UploadDir) {
		return fmt.Errorf("%w: upload id %q escapes the upload directory", ErrDurabilityFinal, uploadID)
	}

	job, err := m.store.GetUploadJob(ctx, uploadID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("%w: no upload job for %s", ErrDurabilityFinal, uploadID)
	}
	switch job.Status {
	case store.JobUploading:
		// Needs the barrier; continue below.
	case store.JobDiscarding, store.JobDiscarded:
		// Reporting success here would tell the browser its upload is safe
		// while the discard worker is removing it.
		return fmt.Errorf("%w: upload %s is being discarded", ErrDurabilityFinal, uploadID)
	default:
		return nil // already durable; nothing to do
	}

	stat, err := os.Stat(dataPath)
	if err != nil {
		return fmt.Errorf("%w: stat completed source: %w", ErrDurabilityFinal, err)
	}
	if !stat.Mode().IsRegular() {
		return fmt.Errorf("%w: source %s is not a regular file", ErrDurabilityFinal, dataPath)
	}
	if stat.Size() != job.ExpectedSize {
		return fmt.Errorf("%w: source size %d, expected %d", ErrDurabilityFinal, stat.Size(), job.ExpectedSize)
	}

	if err := fsyncFile(dataPath); err != nil {
		return err
	}
	if err := fsyncFile(m.InfoPath(uploadID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := fsyncDir(m.opts.UploadDir); err != nil {
		return err
	}

	if err := m.store.PromoteToPending(ctx, uploadID, store.NowMicros()); err != nil {
		if errors.Is(err, store.ErrNotClaimed) {
			return m.classifyLostPromotion(ctx, uploadID)
		}
		return err
	}
	return nil
}

// classifyLostPromotion decides what a promotion that matched no row means.
// PromoteToPending reports the same ErrNotClaimed whether another caller has
// already committed the row — which is a success — or the upload was cancelled
// underneath us, which is final. Only the row itself can tell them apart.
func (m *Manager) classifyLostPromotion(ctx context.Context, uploadID string) error {
	job, err := m.store.GetUploadJob(ctx, uploadID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("%w: upload job %s disappeared during promotion", ErrDurabilityFinal, uploadID)
	}
	switch job.Status {
	case store.JobDiscarding, store.JobDiscarded:
		return fmt.Errorf("%w: upload %s is being discarded", ErrDurabilityFinal, uploadID)
	case store.JobUploading:
		if job.CancellationRequestedAt != nil {
			return fmt.Errorf("%w: upload %s was cancelled", ErrDurabilityFinal, uploadID)
		}
		// Still uploading, still not cancelled, yet the update matched nothing.
		// Unexplained, so transient: never tell a client to give up on bytes
		// that may still be committable.
		return fmt.Errorf("promotion of upload %s matched no row", uploadID)
	default:
		return nil // pending or later: another caller committed it
	}
}
