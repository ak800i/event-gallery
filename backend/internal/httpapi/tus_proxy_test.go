package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"event-gallery/backend/internal/db"
	"event-gallery/backend/internal/ingest"
	"event-gallery/backend/internal/store"
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

// Upload ids must be hex: safeUploadID rejects anything else, and
// tusUploadIDFromPath would return "" so the handler would never see the job.
const testUploadID = "a1b2c3"

func TestTusUploadIDFromPathAcceptsOnlyGeneratedIdentifiers(t *testing.T) {
	for _, p := range []string{"/api/tus/" + testUploadID, "/api/tus/" + testUploadID + "/"} {
		if got := tusUploadIDFromPath(p); got != testUploadID {
			t.Errorf("tusUploadIDFromPath(%q) = %q, want %q", p, got, testUploadID)
		}
	}
	rejected := []string{
		"/api/tus",
		"/api/tus/",
		"/api/tus/.",
		"/api/tus/../../etc/passwd",
		// One path segment can still carry a Windows separator, which
		// filepath.Join would honour, so path.Base alone is not containment.
		`/api/tus/..\..\etc\passwd`,
		"/api/tus/not-hex-id",
	}
	for _, p := range rejected {
		if got := tusUploadIDFromPath(p); got != "" {
			t.Errorf("tusUploadIDFromPath(%q) = %q, want empty", p, got)
		}
	}
}

func seedUploadingJobSized(t *testing.T, h *testHarness, uploadID string, size int64) {
	t.Helper()
	err := h.store.CreateUploadingJob(context.Background(), &store.UploadJob{
		UploadID:         uploadID,
		MediaID:          "media-" + uploadID,
		OriginalFilename: "a.jpg",
		ExpectedSize:     size,
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
}

// writeSource lays down what tusd's filestore would have written. The sidecar
// is written too because that is what the barrier fsyncs alongside the data.
func writeSource(t *testing.T, h *testHarness, uploadID string, payload []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.cfg.TusUploadDir, uploadID), payload, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(h.cfg.TusUploadDir, uploadID+".info"), []byte(`{"ID":"`+uploadID+`"}`), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

func loadJob(t *testing.T, h *testHarness, uploadID string) *store.UploadJob {
	t.Helper()
	job, err := h.store.GetUploadJob(context.Background(), uploadID)
	if err != nil {
		t.Fatalf("get job %s: %v", uploadID, err)
	}
	return job
}

func requireSourceIntact(t *testing.T, h *testHarness, uploadID string, payload []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(h.cfg.TusUploadDir, uploadID))
	if err != nil {
		t.Fatalf("source %s must still exist: %v", uploadID, err)
	}
	if string(got) != string(payload) {
		t.Fatalf("source %s was modified", uploadID)
	}
}

// saturateDurability parks the durability executor's only slot on a stalled
// database, so every later EnsureDurable is refused as busy while the fence's
// own reads keep working. That is the exact production shape this fence exists
// for: the executor is full, so no success can be reported, and the client
// gives up and cancels.
//
// The parked manager gets its own connection pool because the stall must not
// reach the pool the HTTP handlers read through.
func saturateDurability(t *testing.T, h *testHarness) {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(h.cfg.DataDir, "test.db"))
	if err != nil {
		t.Fatalf("open second pool: %v", err)
	}
	stalledStore := store.New(sqlDB)

	manager := ingest.New(stalledStore, h.proc, ingest.Options{
		Workers:           1,
		DurabilityWorkers: 1,
		ProcessingTimeout: time.Minute,
		MaxBackoff:        time.Minute,
		ReconcileInterval: time.Hour,
		JobRetention:      time.Hour,
		UploadDir:         h.cfg.TusUploadDir,
		Terminator:        h.server,
	})
	manager.Start() // needs the database, so stall only afterwards
	h.server.SetIngest(manager)

	sqlDB.SetMaxOpenConns(1)
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("take the only connection: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		manager.Stop()
		sqlDB.Close()
	})

	const blockerID = "beef01"
	blocker := []byte("blocker bytes")
	seedUploadingJobSized(t, h, blockerID, int64(len(blocker)))
	writeSource(t, h, blockerID, blocker)

	// The slot is taken synchronously before the wait, and the operation then
	// parks on the stalled store, so it still holds that slot once our own
	// budget expires.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := manager.EnsureDurable(ctx, blockerID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocker durability = %v, want the wait to expire with the operation still holding the slot", err)
	}
}

