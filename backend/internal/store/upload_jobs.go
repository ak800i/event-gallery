package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"event-gallery/backend/internal/models"
)

// JobStatus mirrors the CHECK constraint on upload_jobs.status.
type JobStatus string

const (
	JobUploading  JobStatus = "uploading"
	JobPending    JobStatus = "pending"
	JobProcessing JobStatus = "processing"
	JobCleanup    JobStatus = "cleanup"
	JobComplete   JobStatus = "complete"
	JobDiscarding JobStatus = "discarding"
	JobDiscarded  JobStatus = "discarded"
)

// ErrNotClaimed means a conditional update matched zero rows: another worker
// owns the job, the caller's lease token is stale, or the row has moved on.
// Callers must treat it as "you do not own this" and never as "retry harder".
var ErrNotClaimed = errors.New("upload job not claimed")

// UploadJob is one row of the durable ingest queue. All timestamps are signed
// UTC Unix microseconds.
type UploadJob struct {
	UploadID                string
	MediaID                 string
	Status                  JobStatus
	OriginalFilename        string
	StoredFilename          string
	MimeType                string
	ExpectedSize            int64
	DeclaredSHA256          string
	AuthoritativeSHA256     string
	GuestName               string
	UploaderIP              string
	SourceCompletedAt       *int64
	PreparedAt              *int64
	CancellationRequestedAt *int64
	ResultMediaID           string
	TerminalReason          string
	LeaseToken              string
	LeaseUntil              *int64
	NextAttemptAt           int64
	ProcessingFailures      int
	ConversionFailures      int
	CleanupFailures         int
	LastError               string
	CreatedAt               int64
	UpdatedAt               int64
	TerminalAt              *int64
}

// NowMicros is the single clock reading a transaction should take.
func NowMicros() int64 { return time.Now().UTC().UnixMicro() }

// NewLeaseToken returns a fresh random ownership token.
func NewLeaseToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate lease token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

const uploadJobColumns = `upload_id, media_id, status, original_filename, stored_filename, mime_type,
	expected_size, declared_sha256, authoritative_sha256, guest_name, uploader_ip,
	source_completed_at, prepared_at, cancellation_requested_at, result_media_id, terminal_reason,
	lease_token, lease_until, next_attempt_at,
	processing_failures, conversion_failures, cleanup_failures, last_error,
	created_at, updated_at, terminal_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUploadJob(row rowScanner) (*UploadJob, error) {
	var (
		job           UploadJob
		stored        sql.NullString
		mime          sql.NullString
		authoritative sql.NullString
		resultMedia   sql.NullString
		leaseToken    sql.NullString
		sourceDone    sql.NullInt64
		prepared      sql.NullInt64
		cancelled     sql.NullInt64
		leaseUntil    sql.NullInt64
		terminalAt    sql.NullInt64
	)
	err := row.Scan(
		&job.UploadID, &job.MediaID, &job.Status, &job.OriginalFilename, &stored, &mime,
		&job.ExpectedSize, &job.DeclaredSHA256, &authoritative, &job.GuestName, &job.UploaderIP,
		&sourceDone, &prepared, &cancelled, &resultMedia, &job.TerminalReason,
		&leaseToken, &leaseUntil, &job.NextAttemptAt,
		&job.ProcessingFailures, &job.ConversionFailures, &job.CleanupFailures, &job.LastError,
		&job.CreatedAt, &job.UpdatedAt, &terminalAt,
	)
	if err != nil {
		return nil, err
	}
	job.StoredFilename = stored.String
	job.MimeType = mime.String
	job.AuthoritativeSHA256 = authoritative.String
	job.ResultMediaID = resultMedia.String
	job.LeaseToken = leaseToken.String
	job.SourceCompletedAt = nullableMicros(sourceDone)
	job.PreparedAt = nullableMicros(prepared)
	job.CancellationRequestedAt = nullableMicros(cancelled)
	job.LeaseUntil = nullableMicros(leaseUntil)
	job.TerminalAt = nullableMicros(terminalAt)
	return &job, nil
}

func nullableMicros(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	out := v.Int64
	return &out
}

// CreateUploadingJob records an admitted upload before any byte is written.
func (s *Store) CreateUploadingJob(ctx context.Context, j *UploadJob) error {
	now := NowMicros()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO upload_jobs (
			upload_id, media_id, status, original_filename, expected_size,
			declared_sha256, guest_name, uploader_ip,
			next_attempt_at, created_at, updated_at
		) VALUES (?, ?, 'uploading', ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.UploadID, j.MediaID, j.OriginalFilename, j.ExpectedSize,
		j.DeclaredSHA256, j.GuestName, j.UploaderIP, now, now, now,
	)
	if err != nil {
		return fmt.Errorf("create upload job: %w", err)
	}
	j.Status = JobUploading
	j.CreatedAt, j.UpdatedAt, j.NextAttemptAt = now, now, now
	return nil
}

// GetUploadJob returns nil, nil when no such job exists.
func (s *Store) GetUploadJob(ctx context.Context, uploadID string) (*UploadJob, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+uploadJobColumns+` FROM upload_jobs WHERE upload_id = ?`, uploadID)
	job, err := scanUploadJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get upload job: %w", err)
	}
	return job, nil
}

// PromoteToPending is the durability commit: after it returns nil, the upload
// is the application's responsibility and tus may report success. It fails
// closed if cancellation was requested first.
func (s *Store) PromoteToPending(ctx context.Context, uploadID string, now int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = 'pending',
		       source_completed_at = COALESCE(source_completed_at, ?),
		       next_attempt_at = ?,
		       updated_at = ?
		 WHERE upload_id = ?
		   AND status = 'uploading'
		   AND cancellation_requested_at IS NULL`,
		now, now, now, uploadID,
	)
	if err != nil {
		return fmt.Errorf("promote upload job: %w", err)
	}
	return requireOneRow(res)
}

