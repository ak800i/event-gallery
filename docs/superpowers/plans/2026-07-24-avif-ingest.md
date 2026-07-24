# AVIF Ingest + ffmpeg Thumbnail Fallback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Accept AVIF image uploads and generate a real JPEG thumbnail for them (and any other image the pure-Go pipeline can't decode, e.g. HEIC/HEIF) by falling back to ffmpeg/ffprobe.

**Architecture:** Add an AVIF magic-byte signature (ordered before HEIF), add `image/avif` to the default allowlist and extension map, and rework `processImage` into a two-stage chain: pure-Go decode first, then an ffmpeg/ffprobe fallback. The fallback reuses a shared ffprobe runner for coded dimensions + capture time, and derives display-correct orientation by comparing the original's coded aspect against the ffmpeg-rendered thumbnail's aspect — making orientation correct by construction rather than relying on ffprobe surfacing still-image rotation metadata.

**Tech Stack:** Go (`CGO_ENABLED=0`), ffmpeg/ffprobe (already runtime deps), `disintegration/imaging`, `rwcarlsen/goexif`. Backend package `event-gallery/backend/internal/media` and `.../config`.

## Global Constraints

- Build is `CGO_ENABLED=0` — no CGO-based decoders. Pure-Go decode or shell out to ffmpeg only.
- Runtime ffmpeg is pinned to `ffmpeg=6.1.2-r2` (Alpine, links libdav1d). Do not assume HEIC decodes; validate (Task 6).
- All ffmpeg/ffprobe calls run under a 30s `context.Context` timeout, matching the existing video path.
- Undecodable files must remain non-fatal: the item is still ingested with no thumbnail.
- Thumbnails are JPEG written to `<ThumbnailsDir>/<id>.jpg` (unchanged from today).
- Follow the existing ffmpeg test pattern: gate ffmpeg/ffprobe-dependent tests behind `requireFFmpeg(t)` so CI without ffmpeg still passes.
- MIME sniffing never trusts client-declared types; detection is magic-byte based in `sniff.go`.

---

## File Structure

- `backend/internal/media/sniff.go` — add AVIF signature (before HEIF). Modify.
- `backend/internal/media/sniff_test.go` — add ftyp fixture helper + AVIF sniff/ordering tests. Modify.
- `backend/internal/config/config.go` — add `image/avif` to default allowlist. Modify.
- `backend/internal/config/config_test.go` — assert `image/avif` default. Modify.
- `backend/internal/media/processor.go` — add `image/avif` ext map entry; rework `processImage` into the fallback chain; thread `ctx`. Modify.
- `backend/internal/media/processor_test.go` — ext-map test + ffmpeg-gated AVIF ingest test. Modify.
- `backend/internal/media/video.go` — extract shared `runFFprobe`; `ProbeVideo` uses it. Modify.
- `backend/internal/media/ffmpeg_image.go` — new: `ImageProbe`, `ProbeImage`, `orientImageDimensions`, `GenerateImageThumbnailFFmpeg`. Create.
- `backend/internal/media/ffmpeg_image_test.go` — new: pure-Go `orientImageDimensions` tests + ffmpeg-gated probe/thumbnail tests. Create.

---

### Task 1: Sniff AVIF (before HEIF)

**Files:**
- Modify: `backend/internal/media/sniff.go` (signatures slice, before the `image/heif` entry)
- Test: `backend/internal/media/sniff_test.go`

**Interfaces:**
- Consumes: existing `isFtypBrand(b []byte, brands ...string) bool`, `sniffSignature`, `Sniff(path) (string, models.MediaKind, error)`.
- Produces: `Sniff` returns `"image/avif", models.KindImage` for AVIF `ftyp` files.

- [ ] **Step 1: Write the failing test**

Add to `backend/internal/media/sniff_test.go`:

