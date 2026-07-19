package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"wedding-gallery/backend/internal/db"
	"wedding-gallery/backend/internal/models"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return New(sqlDB)
}

func sampleMedia(id, sha string, uploadedAt time.Time) *models.MediaItem {
	return &models.MediaItem{
		ID:               id,
		OriginalFilename: "photo.jpg",
		StoredFilename:   id + ".jpg",
		Kind:             models.KindImage,
		MimeType:         "image/jpeg",
		SizeBytes:        1234,
		SHA256:           sha,
		Width:            800,
		Height:           600,
		UploadedAt:       uploadedAt,
		UploaderName:     "Alice",
		UploaderIP:       "127.0.0.1",
	}
}

func TestInsertAndGetMedia(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	m := sampleMedia("id1", "sha1value", time.Now())
	if err := s.InsertMedia(ctx, m); err != nil {
		t.Fatalf("insert media: %v", err)
	}

	got, err := s.GetByID(ctx, "id1", "device-a")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.OriginalFilename != "photo.jpg" || got.SHA256 != "sha1value" {
		t.Errorf("unexpected media: %+v", got)
	}
	if got.LikeCount != 0 || got.LikedByDevice {
		t.Errorf("expected no likes initially")
	}

	bySha, err := s.GetBySHA256(ctx, "sha1value")
	if err != nil {
		t.Fatalf("get by sha256: %v", err)
	}
	if bySha.ID != "id1" {
		t.Errorf("expected id1, got %s", bySha.ID)
	}
}

func TestInsertMedia_DuplicateSHA256Rejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	m1 := sampleMedia("id1", "dupsha", time.Now())
	m2 := sampleMedia("id2", "dupsha", time.Now())
	if err := s.InsertMedia(ctx, m1); err != nil {
		t.Fatalf("insert first: %v", err)
	}
	if err := s.InsertMedia(ctx, m2); err == nil {
		t.Fatal("expected error inserting duplicate sha256")
	}
}

func TestGetBySHA256_NotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.GetBySHA256(ctx, "missing"); err == nil {
		t.Fatal("expected error for missing sha256")
	}
}

func TestListGallery_PaginationAndOrder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		m := sampleMedia(
			fmtID(i),
			fmtID(i)+"-sha",
			base.Add(time.Duration(i)*time.Minute),
		)
		if err := s.InsertMedia(ctx, m); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// Descending (newest first): expect id4, id3, ...
	page1, cursor1, err := s.ListGallery(ctx, ListGalleryParams{
		Sort: models.SortUploaded, Order: models.OrderDesc, Limit: 2, DeviceID: "dev",
	})
	if err != nil {
		t.Fatalf("list gallery page1: %v", err)
	}
	if len(page1) != 2 || page1[0].ID != "id4" || page1[1].ID != "id3" {
		t.Fatalf("unexpected page1: %+v", page1)
	}
	if cursor1 == "" {
		t.Fatal("expected next cursor")
	}

	cur, err := DecodeCursor(cursor1)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	page2, cursor2, err := s.ListGallery(ctx, ListGalleryParams{
		Sort: models.SortUploaded, Order: models.OrderDesc, Limit: 2, DeviceID: "dev", Cursor: cur,
	})
	if err != nil {
		t.Fatalf("list gallery page2: %v", err)
	}
	if len(page2) != 2 || page2[0].ID != "id2" || page2[1].ID != "id1" {
		t.Fatalf("unexpected page2: %+v", page2)
	}
	_ = cursor2
}

func fmtID(i int) string {
	return "id" + string(rune('0'+i))
}

func TestLikes_AddRemoveAndDedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	m := sampleMedia("id1", "sha1", time.Now())
	if err := s.InsertMedia(ctx, m); err != nil {
		t.Fatalf("insert: %v", err)
	}

	added, err := s.AddLike(ctx, "id1", "device-a")
	if err != nil || !added {
		t.Fatalf("expected like added, err=%v added=%v", err, added)
	}
	// Same device likes again: should be a no-op, not duplicated.
	added2, err := s.AddLike(ctx, "id1", "device-a")
	if err != nil {
		t.Fatalf("second add like: %v", err)
	}
	if added2 {
		t.Fatal("expected second like from same device to be ignored")
	}

	added3, err := s.AddLike(ctx, "id1", "device-b")
	if err != nil || !added3 {
		t.Fatalf("expected like from second device to succeed")
	}

	got, err := s.GetByID(ctx, "id1", "device-a")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.LikeCount != 2 {
		t.Errorf("expected 2 likes, got %d", got.LikeCount)
	}
	if !got.LikedByDevice {
		t.Errorf("expected device-a to have liked")
	}

	removed, err := s.RemoveLike(ctx, "id1", "device-a")
	if err != nil || !removed {
		t.Fatalf("expected like removed, err=%v removed=%v", err, removed)
	}
	got2, err := s.GetByID(ctx, "id1", "device-a")
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got2.LikeCount != 1 || got2.LikedByDevice {
		t.Errorf("unexpected like state after removal: %+v", got2)
	}
}

