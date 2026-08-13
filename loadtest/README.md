# Production tus battle test

Dependency-free Python 3 harness for the deployed event-gallery upload path.
It sends real tus `POST`, `PATCH`, `HEAD`, and cleanup `DELETE` requests through
Cloudflare to the public hostname. Payloads are valid PNG files generated as a
small real PNG followed by streamed padding and a unique marker, so successful
uploads exercise tusd, hook processing, whole-file hashing, SQLite insertion,
and permanent media storage without keeping large fixtures in RAM or on disk.

## Safety

This is production load. Run it during a quiet window and monitor Portainer.
The guarded staged runner sends approximately **1.04 GiB** total:

| Stage | Load | Purpose |
|---|---:|---|
| smoke-resume | 1 × 16 MiB | Two 8 MiB chunks; forced HEAD offset check before resume |
| concurrent-10 | 10 × 5 MiB | Normal small-event concurrency |
| concurrent-40 | 40 × 5 MiB | Near configured shared-IP limit |
| boundary-60 | 60 × 5 MiB | Deliberately crosses the 50 concurrent PATCH limit; retries must recover |
| large-2x250 | 2 × 250 MiB | Sustained Cloudflare/tunnel/disk/hash/copy behavior |

One generator appears as one public IP, which intentionally models many guests
sharing venue Wi-Fi/NAT. Do not distribute generators unless you explicitly
want to test multiple source IPs.

Default pass thresholds:

- required stage success rate: 100%, except 98% at 40 and 90% at 60;
- HTTP 5xx rate no more than 2%;
- every retry reconciles the durable tus offset with `HEAD`;
- any stage failure stops `run_staged.sh` before the next stage.

## Run one stage first

```sh
cd /path/to/repository
python loadtest/tus_battle.py \
  --stage smoke-resume \
  --count 1 \
  --size-mb 16 \
  --chunk-mb 8 \
  --resume \
  --min-success-rate 1 \
  --json-out loadtest/results/01-smoke-resume.json
```

## Run the full staged battle

```sh
cd /path/to/repository
I_UNDERSTAND_PRODUCTION_LOAD=YES \
BASE_URL=https://your-gallery.example \
sh loadtest/run_staged.sh
```

The harness keeps only a 64 KiB zero block plus active TLS buffers in memory.
`loadtest/results/state.json` records upload URLs and last observed offsets for
forensics; each stage report contains success rate, retries, status counts,
throughput, patch latency percentiles, and failed filenames.

## Monitor between stages

In Portainer, check:

- the `app` and `tusd` services remain healthy;
- restart counts stay zero;
- app/tusd logs contain no processing errors, panics, or 5xx responses;
- app memory is interpreted using RSS versus filesystem cache (large hashing
  and cross-volume copies can temporarily charge page cache to the container);
- host storage and network utilization remain acceptable.

At the intentional 60-way boundary, app logs may show 429 responses. The stage
passes only if clients reconcile with HEAD/retry and all required uploads finish.
When a server rejects before consuming a streamed body, the generator may record
a transport retry rather than the 429 itself; correlate retry count with app logs.

## Clean up

Successful test files are named `event-gallery-battle-*.png`. Move all published or
pending battle items to Admin Trash with:

```sh
BASE_URL=https://your-gallery.example \
ADMIN_PASSWORD='your-admin-password' \
python loadtest/cleanup_battle.py
```

This uses the normal authenticated admin and CSRF flow. It first performs a
**soft delete**: test items disappear publicly, then the normal trash-retention
janitor purges them. To reclaim space immediately, select them in Admin Trash
and use **Delete permanently**. Do not edit live SQLite or media files directly.

## Individual controls

```text
--count N                 concurrent unique uploads
--size-mb N               logical size of each valid generated PNG
--chunk-mb N              tus PATCH size (keep <= Cloudflare request limit)
--resume                  force HEAD/resume after the first chunk of upload 0
--timeout SECONDS         per-request timeout (default 600)
--min-success-rate RATE   stage threshold, 0..1
--max-5xx-rate RATE       stage threshold, 0..1
--state PATH              forensic upload URL/offset state
--json-out PATH           stage metrics report
```

## Wedding load proof

A second, separate harness under `loadtest/wedding/`. The battle test above asks
whether uploads succeed; this one asks whether they were *published*, which is
not the same question.

### Why transport success is not enough

The July incident reported every upload successful while seventeen guests' files
were destroyed, because the only thing checked was `offset == size`. An item here
counts as verified only when all five criteria hold:

1. `transport_ok` — the tus upload completed;
2. `published_ok` — the server reports a terminal success state for the id;
3. `digest_ok` — the stored original is re-downloaded and its SHA-256 matches
   what was sent;
