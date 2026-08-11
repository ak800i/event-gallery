package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"event-gallery/backend/internal/ingest"
	"event-gallery/backend/internal/ratelimit"
	"event-gallery/backend/internal/store"
)

func postStatusRaw(t *testing.T, h *testHarness, ids []string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(uploadStatusRequest{UploadIDs: ids})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return doRequest(h, http.MethodPost, "/api/uploads/status", body)
}

func postStatus(t *testing.T, h *testHarness, ids []string) map[string]uploadStatusEntry {
	t.Helper()
	rec := postStatusRaw(t, h, ids)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp uploadStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Results
}

// stagedJob describes a row planted directly at a queue stage. Only the
// uploading stage has a store constructor, so later stages are reached with a
// direct update; the read path under test sees an ordinary row either way.
type stagedJob struct {
	status         store.JobStatus
	terminalReason string
	resultMediaID  string
	cancelled      bool
	guestName      string
	filename       string
	lastError      string
}

func seedStagedJob(t *testing.T, h *testHarness, uploadID string, j stagedJob) {
	t.Helper()
	filename := j.filename
	if filename == "" {
		filename = "photo.jpg"
	}
	err := h.store.CreateUploadingJob(context.Background(), &store.UploadJob{
		UploadID:         uploadID,
		MediaID:          uploadID + "-media",
		OriginalFilename: filename,
		ExpectedSize:     10,
		GuestName:        j.guestName,
		UploaderIP:       "203.0.113.9",
	})
	if err != nil {
		t.Fatalf("seed job %s: %v", uploadID, err)
	}
	holdFromIngestWorkers(t, h, uploadID)

	var cancelledAt any
	if j.cancelled {
		cancelledAt = store.NowMicros()
	}
	if _, err := h.store.DB().Exec(
		`UPDATE upload_jobs
		    SET status = ?, terminal_reason = ?, result_media_id = ?,
		        cancellation_requested_at = ?, last_error = ?
		  WHERE upload_id = ?`,
		string(j.status), j.terminalReason, j.resultMediaID, cancelledAt, j.lastError, uploadID,
	); err != nil {
		t.Fatalf("stage job %s at %s: %v", uploadID, j.status, err)
	}
}

func TestUploadStatusMapsQueueStatesToPublicStates(t *testing.T) {
	h := newTestHarness(t)
	insertTestMedia(t, h, "tidying-media", repeatChar('a', 64), time.Now())
	insertTestMedia(t, h, "done-media", repeatChar('b', 64), time.Now())

	cases := []struct {
		id          string
		job         stagedJob
		want        string
		wantMediaID string
	}{
		{"in-transit", stagedJob{status: store.JobUploading}, "uploading", ""},
		{"cancel-requested", stagedJob{status: store.JobUploading, cancelled: true}, "cancelled", ""},
		{"queued", stagedJob{status: store.JobPending}, "processing", ""},
		{"working", stagedJob{status: store.JobProcessing}, "processing", ""},
		// Cleanup still owes the source a deletion, but the media row is
		// already committed, so the guest must be told it is published.
		{"tidying", stagedJob{status: store.JobCleanup, resultMediaID: "tidying-media"}, "published", "tidying-media"},
		{"done", stagedJob{status: store.JobComplete, resultMediaID: "done-media"}, "published", "done-media"},
		// A job whose result is somebody else's media row resolved to a duplicate.
		{"same-content", stagedJob{status: store.JobComplete, resultMediaID: "done-media"}, "duplicate", "done-media"},
		{"bad-type", stagedJob{status: store.JobDiscarded, terminalReason: "unsupported_type"}, "failed", ""},
		{"bad-hash", stagedJob{status: store.JobDiscarding, terminalReason: "checksum_mismatch"}, "failed", ""},
		{"abandoned", stagedJob{status: store.JobDiscarded, terminalReason: "cancelled"}, "cancelled", ""},
		{"never-seen", stagedJob{status: store.JobDiscarded, terminalReason: "unobservable"}, "cancelled", ""},
		// Only a reason that genuinely means the guest cancelled may say so;
		// accusing them of cancelling an upload they did not is worse than
		// reporting the failure it actually was.
		{"reason-we-added-later", stagedJob{status: store.JobDiscarded, terminalReason: "quarantined"}, "failed", ""},
		{"reason-we-never-recorded", stagedJob{status: store.JobDiscarded}, "failed", ""},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			seedStagedJob(t, h, tc.id, tc.job)
			got := postStatus(t, h, []string{tc.id})
			if got[tc.id].State != tc.want {
				t.Errorf("state = %q, want %q", got[tc.id].State, tc.want)
			}
			if got[tc.id].MediaID != tc.wantMediaID {
				t.Errorf("mediaId = %q, want %q", got[tc.id].MediaID, tc.wantMediaID)
			}
		})
	}
}

// An upload receipt must not become a back door onto media the gallery itself
// refuses to show.
func TestUploadStatusWithholdsTheMediaIDUntilTheItemIsPubliclyVisible(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	if err := h.store.SetConfig(ctx, store.ConfigKeyApprovalRequired, "true"); err != nil {
		t.Fatalf("enable approval: %v", err)
	}
	insertTestMedia(t, h, "held-media", repeatChar('c', 64), time.Now())
	seedStagedJob(t, h, "held", stagedJob{status: store.JobComplete, resultMediaID: "held-media"})

	got := postStatus(t, h, []string{"held"})
	if got["held"].State != "published" {
		t.Errorf("state = %q, want published: the media row is committed", got["held"].State)
	}
	if got["held"].MediaID != "" {
		t.Errorf("mediaId = %q, want empty: an item awaiting approval must not be addressable through an upload receipt", got["held"].MediaID)
	}
}

