package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"wedding-gallery/backend/internal/models"
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

func TestHandleUploadCheck_NotDuplicate(t *testing.T) {
	h := newTestHarness(t)
	body := []byte(`{"sha256":"` + repeatChar('a', 64) + `","size":1000,"filename":"a.jpg"}`)
	rec := doRequest(h, http.MethodPost, "/api/uploads/check", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp uploadCheckResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Duplicate {
		t.Error("expected not duplicate")
	}
}

func TestHandleUploadCheck_Duplicate(t *testing.T) {
	h := newTestHarness(t)
	sha := repeatChar('b', 64)
	insertTestMedia(t, h, "id1", sha, time.Now())

	body := []byte(`{"sha256":"` + sha + `","size":1000,"filename":"a.jpg"}`)
	rec := doRequest(h, http.MethodPost, "/api/uploads/check", body)
	var resp uploadCheckResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Duplicate || resp.MediaID != "id1" {
		t.Errorf("expected duplicate of id1, got %+v", resp)
	}
}

func TestHandleUploadCheck_InvalidSHA(t *testing.T) {
	h := newTestHarness(t)
	rec := doRequest(h, http.MethodPost, "/api/uploads/check", []byte(`{"sha256":"short","size":100}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleUploadCheck_SizeTooLarge(t *testing.T) {
	h := newTestHarness(t)
	body := []byte(`{"sha256":"` + repeatChar('c', 64) + `","size":999999999999}`)
	rec := doRequest(h, http.MethodPost, "/api/uploads/check", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
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
