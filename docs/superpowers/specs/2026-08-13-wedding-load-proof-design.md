# Wedding Load Proof — Design

Date: 2026-08-13
Status: approved for planning
Target: `event-gallery` at `main@99e06d7`, deployed on this host as Portainer stack `wedding-gallery`

## Purpose

Demonstrate that the deployed instance publishes a typical wedding's uploads,
intact, within an acceptable time, and quantify how much headroom exists beyond
that. The result must be a defensible number, not a green checkmark.

The existing harness cannot do this. Its success oracle is
`result.success = offset == size` — transport only. That is the same check that
reported success while the July incident destroyed seventeen completed uploads.
Every stage below therefore depends on building a real completion oracle first.

## Environment under test

Established by inspection, not assumption:

| Fact | Value |
|---|---|
| Host | AMD Ryzen 9 3900X, 12 physical / 24 logical cores, 32 GB RAM |
| Data volume | `D:` — NVMe SSD, 363 GB free of 838 GB |
| Media and tus dirs | Both on `D:`, via Windows bind mounts into Linux containers |
| Deployed code | `main@99e06d7` — verified live via `/readyz`, `/api/uploads/status`, and a 400 on `X-HTTP-Method-Override` |
| Stack | Git-backed Portainer stack, id 8; tunables are `${VAR:-default}` sourced from `stack.env` |
| Neighbours | Immich + ML, six Postgres, Portainer, three cloudflared — idle during the run |
| Accepted types | JPEG, PNG, WebP, GIF, **HEIC**, **HEIF**, AVIF; MP4, MOV, WebM |
| `MAX_UPLOAD_BYTES` | 5 GiB |
| `UPLOAD_CONCURRENCY_PER_IP` | 50 — the generator is one IP, which correctly models venue NAT |

Both volumes sharing one disk matters: every upload transiently occupies roughly
twice its size until cleanup removes the source.

## Definition of success

An upload counts only if **all five** hold:

1. Transport completed — `offset == size`.
2. `/api/uploads/status` reaches `published` or `duplicate`.
3. `GET /api/media/{id}/download` returns bytes whose SHA-256 equals what was sent.
4. The item appears in `/api/gallery`.
5. Its tus source is **gone** from the upload directory — proving cleanup ran.

The SHA-256 is also sent as tus metadata, so the server independently verifies
integrity through its own `checksum_mismatch` path. Corruption is then caught
from both ends.

Run-level assertions:

- Zero unexpected 5xx. Documented `503` and `429` carrying `Retry-After` are
  counted separately as backpressure, not failure — but their uploads must still
  publish before the deadline.
- Zero `ERROR` log lines. This became a valid assertion only after the fix that
  stopped a clean shutdown emitting one; since nothing is restarted here, any
  `ERROR` is real.
- Free space on `D:` returns to baseline after cleanup.

Criterion 5 is the one that would have caught the original incident, and it is
the reason transport success is never sufficient on its own.

## Corpus

5,000 items, ≈108 GB:

| Class | Count | Size each | Total |
|---|---:|---:|---:|
| Photos | 4,600 | ~6 MB | ~27.6 GB |
| Videos | 400 | ~200 MB | ~80 GB |

Types span JPEG, PNG, WebP, HEIC, MP4 and MOV, so the run doubles as a
media-type matrix. HEIC is deliberately included: iPhones emit it by default, it
is accepted today, but the `heif-preview` helper is deferred to plan 2 — so the
run must report publish success *and* thumbnail success per type. A HEIC item
that publishes without a preview is a finding worth having before the wedding,
not during it.

Base assets are generated once at true phone dimensions (4000×3000) with real
entropy. Each upload streams `base asset + unique trailing marker`. Decoders
ignore bytes after JPEG's EOI, so derivation stays genuinely expensive while
every file gets a distinct SHA-256 and avoids collapsing into the dedupe path.
Nothing is materialised: the hash is computed while streaming.

This matters because the current generator emits a small PNG followed by zero
padding, which makes thumbnailing artificially cheap. A pass on that payload
would prove nothing about the real bottleneck.

## Central hypothesis

The system is expected to be **I/O-bound on the bind mount, not CPU-bound**.

Publishing 108 GB requires roughly 324 GB of traffic across that mount — read to
hash, read to copy, write the copy — before any derivation. At 200 MB/s
effective that is ~27 minutes; at 500 MB/s, ~11. Meanwhile derivation at 16
workers should clear 5,000 items in well under ten minutes.

If this holds, raising worker counts past a threshold will not help and may hurt
through contention. Calibration exists to settle this before the main run, and
the answer determines whether the recommended wedding-day configuration is
"more workers" or "fewer, larger".

## Configuration change

