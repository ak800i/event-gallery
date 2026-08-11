package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestHealthGateStartsClosedBeforeAnyCheck(t *testing.T) {
	st, proc := newIngestFixture(t)

	// Deletions consult Healthy() directly. A gate that reported true before any
	// check had run would let the first deletion through on an unmounted volume.
	gate := NewHealthGate(st, proc, 5)
	if gate.Healthy() {
		t.Error("a freshly constructed gate must report unhealthy until a check proves the volume is mounted")
	}
}

func TestHealthGateEmptyDatabaseIsHealthy(t *testing.T) {
	st, proc := newIngestFixture(t)
	gate := NewHealthGate(st, proc, 5)
	if err := gate.Check(context.Background()); err != nil {
		t.Fatalf("empty database must be trivially healthy: %v", err)
	}
}

func TestHealthGateOpensCircuitWhenAllSampledOriginalsMissing(t *testing.T) {
	st, proc := newIngestFixture(t)
	insertMediaRow(t, st, "m1", "m1.jpg", "hash-1")

	gate := NewHealthGate(st, proc, 5)
	if err := gate.Check(context.Background()); err == nil {
		t.Fatal("a non-empty database with no originals on disk must open the circuit")
	}
	if gate.Healthy() {
		t.Error("Healthy() must report false while the circuit is open")
	}

	// The volume returns: the circuit must close by itself.
	if err := os.WriteFile(filepath.Join(proc.OriginalsDir(), "m1.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write original: %v", err)
	}
	if err := gate.Check(context.Background()); err != nil {
		t.Fatalf("circuit must close once the volume returns: %v", err)
	}
	if !gate.Healthy() {
		t.Error("Healthy() must report true after recovery")
	}
}

func TestHealthGateOneSurvivingOriginalIsEnough(t *testing.T) {
	st, proc := newIngestFixture(t)
	insertMediaRow(t, st, "m1", "m1.jpg", "hash-1")
	insertMediaRow(t, st, "m2", "m2.jpg", "hash-2")
	if err := os.WriteFile(filepath.Join(proc.OriginalsDir(), "m2.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write original: %v", err)
	}

	gate := NewHealthGate(st, proc, 5)
	if err := gate.Check(context.Background()); err != nil {
		t.Fatalf("a mounted volume with one deleted file is healthy, not faulted: %v", err)
	}
}

func TestHealthGateNonPositiveSampleSizeStillDemandsEvidence(t *testing.T) {
	st, proc := newIngestFixture(t)
	insertMediaRow(t, st, "m1", "m1.jpg", "hash-1")

	// A sample size of zero would query LIMIT 0, read back no rows, and mistake
	// that for an empty gallery: a permanently open gate on an unmounted volume.
	gate := NewHealthGate(st, proc, 0)
	if err := gate.Check(context.Background()); err == nil {
		t.Fatal("a non-positive sample size must fall back to a real sample, not certify the volume")
	}
}
