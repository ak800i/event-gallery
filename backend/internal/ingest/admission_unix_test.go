//go:build unix

package ingest

import (
	"context"
	"path/filepath"
	"testing"
)

// freeBytes only has a real implementation on unix; elsewhere it reports
// math.MaxInt64, so no floor can ever be crossed and these two tests would
// assert nothing.

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

// The floor guards two independent volumes. The media directory fills with
// originals and derivatives long after the upload directory has been drained,
// so an admission check that only stats the upload directory lets the media
// volume run dry unannounced.
func TestAdmitCapacityAlsoGuardsTheMediaDirectory(t *testing.T) {
	st, proc := newIngestFixture(t)
	opts := testOptions(t)
	// An upload directory that cannot be stat'ed: that iteration fails open, so
	// only a loop that also reaches the media directory can refuse anything.
	opts.UploadDir = filepath.Join(t.TempDir(), "unmounted")
	opts.MinFreeBytes = 1 << 62
	m := New(st, proc, opts)

	if err := m.AdmitCapacity(context.Background(), 1); err == nil {
		t.Fatal("the media volume's free space must be checked too")
	}

	// The same unstattable upload directory under a floor the media volume does
	// satisfy must admit, which is what pins the refusal above to the media
	// directory rather than to the failed stat.
	opts.MinFreeBytes = 1
	relaxed := New(st, proc, opts)
	if err := relaxed.AdmitCapacity(context.Background(), 1); err != nil {
		t.Fatalf("a filesystem it cannot stat must fail open, not refuse: %v", err)
	}
}
