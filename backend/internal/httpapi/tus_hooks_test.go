package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"event-gallery/backend/internal/ingest"
	"event-gallery/backend/internal/models"
	"event-gallery/backend/internal/store"
)

func writeTestJPEGFile(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write jpeg: %v", err)
	}
}

func hookRequestBody(t *testing.T, req tusHookRequest) []byte {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal hook request: %v", err)
	}
	return b
}

func TestTusHook_UnauthorizedWithoutSecret(t *testing.T) {
	h := newTestHarness(t)
	req := newTestRequest(http.MethodPost, "/api/internal/tus-hooks", []byte(`{"Type":"post-create"}`))
	rec := serveRequest(h, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestTusHook_PreCreate_RejectsOversized(t *testing.T) {
	h := newTestHarness(t)
	body := hookRequestBody(t, tusHookRequest{
		Type: "pre-create",
		Event: tusHookEvent{
			Upload: tusHookUpload{
				Size:     h.cfg.MaxUploadBytes + 1,
				MetaData: map[string]string{"filename": "big.jpg"},
			},
		},
	})
	req := newRequestWithHeader(http.MethodPost, "/api/internal/tus-hooks", body, internalProxySecretHeader, h.cfg.TusHookSecret)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusOK { // hook endpoint always 200s; rejection is in the JSON body
		t.Fatalf("expected 200 (hook envelope), got %d", rec.Code)
	}
	var resp tusHookResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.RejectUpload {
		t.Fatal("expected RejectUpload true for oversized upload")
	}
}

func TestTusHook_PreCreate_RejectsMissingFilename(t *testing.T) {
	h := newTestHarness(t)
	body := hookRequestBody(t, tusHookRequest{
		Type: "pre-create",
		Event: tusHookEvent{
			Upload: tusHookUpload{Size: 100, MetaData: map[string]string{}},
		},
	})
	req := newRequestWithHeader(http.MethodPost, "/api/internal/tus-hooks", body, internalProxySecretHeader, h.cfg.TusHookSecret)
	rec := serveRequest(h, req)
	var resp tusHookResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.RejectUpload {
		t.Fatal("expected RejectUpload true for missing filename")
	}
}

func TestTusHook_PreCreate_AllowsValid(t *testing.T) {
	h := newTestHarness(t)
	body := hookRequestBody(t, tusHookRequest{
		Type: "pre-create",
		Event: tusHookEvent{
			Upload: tusHookUpload{Size: 100, MetaData: map[string]string{"filename": "a.jpg"}},
		},
	})
	req := newRequestWithHeader(http.MethodPost, "/api/internal/tus-hooks", body, internalProxySecretHeader, h.cfg.TusHookSecret)
	rec := serveRequest(h, req)
	var resp tusHookResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.RejectUpload {
		t.Fatal("expected valid upload to be allowed")
	}
	if resp.ChangeFileInfo == nil || resp.ChangeFileInfo.ID == "" {
		t.Fatal("pre-create must generate the upload id")
	}
}

func postHook(t *testing.T, h *testHarness, req tusHookRequest) tusHookResponse {
	t.Helper()
	httpReq := newRequestWithHeader(http.MethodPost, "/api/internal/tus-hooks",
		hookRequestBody(t, req), internalProxySecretHeader, h.cfg.TusHookSecret)
	rec := serveRequest(h, httpReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("hook endpoint must always answer 200, got %d", rec.Code)
	}
	var resp tusHookResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode hook response: %v", err)
	}
	return resp
}

func TestPreCreateGeneratesIDAndJob(t *testing.T) {
	h := newTestHarness(t)

	resp := postHook(t, h, tusHookRequest{
		Type: "pre-create",
		Event: tusHookEvent{
			Upload: tusHookUpload{
				Size:     10,
				MetaData: map[string]string{"filename": "a.jpg", "sha256": "ABC", "guestName": "Ana"},
			},
			HTTPRequest: tusHookHTTPRequest{
				RemoteAddr: "10.0.0.9",
				Header:     map[string][]string{clientIPHeader: {"203.0.113.5"}},
			},
		},
	})

	if resp.RejectUpload {
		t.Fatal("valid upload must be admitted")
	}
	if resp.ChangeFileInfo == nil || resp.ChangeFileInfo.ID == "" {
		t.Fatal("pre-create must generate the upload id")
	}

	job, err := h.store.GetUploadJob(context.Background(), resp.ChangeFileInfo.ID)
	if err != nil || job == nil {
		t.Fatalf("uploading row must exist: %+v %v", job, err)
	}
	if job.Status != store.JobUploading || job.ExpectedSize != 10 {
		t.Errorf("unexpected job %+v", job)
	}
	if job.DeclaredSHA256 != "abc" {
		t.Errorf("declared sha256 = %q, want lowercased", job.DeclaredSHA256)
	}
	if job.MediaID == "" {
		t.Error("media id must be allocated at admission")
	}
	if job.GuestName != "Ana" || job.OriginalFilename != "a.jpg" {
		t.Errorf("metadata not recorded: %+v", job)
	}
	// The forwarded client IP wins over tusd's own connection address, which
	// is always the backend's proxy hop.
	if job.UploaderIP != "203.0.113.5" {
		t.Errorf("uploader ip = %q, want the forwarded client address", job.UploaderIP)
	}
}

// Two admissions must never collide on an id, because tusd would then open an
// existing data or sidecar path.
func TestPreCreateGeneratesADistinctIDPerUpload(t *testing.T) {
	h := newTestHarness(t)
	req := tusHookRequest{
		Type:  "pre-create",
		Event: tusHookEvent{Upload: tusHookUpload{Size: 10, MetaData: map[string]string{"filename": "a.jpg"}}},
	}
	first := postHook(t, h, req)
	second := postHook(t, h, req)
	// Assert admission before comparing: a colliding id fails the INSERT and
	// comes back as backpressure with no ChangeFileInfo, and dereferencing that
	// would panic the whole test binary instead of reporting this one failure.
	if first.ChangeFileInfo == nil || second.ChangeFileInfo == nil {
		t.Fatalf("both admissions must return an id, got %+v and %+v", first, second)
	}
	if first.ChangeFileInfo.ID == second.ChangeFileInfo.ID {
		t.Fatalf("two admissions produced the same upload id %q", first.ChangeFileInfo.ID)
	}
}

// Migration 0004 puts CHECK (expected_size > 0) on upload_jobs. A deferred or
// negative length must be refused at the boundary so that constraint can never
// reach a guest as an opaque 500 from a failed INSERT.
func TestPreCreateRejectsDeferredSize(t *testing.T) {
	for name, size := range map[string]int64{"deferred": 0, "negative": -1} {
		t.Run(name, func(t *testing.T) {
			h := newTestHarness(t)

			resp := postHook(t, h, tusHookRequest{
				Type:  "pre-create",
				Event: tusHookEvent{Upload: tusHookUpload{Size: size, MetaData: map[string]string{"filename": "a.jpg"}}},
			})

			if !resp.RejectUpload || resp.HTTPResponse == nil || resp.HTTPResponse.StatusCode != http.StatusBadRequest {
				t.Errorf("deferred size must be a deterministic 400, got %+v", resp)
			}
			if resp.ChangeFileInfo != nil {
				t.Error("a rejected upload must not be given an id")
			}
			assertNoUploadJobs(t, h)
		})
	}
}

func TestPreCreateRejectionsRecordNoJob(t *testing.T) {
	cases := map[string]struct {
		upload tusHookUpload
		status int
	}{
		"oversized": {
			upload: tusHookUpload{Size: 1 << 40, MetaData: map[string]string{"filename": "a.jpg"}},
			status: http.StatusRequestEntityTooLarge,
		},
		"missing filename": {
			upload: tusHookUpload{Size: 10, MetaData: map[string]string{"filename": "   "}},
			status: http.StatusBadRequest,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newTestHarness(t)
			resp := postHook(t, h, tusHookRequest{Type: "pre-create", Event: tusHookEvent{Upload: tc.upload}})
			if !resp.RejectUpload || resp.HTTPResponse == nil || resp.HTTPResponse.StatusCode != tc.status {
				t.Errorf("want deterministic %d, got %+v", tc.status, resp)
			}
			assertNoUploadJobs(t, h)
		})
	}
}

