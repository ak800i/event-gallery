package media

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"event-gallery/backend/internal/models"
)

// Processor turns a completed upload's temporary file into a permanent,
// vetted media item: it validates the true content type, computes the
// whole-file hash, extracts dimensions/duration/capture time, and writes
// the original plus a thumbnail into MediaDir.
type Processor struct {
	MediaDir              string
	ThumbnailMaxDimension int
	PreviewMaxDimension   int
	AllowedImageMIMEs     []string
	AllowedVideoMIMEs     []string

	// DecodeMaxPixels caps the pixel count a still may have before the
	// in-process decoder is bypassed for ffmpeg. Zero disables the cap.
	DecodeMaxPixels int64
}

// NewProcessor constructs a Processor. mediaDir is the root of the host's
// persistent media bind mount.
func NewProcessor(mediaDir string, thumbnailMaxDimension, previewMaxDimension int, allowedImages, allowedVideos []string, decodeMaxPixels int64) *Processor {
	return &Processor{
		MediaDir:              mediaDir,
		ThumbnailMaxDimension: thumbnailMaxDimension,
		PreviewMaxDimension:   previewMaxDimension,
		AllowedImageMIMEs:     allowedImages,
		AllowedVideoMIMEs:     allowedVideos,
		DecodeMaxPixels:       decodeMaxPixels,
	}
}

// OriginalsDir is where permanent original files live.
func (p *Processor) OriginalsDir() string { return filepath.Join(p.MediaDir, "originals") }

// ThumbnailsDir is where generated thumbnails live.
func (p *Processor) ThumbnailsDir() string { return filepath.Join(p.MediaDir, "thumbnails") }

// PreviewsDir is where generated browser-viewable previews live.
func (p *Processor) PreviewsDir() string { return filepath.Join(p.MediaDir, "previews") }

// EnsureDirs creates the originals/thumbnails/previews directories if missing.
func (p *Processor) EnsureDirs() error {
	for _, dir := range []string{p.OriginalsDir(), p.ThumbnailsDir(), p.PreviewsDir()} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create media dir %s: %w", dir, err)
		}
	}
	return nil
}

// ThumbnailPath returns the on-disk path of a media item's thumbnail.
func (p *Processor) ThumbnailPath(id string) string {
	return filepath.Join(p.ThumbnailsDir(), id+".jpg")
}

// PreviewPath returns the on-disk path of a media item's generated preview.
func (p *Processor) PreviewPath(id string) string {
	return filepath.Join(p.PreviewsDir(), id+".jpg")
}

// OriginalPath returns the on-disk path of a media item's stored original
// file given its stored filename (id + original extension).
func (p *Processor) OriginalPath(storedFilename string) string {
	return filepath.Join(p.OriginalsDir(), storedFilename)
}

var mimeToExt = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"image/heic":      ".heic",
	"image/heif":      ".heif",
	"image/avif":      ".avif",
	"video/mp4":       ".mp4",
	"video/quicktime": ".mov",
	"video/webm":      ".webm",
}

// Result is everything derived from processing one upload, ready to be
// persisted as a models.MediaItem.
type Result struct {
	ID              string
	StoredFilename  string
	Kind            models.MediaKind
	MimeType        string
	SizeBytes       int64
	SHA256          string
	Width           int
	Height          int
	DurationSeconds float64
	HasThumbnail    bool
	HasPreview      bool
	CapturedAt      *time.Time
}

// previewMIMEs lists the stored image types that browsers other than Safari
// cannot decode at all, so the gallery has to serve a derived JPEG instead.
// AVIF is deliberately absent: Chrome, Firefox and Safari 16+ render it
// natively, so a preview would only waste disk.
var previewMIMEs = map[string]bool{"image/heic": true, "image/heif": true}

// NeedsPreview reports whether a stored MIME type requires a derived
// browser-viewable JPEG alongside the untouched original.
func NeedsPreview(mimeType string) bool { return previewMIMEs[mimeType] }

// ExtensionForMIME picks the stored file's extension from sniffed content,
// falling back to the client's filename only for unmapped types.
func ExtensionForMIME(mimeType, originalFilename string) string {
	if ext := mimeToExt[mimeType]; ext != "" {
		return ext
	}
	return filepath.Ext(originalFilename)
}

