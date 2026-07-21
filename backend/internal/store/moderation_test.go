package store

import (
	"context"
	"testing"
	"time"

	"event-gallery/backend/internal/models"
)

func boolPtr(value bool) *bool { return &value }

func TestApprovalOffByDefaultAndToggleLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	first := sampleMedia("approved", "sha-approved", time.Now().Add(-time.Minute))
	if err := st.InsertMedia(ctx, first); err != nil {
		t.Fatalf("insert with approval off: %v", err)
	}
	visible, err := st.GetVisibleByID(ctx, first.ID, "")
	if err != nil || visible.ApprovedAt == nil {
		t.Fatalf("default-off upload should be visible and approved: %+v, %v", visible, err)
	}

	if _, err := st.SetApprovalRequired(ctx, true); err != nil {
		t.Fatalf("enable approval: %v", err)
	}
	pending := sampleMedia("pending", "sha-pending", time.Now())
	if err := st.InsertMedia(ctx, pending); err != nil {
		t.Fatalf("insert pending: %v", err)
	}
	if _, err := st.GetVisibleByID(ctx, pending.ID, ""); err == nil {
		t.Fatal("pending media must not be publicly visible")
	}
	pendingItems, _, err := st.ListAdmin(ctx, AdminListParams{Status: models.StatusActive, Approved: boolPtr(false), Limit: 10})
	if err != nil || len(pendingItems) != 1 || pendingItems[0].ID != pending.ID {
		t.Fatalf("unexpected pending queue: %+v, %v", pendingItems, err)
	}

	autoApproved, err := st.SetApprovalRequired(ctx, false)
	if err != nil {
		t.Fatalf("disable approval: %v", err)
	}
	if autoApproved != 1 {
		t.Fatalf("expected one auto-approved row, got %d", autoApproved)
	}
	visible, err = st.GetVisibleByID(ctx, pending.ID, "")
	if err != nil || visible.ApprovedAt == nil {
		t.Fatalf("pending row not visible after disabling: %+v, %v", visible, err)
	}
}

func TestApproveBulkDoesNotRestoreTrash(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if _, err := st.SetApprovalRequired(ctx, true); err != nil {
		t.Fatal(err)
	}
	pending := sampleMedia("pending-trash", "sha-pending-trash", time.Now())
	if err := st.InsertMedia(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetStatusBulk(ctx, []string{pending.ID}, models.StatusTrashed); err != nil {
		t.Fatal(err)
	}
	changed, err := st.ApproveBulk(ctx, []string{pending.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("trashed pending row must not be approved: %v", changed)
	}
}
