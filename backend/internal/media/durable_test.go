package media

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestProcessor(t *testing.T) *Processor {
	t.Helper()
	p := NewProcessor(t.TempDir(), 320, []string{"image/jpeg"}, nil)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	return p
}

// swapPrepareSync replaces the copy's sync calls for one test, so the fakes run
// at exactly the points the real calls do and the ordering itself can be
// observed rather than merely the fact that a sync happened.
func swapPrepareSync(t *testing.T, file func(*os.File) error, dir func(string) error) {
	t.Helper()
	origFile, origDir := prepareSyncFile, prepareSyncDir
	prepareSyncFile, prepareSyncDir = file, dir
	t.Cleanup(func() { prepareSyncFile, prepareSyncDir = origFile, origDir })
}

// A crash may only ever leave a stale temporary, never a truncated or
// unreferenced original. That requires write, fsync file, fsync dir, rename,
// fsync dir — in that order — so each step is observed against the filesystem
// state at the moment it runs.
func TestPrepareOriginalSyncsBeforeItPublishes(t *testing.T) {
	p := newTestProcessor(t)
	source := filepath.Join(t.TempDir(), "incoming")
	payload := bytes.Repeat([]byte("z"), 4096)
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	sum, err := SHA256File(source)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	finalPath := p.OriginalPath("media-1.jpg")
	tmpPath := filepath.Join(p.OriginalsDir(), ".ingest-media-1-lease-a-original.tmp")

	var events []string
	swapPrepareSync(t,
		func(f *os.File) error {
			// The temporary must be whole before it is durable, or a crash
			// could leave a short file that the next attempt renames.
			stat, err := os.Stat(tmpPath)
			if err != nil || stat.Size() != int64(len(payload)) {
				t.Errorf("temporary was synced before it was fully written: %v %+v", err, stat)
			}
			events = append(events, "sync-file")
			return f.Sync()
		},
		func(dir string) error {
			if _, err := os.Stat(finalPath); err == nil {
				events = append(events, "sync-dir-after-rename")
			} else {
				events = append(events, "sync-dir-before-rename")
			}
			return FsyncDir(dir)
		})

	err = p.PrepareOriginal(context.Background(), PrepareRequest{
		SourcePath:     source,
		MediaID:        "media-1",
		LeaseToken:     "lease-a",
		StoredFilename: "media-1.jpg",
		Size:           int64(len(payload)),
		SHA256:         sum,
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	want := []string{"sync-file", "sync-dir-before-rename", "sync-dir-after-rename"}
	if len(events) != len(want) {
		t.Fatalf("sync sequence = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("sync sequence = %v, want %v", events, want)
		}
	}
}

func TestPrepareOriginalKeepsSource(t *testing.T) {
	p := newTestProcessor(t)
	source := filepath.Join(t.TempDir(), "incoming")
	payload := []byte("some bytes")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	sum, err := SHA256File(source)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	req := PrepareRequest{
		SourcePath:     source,
		MediaID:        "media-1",
		LeaseToken:     "lease-a",
		StoredFilename: "media-1.jpg",
		Size:           int64(len(payload)),
		SHA256:         sum,
	}

	if err := p.PrepareOriginal(context.Background(), req); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// The whole point: preparation must never consume the only complete copy.
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source must survive preparation: %v", err)
	}
	got, err := os.ReadFile(p.OriginalPath("media-1.jpg"))
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("original = %q, want %q", got, payload)
	}
	// No temporary may be left behind.
	entries, _ := os.ReadDir(p.OriginalsDir())
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temporary %s", e.Name())
		}
	}

	// Re-running under a different lease must reuse the verified original
	// rather than copying again.
	req.LeaseToken = "lease-b"
	if err := p.PrepareOriginal(context.Background(), req); err != nil {
		t.Fatalf("second prepare: %v", err)
	}
}

func TestPrepareOriginalRefusesToOverwriteDifferentContent(t *testing.T) {
	p := newTestProcessor(t)
	source := filepath.Join(t.TempDir(), "incoming")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(p.OriginalPath("media-2.jpg"), []byte("pre-existing"), 0o600); err != nil {
		t.Fatalf("write squatter: %v", err)
	}

	err := p.PrepareOriginal(context.Background(), PrepareRequest{
		SourcePath:     source,
		MediaID:        "media-2",
		LeaseToken:     "lease-a",
		StoredFilename: "media-2.jpg",
		Size:           3,
		SHA256:         "deadbeef",
	})
	if err == nil {
		t.Fatal("an existing original with different content must be an error, never silently overwritten")
	}
	got, _ := os.ReadFile(p.OriginalPath("media-2.jpg"))
	if string(got) != "pre-existing" {
		t.Errorf("existing original was modified: %q", got)
	}
}

func TestVerifyOriginalDetectsMismatch(t *testing.T) {
	p := newTestProcessor(t)
	path := p.OriginalPath("media-3.jpg")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	sum, err := SHA256File(path)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := p.VerifyOriginal("media-3.jpg", 3, sum); err != nil {
		t.Fatalf("verify good: %v", err)
	}
	if err := p.VerifyOriginal("media-3.jpg", 3, "deadbeef"); err == nil {
		t.Error("expected hash mismatch to be reported")
	}
	if err := p.VerifyOriginal("media-3.jpg", 99, sum); err == nil {
		t.Error("expected size mismatch to be reported")
	}
}

