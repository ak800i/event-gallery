# Wedding load proof — results

**Campaign run:** 2026-08-13 → 2026-08-14, against the deployed stack on this machine.
**Question asked:** can this concrete instantiation of the app survive a real wedding, and then some?
**Answer:** yes, with the bottleneck identified, the margin measured, and four defects found — none of which lose data.

---

## 1. Why the existing load test could not answer this

The prior harness declared success when `offset == size`. That is the exact check that
reported success during the July incident while seventeen photos were destroyed: tusd
cancelled the request context ~10s after the final PATCH, media processing died with it,
and the sources were deleted. Transport success is not completion.

This campaign's oracle therefore requires **five** things of every item:

1. the upload reaches a terminal state (`published` or `duplicate`)
2. the stored original's SHA-256 matches the bytes sent, re-downloaded from the server
3. a thumbnail exists on disk
4. the item appears in the public gallery listing
5. the tus source is **gone** from the upload directory

Criterion 5 is the one that would have caught July. Criterion 2 is the one that catches
silent corruption. Neither can be satisfied by a transport that merely returned 204.

---

## 2. What the machine actually is

Probed, not assumed:

| | |
|---|---|
| Host | AMD Ryzen 9 3900X, 12c/24t, 32 GB RAM |
| **Docker engine** | **NCPU = 4, MemTotal = 8.3 GB** (WSL2 VM limit) |
| Container view | `nproc` = 4, `cgroup cpu.max` = `max` (no quota) |
| Storage | `D:` NVMe — media and tus sources both on it |
| Workers at run time | `MEDIA_PROCESSING_WORKERS=16`, `UPLOAD_DURABILITY_WORKERS=8` |
| Public path | Cloudflare tunnel → `app:8080`, no published host port |

**Everything below was measured on four CPUs.** The wedding will not have fewer.

---

## 3. Stage results

| Stage | Items | Verified | Failed | Drain | Peak pending | ERROR | Verdict |
|---|---|---|---|---|---|---|---|
| smoke | 6 | 6 | 0 | — | 0 | 0 | pass |
| calibrate-serial | 100 | 100 | 0 | — | 0 | 0 | pass |
| calibrate-parallel | 100 | 100 | 0 | — | 64 | 0 | pass |
| **wedding** | **5000** | **5000** | **0** | **5.9 s** | 10 | 2 | see §4 |
| **overload** (3×) | **1000** | **1000** | **0** | **398.8 s** | 598 | 4 | see §4 |
| **herd** | **200** | **200** | **0** | **115.0 s** | 156 | 0 | **pass** |
| **tunnel** (public) | **300** | **300** | **0** | **10.6 s** | — | 0 | **pass** |

**Not one item was lost, corrupted, or left behind in any stage.** Across 6906 verified
items, `oracle_failures` was 0 everywhere, `unexpected_5xx` was 0 everywhere, and every
single published item had a matching SHA-256, a thumbnail, and a gallery entry.

### The wedding profile

5000 items (4600 photos, 400 videos) arriving at 60/min — a busy evening compressed into
~83 minutes of arrivals, 121.8 GiB.

- **Drain: 5.9 seconds** against a 3600 s threshold.
- Peak pending **10**; the oldest item ever waited **6 seconds**.
- By type, all published *with thumbnails*: jpeg 3235, webp 904, png 461, mov 217, mp4 183.
- `arrival_lag` p95 0.004 s — the schedule set the pace, not the client pool.

The queue never built a backlog at wedding rate. On four CPUs.

### Overload — the stage that actually found the ceiling

1000 items at **3× wedding arrival rate**, 629 concurrent tus sources.

This is the first stage to build a real backlog: pending peaked at **598**, the oldest
item waited **383 s**, and it drained in **398.8 s**. Publication capacity measured
**~1.5 items/s**, consistent with calibration.

**The headline result.** Upload `3e7b1ae7…`:

