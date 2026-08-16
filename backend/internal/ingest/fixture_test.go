package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"event-gallery/backend/internal/db"
	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/models"
	"event-gallery/backend/internal/store"
)

// The fixture image is deliberately non-square, and its long edge exceeds the
// fixture processor's thumbnail bound. A square image would let a width/height
// transposition through, and an image smaller than the bound would let a
// derivation that reports the scaled thumbnail's size as the original's
// through, because the two would be equal.
const (
	jpegFixtureWidth  = 400
	jpegFixtureHeight = 200
)

// jpegFixture returns a tiny but genuinely valid JPEG, so the processing path
// runs against content Sniff recognises and the thumbnailer can decode.
func jpegFixture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, jpegFixtureWidth, jpegFixtureHeight)), nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// pngFixture is valid, recognised content that the fixture processor does not
// allow, which is a different rejection branch from unrecognised bytes.
func pngFixture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func newIngestFixture(t *testing.T) (*store.Store, *media.Processor) {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })

	proc := media.NewProcessor(t.TempDir(), 320, 640, []string{"image/jpeg"}, []string{"video/mp4"})
	if err := proc.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	return store.New(sqlDB), proc
}

// writeSidecar writes the sidecar tusd's filestore would have written for a
// well-formed upload of the given size.
func writeSidecar(t *testing.T, m *Manager, uploadID string, size int64) {
	t.Helper()
	writeSidecarMeta(t, m, uploadID, uploadID, size, map[string]string{"filename": "a.jpg"})
}

// writeSidecarMeta writes the full shape tussidecar.Parse demands. Storage.Type
// is load-bearing: without it Parse fails, the sidecar reads as absent, and
// tests meant to prove a sidecar was consulted would instead exercise the
// no-sidecar branch.
func writeSidecarMeta(t *testing.T, m *Manager, fileID, declaredID string, size int64, metadata map[string]string) {
	t.Helper()
	blob, err := json.Marshal(map[string]any{
		"ID":             declaredID,
		"Size":           size,
		"SizeIsDeferred": false,
		"MetaData":       metadata,
		"Storage": map[string]string{
			"Type":     "filestore",
			"Path":     m.DataPath(fileID),
			"InfoPath": m.InfoPath(fileID),
		},
	})
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	if err := os.WriteFile(m.InfoPath(fileID), blob, 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
}

// backdateJob ages a row's activity timestamp so the reconciler's idle windows
// apply to it, instead of the test depending on wall-clock delay.
func backdateJob(t *testing.T, st *store.Store, uploadID string, age time.Duration) {
	t.Helper()
	_, err := st.DB().Exec(`UPDATE upload_jobs SET updated_at = ? WHERE upload_id = ?`,
		store.NowMicros()-age.Microseconds(), uploadID)
	if err != nil {
		t.Fatalf("backdate %s: %v", uploadID, err)
	}
}

func insertMediaRow(t *testing.T, st *store.Store, id, storedFilename, sha string) {
	t.Helper()
	err := st.InsertMedia(context.Background(), &models.MediaItem{
		ID:               id,
		OriginalFilename: "photo.jpg",
		StoredFilename:   storedFilename,
		Kind:             models.KindImage,
		MimeType:         "image/jpeg",
		SizeBytes:        1,
		SHA256:           sha,
		UploadedAt:       time.Now(),
	})
	if err != nil {
		t.Fatalf("insert media: %v", err)
	}
}