// Derive generates thumbnails and metadata for an already-stored original.
// Every failure here is best effort: the item publishes regardless.
func (p *Processor) Derive(ctx context.Context, finalPath, id string, kind models.MediaKind, mimeType string) *Result {
	result := &Result{ID: id}
	switch kind {
	case models.KindImage:
		p.processImage(ctx, finalPath, id, result)
		// A missing thumbnail means ffmpeg could not decode this file at all,
		// so a second attempt at a larger size would only burn another timeout.
		if result.HasThumbnail && NeedsPreview(mimeType) {
			// Previews only exist for HEIC/HEIF, whose HEVC decoder has no
			// lowres support, so there is nothing to ask for here.
			if err := GenerateImageThumbnailFFmpeg(ctx, finalPath, p.PreviewPath(id), p.PreviewMaxDimension, 0); err == nil {
				result.HasPreview = true
			}
		}
	case models.KindVideo:
		p.processVideo(ctx, finalPath, id, result)
	}
	return result
}

// Swapped in tests to prove an oversized still never reaches the in-process
// decoder; both paths otherwise produce an indistinguishable thumbnail.
var generateImageThumbnail = GenerateImageThumbnail

func (p *Processor) processImage(ctx context.Context, finalPath, id string, result *Result) {
	thumbPath := p.ThumbnailPath(id)

	// Fast path: pure-Go decode (jpeg/png/gif/webp), skipped for stills large
	// enough that decoding in-process would dominate the memory budget.
	if shouldDecodeInProcess(finalPath, p.DecodeMaxPixels) {
		if width, height, err := generateImageThumbnail(finalPath, thumbPath, p.ThumbnailMaxDimension); err == nil {
			result.Width, result.Height = width, height
			result.HasThumbnail = true
			if capturedAt, err := ImageCapturedAt(finalPath); err == nil && capturedAt != nil {
				result.CapturedAt = capturedAt
			}
			return
		}
	}

	// Fallback: formats the pure-Go pipeline can't decode (AVIF/HEIC/HEIF, or
	// otherwise-undecodable images), plus anything the guard above diverted,
	// go through ffmpeg/ffprobe. Non-fatal: on any failure the item is still
	// ingested, just without a thumbnail.
	probe, probeErr := ProbeImage(ctx, finalPath)

	lowres := 0
	if probeErr == nil {
		lowres = lowresLevel(probe.Codec, probe.CodedWidth, probe.CodedHeight, p.ThumbnailMaxDimension)
	}

	if err := GenerateImageThumbnailFFmpeg(ctx, finalPath, thumbPath, p.ThumbnailMaxDimension, lowres); err == nil {
		result.HasThumbnail = true
		// Original display dimensions come only from the probe. If the probe
		// failed, leave Width/Height unset rather than persisting the scaled
		// thumbnail size as if it were the original.
		if probeErr == nil {
			if thumbW, thumbH, dimErr := ImageDimensions(thumbPath); dimErr == nil {
				result.Width, result.Height = orientImageDimensions(probe.CodedWidth, probe.CodedHeight, thumbW, thumbH)
			} else {
				result.Width, result.Height = probe.CodedWidth, probe.CodedHeight
			}
		}
	} else if probeErr == nil {
		// No thumbnail available; keep best-effort coded dimensions.
		result.Width, result.Height = probe.CodedWidth, probe.CodedHeight
	}

	// Capture time: prefer goexif (works for JPEG/TIFF EXIF), fall back to the
	// probe's container creation_time for ISOBMFF formats goexif can't parse.
	if result.CapturedAt == nil {
		if capturedAt, err := ImageCapturedAt(finalPath); err == nil && capturedAt != nil {
			result.CapturedAt = capturedAt
		} else if probeErr == nil && probe.CapturedAt != nil {
			result.CapturedAt = probe.CapturedAt
		}
	}
}

func (p *Processor) processVideo(ctx context.Context, finalPath, id string, result *Result) {
	info, err := ProbeVideo(ctx, finalPath)
	if err == nil {
		result.DurationSeconds = info.DurationSeconds
		result.Width, result.Height = info.Width, info.Height
		result.CapturedAt = info.CapturedAt
	}

	duration := result.DurationSeconds
	if err := GenerateVideoThumbnail(ctx, finalPath, p.ThumbnailPath(id), p.ThumbnailMaxDimension, duration); err == nil {
		result.HasThumbnail = true
	}
}