// ClaimNextJob atomically takes ownership of one due job. Receiving the row is
// the definition of ownership: every later write by this worker must present
// the returned lease token.
func (s *Store) ClaimNextJob(ctx context.Context, from, to JobStatus, now int64, leaseFor time.Duration) (*UploadJob, error) {
	token, err := NewLeaseToken()
	if err != nil {
		return nil, err
	}
	until := now + leaseFor.Microseconds()

	row := s.db.QueryRowContext(ctx, `
		UPDATE upload_jobs
		   SET status = ?, lease_token = ?, lease_until = ?, updated_at = ?
		 WHERE upload_id = (
		       SELECT upload_id FROM upload_jobs
		        WHERE status = ?
		          AND next_attempt_at <= ?
		          AND (lease_until IS NULL OR lease_until <= ?)
		        ORDER BY next_attempt_at
		        LIMIT 1
		 )
		RETURNING `+uploadJobColumns,
		to, token, until, now, from, now, now,
	)
	job, err := scanUploadJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim upload job: %w", err)
	}
	return job, nil
}

// ReleaseForRetry hands a job back for another attempt with capped backoff.
// status is the stage to return to: processing failures go back to pending,
// but a cleanup or discard failure must stay in its own stage. Demoting a
// published job to pending would re-run processing against a source that
// cleanup already deleted, and it would never terminalize.
//
// counter must be one of the three failure column names so budgets and logs
// stay unambiguous.
func (s *Store) ReleaseForRetry(ctx context.Context, uploadID, leaseToken string, status JobStatus, nextAttemptAt int64, counter, lastError string) error {
	switch counter {
	case "processing_failures", "conversion_failures", "cleanup_failures":
	default:
		return fmt.Errorf("unknown failure counter %q", counter)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = ?,
		       lease_token = NULL,
		       lease_until = NULL,
		       next_attempt_at = ?,
		       `+counter+` = `+counter+` + 1,
		       last_error = ?,
		       updated_at = ?
		 WHERE upload_id = ? AND lease_token = ?`,
		status, nextAttemptAt, lastError, NowMicros(), uploadID, leaseToken,
	)
	if err != nil {
		return fmt.Errorf("release upload job: %w", err)
	}
	return requireOneRow(res)
}

// ClaimUploadingForDiscard atomically moves a still-uploading row to discard,
// so a caller about to delete its files cannot be overtaken by a completion.
// It returns ErrNotClaimed when the row has already moved on.
func (s *Store) ClaimUploadingForDiscard(ctx context.Context, uploadID string, now int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = 'discarding',
		       terminal_reason = CASE WHEN terminal_reason = '' THEN 'cancelled' ELSE terminal_reason END,
		       next_attempt_at = ?,
		       updated_at = ?
		 WHERE upload_id = ? AND status = 'uploading'`, now, now, uploadID)
	if err != nil {
		return fmt.Errorf("claim uploading for discard: %w", err)
	}
	return requireOneRow(res)
}