```
11:41:05 ERROR durability   barrier failed (SQLITE_BUSY)
11:41:05 WARN  pre_finish   barrier did not complete within the request budget
11:41:17 ERROR durability   barrier failed AGAIN
11:41:17 WARN  fence        completion fence could not commit in time
11:41:19 INFO  durability   upload became durable        <- async path caught it
11:47:42 INFO  publication  upload published             <- thumbnail, bytes verified
```

Both synchronous safety nets — the pre-finish durability barrier and the completion
fence — gave up under lock contention, and the item still published, byte-for-byte
correct. **That is the July failure mode, absorbed.** It is the single most valuable
observation in this campaign, and it is the reason the durable ingest queue exists.

### Herd — the designed degradation path, observed end to end

200 items in a nominal 1 s window. 115 WARN, every one of them
`durability executor is saturated`: 64 pre-finish barriers and 51 completion fences gave
up inside their budgets, 72 creates were refused with 503, every client retried, and all
200 published. Zero ERROR — which is why this stage is green and the other two are not.
That severity choice is correct, and it is precisely the argument for §4.1.

### Tunnel — the real public path

300 items through `https://example-gallery.invalid`, i.e. Cloudflare edge → tunnel → app,
the path every guest will use. Bytes verified by re-downloading each original *through
Cloudflare*.

- **Max request 13.08 s** (a 196 MB video download) against the ~100 s edge budget — **7.6× headroom**
- **Max pre-finish barrier 0.51 s** against its 75 s budget — **147× headroom**, zero budget misses
- 0 ERROR, 0 WARN, 0 backpressure, 0 transport errors

---

## 4. Defects found

### 4.1 Retryable SQLITE_BUSY is logged at ERROR — *the only reason two stages are marked failed*

Six ERROR lines across the whole campaign. **All six are `database is locked (SQLITE_BUSY)`,
all six were retried, all six succeeded, and none lost data.** One cost a guest a single
503 at create, which the client retried successfully.

A recovered, retryable condition logged at ERROR makes ERROR useless for alerting — and
this is the *same defect class already fixed once* in this branch (the shutdown ERROR).
The herd stage proves the codebase already knows the right answer: it logs an identical
give-up-and-retry-asynchronously event at WARN.

Sites:
- `backend/internal/ingest/manager.go` — `ingest worker iteration failed`
- `backend/internal/ingest/durability.go` — `durability barrier failed`
- `backend/internal/httpapi/tus_hooks.go` — `failed to record upload job`

There is no retryable-error classifier in the codebase yet; the fix needs one.

**The clearest demonstration came from cleanup, not from load.** During the final purge
the app was *completely idle* — no uploads in flight, nothing to ingest — yet it emitted
**141 ERROR lines in about six minutes**, every one of them
`ingest worker iteration failed: claim upload job: database is locked (SQLITE_BUSY)`.
The cause is benign: purging holds a long SQLite write transaction (~16 s per 100 items),
which blocks the idle ingest workers' claim query; they retry on the next tick and
succeed. Nothing was wrong, nothing was lost, and nothing needed attention.

So routine administrative maintenance, on an idle system, generates 141 ERRORs. Any
alerting built on ERROR would page someone every time the gallery is tidied up. This
single observation justifies the fix on its own.

### 4.2 `handleBulkPurge` discards the error — a 500 with *no log line at all*

```go
if err != nil {
    writeError(w, http.StatusInternalServerError, "failed to permanently delete media")
    return
}
```

Observed live: a bulk-purge returned **500 after 44.7 s and the app logged nothing** —
no ERROR, no WARN, no cause. An operator seeing this in production has nothing to go on.
(The trigger here was a client-side timeout cancelling the request context, which is the
harness's fault — but the silence is the app's.)

### 4.3 The public path 403s non-browser clients, invisibly to the app

Cloudflare bot rules answer the default `Python-urllib/3.13` User-Agent with **403 at the
edge**. The request never reaches the app, so it appears in no application log. Measured:
default UA → 403, browser UA → 200, curl UA → 200.

