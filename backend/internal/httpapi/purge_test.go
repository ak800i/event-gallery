package httpapi

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"event-gallery/backend/internal/models"
)

func insertPurgeTestMedia(t *testing.T, h *testHarness, id string) {
	t.Helper()
	item := &models.MediaItem{
		ID: id, OriginalFilename: id + ".jpg", StoredFilename: id + ".jpg",
		Kind: models.KindImage, MimeType: "image/jpeg", SizeBytes: 8,
		SHA256: id + "-sha", HasThumbnail: true, UploadedAt: time.Now(),
	}
	if err := h.store.InsertMedia(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.proc.OriginalPath(item.StoredFilename), []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.proc.ThumbnailPath(item.ID), []byte("thumbnail"), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestAdminBulkPurgePermanentlyDeletesTrashedMedia(t *testing.T) {
	h := newTestHarness(t)
	insertPurgeTestMedia(t, h, "purge-me")
	if _, err := h.store.SetStatusBulk(t.Context(), []string{"purge-me"}, models.StatusTrashed); err != nil {
		t.Fatal(err)
	}
	sess, csrfCookie, token := adminLogin(t, h, h.cfg.AdminPassword)
	req := authedRequest(h, http.MethodPost, "/api/admin/media/bulk-purge", []byte(`{"ids":["purge-me"]}`), sess, csrfCookie, token)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("purge: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := h.store.GetByID(t.Context(), "purge-me", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("row still exists: %v", err)
	}
	if _, err := os.Stat(h.proc.OriginalPath("purge-me.jpg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("original still exists")
	}
	if _, err := os.Stat(h.proc.ThumbnailPath("purge-me")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("thumbnail still exists")
	}
}

func TestExpiredTrashIsPurgedAutomatically(t *testing.T) {
	h := newTestHarness(t)
	insertPurgeTestMedia(t, h, "expired-trash")
	if _, err := h.store.SetStatusBulk(t.Context(), []string{"expired-trash"}, models.StatusTrashed); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().Exec(`UPDATE media_items SET deleted_at = ? WHERE id = ?`, time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339Nano), "expired-trash"); err != nil {
		t.Fatal(err)
	}
	h.cfg.TrashRetention = 24 * time.Hour
	h.server.purgeExpiredTrash(t.Context())
	if _, err := h.store.GetByID(t.Context(), "expired-trash", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired row still exists: %v", err)
	}
	if _, err := os.Stat(h.proc.OriginalPath("expired-trash.jpg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired original still exists")
	}
}

func TestAdminBulkPurgeRejectsActiveMedia(t *testing.T) {
	h := newTestHarness(t)
	insertPurgeTestMedia(t, h, "keep-me")
	sess, csrfCookie, token := adminLogin(t, h, h.cfg.AdminPassword)
	req := authedRequest(h, http.MethodPost, "/api/admin/media/bulk-purge", []byte(`{"ids":["keep-me"]}`), sess, csrfCookie, token)
	rec := serveRequest(h, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if _, err := os.Stat(h.proc.OriginalPath("keep-me.jpg")); err != nil {
		t.Fatalf("active original changed: %v", err)
	}
}
