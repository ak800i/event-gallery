package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpen_AppliesMigrations(t *testing.T) {
	dir := t.TempDir()
	sqlDB, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer sqlDB.Close()

	tables := []string{"media_items", "likes", "audit_log", "admin_sessions", "app_config", "schema_migrations"}
	for _, tbl := range tables {
		var name string
		err := sqlDB.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, tbl).Scan(&name)
		if err != nil {
			t.Errorf("expected table %s to exist: %v", tbl, err)
		}
	}
	rows, err := sqlDB.Query(`PRAGMA table_info(media_items)`)
	if err != nil {
		t.Fatalf("inspect media_items: %v", err)
	}
	defer rows.Close()
	foundApprovedAt := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "approved_at" {
			foundApprovedAt = true
		}
	}
	if !foundApprovedAt {
		t.Error("expected approved_at migration")
	}
}

func TestApprovalMigrationGrandfathersExistingMedia(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := migrationsFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES ('0001_init.sql', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO media_items
		(id, original_filename, stored_filename, kind, mime_type, size_bytes, sha256, uploaded_at, status)
		VALUES ('legacy', 'photo.jpg', 'legacy.jpg', 'image', 'image/jpeg', 10, 'legacy-sha', '2026-01-01T00:00:00Z', 'active')`); err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("migrate legacy database: %v", err)
	}
	defer migrated.Close()
	var uploadedAt, approvedAt string
	if err := migrated.QueryRow(`SELECT uploaded_at, approved_at FROM media_items WHERE id = 'legacy'`).Scan(&uploadedAt, &approvedAt); err != nil {
		t.Fatal(err)
	}
	if approvedAt != uploadedAt {
		t.Fatalf("existing media not grandfathered: uploaded=%q approved=%q", uploadedAt, approvedAt)
	}
}

func TestOpen_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open failed: %v", err)
	}
	db1.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	defer db2.Close()

	var count int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if count == 0 {
		t.Errorf("expected at least one migration recorded")
	}
}

func TestOpenAppliesDurabilityPragmas(t *testing.T) {
	sqlDB, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sqlDB.Close()

	var journalMode string
	if err := sqlDB.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	// 2 == FULL. Without it a power loss can lose the very commit that
	// authorized deleting an upload's only source.
	var synchronous int
	if err := sqlDB.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatalf("synchronous: %v", err)
	}
	if synchronous != 2 {
		t.Errorf("synchronous = %d, want 2 (FULL)", synchronous)
	}
}

func TestMigrationCreatesUploadJobs(t *testing.T) {
	sqlDB, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sqlDB.Close()

	var hasPreview int
	err = sqlDB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('media_items') WHERE name = 'has_preview'`).Scan(&hasPreview)
	if err != nil || hasPreview != 1 {
		t.Fatalf("media_items.has_preview missing: count=%d err=%v", hasPreview, err)
	}

	for _, index := range []string{"idx_upload_jobs_due", "idx_upload_jobs_lease", "idx_upload_jobs_terminal"} {
		var n int
		if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s missing: count=%d err=%v", index, n, err)
		}
	}

	// The paired-lease CHECK must reject a half-set lease.
	_, err = sqlDB.Exec(`INSERT INTO upload_jobs
		(upload_id, media_id, status, original_filename, expected_size, lease_token, next_attempt_at, created_at, updated_at)
		VALUES ('u1', 'm1', 'pending', 'a.jpg', 10, 'tok', 0, 0, 0)`)
	if err == nil {
		t.Error("expected CHECK violation for lease_token without lease_until")
	}
}
