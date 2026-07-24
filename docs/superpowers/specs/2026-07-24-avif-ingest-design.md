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
  ffmpeg 6.1.2 links libdav1d, which is the AV1 decoder needed for AVIF still
  images. This is the mechanism used for the ffmpeg image fallback.
- The pinned ffmpeg build's actual decode capability must be validated, not
  assumed. AVIF requires the AV1 decoder (dav1d); HEIC/HEIF additionally
  require an HEVC decoder plus HEIF demuxing, which may or may not be present
  in the same build. See the validation step under Testing.
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
(`disintegration/imaging` + `x/image/webp`). Route decode failures through
ffmpeg via a generic fallback rather than special-casing AVIF by MIME type.

Scope of the fallback (explicit): the fallback triggers on *any* pure-Go
decode failure, not only AVIF/HEIC/HEIF. In practice this means a malformed
JPEG/PNG/GIF/WebP that passes magic-byte sniffing but fails pure-Go decoding
will now also be handed to ffmpeg. This is intentional and acceptable, and it
introduces no new resource-exposure class: media processing already runs
inline in the tusd `post-finish` hook (`Server.handlePostFinishHook` →
`processor.Process`), and that path already shells out to ffmpeg/ffprobe for
*every* video upload. Note this hook path is NOT bounded by the per-IP
`uploadConcurrency` semaphore — that only guards guest-facing tus `PATCH`
transport in the proxy — so the fallback inherits the same (already-accepted)
inline-processing tradeoff as video, not a stricter one. The fallback can only
ever add a thumbnail the pure-Go path couldn't produce.

Graceful degradation: the generic fallback upgrades whatever the pinned
ffmpeg can actually decode and leaves everything else at today's behavior
(original kept, no thumbnail). AVIF thumbnails are the committed deliverable
(dav1d is present); HEIC/HEIF thumbnails are an expected bonus *contingent on
the pinned ffmpeg decoding them* — to be confirmed by the validation step,
not assumed here.

Probing — reuse, don't reinvent: the existing ffprobe-based media probe
(`ProbeVideo`) already extracts container `creation_time` and carries the
rotation-correction machinery (`displayDimensions`). The image fallback
reuses this probe rather than a naive dimensions-only read, for two reasons:

- Orientation consistency (requirement, mechanism to be validated): the
  pure-Go path stores display-oriented dimensions (post
  `imaging.AutoOrientation`), and ffmpeg auto-rotates the thumbnail it
  renders. Stored `Width`/`Height` MUST therefore reflect display orientation
  consistent with the generated thumbnail, or we reproduce the wrong-aspect-
  tile bug `displayDimensions` was written to prevent. Caveat that the plan
  must resolve: `displayDimensions` reads only *video* display-matrix
  rotation (`side_data_list[].rotation` / the `rotate` tag). Still-image
  orientation in AVIF/HEIC lives in ISOBMFF `irot`/`imir` (or embedded EXIF),
  which the pinned ffprobe may NOT surface through those fields. So reuse
  alone does not *guarantee* consistency. The validation step must confirm,
  for a rotated still, that ffprobe reports rotation via the fields
  `displayDimensions` reads. If it does not, the implementation must obtain
  display-oriented dimensions another way that is consistent with what ffmpeg
  renders (e.g. read the dimensions of the ffmpeg-decoded/auto-rotated frame,
  or read EXIF orientation) — so the guarantee holds by construction, not by
  assumption. The rotated-fixture test (see Testing) enforces this either way.
- Capture time (best-effort, usually absent for these formats):
  `ImageCapturedAt` uses goexif, which cannot parse ISOBMFF (AVIF/HEIC/HEIF),
  so those files get no `CapturedAt` today. Since the fallback already
  invokes ffprobe, populate `CapturedAt` from the probe's container
  `creation_time` when present. Be realistic about yield: phone AVIF/HEIC
  typically store capture time in embedded EXIF, not the container-level
  `creation_time` tag `ProbeVideo` reads, so for the common case this stays
  nil. It is a cheap, harmless win for the files that do carry it, not parity
  with the video path.

Generalize the probe so it reads as a media probe rather than a video-only
one (e.g. rename/extend `ProbeVideo`, or share its internal parse); the image
fallback ignores the duration field.

New thumbnail helper (in `internal/media/video.go` or a new `ffmpeg_image.go`):