// An upload id is a capability, so possessing one must reveal the progress of
// that upload and nothing else about it.
func TestUploadStatusRevealsNothingBeyondTheState(t *testing.T) {
	h := newTestHarness(t)
	seedStagedJob(t, h, "u1", stagedJob{
		status:    store.JobProcessing,
		guestName: "Bartholomew Nightingale",
		filename:  "wedding-speech-draft.jpg",
		lastError: "prepare original: no space left on device",
	})

	rec := postStatusRaw(t, h, []string{"u1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"results":{"u1":{"state":"processing"}}}` {
		t.Errorf("body = %s, want the state alone", body)
	}
	for _, private := range []string{
		"Bartholomew", "wedding-speech-draft", "203.0.113.9", "no space left", "u1-media",
	} {
		if strings.Contains(rec.Body.String(), private) {
			t.Errorf("response disclosed %q", private)
		}
	}
}

func TestUploadStatusRejectsBatchesOutsideTheAllowedRange(t *testing.T) {
	h := newTestHarness(t)
	cases := []struct {
		name string
		n    int
		want int
	}{
		{"empty", 0, http.StatusBadRequest},
		{"at the limit", 100, http.StatusOK},
		{"over the limit", 101, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids := make([]string, tc.n)
			for i := range ids {
				ids[i] = fmt.Sprintf("u%d", i)
			}
			if rec := postStatusRaw(t, h, ids); rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestUploadStatusReportsUnknownPerID(t *testing.T) {
	h := newTestHarness(t)
	got := postStatus(t, h, []string{"never-existed"})
	if got["never-existed"].State != "unknown" {
		t.Errorf("state = %q, want unknown", got["never-existed"].State)
	}
}

// Failing to read the queue is a transient database condition, not a verdict
// about the upload, so it has to be retryable and must not describe itself.
func TestUploadStatusIsRetryableWhenTheQueueCannotBeRead(t *testing.T) {
	h := newTestHarness(t)
	body, err := json.Marshal(uploadStatusRequest{UploadIDs: []string{"u1"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := serveRequest(h, httptest.NewRequest(
		http.MethodPost, "/api/uploads/status", bytes.NewReader(body)).WithContext(ctx))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a transient refusal must carry Retry-After")
	}
	if strings.Contains(rec.Body.String(), "context canceled") {
		t.Errorf("the response disclosed the internal error: %s", rec.Body.String())
	}
}

// A failed media lookup is the same transient database condition as a failed
// queue read. Rendering it as a published entry whose mediaId is quietly
// absent would make "the database is unhappy" indistinguishable from "the item
// is not publicly visible", and hand the caller a deterministic-looking 200.
func TestUploadStatusIsRetryableWhenTheMediaLookupFails(t *testing.T) {
	h := newTestHarness(t)
	insertTestMedia(t, h, "visible-media", repeatChar('d', 64), time.Now())
	seedStagedJob(t, h, "shy", stagedJob{status: store.JobComplete, resultMediaID: "visible-media"})

	// The visibility query counts likes, so losing that table breaks the media
	// lookup alone; the queue read is untouched and still answers.
	if _, err := h.store.DB().Exec(`DROP TABLE likes`); err != nil {
		t.Fatalf("drop likes: %v", err)
	}

	rec := postStatusRaw(t, h, []string{"shy"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a transient refusal must carry Retry-After")
	}
	if strings.Contains(rec.Body.String(), "likes") {
		t.Errorf("the response disclosed the internal error: %s", rec.Body.String())
	}
}

// Before the startup inventory finishes, an id with no row may simply not have
// been adopted yet. Calling that "unknown" would tell a guest their upload is
// lost while it is being recovered.
func TestUploadStatusReportsRecoveringWhileTheInventoryRuns(t *testing.T) {
	h := newTestHarness(t)
	seedStagedJob(t, h, "adopted", stagedJob{status: store.JobPending})
	h.server.SetIngest(ingest.New(h.store, h.proc, ingest.Options{UploadDir: h.cfg.TusUploadDir}))

	got := postStatus(t, h, []string{"stranded", "adopted"})
	if got["stranded"].State != "recovering" {
		t.Errorf("state = %q, want recovering", got["stranded"].State)
	}
	if got["adopted"].State != "processing" {
		t.Errorf("state = %q, want processing: a row that exists is reported from the row", got["adopted"].State)
	}
}

// The whole point of a second limiter is that a tab polling its uploads cannot
// spend the budget the gallery needs.
func TestUploadStatusPollingDoesNotDrainTheGalleryBudget(t *testing.T) {
	h := newTestHarness(t)
	h.server.uploadStatusLimiter = ratelimit.NewKeyedLimiter(0, 0, time.Minute)

	if rec := postStatusRaw(t, h, []string{"whatever"}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: the status route must sit behind its own limiter", rec.Code)
	}
	if rec := doRequest(h, http.MethodGet, "/api/gallery", nil); rec.Code != http.StatusOK {
		t.Errorf("gallery = %d, want 200: an exhausted status bucket must not close the gallery", rec.Code)
	}
}

func TestExhaustedGalleryBudgetDoesNotStopStatusPolling(t *testing.T) {
	h := newTestHarness(t)
	h.server.publicLimiter = ratelimit.NewKeyedLimiter(0, 0, time.Minute)

	if rec := doRequest(h, http.MethodGet, "/api/gallery", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("gallery = %d, want 429", rec.Code)
	}
	if rec := postStatusRaw(t, h, []string{"whatever"}); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: status polling must not draw on the gallery bucket", rec.Code)
	}
}