func TestSetStatusBulk_TrashAndRestore(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		if err := s.InsertMedia(ctx, sampleMedia(fmtID(i), fmtID(i)+"-sha", time.Now())); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	changed, err := s.SetStatusBulk(ctx, []string{"id0", "id1"}, models.StatusTrashed)
	if err != nil {
		t.Fatalf("trash: %v", err)
	}
	if len(changed) != 2 {
		t.Fatalf("expected 2 changed, got %d", len(changed))
	}

	// Trashing again should be a no-op (already trashed).
	changedAgain, err := s.SetStatusBulk(ctx, []string{"id0"}, models.StatusTrashed)
	if err != nil {
		t.Fatalf("re-trash: %v", err)
	}
	if len(changedAgain) != 0 {
		t.Errorf("expected no-op for already trashed item")
	}

	active, _, err := s.ListGallery(ctx, ListGalleryParams{Sort: models.SortUploaded, Order: models.OrderDesc, Limit: 10, DeviceID: "d"})
	if err != nil {
		t.Fatalf("list gallery: %v", err)
	}
	if len(active) != 1 || active[0].ID != "id2" {
		t.Fatalf("expected only id2 active, got %+v", active)
	}

	restored, err := s.SetStatusBulk(ctx, []string{"id0"}, models.StatusActive)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("expected 1 restored, got %d", len(restored))
	}

	activeAfter, _, err := s.ListGallery(ctx, ListGalleryParams{Sort: models.SortUploaded, Order: models.OrderDesc, Limit: 10, DeviceID: "d"})
	if err != nil {
		t.Fatalf("list gallery after restore: %v", err)
	}
	if len(activeAfter) != 2 {
		t.Fatalf("expected 2 active after restore, got %d", len(activeAfter))
	}
}

func TestListAdmin_FiltersByStatus(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		if err := s.InsertMedia(ctx, sampleMedia(fmtID(i), fmtID(i)+"-sha", time.Now())); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if _, err := s.SetStatusBulk(ctx, []string{"id1"}, models.StatusTrashed); err != nil {
		t.Fatalf("trash: %v", err)
	}

	all, _, err := s.ListAdmin(ctx, AdminListParams{Limit: 10})
	if err != nil {
		t.Fatalf("list admin all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 total, got %d", len(all))
	}

	trashed, _, err := s.ListAdmin(ctx, AdminListParams{Status: models.StatusTrashed, Limit: 10})
	if err != nil {
		t.Fatalf("list admin trashed: %v", err)
	}
	if len(trashed) != 1 || trashed[0].ID != "id1" {
		t.Fatalf("expected only id1 trashed, got %+v", trashed)
	}
}

func TestAuditLog_RecordAndList(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.RecordAudit(ctx, models.ActionUpload, "Alice", "id1", "photo.jpg", ""); err != nil {
		t.Fatalf("record audit: %v", err)
	}
	if err := s.RecordAudit(ctx, models.ActionLogin, "admin", "", "", ""); err != nil {
		t.Fatalf("record audit: %v", err)
	}

	entries, _, err := s.ListAudit(ctx, ListAuditParams{Limit: 10})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	// Newest first.
	if entries[0].Action != models.ActionLogin {
		t.Errorf("expected most recent entry first, got %+v", entries[0])
	}
}

func TestSessions_CreateGetDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sess, err := s.CreateSession(ctx, time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if sess.ID == "" || sess.CSRFToken == "" {
		t.Fatal("expected non-empty session id and csrf token")
	}

	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.CSRFToken != sess.CSRFToken {
		t.Errorf("csrf token mismatch")
	}

	if err := s.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := s.GetSession(ctx, sess.ID); err != ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound after delete, got %v", err)
	}
}

func TestSessions_ExpiredNotReturned(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sess, err := s.CreateSession(ctx, -time.Minute) // already expired
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := s.GetSession(ctx, sess.ID); err != ErrSessionNotFound {
		t.Errorf("expected ErrSessionNotFound for expired session, got %v", err)
	}
}

func TestConfig_GetSetDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, ok, err := s.GetConfig(ctx, "missing"); err != nil || ok {
		t.Fatalf("expected missing key to be absent, ok=%v err=%v", ok, err)
	}
	if err := s.SetConfig(ctx, ConfigKeyUploadExpiresAt, "2030-01-01T00:00:00Z"); err != nil {
		t.Fatalf("set config: %v", err)
	}
	v, ok, err := s.GetConfig(ctx, ConfigKeyUploadExpiresAt)
	if err != nil || !ok || v != "2030-01-01T00:00:00Z" {
		t.Fatalf("unexpected config value: v=%s ok=%v err=%v", v, ok, err)
	}
	if err := s.SetConfig(ctx, ConfigKeyUploadExpiresAt, "2031-01-01T00:00:00Z"); err != nil {
		t.Fatalf("update config: %v", err)
	}
	v2, _, _ := s.GetConfig(ctx, ConfigKeyUploadExpiresAt)
	if v2 != "2031-01-01T00:00:00Z" {
		t.Fatalf("expected updated value, got %s", v2)
	}
	if err := s.DeleteConfig(ctx, ConfigKeyUploadExpiresAt); err != nil {
		t.Fatalf("delete config: %v", err)
	}
	if _, ok, _ := s.GetConfig(ctx, ConfigKeyUploadExpiresAt); ok {
		t.Fatalf("expected key removed")
	}
}
