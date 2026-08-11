package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/store"
)

// ErrDurabilityBusy means the fixed executor is saturated. Callers must relay
// backpressure to the browser, never report success.
var ErrDurabilityBusy = errors.New("durability executor is saturated")

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
// close the database while a promotion is still committing.
func (r *durabilityRegistry) wait() { r.wg.Wait() }

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
		r.mu.Lock()
		delete(r.inFlight, uploadID)
		r.mu.Unlock()
		<-r.slots
		close(op.done)
		r.wg.Done()
	}()

	// Bound the detached work so a stalled fsync cannot hold a slot forever.
	ctx, cancel := context.WithTimeout(r.manager.lifetime, r.manager.opts.ProcessingTimeout)
	defer cancel()

	op.err = r.manager.makeDurable(ctx, uploadID)
	if op.err != nil {
		slog.Error("durability barrier failed", "operation", "durability", "upload_id", uploadID, "error", op.err)
		return
	}
	slog.Info("upload became durable", "operation", "durability", "upload_id", uploadID)
	r.manager.Wake()
}

// makeDurable performs the fsync barrier and the promotion. It never moves,
// hashes, decodes, or deletes anything: that work belongs to the workers,
// where it cannot delay or fail the client's request.
func (m *Manager) makeDurable(ctx context.Context, uploadID string) error {
	job, err := m.store.GetUploadJob(ctx, uploadID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("no upload job for %s", uploadID)
	}
	switch job.Status {
	case store.JobUploading:
		// Needs the barrier; continue below.
	case store.JobDiscarding, store.JobDiscarded:
		// Reporting success here would tell the browser its upload is safe
		// while the discard worker is removing it.
		return fmt.Errorf("upload %s is being discarded", uploadID)
	default:
		return nil // already durable; nothing to do
	}

	dataPath := m.DataPath(uploadID)
	stat, err := os.Stat(dataPath)
	if err != nil {
		return fmt.Errorf("stat completed source: %w", err)
	}
	if !stat.Mode().IsRegular() {
		return fmt.Errorf("source %s is not a regular file", dataPath)
	}
	if stat.Size() != job.ExpectedSize {
		return fmt.Errorf("source size %d, expected %d", stat.Size(), job.ExpectedSize)
	}

	if err := media.FsyncFile(dataPath); err != nil {
		return err
	}
	if err := media.FsyncFile(m.InfoPath(uploadID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := media.FsyncDir(m.opts.UploadDir); err != nil {
		return err
	}

	return m.store.PromoteToPending(ctx, uploadID, store.NowMicros())
}