func TestFenceBlocksSuccessUntilDurable(t *testing.T) {
	h := newTestHarness(t)
	// A complete source whose row is still 'uploading' must never be reported
	// as successful, because tusd would otherwise short-circuit an
	// already-complete upload with a plain 204.
	payload := []byte("complete bytes")
	seedUploadingJobSized(t, h, testUploadID, int64(len(payload)))
	writeSource(t, h, testUploadID, payload)

	req := httptest.NewRequest(http.MethodHead, "/api/tus/"+testUploadID, nil)
	rec := httptest.NewRecorder()
	intercepted := h.server.fenceCompletedUpload(rec, req, testUploadID)

	// With a working executor the fence commits the row itself; what it must
	// never do is let the request through while the row says 'uploading'.
	job := loadJob(t, h, testUploadID)
	if job.Status == store.JobUploading {
		t.Fatalf("fence must commit a complete upload, status = %q", job.Status)
	}
	if intercepted {
		t.Fatalf("fence blocked an upload it had just made durable (status %d)", rec.Code)
	}
	requireSourceIntact(t, h, testUploadID, payload)
}

func TestFenceLetsAnIncompleteUploadThrough(t *testing.T) {
	h := newTestHarness(t)
	// Fencing a transfer still in progress would stall every chunk upload.
	seedUploadingJobSized(t, h, testUploadID, 100)
	writeSource(t, h, testUploadID, []byte("partial"))

	req := httptest.NewRequest(http.MethodPatch, "/api/tus/"+testUploadID, nil)
	rec := httptest.NewRecorder()
	if h.server.fenceCompletedUpload(rec, req, testUploadID) {
		t.Fatalf("fence blocked an in-progress upload with %d", rec.Code)
	}
	if job := loadJob(t, h, testUploadID); job.Status != store.JobUploading {
		t.Errorf("status = %q, want uploading: an incomplete source is not durable", job.Status)
	}
}

func TestFenceRefusesUntilIngestIsReady(t *testing.T) {
	h := newTestHarness(t)
	// Before the startup inventory finishes we cannot tell a rowless orphan
	// from an upload not yet inventoried, so nothing may be forwarded.
	h.server.SetIngest(ingest.New(h.store, h.proc, ingest.Options{UploadDir: h.cfg.TusUploadDir}))

	req := httptest.NewRequest(http.MethodHead, "/api/tus/"+testUploadID, nil)
	rec := httptest.NewRecorder()
	if !h.server.fenceCompletedUpload(rec, req, testUploadID) {
		t.Fatal("fence forwarded while the queue was still recovering")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("readiness is transient, so the client must be told to come back")
	}
}

func TestFenceFailsClosedWhenTheRowCannotBeRead(t *testing.T) {
	h := newTestHarness(t)
	payload := []byte("complete bytes")
	seedUploadingJobSized(t, h, testUploadID, int64(len(payload)))
	writeSource(t, h, testUploadID, payload)
	// With the database unreadable we cannot tell whether reporting success
	// would be truthful, and an unnecessary retry is always cheaper than a
	// false success.
	if err := h.store.DB().Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	req := httptest.NewRequest(http.MethodHead, "/api/tus/"+testUploadID, nil)
	rec := httptest.NewRecorder()
	if !h.server.fenceCompletedUpload(rec, req, testUploadID) {
		t.Fatal("fence forwarded an upload whose state it could not read")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("an unreadable database is transient, so the client must be told to come back")
	}
}

