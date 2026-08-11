package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"event-gallery/backend/internal/store"
)

func writeTusPartial(t *testing.T, dir, id string, size, offset int64, age time.Duration) {
	t.Helper()
	writeTusPartialWithSidecar(t, dir, id, size, offset, age, nil)
}

// mutate, when non-nil, corrupts the sidecar before it is written.
func writeTusPartialWithSidecar(t *testing.T, dir, id string, size, offset int64, age time.Duration, mutate func(info map[string]any)) {
	t.Helper()
	dataPath := filepath.Join(dir, id)
	infoPath := dataPath + ".info"
	if err := os.WriteFile(dataPath, make([]byte, offset), 0o640); err != nil {
		t.Fatal(err)
	}
	info := map[string]any{
		"ID": id, "Size": size, "SizeIsDeferred": false,
		"Storage": map[string]any{"Type": "filestore", "Path": dataPath, "InfoPath": infoPath},
	}
	if mutate != nil {
		mutate(info)
	}
	raw, _ := json.Marshal(info)
	if err := os.WriteFile(infoPath, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(dataPath, when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(infoPath, when, when); err != nil {
		t.Fatal(err)
	}
}

func TestTusCleanupDeletesStalePartialThroughTusd(t *testing.T) {
	h := newTestHarness(t)
	writeTusPartial(t, h.cfg.TusUploadDir, "stale", 100, 10, 72*time.Hour)
	var calls atomic.Int32
	fakeTusd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodDelete || r.URL.Path != "/files/stale" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Tus-Resumable") != "1.0.0" || r.Header.Get(internalProxySecretHeader) != h.cfg.TusHookSecret {
			t.Fatal("missing maintenance headers")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fakeTusd.Close()
	h.cfg.TusInternalURL = fakeTusd.URL
	h.server.cleanupIncompleteTusUploads(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("expected one DELETE, got %d", calls.Load())
	}
	// Only tusd may unlink these files; the fake intentionally leaves them.
	if _, err := os.Stat(filepath.Join(h.cfg.TusUploadDir, "stale")); err != nil {
		t.Fatalf("cleaner directly removed data: %v", err)
	}
}

func TestTusCleanupRetainsFreshAndCompleteUploads(t *testing.T) {
	h := newTestHarness(t)
	writeTusPartial(t, h.cfg.TusUploadDir, "fresh", 100, 10, time.Hour)
	writeTusPartial(t, h.cfg.TusUploadDir, "complete", 10, 10, 72*time.Hour)
	var calls atomic.Int32
	fakeTusd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fakeTusd.Close()
	h.cfg.TusInternalURL = fakeTusd.URL
	h.server.cleanupIncompleteTusUploads(context.Background())
	if calls.Load() != 0 {
		t.Fatalf("fresh/complete uploads must be retained, got %d DELETEs", calls.Load())
	}
}

func TestTusCleanupUsesDataFileActivity(t *testing.T) {
	h := newTestHarness(t)
	writeTusPartial(t, h.cfg.TusUploadDir, "resumed", 100, 10, 72*time.Hour)
	dataPath := filepath.Join(h.cfg.TusUploadDir, "resumed")
	now := time.Now()
	if err := os.Chtimes(dataPath, now, now); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	fakeTusd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fakeTusd.Close()
	h.cfg.TusInternalURL = fakeTusd.URL
	h.server.cleanupIncompleteTusUploads(context.Background())
	if calls.Load() != 0 {
		t.Fatal("recent data-file activity must prevent expiration")
	}
}

func TestTusCleanupRetainsSidecarsWithMismatchedIdentity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(dir string, info map[string]any)
	}{
		{"id", func(dir string, info map[string]any) { info["ID"] = "someone-else" }},
		{"path", func(dir string, info map[string]any) {
			info["Storage"].(map[string]any)["Path"] = filepath.Join(dir, "someone-else")
		}},
		{"infopath", func(dir string, info map[string]any) {
			info["Storage"].(map[string]any)["InfoPath"] = filepath.Join(dir, "someone-else.info")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHarness(t)
			dir := h.cfg.TusUploadDir
			writeTusPartialWithSidecar(t, dir, "stale", 100, 10, 72*time.Hour, func(info map[string]any) {
				tc.mutate(dir, info)
			})
			var calls atomic.Int32
			fakeTusd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer fakeTusd.Close()
			h.cfg.TusInternalURL = fakeTusd.URL
			h.server.cleanupIncompleteTusUploads(context.Background())
			if calls.Load() != 0 {
				t.Fatalf("sidecar that does not describe this upload must never be deleted, got %d DELETEs", calls.Load())
			}
		})
	}
}

