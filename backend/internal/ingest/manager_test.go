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
		Workers:           2,
		DurabilityWorkers: 2,
		ProcessingTimeout: time.Minute,
		MaxBackoff:        time.Minute,
		ReconcileInterval: 50 * time.Millisecond,
		JobRetention:      time.Hour,
		UploadDir:         uploadDir,
		MinFreeBytes:      0,
		Terminator:        &unlinkTerminator{dir: uploadDir},
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
}

func TestStartStopIsClean(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	m.Start()
	m.Wake()
	m.Wake() // must not block even when nothing is draining the channel
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
