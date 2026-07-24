package media

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// ImageProbe holds the raw (coded) pixel dimensions and best-effort capture
// time read from an image container via ffprobe. Coded dimensions are
// orientation-agnostic; display orientation is resolved separately by
// comparing against the ffmpeg-rendered thumbnail (see orientImageDimensions).
type ImageProbe struct {
	CodedWidth  int
	CodedHeight int
	CapturedAt  *time.Time
}

// ProbeImage reads coded dimensions and container creation_time (when present)
// for an image ffmpeg can demux (e.g. AVIF/HEIC/HEIF). It reuses the shared
// ffprobe runner and the same JSON shape as video probing.
func ProbeImage(ctx context.Context, path string) (*ImageProbe, error) {
	out, err := runFFprobe(ctx, path)
	if err != nil {
		return nil, err
	}
	p := &ImageProbe{}
	for _, s := range out.Streams {
		if s.CodecType == "video" && p.CodedWidth == 0 {
			p.CodedWidth, p.CodedHeight = s.Width, s.Height
		}
	}
	if creation, ok := out.Format.Tags["creation_time"]; ok {
		if t, err := time.Parse(time.RFC3339, creation); err == nil {
			utc := t.UTC()
			p.CapturedAt = &utc
		}
	}
	return p, nil
}

// orientImageDimensions returns display-oriented original dimensions. It takes
// the coded (un-rotated) dimensions from ffprobe and the ffmpeg-rendered
// thumbnail's dimensions (which are already auto-rotated by ffmpeg). If the
// coded aspect and the thumbnail aspect disagree on portrait-vs-landscape, the
// still is rotated 90/270 degrees, so the coded dimensions are swapped. This
// makes stored dimensions consistent with the thumbnail by construction,
// independent of whether ffprobe surfaces still-image rotation metadata.
func orientImageDimensions(codedW, codedH, thumbW, thumbH int) (int, int) {
	if codedW <= 0 || codedH <= 0 {
		return codedW, codedH
	}
	if thumbW > 0 && thumbH > 0 {
		if (codedW > codedH) != (thumbW > thumbH) {
			return codedH, codedW
		}
	}
	return codedW, codedH
}

// GenerateImageThumbnailFFmpeg renders a single scaled JPEG frame from an
// image ffmpeg can decode, using the same scale filter and quality as the
// video thumbnail path. Bounded by a 30s context timeout.
func GenerateImageThumbnailFFmpeg(ctx context.Context, srcPath, dstPath string, maxDimension int) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	scaleFilter := fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", maxDimension, maxDimension)

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y",
		"-i", srcPath,
		"-frames:v", "1",
		"-vf", scaleFilter,
		"-q:v", "3",
		dstPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg image thumbnail failed: %w (stderr: %s)", err, stderr.String())
	}
	return nil
}
