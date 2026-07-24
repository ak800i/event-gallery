package media

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"event-gallery/backend/internal/models"
)

func writeJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 100, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write jpeg: %v", err)
	}
}

func writePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}
}

func writeGIF(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, w, h), []color.Color{color.White, color.Black})
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write gif: %v", err)
	}
}

func TestSniff_KnownFormats(t *testing.T) {
	dir := t.TempDir()

	jpegPath := filepath.Join(dir, "photo.jpg")
	writeJPEG(t, jpegPath, 20, 10)
	if mt, kind, err := Sniff(jpegPath); err != nil || mt != "image/jpeg" || kind != models.KindImage {
		t.Errorf("jpeg sniff: mt=%s kind=%s err=%v", mt, kind, err)
	}

	pngPath := filepath.Join(dir, "photo.png")
	writePNG(t, pngPath, 20, 10)
	if mt, kind, err := Sniff(pngPath); err != nil || mt != "image/png" || kind != models.KindImage {
		t.Errorf("png sniff: mt=%s kind=%s err=%v", mt, kind, err)
	}

	gifPath := filepath.Join(dir, "photo.gif")
	writeGIF(t, gifPath, 20, 10)
	if mt, kind, err := Sniff(gifPath); err != nil || mt != "image/gif" || kind != models.KindImage {
		t.Errorf("gif sniff: mt=%s kind=%s err=%v", mt, kind, err)
	}
}

func TestSniff_RejectsUnknownContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-media.txt")
	if err := os.WriteFile(path, []byte("just some plain text content here"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, _, err := Sniff(path); err == nil {
		t.Fatal("expected error sniffing plain text")
	}
}

func TestSniff_IgnoresDeceptiveExtension(t *testing.T) {
	// A file named .jpg but containing plain text must NOT be sniffed as an
	// image: we never trust filenames or client-declared types.
	dir := t.TempDir()
	path := filepath.Join(dir, "fake.jpg")
	if err := os.WriteFile(path, []byte("this is not really a jpeg"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, _, err := Sniff(path); err == nil {
		t.Fatal("expected sniff to reject content that only has a jpeg extension")
	}
}

func TestIsAllowed(t *testing.T) {
	images := []string{"image/jpeg", "image/png"}
	videos := []string{"video/mp4"}
	if !IsAllowed("image/jpeg", models.KindImage, images, videos) {
		t.Error("expected image/jpeg to be allowed")
	}
	if IsAllowed("image/heic", models.KindImage, images, videos) {
		t.Error("expected image/heic to be disallowed given restricted list")
	}
	if !IsAllowed("video/mp4", models.KindVideo, images, videos) {
		t.Error("expected video/mp4 to be allowed")
	}
}

func TestSHA256File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	content := []byte("hello event gallery")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	sum, err := SHA256File(path)
	if err != nil {
		t.Fatalf("hash file: %v", err)
	}

	sum2, err := SHA256File(path)
	if err != nil {
		t.Fatalf("hash file again: %v", err)
	}
	if sum != sum2 {
		t.Errorf("expected deterministic hash, got %s vs %s", sum, sum2)
	}
	if len(sum) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(sum))
	}
}

func TestImageDimensions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "photo.png")
	writePNG(t, path, 37, 21)
	w, h, err := ImageDimensions(path)
	if err != nil {
		t.Fatalf("image dimensions: %v", err)
	}
	if w != 37 || h != 21 {
		t.Errorf("expected 37x21, got %dx%d", w, h)
	}
}

func TestGenerateImageThumbnail(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "big.jpg")
	writeJPEG(t, src, 400, 200)
	dst := filepath.Join(dir, "thumb.jpg")

	w, h, err := GenerateImageThumbnail(src, dst, 100)
	if err != nil {
		t.Fatalf("generate thumbnail: %v", err)
	}
	if w != 400 || h != 200 {
		t.Errorf("expected original dimensions 400x200, got %dx%d", w, h)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("expected thumbnail file to exist: %v", err)
	}
	thumbW, thumbH, err := ImageDimensions(dst)
	if err != nil {
		t.Fatalf("thumb dimensions: %v", err)
	}
	if thumbW > 100 || thumbH > 100 {
		t.Errorf("expected thumbnail to fit within 100px, got %dx%d", thumbW, thumbH)
	}
}

func TestGenerateImageThumbnail_SmallImageUnscaled(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "small.jpg")
	writeJPEG(t, src, 50, 30)
	dst := filepath.Join(dir, "thumb.jpg")

	_, _, err := GenerateImageThumbnail(src, dst, 100)
	if err != nil {
		t.Fatalf("generate thumbnail: %v", err)
	}
	thumbW, thumbH, err := ImageDimensions(dst)
	if err != nil {
		t.Fatalf("thumb dimensions: %v", err)
	}
	if thumbW != 50 || thumbH != 30 {
		t.Errorf("expected small image to remain unscaled at 50x30, got %dx%d", thumbW, thumbH)
	}
}
