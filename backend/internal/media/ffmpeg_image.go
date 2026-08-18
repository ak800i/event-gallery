package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

// ImageProbe holds the raw (coded) pixel dimensions and best-effort capture
// time read from an image container via ffprobe. Coded dimensions are
// orientation-agnostic; display orientation is resolved separately by
// comparing against the ffmpeg-rendered thumbnail (see orientImageDimensions).
type ImageProbe struct {
	CodedWidth  int
	CodedHeight int
	Codec       string
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
			p.Codec = s.CodecName
		}
	}
	// A tiled still's streams are individual tiles, so the loop above finds
	// 512x512 rather than the photo. The assembled size only exists on the
	// stream group.
	if w, h, ok := probeTileGrid(ctx, path); ok {
		p.CodedWidth, p.CodedHeight = w, h
	}
	if creation, ok := out.Format.Tags["creation_time"]; ok {
		if t, err := time.Parse(time.RFC3339, creation); err == nil {
			utc := t.UTC()
			p.CapturedAt = &utc
		}
	}
	return p, nil
}

type ffprobeStreamGroups struct {
	StreamGroups []struct {
		Type       string `json:"type"`
		Components []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"components"`
	} `json:"stream_groups"`
}

// probeTileGrid reports the assembled dimensions of a tiled still image.
// Deliberately silent on every failure: -show_stream_groups only exists in
// ffmpeg 7.0+, and an older binary must degrade to per-stream dimensions
// rather than break probing outright.
func probeTileGrid(ctx context.Context, path string) (int, int, bool) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_stream_groups",
		path,
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return 0, 0, false
	}
	var groups ffprobeStreamGroups
	if err := json.Unmarshal(stdout.Bytes(), &groups); err != nil {
		return 0, 0, false
	}
	for _, g := range groups.StreamGroups {
		for _, c := range g.Components {
			if c.Width > 0 && c.Height > 0 {
				return c.Width, c.Height, true
			}
		}
	}
	return 0, 0, false
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

// lowresLevel picks a libavcodec `-lowres` level for a still: the decoder
// emits the frame at 1/2^level directly from the DCT coefficients, so a 48 MP
// JPEG costs 54 MB instead of 191 MB. Only codecs whose decoders implement
// lowres qualify -- HEVC and AV1, which back HEIC/AVIF, do not -- and the
// level never shrinks the longest edge below maxDimension, so the thumbnail
// is bit-for-bit the size it would have been at full decode.
func lowresLevel(codec string, codedW, codedH, maxDimension int) int {
	if codec != "mjpeg" || maxDimension <= 0 {
		return 0
	}
	longest := codedW
	if codedH > longest {
		longest = codedH
	}
	level := 0
	for level < maxLowresLevel && (longest>>(level+1)) >= maxDimension {
		level++
	}
	return level
}

// maxLowresLevel is libavcodec's ceiling for the mjpeg decoder.
const maxLowresLevel = 3

// imageThumbnailArgs builds the ffmpeg invocation for scaling a still image.
//
// The scale must be attached with -filter_complex rather than -vf. iPhone HEIC
// stores the photo as a grid of 512x512 tiles which ffmpeg stitches using an
// internal complex graph, and simple filtering cannot be attached to such a
// stream -- ffmpeg fails the whole run with "Simple and complex filtering
// cannot be used together for the same stream" and no thumbnail is written.
// The complex form is equivalent for every other still we decode here.
//
// lowres, when non-zero, must precede -i: it is an input decoder option.
func imageThumbnailArgs(srcPath, dstPath string, maxDimension, lowres int) []string {
	scaleFilter := fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", maxDimension, maxDimension)
	args := []string{"-y"}
	if lowres > 0 {
		args = append(args, "-lowres", strconv.Itoa(lowres))
	}
	return append(args,
		"-i", srcPath,
		"-filter_complex", scaleFilter,
		"-frames:v", "1",
		"-q:v", "3",
		dstPath,
	)
}

// GenerateImageThumbnailFFmpeg renders a single scaled JPEG frame from an
// image ffmpeg can decode, using the same scale filter and quality as the
// video thumbnail path. Bounded by a 30s context timeout.
//
// A lowres attempt is retried at full resolution on failure: lowres support is
// a property of the local build's decoder, and a thumbnail that costs more
// memory beats no thumbnail at all.
func GenerateImageThumbnailFFmpeg(ctx context.Context, srcPath, dstPath string, maxDimension, lowres int) error {
	err := runImageThumbnail(ctx, srcPath, dstPath, maxDimension, lowres)
	if err != nil && lowres > 0 {
		return runImageThumbnail(ctx, srcPath, dstPath, maxDimension, 0)
	}
	return err
}

func runImageThumbnail(ctx context.Context, srcPath, dstPath string, maxDimension, lowres int) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg", imageThumbnailArgs(srcPath, dstPath, maxDimension, lowres)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg image thumbnail failed: %w (stderr: %s)", err, stderr.String())
	}
	return nil
}
