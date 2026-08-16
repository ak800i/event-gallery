package ingest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/models"
	"event-gallery/backend/internal/store"
)

// claimAndRunOnce takes one due job and advances it by one stage. It reports
// whether it did any work so the worker can drain a burst before sleeping.
func (m *Manager) claimAndRunOnce() (bool, error) {
	// runWorker's drain path never consults the lifetime, so a claim made
	// after cancellation would keep the worker running and hold Stop open
	// while main waits to close the database. Shutdown is not a failure, so
	// it is reported as "no work" rather than as an error.
	if m.lifetime.Err() != nil {
		return false, nil
	}
	now := store.NowMicros()

	stages := []struct {
		from, to store.JobStatus
		run      func(*store.UploadJob)
	}{
		// Cleanup and discard come first. They are one stat plus one HTTP
		// DELETE, and they are what returns disk to the pool; processing is a
		// whole-file copy. Running them last would mean that under sustained
		// load every tus source is retained for the entire burst on top of its
		// permanent copy, and INGEST_MIN_FREE_BYTES would start refusing new
		// uploads because of work already finished.
		{store.JobCleanup, store.JobCleanup, m.runCleanup},
		// Also adopts rows the janitor claimed to discarding and then could
		// not terminate: nothing else ever revisits them, and their bytes stay
		// on disk until this stage picks them up.
		{store.JobDiscarding, store.JobDiscarding, m.runDiscard},
		{store.JobPending, store.JobProcessing, m.runProcessing},
		// Reclaims work abandoned by an expired lease without waiting for a
		// restart. The claim's own lease_until filter makes this a no-op while
		// another worker still owns the job.
		{store.JobProcessing, store.JobProcessing, m.runProcessing},
	}

	for _, stage := range stages {
		job, err := m.store.ClaimNextJob(m.lifetime, stage.from, stage.to, now, m.leaseDuration())
		if err != nil {
			if m.lifetime.Err() != nil {
				return false, nil // the claim was interrupted by shutdown
			}
			return false, err
		}
		if job != nil {
			stage.run(job)
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) runProcessing(job *store.UploadJob) {
	// The deadline starts at claim time, before the health check, so the
	// attempt context always expires before the lease does.
	ctx, cancel := context.WithTimeout(m.lifetime, m.opts.ProcessingTimeout)
	defer cancel()

	// Publishing into a media volume we cannot prove is mounted would write the
	// original somewhere the real volume cannot see, and that new file would
	// then satisfy the health gate's "one original exists" test and authorize
	// deleting the tus source.
	if err := m.health.Check(ctx); err != nil {
		next := store.NowMicros() + m.backoffFor(job.ProcessingFailures).Microseconds()
		if err := m.store.ReleaseForRetry(m.lifetime, job.UploadID, job.LeaseToken, store.JobPending, next, "processing_failures", truncateError(err)); err != nil {
			m.logRetryScheduleFailure("failed to reschedule after health failure", "processing", job.UploadID, err)
		}
		return
	}

	if err := m.prepareAndPublish(ctx, job); err != nil {
		var rejection *clientRejection
		if errors.As(err, &rejection) {
			// The only class permitted to discard a complete source, and only
			// while no final artifact exists yet.
			m.recordUploadAudit(job, "", rejection.auditDetail)
			if err := m.store.FinishJob(m.lifetime, job.UploadID, job.LeaseToken, store.JobDiscarding, rejection.reason, store.NowMicros()); err != nil {
				slog.Error("failed to record rejection", "operation", "processing", "upload_id", job.UploadID, "error", err)
			}
			slog.Warn("rejected upload", "operation", "processing", "upload_id", job.UploadID, "reason", rejection.reason)
			m.Wake()
			return
		}

		// Everything else is transient and keeps both the source and any
		// prepared artifacts. There is no failure count that deletes data.
		next := store.NowMicros() + m.backoffFor(job.ProcessingFailures).Microseconds()
		if err := m.store.ReleaseForRetry(m.lifetime, job.UploadID, job.LeaseToken, store.JobPending, next, "processing_failures", truncateError(err)); err != nil {
			m.logRetryScheduleFailure("failed to schedule retry", "processing", job.UploadID, err)
		}
		slog.Warn("ingest attempt failed, retrying",
			"operation", "processing", "upload_id", job.UploadID,
			"processing_failures", job.ProcessingFailures+1, "error", err)
		return
	}
	m.Wake()
}

// clientRejection marks a deterministic client fault. Repeating the work
// cannot change the outcome, so the source may be discarded.
type clientRejection struct {
	reason      string
	auditDetail string
}

func (e *clientRejection) Error() string { return "client rejection: " + e.reason }

// recordUploadAudit preserves the admin audit-log coverage the deleted
// post-finish handler used to provide. Best effort by design: an audit write
// must never fail an upload.
func (m *Manager) recordUploadAudit(job *store.UploadJob, mediaID, detail string) {
	actor := job.GuestName
	if actor == "" {
		actor = "anonymous guest"
	}
	_ = m.store.RecordAudit(m.lifetime, models.ActionUpload, actor, mediaID, job.OriginalFilename, detail)
}

func (m *Manager) prepareAndPublish(ctx context.Context, job *store.UploadJob) error {
	sourcePath := m.DataPath(job.UploadID)

	// Sniff reports unrecognised content as an error, not as an empty MIME
	// type, so the deterministic rejection has to be recognised here or
	// garbage would retry forever.
	mimeType, kind, err := media.Sniff(sourcePath)
	if err != nil {
		var unsupported *media.ErrUnsupportedType
		if errors.As(err, &unsupported) {
			return &clientRejection{reason: "unsupported_type", auditDetail: "rejected: unsupported or unrecognized file type"}
		}
		return fmt.Errorf("sniff source: %w", err)
	}
	if !media.IsAllowed(mimeType, kind, m.processor.AllowedImageMIMEs, m.processor.AllowedVideoMIMEs) {
		return &clientRejection{reason: "unsupported_type", auditDetail: "rejected: unsupported or unrecognized file type"}
	}

	sum, err := media.SHA256FileContext(ctx, sourcePath)
	if err != nil {
		return fmt.Errorf("hash source: %w", err)
	}
	if job.DeclaredSHA256 != "" && job.DeclaredSHA256 != sum {
		return &clientRejection{reason: "checksum_mismatch", auditDetail: "rejected: checksum mismatch after upload"}
	}

	stat, err := os.Stat(sourcePath)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	// Persist the artifact identity before the copy, so a crash can be
	// recovered by name and hash instead of by re-copying or deleting.
	storedFilename := job.MediaID + media.ExtensionForMIME(mimeType, job.OriginalFilename)
	if err := m.store.RecordArtifactIdentity(ctx, job.UploadID, job.LeaseToken, storedFilename, mimeType, sum); err != nil {
		return err
	}

	if err := m.processor.PrepareOriginal(ctx, media.PrepareRequest{
		SourcePath:     sourcePath,
		MediaID:        job.MediaID,
		LeaseToken:     job.LeaseToken,
		StoredFilename: storedFilename,
		Size:           stat.Size(),
		SHA256:         sum,
	}); err != nil {
		return fmt.Errorf("prepare original: %w", err)
	}
	if err := m.processor.VerifyOriginal(storedFilename, stat.Size(), sum); err != nil {
		return fmt.Errorf("verify prepared original: %w", err)
	}
	// prepared_at means "a verified final original exists", so it is only
	// committed once that is true.
	if err := m.store.RecordPrepared(ctx, job.UploadID, job.LeaseToken); err != nil {
		return err
	}

	// Before we may resolve a duplicate, the copy we would keep must be proven
	// intact. Otherwise "duplicate detected" could delete a good new file in
	// favor of a corrupt old one.
	existing, err := m.store.GetBySHA256(ctx, sum)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("look up existing content: %w", err)
	}
	if existing != nil {
		if err := m.processor.VerifyOriginal(existing.StoredFilename, existing.SizeBytes, existing.SHA256); err != nil {
			slog.Error("authoritative original failed validation; refusing to treat this upload as a duplicate",
				"operation", "duplicate_check", "upload_id", job.UploadID, "existing_id", existing.ID, "error", err)
			return fmt.Errorf("integrity fault on existing original %s: %w", existing.ID, err)
		}
	}

	result := m.processor.Derive(ctx, m.processor.OriginalPath(storedFilename), job.MediaID, kind, mimeType)

	item := &models.MediaItem{
		ID:               job.MediaID,
		OriginalFilename: job.OriginalFilename,
		StoredFilename:   storedFilename,
		Kind:             kind,
		MimeType:         mimeType,
		SizeBytes:        stat.Size(),
		SHA256:           sum,
		Width:            result.Width,
		Height:           result.Height,
		DurationSeconds:  result.DurationSeconds,
		HasThumbnail:     result.HasThumbnail,
		HasPreview:       result.HasPreview,
		CapturedAt:       result.CapturedAt,
		UploadedAt:       time.Now(),
		UploaderName:     job.GuestName,
		UploaderIP:       job.UploaderIP,
	}

	resultID, duplicate, err := m.store.PublishMedia(ctx, job.UploadID, job.LeaseToken, item, store.NowMicros())
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	if duplicate {
		m.recordUploadAudit(job, resultID, "duplicate content ignored (already in gallery)")
	} else {
		m.recordUploadAudit(job, resultID, "")
	}
	slog.Info("upload published", "operation", "publication",
		"upload_id", job.UploadID, "media_id", resultID, "duplicate", duplicate)
	return nil
}

// runCleanup removes the source, and a duplicate's own artifacts, now that a
// media row is committed. This is the only path that may delete a source
// after a successful publication.
func (m *Manager) runCleanup(job *store.UploadJob) {
	// Bounded exactly like runProcessing, and for the same reason: the lease
	// outlives the attempt timeout by design, so an attempt that cannot exceed
	// it can never run beside the worker that reclaimed the job. Every call
	// below happens to be individually bounded today; this is what keeps that
	// true of the next one added.
	ctx, cancel := context.WithTimeout(m.lifetime, m.opts.ProcessingTimeout)
	defer cancel()

	if !m.deletionAllowed(ctx, job) {
		return
	}
	if job.ResultMediaID != "" && job.ResultMediaID != job.MediaID {
		if !m.removeArtifactsIfUnowned(ctx, job) {
			return
		}
	}
	// The media row is committed, so any temporary from an earlier attempt is
	// provably redundant.
	m.sweepTemporaries(job)
	m.finishBySourceRemoval(ctx, job, store.JobComplete, "")
}

// runDiscard terminates a rejected or cancelled upload. Both reasons are
// decisions we honor, so no further guard is needed here: an upload whose bytes
// were simply never observable never reaches this state — it is closed out as
// 'unobservable' instead, which deletes nothing and stays reversible.
func (m *Manager) runDiscard(job *store.UploadJob) {
	ctx, cancel := context.WithTimeout(m.lifetime, m.opts.ProcessingTimeout)
	defer cancel()

	if !m.deletionAllowed(ctx, job) {
		return
	}
	if !m.removeArtifactsIfUnowned(ctx, job) {
		return
	}
	m.sweepTemporaries(job)
	m.finishBySourceRemoval(ctx, job, store.JobDiscarded, job.TerminalReason)
}

// deletionAllowed re-checks storage health against the filesystem rather than
// reading the cached flag, because a volume that disappeared since the last
// reconcile tick must not produce a cascade of deletions. On refusal the lease
// is released so the job is retried in seconds instead of after the full lease
// duration.
func (m *Manager) deletionAllowed(ctx context.Context, job *store.UploadJob) bool {
	if err := m.health.Check(ctx); err != nil {
		slog.Warn("refusing to delete while storage health is unproven",
			"operation", "cleanup", "upload_id", job.UploadID, "error", err)
		m.retryCleanup(job, err)
		return false
	}
	return true
}

// removeArtifactsIfUnowned deletes this job's own artifacts only after proving
// no committed media row claims its media id. GetByID reports absence as
// sql.ErrNoRows, and any other error means ownership is unknown — in which
// case nothing may be deleted. Returns false when the caller should stop.
func (m *Manager) removeArtifactsIfUnowned(ctx context.Context, job *store.UploadJob) bool {
	_, err := m.store.GetByID(ctx, job.MediaID, "")
	switch {
	case errors.Is(err, sql.ErrNoRows):
		m.removeArtifacts(job)
		return true
	case err != nil:
		m.retryCleanup(job, fmt.Errorf("ownership check failed: %w", err))
		return false
	default:
		// A committed row owns these artifacts; leave them alone.
		return true
	}
}

func (m *Manager) removeArtifacts(job *store.UploadJob) {
	if job.StoredFilename != "" {
		_ = os.Remove(m.processor.OriginalPath(job.StoredFilename))
	}
	_ = os.Remove(m.processor.ThumbnailPath(job.MediaID))
	_ = os.Remove(m.processor.PreviewPath(job.MediaID))
	_ = media.FsyncDir(m.processor.OriginalsDir())
}

// sweepTemporaries removes this job's leftover copies. A crash between the
// copy and the rename leaves a full-size file in originals/, and because the
// name is lease-scoped a retry writes a new one rather than reusing it. Left
// alone they accumulate at upload size and eventually push the free-space
// floor far enough to refuse new uploads.
func (m *Manager) sweepTemporaries(job *store.UploadJob) {
	matches, err := filepath.Glob(filepath.Join(m.processor.OriginalsDir(), ".ingest-"+job.MediaID+"-*-original.tmp"))
	if err != nil {
		return
	}
	for _, path := range matches {
		_ = os.Remove(path)
	}
	if len(matches) > 0 {
		_ = media.FsyncDir(m.processor.OriginalsDir())
	}
}

// finishBySourceRemoval deletes the tus source and commits the terminal state.
// Removal goes through tusd's own DELETE rather than unlinking its files, so
// tusd's sidecar and lock state stay consistent, and it is issued only for a
// path just observed to exist: an absent file may be a faulted mount rather
// than a deleted one.
func (m *Manager) finishBySourceRemoval(ctx context.Context, job *store.UploadJob, status store.JobStatus, reason string) {
	dataPath := m.DataPath(job.UploadID)
	switch _, err := os.Stat(dataPath); {
	case err == nil:
		if err := m.opts.Terminator.Terminate(ctx, job.UploadID); err != nil {
			m.retryCleanup(job, fmt.Errorf("terminate source: %w", err))
			return
		}
	case !errors.Is(err, os.ErrNotExist):
		m.retryCleanup(job, fmt.Errorf("stat source: %w", err))
		return
	}

	if err := media.FsyncDir(m.opts.UploadDir); err != nil {
		m.retryCleanup(job, err)
		return
	}

	// Only ENOENT proves absence. Treating EIO or EACCES as "gone" would be the
	// same mistake that caused the incident.
	dataGone, err := pathIsAbsent(dataPath)
	if err != nil {
		m.retryCleanup(job, err)
		return
	}
	infoPath := m.InfoPath(job.UploadID)
	sidecarGone, err := pathIsAbsent(infoPath)
	if err != nil {
		m.retryCleanup(job, err)
		return
	}

	// tusd resolves an upload through its sidecar, so if the sidecar is gone it
	// answers 404 and never touches the data file. That is exactly the shape the
	// old ingest path left behind, and it is the shape this feature adopts, so
	// without this branch every recovered upload would spin in cleanup forever
	// and hold disk it no longer needs. Unlinking is safe here for the same
	// reason the orphan sidecar is: nothing reaches this function until the
	// source is already condemned — cleanup only after the publication
	// transaction committed, discard only after a terminal decision was
	// recorded — and tusd cannot address the file either way.
	if !dataGone && sidecarGone {
		slog.Warn("removing tus data file that tusd can no longer address",
			"operation", "cleanup", "upload_id", job.UploadID)
		if err := os.Remove(dataPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.retryCleanup(job, fmt.Errorf("remove orphan data file: %w", err))
			return
		}
		dataGone = true
	}

	// A sidecar can also outlive its data file if tusd crashed between the two
	// unlinks. tusd will not remove it — its own GetUpload fails without the
	// data file — and neither will the janitor, so the app does it here.
	if dataGone && !sidecarGone {
		slog.Warn("removing tus sidecar that outlived its data file",
			"operation", "cleanup", "upload_id", job.UploadID)
		if err := os.Remove(infoPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.retryCleanup(job, fmt.Errorf("remove orphan sidecar: %w", err))
			return
		}
	}

	if !dataGone {
		m.retryCleanup(job, errors.New("source still present after termination"))
		return
	}

	// One final verification of both paths across a directory fsync.
	if err := media.FsyncDir(m.opts.UploadDir); err != nil {
		m.retryCleanup(job, err)
		return
	}
	for _, path := range []string{dataPath, infoPath} {
		absent, err := pathIsAbsent(path)
		if err != nil || !absent {
			m.retryCleanup(job, fmt.Errorf("%s is not verifiably gone", path))
			return
		}
	}

	// Deliberately on the lifetime rather than the attempt context: by here
	// the source is already gone, and abandoning the record of that because
	// the attempt clock ran out would leave the job retrying a deletion it has
	// no way left to observe.
	if err := m.store.FinishJob(m.lifetime, job.UploadID, job.LeaseToken, status, reason, store.NowMicros()); err != nil {
		slog.Error("failed to commit terminal state", "operation", "cleanup", "upload_id", job.UploadID, "error", err)
	}
}

// pathIsAbsent distinguishes "proven gone" from "could not tell". Only ENOENT
// counts as absence; every other error is ambiguity and must be retried.
func pathIsAbsent(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, os.ErrNotExist):
		return true, nil
	default:
		return false, err
	}
}

