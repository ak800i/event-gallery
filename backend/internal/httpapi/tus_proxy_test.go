package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newFakeTusd(t *testing.T) (*httptest.Server, *[]http.Request) {
	t.Helper()
	var received []http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clone := r.Clone(r.Context())
		received = append(received, *clone)
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Location", "http://example.invalid/files/new-upload-id")
			w.WriteHeader(http.StatusCreated)
		case http.MethodPatch:
			w.Header().Set("Upload-Offset", "100")
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &received
}

func TestTusProxy_RewritesPathAndInjectsSecret(t *testing.T) {
	h := newTestHarness(t)
	fake, received := newFakeTusd(t)
	h.withTusTarget(t, fake.URL)

	req := httptest.NewRequest(http.MethodPost, "/api/tus/", nil)
	req.Header.Set("Upload-Length", "1000")
	rec := serveRequest(h, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(*received) != 1 {
		t.Fatalf("expected exactly 1 request reaching tusd, got %d", len(*received))
	}
	got := (*received)[0]
	if got.URL.Path != "/files/" {
		t.Errorf("expected rewritten path /files/, got %s", got.URL.Path)
	}
	if got.Header.Get(internalProxySecretHeader) != h.cfg.TusHookSecret {
		t.Errorf("expected internal proxy secret header to be injected")
	}
}

func TestTusProxy_RewritesLocationHeader(t *testing.T) {
	h := newTestHarness(t)
	fake, _ := newFakeTusd(t)
	h.withTusTarget(t, fake.URL)

	// tusd replies with an absolute Location pointing at its own (internal)
	// address. The proxy must rewrite it to this backend's public tus route
	// so the guest's browser keeps talking to us, not directly to tusd.
	req := httptest.NewRequest(http.MethodPost, "/api/tus/", nil)
	req.Header.Set("Upload-Length", "1000")
	rec := serveRequest(h, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/api/tus/new-upload-id" {
		t.Errorf("expected rewritten Location /api/tus/new-upload-id, got %q", got)
	}
}

func TestTusProxy_BlocksNewUploadsWhenExpired(t *testing.T) {
	h := newTestHarness(t)
	fake, received := newFakeTusd(t)
	h.withTusTarget(t, fake.URL)

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	if err := h.store.SetConfig(t.Context(), "upload_expires_at", past); err != nil {
		t.Fatalf("set config: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/tus/", nil)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when uploads closed, got %d", rec.Code)
	}
	if len(*received) != 0 {
		t.Fatalf("expected request to never reach tusd, got %d", len(*received))
	}
}

func TestTusProxy_AllowsNewUploadsWithoutExpiry(t *testing.T) {
	h := newTestHarness(t)
	fake, _ := newFakeTusd(t)
	h.withTusTarget(t, fake.URL)

	req := httptest.NewRequest(http.MethodPost, "/api/tus/", nil)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}
}

func TestTusProxy_EnforcesUploadConcurrencyPerIP(t *testing.T) {
	h := newTestHarness(t)
	// Concurrency limit is 2 (see testharness config). Manually hold both
	// slots to simulate two in-flight uploads from the same IP, then verify
	// a third PATCH is rejected.
	rel1, ok1 := h.server.uploadConcurrency.TryAcquire("192.0.2.1")
	rel2, ok2 := h.server.uploadConcurrency.TryAcquire("192.0.2.1")
	if !ok1 || !ok2 {
		t.Fatalf("expected to acquire both slots")
	}
	defer rel1()
	defer rel2()

	fake, _ := newFakeTusd(t)
	h.withTusTarget(t, fake.URL)

	req := httptest.NewRequest(http.MethodPatch, "/api/tus/some-id", nil)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when concurrency limit reached, got %d", rec.Code)
	}
}
