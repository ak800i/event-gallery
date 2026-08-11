package ingest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"event-gallery/backend/internal/store"
)

func drainQueue(t *testing.T, m *Manager) {
	t.Helper()
	for i := 0; i < 50; i++ {
		worked, err := m.claimAndRunOnce()
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("queue did not drain")
}

// requireSourceIntact is the invariant this whole feature exists to protect:
// until publication has committed, the guest's upload is the only complete
// copy, so it must still be there and still be the same bytes.
func requireSourceIntact(t *testing.T, m *Manager, uploadID string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(m.DataPath(uploadID))
	if err != nil {
		t.Fatalf("THE INCIDENT: source %s is gone: %v", uploadID, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("source %s changed: %d bytes on disk, %d uploaded", uploadID, len(got), len(want))
	}
}

func TestPublishDeletesSourceOnlyAfterCommit(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	seedCompleteUpload(t, m, st, "u1", payload)
	if err := m.EnsureDurable(context.Background(), "u1"); err != nil {
		t.Fatalf("durable: %v", err)
	}

	drainQueue(t, m)

	job, _ := st.GetUploadJob(context.Background(), "u1")
	if job.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete (last error: %s)", job.Status, job.LastError)
	}
	if job.ResultMediaID == "" {
		t.Error("published job must record its media id")
	}
	if _, err := os.Stat(m.DataPath("u1")); !os.IsNotExist(err) {
		t.Error("source must be removed after publication commits")
	}
	if _, err := os.Stat(m.InfoPath("u1")); !os.IsNotExist(err) {
		t.Error("the sidecar goes with the source, or the janitor trips over it forever")
	}
	if err := proc.VerifyOriginal(job.StoredFilename, int64(len(payload)), job.AuthoritativeSHA256); err != nil {
		t.Errorf("published original must be intact: %v", err)
	}
	item, err := st.GetBySHA256(context.Background(), job.AuthoritativeSHA256)
	if err != nil {
		t.Fatalf("published row must exist: %v", err)
	}
	if item.ID != job.ResultMediaID || item.StoredFilename != job.StoredFilename {
		t.Errorf("row %+v does not describe the artifact the job recorded", item)
	}
	// Everything derived from the stored original has to reach the row, or the
	// gallery renders an item with no thumbnail and no dimensions.
	if item.Width != 2 || item.Height != 2 || !item.HasThumbnail {
		t.Errorf("derived metadata missing: %dx%d thumbnail=%v", item.Width, item.Height, item.HasThumbnail)
	}
	if _, err := os.Stat(proc.ThumbnailPath(item.ID)); err != nil {
		t.Errorf("a row claiming a thumbnail must have one on disk: %v", err)
	}
	if item.MimeType != "image/jpeg" || item.SizeBytes != int64(len(payload)) {
		t.Errorf("row records %s/%d, want image/jpeg/%d", item.MimeType, item.SizeBytes, len(payload))
	}
}

func TestTransientFailureNeverDeletesTheSource(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	m := New(st, proc, opts)
	defer m.Stop()

	payload := jpegFixture(t)
	seedCompleteUpload(t, m, st, "u1", payload)
	_ = m.EnsureDurable(context.Background(), "u1")

	// Make preparation fail transiently. A regular file where the originals
	// directory must be makes every create and rename fail with ENOTDIR, which
	// works regardless of uid — chmod would not, because the test container
	// runs as root and root bypasses directory permission bits.
	originals := proc.OriginalsDir()
	if err := os.RemoveAll(originals); err != nil {
		t.Fatalf("clear originals: %v", err)
	}
	if err := os.WriteFile(originals, []byte("blocker"), 0o600); err != nil {
		t.Fatalf("block originals dir: %v", err)
	}

	// Drained, not claimed once: if a transient failure were ever routed to
	// discarding, the discard stage would run in the same drain and take the
	// source with it, which is the failure this test exists to catch.
	drainQueue(t, m)

	job, _ := st.GetUploadJob(context.Background(), "u1")
	if job.Status != store.JobPending {
		t.Errorf("status = %q, want pending: transient failures retry forever", job.Status)
	}
	if job.ProcessingFailures != 1 {
		t.Errorf("processing_failures = %d, want 1", job.ProcessingFailures)
	}
	requireSourceIntact(t, m, "u1", payload)
}

func TestChecksumMismatchDiscardsBeforeAnyArtifactExists(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	job := &store.UploadJob{
		UploadID:         "u1",
		MediaID:          "media-u1",
		OriginalFilename: "a.jpg",
		ExpectedSize:     int64(len(payload)),
		DeclaredSHA256:   "0000000000000000000000000000000000000000000000000000000000000000",
	}
	if err := st.CreateUploadingJob(context.Background(), job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.WriteFile(m.DataPath("u1"), payload, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = m.EnsureDurable(context.Background(), "u1")

	drainQueue(t, m)

	got, _ := st.GetUploadJob(context.Background(), "u1")
	if got.Status != store.JobDiscarded {
		t.Errorf("status = %q, want discarded", got.Status)
	}
	if got.TerminalReason != "checksum_mismatch" {
		t.Errorf("terminal_reason = %q, want checksum_mismatch", got.TerminalReason)
	}
	// The rejection is decided before anything is copied, so the discard can
	// never be the thing that removes a published artifact.
	entries, err := os.ReadDir(proc.OriginalsDir())
	if err != nil {
		t.Fatalf("read originals: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a rejected upload left %d files in originals", len(entries))
	}
}

func TestDuplicateResolutionValidatesTheSurvivingOriginal(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)

	// A second, intact row so the storage health sample stays positive.
	// Without it the health gate trips first and this test would pass while
	// never reaching the duplicate-validation code at all.
	insertMediaRow(t, st, "keeper", "keeper.jpg", "keeper-hash")
	if err := os.WriteFile(proc.OriginalPath("keeper.jpg"), []byte("keeper"), 0o600); err != nil {
		t.Fatalf("write keeper: %v", err)
	}

	// Publish once.
	seedCompleteUpload(t, m, st, "u1", payload)
	_ = m.EnsureDurable(context.Background(), "u1")
	drainQueue(t, m)

	// Corrupt the surviving original, then upload the same bytes again.
	// Corrupting rather than deleting keeps the health sample positive, so the
	// failure is attributable to hash validation and nothing else.
	first, _ := st.GetUploadJob(context.Background(), "u1")
	if err := os.WriteFile(proc.OriginalPath(first.StoredFilename), []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("corrupt original: %v", err)
	}

	seedCompleteUpload(t, m, st, "u2", payload)
	_ = m.EnsureDurable(context.Background(), "u2")
	_, _ = m.claimAndRunOnce()

	second, _ := st.GetUploadJob(context.Background(), "u2")
	if second.Status != store.JobPending {
		t.Errorf("status = %q, want pending: a corrupt authoritative original is an integrity fault, not a duplicate", second.Status)
	}
	if !strings.Contains(second.LastError, "integrity fault") {
		t.Errorf("last_error = %q, want it to name the integrity fault (proves which branch ran)", second.LastError)
	}
	requireSourceIntact(t, m, "u2", payload)
}

func TestUnsupportedTypeIsDeterministicallyRejected(t *testing.T) {
	// The fixture allows image/jpeg only, so a PNG is recognised content that
	// is nonetheless not permitted — a different branch from bytes no
	// signature matches, and equally hopeless to retry.
	cases := map[string][]byte{
		"no signature matches":       []byte("not a media file at all"),
		"recognised but not allowed": pngFixture(t),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			st, proc := newIngestFixture(t)
			m := New(st, proc, testOptions(t))
			defer m.Stop()

			seedCompleteUpload(t, m, st, "u1", payload)
			_ = m.EnsureDurable(context.Background(), "u1")

			drainQueue(t, m)

			job, _ := st.GetUploadJob(context.Background(), "u1")
			if job.TerminalReason != "unsupported_type" {
				t.Errorf("terminal_reason = %q, want unsupported_type", job.TerminalReason)
			}
			if job.Status != store.JobDiscarded {
				t.Errorf("status = %q, want discarded", job.Status)
			}
		})
	}
}

