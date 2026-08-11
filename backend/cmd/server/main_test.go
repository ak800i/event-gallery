package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"event-gallery/backend/internal/config"
	"event-gallery/backend/internal/db"
	"event-gallery/backend/internal/ingest"
	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/store"
)

// The field is what makes identity observable: an empty struct compares equal
// to every other value of its type, so a mapping that substituted a different
// terminator would still pass the assertion below.
type stubTerminator struct{ name string }

func (stubTerminator) Terminate(context.Context, string) error { return nil }

// Every value is distinct so a field crossed with its neighbour cannot pass.
// This mapping is the whole of the wiring that a compiler cannot check: the
// types repeat (three int-ish, four durations), so a transposition here is
// silent at runtime and would only surface as, say, durability commits running
// on the media-processing worker count.
func TestIngestOptionsMapsEveryConfigField(t *testing.T) {
	term := stubTerminator{name: "the one passed in"}
	cfg := &config.Config{
		MediaProcessingWorkers:  3,
		UploadDurabilityWorkers: 5,
		MediaProcessingTimeout:  41 * time.Minute,
		UploadRetryMaxBackoff:   43 * time.Minute,
		IngestReconcileInterval: 47 * time.Second,
		UploadJobRetention:      53 * time.Hour,
		TusIncompleteRetention:  59 * time.Hour,
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
	// The reconciler reads this as the age at which a data file of unknown
	// standing has stopped being an upload in progress, so a job-retention
	// value arriving here instead would authorise adoption hours early.
	if opts.IncompleteRetention != 59*time.Hour {
		t.Errorf("IncompleteRetention = %v, want 59h (TusIncompleteRetention)", opts.IncompleteRetention)
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

// ingestOptions must pass the terminator it is given through verbatim, never
// substitute a fallback of its own. A caller that forgets to pass the server
// has to surface as a nil, because a quietly filled-in default would leave the
// manager terminating sources through something other than the live server.
func TestIngestOptionsSubstitutesNoTerminator(t *testing.T) {
	if opts := ingestOptions(&config.Config{}, nil); opts.Terminator != nil {
		t.Fatalf("ingestOptions supplied a terminator of its own (%T) instead of passing nil through", opts.Terminator)
	}
}

// The manager is only reachable from the HTTP layer because the wiring hands
// it to the server. Without that, s.ingest stays nil, and the completion
// fence, the pre-create gate and the durability barrier all quietly disable
// themselves while every upload is refused -- the exact inertness this feature
// exists to remove. Nothing in httpapi can catch a regression here, because
// its harness attaches a manager of its own.
//
// /readyz is the assertion because it is the one place the attachment is
// externally visible: a started manager reports ready, and a server that was
// never handed one answers 503 with "ingest is still recovering".
func TestNewServerWithIngestAttachesTheManager(t *testing.T) {
	dir := t.TempDir()
	mediaDir := filepath.Join(dir, "media")
	tusDir := filepath.Join(dir, "tus")
	if err := os.MkdirAll(tusDir, 0o750); err != nil {
		t.Fatalf("create tus dir: %v", err)
	}

	sqlDB, err := db.Open(filepath.Join(dir, "gallery.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })

	proc := media.NewProcessor(mediaDir, 200, []string{"image/jpeg"}, []string{"video/mp4"})
	if err := proc.EnsureDirs(); err != nil {
		t.Fatalf("ensure media dirs: %v", err)
	}

	cfg := &config.Config{
		DataDir:                         dir,
		MediaDir:                        mediaDir,
		TusInternalURL:                  "http://127.0.0.1:1",
		TusHookSecret:                   "test-hook-secret",
		TusUploadDir:                    tusDir,
		PublicRateLimitPerMinute:        6000,
		PublicRateLimitBurst:            1000,
		UploadStatusRateLimitPerMinute:  6000,
		UploadConcurrencyPerIP:          2,
		UploadBandwidthPerIPBytesPerSec: 1 << 20,
		ThumbnailMaxDimension:           200,
		MediaProcessingWorkers:          1,
		MediaProcessingTimeout:          time.Minute,
		UploadDurabilityWait:            5 * time.Second,
		UploadDurabilityWorkers:         1,
		UploadRetryMaxBackoff:           time.Minute,
		IngestReconcileInterval:         time.Hour,
		UploadJobRetention:              time.Hour,
	}

	srv, manager, err := newServerWithIngest(cfg, store.New(sqlDB), proc, nil)
	if err != nil {
		t.Fatalf("wire the server: %v", err)
	}
	manager.Start()
	t.Cleanup(manager.Stop)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200 -- the started manager was not attached to the server: %s",
			rec.Code, rec.Body.String())
	}
}
