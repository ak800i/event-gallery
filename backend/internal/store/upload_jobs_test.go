package store

import (
	"context"
	"testing"
	"time"
)

func seedUploading(t *testing.T, s *Store, uploadID, mediaID string) *UploadJob {
	t.Helper()
	job := &UploadJob{
		UploadID:         uploadID,
		MediaID:          mediaID,
		OriginalFilename: "photo.jpg",
		ExpectedSize:     1024,
		DeclaredSHA256:   "abc",
		GuestName:        "Ana",
		UploaderIP:       "10.0.0.1",
	}
	if err := s.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	return job
}

func TestCreateAndGetUploadJob(t *testing.T) {
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")

	got, err := s.GetUploadJob(context.Background(), "u1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != JobUploading {
		t.Errorf("status = %q, want uploading", got.Status)
	}
	if got.ExpectedSize != 1024 || got.MediaID != "m1" {
		t.Errorf("unexpected job: %+v", got)
	}

	missing, err := s.GetUploadJob(context.Background(), "nope")
	if err != nil || missing != nil {
		t.Errorf("missing job: got %+v err %v, want nil nil", missing, err)
	}
}

func TestPromoteToPendingIsBlockedByCancellation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")

	if err := s.RequestCancellation(ctx, "u1", NowMicros()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := s.PromoteToPending(ctx, "u1", NowMicros()); err != ErrNotClaimed {
		t.Fatalf("promote after cancel = %v, want ErrNotClaimed", err)
	}

	got, _ := s.GetUploadJob(ctx, "u1")
	if got.Status != JobUploading {
		t.Errorf("status = %q, want uploading (promotion must fail closed)", got.Status)
	}
}

func TestClaimIsExclusiveAndFenced(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")
	if err := s.PromoteToPending(ctx, "u1", NowMicros()); err != nil {
		t.Fatalf("promote: %v", err)
	}

	now := NowMicros()
	first, err := s.ClaimNextJob(ctx, JobPending, JobProcessing, now, time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first claim: %+v %v", first, err)
	}
	if first.LeaseToken == "" {
		t.Fatal("claim must issue a lease token")
	}

	second, err := s.ClaimNextJob(ctx, JobPending, JobProcessing, now, time.Minute)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second != nil {
		t.Fatal("a job must be claimable by exactly one worker")
	}

	// A stale token must not be able to publish, clean, or discard.
	if err := s.FinishJob(ctx, "u1", "stale-token", JobComplete, "", NowMicros()); err != ErrNotClaimed {
		t.Fatalf("stale finish = %v, want ErrNotClaimed", err)
	}
	if err := s.FinishJob(ctx, "u1", first.LeaseToken, JobComplete, "", NowMicros()); err != nil {
		t.Fatalf("owner finish: %v", err)
	}
}

func TestExpiredLeaseBecomesClaimableAgain(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")
	_ = s.PromoteToPending(ctx, "u1", NowMicros())

	now := NowMicros()
	if _, err := s.ClaimNextJob(ctx, JobPending, JobProcessing, now, time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}

	later := now + int64(2*time.Minute/time.Microsecond)
	reclaimed, err := s.ClaimNextJob(ctx, JobProcessing, JobProcessing, later, time.Minute)
	if err != nil || reclaimed == nil {
		t.Fatalf("expired lease must be reclaimable: %+v %v", reclaimed, err)
	}
}

func TestReleaseForRetryIncrementsNamedCounter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")
	_ = s.PromoteToPending(ctx, "u1", NowMicros())
	claimed, _ := s.ClaimNextJob(ctx, JobPending, JobProcessing, NowMicros(), time.Minute)

	next := NowMicros() + 1_000_000
	if err := s.ReleaseForRetry(ctx, "u1", claimed.LeaseToken, JobPending, next, "processing_failures", "disk hiccup"); err != nil {
		t.Fatalf("release: %v", err)
	}

	got, _ := s.GetUploadJob(ctx, "u1")
	if got.Status != JobPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.ProcessingFailures != 1 || got.ConversionFailures != 0 {
		t.Errorf("counters = proc %d conv %d, want 1 and 0", got.ProcessingFailures, got.ConversionFailures)
	}
	if got.LeaseToken != "" {
		t.Error("retry must clear the lease")
	}
}

func TestReleaseForRetryCanKeepTheCurrentStage(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")
	_ = s.PromoteToPending(ctx, "u1", NowMicros())
	claimed, _ := s.ClaimNextJob(ctx, JobPending, JobCleanup, NowMicros(), time.Minute)

	// A cleanup failure must not demote an already-published job back to
	// pending, which would re-run processing against a deleted source.
	if err := s.ReleaseForRetry(ctx, "u1", claimed.LeaseToken, JobCleanup, NowMicros(), "cleanup_failures", "fsync failed"); err != nil {
		t.Fatalf("release: %v", err)
	}
	got, _ := s.GetUploadJob(ctx, "u1")
	if got.Status != JobCleanup {
		t.Errorf("status = %q, want cleanup", got.Status)
	}
	if got.CleanupFailures != 1 {
		t.Errorf("cleanup_failures = %d, want 1", got.CleanupFailures)
	}
}

func TestRequeueStartupResetsProcessing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedUploading(t, s, "u1", "m1")
	_ = s.PromoteToPending(ctx, "u1", NowMicros())
	_, _ = s.ClaimNextJob(ctx, JobPending, JobProcessing, NowMicros(), time.Hour)

	if _, err := s.RequeueStartup(ctx, NowMicros()); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	got, _ := s.GetUploadJob(ctx, "u1")
	if got.Status != JobPending {
		t.Errorf("status = %q, want pending after restart", got.Status)
	}
	// A redeploy must not wait out a wall-clock lease.
	if got.LeaseToken != "" {
		t.Error("startup must clear stale leases")
	}
}
