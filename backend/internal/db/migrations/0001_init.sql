-- Initial schema for the event gallery application.

CREATE TABLE IF NOT EXISTS media_items (
    id TEXT PRIMARY KEY,
    original_filename TEXT NOT NULL,
    stored_filename TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('image', 'video')),
    mime_type TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    sha256 TEXT NOT NULL UNIQUE,
    width INTEGER,
    height INTEGER,
    duration_seconds REAL,
    has_thumbnail INTEGER NOT NULL DEFAULT 0,
    captured_at TEXT,
    uploaded_at TEXT NOT NULL,
    uploader_name TEXT NOT NULL DEFAULT '',
    uploader_ip TEXT,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'trashed')),
    deleted_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_media_items_status_uploaded_at ON media_items (status, uploaded_at, id);
CREATE INDEX IF NOT EXISTS idx_media_items_status_captured_at ON media_items (status, captured_at, id);
CREATE INDEX IF NOT EXISTS idx_media_items_sha256 ON media_items (sha256);

CREATE TABLE IF NOT EXISTS likes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    media_id TEXT NOT NULL REFERENCES media_items (id) ON DELETE CASCADE,
    device_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (media_id, device_id)
);

CREATE INDEX IF NOT EXISTS idx_likes_media_id ON likes (media_id);

CREATE TABLE IF NOT EXISTS audit_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    action TEXT NOT NULL,
    actor TEXT NOT NULL,
    media_id TEXT,
    filename TEXT,
    details TEXT,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_log_created_at ON audit_log (created_at, id);

CREATE TABLE IF NOT EXISTS admin_sessions (
    id TEXT PRIMARY KEY,
    csrf_token TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_admin_sessions_expires_at ON admin_sessions (expires_at);

CREATE TABLE IF NOT EXISTS app_config (
    key TEXT PRIMARY KEY,
    value TEXT
);