func TestFenceIsTerminalForACancelledUpload(t *testing.T) {
	h := newTestHarness(t)
	// The guest cancelled, and a stale retry arrives with the bytes complete.
	// This can never commit, so answering with backpressure would leave the
	// browser retrying on a five-second cadence forever.
	payload := []byte("complete bytes")
	seedUploadingJobSized(t, h, testUploadID, int64(len(payload)))
	writeSource(t, h, testUploadID, payload)
	if err := h.store.RequestCancellation(context.Background(), testUploadID, store.NowMicros()); err != nil {
		t.Fatalf("request cancellation: %v", err)
	}

	req := httptest.NewRequest(http.MethodHead, "/api/tus/"+testUploadID, nil)
	rec := httptest.NewRecorder()
	if !h.server.fenceCompletedUpload(rec, req, testUploadID) {
		t.Fatal("fence forwarded an upload that can never commit")
	}
	// 410 and not 409, 423 or 429: @uppy/tus installs its own onShouldRetry,
	// which retries all three, so none of them is terminal at the browser.
	if rec.Code != http.StatusGone {
		t.Errorf("status = %d, want 410 Gone", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want none: there is nothing to come back for", got)
	}
}

func TestFenceRefusesWhenDurabilityIsSaturated(t *testing.T) {
	h := newTestHarness(t)
	payload := []byte("complete bytes")
	seedUploadingJobSized(t, h, testUploadID, int64(len(payload)))
	writeSource(t, h, testUploadID, payload)
	saturateDurability(t, h)

	req := httptest.NewRequest(http.MethodHead, "/api/tus/"+testUploadID, nil)
	rec := httptest.NewRecorder()
	if !h.server.fenceCompletedUpload(rec, req, testUploadID) {
		t.Fatal("fence forwarded a complete upload the database has not recorded")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: saturation is transient", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("fence must tell the client when to retry")
	}
	if job := loadJob(t, h, testUploadID); job.Status != store.JobUploading {
		t.Errorf("status = %q, want uploading: nothing was committed", job.Status)
	}
}

func TestDeleteAfterDurabilityReturns409(t *testing.T) {
	h := newTestHarness(t)
	seedUploadingJobSized(t, h, testUploadID, 10)
	if err := h.store.PromoteToPending(context.Background(), testUploadID, store.NowMicros()); err != nil {
		t.Fatalf("promote: %v", err)
	}

	rec := doRequest(h, http.MethodDelete, "/api/tus/"+testUploadID, nil)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409: a durable completion is never silently reversed", rec.Code)
	}
	if job := loadJob(t, h, testUploadID); job.CancellationRequestedAt != nil {
		t.Error("a durable upload must not record cancellation intent")
	}
}

func TestDeleteBeforeDurabilityRecordsCancellation(t *testing.T) {
	h := newTestHarness(t)
	partial := []byte("partial")
	seedUploadingJobSized(t, h, testUploadID, 100)
	writeSource(t, h, testUploadID, partial)

	rec := doRequest(h, http.MethodDelete, "/api/tus/"+testUploadID, nil)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	job := loadJob(t, h, testUploadID)
	if job.CancellationRequestedAt == nil {
		t.Error("cancellation intent must be durable")
	}
	if job.Status != store.JobUploading {
		t.Errorf("status = %q: intent is recorded, the discard itself belongs to the queue", job.Status)
	}
	// Removing the source is the janitor's job, and only after it has claimed
	// the row to discarding. A handler must never do it inline.
	requireSourceIntact(t, h, testUploadID, partial)
}

func TestDeleteOfACompleteUploadIsRefused(t *testing.T) {
	h := newTestHarness(t)
	payload := []byte("complete bytes")
	seedUploadingJobSized(t, h, testUploadID, int64(len(payload)))
	writeSource(t, h, testUploadID, payload)

	// The client gave up after backpressure and cancelled. The bytes are
	// complete, so the fence must promote them rather than let the cancellation
	// destroy the guest's only copy.
	rec := doRequest(h, http.MethodDelete, "/api/tus/"+testUploadID, nil)

	if rec.Code == http.StatusNoContent {
		t.Fatal("a complete upload must not be cancellable")
	}
	job := loadJob(t, h, testUploadID)
	if job.CancellationRequestedAt != nil {
		t.Error("a complete upload must not record cancellation intent")
	}
	if job.Status != store.JobPending {
		t.Errorf("status = %q, want pending: the fence commits the bytes it refuses to discard", job.Status)
	}
	requireSourceIntact(t, h, testUploadID, payload)
}

