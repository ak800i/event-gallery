package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A tiled HEIC can only be scaled through a complex filtergraph, and ffmpeg
// has no HEIC muxer so no such fixture can be generated here. Pin the flag
// itself: with -vf, ffmpeg writes no thumbnail at all for iPhone photos.
func TestImageThumbnailArgsUseComplexFiltering(t *testing.T) {
	args := imageThumbnailArgs("/in.heic", "/out.jpg", 1600, 0)
	var filterFlag, filterValue string
	for i, a := range args {
		if a == "-vf" || a == "-filter_complex" {
			filterFlag = a
			if i+1 < len(args) {
				filterValue = args[i+1]
			}
		}
	}
	if filterFlag != "-filter_complex" {
		t.Errorf("scaling must use -filter_complex, got %q", filterFlag)
	}
	if filterValue != "scale='min(1600,iw)':'min(1600,ih)':force_original_aspect_ratio=decrease" {
		t.Errorf("unexpected scale filter %q", filterValue)
	}
	for _, a := range args {
		if a == "-lowres" {
			t.Error("lowres 0 must not emit the flag at all")
		}
	}
}

// -lowres is an input decoder option: after -i it is silently ignored and the
// full-resolution decode happens anyway, which is the whole cost being avoided.
func TestImageThumbnailArgsPlaceLowresBeforeInput(t *testing.T) {
	args := imageThumbnailArgs("/in.jpg", "/out.jpg", 1600, 2)
	lowresAt, inputAt := -1, -1
	for i, a := range args {
		switch a {
		case "-lowres":
			if lowresAt < 0 {
				lowresAt = i
			}
		case "-i":
			if inputAt < 0 {
				inputAt = i
			}
		}
	}
	if lowresAt < 0 {
		t.Fatalf("expected -lowres in %v", args)
	}
	if args[lowresAt+1] != "2" {
		t.Errorf("-lowres value = %q, want 2", args[lowresAt+1])
	}
	if lowresAt > inputAt {
		t.Errorf("-lowres at %d must come before -i at %d: %v", lowresAt, inputAt, args)
	}
}

// The level must never shrink the longest edge below the thumbnail target,
// otherwise the cheaper decode silently costs thumbnail resolution.
func TestLowresLevel(t *testing.T) {
	cases := []struct {
		name         string
		codec        string
		w, h, maxDim int
		want         int
	}{
		{"48 MP jpeg", "mjpeg", 8000, 6000, 1600, 2},
		{"12 MP jpeg", "mjpeg", 4032, 3024, 1600, 1},
		{"portrait uses longest edge", "mjpeg", 3024, 4032, 1600, 1},
		{"already small", "mjpeg", 1000, 800, 1600, 0},
		{"exactly at target after one halving", "mjpeg", 3200, 2400, 1600, 1},
		{"capped at the decoder ceiling", "mjpeg", 100000, 100000, 1, 3},
		// HEIC/AVIF decoders have no lowres support; asking would fail the run.
		{"hevc", "hevc", 8000, 6000, 1600, 0},
		{"av1", "av1", 8000, 6000, 1600, 0},
		{"unknown codec", "", 8000, 6000, 1600, 0},
		{"no target", "mjpeg", 8000, 6000, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lowresLevel(c.codec, c.w, c.h, c.maxDim); got != c.want {
				t.Errorf("lowresLevel(%q, %d, %d, %d) = %d, want %d", c.codec, c.w, c.h, c.maxDim, got, c.want)
			}
		})
	}
}