// A file the gallery already holds must not be stored twice, and resolving it
// must never touch the copy that is already published.
func TestDuplicateContentPublishesOnceAndRemovesTheSecondCopy(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	seedCompleteUpload(t, m, st, "u1", payload)
	_ = m.EnsureDurable(context.Background(), "u1")
	drainQueue(t, m)
	first, _ := st.GetUploadJob(context.Background(), "u1")

	seedCompleteUpload(t, m, st, "u2", payload)
	_ = m.EnsureDurable(context.Background(), "u2")
	drainQueue(t, m)

	second, _ := st.GetUploadJob(context.Background(), "u2")
	if second.Status != store.JobComplete {
		t.Fatalf("status = %q, want complete (last error: %s)", second.Status, second.LastError)
	}
	if second.ResultMediaID != first.ResultMediaID {
		t.Errorf("result_media_id = %q, want the id that already owns this content (%q)", second.ResultMediaID, first.ResultMediaID)
	}
	if second.MediaID == second.ResultMediaID {
		t.Fatal("the second upload published its own row, so this is not the duplicate path")
	}
	if err := proc.VerifyOriginal(first.StoredFilename, int64(len(payload)), first.AuthoritativeSHA256); err != nil {
		t.Errorf("the surviving original must be untouched: %v", err)
	}
	if _, err := os.Stat(proc.OriginalPath(second.StoredFilename)); !os.IsNotExist(err) {
		t.Errorf("the duplicate's own copy must be removed once its row resolved to another item: %v", err)
	}
}

