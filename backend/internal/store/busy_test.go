package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"event-gallery/backend/internal/db"
)

// The point of IsBusy is to demote a specific, recoverable condition out of
// ERROR. A classifier that silently never matches would leave the noise in
// place and look fine, so this provokes a genuine SQLITE_BUSY rather than
// asserting against a hand-built error value.
func TestIsBusyMatchesARealLockedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")

	held, err := db.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer held.Close()
	if _, err := held.Exec(`CREATE TABLE t (v INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	tx, err := held.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO t VALUES (1)`); err != nil {
		t.Fatalf("write in holder: %v", err)
	}

	// A second connection that refuses to wait at all, so the lock it cannot
	// take surfaces immediately instead of after busy_timeout.
	other, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(0)", path))
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	defer other.Close()

	_, err = other.Exec(`INSERT INTO t VALUES (2)`)
	if err == nil {
		t.Fatal("expected the second writer to be refused while a write txn is open")
	}
	if !IsBusy(err) {
		t.Fatalf("IsBusy(%v) = false, want true", err)
	}
	// Every call site wraps before logging, so seeing through %w is the case
	// that actually matters.
	if !IsBusy(fmt.Errorf("claim upload job: %w", err)) {
		t.Fatalf("IsBusy must see through wrapping, got false for %v", err)
	}
}

func TestIsBusyIgnoresLookalikes(t *testing.T) {
	if IsBusy(nil) {
		t.Error("IsBusy(nil) = true, want false")
	}
	if IsBusy(errors.New("database is locked (5) (SQLITE_BUSY)")) {
		t.Error("IsBusy classified by message text; it must use the result code")
	}
	if IsBusy(sql.ErrNoRows) {
		t.Error("IsBusy(sql.ErrNoRows) = true, want false")
	}
}
