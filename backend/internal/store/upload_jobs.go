package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
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
