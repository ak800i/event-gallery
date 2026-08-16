package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"event-gallery/backend/internal/models"
)

func insertTestMedia(t *testing.T, h *testHarness, id, sha string, uploadedAt time.Time) {
	t.Helper()
	err := h.store.InsertMedia(context.Background(), &models.MediaItem{
		ID: id, OriginalFilename: "photo.jpg", StoredFilename: id + ".jpg",
		Kind: models.KindImage, MimeType: "image/jpeg", SizeBytes: 100,
		SHA256: sha, UploadedAt: uploadedAt, UploaderName: "Alice",
	})
	if err != nil {
		t.Fatalf("insert media: %v", err)
	}
}

func TestHandleGallery_ReturnsItemsNewestFirst(t *testing.T) {
	h := newTestHarness(t)
	base := time.Now().Add(-time.Hour)
	insertTestMedia(t, h, "id1", "sha1", base)
	insertTestMedia(t, h, "id2", "sha2", base.Add(time.Minute))

	rec := doRequest(h, http.MethodGet, "/api/gallery", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp galleryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Items) != 2 || resp.Items[0].ID != "id2" {
		t.Fatalf("unexpected items: %+v", resp.Items)
	}
}

func TestHandleGallery_InvalidCursor(t *testing.T) {
	h := newTestHarness(t)
	rec := doRequest(h, http.MethodGet, "/api/gallery?cursor=not-valid!!", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandlePreview(t *testing.T) {
	h := newTestHarness(t)
	if err := h.store.InsertMedia(context.Background(), &models.MediaItem{
		ID: "heic-id", OriginalFilename: "IMG_0001.HEIC", StoredFilename: "heic-id.heic",
		Kind: models.KindImage, MimeType: "image/heic", SizeBytes: 100,
		SHA256: "heic-sha", HasPreview: true, UploadedAt: time.Now(), UploaderName: "Alice",
	}); err != nil {
		t.Fatalf("insert media: %v", err)
	}
	if err := os.WriteFile(h.proc.PreviewPath("heic-id"), []byte("preview-bytes"), 0o640); err != nil {
		t.Fatal(err)
	}

	rec := doRequest(h, http.MethodGet, "/api/media/heic-id/preview", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "preview-bytes" {
		t.Fatalf("expected the preview to be served, got %d: %s", rec.Code, rec.Body.String())
	}

	// A JPEG original is displayable as-is, so it never gets a preview file.
	insertTestMedia(t, h, "jpeg-id", "jpeg-sha", time.Now())
	if rec := doRequest(h, http.MethodGet, "/api/media/jpeg-id/preview", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an item without a preview, got %d", rec.Code)
	}
}

func TestGalleryDTOReportsPreviewAvailability(t *testing.T) {
	h := newTestHarness(t)
	if err := h.store.InsertMedia(context.Background(), &models.MediaItem{
		ID: "heic-id", OriginalFilename: "IMG_0001.HEIC", StoredFilename: "heic-id.heic",
		Kind: models.KindImage, MimeType: "image/heic", SizeBytes: 100,
		SHA256: "heic-sha", HasPreview: true, UploadedAt: time.Now(), UploaderName: "Alice",
	}); err != nil {
		t.Fatalf("insert media: %v", err)
	}

	rec := doRequest(h, http.MethodGet, "/api/gallery", nil)
	var resp galleryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Items) != 1 || !resp.Items[0].HasPreview {
		t.Fatalf("expected hasPreview to survive the round trip, got %+v", resp.Items)
	}
}

func TestHandlePublicConfig(t *testing.T) {
	h := newTestHarness(t)
	rec := doRequest(h, http.MethodGet, "/api/config/public", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp publicConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.UploadsEnabled {
		t.Error("expected uploads enabled by default")
	}
	if resp.ApprovalRequired {
		t.Error("expected approval queue disabled by default")
	}
	if resp.MaxUploadBytes != h.cfg.MaxUploadBytes {
		t.Errorf("expected max upload bytes to match config")
	}
	if resp.UploadConcurrency != h.cfg.UploadConcurrencyPerIP {
		t.Errorf("expected upload concurrency to match config")
	}
}

func TestHandlePublicConfig_ReflectsExpiredUploads(t *testing.T) {
	h := newTestHarness(t)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	if err := h.store.SetConfig(context.Background(), "upload_expires_at", past); err != nil {
		t.Fatalf("set config: %v", err)
	}
	rec := doRequest(h, http.MethodGet, "/api/config/public", nil)
	var resp publicConfigResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.UploadsEnabled {
		t.Error("expected uploads disabled once expiry has passed")
	}
}

// The legacy preflight is a shim now: a pre-upgrade tab deletes the guest's
// only copy of the file on a duplicate verdict, so it must answer false to
// everything, including input the old handler would have rejected.
func TestHandleUploadCheck_AlwaysAnswersNotDuplicate(t *testing.T) {
	h := newTestHarness(t)
	duplicateSHA := repeatChar('b', 64)
	insertTestMedia(t, h, "id1", duplicateSHA, time.Now())

	bodies := map[string][]byte{
		"unseen content":   []byte(`{"sha256":"` + repeatChar('a', 64) + `","size":1000,"filename":"a.jpg"}`),
		"known content":    []byte(`{"sha256":"` + duplicateSHA + `","size":1000,"filename":"a.jpg"}`),
		"malformed digest": []byte(`{"sha256":"short","size":100}`),
		"impossible size":  []byte(`{"sha256":"` + repeatChar('c', 64) + `","size":999999999999}`),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			rec := doRequest(h, http.MethodPost, "/api/uploads/check", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			var resp uploadCheckResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Duplicate || resp.MediaID != "" {
				t.Errorf("expected no duplicate verdict, got %+v", resp)
			}
		})
	}
}

func TestHandleMediaNotFound(t *testing.T) {
	h := newTestHarness(t)
	rec := doRequest(h, http.MethodGet, "/api/media/does-not-exist/thumbnail", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestLikeAndUnlike(t *testing.T) {
	h := newTestHarness(t)
	insertTestMedia(t, h, "id1", "shalike", time.Now())

	req := newRequestWithHeader(http.MethodPost, "/api/media/id1/like", nil, deviceIDHeader, "device-1")
	rec := serveRequest(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp likeResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.LikeCount != 1 || !resp.LikedByDevice {
		t.Fatalf("unexpected like response: %+v", resp)
	}

	// Liking again from the same device should stay at 1 (idempotent).
	rec2 := serveRequest(h, newRequestWithHeader(http.MethodPost, "/api/media/id1/like", nil, deviceIDHeader, "device-1"))
	var resp2 likeResponse
	json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if resp2.LikeCount != 1 {
		t.Fatalf("expected like count to stay at 1, got %d", resp2.LikeCount)
	}

	rec3 := serveRequest(h, newRequestWithHeader(http.MethodDelete, "/api/media/id1/like", nil, deviceIDHeader, "device-1"))
	var resp3 likeResponse
	json.Unmarshal(rec3.Body.Bytes(), &resp3)
	if resp3.LikeCount != 0 || resp3.LikedByDevice {
		t.Fatalf("expected unlike to remove like: %+v", resp3)
	}
}

func TestLike_RequiresDeviceID(t *testing.T) {
	h := newTestHarness(t)
	insertTestMedia(t, h, "id1", "shax", time.Now())
	rec := doRequest(h, http.MethodPost, "/api/media/id1/like", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without device id, got %d", rec.Code)
	}
}

func repeatChar(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