// RequestCancellation records durable intent. It never reverses a completion:
// callers must check for pending-or-later first and return 409 instead.
func (s *Store) RequestCancellation(ctx context.Context, uploadID string, now int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET cancellation_requested_at = COALESCE(cancellation_requested_at, ?),
		       updated_at = ?
		 WHERE upload_id = ? AND status = 'uploading'`,
		now, now, uploadID,
	)
	if err != nil {
		return fmt.Errorf("request cancellation: %w", err)
	}
	return requireOneRow(res)
}

// FinishJob commits a terminal or intermediate transition under the caller's lease.
func (s *Store) FinishJob(ctx context.Context, uploadID, leaseToken string, status JobStatus, reason string, now int64) error {
	terminal := status == JobComplete || status == JobDiscarded
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = ?,
		       terminal_reason = CASE WHEN ? = '' THEN terminal_reason ELSE ? END,
		       terminal_at = CASE WHEN ? THEN ? ELSE terminal_at END,
		       lease_token = NULL,
		       lease_until = NULL,
		       updated_at = ?
		 WHERE upload_id = ? AND lease_token = ?`,
		status, reason, reason, terminal, now, now, uploadID, leaseToken,
	)
	if err != nil {
		return fmt.Errorf("finish upload job: %w", err)
	}
	return requireOneRow(res)
}

// RequeueStartup makes interrupted work claimable immediately after a restart
// rather than waiting out wall-clock leases.
func (s *Store) RequeueStartup(ctx context.Context, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = CASE WHEN status = 'processing' THEN 'pending' ELSE status END,
		       lease_token = NULL,
		       lease_until = NULL,
		       next_attempt_at = ?,
		       updated_at = ?
		 WHERE status IN ('processing', 'cleanup', 'discarding')`,
		now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("requeue on startup: %w", err)
	}
	return res.RowsAffected()
}

// DeleteTerminalJobsBefore expires status rows in bounded batches.
func (s *Store) DeleteTerminalJobsBefore(ctx context.Context, cutoff int64, limit int) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM upload_jobs
		 WHERE upload_id IN (
		       SELECT upload_id FROM upload_jobs
		        WHERE status IN ('complete', 'discarded')
		          AND terminal_at IS NOT NULL
		          AND terminal_at < ?
		        LIMIT ?
		 )`, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("expire terminal jobs: %w", err)
	}
	return res.RowsAffected()
}

// SampleStoredFilenames returns up to limit stored filenames, used to prove a
// media volume is actually mounted before anything is deleted. Ordered by
// rowid so the sample is the oldest rows: an original this run just wrote must
// never be the evidence that authorizes deleting its own source.
func (s *Store) SampleStoredFilenames(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT stored_filename FROM media_items ORDER BY rowid LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("sample stored filenames: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan stored filename: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func requireOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotClaimed
	}
	return nil
}

// RecordArtifactIdentity persists the deterministic artifact identity before
// the final original exists, so a crash can be recovered by name and hash
// instead of by re-copying or deleting.
func (s *Store) RecordArtifactIdentity(ctx context.Context, uploadID, leaseToken, storedFilename, mimeType, sha256Hex string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET stored_filename = ?, mime_type = ?, authoritative_sha256 = ?, updated_at = ?
		 WHERE upload_id = ? AND lease_token = ?`,
		storedFilename, mimeType, sha256Hex, NowMicros(), uploadID, leaseToken)
	if err != nil {
		return fmt.Errorf("record artifact identity: %w", err)
	}
	return requireOneRow(res)
}

// RecordPrepared marks that a verified final original now exists. It is
// deliberately separate from RecordArtifactIdentity: prepared_at is a crash
// marker, and it would be worthless if it were set before the copy succeeded.
func (s *Store) RecordPrepared(ctx context.Context, uploadID, leaseToken string) error {
	now := NowMicros()
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs SET prepared_at = ?, updated_at = ?
		 WHERE upload_id = ? AND lease_token = ?`,
		now, now, uploadID, leaseToken)
	if err != nil {
		return fmt.Errorf("record prepared: %w", err)
	}
	return requireOneRow(res)
}

