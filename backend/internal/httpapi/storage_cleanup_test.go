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
)

func writeTusPartial(t *testing.T, dir, id string, size, offset int64, age time.Duration) {
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