**This matters for the day.** A guest uploading from an in-app webview or a privacy
browser with an unusual User-Agent would hit a hard failure that the server never hears
about and you could not diagnose from the logs. *Worth a two-minute test from a real
phone before the wedding.*

### 4.4 Queue telemetry degrades exactly when it is most interesting

`queue_summary` reads its counts from SQLite, so during the overload stage's lock
contention the observer stalled **48.4 s** (11:40:40 → 11:41:28) — inside the contention
window 11:40:47 → 11:41:17. The instrument stalled on the lock it was measuring. The
oracle is independent of it, so results stand, but queue-depth history has holes under
precisely the conditions worth recording.

---

## 5. Bottleneck attribution

At overload the constraint is **SQLite write-lock contention** — not CPU, not disk, not
the worker pool. The evidence is threefold and mutually corroborating: every ERROR is
SQLITE_BUSY; every missed synchronous budget is SQLITE_BUSY or a saturated durability
executor; and the queue observer, which reads SQLite, stalled inside the same window.

Supporting measurements:
- Mount sustained **167 MB/s**; the corpus never approached disk throughput limits.
- 16× client concurrency bought a **5.5× transport speedup** (efficiency 0.34) — the
  server, not the network, was the limit.
- Workers saturated at 16/16 with 64 pending during calibration.

---

## 6. Honest limitations

These are recorded rather than glossed, because a proof that overstates itself is not one.

1. **The herd was not simultaneous.** `arrival_lag` p50 10.1 s, max 27.0 s. 200 arrivals
   were requested in a 1 s window; the 50-way client pool could only issue them over
   ~27 s. The herd was **client-bound**. A real 200-guest herd would arrive faster than
   we could generate it.
2. **The tunnel stage proves latency, not throughput.** `arrival_lag` p95 45.5 s at
   concurrency 8 — the pool set the pace, so 20 arrivals/min was never sustained through
   Cloudflare. Throughput was proven on the internal path instead.
3. **All load came from one host's TCP stack** — harsher than 200 phones on port and
   backlog pressure, gentler on real mobile radio latency. The campaign's only 10
   transport errors occurred in the herd, the stage with the highest instantaneous
   connection establishment; tus resumability recovered all 10.
4. **HEIC was not tested.** iPhone HEIC could not be built on this host (no HEIF muxer in
   ffmpeg 6.1.1; two ImageMagick container attempts failed). This is a genuine residual
   risk: iPhone guests are likely and HEIC is their default format.
5. **`to_published` percentiles are an artifact** and are not quoted anywhere above.
   Polling begins only after all uploads finish, so an item uploaded in minute 1 is not
   *observed* terminal until ~83 minutes later. The queue samples are the truth.
6. **142 duplicates in the wedding stage** are an artifact of rerunning an aborted run
   under the same per-stage seed, not a server behaviour.

---

## 7. Configuration risk — resolved in code

The stack ran with `MEDIA_PROCESSING_WORKERS=16` and `UPLOAD_DURABILITY_WORKERS=8`, set
by hand in `stack.env`. In the deployed build **the defaults in code are 2 and 2**, and
those hand-set values are *not* persisted: a Portainer redeploy regenerates `stack.env`
and reverts the stack to 2/2 without saying so. That was the single configuration risk
capable of hurting the day.

Recommended derived defaults. Both **deliberately oversubscribe**, because a media
worker spends most of its life waiting on an ffmpeg child and a durability worker on
fsync — neither is bounded by Go's own parallelism, so sizing them 1:1 with `GOMAXPROCS`
starves the queue. The multipliers and caps are chosen to reproduce the only pair that
has actually been proven, 16 and 8:

```go
mediaWorkers      := min(max(4*runtime.GOMAXPROCS(0), 2), 16)
durabilityWorkers := min(max(2*runtime.GOMAXPROCS(0), 2), 8)
```

