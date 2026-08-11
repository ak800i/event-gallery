package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUploadPathsExistSeesEitherDerivedPath(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	m := New(st, proc, opts)

	if m.UploadPathsExist("fresh") {
		t.Fatal("an unused id must be free")
	}

	// tusd's filestore derives exactly these two paths, and either one already
	// existing means a create would reopen someone else's upload.
	for _, path := range []string{m.DataPath("taken"), m.InfoPath("taken")} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if !m.UploadPathsExist("taken") {
			t.Errorf("%s exists but the id was reported free", filepath.Base(path))
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdmitCapacityIsDisabledWithoutAFloor(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	opts.MinFreeBytes = 0
	m := New(st, proc, opts)

	if err := m.AdmitCapacity(context.Background(), 1<<62); err != nil {
		t.Fatalf("no floor configured must admit anything: %v", err)
	}
}

func TestAdmitCapacityRefusesWhenTheFloorWouldBeCrossed(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	// A floor no real filesystem can satisfy, so the refusal is deterministic
	// rather than dependent on the test machine's free space.
	opts.MinFreeBytes = 1 << 62
	m := New(st, proc, opts)

	if err := m.AdmitCapacity(context.Background(), 1); err == nil {
		t.Fatal("a create that would cross the free space floor must be refused")
	}

	opts.MinFreeBytes = 1
	relaxed := New(st, proc, opts)
	if err := relaxed.AdmitCapacity(context.Background(), 1); err != nil {
		t.Fatalf("a one byte floor must admit a one byte upload: %v", err)
	}
}
