# Task 6 Report: Real AVIF Runtime Validation + Portrait Fixture

## Fixture

- Added `backend/internal/media/testdata/sample_portrait.avif` from `K:\Users\Belgr\Desktop\wg\river-trees_410324-28.avif`.
- Fixture size: 74,065 bytes.
- Local ffprobe fixture dimensions: `av1,418,626`.
- Generated 100px-max thumbnail dimensions captured for reporting: `67,100`, confirming portrait output.

## Test Code Added

Added to `backend/internal/media/ffmpeg_image_test.go`:

```go
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

## Spec Note Added

Appended under `docs/superpowers/specs/2026-07-24-avif-ingest-design.md` Testing / Environment validation:

```md
Task 6 v2 validation result: AVIF decode CONFIRMED in the pinned
`ffmpeg=6.1.2-r2` with a real sample (`DECODE_OK`; ffprobe `av1 418x626`).
HEIC/HEIF decode NOT tested (no HEIC sample available); graceful degradation applies. No irot-rotated fixture was available, so the rotation-swap path is covered by deterministic `TestOrientImageDimensions` while `TestProcessAVIF_RealPortraitFixture` confirms real-AVIF portrait handling end-to-end.
```

## Verification

Fixture check:

```powershell
ffprobe -v error -show_entries stream=codec_name,width,height -of csv=p=0 "backend\internal\media\testdata\sample_portrait.avif"
Get-Item "backend\internal\media\testdata\sample_portrait.avif" | Select-Object Name,Length
```

Output:

```text
av1,418,626

Name                 Length
----                 ------
sample_portrait.avif  74065
```

Thumbnail dimension check for the test's 100px max size:

```powershell
ffmpeg -v error -y -i "backend\internal\media\testdata\sample_portrait.avif" -vf "scale='min(100,iw)':'min(100,ih)':force_original_aspect_ratio=decrease" -frames:v 1 $tmp
ffprobe -v error -show_entries stream=width,height -of csv=p=0 $tmp
```

Output:

```text
67,100
```

Focused new test, from `backend` with `CGO_ENABLED=0` and portable Go. The required command was run, then re-run with `-count=1` for uncached final evidence:

```powershell
$env:CGO_ENABLED='0'
& "$env:TEMP\go-1.25.0-portable\go\bin\go.exe" test ./internal/media/ -count=1 -run 'TestProcessAVIF_RealPortraitFixture' -v
```

Output:

```text
=== RUN   TestProcessAVIF_RealPortraitFixture
--- PASS: TestProcessAVIF_RealPortraitFixture (0.22s)
PASS
ok      event-gallery/backend/internal/media    0.739s
```

Required regression slice, from `backend` with `CGO_ENABLED=0` and portable Go. The required command was run, then re-run with `-count=1` for uncached final evidence:

```powershell
$env:CGO_ENABLED='0'
& "$env:TEMP\go-1.25.0-portable\go\bin\go.exe" test ./internal/media/ -count=1 -run 'TestOrientImageDimensions|TestProbeImageAndThumbnail_AVIF|TestProcessor_' -v
```

Output:

```text
=== RUN   TestOrientImageDimensions
=== RUN   TestOrientImageDimensions/landscape_unrotated
=== RUN   TestOrientImageDimensions/portrait_unrotated
=== RUN   TestOrientImageDimensions/rotated_to_portrait
=== RUN   TestOrientImageDimensions/rotated_to_landscape
=== RUN   TestOrientImageDimensions/square
=== RUN   TestOrientImageDimensions/missing_coded
=== RUN   TestOrientImageDimensions/missing_thumb_keeps_coded
--- PASS: TestOrientImageDimensions (0.00s)
    --- PASS: TestOrientImageDimensions/landscape_unrotated (0.00s)
    --- PASS: TestOrientImageDimensions/portrait_unrotated (0.00s)
    --- PASS: TestOrientImageDimensions/rotated_to_portrait (0.00s)
    --- PASS: TestOrientImageDimensions/rotated_to_landscape (0.00s)
    --- PASS: TestOrientImageDimensions/square (0.00s)
    --- PASS: TestOrientImageDimensions/missing_coded (0.00s)
    --- PASS: TestOrientImageDimensions/missing_thumb_keeps_coded (0.00s)
=== RUN   TestProbeImageAndThumbnail_AVIF
--- PASS: TestProbeImageAndThumbnail_AVIF (0.28s)
=== RUN   TestProcessor_ProcessImage
--- PASS: TestProcessor_ProcessImage (0.02s)
=== RUN   TestProcessor_ProcessAVIF
--- PASS: TestProcessor_ProcessAVIF (0.29s)
=== RUN   TestProcessor_RejectsDisallowedType
--- PASS: TestProcessor_RejectsDisallowedType (0.01s)
=== RUN   TestProcessor_RejectsUnknownContent
--- PASS: TestProcessor_RejectsUnknownContent (0.01s)
PASS
ok      event-gallery/backend/internal/media    1.133s
```

## Files Changed

- `backend/internal/media/testdata/sample_portrait.avif`
- `backend/internal/media/ffmpeg_image_test.go`
- `docs/superpowers/specs/2026-07-24-avif-ingest-design.md`
- `.superpowers/sdd/task-6-report.md`

## Self-Review Findings

- The new test uses the existing package-level `requireFFmpeg(t)` helper and adds no imports.
- The fixture is present, real, decodable locally, and portrait coded/display dimensions are 418x626.
- The test passed and did not skip in this environment.
- The spec update records the controller-confirmed pinned-ffmpeg AVIF decode result and the HEIC/irot caveats requested by the v2 brief.

## Concerns

- HEIC/HEIF decode remains untested because no HEIC sample was available.
- No irot-rotated AVIF fixture was available; rotation-swap coverage remains the deterministic `TestOrientImageDimensions` unit test.
- Broader pre-existing unrelated Windows failures mentioned in the brief were not investigated because this task only required the scoped media tests above.