func TestDeleteDuringSaturatedDurabilityKeepsTheSource(t *testing.T) {
	h := newTestHarness(t)
	payload := []byte("complete bytes")
	seedUploadingJobSized(t, h, testUploadID, int64(len(payload)))
	writeSource(t, h, testUploadID, payload)
	// This is the incident in miniature: the executor is saturated, so the
	// client's final PATCH was answered 503, it exhausted its retries, and it
	// is now cancelling an upload whose bytes are all on disk.
	saturateDurability(t, h)

	rec := doRequest(h, http.MethodDelete, "/api/tus/"+testUploadID, nil)

	if rec.Code == http.StatusNoContent {
		t.Fatal("a complete upload was accepted for cancellation while durability was saturated")
	}
	job := loadJob(t, h, testUploadID)
	if job.CancellationRequestedAt != nil {
		t.Error("cancellation intent was recorded for a complete upload")
	}
	if job.Status != store.JobUploading {
		t.Errorf("status = %q, want uploading: nothing could be committed", job.Status)
	}
	requireSourceIntact(t, h, testUploadID, payload)
}

func TestDeleteOfAnUnknownUploadIsAccepted(t *testing.T) {
	h := newTestHarness(t)
	// Nothing was ever admitted under this id, so there is nothing to protect
	// and nothing to record; the client's cancellation is simply satisfied.
	rec := doRequest(h, http.MethodDelete, "/api/tus/"+testUploadID, nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if job := loadJob(t, h, testUploadID); job != nil {
		t.Error("a DELETE must not create an upload job")
	}
}

func TestClientDeleteIsNeverForwardedToTusd(t *testing.T) {
	h := newTestHarness(t)
	var forwarded atomic.Bool
	tusd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer tusd.Close()
	h.withTusTarget(t, tusd.URL)

	payload := []byte("complete bytes")
	seedUploadingJobSized(t, h, testUploadID, int64(len(payload)))
	writeSource(t, h, testUploadID, payload)
	seedUploadingJobSized(t, h, "c0ffee", 100)

	// An id this proxy cannot parse must still never reach tusd's terminate
	// path; that is what makes client termination structurally impossible.
	doRequest(h, http.MethodDelete, "/api/tus/../../etc/passwd", nil)
	doRequest(h, http.MethodDelete, "/api/tus/unknown-id", nil)
	doRequest(h, http.MethodDelete, "/api/tus/", nil)
	doRequest(h, http.MethodDelete, "/api/tus/"+testUploadID, nil) // complete
	doRequest(h, http.MethodDelete, "/api/tus/c0ffee", nil)        // still uploading

	// tusd re-derives the method from this header in its middleware, before it
	// routes, so a POST carrying it would reach terminateUpload with the DELETE
	// interception never having run.
	override := httptest.NewRequest(http.MethodPost, "/api/tus/"+testUploadID, nil)
	override.Header.Set("X-HTTP-Method-Override", http.MethodDelete)
	serveRequest(h, override)

	if forwarded.Load() {
		t.Fatal("a client DELETE reached tusd")
	}
}

func TestMethodOverrideIsRefused(t *testing.T) {
	h := newTestHarness(t)
	var forwarded atomic.Bool
	tusd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer tusd.Close()
	h.withTusTarget(t, tusd.URL)
	seedUploadingJobSized(t, h, testUploadID, 100)

	// This proxy routes on r.Method alone, so a request that means one method
	// and says another has no unambiguous handling here. Refusing it outright
	// is what keeps every check below reading the same method.
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodHead, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/tus/"+testUploadID, nil)
		req.Header.Set("X-HTTP-Method-Override", http.MethodDelete)
		if rec := serveRequest(h, req); rec.Code != http.StatusBadRequest {
			t.Errorf("%s carrying an override = %d, want 400", method, rec.Code)
		}
	}
	if forwarded.Load() {
		t.Error("a request carrying a method override reached tusd")
	}
	if job := loadJob(t, h, testUploadID); job.CancellationRequestedAt != nil {
		t.Error("a refused request must not record cancellation intent")
	}
}

