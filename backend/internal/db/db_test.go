package db

import (
	"database/sql"
	"path/filepath"
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
