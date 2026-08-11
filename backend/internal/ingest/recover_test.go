package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"event-gallery/backend/internal/store"
)

func TestAdoptsCompletedUploadWithNoRow(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	// An upload that completed under the old code, or whose hook was lost.
	payload := jpegFixture(t)
	if err := os.WriteFile(m.DataPath("legacy1"), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	writeSidecar(t, m, "legacy1", int64(len(payload)))

	m.reconcileOnce()

	job, err := st.GetUploadJob(context.Background(), "legacy1")
	if err != nil || job == nil {
		t.Fatalf("completed upload must be adopted: %+v %v", job, err)
	}
	if job.Status != store.JobPending {
		t.Errorf("status = %q, want pending", job.Status)
	}
	if job.OriginalFilename != "a.jpg" {
		t.Errorf("original filename = %q, want the name the sidecar carried", job.OriginalFilename)
	}
}

func TestPartialUploadIsLeftAlone(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	if err := os.WriteFile(m.DataPath("partial"), []byte("half"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeSidecar(t, m, "partial", 999)

	m.reconcileOnce()

	if job, _ := st.GetUploadJob(context.Background(), "partial"); job != nil {
		t.Errorf("a partial upload must not be adopted as complete: %+v", job)
	}
	if _, err := os.Stat(m.DataPath("partial")); err != nil {
		t.Error("a partial upload must not be deleted by reconciliation")
	}
}

func TestPromotesCompleteUploadStuckInUploading(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	// A crash between pre-create and the durability commit, or a client that
	// gave up after a 503. The bytes are complete and the row exists, so
	// nothing else in the system would ever look at it again.
	payload := jpegFixture(t)
	job := &store.UploadJob{
		UploadID:         "stuck",
		MediaID:          "media-stuck",
		OriginalFilename: "a.jpg",
		ExpectedSize:     int64(len(payload)),
	}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.WriteFile(m.DataPath("stuck"), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	writeSidecar(t, m, "stuck", int64(len(payload)))

	m.reconcileOnce()

	got, _ := st.GetUploadJob(context.Background(), "stuck")
	if got.Status != store.JobPending {
		t.Errorf("status = %q, want pending: a complete source must never be stranded", got.Status)
	}
}

// An upload still being transferred has exactly the shape of the stuck one
// above apart from its size, and reconciliation must not start a durability
// operation for it. Such an operation is doomed — it fails its own size check —
// and it takes one of the few durability slots each tick, so a live client's
// pre-finish call is either refused as busy or joins the doomed operation and
// is told its completed upload can never be accepted.
func TestInFlightUploadIsNotPushedThroughTheBarrier(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	job := &store.UploadJob{
		UploadID:         "live",
		MediaID:          "media-live",
		OriginalFilename: "a.jpg",
		ExpectedSize:     int64(len(payload)) + 10,
	}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.WriteFile(m.DataPath("live"), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	writeSidecar(t, m, "live", int64(len(payload))+10)

	// Nothing was attempted, so there is nothing to report: any durability
	// attempt against a partial source comes back as a final refusal, which is
	// the only trace such an attempt leaves behind.
	if err := m.reconcileOne("live"); err != nil {
		t.Errorf("reconciling a transfer in progress must attempt nothing, got %v", err)
	}
	if got := jobStatus(t, st, "live"); got != store.JobUploading {
		t.Errorf("status = %q, want uploading: an unfinished transfer must be left to its client", got)
	}
}

func TestCancelledUploadIsSweptIntoDiscard(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("partial"))
	if err := st.RequestCancellation(context.Background(), "u1", store.NowMicros()); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Sweep with a zero idle window; the production reconciler waits one
	// interval so it cannot race an upload that is still being written.
	m.sweepCancelled(0)

	got, _ := st.GetUploadJob(context.Background(), "u1")
	if got.Status != store.JobDiscarding {
		t.Errorf("status = %q, want discarding", got.Status)
	}
}

// The sweep is the only reconciler path that can send a row to a stage which
// deletes its source, so it must refuse anything that already crossed the
// durability barrier. A cancellation flag that outlived a promotion is
// exactly the shape the incident took: bytes the app had promised were safe,
// deleted because something else believed the upload was abandoned.
func TestSweepNeverReversesADurableCompletion(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "durable", []byte("partial"))
	if err := m.EnsureDurable(context.Background(), "durable"); err != nil {
		t.Fatalf("ensure durable: %v", err)
	}
	if _, err := st.DB().Exec(
		`UPDATE upload_jobs SET cancellation_requested_at = ? WHERE upload_id = ?`,
		store.NowMicros(), "durable"); err != nil {
		t.Fatalf("flag cancellation: %v", err)
	}

	m.sweepCancelled(0)

	if got := jobStatus(t, st, "durable"); got != store.JobPending {
		t.Errorf("status = %q, want pending: a committed upload must never be swept into a deleting stage", got)
	}
}

// ageFile backdates a file's mtime so the reconciler's quiet period applies to
// it without the test waiting out wall-clock time.
func ageFile(t *testing.T, path string, age time.Duration) {
	t.Helper()
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("backdate %s: %v", path, err)
	}
}

// This is the residue the old ingest path left behind: it deleted the sidecar
// before its failure branch returned, so a complete data file survived alone.
// Rescuing it is the whole reason the sidecar-less branch exists, and the
// witness that authorises it must not shut that rescue down.
func TestAdoptsCompletedUploadWithNoSidecarOnceItIsQuiescent(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	if err := os.WriteFile(m.DataPath("orphan"), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	ageFile(t, m.DataPath("orphan"), 72*time.Hour)

	m.reconcileOnce()
	if job, _ := st.GetUploadJob(context.Background(), "orphan"); job != nil {
		t.Fatalf("a single observation is not a witness that the file is finished: %+v", job)
	}

	m.reconcileOnce()

	job, err := st.GetUploadJob(context.Background(), "orphan")
	if err != nil || job == nil {
		t.Fatalf("residue older than the retention policy and unchanged across passes must be adopted: %+v %v", job, err)
	}
	if job.Status != store.JobPending {
		t.Errorf("status = %q, want pending", job.Status)
	}
}

// THE PROPERTY. With no row and no sidecar the observed size is the only
// number in play, so adopting at it makes every later guard circular: the
// durability barrier compares the size against itself, a nil metadata map
// leaves no declared checksum to mismatch, a truncated JPEG still sniffs as
// image/jpeg, and a thumbnail that fails to decode is not fatal. The job
// publishes, reaches cleanup, and the tus DELETE destroys the guest's only
// copy of a partial upload that was presented to them as a success.
func TestFreshRowlessPartialIsNeitherPublishedNorDeleted(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	truncated := payload[:len(payload)/2]
	if err := os.WriteFile(m.DataPath("partial1"), truncated, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}

	// Two passes and then a full drain: whatever reconciliation admitted, the
	// queue is given every chance to carry it all the way to cleanup.
	m.reconcileOnce()
	m.reconcileOnce()
	drainQueue(t, m)

	if job, _ := st.GetUploadJob(context.Background(), "partial1"); job != nil {
		t.Errorf("a partial adopted on its own observed size: %+v", job)
	}
	requireSourceIntact(t, m, "partial1", truncated)
	if n := countMediaRows(t, st); n != 0 {
		t.Errorf("%d truncated uploads were published into the gallery", n)
	}
}

// countMediaRows is the publication side of the property: a source may only be
// deleted after a media row exists, so proving no row exists proves no
// deletion was ever authorised.
func countMediaRows(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM media_items`).Scan(&n); err != nil {
		t.Fatalf("count media rows: %v", err)
	}
	return n
}

// The same file, aged past the retention policy but still growing between
// passes. Age alone is not the witness: a resumed transfer whose sidecar was
// lost would otherwise be adopted at whatever length one pass happened to see.
func TestSidecarLessSourceThatChangedBetweenPassesIsNotAdopted(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	if err := os.WriteFile(m.DataPath("growing"), payload[:10], 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	ageFile(t, m.DataPath("growing"), 72*time.Hour)

	m.reconcileOnce()

	if err := os.WriteFile(m.DataPath("growing"), payload[:20], 0o600); err != nil {
		t.Fatalf("extend data: %v", err)
	}
	ageFile(t, m.DataPath("growing"), 72*time.Hour)

	m.reconcileOnce()

	if job, _ := st.GetUploadJob(context.Background(), "growing"); job != nil {
		t.Errorf("a file that changed between passes must not be adopted: %+v", job)
	}
}

// tussidecar's own contract: a sidecar that cannot be parsed is "unknown",
// never "safe to delete". An absent sidecar is the old-path residue shape; one
// that is present but unreadable is a fault, and reading a fault as an absence
// is what turns a partial into a published-then-deleted upload.
func TestUnparseableSidecarIsNotTreatedAsAnAbsentOne(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	truncated := payload[:len(payload)/2]
	if err := os.WriteFile(m.DataPath("garbled"), truncated, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	if err := os.WriteFile(m.InfoPath("garbled"), []byte(`{"ID":"garb`), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	ageFile(t, m.DataPath("garbled"), 72*time.Hour)
	ageFile(t, m.InfoPath("garbled"), 72*time.Hour)

	m.reconcileOnce()
	m.reconcileOnce()
	m.reconcileOnce()
	drainQueue(t, m)

	if job, _ := st.GetUploadJob(context.Background(), "garbled"); job != nil {
		t.Errorf("an unreadable sidecar is not evidence of anything: %+v", job)
	}
	requireSourceIntact(t, m, "garbled", truncated)
	if _, err := os.Stat(m.InfoPath("garbled")); err != nil {
		t.Errorf("the sidecar must be left where it is: %v", err)
	}
}

// The unobservable-reappear branch deletes the row and falls through to the
// sidecar switch in the same pass, so the bypass is reachable without waiting
// for a terminal row to expire. A faulted mount that returns is also the case
// most likely to read a short or unreadable sidecar back.
func TestReturningBytesAreNotAdoptedOnTheirOwnObservedSize(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	job := &store.UploadJob{UploadID: "hidden2", MediaID: "media-hidden2", OriginalFilename: "a.jpg", ExpectedSize: int64(len(payload))}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	backdateJob(t, st, "hidden2", time.Hour)
	m.reconcileOnce()
	if got := jobStatus(t, st, "hidden2"); got != store.JobDiscarded {
		t.Fatalf("precondition: status = %q, want discarded", got)
	}

	// The mount returns, but only part of the upload came back with it.
	truncated := payload[:len(payload)/2]
	if err := os.WriteFile(m.DataPath("hidden2"), truncated, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}

	m.reconcileOnce()
	drainQueue(t, m)

	if got, _ := st.GetUploadJob(context.Background(), "hidden2"); got != nil && got.Status != store.JobUploading {
		t.Errorf("returned bytes were adopted at whatever length the mount handed back: %+v", got)
	}
	requireSourceIntact(t, m, "hidden2", truncated)
}

// The classification itself, because the three verdicts are what the branch
// above switches on and two of them lead to opposite treatments of the same
// bytes.
func TestSidecarEvidenceSeparatesAbsentFromUnreadable(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	writeSidecar(t, m, "whole", 4)
	if err := os.WriteFile(m.InfoPath("garbled"), []byte(`{"ID":"garb`), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	writeSidecarMeta(t, m, "mixed", "someone-else", 4, nil)

	for _, tc := range []struct {
		uploadID string
		want     sidecarEvidence
		why      string
	}{
		{"whole", sidecarTrusted, "a self-describing sidecar is the only one that may supply metadata"},
		{"nothing-here", sidecarAbsent, "ENOENT is the old ingest path's deliberately removed sidecar"},
		{"garbled", sidecarUntrusted, "a sidecar that cannot be parsed proves nothing, and must not read as an absence"},
		{"mixed", sidecarUntrusted, "a sidecar naming another upload must not supply this file's metadata"},
	} {
		info, got := m.trustedSidecar(tc.uploadID)
		if got != tc.want {
			t.Errorf("%s: evidence = %d, want %d: %s", tc.uploadID, got, tc.want, tc.why)
		}
		if (info != nil) != (tc.want == sidecarTrusted) {
			t.Errorf("%s: info = %+v, which does not match verdict %d", tc.uploadID, info, got)
		}
	}
}

func TestUntrustworthySidecarIsLeftAlone(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	if err := os.WriteFile(m.DataPath("mixed"), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	// A sidecar naming a different upload must never supply this file's
	// metadata: the checksum below belongs to someone else's bytes, so
	// adopting it would turn a good file into a deterministic rejection and
	// the discard worker would delete it.
	writeSidecarMeta(t, m, "mixed", "someone-else", int64(len(payload)), map[string]string{
		"filename": "a.jpg",
		"sha256":   "0000000000000000000000000000000000000000000000000000000000000000",
	})

	m.reconcileOnce()

	if job, _ := st.GetUploadJob(context.Background(), "mixed"); job != nil {
		t.Errorf("must not adopt from an untrusted sidecar: %+v", job)
	}
	if _, err := os.Stat(m.DataPath("mixed")); err != nil {
		t.Error("the data file must be left untouched")
	}
}

func TestAbsentSourceNeverTerminalizesADurableJob(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", jpegFixture(t))
	if err := m.EnsureDurable(context.Background(), "u1"); err != nil {
		t.Fatalf("ensure durable: %v", err)
	}

	// Simulate a faulted upload mount: everything vanishes. Ageing the row
	// puts it inside every idle window the reconciler applies, so only its
	// durability keeps it out of the stale-upload sweep.
	_ = os.Remove(m.DataPath("u1"))
	_ = os.Remove(m.InfoPath("u1"))
	backdateJob(t, st, "u1", time.Hour)

	m.reconcileOnce()
	m.reconcileOnce()

	job, _ := st.GetUploadJob(context.Background(), "u1")
	if job.Status != store.JobPending {
		t.Errorf("status = %q, want pending: absence must never terminalize durable work", job.Status)
	}
}

// A row admitted at pre-create whose client never sent a byte has nothing to
// delete, so closing it out is safe — but only once the reconciler has looked
// for its files and not found them.
func TestRowWithNoBytesIsClosedOutAsUnobservable(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	job := &store.UploadJob{UploadID: "ghost", MediaID: "media-ghost", OriginalFilename: "a.jpg", ExpectedSize: 10}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	backdateJob(t, st, "ghost", time.Hour)

	m.reconcileOnce()

	got, _ := st.GetUploadJob(context.Background(), "ghost")
	if got.Status != store.JobDiscarded {
		t.Fatalf("status = %q, want discarded", got.Status)
	}
	if got.TerminalReason != unobservableReason {
		t.Errorf("terminal reason = %q, want %q: a cancellation would authorise a deletion", got.TerminalReason, unobservableReason)
	}
}

// The stale-upload sweep must look at the filesystem before it closes a row
// out. Without that look it would terminalize live uploads, and the pre-finish
// barrier would then answer their clients with a permanent rejection.
func TestUploadWithBytesOnDiskIsNeverClosedOutAsUnobservable(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	job := &store.UploadJob{UploadID: "slow", MediaID: "media-slow", OriginalFilename: "a.jpg", ExpectedSize: 4096}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.WriteFile(m.DataPath("slow"), []byte("half"), 0o600); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	writeSidecar(t, m, "slow", 4096)
	backdateJob(t, st, "slow", time.Hour)

	m.reconcileOnce()

	if got := jobStatus(t, st, "slow"); got != store.JobUploading {
		t.Errorf("status = %q, want uploading: bytes on disk are not an abandoned upload", got)
	}
	if _, err := os.Stat(m.DataPath("slow")); err != nil {
		t.Errorf("the partial source must still be there: %v", err)
	}
}

// The whole point of closing a row out as 'unobservable' rather than
// cancelling it is that it deletes nothing and therefore stays reversible.
func TestUnobservableRowIsReAdoptedWhenItsBytesReturn(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	job := &store.UploadJob{UploadID: "hidden", MediaID: "media-hidden", OriginalFilename: "a.jpg", ExpectedSize: int64(len(payload))}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	backdateJob(t, st, "hidden", time.Hour)
	m.reconcileOnce()
	if got := jobStatus(t, st, "hidden"); got != store.JobDiscarded {
		t.Fatalf("precondition: status = %q, want discarded", got)
	}

	// The mount comes back with a complete upload on it.
	if err := os.WriteFile(m.DataPath("hidden"), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	writeSidecar(t, m, "hidden", int64(len(payload)))

	m.reconcileOnce()

	got, _ := st.GetUploadJob(context.Background(), "hidden")
	if got == nil || got.Status != store.JobPending {
		t.Fatalf("returned bytes must be adopted afresh, got %+v", got)
	}
	if _, err := os.Stat(m.DataPath("hidden")); err != nil {
		t.Errorf("the returned source must still be there: %v", err)
	}
}

// A job that finished only because its files were unreachable has to be
// reopened when they come back: leaving them would let the terminal row expire
// and the source be adopted and republished as a new upload.
func TestFilesReappearingForAFinishedJobReopenIt(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	for _, tc := range []struct {
		uploadID string
		from     store.JobStatus
		want     store.JobStatus
	}{
		{"done", store.JobComplete, store.JobCleanup},
		{"rejected", store.JobDiscarded, store.JobDiscarding},
	} {
		job := &store.UploadJob{UploadID: tc.uploadID, MediaID: "media-" + tc.uploadID, OriginalFilename: "a.jpg", ExpectedSize: 4}
		if err := st.CreateUploadingJob(context.Background(), job); err != nil {
			t.Fatalf("create %s: %v", tc.uploadID, err)
		}
		if _, err := st.DB().Exec(
			`UPDATE upload_jobs SET status = ?, terminal_reason = 'unsupported_type', terminal_at = ? WHERE upload_id = ?`,
			string(tc.from), store.NowMicros(), tc.uploadID); err != nil {
			t.Fatalf("finish %s: %v", tc.uploadID, err)
		}
		if err := os.WriteFile(m.DataPath(tc.uploadID), []byte("back"), 0o600); err != nil {
			t.Fatalf("restore data: %v", err)
		}

		m.reconcileOnce()

		if got := jobStatus(t, st, tc.uploadID); got != tc.want {
			t.Errorf("%s: status = %q, want %q", tc.uploadID, got, tc.want)
		}
		if _, err := os.Stat(m.DataPath(tc.uploadID)); err != nil {
			t.Errorf("%s: reconciliation must not delete anything itself: %v", tc.uploadID, err)
		}
	}
}

// The reopen above is a claim that files this job had verified as gone are
// back, so it has to rest on seeing one of them. The inventory keys on the
// data file or its sidecar, and reaching a row is not by itself evidence that
// either is there.
func TestReopeningAFinishedJobRequiresSeeingItsFiles(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	for _, tc := range []struct {
		uploadID string
		sidecar  bool
		want     store.JobStatus
		why      string
	}{
		// A sidecar with no data file is still something only this job can
		// clear: tusd cannot address the upload without its data file, and the
		// incomplete-upload janitor skips a candidate whose data file is gone.
		{"sidecar-only", true, store.JobCleanup, "an orphan sidecar has to be removed by the job that owns it"},
		// Nothing of this upload is on the volume, so nothing came back.
		{"nothing-there", false, store.JobComplete, "absence is not a reappearance"},
	} {
		job := &store.UploadJob{UploadID: tc.uploadID, MediaID: "media-" + tc.uploadID, OriginalFilename: "a.jpg", ExpectedSize: 4}
		if err := st.CreateUploadingJob(context.Background(), job); err != nil {
			t.Fatalf("create %s: %v", tc.uploadID, err)
		}
		if _, err := st.DB().Exec(
			`UPDATE upload_jobs SET status = 'complete', terminal_at = ? WHERE upload_id = ?`,
			store.NowMicros(), tc.uploadID); err != nil {
			t.Fatalf("finish %s: %v", tc.uploadID, err)
		}
		if tc.sidecar {
			writeSidecar(t, m, tc.uploadID, 4)
		}

		if err := m.reconcileOne(tc.uploadID); err != nil {
			t.Fatalf("%s: reconcile: %v", tc.uploadID, err)
		}

		if got := jobStatus(t, st, tc.uploadID); got != tc.want {
			t.Errorf("%s: status = %q, want %q: %s", tc.uploadID, got, tc.want, tc.why)
		}
	}
}

// Reconciliation must not touch work the workers own, or it would race their
// leases and could demote a job that is midway through publication.
func TestJobsOwnedByTheWorkersAreNotReconciled(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	for _, status := range []store.JobStatus{store.JobPending, store.JobProcessing, store.JobCleanup, store.JobDiscarding} {
		uploadID := "owned-" + string(status)
		job := &store.UploadJob{UploadID: uploadID, MediaID: "media-" + uploadID, OriginalFilename: "a.jpg", ExpectedSize: 4}
		if err := st.CreateUploadingJob(context.Background(), job); err != nil {
			t.Fatalf("create %s: %v", uploadID, err)
		}
		if _, err := st.DB().Exec(`UPDATE upload_jobs SET status = ? WHERE upload_id = ?`, string(status), uploadID); err != nil {
			t.Fatalf("set status %s: %v", status, err)
		}
		if err := os.WriteFile(m.DataPath(uploadID), []byte("back"), 0o600); err != nil {
			t.Fatalf("write data: %v", err)
		}
		backdateJob(t, st, uploadID, time.Hour)

		m.reconcileOnce()

		if got := jobStatus(t, st, uploadID); got != status {
			t.Errorf("status = %q, want %q: the workers own this row", got, status)
		}
	}
}

// Names that are not upload ids belong to tusd's locks and our own
// temporaries. Deriving a data path from one would let reconciliation act on a
// file it does not understand.
func TestNonUploadEntriesAreIgnored(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	for _, name := range []string{".hidden", "with space", "u1.lock"} {
		if err := os.WriteFile(filepath.Join(m.opts.UploadDir, name), payload, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(m.opts.UploadDir, "subdir"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := m.reconcileOnce(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, name := range []string{".hidden", "with space", "u1.lock", "u1", "subdir"} {
		if job, _ := st.GetUploadJob(context.Background(), name); job != nil {
			t.Errorf("adopted %q, which is not an upload: %+v", name, job)
		}
	}
}

func TestUnreadableUploadDirectoryKeepsReadinessClosed(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	opts.UploadDir = filepath.Join(opts.UploadDir, "not-yet-mounted")
	m := New(st, proc, opts)
	defer m.Stop()

	m.startupRecovery()

	if m.Ready() {
		t.Error("readiness must stay closed while the inventory of completed uploads is unknown")
	}
}

// Closing a row out is a conclusion drawn from the absence of its files, so
// the pass has to establish it can see the volume before it draws it. On a
// mount that faults hard every stale row would otherwise be closed out at
// once, on evidence the process never gathered.
func TestAnUnreadableUploadDirectoryClosesOutNothing(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	opts.UploadDir = filepath.Join(opts.UploadDir, "not-yet-mounted")
	m := New(st, proc, opts)
	defer m.Stop()

	job := &store.UploadJob{UploadID: "ghost", MediaID: "media-ghost", OriginalFilename: "a.jpg", ExpectedSize: 10}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	backdateJob(t, st, "ghost", time.Hour)

	if err := m.reconcileOnce(); err == nil {
		t.Error("a pass that could not read the upload directory must report the failure")
	}

	if got := jobStatus(t, st, "ghost"); got != store.JobUploading {
		t.Errorf("status = %q, want uploading: an unreadable volume is not evidence the bytes are gone", got)
	}
}

func TestStartupRecoveryOpensReadinessAndClearsPreCrashLeases(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	// A job the previous process was midway through, still carrying its lease.
	job := &store.UploadJob{UploadID: "crashed", MediaID: "media-crashed", OriginalFilename: "a.jpg", ExpectedSize: 4}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.DB().Exec(
		`UPDATE upload_jobs SET status = 'processing', lease_token = 'pre-crash', lease_until = ? WHERE upload_id = ?`,
		store.NowMicros()+time.Hour.Microseconds(), "crashed"); err != nil {
		t.Fatalf("simulate crash: %v", err)
	}

	m.startupRecovery()

	if !m.Ready() {
		t.Fatal("readiness must open once the inventory has run")
	}
	got, _ := st.GetUploadJob(context.Background(), "crashed")
	if got.Status != store.JobPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.LeaseToken != "" {
		t.Errorf("lease token = %q, want it cleared so a worker can claim the job now", got.LeaseToken)
	}
}

// Recovery runs on the manager's lifetime, so a shutdown that arrives while it
// is working must leave the gate shut rather than admit uploads into a process
// that is closing its database.
func TestShutdownDuringStartupLeavesReadinessClosed(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	payload := jpegFixture(t)
	if err := os.WriteFile(m.DataPath("racy"), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	writeSidecar(t, m, "racy", int64(len(payload)))

	m.Stop() // cancels the lifetime before Start ever reaches the inventory
	m.Start()

	if m.Ready() {
		t.Error("readiness must stay closed when shutdown overtakes recovery")
	}
	if job, _ := st.GetUploadJob(context.Background(), "racy"); job != nil {
		t.Errorf("nothing may be adopted into a process that is shutting down: %+v", job)
	}
}

// A pass that skipped its work must not report that it completed one, because
// completion is what opens the readiness gate.
func TestAnInventoryCutShortByShutdownIsReportedAsIncomplete(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	payload := jpegFixture(t)
	if err := os.WriteFile(m.DataPath("pending-work"), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	writeSidecar(t, m, "pending-work", int64(len(payload)))

	m.Stop() // cancels the lifetime every store call in the pass runs on

	if err := m.reconcileOnce(); err == nil {
		t.Error("reconcileOnce reported a complete pass while every step of it was being cancelled")
	}
	if job, _ := st.GetUploadJob(context.Background(), "pending-work"); job != nil {
		t.Errorf("nothing may be adopted once the lifetime is cancelled: %+v", job)
	}
}

// Adopted metadata comes off a shared volume, and the filename is echoed back
// to browsers in Content-Disposition and used to name downloads.
func TestAdoptedFilenameIsSanitised(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	if err := os.WriteFile(m.DataPath("nasty"), payload, 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	writeSidecarMeta(t, m, "nasty", "nasty", int64(len(payload)), map[string]string{
		"filename": "../../etc/passwd",
	})

	m.reconcileOnce()

	job, _ := st.GetUploadJob(context.Background(), "nasty")
	if job == nil {
		t.Fatal("the upload should still have been adopted")
	}
	if job.OriginalFilename != "passwd" {
		t.Errorf("original filename = %q, want the bare base name", job.OriginalFilename)
	}
}

// runReconciler's own body: everything below happens only because its ticker
// fired, since all of it is seeded after Start has already returned.
func TestReconcilerTickAdoptsUploadsAndExpiresTerminalJobs(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	m.Start()
	defer m.Stop()
	if !m.Ready() {
		t.Fatal("precondition: startup recovery should have opened the gate")
	}

	placeUpload(t, m, "ticked", jpegFixture(t))

	expired := &store.UploadJob{UploadID: "expired", MediaID: "media-expired", OriginalFilename: "a.jpg", ExpectedSize: 4}
	if err := st.CreateUploadingJob(context.Background(), expired); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.DB().Exec(
		`UPDATE upload_jobs SET status = 'discarded', terminal_at = ? WHERE upload_id = ?`,
		store.NowMicros()-2*m.opts.JobRetention.Microseconds(), "expired"); err != nil {
		t.Fatalf("age the terminal row: %v", err)
	}

	awaitCondition(t, "the ticker adopts a completed upload", func() bool {
		job, err := st.GetUploadJob(context.Background(), "ticked")
		return err == nil && job != nil && job.SourceCompletedAt != nil
	})
	awaitCondition(t, "the ticker expires a terminal row past its retention", func() bool {
		job, err := st.GetUploadJob(context.Background(), "expired")
		return err == nil && job == nil
	})
}

// The not-ready branch of the ticker: startup could not read the inventory, so
// every later tick retries it until it can. What it must not retry is the
// startup requeue. That statement resets ownership unconditionally, and its
// doc comment names the premise it rests on -- "immediately after a restart",
// when no worker is running. By the time this branch executes the pool has
// been launched, so a lease it would clear may belong to a worker midway
// through an attempt.
func TestReconcilerTickOpensReadinessWithoutStrippingLiveLeases(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	uploadDir := filepath.Join(opts.UploadDir, "not-yet-mounted")
	opts.UploadDir = uploadDir
	m := New(st, proc, opts)

	m.Start()
	defer m.Stop()
	if m.Ready() {
		t.Fatal("precondition: the gate must start closed when the inventory failed")
	}

	// Seeded after Start, so only a later tick can act on it, and held under a
	// lease that has not expired -- which is indistinguishable from the lease
	// of a worker of this process that is running right now.
	job := &store.UploadJob{UploadID: "leased", MediaID: "media-leased", OriginalFilename: "a.jpg", ExpectedSize: 4}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.DB().Exec(
		`UPDATE upload_jobs SET status = 'processing', lease_token = 'held', lease_until = ? WHERE upload_id = ?`,
		store.NowMicros()+time.Hour.Microseconds(), "leased"); err != nil {
		t.Fatalf("hold the job under a live lease: %v", err)
	}

	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		t.Fatalf("mount the upload dir: %v", err)
	}

	awaitCondition(t, "a later tick opens readiness", m.Ready)

	got, _ := st.GetUploadJob(context.Background(), "leased")
	if got == nil {
		t.Fatal("the row disappeared")
	}
	if got.LeaseToken != "held" || got.Status != store.JobProcessing {
		t.Errorf("status = %q lease = %q, want processing/held: the retry path took a job away from a live worker",
			got.Status, got.LeaseToken)
	}
}

// placeUpload publishes a complete upload into the watch directory atomically,
// so a concurrent reconciler tick can never observe a half-written data file
// and adopt it at the wrong size.
func placeUpload(t *testing.T, m *Manager, uploadID string, payload []byte) {
	t.Helper()
	writeSidecar(t, m, uploadID, int64(len(payload)))
	staging := filepath.Join(m.opts.UploadDir, ".staging-"+uploadID)
	if err := os.WriteFile(staging, payload, 0o600); err != nil {
		t.Fatalf("stage upload: %v", err)
	}
	if err := os.Rename(staging, m.DataPath(uploadID)); err != nil {
		t.Fatalf("place upload: %v", err)
	}
}

func awaitCondition(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting until %s", what)
}
