# Production tus battle test

Dependency-free Python 3 harness for the deployed wedding-gallery upload path.
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
cd /path/to/wedding-gallery
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
cd /path/to/wedding-gallery
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

- `wedding-gallery-app-1` and `wedding-gallery-tusd-1` remain healthy;
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

Successful test files are named `wg-battle-*.png`. Move all published or
pending battle items to Admin Trash with:

```sh
BASE_URL=https://your-gallery.example \
ADMIN_PASSWORD='your-admin-password' \
python loadtest/cleanup_battle.py
```

This uses the normal authenticated admin and CSRF flow. It is a **soft delete**:
test items disappear from the public gallery but their originals remain on disk.
The application currently has no permanent-purge API. Permanently deleting the
test originals and database rows requires a separately approved maintenance
procedure/feature; do not edit the live SQLite database while the stack is running.

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
