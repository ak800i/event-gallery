package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"event-gallery/backend/internal/config"
	"event-gallery/backend/internal/db"
	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/store"
)

// testHarness bundles everything needed to exercise the HTTP API in tests.
type testHarness struct {
	server *Server
	router http.Handler
	store  *store.Store
	cfg    *config.Config
	proc   *media.Processor
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	dir := t.TempDir()

	sqlDB, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	st := store.New(sqlDB)

	proc := media.NewProcessor(filepath.Join(dir, "media"), 200, []string{"image/jpeg", "image/png"}, []string{"video/mp4"})
	if err := proc.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}

	tusDir := filepath.Join(dir, "tus")
	if err := os.MkdirAll(tusDir, 0o750); err != nil {
		t.Fatalf("create tus dir: %v", err)
	}

	cfg := &config.Config{
		ListenAddr:                      ":0",
		AdminPassword:                   "supersecretadminpassword",
		DataDir:                         dir,
		MediaDir:                        filepath.Join(dir, "media"),
		TusInternalURL:                  "http://127.0.0.1:1", // overridden per-test where needed
		TusHookSecret:                   "test-hook-secret",
		TusUploadDir:                    tusDir,
		MaxUploadBytes:                  50 * 1024 * 1024,
		PublicRateLimitPerMinute:        6000,
		PublicRateLimitBurst:            1000,
		UploadConcurrencyPerIP:          2,
		UploadBandwidthPerIPBytesPerSec: 100 * 1024 * 1024,
		GuestNameMaxLength:              60,
		CookieSecure:                    false,
		SessionTTL:                      time.Hour,
		ThumbnailMaxDimension:           200,
		TrashRetention:                  30 * 24 * time.Hour,
		TusIncompleteRetention:          48 * time.Hour,
		StorageCleanupInterval:          time.Hour,
		AllowedImageMIMEs:               []string{"image/jpeg", "image/png"},
		AllowedVideoMIMEs:               []string{"video/mp4"},
	}

	srv, err := NewServer(cfg, st, proc, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	return &testHarness{server: srv, router: srv.Router(), store: st, cfg: cfg, proc: proc}
}

// withTusTarget rebuilds the server's tus reverse proxy to point at a given
// backend URL (used to stand in for tusd via httptest.Server).
func (h *testHarness) withTusTarget(t *testing.T, targetURL string) {
	t.Helper()
	proxy, err := newTusReverseProxy(targetURL, h.cfg.TusHookSecret, h.cfg.TrustedProxyCIDRs)
	if err != nil {
		t.Fatalf("new tus reverse proxy: %v", err)
	}
	h.server.tusProxy = proxy
}

func doRequest(h *testHarness, method, target string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func newRequestWithHeader(method, target string, body []byte, headerKey, headerValue string) *http.Request {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Header.Set(headerKey, headerValue)
	return req
}

func serveRequest(h *testHarness, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}