Applied before testing and intended to persist, per explicit approval:

| Variable | From | To | Reason |
|---|---:|---:|---|
| `MEDIA_PROCESSING_WORKERS` | 2 | 16 | Derivation is ~1 core per item; 16 of 24 logical cores |
| `UPLOAD_DURABILITY_WORKERS` | 2 | 8 | fsync-bound, not CPU-bound; caps simultaneous completions before 503 |

`UPLOAD_CONCURRENCY_PER_IP` stays at 50 — it is the transport ceiling and it
accurately models a venue behind one NAT.

These are `${VAR:-default}` in the compose file, so the change belongs in the
Portainer stack environment rather than in the file. The authoritative store is
`portainer.db`; `stack.env` is generated from it and contains secrets, so it is
edited without being read. A change made only to `stack.env` would be reverted
by the next Portainer redeploy, so the same two values must also be set in the
Portainer stack environment for the change to be genuinely permanent.

Both values are subject to calibration confirming they help; if the hypothesis
above holds, the recommendation may be lower.

## Stages

| # | Stage | Shape | Answers |
|---|---|---|---|
| 0 | Baseline and smoke | Record free space, gallery count, log counts. Verify the quiescence witness rejects a fresh rowless partial. | Clean comparison; confirms the Critical fix is live in production |
| 1 | Calibration | ~100 items across all types, measured at concurrency 1 and 16 | Per-item derive cost, bind-mount throughput, scaling efficiency — settles the hypothesis |
| 2 | Wedding | 5,000 items / 108 GB, Poisson arrivals with occasional clustering, direct to container | The proof, and the drain time |
| 3 | Overload | 3× arrival rate, then a thundering herd | Headroom; where backpressure begins and whether it recovers |
| 4 | Tunnel | ~300 items via Cloudflare | The 75s durability budget against the ~100s edge timeout |
| 5 | Cleanup | Purge and verify | Bytes and rows actually reclaimed |

Stage 0's smoke check is cheap and directly exercises the most serious defect
found in review: a rowless, sidecar-less partial must not be adopted until it is
older than the retention policy and unchanged across two reconcile passes.

Arrival modelling follows the stated reality — guests do not coordinate, though
a few cluster — so stage 2 uses a Poisson process. Stage 3's herd is strictly
more stressful, so passing it implies the realistic case a fortiori.

## Pass thresholds

- Stage 2: 100% of the five-criteria oracle. Backlog fully drained **within one
  hour of the last upload**. Zero unexpected 5xx. Zero `ERROR` lines.
- Stage 3: no data loss and no permanent failure. Backpressure is expected and
  acceptable; every accepted upload must still publish.
- Stage 4: no request exceeds the edge timeout; the final PATCH stays inside the
  75s budget.

A missed threshold is reported as a measured number with the bottleneck
attributed — not retried with adjusted settings until it passes. Tuning is
applied once, up front, on calibration evidence; tuning afterwards to reach a
target would make the result unfalsifiable.

## Instrumentation

The reconciler already logs `INFO ingest queue` once per pass with counts by
status, oldest pending age, and maximum processing failures. That is the drain
curve, sampled every 15 seconds, for free — it is read directly rather than
inferred.

Alongside it: `docker stats` for app and tusd, free space on `D:`, log-line
counts by level, and per-item timings split into transport, time-to-published,
and verification.

## Cleanup

Test items are prefixed `event-gallery-battle-`. Cleanup soft-deletes through
the existing admin flow, then forces permanent deletion, then verifies by
free-space delta on `D:` rather than by assumption. Purge interacts with the
storage-health gate and the in-flight upload-job guard, so a health circuit that
is open will correctly refuse to purge; that is a condition to check, not a bug
to work around.

## Risks

- **Disk exhaustion.** Peak need ≈118 GB against 363 GB free. The harness aborts
  if free space falls below 50 GB.
- **Live gallery pollution.** The gallery is near-empty, so anything left behind
  is conspicuous. Mitigated by the name prefix and verified cleanup.
- **Cloudflare protections.** Stage 4 stays small and slow.
- **Worker oversubscription.** Sixteen concurrent ffmpeg processes on a shared
  box; `MEDIA_TOOL_MEMORY_BYTES` enforcement is deferred to plan 2, so memory is
  observed rather than enforced.

## Non-goals

Excluded by instruction: restart, crash-recovery, and disk-failure testing.

Also out of scope: multi-IP behaviour, since one generator is one public IP —
which happens to model venue NAT accurately rather than being a gap.

The free-space admission gate will not trigger naturally, since 245 GB remains
free at peak. If it is to be proven, it needs a targeted micro-test that raises
`INGEST_MIN_FREE_BYTES` temporarily rather than filling the disk.