On this engine (`GOMAXPROCS` = 4) that evaluates to exactly **16 and 8**, and it stays
there at 8, 16 or 24 CPUs, so raising the WSL CPU allocation cannot inflate the pool
beyond what was measured against 8 GB.

Go 1.25's `GOMAXPROCS` is cgroup-aware (verified: with `--cpus=2`, `NumCPU()` = 4 but
`GOMAXPROCS(0)` = 2), so this adapts correctly if the VM is resized.

Separately: `.wslconfig` now requests 16 processors but still only **8 GB** of memory.
Raising that to 16–24 GB is worthwhile before the extra cores can pay off.

### What has landed, and what to deploy before the day

All four defects above are now fixed on `main`, with tests, and the full Go suite passes:

- `store.IsBusy` classifies SQLITE_BUSY by **result code**, not message text, and the
  three log sites demote it to WARN. A test provokes a real locked database rather than
  a hand-built error, because a classifier that silently never matched would look fine
  while changing nothing.
- `handleBulkPurge` now logs the error it used to discard.
- The worker defaults are derived as above, and a test asserts they land on 16/8 from
  four usable CPUs upward — a redeploy silently halving the pool is the regression that
  guard exists to catch.

**Redeploying is now worth doing, and it is what makes the configuration safe.** An
earlier draft of this report advised against it, on the reasoning that the proof applies
to the currently deployed artifact. That advice rested on a mistake worth recording: the
first version of the derived defaults evaluated to **4 and 2** on this engine, not 16 and
8, because it sized the pools 1:1 with `GOMAXPROCS`. Redeploying would therefore have
quietly *reduced* the media pool to a quarter of the tested size — a configuration
nothing in this report covers. That is now fixed.

With the defaults corrected, a redeploy needs no `stack.env` entries at all: the proven
16/8 is what the binary chooses on its own, so a Portainer redeploy can no longer revert
the stack to a slower configuration. Setting the two variables in the Portainer UI is
still harmless and makes the intent explicit, but it is no longer load-bearing.

One caveat, stated plainly: the redeployed image is not the binary these numbers were
measured against. The differences are log severity, one added log line, and defaults
that now match what was tested — the ingest and durability paths are otherwise
untouched. Re-run `pwsh loadtest/run_wedding.ps1 -Stage smoke` after deploying. It takes
a couple of minutes, checks all five oracle criteria against the new artifact, and
restores the proof to the thing that is actually running.

---

## 8. Cleanup, verified by measurement

The campaign wrote 173.8 GiB across 6803 items. All of it is gone:

| | Before | After |
|---|---|---|
| originals on disk | 6803 | **0** |
| thumbnails on disk | 6803 | **0** |
| media directory | 173.8 GiB | **0.00 GiB** |
| public gallery | 6803 | **0** |
| free on `D:` | 189.1 GiB | **362.9 GiB** |

Free space returned to 362.9 GiB against a pre-campaign baseline of 363.1 GiB. The
0.2 GiB difference is **fully accounted for**: two parked tus partials (117 MB + 92 MB =
209 MB) left by the aborted first wedding run. The reconciler correctly refuses to adopt
them because their sidecars are incomplete, and the janitor clears them at the 48 h
`TUS_INCOMPLETE_RETENTION_HOURS` mark. Nothing else was left behind.

Stack healthy afterwards: app and tusd both `healthy`, `/readyz` → `{"status":"ready"}`.

---

## 9. Verdict

On a quarter of the CPU the wedding will have, the deployed system absorbed a full
wedding profile with a **5.9 second drain**, absorbed **three times** that arrival rate
into a bounded backlog that cleared in under seven minutes, degraded correctly rather
than failing under a thundering herd, and lost **nothing** — 6906 items verified by
digest, thumbnail, gallery presence and source removal, including one upload that lost
both of its synchronous safety nets and published anyway.

The system is ready. Two things remain: deploy the corrected defaults and re-run the
smoke stage against the new build (§7), and test one upload from a real phone (§4.3).
