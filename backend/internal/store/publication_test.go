package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"event-gallery/backend/internal/models"
)

func publishable(id, sha string) *models.MediaItem {
	return sampleMedia(id, sha, time.Now())
}

// Publication is the moment a source stops being the only copy, so it must be
// one transaction: the row and the job's transition commit together or not at
// all.
func TestPublishMediaCommitsRowAndTransitionTogether(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	job := claimInto(t, s, "u1", "m1", JobProcessing)

	id, duplicate, err := s.PublishMedia(ctx, "u1", job.LeaseToken, publishable("m1", "sha-1"), NowMicros())
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if id != "m1" || duplicate {
		t.Errorf("publish = (%q, %v), want (m1, false)", id, duplicate)
	}

	got, _ := s.GetUploadJob(ctx, "u1")
	if got.Status != JobCleanup {
		t.Errorf("status = %q, want cleanup", got.Status)
	}
	if got.ResultMediaID != "m1" {
		t.Errorf("result_media_id = %q, want m1", got.ResultMediaID)
	}
	if got.LeaseToken != "" {
		t.Error("publication ends the processing lease")
	}
	if _, err := s.GetByID(ctx, "m1", ""); err != nil {
		t.Errorf("media row must be committed: %v", err)
	}
}

// The lease is the only proof of ownership. A worker whose lease expired must
// not be able to publish on top of its successor's work.
func TestPublishMediaRefusesAStaleLease(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	claimInto(t, s, "u1", "m1", JobProcessing)

	_, _, err := s.PublishMedia(ctx, "u1", "stale-token", publishable("m1", "sha-1"), NowMicros())
	if !errors.Is(err, ErrNotClaimed) {
		t.Fatalf("stale publish = %v, want ErrNotClaimed", err)
	}
	if _, err := s.GetByID(ctx, "m1", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Error("a refused publication must leave no media row")
	}
	got, _ := s.GetUploadJob(ctx, "u1")
	if got.Status != JobProcessing {
		t.Errorf("status = %q, want processing untouched", got.Status)
	}
}

// Whoever won the unique hash constraint owns the content. The loser must be
// told which id survived so it can clean up its own copy instead of the one
// that is published.
func TestPublishMediaResolvesToTheExistingOwnerOfTheContent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.InsertMedia(ctx, publishable("first", "same-sha")); err != nil {
		t.Fatalf("seed first: %v", err)
	}
	job := claimInto(t, s, "u2", "second", JobProcessing)

	id, duplicate, err := s.PublishMedia(ctx, "u2", job.LeaseToken, publishable("second", "same-sha"), NowMicros())
	if err != nil {
		t.Fatalf("publish duplicate: %v", err)
	}
	if id != "first" || !duplicate {
		t.Errorf("publish = (%q, %v), want (first, true)", id, duplicate)
	}
	if _, err := s.GetByID(ctx, "second", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Error("a duplicate must not create a second row for the same content")
	}
	got, _ := s.GetUploadJob(ctx, "u2")
	if got.ResultMediaID != "first" {
		t.Errorf("result_media_id = %q, want the surviving id", got.ResultMediaID)
	}
}

func TestArtifactIdentityAndPreparedRequireTheLease(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	job := claimInto(t, s, "u1", "m1", JobProcessing)

	if err := s.RecordArtifactIdentity(ctx, "u1", "stale", "m1.jpg", "image/jpeg", "sha-1"); !errors.Is(err, ErrNotClaimed) {
		t.Errorf("stale identity write = %v, want ErrNotClaimed", err)
	}
	if err := s.RecordPrepared(ctx, "u1", "stale"); !errors.Is(err, ErrNotClaimed) {
		t.Errorf("stale prepared write = %v, want ErrNotClaimed", err)
	}

	if err := s.RecordArtifactIdentity(ctx, "u1", job.LeaseToken, "m1.jpg", "image/jpeg", "sha-1"); err != nil {
		t.Fatalf("identity: %v", err)
	}
	mid, _ := s.GetUploadJob(ctx, "u1")
	if mid.PreparedAt != nil {
		t.Error("prepared_at means a verified original exists, so recording the identity must not set it")
	}
	if err := s.RecordPrepared(ctx, "u1", job.LeaseToken); err != nil {
		t.Fatalf("prepared: %v", err)
	}

	got, _ := s.GetUploadJob(ctx, "u1")
	if got.StoredFilename != "m1.jpg" || got.MimeType != "image/jpeg" || got.AuthoritativeSHA256 != "sha-1" {
		t.Errorf("identity not persisted: %+v", got)
	}
	if got.PreparedAt == nil {
		t.Error("prepared_at must be set once the original is verified")
	}
}

// 'unobservable' is deliberately distinct from a cancellation: it deletes
// nothing, so it stays reversible if the paths were merely hidden.
func TestMarkUnobservableOnlyClosesAnUploadThatNeverCompleted(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")
	seedUploading(t, s, "u2", "m2")
	if err := s.PromoteToPending(ctx, "u2", NowMicros()); err != nil {
		t.Fatalf("promote: %v", err)
	}

	if err := s.MarkUnobservable(ctx, "u2", NowMicros()); !errors.Is(err, ErrNotClaimed) {
		t.Errorf("completed upload = %v, want ErrNotClaimed: its bytes were observed", err)
	}
	if err := s.MarkUnobservable(ctx, "u1", NowMicros()); err != nil {
		t.Fatalf("mark: %v", err)
	}
	got, _ := s.GetUploadJob(ctx, "u1")
	if got.Status != JobDiscarded || got.TerminalReason != "unobservable" {
		t.Errorf("job = %q/%q, want discarded/unobservable", got.Status, got.TerminalReason)
	}
	if got.TerminalAt == nil {
		t.Error("terminal_at must be stamped so retention can expire the row")
	}
}

func TestReopenTerminalAndDeleteOnlyTouchFinishedJobs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")

	if err := s.ReopenTerminal(ctx, "u1", JobCleanup, NowMicros()); !errors.Is(err, ErrNotClaimed) {
		t.Errorf("reopen of a live job = %v, want ErrNotClaimed", err)
	}
	if err := s.DeleteUploadJob(ctx, "u1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if live, _ := s.GetUploadJob(ctx, "u1"); live == nil {
		t.Fatal("a live job must not be deleted")
	}

	if err := s.MarkUnobservable(ctx, "u1", NowMicros()); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := s.ReopenTerminal(ctx, "u1", JobCleanup, NowMicros()); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	reopened, _ := s.GetUploadJob(ctx, "u1")
	if reopened.Status != JobCleanup || reopened.TerminalAt != nil {
		t.Errorf("job = %q terminal_at %v, want cleanup and no terminal stamp", reopened.Status, reopened.TerminalAt)
	}

	claimed, err := s.ClaimNextJob(ctx, JobCleanup, JobCleanup, NowMicros(), time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("a reopened job must be claimable: %+v %v", claimed, err)
	}

	if err := s.FinishJob(ctx, "u1", claimed.LeaseToken, JobComplete, "", NowMicros()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := s.DeleteUploadJob(ctx, "u1"); err != nil {
		t.Fatalf("delete terminal: %v", err)
	}
	if gone, _ := s.GetUploadJob(ctx, "u1"); gone != nil {
		t.Error("a terminal job must be deletable so its id can be adopted again")
	}
}
