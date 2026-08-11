-- Durable upload queue. A tus upload reported successful to the browser must
-- already have a committed row here, so that no server-side deadline can
-- destroy the guest's only copy.

ALTER TABLE media_items ADD COLUMN has_preview INTEGER NOT NULL DEFAULT 0;

CREATE TABLE upload_jobs (
    upload_id TEXT PRIMARY KEY,
    media_id TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL CHECK (
        status IN (
            'uploading', 'pending', 'processing', 'cleanup',
            'complete', 'discarding', 'discarded'
        )
    ),
    original_filename TEXT NOT NULL,
    stored_filename TEXT,
    mime_type TEXT,
    expected_size INTEGER NOT NULL CHECK (expected_size > 0),
    declared_sha256 TEXT NOT NULL DEFAULT '',
    authoritative_sha256 TEXT,
    guest_name TEXT NOT NULL DEFAULT '',
    uploader_ip TEXT NOT NULL DEFAULT '',
    source_completed_at INTEGER,
    prepared_at INTEGER,
    cancellation_requested_at INTEGER,
    result_media_id TEXT,
    terminal_reason TEXT NOT NULL DEFAULT '',
    lease_token TEXT,
    lease_until INTEGER,
    next_attempt_at INTEGER NOT NULL,
    processing_failures INTEGER NOT NULL DEFAULT 0,
    conversion_failures INTEGER NOT NULL DEFAULT 0,
    cleanup_failures INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    terminal_at INTEGER,
    CHECK ((lease_token IS NULL) = (lease_until IS NULL))
);

CREATE INDEX idx_upload_jobs_due
  ON upload_jobs(status, next_attempt_at);

CREATE INDEX idx_upload_jobs_lease
  ON upload_jobs(status, lease_until);

CREATE INDEX idx_upload_jobs_terminal
  ON upload_jobs(terminal_at)
  WHERE status IN ('complete', 'discarded');
