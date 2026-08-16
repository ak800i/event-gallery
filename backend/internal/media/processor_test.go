package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"event-gallery/backend/internal/models"
)

func TestMimeToExt_AVIF(t *testing.T) {
	if got := mimeToExt["image/avif"]; got != ".avif" {
		t.Errorf("expected .avif for image/avif, got %q", got)
	}
}

// A preview only earns its disk space for originals browsers cannot decode.
func TestNeedsPreview(t *testing.T) {
	for _, mime := range []string{"image/heic", "image/heif"} {
		if !NeedsPreview(mime) {
			t.Errorf("expected %s to need a preview", mime)
		}
	}
	for _, mime := range []string{"image/jpeg", "image/png", "image/webp", "image/gif", "image/avif", "video/quicktime"} {
		if NeedsPreview(mime) {
			t.Errorf("expected %s to be displayable without a preview", mime)
		}
	}
}

// ffmpeg has no HEIC muxer, so no real HEIC fixture can be produced here. The
// branch under test is the MIME gate and the derived write, not the decoding —
// which is why an AVIF body declared as image/heic is the right stand-in.
func TestDeriveWritesPreviewOnlyForUndisplayableTypes(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	p := NewProcessor(dir, 100, 300, nil, nil)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "source.avif")
	generateTestAVIF(t, src, 640, 480)

	needsPreview := p.Derive(context.Background(), src, "needs-preview", models.KindImage, "image/heic")
	if !needsPreview.HasThumbnail || !needsPreview.HasPreview {
		t.Fatalf("expected thumbnail and preview, got thumbnail=%v preview=%v", needsPreview.HasThumbnail, needsPreview.HasPreview)
	}
	if _, err := os.Stat(p.PreviewPath("needs-preview")); err != nil {
		t.Fatalf("preview file missing: %v", err)
	}
	previewW, previewH, err := ImageDimensions(p.PreviewPath("needs-preview"))
	if err != nil {
		t.Fatalf("preview dims: %v", err)
	}
	thumbW, thumbH, err := ImageDimensions(p.ThumbnailPath("needs-preview"))
	if err != nil {
		t.Fatalf("thumbnail dims: %v", err)
	}
	if previewW <= thumbW || previewH <= thumbH {
		t.Errorf("preview %dx%d should be larger than thumbnail %dx%d", previewW, previewH, thumbW, thumbH)
	}

	displayable := p.Derive(context.Background(), src, "displayable", models.KindImage, "image/avif")
	if displayable.HasPreview {
		t.Error("AVIF renders natively; it must not get a preview")
	}
	if _, err := os.Stat(p.PreviewPath("displayable")); !os.IsNotExist(err) {
		t.Errorf("expected no preview file for a displayable original, got %v", err)
	}
}

// The stored name comes from sniffed content, not from whatever the client
// called the file. Only an unmapped type may fall back to the client's
// extension.
func TestExtensionForMIMEPrefersSniffedContent(t *testing.T) {
	if got := ExtensionForMIME("image/jpeg", "photo.png"); got != ".jpg" {
		t.Errorf("ExtensionForMIME(image/jpeg) = %q, want .jpg", got)
	}
	if got := ExtensionForMIME("application/x-unmapped", "clip.mkv"); got != ".mkv" {
		t.Errorf("unmapped type must fall back to the filename, got %q", got)
	}
	if got := ExtensionForMIME("application/x-unmapped", "noextension"); got != "" {
		t.Errorf("no extension anywhere must stay empty, got %q", got)
	}
}