// PublishMedia inserts the media row and moves the job to cleanup in one
// transaction, so a crash can never leave a published file with no job or a
// finished job with no row. It returns the authoritative media id for this
// content, which may belong to an earlier upload of the same bytes.
func (s *Store) PublishMedia(ctx context.Context, uploadID, leaseToken string, item *models.MediaItem, now int64) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("begin publish: %w", err)
	}
	defer tx.Rollback()

	// Fence and take the write lock in one statement. Starting with a read
	// would open a deferred transaction that has to upgrade later, and SQLite
	// answers that upgrade with SQLITE_BUSY_SNAPSHOT, which busy_timeout does
	// not retry.
	fence, err := tx.ExecContext(ctx,
		`UPDATE upload_jobs SET updated_at = ? WHERE upload_id = ? AND lease_token = ? AND status = 'processing'`,
		now, uploadID, leaseToken)
	if err != nil {
		return "", false, fmt.Errorf("fence publish: %w", err)
	}
	if owned, err := fence.RowsAffected(); err != nil {
		return "", false, fmt.Errorf("rows affected: %w", err)
	} else if owned == 0 {
		return "", false, ErrNotClaimed
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO media_items (
			id, original_filename, stored_filename, kind, mime_type, size_bytes, sha256,
			width, height, duration_seconds, has_thumbnail, captured_at, uploaded_at, approved_at,
			uploader_name, uploader_ip, status
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			CASE WHEN COALESCE((SELECT value FROM app_config WHERE key = ?), 'false') = 'false' THEN ? ELSE NULL END,
			?, ?, ?)
		ON CONFLICT(sha256) DO NOTHING`,
		item.ID, item.OriginalFilename, item.StoredFilename, string(item.Kind), item.MimeType,
		item.SizeBytes, item.SHA256, nullableInt(item.Width), nullableInt(item.Height),
		nullableFloat(item.DurationSeconds), boolToInt(item.HasThumbnail),
		formatTimePtr(item.CapturedAt), formatTime(item.UploadedAt), ConfigKeyApprovalRequired,
		formatTime(item.UploadedAt), item.UploaderName, item.UploaderIP, string(models.StatusActive),
	)
	if err != nil {
		return "", false, fmt.Errorf("insert media: %w", err)
	}

	// Whoever won the unique hash constraint owns this content.
	var authoritativeID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM media_items WHERE sha256 = ?`, item.SHA256).Scan(&authoritativeID); err != nil {
		return "", false, fmt.Errorf("resolve authoritative media: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = 'cleanup', result_media_id = ?, lease_token = NULL, lease_until = NULL, updated_at = ?
		 WHERE upload_id = ? AND lease_token = ?`,
		authoritativeID, now, uploadID, leaseToken)
	if err != nil {
		return "", false, fmt.Errorf("transition to cleanup: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("commit publish: %w", err)
	}
	return authoritativeID, authoritativeID != item.ID, nil
}

// MarkUnobservable closes out an admitted upload whose bytes were never
// observable — a create whose client vanished, or a partial the retention
// janitor removed. It goes straight to a terminal state because there is
// nothing to delete, and it carries a reason distinct from cancellation
// precisely so it stays reversible: if the paths were merely hidden by a
// faulted mount, reconciliation re-adopts them when they return.
func (s *Store) MarkUnobservable(ctx context.Context, uploadID string, now int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = 'discarded',
		       terminal_reason = 'unobservable',
		       terminal_at = ?,
		       lease_token = NULL,
		       lease_until = NULL,
		       updated_at = ?
		 WHERE upload_id = ?
		   AND status = 'uploading'
		   AND source_completed_at IS NULL`, now, now, uploadID)
	if err != nil {
		return fmt.Errorf("mark upload unobservable: %w", err)
	}
	return requireOneRow(res)
}

// DeleteUploadJob removes a terminal row so its upload id can be adopted
// afresh. Only used when bytes reappear for an 'unobservable' closure.
func (s *Store) DeleteUploadJob(ctx context.Context, uploadID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM upload_jobs WHERE upload_id = ? AND status IN ('complete', 'discarded')`, uploadID)
	if err != nil {
		return fmt.Errorf("delete terminal upload job: %w", err)
	}
	return nil
}

// ReopenTerminal returns a finished job to a working stage because files it
// had verified as gone have reappeared. The ordinary worker path then completes
// the removal it could not perform while the volume was unavailable.
func (s *Store) ReopenTerminal(ctx context.Context, uploadID string, status JobStatus, now int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE upload_jobs
		   SET status = ?,
		       terminal_at = NULL,
		       next_attempt_at = ?,
		       lease_token = NULL,
		       lease_until = NULL,
		       updated_at = ?
		 WHERE upload_id = ? AND status IN ('complete', 'discarded')`,
		status, now, now, uploadID)
	if err != nil {
		return fmt.Errorf("reopen terminal upload job: %w", err)
	}
	return requireOneRow(res)
}