// Backpressure is not a client error: the browser must be told to retry, and
// tusd only relays a chosen status through a 2xx envelope.
func TestPreCreateReturnsRetryableBackpressureBeforeReady(t *testing.T) {
	h := newTestHarness(t)
	// A manager that has not run its startup inventory yet: exactly the state
	// the app is in for the first moments after a restart.
	h.server.SetIngest(ingest.New(h.store, h.proc, ingest.Options{UploadDir: h.cfg.TusUploadDir}))

	resp := postHook(t, h, tusHookRequest{
		Type:  "pre-create",
		Event: tusHookEvent{Upload: tusHookUpload{Size: 10, MetaData: map[string]string{"filename": "a.jpg"}}},
	})

	assertRetryable(t, resp)
	assertNoUploadJobs(t, h)
}

func TestPreCreateRefusesWhileStorageIsUnhealthy(t *testing.T) {
	h := newTestHarness(t)
	// A row whose original is absent is what an unmounted media volume looks
	// like, so the gate opens the circuit on the next check.
	insertMediaRowForHealth(t, h, "m1", "missing.jpg", "sha-missing")
	if err := h.ingest.Health().Check(context.Background()); err == nil {
		t.Fatal("health check should have failed with the original missing")
	}

	resp := postHook(t, h, tusHookRequest{
		Type:  "pre-create",
		Event: tusHookEvent{Upload: tusHookUpload{Size: 10, MetaData: map[string]string{"filename": "a.jpg"}}},
	})

	assertRetryable(t, resp)
	assertNoUploadJobs(t, h)
}