```go
// writeFtyp writes a minimal ISO-BMFF file whose ftyp box declares the given
// major brand followed by the given compatible brands. Enough for Sniff.
func writeFtyp(t *testing.T, path, major string, compatible ...string) {
	t.Helper()
	if len(major) != 4 {
		t.Fatalf("major brand must be 4 bytes, got %q", major)
	}
	body := []byte(major)
	body = append(body, 0x00, 0x00, 0x00, 0x00) // minor version
	for _, c := range compatible {
		if len(c) != 4 {
			t.Fatalf("compatible brand must be 4 bytes, got %q", c)
		}
		body = append(body, []byte(c)...)
	}
	boxLen := 8 + len(body)
	buf := make([]byte, 0, boxLen+8)
	buf = append(buf, byte(boxLen>>24), byte(boxLen>>16), byte(boxLen>>8), byte(boxLen))
	buf = append(buf, []byte("ftyp")...)
	buf = append(buf, body...)
	// Pad so Sniff's 64-byte read always succeeds.
	for len(buf) < 64 {
		buf = append(buf, 0x00)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write ftyp: %v", err)
	}
}

func TestSniff_AVIF(t *testing.T) {
	dir := t.TempDir()

	avifPath := filepath.Join(dir, "photo.avif")
	writeFtyp(t, avifPath, "avif", "mif1", "miaf")
	if mt, kind, err := Sniff(avifPath); err != nil || mt != "image/avif" || kind != models.KindImage {
		t.Errorf("avif sniff: mt=%s kind=%s err=%v", mt, kind, err)
	}

	avisPath := filepath.Join(dir, "seq.avif")
	writeFtyp(t, avisPath, "avis", "avif", "mif1")
	if mt, _, err := Sniff(avisPath); err != nil || mt != "image/avif" {
		t.Errorf("avis sniff: mt=%s err=%v", mt, err)
	}
}

func TestSniff_AVIFBeforeHEIF(t *testing.T) {
	dir := t.TempDir()
	// AVIF with mif1 in compatible brands must NOT be seen as image/heif.
	p := filepath.Join(dir, "amb.avif")
	writeFtyp(t, p, "avif", "mif1")
	if mt, _, err := Sniff(p); err != nil || mt != "image/avif" {
		t.Fatalf("expected image/avif (ordering), got mt=%s err=%v", mt, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend; go test ./internal/media/ -run 'TestSniff_AVIF' -v`
Expected: FAIL — Sniff returns `image/heif` or an unsupported-type error for AVIF.

- [ ] **Step 3: Write minimal implementation**

In `backend/internal/media/sniff.go`, insert the AVIF entry immediately **before** the `image/heif` entry in the `signatures` slice:

```go
	{mime: "image/avif", kind: models.KindImage, match: func(b []byte) bool {
		return isFtypBrand(b, "avif", "avis")
	}},
	{mime: "image/heif", kind: models.KindImage, match: func(b []byte) bool {
		return isFtypBrand(b, "mif1", "msf1")
	}},
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend; go test ./internal/media/ -run 'TestSniff' -v`
Expected: PASS (including the existing `TestSniff_KnownFormats`).

- [ ] **Step 5: Commit**

```bash
git add backend/internal/media/sniff.go backend/internal/media/sniff_test.go
git commit -m "feat(media): sniff AVIF via ftyp brands, ordered before HEIF"
```

---

### Task 2: Add image/avif to the default allowlist

**Files:**
- Modify: `backend/internal/config/config.go:241-243` (the `ALLOWED_IMAGE_MIME_TYPES` default)
- Test: `backend/internal/config/config_test.go`

**Interfaces:**
- Consumes: `Load() (*Config, error)`, `Config.AllowedImageMIMEs []string`.
- Produces: default `AllowedImageMIMEs` includes `"image/avif"`.

- [ ] **Step 1: Write the failing test**

Add to `backend/internal/config/config_test.go`:

```go
func TestLoad_AllowsAVIFByDefault(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
	t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, m := range cfg.AllowedImageMIMEs {
		if m == "image/avif" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected image/avif in default allowlist, got %v", cfg.AllowedImageMIMEs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend; go test ./internal/config/ -run 'TestLoad_AllowsAVIFByDefault' -v`