// Publishing into a media volume we cannot prove is mounted would write the
// original where the real volume cannot see it, and that file would then
// satisfy the health gate and authorize deleting the source.
func TestProcessingRefusesWhileStorageIsUnproven(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	// A row whose original is absent is what an unmounted media volume looks
	// like from the database's side.
	insertMediaRow(t, st, "old", "old.jpg", "old-hash")

	payload := jpegFixture(t)
	seedCompleteUpload(t, m, st, "u1", payload)
	_ = m.EnsureDurable(context.Background(), "u1")

	_, _ = m.claimAndRunOnce()

	job, _ := st.GetUploadJob(context.Background(), "u1")
	if job.Status != store.JobPending {
		t.Errorf("status = %q, want pending: an unproven volume is transient", job.Status)
	}
	if job.ProcessingFailures != 1 {
		t.Errorf("processing_failures = %d, want 1", job.ProcessingFailures)
	}
	if entries, err := os.ReadDir(proc.OriginalsDir()); err != nil || len(entries) != 0 {
		t.Errorf("nothing may be written into an unproven volume: %v %v", entries, err)
	}
	requireSourceIntact(t, m, "u1", payload)
}

// A volume that disappears between publication and cleanup must not produce a
// cascade of deletions, so cleanup re-checks the filesystem rather than
// trusting the cached flag.
func TestCleanupRefusesWhileStorageIsUnproven(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	payload := jpegFixture(t)
	seedCompleteUpload(t, m, st, "u1", payload)
	_ = m.EnsureDurable(context.Background(), "u1")

	if worked, err := m.claimAndRunOnce(); !worked || err != nil {
		t.Fatalf("publication pass: worked=%v err=%v", worked, err)
	}
	if got := jobStatus(t, st, "u1"); got != store.JobCleanup {
		t.Fatalf("status = %q, want cleanup before the volume drops", got)
	}

	// The media volume vanishes: its row is still in the database, but the
	// file it names is no longer visible.
	published, _ := st.GetUploadJob(context.Background(), "u1")
	if err := os.Remove(proc.OriginalPath(published.StoredFilename)); err != nil {
		t.Fatalf("simulate a faulted mount: %v", err)
	}

	if worked, err := m.claimAndRunOnce(); !worked || err != nil {
		t.Fatalf("cleanup pass: worked=%v err=%v", worked, err)
	}

	job, _ := st.GetUploadJob(context.Background(), "u1")
	if job.Status != store.JobCleanup {
		t.Errorf("status = %q, want cleanup: the deletion is deferred, not abandoned", job.Status)
	}
	if job.CleanupFailures != 1 {
		t.Errorf("cleanup_failures = %d, want 1", job.CleanupFailures)
	}
	requireSourceIntact(t, m, "u1", payload)
}

// tusd being briefly unreachable must not lose the source, terminalize the
// job, or undo the publication.
func TestFailedTerminationKeepsTheJobInCleanup(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	opts.Terminator = failingTerminator{}
	m := New(st, proc, opts)
	defer m.Stop()

	payload := jpegFixture(t)
	seedCompleteUpload(t, m, st, "u1", payload)
	_ = m.EnsureDurable(context.Background(), "u1")

	drainQueue(t, m)

	job, _ := st.GetUploadJob(context.Background(), "u1")
	if job.Status != store.JobCleanup {
		t.Errorf("status = %q, want cleanup: the removal is retried, never given up on", job.Status)
	}
	if job.CleanupFailures != 1 {
		t.Errorf("cleanup_failures = %d, want 1", job.CleanupFailures)
	}
	if err := proc.VerifyOriginal(job.StoredFilename, int64(len(payload)), job.AuthoritativeSHA256); err != nil {
		t.Errorf("a failed cleanup must not disturb the published artifact: %v", err)
	}
	if _, err := st.GetBySHA256(context.Background(), job.AuthoritativeSHA256); err != nil {
		t.Errorf("the published row must survive a failed cleanup: %v", err)
	}
	requireSourceIntact(t, m, "u1", payload)
}

type failingTerminator struct{}

