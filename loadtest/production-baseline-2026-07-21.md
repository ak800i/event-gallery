# Production upload battle baseline — 2026-07-21

Target: production deployment (origin intentionally omitted)

Configuration observed before the run:

- uploads enabled;
- max upload size: 5 GiB;
- per-source-IP concurrent PATCH limit: 50;
- Cloudflare HTTP/2 edge in front of the app;
- app and tusd healthy, zero restarts;
- baseline memory: app 20 MiB, tusd 43 MiB.

## Results

| Stage | Successful | Logical traffic | Elapsed | Aggregate throughput | PATCH p95 | Retries |
|---|---:|---:|---:|---:|---:|---:|
| forced resume, 1 × 16 MiB | 1/1 | 16 MiB | 6.15s | 2.60 MiB/s | 2.23s | 0 |
| 10 × 5 MiB | 10/10 | 50 MiB | 8.67s | 5.77 MiB/s | 8.24s | 0 |
| 40 × 5 MiB | 40/40 | 200 MiB | 22.61s | 8.85 MiB/s | 21.50s | 0 |
| 60 × 5 MiB | 60/60 | 300 MiB | 48.72s | 6.16 MiB/s | 44.60s | 19 |
| 2 × 250 MiB | 2/2 | 500 MiB | 73.89s | 6.77 MiB/s | 6.26s | 0 |

Total logical/wire upload traffic: **1,066 MiB**. All **113** unique valid PNGs completed the full tus/hook/hash/database/media-storage pipeline.

## Resume and overload behavior

The resume stage uploaded the first 8 MiB chunk, issued a separate HEAD request,
verified the durable offset, and resumed on fresh TLS connections. The final file
was processed and visible at exactly 16,777,216 bytes.

The 60-way stage intentionally crossed the configured limit of 50. App logs
reported exactly **19 HTTP 429** responses. The harness reported exactly **19
HEAD/retry reconciliations**. All 60 uploads completed; there were no 5xx
responses. Some rejections closed before the generator finished streaming, so
they appeared as transport retries in client metrics and as 429s in server logs.

## Large-file behavior

Two 250 MiB uploads used 64 total 8 MiB PATCH requests, safely below Cloudflare's
per-request limit. Both completed without retry and were processed into gallery
items at exactly 262,144,000 bytes. This covered sustained tunnel throughput,
tusd writes, whole-file SHA-256, cross-volume copy/move behavior, and SQLite
insertion.

## Resource observations

- After 10-way processing: app 110 MiB; tusd 44 MiB.
- After 40-way processing: app 312 MiB; tusd 45 MiB.
- After 60-way processing: app 616 MiB; both healthy.
- Immediately after the two 250 MiB files: app cgroup usage about 2,073 MiB.
- After 60 seconds: app usage 1,064 MiB, composed of **1,045 MiB filesystem
  cache and 19 MiB RSS**. This confirms the high reading was transient page
  cache from hashing/copying rather than retained Go heap.
- Docker host memory available: 17.4 GiB.
- Final app/tusd state: healthy, zero restarts, zero upload-processing errors,
  zero recovered panics, zero 5xx observed.

## Cleanup

All 113 `wg-battle-*` items were moved to Admin Trash through the authenticated,
CSRF-protected admin API. The public gallery was verified to contain zero active
battle-test items.

Trash is a soft delete. The approximately 1.04 GiB of originals still occupies
media storage. Permanent removal requires a separately approved purge feature or
offline maintenance procedure; the test did not directly edit live SQLite or
media files.

## Interpretation

The current production deployment cleanly handles 40 simultaneous uploads from
one shared source IP. At 60, the configured concurrency guard activates as
expected, and standard tus offset reconciliation/retry successfully recovers.
Sustained large uploads remain below Cloudflare body limits through 8 MiB chunks
and complete without error. The practical bottleneck observed was aggregate WAN
throughput (roughly 6–9 MiB/s), not app/tusd stability.