Expected: FAIL — `image/avif` not in defaults.

- [ ] **Step 3: Write minimal implementation**

In `backend/internal/config/config.go`, update the default list:

```go
	cfg.AllowedImageMIMEs = envList("ALLOWED_IMAGE_MIME_TYPES", []string{
		"image/jpeg", "image/png", "image/webp", "image/gif", "image/heic", "image/heif", "image/avif",
	})
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend; go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/config/config.go backend/internal/config/config_test.go
git commit -m "feat(config): allow image/avif uploads by default"
```

---

### Task 3: Add image/avif to the extension map

**Files:**
- Modify: `backend/internal/media/processor.go` (`mimeToExt` map)
- Test: `backend/internal/media/processor_test.go`

**Interfaces:**
- Consumes: unexported `mimeToExt map[string]string` (same-package test).
- Produces: `mimeToExt["image/avif"] == ".avif"`.

- [ ] **Step 1: Write the failing test**

Add to `backend/internal/media/processor_test.go`:

```go
func TestMimeToExt_AVIF(t *testing.T) {
	if got := mimeToExt["image/avif"]; got != ".avif" {
		t.Errorf("expected .avif for image/avif, got %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend; go test ./internal/media/ -run 'TestMimeToExt_AVIF' -v`
Expected: FAIL — map returns `""`.

- [ ] **Step 3: Write minimal implementation**

In `backend/internal/media/processor.go`, add the entry to `mimeToExt` (next to the other image types):

```go
	"image/heif":      ".heif",
	"image/avif":      ".avif",
	"video/mp4":       ".mp4",
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd backend; go test ./internal/media/ -run 'TestMimeToExt_AVIF' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/media/processor.go backend/internal/media/processor_test.go
git commit -m "feat(media): map image/avif to .avif extension"
```

---

### Task 4: Extract shared ffprobe runner; add image probe, orientation helper, and ffmpeg image thumbnailer

**Files:**
- Modify: `backend/internal/media/video.go` (extract `runFFprobe`; `ProbeVideo` uses it)
- Create: `backend/internal/media/ffmpeg_image.go`
- Create: `backend/internal/media/ffmpeg_image_test.go`

**Interfaces:**
- Consumes: existing `ffprobeOutput`, `ffprobeStream`, `displayDimensions` (in `video.go`); existing `requireFFmpeg(t)` (in `video_test.go`).
- Produces:
  - `runFFprobe(ctx context.Context, path string) (*ffprobeOutput, error)`
  - `type ImageProbe struct { CodedWidth int; CodedHeight int; CapturedAt *time.Time }`
  - `ProbeImage(ctx context.Context, path string) (*ImageProbe, error)`
  - `orientImageDimensions(codedW, codedH, thumbW, thumbH int) (int, int)`
  - `GenerateImageThumbnailFFmpeg(ctx context.Context, srcPath, dstPath string, maxDimension int) error`

- [ ] **Step 1: Write the failing test (pure-Go orientation logic + ffmpeg-gated probe/thumbnail)**

Create `backend/internal/media/ffmpeg_image_test.go`:

```go
package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestOrientImageDimensions(t *testing.T) {
	cases := []struct {
		name                           string
		cw, ch, tw, th                 int
		wantW, wantH                   int
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
// e.g. some CI, still exercise everything else). Returns the coded w,h.
func generateTestAVIF(t *testing.T, path string, w, h int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y",
		"-f", "lavfi",
		"-i", "testsrc=size="+itoa(w)+"x"+itoa(h)+":duration=1:rate=1",
		"-frames:v", "1",
		"-c:v", "libaom-av1", "-still-picture", "1",
		path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot encode test AVIF with this ffmpeg build: %v\n%s", err, out)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

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
	if err := GenerateImageThumbnailFFmpeg(ctx, src, dst, 100); err != nil {
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
```