// The hook budget bounds admission. A misconfigured or unset budget must
// degrade to "no app-level bound", because context.WithTimeout(ctx, 0) yields
// an already-expired context that would refuse every upload as transient.
func TestPreCreateAdmitsWithNoDurabilityBudgetConfigured(t *testing.T) {
	h := newTestHarness(t)
	h.cfg.UploadDurabilityWait = 0

	resp := postHook(t, h, tusHookRequest{
		Type:  "pre-create",
		Event: tusHookEvent{Upload: tusHookUpload{Size: 10, MetaData: map[string]string{"filename": "a.jpg"}}},
	})

	if resp.RejectUpload || resp.ChangeFileInfo == nil {
		t.Fatalf("an unset budget must not refuse uploads, got %+v", resp)
	}
}

func TestPreCreateRefusesWhenTheFreeSpaceFloorWouldBeCrossed(t *testing.T) {
	h := newTestHarness(t)
	// A floor no filesystem can satisfy, so the refusal does not depend on the
	// test machine's free space.
	manager := ingest.New(h.store, h.proc, ingest.Options{
		UploadDir:    h.cfg.TusUploadDir,
		MinFreeBytes: 1 << 62,
	})
	manager.Start()
	t.Cleanup(manager.Stop)
	h.server.SetIngest(manager)

	resp := postHook(t, h, tusHookRequest{
		Type:  "pre-create",
		Event: tusHookEvent{Upload: tusHookUpload{Size: 10, MetaData: map[string]string{"filename": "a.jpg"}}},
	})

	assertRetryable(t, resp)
	assertNoUploadJobs(t, h)
}

// post-finish is unordered and non-blocking; tusd may never re-deliver it. It
// must therefore do nothing but nudge the queue. Processing here, on a
// request context tusd cancels seconds later, is the production incident.
func TestPostFinishOnlyNudgesTheQueue(t *testing.T) {
	h := newTestHarness(t)
	dataPath := filepath.Join(h.cfg.TusUploadDir, "abc123")
	writeTestJPEGFile(t, dataPath, 60, 40)
	infoPath := dataPath + ".info"
	if err := os.WriteFile(infoPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := postHook(t, h, tusHookRequest{
		Type: "post-finish",
		Event: tusHookEvent{Upload: tusHookUpload{
			ID:       "abc123",
			Size:     10,
			MetaData: map[string]string{"filename": "event.jpg"},
			Storage:  tusHookStorage{Type: "filestore", Path: dataPath},
		}},
	})

	if resp.RejectUpload {
		t.Error("post-finish cannot reject anything; the bytes are already written")
	}
	if _, err := os.Stat(dataPath); err != nil {
		t.Errorf("post-finish must never remove the source: %v", err)
	}
	if _, err := os.Stat(infoPath); err != nil {
		t.Errorf("post-finish must never remove the sidecar: %v", err)
	}
	galRec := doRequest(h, http.MethodGet, "/api/gallery", nil)
	var galResp galleryResponse
	json.Unmarshal(galRec.Body.Bytes(), &galResp)
	if len(galResp.Items) != 0 {
		t.Errorf("post-finish must not publish media, got %d items", len(galResp.Items))
	}
}

func assertRetryable(t *testing.T, resp tusHookResponse) {
	t.Helper()
	if !resp.RejectUpload || resp.HTTPResponse == nil {
		t.Fatalf("expected a rejection envelope, got %+v", resp)
	}
	if resp.HTTPResponse.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 so the client retries", resp.HTTPResponse.StatusCode)
	}
	if resp.HTTPResponse.Header["Retry-After"] == "" {
		t.Errorf("backpressure must carry Retry-After, got %+v", resp.HTTPResponse.Header)
	}
}

func assertNoUploadJobs(t *testing.T, h *testHarness) {
	t.Helper()
	var n int
	if err := h.store.DB().QueryRow(`SELECT COUNT(*) FROM upload_jobs`).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if n != 0 {
		t.Errorf("a refused upload must leave no job row, found %d", n)
	}
}

func insertMediaRowForHealth(t *testing.T, h *testHarness, id, storedFilename, sha string) {
	t.Helper()
	err := h.store.InsertMedia(context.Background(), &models.MediaItem{
		ID:               id,
		OriginalFilename: "photo.jpg",
		StoredFilename:   storedFilename,
		Kind:             models.KindImage,
		MimeType:         "image/jpeg",
		SizeBytes:        1,
		SHA256:           sha,
		UploadedAt:       time.Now(),
	})
	if err != nil {
		t.Fatalf("insert media: %v", err)
	}
}