func TestOrientImageDimensions(t *testing.T) {
	cases := []struct {
		name           string
		cw, ch, tw, th int
		wantW, wantH   int
	}{
		{"landscape unrotated", 4000, 3000, 40, 30, 4000, 3000},
		{"portrait unrotated", 3000, 4000, 30, 40, 3000, 4000},
		// Coded landscape but thumbnail portrait => rotated 90/270 => swap.
		{"rotated to portrait", 4000, 3000, 30, 40, 3000, 4000},
		// Coded portrait but thumbnail landscape => swap.
		{"rotated to landscape", 3000, 4000, 40, 30, 4000, 3000},
		{"square", 2000, 2000, 20, 20, 2000, 2000},
		{"missing coded", 0, 0, 40, 30, 0, 0},
		{"missing thumb keeps coded", 4000, 3000, 0, 0, 4000, 3000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h := orientImageDimensions(c.cw, c.ch, c.tw, c.th)
			if w != c.wantW || h != c.wantH {
				t.Errorf("orientImageDimensions(%d,%d,%d,%d) = %dx%d, want %dx%d",
					c.cw, c.ch, c.tw, c.th, w, h, c.wantW, c.wantH)
			}
		})
	}
}

// generateTestAVIF tries to produce a still AVIF via ffmpeg. If the local
// ffmpeg has no AV1 encoder, the test is skipped (decode-only environments,
// e.g. some CI, still exercise everything else). The output muxer is forced
// with `-f avif` so it does not depend on the destination file extension
// (callers may write to a temp path like incoming.tmp). Reuses the existing
// package-level itoa helper from video_test.go.
func generateTestAVIF(t *testing.T, path string, w, h int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y",
		"-f", "lavfi",
		"-i", "testsrc=size="+itoa(w)+"x"+itoa(h)+":duration=1:rate=1",
		"-frames:v", "1",
		"-c:v", "libaom-av1", "-still-picture", "1",
		"-f", "avif",
		path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot encode test AVIF with this ffmpeg build: %v\n%s", err, out)
	}
}

func TestProbeImageAndThumbnail_AVIF(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "sample.avif")
	generateTestAVIF(t, src, 60, 40) // landscape

	ctx := context.Background()
	probe, err := ProbeImage(ctx, src)
	if err != nil {
		t.Fatalf("ProbeImage: %v", err)
	}
	if probe.CodedWidth != 60 || probe.CodedHeight != 40 {
		t.Errorf("expected coded 60x40, got %dx%d", probe.CodedWidth, probe.CodedHeight)
	}

	dst := filepath.Join(dir, "thumb.jpg")
	if err := GenerateImageThumbnailFFmpeg(ctx, src, dst, 100, 0); err != nil {
		t.Fatalf("GenerateImageThumbnailFFmpeg: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("expected thumbnail file: %v", err)
	}
	tw, th, err := ImageDimensions(dst)
	if err != nil {
		t.Fatalf("thumbnail dims: %v", err)
	}
	if !(tw > th) {
		t.Errorf("expected landscape thumbnail, got %dx%d", tw, th)
	}
}

func TestProcessAVIF_RealPortraitFixture(t *testing.T) {
	requireFFmpeg(t)
	const fixture = "testdata/sample_portrait.avif"
	if _, err := os.Stat(fixture); err != nil {
		t.Skip("sample_portrait.avif fixture not present")
	}
	// The fixture is a real AVIF whose display orientation is portrait.
	dir := t.TempDir()
	dst := filepath.Join(dir, "thumb.jpg")
	ctx := context.Background()

	if err := GenerateImageThumbnailFFmpeg(ctx, fixture, dst, 100, 0); err != nil {
		t.Fatalf("thumbnail: %v", err)
	}
	tw, th, err := ImageDimensions(dst)
	if err != nil {
		t.Fatalf("thumb dims: %v", err)
	}
	if !(th > tw) {
		t.Errorf("expected portrait thumbnail (ground truth), got %dx%d", tw, th)
	}

	probe, err := ProbeImage(ctx, fixture)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	w, h := orientImageDimensions(probe.CodedWidth, probe.CodedHeight, tw, th)
	if !(h > w) {
		t.Errorf("expected portrait stored dims (ground truth), got %dx%d", w, h)
	}
}
