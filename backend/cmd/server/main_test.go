package main

import (
	"context"
	"testing"
	"time"

	"event-gallery/backend/internal/config"
	"event-gallery/backend/internal/ingest"
)

type stubTerminator struct{}

func (stubTerminator) Terminate(context.Context, string) error { return nil }

// Every value is distinct so a field crossed with its neighbour cannot pass.
// This mapping is the whole of the wiring that a compiler cannot check: the
// types repeat (three int-ish, four durations), so a transposition here is
// silent at runtime and would only surface as, say, durability commits running
// on the media-processing worker count.
func TestIngestOptionsMapsEveryConfigField(t *testing.T) {
	term := stubTerminator{}
	cfg := &config.Config{
		MediaProcessingWorkers:  3,
		UploadDurabilityWorkers: 5,
		MediaProcessingTimeout:  41 * time.Minute,
		UploadRetryMaxBackoff:   43 * time.Minute,
		IngestReconcileInterval: 47 * time.Second,
		UploadJobRetention:      53 * time.Hour,
		TusUploadDir:            "/data/tusd-incoming",
		IngestMinFreeBytes:      1234567,
	}

	opts := ingestOptions(cfg, term)

	if opts.Workers != 3 {
		t.Errorf("Workers = %d, want 3 (MediaProcessingWorkers)", opts.Workers)
	}
	if opts.DurabilityWorkers != 5 {
		t.Errorf("DurabilityWorkers = %d, want 5 (UploadDurabilityWorkers)", opts.DurabilityWorkers)
	}
	if opts.ProcessingTimeout != 41*time.Minute {
		t.Errorf("ProcessingTimeout = %v, want 41m (MediaProcessingTimeout)", opts.ProcessingTimeout)
	}
	if opts.MaxBackoff != 43*time.Minute {
		t.Errorf("MaxBackoff = %v, want 43m (UploadRetryMaxBackoff)", opts.MaxBackoff)
	}
	if opts.ReconcileInterval != 47*time.Second {
		t.Errorf("ReconcileInterval = %v, want 47s (IngestReconcileInterval)", opts.ReconcileInterval)
	}
	if opts.JobRetention != 53*time.Hour {
		t.Errorf("JobRetention = %v, want 53h (UploadJobRetention)", opts.JobRetention)
	}
	if opts.UploadDir != "/data/tusd-incoming" {
		t.Errorf("UploadDir = %q, want the configured TusUploadDir", opts.UploadDir)
	}
	if opts.MinFreeBytes != 1234567 {
		t.Errorf("MinFreeBytes = %d, want 1234567 (IngestMinFreeBytes)", opts.MinFreeBytes)
	}
	if opts.Terminator != ingest.SourceTerminator(term) {
		t.Error("Terminator was not the value passed in")
	}
}

// The manager may never be handed a nil terminator: claimAndRunOnce calls it to
// remove a discarded source, so a nil here is a panic in a worker goroutine
// rather than a compile error at the call site.
func TestIngestOptionsCarriesTerminator(t *testing.T) {
	if opts := ingestOptions(&config.Config{}, nil); opts.Terminator != nil {
		t.Fatal("expected the nil terminator to pass through unchanged")
	}
}
