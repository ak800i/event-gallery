package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	uploadDir := t.TempDir()
	return Options{
		Workers:             2,
		DurabilityWorkers:   2,
		ProcessingTimeout:   time.Minute,
		MaxBackoff:          time.Minute,
		ReconcileInterval:   50 * time.Millisecond,
		JobRetention:        time.Hour,
		IncompleteRetention: 48 * time.Hour,
		UploadDir:           uploadDir,
		MinFreeBytes:        0,
		Terminator:          &unlinkTerminator{dir: uploadDir},
	}
}

// unlinkTerminator stands in for tusd in unit tests: it removes exactly the
// two files tusd's filestore would remove.
type unlinkTerminator struct{ dir string }

func (u *unlinkTerminator) Terminate(_ context.Context, uploadID string) error {
	for _, p := range []string{filepath.Join(u.dir, uploadID), filepath.Join(u.dir, uploadID+".info")} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func TestBackoffIsExponentialAndCapped(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	first := m.backoffFor(0)
	second := m.backoffFor(1)
	if second <= first {
		t.Errorf("backoff must grow: %v then %v", first, second)
	}
	if capped := m.backoffFor(40); capped != m.opts.MaxBackoff {
		t.Errorf("backoff must cap at %v, got %v", m.opts.MaxBackoff, capped)
	}
	// 2^20s is far past MaxBackoff but well under the failures>30 shortcut, so
	// only the ceiling itself can produce this. Without it a 15m cap would be
	// ignored from failure 11 on and retries would stall for years.
	if capped := m.backoffFor(20); capped != m.opts.MaxBackoff {
		t.Errorf("backoff must cap at %v below the overflow guard, got %v", m.opts.MaxBackoff, capped)
	}
}

func TestWakeDoesNotBlockWithNoWorkerDraining(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	// Deliberately not started: with workers parked in their select, they drain
	// m.wake and a blocking send would look fine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Wake()
		m.Wake()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wake blocked on a full channel; a dropped nudge is correct, a stalled caller is not")
	}
}

func TestStartStopIsClean(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	m.Start()
	m.Wake()
	m.Wake() // workers are draining here; the no-drain case is covered by
	// TestWakeDoesNotBlockWithNoWorkerDraining
	m.Stop()
}

func TestLeaseOutlivesTheAttemptTimeout(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	// If the lease expired exactly when the attempt timed out, a second worker
	// could claim a job whose first worker is still shutting down.
	if m.leaseDuration() <= m.opts.ProcessingTimeout {
		t.Errorf("leaseDuration = %v, must exceed ProcessingTimeout %v", m.leaseDuration(), m.opts.ProcessingTimeout)
	}
}