Add `"strconv"` to the import block.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend; go test ./internal/media/ -run 'TestOrientImageDimensions|TestProbeImageAndThumbnail_AVIF' -v`
Expected: FAIL — `orientImageDimensions`, `ProbeImage`, `GenerateImageThumbnailFFmpeg` undefined.

- [ ] **Step 3a: Extract the shared ffprobe runner in `video.go`**

In `backend/internal/media/video.go`, replace the body of `ProbeVideo` so it delegates to a new `runFFprobe`, and add `runFFprobe`:

```go
// runFFprobe runs ffprobe (JSON, format+streams) on path under a 30s timeout
// and returns the decoded output. Shared by video and image probing.
func runFFprobe(ctx context.Context, path string) (*ffprobeOutput, error) {
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
	return &out, nil
}

// ProbeVideo shells out to ffprobe to extract duration, dimensions, and
// (when present) the recording creation time embedded in the container's
// metadata.
func ProbeVideo(ctx context.Context, path string) (*VideoInfo, error) {
	out, err := runFFprobe(ctx, path)
	if err != nil {
		return nil, err
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
```

- [ ] **Step 3b: Create `ffmpeg_image.go`**

Create `backend/internal/media/ffmpeg_image.go`:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd backend; go test ./internal/media/ -run 'TestOrientImageDimensions|TestProbeImageAndThumbnail_AVIF|TestProbeVideo' -v`
Expected: PASS (`TestOrientImageDimensions` always; the AVIF test passes where ffmpeg can encode AVIF, else skips; `TestProbeVideo` still passes after the refactor).

- [ ] **Step 5: Run the full media + vet to confirm no breakage**

Run: `cd backend; go vet ./internal/media/; go test ./internal/media/ -v`
Expected: PASS/skip, no vet errors.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/media/video.go backend/internal/media/ffmpeg_image.go backend/internal/media/ffmpeg_image_test.go
git commit -m "feat(media): add shared ffprobe runner, image probe, and ffmpeg image thumbnailer"
```

---

### Task 5: Rework processImage into the pure-Go → ffmpeg fallback chain

**Files:**
- Modify: `backend/internal/media/processor.go` (`Process` call site + `processImage`)
- Test: `backend/internal/media/processor_test.go`

**Interfaces:**
- Consumes: `GenerateImageThumbnail` (thumbnail.go), `ImageDimensions` (thumbnail.go), `ImageCapturedAt` (exif.go), and from Task 4: `ProbeImage`, `orientImageDimensions`, `GenerateImageThumbnailFFmpeg`.
- Produces: `processImage(ctx context.Context, finalPath, id string, result *Result)`; AVIF/HEIC/HEIF now yield `HasThumbnail = true` with display-oriented `Width`/`Height` when ffmpeg can decode them; existing pure-Go formats are unchanged.

- [ ] **Step 1: Write the failing test**

Add to `backend/internal/media/processor_test.go`:

```go
func TestProcessor_ProcessAVIF(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	proc := NewProcessor(filepath.Join(dir, "media"), 100, []string{"image/avif"}, nil)

	tempPath := filepath.Join(dir, "incoming.tmp")
	generateTestAVIF(t, tempPath, 60, 40) // landscape; skips if no AV1 encoder

	result, err := proc.Process(context.Background(), tempPath, "guest.avif")
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if result.MimeType != "image/avif" {
		t.Errorf("expected image/avif, got %s", result.MimeType)
	}
	if !result.HasThumbnail {
		t.Error("expected an ffmpeg-generated thumbnail for AVIF")
	}
	if result.Width <= 0 || result.Height <= 0 {
		t.Errorf("expected non-zero dimensions, got %dx%d", result.Width, result.Height)
	}
	if !(result.Width > result.Height) {
		t.Errorf("expected landscape stored dims, got %dx%d", result.Width, result.Height)
	}
	if _, err := os.Stat(proc.ThumbnailPath(result.ID)); err != nil {
		t.Errorf("expected thumbnail file on disk: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd backend; go test ./internal/media/ -run 'TestProcessor_ProcessAVIF' -v`
Expected: FAIL — `processImage` doesn't decode AVIF, so `HasThumbnail` is false and dims are 0 (or compile error once the call site is changed).

- [ ] **Step 3: Update the `Process` call site**

In `backend/internal/media/processor.go`, thread `ctx` into `processImage`:

```go
	switch kind {
	case models.KindImage:
		p.processImage(ctx, finalPath, id, result)
	case models.KindVideo:
		p.processVideo(ctx, finalPath, id, result)
	}
```

- [ ] **Step 4: Replace `processImage` with the fallback chain**

In `backend/internal/media/processor.go`, replace the whole `processImage` function:

```go
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
		thumbW, thumbH, dimErr := ImageDimensions(thumbPath)
		switch {
		case probeErr == nil && dimErr == nil:
			result.Width, result.Height = orientImageDimensions(probe.CodedWidth, probe.CodedHeight, thumbW, thumbH)
		case dimErr == nil:
			result.Width, result.Height = thumbW, thumbH
		case probeErr == nil:
			result.Width, result.Height = probe.CodedWidth, probe.CodedHeight
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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd backend; go test ./internal/media/ -run 'TestProcessor_' -v`
Expected: PASS — `TestProcessor_ProcessImage` (pure-Go JPEG, unchanged) still passes; `TestProcessor_ProcessAVIF` passes (or skips without an AV1 encoder).

- [ ] **Step 6: Run the whole backend test suite + vet**

Run: `cd backend; go vet ./...; go test ./...`
Expected: PASS (ffmpeg-dependent tests skip where ffmpeg is unavailable).

- [ ] **Step 7: Commit**

```bash
git add backend/internal/media/processor.go backend/internal/media/processor_test.go
git commit -m "feat(media): ffmpeg fallback thumbnails+dims for AVIF/HEIC/HEIF in processImage"
```

---

### Task 6: Validate decode capability + orientation in the pinned runtime image

This task confirms the spec's runtime assumptions in the actual production image and locks in a ground-truth rotated-orientation regression test. It is a required deliverable: the AVIF thumbnail feature is only "done" once real decode is confirmed in the pinned build.

**Files:**
- Create: `backend/internal/media/testdata/rotated_portrait.avif` (a real AVIF whose intended display orientation is portrait)
- Modify: `backend/internal/media/ffmpeg_image_test.go` (ground-truth rotated test using the committed fixture)
- Modify: `docs/superpowers/specs/2026-07-24-avif-ingest-design.md` (record the HEIC decode result)

**Interfaces:**
- Consumes: `ProbeImage`, `GenerateImageThumbnailFFmpeg`, `orientImageDimensions`, `ImageDimensions`.
- Produces: a committed fixture + a deterministic (fixture-based) rotated-orientation test.

- [ ] **Step 1: Confirm AVIF and HEIC decode in the pinned image**

Run (from repo root):

```bash
docker build -t event-gallery-validate .
# Use a portrait-oriented real photo saved locally as sample.avif / sample.heic.
docker run --rm -v "${PWD}:/w" -w /w event-gallery-validate \
  ffmpeg -i sample.avif -frames:v 1 -y /tmp/out_avif.jpg
docker run --rm -v "${PWD}:/w" -w /w event-gallery-validate \
  ffmpeg -i sample.heic -frames:v 1 -y /tmp/out_heic.jpg
```

Expected: the AVIF command produces a JPEG (exit 0). Record whether the HEIC command succeeds.

- [ ] **Step 2: Inspect whether ffprobe surfaces still rotation**

Run:

```bash
docker run --rm -v "${PWD}:/w" -w /w event-gallery-validate \
  ffprobe -v error -print_format json -show_streams sample.avif
```

Expected: note whether `side_data_list[].rotation` or the `rotate` tag appears. (Informational — the implementation does not depend on it because orientation is derived from the rendered thumbnail via `orientImageDimensions`. This step documents reality.)

- [ ] **Step 3: Commit a real rotated fixture**

Save the confirmed-portrait AVIF from Step 1 into `backend/internal/media/testdata/rotated_portrait.avif`. This is a real, decodable AVIF whose displayed orientation is portrait (taller than wide).

- [ ] **Step 4: Write the ground-truth rotated-orientation test**

Add to `backend/internal/media/ffmpeg_image_test.go`:

```go
func TestProcessAVIF_RotatedGroundTruth(t *testing.T) {
	requireFFmpeg(t)
	const fixture = "testdata/rotated_portrait.avif"
	if _, err := os.Stat(fixture); err != nil {
		t.Skip("rotated_portrait.avif fixture not present")
	}
	// The fixture is a still whose intended DISPLAY orientation is portrait.
	dir := t.TempDir()
	dst := filepath.Join(dir, "thumb.jpg")

	ctx := context.Background()
	if err := GenerateImageThumbnailFFmpeg(ctx, fixture, dst, 100); err != nil {
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
```

- [ ] **Step 5: Run the test**

Run: `cd backend; go test ./internal/media/ -run 'TestProcessAVIF_RotatedGroundTruth' -v`
Expected: PASS (both the rendered thumbnail and the stored dims are portrait).

- [ ] **Step 6: Record the HEIC result in the spec**

In `docs/superpowers/specs/2026-07-24-avif-ingest-design.md`, under the Testing "Environment validation" bullet, append one line stating whether HEIC/HEIF decode was confirmed present or absent in `ffmpeg=6.1.2-r2`.

- [ ] **Step 7: Commit**

```bash
git add backend/internal/media/testdata/rotated_portrait.avif backend/internal/media/ffmpeg_image_test.go docs/superpowers/specs/2026-07-24-avif-ingest-design.md
git commit -m "test(media): validate AVIF decode + ground-truth rotated orientation; record HEIC result"
```

---

## Self-Review

**Spec coverage:**
- Sniffing (AVIF before HEIF) → Task 1. ✔
- Allowlist default → Task 2. ✔
- Extension map → Task 3. ✔
- ffmpeg image thumbnail + probe reuse (shared runner) → Task 4. ✔
- Orientation consistency by construction (rendered-thumbnail cross-check) → Task 4 (`orientImageDimensions`) + Task 6 (ground-truth fixture). ✔
- Best-effort capture time from probe → Task 5 (`processImage` fallback). ✔
- `processImage` gains `ctx`; pure-Go → ffmpeg fallback chain; non-fatal → Task 5. ✔
- Generic fallback (any decode failure, not AVIF-only) → Task 5 (branches on pure-Go decode error, not MIME). ✔
- Environment validation (AVIF/HEIC decode, rotation surfacing) → Task 6. ✔
- ffmpeg-gated tests skip without ffmpeg → all ffmpeg tests use `requireFFmpeg` / skip on missing encoder or fixture. ✔

**Placeholder scan:** No TBD/TODO; every code and test step shows full code; commands include expected output. ✔

**Type consistency:** `ImageProbe{CodedWidth,CodedHeight,CapturedAt}`, `ProbeImage`, `orientImageDimensions(codedW,codedH,thumbW,thumbH)`, `GenerateImageThumbnailFFmpeg(ctx,src,dst,max)`, `runFFprobe(ctx,path)` are defined in Task 4 and consumed with identical signatures in Task 5/6. `processImage(ctx,finalPath,id,result)` matches its updated call site. ✔

**Note on fixture dependence:** The core orientation logic is covered deterministically by `TestOrientImageDimensions` (no ffmpeg). End-to-end AVIF tests skip cleanly when the local ffmpeg lacks an AV1 encoder or the rotated fixture is absent, so the suite stays green everywhere while real decode/orientation is confirmed in the pinned image (Task 6).

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-07-24-avif-ingest.md`.