// countingTusd stands in for tusd and records how many DELETEs it received.
func countingTusd(t *testing.T, h *testHarness, calls *atomic.Int32) {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(fake.Close)
	h.cfg.TusInternalURL = fake.URL
}

func seedUploadingJob(t *testing.T, h *testHarness, uploadID string) {
	t.Helper()
	err := h.store.CreateUploadingJob(context.Background(), &store.UploadJob{
		UploadID:         uploadID,
		MediaID:          uploadID + "-media",
		OriginalFilename: "photo.jpg",
		ExpectedSize:     100,
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	holdFromIngestWorkers(t, h, uploadID)
}

func TestTusCleanupClaimsTheRowBeforeDeleting(t *testing.T) {
	h := newTestHarness(t)
	writeTusPartial(t, h.cfg.TusUploadDir, "stale", 100, 10, 72*time.Hour)
	seedUploadingJob(t, h, "stale")
	var calls atomic.Int32
	countingTusd(t, h, &calls)

	h.server.cleanupIncompleteTusUploads(context.Background())

	if calls.Load() != 1 {
		t.Fatalf("expected one DELETE, got %d", calls.Load())
	}
	job, err := h.store.GetUploadJob(context.Background(), "stale")
	if err != nil || job == nil {
		t.Fatalf("job row: %+v %v", job, err)
	}
	if job.Status != store.JobDiscarding {
		t.Errorf("status = %q, want discarding: the row must be claimed before the files go", job.Status)
	}
}

// The janitor decides an upload is abandoned, and before it acts the final
// PATCH lands and the queue takes ownership of the source. Deleting it then
// would destroy a file the app has already promised to publish.
func TestTusCleanupWillNotDeleteAnUploadTheQueueOwns(t *testing.T) {
	for _, status := range []store.JobStatus{store.JobPending, store.JobProcessing} {
		t.Run(string(status), func(t *testing.T) {
			h := newTestHarness(t)
			writeTusPartial(t, h.cfg.TusUploadDir, "stale", 100, 10, 72*time.Hour)
			seedUploadingJob(t, h, "stale")
			if _, err := h.store.DB().Exec(`UPDATE upload_jobs SET status = ? WHERE upload_id = ?`, string(status), "stale"); err != nil {
				t.Fatalf("advance status: %v", err)
			}
			var calls atomic.Int32
			countingTusd(t, h, &calls)

			h.server.cleanupIncompleteTusUploads(context.Background())

			if calls.Load() != 0 {
				t.Fatalf("an upload the queue owns must never be terminated, got %d DELETEs", calls.Load())
			}
			job, _ := h.store.GetUploadJob(context.Background(), "stale")
			if job.Status != status {
				t.Errorf("status = %q, want %q untouched", job.Status, status)
			}
		})
	}
}

// Terminate is the single seam both the janitor and the ingest workers use. It
// must refuse outright while the queue may still need the source, so a caller
// that forgets to claim the row first cannot delete a live upload. An
// uploading row counts: its bytes are still arriving and its final PATCH may
// hand it to the queue at any moment.
func TestTerminateRefusesSourcesTheQueueStillNeeds(t *testing.T) {
	for _, status := range []store.JobStatus{store.JobUploading, store.JobPending, store.JobProcessing} {
		t.Run(string(status), func(t *testing.T) {
			h := newTestHarness(t)
			seedUploadingJob(t, h, "u1")
			if _, err := h.store.DB().Exec(`UPDATE upload_jobs SET status = ? WHERE upload_id = ?`, string(status), "u1"); err != nil {
				t.Fatalf("advance status: %v", err)
			}
			var calls atomic.Int32
			countingTusd(t, h, &calls)

			if err := h.server.Terminate(context.Background(), "u1"); err == nil {
				t.Fatalf("Terminate must refuse a %s job", status)
			}
			if calls.Load() != 0 {
				t.Fatalf("no DELETE may be issued for a refused termination, got %d", calls.Load())
			}
		})
	}
}

func TestTerminateRemovesADiscardingSource(t *testing.T) {
	h := newTestHarness(t)
	seedUploadingJob(t, h, "u1")
	if err := h.store.ClaimUploadingForDiscard(context.Background(), "u1", store.NowMicros()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	var calls atomic.Int32
	countingTusd(t, h, &calls)

	if err := h.server.Terminate(context.Background(), "u1"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one DELETE, got %d", calls.Load())
	}
}
