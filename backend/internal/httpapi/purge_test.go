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

// A duplicate upload validates the authoritative original, records it as its
// result, and only then deletes its own source and copy. Purging that row in
// between would leave both copies gone, so the delete predicate defers to any
// job still in flight against it.
func TestPurgeDefersToAnInFlightUploadJob(t *testing.T) {
	h := newTestHarness(t)
	insertPurgeTestMedia(t, h, "referenced")
	if _, err := h.store.SetStatusBulk(t.Context(), []string{"referenced"}, models.StatusTrashed); err != nil {
		t.Fatal(err)
	}
	seedUploadingJob(t, h, "dup")
	if _, err := h.store.DB().Exec(
		`UPDATE upload_jobs SET status = 'cleanup', result_media_id = ? WHERE upload_id = ?`,
		"referenced", "dup"); err != nil {
		t.Fatal(err)
	}

	changed, err := h.server.purgeMedia(t.Context(), []string{"referenced"}, "admin")
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("purged %v while a job still depends on it", changed)
	}
	if _, err := h.store.GetByID(t.Context(), "referenced", ""); err != nil {
		t.Errorf("the row must survive: %v", err)
	}
	// The staged files must be put back, not left in the purge staging area,
	// or the deferral would still have destroyed the only copy.
	if _, err := os.Stat(h.proc.OriginalPath("referenced.jpg")); err != nil {
		t.Errorf("original must be restored after a deferred purge: %v", err)
	}
	if _, err := os.Stat(h.proc.ThumbnailPath("referenced")); err != nil {
		t.Errorf("thumbnail must be restored after a deferred purge: %v", err)
	}

	// Once the job is terminal nothing depends on the row any more.
	if _, err := h.store.DB().Exec(`UPDATE upload_jobs SET status = 'complete' WHERE upload_id = ?`, "dup"); err != nil {
		t.Fatal(err)
	}
	changed, err = h.server.purgeMedia(t.Context(), []string{"referenced"}, "admin")
	if err != nil {
		t.Fatalf("second purge: %v", err)
	}
	if len(changed) != 1 {
		t.Fatalf("purged %v, want the row once its job finished", changed)
	}
	if _, err := h.store.GetByID(t.Context(), "referenced", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("row still exists: %v", err)
	}
	if _, err := os.Stat(h.proc.OriginalPath("referenced.jpg")); !errors.Is(err, os.ErrNotExist) {
		t.Error("original still exists")
	}
}

// A purge commits a row deletion, and StageForPurge treats a missing original
// as success. Doing that while the media volume is unproven would orphan the
// real file the moment the mount came back.
func TestPurgeRefusesWhileStorageIsUnproven(t *testing.T) {
	h := newTestHarness(t)
	insertPurgeTestMedia(t, h, "trashed-item")
	if _, err := h.store.SetStatusBulk(t.Context(), []string{"trashed-item"}, models.StatusTrashed); err != nil {
		t.Fatal(err)
	}
	// What an unmounted media volume looks like: the rows are there and the
	// files they name are not.
	if err := os.Remove(h.proc.OriginalPath("trashed-item.jpg")); err != nil {
		t.Fatal(err)
	}

	if _, err := h.server.purgeMedia(t.Context(), []string{"trashed-item"}, "admin"); err == nil {
		t.Fatal("purge must refuse while the media volume is unproven")
	}
	if _, err := h.store.GetByID(t.Context(), "trashed-item", ""); err != nil {
		t.Errorf("the row must survive a refused purge: %v", err)
	}
}
