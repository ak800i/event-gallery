package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"time"
)

// VideoInfo holds metadata extracted from a video file via ffprobe.
type VideoInfo struct {
	DurationSeconds float64
	Width           int
	Height          int
	CapturedAt      *time.Time
}

type ffprobeFormat struct {
	Duration string            `json:"duration"`
	Tags     map[string]string `json:"tags"`
}

type ffprobeSideData struct {
	Rotation float64 `json:"rotation"`
}

type ffprobeStream struct {
	CodecType   string            `json:"codec_type"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	Tags        map[string]string `json:"tags"`
	SideDataList []ffprobeSideData `json:"side_data_list"`
}

type ffprobeOutput struct {
	Format  ffprobeFormat   `json:"format"`
	Streams []ffprobeStream `json:"streams"`
}

// ProbeVideo shells out to ffprobe to extract duration, dimensions, and
// (when present) the recording creation time embedded in the container's
// metadata.
func ProbeVideo(ctx context.Context, path string) (*VideoInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffprobe failed: %w (stderr: %s)", err, stderr.String())
	}

	var out ffprobeOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}

	info := &VideoInfo{}
	if d, err := strconv.ParseFloat(out.Format.Duration, 64); err == nil {
		info.DurationSeconds = d
	}
	for _, s := range out.Streams {
		if s.CodecType == "video" && info.Width == 0 {
			info.Width, info.Height = displayDimensions(s)
		}
	}
	if creation, ok := out.Format.Tags["creation_time"]; ok {
		if t, err := time.Parse(time.RFC3339, creation); err == nil {
			utc := t.UTC()
			info.CapturedAt = &utc
		}
	}
	return info, nil
}

// displayDimensions applies the stream display matrix / rotate tag to the raw
// encoded dimensions. Phones commonly encode portrait video as landscape
// frames plus a +/-90 degree display rotation; the gallery must use display
// dimensions or it allocates a wide tile for a portrait thumbnail.
func displayDimensions(stream ffprobeStream) (int, int) {
	rotation := 0.0
	for _, sideData := range stream.SideDataList {
		if math.Abs(sideData.Rotation) > 0.01 {
			rotation = sideData.Rotation
			break
		}
	}
	if math.Abs(rotation) <= 0.01 {
		if tagged, ok := stream.Tags["rotate"]; ok {
			if parsed, err := strconv.ParseFloat(tagged, 64); err == nil {
				rotation = parsed
			}
		}
	}
	normalized := math.Mod(rotation, 360)
	if normalized < 0 {
		normalized += 360
	}
	if math.Abs(normalized-90) < 0.5 || math.Abs(normalized-270) < 0.5 {
		return stream.Height, stream.Width
	}
	return stream.Width, stream.Height
}

// GenerateVideoThumbnail extracts a representative JPEG frame from a video
// using ffmpeg. It seeks to roughly the earliest of 1 second or 10% into
// the clip, to skip black opening frames while staying safe for very short
// clips.
func GenerateVideoThumbnail(ctx context.Context, srcPath, dstPath string, maxDimension int, duration float64) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	seek := 1.0
	if duration > 0 && duration*0.1 < seek {
		seek = duration * 0.1
	}
	if seek < 0 {
		seek = 0
	}

	scaleFilter := fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", maxDimension, maxDimension)

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y",
		"-ss", strconv.FormatFloat(seek, 'f', 3, 64),
		"-i", srcPath,
		"-frames:v", "1",
		"-vf", scaleFilter,
		"-q:v", "3",
		dstPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg thumbnail failed: %w (stderr: %s)", err, stderr.String())
	}
	return nil
}