func (failingTerminator) Terminate(context.Context, string) error {
	return errors.New("tusd is unreachable")
}

// The janitor claims an abandoned upload to discarding and then fails to reach
// tusd. Nothing else ever revisits that row — RequeueStartup clears the lease
// but leaves the status — so the discard stage has to adopt it, or the upload
// holds its disk forever.
func TestDiscardStageAdoptsARowTheJanitorStranded(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", jpegFixture(t))
	if err := st.ClaimUploadingForDiscard(context.Background(), "u1", store.NowMicros()); err != nil {
		t.Fatalf("janitor claim: %v", err)
	}

	drainQueue(t, m)

	job, _ := st.GetUploadJob(context.Background(), "u1")
	if job.Status != store.JobDiscarded {
		t.Fatalf("status = %q, want discarded: a stranded discard must be adopted (last error: %s)", job.Status, job.LastError)
	}
	if job.TerminalReason != "cancelled" {
		t.Errorf("terminal_reason = %q, want the janitor's reason preserved", job.TerminalReason)
	}
	if _, err := os.Stat(m.DataPath("u1")); !os.IsNotExist(err) {
		t.Error("adopting the row must actually return the disk")
	}
}

// runWorker's drain path never consults the lifetime: its only exit is a false
// return. A claimAndRunOnce that kept working after cancellation would make
// Stop block until the queue drained, while main waits to close the database.
func TestClaimAndRunOnceStopsWhenTheLifetimeEnds(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))

	payload := jpegFixture(t)
	seedCompleteUpload(t, m, st, "u1", payload)
	if err := m.EnsureDurable(context.Background(), "u1"); err != nil {
		t.Fatalf("durable: %v", err)
	}

	m.Stop()

	worked, err := m.claimAndRunOnce()
	if worked {
		t.Error("work was claimed after the lifetime ended, so Stop would never return")
	}
	if err != nil {
		t.Errorf("shutdown is not a failure to log: %v", err)
	}
	if got := jobStatus(t, st, "u1"); got != store.JobPending {
		t.Errorf("status = %q, want pending: a cancelled manager must claim nothing", got)
	}
	requireSourceIntact(t, m, "u1", payload)
}

func TestPublicationRecordsTheUploadAudit(t *testing.T) {
	cases := map[string]struct{ guest, wantActor string }{
		"named guest": {guest: "Ana", wantActor: "Ana"},
		"no name":     {guest: "", wantActor: "anonymous guest"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			st, proc := newIngestFixture(t)
			m := New(st, proc, testOptions(t))
			defer m.Stop()

			seedCompleteUpload(t, m, st, "u1", jpegFixture(t))
			if _, err := st.DB().Exec(`UPDATE upload_jobs SET guest_name = ? WHERE upload_id = ?`, tc.guest, "u1"); err != nil {
				t.Fatalf("set guest name: %v", err)
			}
			_ = m.EnsureDurable(context.Background(), "u1")
			drainQueue(t, m)

			job, _ := st.GetUploadJob(context.Background(), "u1")
			var action, actor, mediaID, filename string
			err := st.DB().QueryRow(`SELECT action, actor, COALESCE(media_id, ''), COALESCE(filename, '') FROM audit_log`).
				Scan(&action, &actor, &mediaID, &filename)
			if err != nil {
				t.Fatalf("an upload must leave an audit entry: %v", err)
			}
			if action != "upload" {
				t.Errorf("action = %q, want upload", action)
			}
			if actor != tc.wantActor {
				t.Errorf("actor = %q, want %q", actor, tc.wantActor)
			}
			if mediaID != job.ResultMediaID {
				t.Errorf("media_id = %q, want the published id %q", mediaID, job.ResultMediaID)
			}
			if filename != job.OriginalFilename {
				t.Errorf("filename = %q, want %q", filename, job.OriginalFilename)
			}
		})
	}
}

func TestRejectedUploadIsAudited(t *testing.T) {
	st, proc := newIngestFixture(t)
	m := New(st, proc, testOptions(t))
	defer m.Stop()

	seedCompleteUpload(t, m, st, "u1", []byte("not a media file at all"))
	_ = m.EnsureDurable(context.Background(), "u1")
	drainQueue(t, m)

	var details string
	if err := st.DB().QueryRow(`SELECT COALESCE(details, '') FROM audit_log WHERE action = 'upload'`).Scan(&details); err != nil {
		t.Fatalf("a rejection must leave an audit entry: %v", err)
	}
	if !strings.Contains(details, "unsupported") {
		t.Errorf("details = %q, want the rejection reason", details)
	}
}