func assertNoTemporaries(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read originals dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temporary %s", e.Name())
		}
	}
}

func assertSourceIntact(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("source must survive: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("source = %q, want %q", got, want)
	}
}

// The source may be the guest's only complete copy, so no failure may consume
// it — not just the happy path.
func TestPrepareOriginalKeepsSourceOnErrorPaths(t *testing.T) {
	payload := []byte("some bytes")

	t.Run("cancelled", func(t *testing.T) {
		p := newTestProcessor(t)
		source := filepath.Join(t.TempDir(), "incoming")
		if err := os.WriteFile(source, payload, 0o600); err != nil {
			t.Fatalf("write source: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := p.PrepareOriginal(ctx, PrepareRequest{
			SourcePath:     source,
			MediaID:        "media-4",
			LeaseToken:     "lease-a",
			StoredFilename: "media-4.jpg",
			Size:           int64(len(payload)),
			SHA256:         "irrelevant",
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("prepare err = %v, want context.Canceled", err)
		}
		assertSourceIntact(t, source, payload)
		assertNoTemporaries(t, p.OriginalsDir())
		if _, err := os.Stat(p.OriginalPath("media-4.jpg")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a cancelled attempt must not publish an original: %v", err)
		}
	})

	t.Run("mismatched existing original", func(t *testing.T) {
		p := newTestProcessor(t)
		source := filepath.Join(t.TempDir(), "incoming")
		if err := os.WriteFile(source, payload, 0o600); err != nil {
			t.Fatalf("write source: %v", err)
		}
		if err := os.WriteFile(p.OriginalPath("media-5.jpg"), []byte("pre-existing"), 0o600); err != nil {
			t.Fatalf("write squatter: %v", err)
		}

		err := p.PrepareOriginal(context.Background(), PrepareRequest{
			SourcePath:     source,
			MediaID:        "media-5",
			LeaseToken:     "lease-a",
			StoredFilename: "media-5.jpg",
			Size:           int64(len(payload)),
			SHA256:         "deadbeef",
		})
		if err == nil {
			t.Fatal("expected mismatch error")
		}
		assertSourceIntact(t, source, payload)
	})
}

// Removing the source between attempts proves the second call took the
// verified-reuse branch rather than copying again.
func TestPrepareOriginalReusesVerifiedOriginalWithoutRereadingSource(t *testing.T) {
	p := newTestProcessor(t)
	source := filepath.Join(t.TempDir(), "incoming")
	payload := []byte("some bytes")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	sum, err := SHA256File(source)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	req := PrepareRequest{
		SourcePath:     source,
		MediaID:        "media-6",
		LeaseToken:     "lease-a",
		StoredFilename: "media-6.jpg",
		Size:           int64(len(payload)),
		SHA256:         sum,
	}
	if err := p.PrepareOriginal(context.Background(), req); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	req.LeaseToken = "lease-b"
	if err := p.PrepareOriginal(context.Background(), req); err != nil {
		t.Fatalf("second prepare must reuse the verified original: %v", err)
	}
	got, err := os.ReadFile(p.OriginalPath("media-6.jpg"))
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("original = %q, want %q", got, payload)
	}
}

// A payload spanning several copy buffers catches a copy loop that stops early.
func TestPrepareOriginalCopiesPayloadLargerThanCopyBuffer(t *testing.T) {
	p := newTestProcessor(t)
	source := filepath.Join(t.TempDir(), "incoming")
	payload := make([]byte, 2<<20+3)
	for i := range payload {
		payload[i] = byte(i*31 + 7)
	}
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	sum, err := SHA256File(source)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	if err := p.PrepareOriginal(context.Background(), PrepareRequest{
		SourcePath:     source,
		MediaID:        "media-7",
		LeaseToken:     "lease-a",
		StoredFilename: "media-7.jpg",
		Size:           int64(len(payload)),
		SHA256:         sum,
	}); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// VerifyOriginal is the truncation detector: size and whole-file hash.
	if err := p.VerifyOriginal("media-7.jpg", int64(len(payload)), sum); err != nil {
		t.Fatalf("copied original is not byte-identical: %v", err)
	}
	assertSourceIntact(t, source, payload)
}

func TestSHA256FileContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload")
	payload := make([]byte, 2<<20+3)
	for i := range payload {
		payload[i] = byte(i*17 + 3)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	want, err := SHA256File(path)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	got, err := SHA256FileContext(context.Background(), path)
	if err != nil {
		t.Fatalf("hash with context: %v", err)
	}
	if got != want {
		t.Errorf("SHA256FileContext = %s, want %s", got, want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SHA256FileContext(ctx, path); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled hash err = %v, want context.Canceled", err)
	}
}

func TestFsyncFileAndFsyncDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := FsyncFile(path); err != nil {
		t.Errorf("FsyncFile: %v", err)
	}
	if err := FsyncDir(dir); err != nil {
		t.Errorf("FsyncDir: %v", err)
	}
	if err := FsyncFile(filepath.Join(dir, "absent")); err == nil {
		t.Error("expected an error for a missing file")
	}
	if err := FsyncDir(filepath.Join(dir, "absent")); err == nil {
		t.Error("expected an error for a missing directory")
	}
}