// retryCleanup keeps the job in its current stage. Demoting it to pending
// would re-run processing against a source cleanup may already have deleted.
func (m *Manager) retryCleanup(job *store.UploadJob, cause error) {
	next := store.NowMicros() + m.backoffFor(job.CleanupFailures).Microseconds()
	if err := m.store.ReleaseForRetry(m.lifetime, job.UploadID, job.LeaseToken, job.Status, next, "cleanup_failures", truncateError(cause)); err != nil {
		m.logRetryScheduleFailure("failed to schedule cleanup retry", "cleanup", job.UploadID, err)
	}
}

// logRetryScheduleFailure reports a retry that could not be written down. A
// cancelled lifetime means the process is shutting down, and every clean stop
// with work in flight produces one of these because the write runs on the very
// context that was cancelled. Logging those at ERROR puts a false page on
// every deploy, and an operator who stops paging on ERROR has no signal left
// for the failures that matter.
func (m *Manager) logRetryScheduleFailure(message, operation, uploadID string, cause error) {
	level := slog.LevelError
	if m.lifetime.Err() != nil {
		level = slog.LevelWarn
	}
	slog.Log(context.Background(), level, message, "operation", operation, "upload_id", uploadID, "error", cause)
}

func truncateError(err error) string {
	const maxLen = 500
	s := err.Error()
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}