- `GenerateImageThumbnailFFmpeg(ctx, srcPath, dstPath, maxDimension) error` —
  shells out to ffmpeg for a single scaled JPEG frame, reusing the same scale
  filter (`scale='min(N,iw)':'min(N,ih)':force_original_aspect_ratio=decrease`)
  and `.jpg` output conventions as the video thumbnail path. Bounded by the
  existing 30s context timeout.

Rework `processImage` into a fallback chain (it must take a `context.Context`,
threaded from `Process`, which already has one):

1. Try pure-Go `GenerateImageThumbnail` (jpeg/png/gif/webp). On success, set
   `Width`/`Height` and `HasThumbnail = true`, and keep the existing
   `ImageCapturedAt` (goexif) capture-time path.
2. On failure, probe via ffprobe (display-oriented dimensions + best-effort
   capture time) and generate the thumbnail via ffmpeg. On success, set
   `Width`/`Height`, `HasThumbnail = true`, and `CapturedAt` from the probe
   when the goexif path yielded none.
3. If ffmpeg also fails, keep the original with no thumbnail (today's
   generic-preview fallback). Populate best-effort dimensions/capture time
   from whichever probe succeeded.

### 5. Data flow

Unchanged end to end: sniff → allowlist check → hash → move to originals →
derive thumbnail/dimensions. AVIF/HEIC/HEIF now take the ffmpeg branch in the
thumbnail step.

### 6. Error handling

ffmpeg and ffprobe failures are non-fatal, matching current behavior: the
media item is ingested regardless; only the thumbnail/dimensions are best
effort. Both calls run under the existing 30s context timeout.

## Testing

- Environment validation (do this first, it gates the framing above): inside
  the exact pinned runtime image (`ffmpeg=6.1.2-r2`), confirm
  `ffmpeg -i sample.avif -frames:v 1 out.jpg` produces a JPEG. Repeat with a
  HEIC sample to determine whether the HEIC/HEIF bonus actually materializes
  in this build. Record the result; if HEIC does not decode, that is not a
  failure of this change (graceful degradation covers it) but the spec's
  HEIC framing should be marked confirmed-absent. Also validate orientation:
  for a rotated AVIF/HEIC still, check whether `ffprobe` reports the rotation
  through the fields `displayDimensions` reads (`side_data_list[].rotation`
  or the `rotate` tag). If it does not, the plan must adopt the alternative
  dimension source described in section 4 so stored dims stay consistent with
  the auto-rotated thumbnail.
- Sniff:
  - Synthetic `ftyp` fixtures assert `avif` and `avis` major brands →
    `image/avif`.
  - An AVIF fixture with major brand `avif` and a `mif1` compatible brand
    asserts resolution to `image/avif` (not `image/heif`), locking in the
    ordering constraint.
- Config: `image/avif` present in the default allowlist.
- Extension map: `image/avif` → `.avif`.
- Thumbnail/probe (ffmpeg-dependent, follow the existing video-test pattern:
  skip when ffmpeg is unavailable, or use a real fixture, so CI without ffmpeg
  is unaffected):
  - An AVIF fixture yields `HasThumbnail = true` and non-zero dimensions.
  - A rotated AVIF (or HEIC) fixture yields dimensions whose orientation
    matches the generated thumbnail (guards the display-orientation
    consistency requirement).
  - Capture time is populated from the probe for a fixture whose container
    carries a creation time.

## Rejected alternatives

- **AVIF-only routing** (special-case `image/avif` through ffmpeg, leave HEIC
  untouched): more MIME special-casing, and it forgoes the HEIC/HEIF upgrade
  that the generic fallback yields for free wherever the pinned ffmpeg can
  decode those formats. Rejected in favor of the generic fallback, which
  degrades gracefully for formats ffmpeg cannot decode.
- **Naive `ImageDimensionsFFprobe` (raw dimensions only)**: rejected because
  raw container dimensions ignore orientation and can disagree with the
  auto-rotated thumbnail. The fallback instead reuses the rotation-aware media
  probe and, where ffprobe does not surface a still's rotation, falls back to
  a dimension source consistent with what ffmpeg renders (see section 4).
- **Go AVIF decoder library**: requires CGO (libaom/dav1d), violating
  `CGO_ENABLED=0`. Rejected.
