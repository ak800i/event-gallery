package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"wedding-gallery/backend/internal/models"
)

func TestProcessor_ProcessImage(t *testing.T) {
	dir := t.TempDir()
	proc := NewProcessor(filepath.Join(dir, "media"), 100, []string{"image/jpeg"}, []string{"video/mp4"})

	tempPath := filepath.Join(dir, "incoming.tmp")
	writeJPEG(t, tempPath, 300, 150)

	result, err := proc.Process(context.Background(), tempPath, "family-photo.jpg")
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if result.Kind != models.KindImage {
		t.Errorf("expected image kind, got %s", result.Kind)
	}
	if result.MimeType != "image/jpeg" {
		t.Errorf("expected image/jpeg, got %s", result.MimeType)
	}
	if result.Width != 300 || result.Height != 150 {
		t.Errorf("expected 300x150, got %dx%d", result.Width, result.Height)
	}
	if !result.HasThumbnail {
		t.Error("expected thumbnail to be generated")
	}
	if result.SHA256 == "" {
		t.Error("expected non-empty sha256")
	}

	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Error("expected temp file to be moved away")
	}
	if _, err := os.Stat(proc.OriginalPath(result.StoredFilename)); err != nil {
		t.Errorf("expected original file at final path: %v", err)
	}
	if _, err := os.Stat(proc.ThumbnailPath(result.ID)); err != nil {
		t.Errorf("expected thumbnail file: %v", err)
	}
}

func TestProcessor_RejectsDisallowedType(t *testing.T) {
	dir := t.TempDir()
	proc := NewProcessor(filepath.Join(dir, "media"), 100, []string{"image/png"}, nil)

	tempPath := filepath.Join(dir, "incoming.tmp")
	writeJPEG(t, tempPath, 100, 100)

	_, err := proc.Process(context.Background(), tempPath, "photo.jpg")
	if err == nil {
		t.Fatal("expected error for disallowed jpeg when only png is allowed")
	}
	if _, statErr := os.Stat(tempPath); statErr != nil {
		t.Error("expected temp file to remain untouched after rejection")
	}
}

func TestProcessor_RejectsUnknownContent(t *testing.T) {
	dir := t.TempDir()
	proc := NewProcessor(filepath.Join(dir, "media"), 100, []string{"image/jpeg"}, []string{"video/mp4"})

	tempPath := filepath.Join(dir, "incoming.tmp")
	if err := os.WriteFile(tempPath, []byte("not a real media file"), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	_, err := proc.Process(context.Background(), tempPath, "notreal.jpg")
	if err == nil {
		t.Fatal("expected error for unrecognized content")
	}
}

func TestMoveFile_CrossDeviceFallback(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "sub", "dst.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := []byte("cross device move test content")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := moveFile(src, dst); err != nil {
		t.Fatalf("moveFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content mismatch after move")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("expected src removed after move")
	}
}
