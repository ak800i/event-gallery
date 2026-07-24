# AVIF Ingest + ffmpeg Thumbnail Fallback — Design

Date: 2026-07-24
Status: Approved

## Goal

Accept AVIF image uploads from guests (ingest) and produce a real JPEG
thumbnail for them — identical in format to today's thumbnails, not a
placeholder or generic preview.

Non-goal: generating AVIF *output* (serving AVIF thumbnails/derivatives to
viewers). This design is ingest-only.

## Constraints

- The backend builds with `CGO_ENABLED=0`, so no CGO-based AVIF decoder
  (libaom/dav1d bindings) is usable. There is no mature pure-Go AVIF decoder.
- ffmpeg is already a runtime dependency (video thumbnails/probing). Alpine's
  ffmpeg 6.1.2 links libdav1d, so it can decode AVIF still images. This is the
  mechanism used for AVIF/HEIC/HEIF thumbnails.
- Behavior for undecodable files must remain non-fatal: the item is still
  ingested even if no thumbnail can be produced.

## Changes

### 1. Sniffing (`internal/media/sniff.go`)

Add an AVIF magic-byte signature using the existing `isFtypBrand` helper:

```go
{mime: "image/avif", kind: models.KindImage, match: func(b []byte) bool {
    return isFtypBrand(b, "avif", "avis")
}},
```

Ordering constraint: the AVIF signature MUST appear before the `image/heif`
signature. AVIF files use major brand `avif`/`avis` but commonly list `mif1`
in their compatible brands, and the HEIF matcher keys on `mif1`/`msf1`. Since
`isFtypBrand` scans compatible brands too, an AVIF file checked against the
HEIF matcher first would be misclassified as `image/heif`.

### 2. Allowlist (`internal/config/config.go`)

Add `image/avif` to the default `ALLOWED_IMAGE_MIME_TYPES`:

```
"image/jpeg", "image/png", "image/webp", "image/gif", "image/heic",
"image/heif", "image/avif"
```

### 3. Extension map (`internal/media/processor.go`)

Add `"image/avif": ".avif"` to `mimeToExt`.

### 4. Thumbnail generation (core change)

Neither AVIF nor HEIC/HEIF can be decoded by the pure-Go image pipeline
(`disintegration/imaging` + `x/image/webp`). Route them through ffmpeg via a
generic fallback rather than special-casing AVIF by MIME type. This also
upgrades HEIC/HEIF, which currently produce no thumbnail.

New helpers (in `internal/media/video.go` or a new `ffmpeg_image.go`):

- `GenerateImageThumbnailFFmpeg(ctx, srcPath, dstPath, maxDimension) error` —
  shells out to ffmpeg for a single scaled JPEG frame, reusing the same scale
  filter (`scale='min(N,iw)':'min(N,ih)':force_original_aspect_ratio=decrease`)
  and `.jpg` output conventions as the video thumbnail path. Bounded by the
  existing 30s context timeout.
- `ImageDimensionsFFprobe(ctx, path) (width, height int, err error)` — reads
  pixel dimensions via ffprobe for formats the pure-Go decoder can't handle.

Rework `processImage` into a fallback chain (it must take a `context.Context`,
threaded from `Process`, which already has one):

1. Try pure-Go `GenerateImageThumbnail` (jpeg/png/gif/webp). On success, set
   `Width`/`Height` and `HasThumbnail = true`.
2. On failure, fall back to ffprobe for dimensions and ffmpeg for the
   thumbnail. On success, set `Width`/`Height` and `HasThumbnail = true`.
3. If ffmpeg also fails, keep the original with no thumbnail (today's
   generic-preview fallback). Best-effort dimensions from whichever probe
   succeeded.

EXIF/capture-time extraction (`ImageCapturedAt`) is unchanged.

### 5. Data flow

Unchanged end to end: sniff → allowlist check → hash → move to originals →
derive thumbnail/dimensions. AVIF/HEIC/HEIF now take the ffmpeg branch in the
thumbnail step.

### 6. Error handling

ffmpeg and ffprobe failures are non-fatal, matching current behavior: the
media item is ingested regardless; only the thumbnail/dimensions are best
effort. Both calls run under the existing 30s context timeout.

## Testing

- Sniff:
  - Synthetic `ftyp` fixtures assert `avif` and `avis` major brands →
    `image/avif`.
  - An AVIF fixture with major brand `avif` and a `mif1` compatible brand
    asserts resolution to `image/avif` (not `image/heif`), locking in the
    ordering constraint.
- Config: `image/avif` present in the default allowlist.
- Extension map: `image/avif` → `.avif`.
- Thumbnail: ffmpeg-dependent tests follow the existing video-test pattern
  (skip when ffmpeg is unavailable, or use a real fixture) so CI without
  ffmpeg is unaffected.

## Rejected alternatives

- **AVIF-only routing** (special-case `image/avif` through ffmpeg, leave HEIC
  untouched): more MIME special-casing and leaves HEIC without thumbnails.
  Rejected in favor of the generic fallback.
- **Go AVIF decoder library**: requires CGO (libaom/dav1d), violating
  `CGO_ENABLED=0`. Rejected.
