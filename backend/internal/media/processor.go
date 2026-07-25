package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"event-gallery/backend/internal/models"
)

// Processor turns a completed upload's temporary file into a permanent,
// vetted media item: it validates the true content type, computes the
// whole-file hash, extracts dimensions/duration/capture time, and writes
// the original plus a thumbnail into MediaDir.
type Processor struct {
	MediaDir              string
	ThumbnailMaxDimension int
	AllowedImageMIMEs     []string
	AllowedVideoMIMEs     []string
}

// NewProcessor constructs a Processor. mediaDir is the root of the host's
// persistent media bind mount.
func NewProcessor(mediaDir string, thumbnailMaxDimension int, allowedImages, allowedVideos []string) *Processor {
	return &Processor{
		MediaDir:              mediaDir,
		ThumbnailMaxDimension: thumbnailMaxDimension,
		AllowedImageMIMEs:     allowedImages,
		AllowedVideoMIMEs:     allowedVideos,
	}
}

// OriginalsDir is where permanent original files live.
func (p *Processor) OriginalsDir() string { return filepath.Join(p.MediaDir, "originals") }

// ThumbnailsDir is where generated thumbnails live.
func (p *Processor) ThumbnailsDir() string { return filepath.Join(p.MediaDir, "thumbnails") }

// EnsureDirs creates the originals/thumbnails directories if missing.
func (p *Processor) EnsureDirs() error {
	for _, dir := range []string{p.OriginalsDir(), p.ThumbnailsDir()} {
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
	CapturedAt      *time.Time
}

// Process validates and ingests a completed upload located at tempPath
// (typically tusd's temporary storage). On success the original file has
// been moved into permanent storage and tempPath no longer exists. On
// failure tempPath is left untouched so the caller can decide how to clean
// up.
func (p *Processor) Process(ctx context.Context, tempPath, originalFilename string) (*Result, error) {
	mimeType, kind, err := Sniff(tempPath)
	if err != nil {
		return nil, err
	}
	if !IsAllowed(mimeType, kind, p.AllowedImageMIMEs, p.AllowedVideoMIMEs) {
		return nil, &ErrUnsupportedType{Sniffed: mimeType}
	}

	stat, err := os.Stat(tempPath)
	if err != nil {
		return nil, fmt.Errorf("stat temp file: %w", err)
	}

	sha256Hex, err := SHA256File(tempPath)
	if err != nil {
		return nil, err
	}

	if err := p.EnsureDirs(); err != nil {
		return nil, err
	}

	id := uuid.NewString()
	ext := mimeToExt[mimeType]
	if ext == "" {
		ext = filepath.Ext(originalFilename)
	}
	storedFilename := id + ext
	finalPath := p.OriginalPath(storedFilename)

	if err := moveFile(tempPath, finalPath); err != nil {
		return nil, fmt.Errorf("move to permanent storage: %w", err)
	}

	result := &Result{
		ID:             id,
		StoredFilename: storedFilename,
		Kind:           kind,
		MimeType:       mimeType,
		SizeBytes:      stat.Size(),
		SHA256:         sha256Hex,
	}

	switch kind {
	case models.KindImage:
		p.processImage(ctx, finalPath, id, result)
	case models.KindVideo:
		p.processVideo(ctx, finalPath, id, result)
	}

	return result, nil
}

func (p *Processor) processImage(ctx context.Context, finalPath, id string, result *Result) {
	thumbPath := p.ThumbnailPath(id)

	// Fast path: pure-Go decode (jpeg/png/gif/webp).
	if width, height, err := GenerateImageThumbnail(finalPath, thumbPath, p.ThumbnailMaxDimension); err == nil {
		result.Width, result.Height = width, height
		result.HasThumbnail = true
		if capturedAt, err := ImageCapturedAt(finalPath); err == nil && capturedAt != nil {
			result.CapturedAt = capturedAt
		}
		return
	}

	// Fallback: formats the pure-Go pipeline can't decode (AVIF/HEIC/HEIF, or
	// otherwise-undecodable images) go through ffmpeg/ffprobe. Non-fatal:
	// on any failure the item is still ingested, just without a thumbnail.
	probe, probeErr := ProbeImage(ctx, finalPath)

	if err := GenerateImageThumbnailFFmpeg(ctx, finalPath, thumbPath, p.ThumbnailMaxDimension); err == nil {
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

// RemoveMedia deletes the stored original and thumbnail (if any) for a
// media item. Used when purging soft-deleted items is ever needed and by
// tests; the current admin flow only soft-deletes, so this is primarily a
// utility for cleanup paths and tests.
func (p *Processor) RemoveMedia(storedFilename, id string) error {
	var firstErr error
	if err := os.Remove(p.OriginalPath(storedFilename)); err != nil && !errors.Is(err, os.ErrNotExist) {
		firstErr = err
	}
	if err := os.Remove(p.ThumbnailPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// Cross-device rename (EXDEV) or other rename failure: fall back to
	// copy + remove, which works across filesystem/volume boundaries.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("remove source after copy: %w", err)
	}
	return nil
}