4. `gallery_ok` — the filename appears in the public gallery listing;
5. `source_gone` — the tus source and its `.info` sidecar have been removed.

Criterion 5 is the one that would have caught the incident. Any single failure
fails the whole stage.

### Proving criterion 5 was watching something

`source_gone` is satisfied by an *absence*, so an `--upload-dir` pointed at the
wrong host directory — one that exists but is not the app's tus data dir —
reports every item clean and proves nothing. While uploads are in flight the
harness therefore samples that directory four times a second and counts tusd
`.info` sidecars, reporting the result as `source_observation`. A stage that
never saw a single source cannot pass: the criterion is unverifiable, and an
unverifiable criterion has not been met.

It cannot false-fail a run that legitimately cleaned up between samples, because
`max_seen` latches — one sighting anywhere in the phase is enough, and sampling
stops when the last upload returns, not after the queue drains. `finalize` fills
in a *failing* observation when the report carries none, so skipping the probe
fails closed rather than silently reverting to the old behaviour.

### Running a stage

```powershell
pwsh loadtest/run_wedding.ps1 -Stage smoke
```

The script builds base assets on the host, runs the stage in a sidecar, collects
the app's logs, and prints the verdict. Stages are `smoke`, `calibrate-serial`,
`calibrate-parallel`, `wedding`, `overload`, `herd`, and `tunnel`.

### Why a sidecar container

The app publishes no host port; it is reachable only on the Docker network
`wedding-gallery_edge` as `http://app:8080`. The harness therefore runs as a
throwaway `python:3.13-slim` container joined to that network, with the repo and
the upload/media directories bind-mounted read-only. It is stdlib-only, so the
image needs no `pip install`.

Base assets are generated **on the host**, once, into `loadtest/assets/` (git
ignored) because ffmpeg is not in the sidecar image. `build_assets` is
idempotent, so later runs only read them. Delete the directory to regenerate;
it is idempotent by existence alone, so it will happily reuse a truncated asset
left by an interrupted run.

### Why `finalize` decides, not the stage

The sidecar has no Docker socket, so it cannot read the app's logs. The report it
writes is marked `"provisional": true` and its `passed` is not authoritative.
`finalize` then runs on the host, merges in the app's own log-derived evidence,
and recomputes the verdict. Both call the same `runner.decide_passed`, so the two
cannot disagree about what passing means.

Logs are collected with `docker logs`, never `docker compose logs`. Compose
prefixes every line with `app-1  | `, so nothing parses as JSON, the ERROR count
comes back zero and the stage passes *vacuously* against a log full of errors.
`finalize` guards against that anyway by comparing parsed lines against raw
lines and refusing to certify a log it could not read. The parser is deliberately
not made prefix-tolerant: compose also multiplexes tusd and cloudflared, whose
errors are not the app's.

### Abort floor

Peak need is roughly 118 GB against 363 GB free. Every arrival re-checks free
space on the upload volume and the stage aborts below **50 GB**. The guard
latches: once tripped, the remaining arrivals are skipped rather than sent, and
the report records the abort and fails.

### Reading a report

`loadtest/results/<stage>.json`. Beyond the verdict:

- `backpressure` counts 429/503 seen on the tus path and
  `verification_backpressure` those seen while verifying. Neither fails a stage —
  they are the server pacing a single-IP client, which is what those limiters are
  for. `unexpected_5xx` counts every other 5xx and does fail it.
- `arrival_lag` is how far behind its scheduled arrival each upload actually
  started. Concurrency is a ceiling on offered load, not a target: lag near zero
  means the schedule set the pace, lag that grows means the pool did and the
  stage ran slower than its nominal arrival rate.
- `queue_summary.suspect_gaps_seconds` flags holes in the app's own drain curve.
  A hole is not proof of an idle queue: the queue summarizer is silent while
  empty, but its failure path logs `WARN`, not `ERROR`, so a summarizer that fell
  over leaves the zero-ERROR criterion green while blanking the curve.
- `by_type` reports published counts and thumbnail coverage per MIME type. A
  published item with no thumbnail is a reported finding, not a failure.
- `timings.to_published_*` measure upload finish → first *observed* terminal
  state. Observation costs up to one 10 s poll interval plus one sweep of the
  batched status endpoint, so these are upper bounds, never optimistic ones.
- `source_observation` is the criterion-5 evidence above. `gallery_unavailable`
  is non-empty when the gallery listing could not be read at all, in which case
  every item fails criterion 4 rather than the run being thrown away.
- `<stage>-uploads.json` is written the moment the upload phase ends, before the
  much longer verification pass. If verification dies, the upload evidence —
  backpressure, 5xx, arrival lag, source observation — survives.

### Tests

```sh
python -m unittest discover -s loadtest/wedding/tests -t .
```

