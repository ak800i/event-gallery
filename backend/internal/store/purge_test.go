package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"event-gallery/backend/internal/models"
)

func TestPurgeTrashedDeletesRowLikesAndAuditsAtomically(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	item := sampleMedia("purge-id", "purge-sha", time.Now())
	if err := st.InsertMedia(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddLike(ctx, item.ID, "device"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetStatusBulk(ctx, []string{item.ID}, models.StatusTrashed); err != nil {
		t.Fatal(err)
	}
	trashed, err := st.GetByID(ctx, item.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	due, err := st.ListTrashedBefore(ctx, time.Now().Add(time.Hour), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("expected due trash: %+v %v", due, err)
	}
	changed, err := st.PurgeTrashed(ctx, []models.MediaItem{*trashed}, "admin")
	if err != nil || len(changed) != 1 {
		t.Fatalf("purge: %v %v", changed, err)
	}
	if _, err := st.GetByID(ctx, item.ID, ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("media row still exists: %v", err)
	}
	var likes int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM likes WHERE media_id = ?`, item.ID).Scan(&likes); err != nil || likes != 0 {
		t.Fatalf("likes not cascaded: %d %v", likes, err)
	}
	var audit int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'purge' AND media_id = ?`, item.ID).Scan(&audit); err != nil || audit != 1 {
		t.Fatalf("purge audit missing: %d %v", audit, err)
	}
}

func TestPurgeTrashedRefusesActiveOrChangedFilename(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	item := sampleMedia("keep-id", "keep-sha", time.Now())
	if err := st.InsertMedia(ctx, item); err != nil {
		t.Fatal(err)
	}
	changed, err := st.PurgeTrashed(ctx, []models.MediaItem{*item}, "admin")
	if err != nil || len(changed) != 0 {
		t.Fatalf("active row must remain: %v %v", changed, err)
	}
}
