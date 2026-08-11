package ingest

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"path/filepath"
	"testing"
	"time"

	"event-gallery/backend/internal/db"
	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/models"
	"event-gallery/backend/internal/store"
)

// jpegFixture returns a tiny but genuinely valid JPEG, so the processing path
// runs against content Sniff recognises and the thumbnailer can decode.
func jpegFixture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
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

	proc := media.NewProcessor(t.TempDir(), 320, []string{"image/jpeg"}, []string{"video/mp4"})
	if err := proc.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	return store.New(sqlDB), proc
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
