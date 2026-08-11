// Package ingest owns the durable upload queue: admission, the pre-finish
// durability barrier, leased workers, publication, and recovery. Everything in
// here runs on the manager's lifetime context, never on a request context.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"

	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/store"
)

// ErrStorageUnhealthy means we cannot prove the media volume is mounted. It
// must block every deletion: a failed bind mount presents an empty directory,
// and treating that as "the files are gone" would delete the database rows
// describing files that are merely temporarily invisible.
var ErrStorageUnhealthy = errors.New("media storage is not verifiably mounted")

type HealthGate struct {
	store      *store.Store
	processor  *media.Processor
	sampleSize int
	healthy    atomic.Bool
}

func NewHealthGate(st *store.Store, proc *media.Processor, sampleSize int) *HealthGate {
	if sampleSize <= 0 {
		sampleSize = 8
	}
	g := &HealthGate{store: st, processor: proc, sampleSize: sampleSize}
	// Starts closed. Nothing may be deleted until a check has actually proven
	// the media volume is mounted.
	g.healthy.Store(false)
	return g
}

// Healthy reports the last observed state without touching the filesystem.
func (g *HealthGate) Healthy() bool { return g.healthy.Load() }

// Check re-evaluates the circuit. It demands positive evidence: at least one
// expected original must be present. An empty gallery is trivially healthy
// because there is nothing that could be missing.
func (g *HealthGate) Check(ctx context.Context) error {
	names, err := g.store.SampleStoredFilenames(ctx, g.sampleSize)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		g.setHealthy(true)
		return nil
	}

	for _, name := range names {
		if _, err := os.Stat(g.processor.OriginalPath(name)); err == nil {
			g.setHealthy(true)
			return nil
		}
	}

	g.setHealthy(false)
	return fmt.Errorf("%w: none of %d sampled originals exist under %s",
		ErrStorageUnhealthy, len(names), g.processor.OriginalsDir())
}

func (g *HealthGate) setHealthy(now bool) {
	if was := g.healthy.Swap(now); was != now {
		if now {
			// Info, not Warn: the first transition is a normal process start.
			slog.Info("storage health circuit closed", "operation", "storage_health", "healthy", true)
		} else {
			slog.Error("storage health circuit opened; refusing uploads and all deletions",
				"operation", "storage_health", "healthy", false,
				"remediation", "verify the media bind mount is attached at "+g.processor.MediaDir)
		}
	}
}