func TestNoForwardedRequestCarriesAMethodOverride(t *testing.T) {
	h := newTestHarness(t)
	fake, received := newFakeTusd(t)
	h.withTusTarget(t, fake.URL)

	// Below the handler's own refusal: whatever reaches the director, tusd
	// must never be handed a method this backend did not authorise. This holds
	// for every forwarded method, not only the one the handler intercepts.
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodHead, http.MethodGet, http.MethodOptions} {
		req := httptest.NewRequest(method, "/api/tus/"+testUploadID, nil)
		req.Header.Set("X-HTTP-Method-Override", http.MethodDelete)
		h.server.tusProxy.proxy.ServeHTTP(httptest.NewRecorder(), req)
	}

	if len(*received) == 0 {
		t.Fatal("nothing reached tusd, so nothing was proven")
	}
	for _, got := range *received {
		if v := got.Header.Get("X-HTTP-Method-Override"); v != "" {
			t.Errorf("%s reached tusd carrying X-HTTP-Method-Override: %q", got.Method, v)
		}
	}
}

func TestADuplicateMethodOverrideStillNeverReachesTusd(t *testing.T) {
	h := newTestHarness(t)
	fake, received := newFakeTusd(t)
	h.withTusTarget(t, fake.URL)
	seedUploadingJobSized(t, h, testUploadID, 100)

	// Header.Get reads only the first value, so an empty first copy slips past
	// the handler's refusal. tusd's own Get would read the same empty string,
	// but only because Del removes every value under the key -- this is the one
	// shape where the director strip, not the refusal, is the guard that holds.
	req := httptest.NewRequest(http.MethodPost, "/api/tus/"+testUploadID, nil)
	req.Header.Add("X-HTTP-Method-Override", "")
	req.Header.Add("X-HTTP-Method-Override", http.MethodDelete)
	serveRequest(h, req)

	if len(*received) == 0 {
		t.Fatal("the request never reached tusd, so the director was not exercised")
	}
	for _, got := range *received {
		if v := got.Header.Values("X-HTTP-Method-Override"); len(v) != 0 {
			t.Errorf("tusd was handed X-HTTP-Method-Override: %q", v)
		}
	}
	if job := loadJob(t, h, testUploadID); job.CancellationRequestedAt != nil {
		t.Error("a forwarded POST must not record cancellation intent")
	}
}

func TestConcurrentDeleteAndCompletionKeepsTheUpload(t *testing.T) {
	h := newTestHarness(t)
	var forwarded atomic.Int64
	tusd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		w.Header().Set("Upload-Offset", "14")
		w.WriteHeader(http.StatusOK)
	}))
	defer tusd.Close()
	h.withTusTarget(t, tusd.URL)

	payload := []byte("complete bytes")
	seedUploadingJobSized(t, h, testUploadID, int64(len(payload)))
	writeSource(t, h, testUploadID, payload)

	// A cancellation and a resume arriving together must not be able to
	// interleave into "cancelled, and the source is gone".
	var wg sync.WaitGroup
	codes := make([]int, 16)
	for i := 0; i < len(codes); i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			method := http.MethodDelete
			if i%2 == 0 {
				method = http.MethodHead
			}
			codes[i] = doRequest(h, method, "/api/tus/"+testUploadID, nil).Code
		}(i)
	}
	wg.Wait()

	job := loadJob(t, h, testUploadID)
	if job.CancellationRequestedAt != nil {
		t.Error("a complete upload was cancelled by a concurrent DELETE")
	}
	if job.Status != store.JobPending {
		t.Errorf("status = %q, want pending", job.Status)
	}
	for i, code := range codes {
		if i%2 == 1 && code == http.StatusNoContent {
			t.Errorf("request %d: DELETE reported success for a complete upload", i)
		}
	}
	requireSourceIntact(t, h, testUploadID, payload)
}
