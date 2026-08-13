# Wedding Load Proof Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a load harness whose success oracle proves *publication and byte integrity*, then use it to demonstrate the deployed instance survives a 5,000-item / 108 GB wedding and quantify the headroom beyond it.

**Architecture:** A new self-contained Python package `loadtest/wedding/` runs as a sidecar container on the `wedding-gallery_edge` network, reaching the app at `http://app:8080` (the app publishes no host port). It mounts the tus upload directory and the media directory read-only so it can verify that sources are removed and thumbnails are produced. Base media assets are generated once on the host with ffmpeg; each upload streams `base asset + unique 64-byte marker`, giving a distinct SHA-256 without duplicating gigabytes on disk.

**Tech Stack:** Python 3.14 (stdlib only — no pip dependencies), `asyncio`, `hashlib`, `unittest`; ffmpeg 6.1.1 on the host for asset generation; Docker for the sidecar.

## Global Constraints

- **Stdlib only.** No pip installs. The existing harness is dependency-free and this one stays that way.
- **`loadtest/tus_battle.py` is not modified.** It is a working production tool with different semantics (staged concurrency, no arrival model). The wedding harness is a separate package; the ~150 lines of duplicated tus transport are an accepted, deliberate cost.
- **The five-criteria oracle is the definition of success.** Transport success alone never passes anything. An item counts only if: transport completed, status reached `published`/`duplicate`, the downloaded bytes hash to what was sent, it appears in `/api/gallery`, and its tus source is gone.
- **SHA-256 is sent as tus metadata** so the server independently verifies integrity via its own `checksum_mismatch` path.
- Documented `503` and `429` carrying `Retry-After` are **backpressure, not failure** — counted separately, but their uploads must still publish before the deadline.
- **Zero `ERROR` log lines** is a valid assertion: nothing is restarted, and the false shutdown ERROR was fixed in `main@99e06d7`.
- **Abort if free space on `D:` falls below 50 GB.** Peak need ≈118 GB against 363 GB free.
- All test filenames are prefixed `event-gallery-battle-`.
- **A missed threshold is reported, not tuned away.** Worker counts are set once, up front, on calibration evidence.
- Target under test: `main@99e06d7`, Portainer stack `wedding-gallery`, project network `wedding-gallery_edge`.
- Host paths: uploads `<data-dir>\uploads`, media `<data-dir>\media`, app data `<data-dir>\app`.

## File Structure

| File | Responsibility |
|---|---|
| `loadtest/wedding/__init__.py` | Package marker |
| `loadtest/wedding/corpus.py` | Generate base assets; stream unique payloads; compute digests |
| `loadtest/wedding/oracle.py` | The five-criteria verification and its terminal-state classification |
| `loadtest/wedding/arrivals.py` | Poisson, clustered, and herd arrival schedules |
| `loadtest/wedding/observe.py` | Parse `ingest queue` log samples, level counts, disk free |
| `loadtest/wedding/tusclient.py` | Minimal async tus client with streaming digest |
| `loadtest/wedding/runner.py` | Stage orchestration, abort guard, JSON report |
| `loadtest/wedding/tests/` | stdlib `unittest` suite for all of the above |
| `loadtest/run_wedding.ps1` | Host entry: build assets, launch the sidecar |
| `loadtest/README.md` | Modify: document the wedding harness alongside the battle test |

---

### Task 1: Corpus — real assets, unique payloads, streaming digests

**Files:**
- Create: `loadtest/wedding/__init__.py`
- Create: `loadtest/wedding/corpus.py`
- Test: `loadtest/wedding/tests/test_corpus.py`

**Interfaces:**
- Consumes: nothing
- Produces: `build_assets(assets_dir: Path) -> list[Asset]`, `Asset(name, path, mime, kind)`, `Payload(base, marker, filename, mime, kind)` with `.size: int`, `.sha256_hex: str`, `.chunks(chunk_size) -> Iterator[bytes]`, and `make_payloads(assets, n_photos, n_videos, seed) -> list[Payload]`

- [ ] **Step 1: Write the failing test**

```python
# loadtest/wedding/tests/test_corpus.py
import hashlib, subprocess, tempfile, unittest
from pathlib import Path
from loadtest.wedding.corpus import Asset, Payload, make_payloads

class TestPayload(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.base = self.tmp / "base.bin"
        self.base.write_bytes(b"A" * 1024)
        self.asset = Asset(name="base", path=self.base, mime="image/jpeg", kind="image")

    def test_digest_matches_the_bytes_actually_streamed(self):
        p = Payload(self.asset, marker=b"M" * 64, filename="x.jpg")
        streamed = b"".join(p.chunks(100))
        self.assertEqual(p.sha256_hex, hashlib.sha256(streamed).hexdigest())
        self.assertEqual(p.size, len(streamed))

    def test_same_base_yields_distinct_digests(self):
        a = Payload(self.asset, marker=b"A" * 64, filename="a.jpg")
        b = Payload(self.asset, marker=b"B" * 64, filename="b.jpg")
        self.assertNotEqual(a.sha256_hex, b.sha256_hex)

    def test_marker_is_exactly_64_bytes_so_size_is_predictable(self):
        p = Payload(self.asset, marker=b"M" * 64, filename="x.jpg")
        self.assertEqual(p.size, self.base.stat().st_size + 64)

    def test_make_payloads_is_deterministic_and_correctly_proportioned(self):
        assets = [self.asset]
        one = make_payloads(assets, n_photos=10, n_videos=0, seed=7)
        two = make_payloads(assets, n_photos=10, n_videos=0, seed=7)
        self.assertEqual([p.sha256_hex for p in one], [p.sha256_hex for p in two])
        self.assertEqual(len(one), 10)
        self.assertEqual(len({p.filename for p in one}), 10)
```

- [ ] **Step 2: Run it and watch it fail for the right reason**

Run: `python -m unittest loadtest.wedding.tests.test_corpus -v`
Expected: `ModuleNotFoundError: No module named 'loadtest.wedding.corpus'`

- [ ] **Step 3: Implement `corpus.py`**

```python
# loadtest/wedding/corpus.py
"""Payload corpus for the wedding load proof.

Each upload streams a real, decodable base asset followed by a unique 64-byte
marker. Decoders ignore trailing bytes after JPEG's EOI and after MP4's last
atom, so derivation stays as expensive as a real photo while every upload gets
a distinct SHA-256 and so never collapses into the dedupe path.
"""
from __future__ import annotations

import hashlib
import random
import subprocess
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator

MARKER_BYTES = 64
NAME_PREFIX = "event-gallery-battle"


@dataclass(frozen=True)
class Asset:
    name: str
    path: Path
    mime: str
    kind: str  # "image" | "video"


class Payload:
    def __init__(self, base: Asset, marker: bytes, filename: str):
        if len(marker) != MARKER_BYTES:
            raise ValueError(f"marker must be {MARKER_BYTES} bytes, got {len(marker)}")
        self.base = base
        self.marker = marker
        self.filename = filename
        self.mime = base.mime
        self.kind = base.kind
        self._size = base.path.stat().st_size + MARKER_BYTES
        self._digest = self._compute_digest()

    @property
    def size(self) -> int:
        return self._size

    @property
    def sha256_hex(self) -> str:
        return self._digest

    def chunks(self, chunk_size: int) -> Iterator[bytes]:
        """Yield the payload without ever holding it all in memory."""
        with self.base.path.open("rb") as fh:
            while True:
                block = fh.read(chunk_size)
                if not block:
                    break
                yield block
        yield self.marker

    def _compute_digest(self) -> str:
        h = hashlib.sha256()
        for block in self.chunks(1 << 20):
            h.update(block)
        return h.hexdigest()


def make_payloads(assets: list[Asset], n_photos: int, n_videos: int, seed: int) -> list[Payload]:
    rng = random.Random(seed)
    images = [a for a in assets if a.kind == "image"] or assets
    videos = [a for a in assets if a.kind == "video"]
    out: list[Payload] = []
    for _ in range(n_photos):
        out.append(_one(rng, rng.choice(images)))
    for _ in range(n_videos):
        if not videos:
            raise ValueError("no video assets available but n_videos > 0")
        out.append(_one(rng, rng.choice(videos)))
    rng.shuffle(out)
    return out


def _one(rng: random.Random, base: Asset) -> Payload:
    token = uuid.UUID(int=rng.getrandbits(128)).hex
    marker = (f"{NAME_PREFIX}:{token}".encode("ascii")).ljust(MARKER_BYTES, b"\0")[:MARKER_BYTES]
    ext = ".jpg" if base.kind == "image" else ".mp4"
    if base.mime == "image/png":
        ext = ".png"
    elif base.mime == "image/webp":
        ext = ".webp"
    elif base.mime == "image/heic":
        ext = ".heic"
    elif base.mime == "video/quicktime":
        ext = ".mov"
    return Payload(base, marker, f"{NAME_PREFIX}-{token}{ext}")
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `python -m unittest loadtest.wedding.tests.test_corpus -v`
Expected: 4 tests, all PASS.

- [ ] **Step 5: Add asset generation, driven by a test that the marked file still decodes**

This is the load-bearing check for the whole marker trick. Append to `test_corpus.py`:

```python
class TestAssetsRemainDecodable(unittest.TestCase):
    """A marked asset must still probe, or derivation cost is not representative."""

    def test_marked_jpeg_and_mp4_still_probe(self):
        from loadtest.wedding.corpus import build_assets
        tmp = Path(tempfile.mkdtemp())
        # Small and short on purpose: this test is about decodability, not size.
        assets = build_assets(tmp, photo_size="320x240", video_seconds=1, video_bytes=200_000)
        self.assertTrue(any(a.kind == "image" for a in assets))
        self.assertTrue(any(a.kind == "video" for a in assets))
        for asset in assets:
            p = Payload(asset, marker=b"Z" * 64, filename="probe" )
            marked = tmp / f"marked-{asset.name}"
            with marked.open("wb") as fh:
                for block in p.chunks(1 << 16):
                    fh.write(block)
            proc = subprocess.run(
                ["ffprobe", "-v", "error", "-show_entries", "stream=width,height",
                 "-of", "csv=p=0", str(marked)],
                capture_output=True, text=True,
            )
            self.assertEqual(proc.returncode, 0, f"{asset.name} stopped probing once marked: {proc.stderr}")
            self.assertTrue(proc.stdout.strip(), f"{asset.name} reported no dimensions")
```

- [ ] **Step 6: Run it and watch it fail**

Run: `python -m unittest loadtest.wedding.tests.test_corpus.TestAssetsRemainDecodable -v`
Expected: `ImportError: cannot import name 'build_assets'`

- [ ] **Step 7: Implement `build_assets`**

Append to `corpus.py`:

```python
# Sizes are the wedding profile: ~6 MB photos, ~200 MB videos.
PHOTO_SIZE = "4000x3000"
VIDEO_SECONDS = 60
VIDEO_TARGET_BYTES = 200 * 1024 * 1024

_PHOTO_VARIANTS = [
    ("photo-jpeg", "image/jpeg", "mjpeg", ".jpg"),
    ("photo-png", "image/png", "png", ".png"),
    ("photo-webp", "image/webp", "libwebp", ".webp"),
]
_VIDEO_VARIANTS = [
    ("video-mp4", "video/mp4", ".mp4"),
    ("video-mov", "video/quicktime", ".mov"),
]


def build_assets(assets_dir: Path, photo_size: str = PHOTO_SIZE,
                 video_seconds: int = VIDEO_SECONDS,
                 video_bytes: int = VIDEO_TARGET_BYTES) -> list[Asset]:
    """Generate the base assets once. Idempotent: existing files are reused."""
    assets_dir.mkdir(parents=True, exist_ok=True)
    out: list[Asset] = []

    for name, mime, encoder, ext in _PHOTO_VARIANTS:
        path = assets_dir / f"{name}{ext}"
        if not path.exists():
            # testsrc2 plus heavy noise defeats compression, so the file reaches
            # its target size with genuine image data rather than padding.
            _run([
                "ffmpeg", "-y", "-v", "error",
                "-f", "lavfi", "-i", f"testsrc2=s={photo_size},noise=alls=90:allf=t+u",
                "-frames:v", "1", "-c:v", encoder, "-q:v", "1", str(path),
            ])
        out.append(Asset(name=name, path=path, mime=mime, kind="image"))

    for name, mime, ext in _VIDEO_VARIANTS:
        path = assets_dir / f"{name}{ext}"
        if not path.exists():
            bitrate = max(100_000, (video_bytes * 8) // max(1, video_seconds))
            _run([
                "ffmpeg", "-y", "-v", "error",
                "-f", "lavfi", "-i", "testsrc2=s=1920x1080:r=30",
                "-t", str(video_seconds), "-c:v", "libx264", "-preset", "veryfast",
                "-b:v", str(bitrate), "-pix_fmt", "yuv420p", str(path),
            ])
        out.append(Asset(name=name, path=path, mime=mime, kind="video"))

    return out


def _run(cmd: list[str]) -> None:
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        raise RuntimeError(f"{cmd[0]} failed: {proc.stderr[:400]}")
```

- [ ] **Step 8: Run the full corpus suite**

Run: `python -m unittest loadtest.wedding.tests.test_corpus -v`
Expected: 5 tests PASS. The probe test takes ~30s because it generates real assets.

- [ ] **Step 9: Falsify the decodability test**

Temporarily change `MARKER_BYTES` to `64` but make `chunks()` yield the marker *first* instead of last, then re-run `TestAssetsRemainDecodable`.
Expected: FAIL — `ffprobe` cannot identify a file whose header is a marker. This proves the test detects a broken marker strategy rather than passing regardless. Revert immediately.

- [ ] **Step 10: Commit**

```bash
git add loadtest/wedding/__init__.py loadtest/wedding/corpus.py loadtest/wedding/tests/test_corpus.py
git commit -m "test(loadtest): corpus of real assets with per-upload unique digests"
```

---

### Task 2: The five-criteria completion oracle

**Files:**
- Create: `loadtest/wedding/oracle.py`
- Test: `loadtest/wedding/tests/test_oracle.py`

**Interfaces:**
- Consumes: nothing from Task 1 at runtime; mirrors its `Payload.sha256_hex` contract
- Produces: `ItemVerdict` dataclass with fields `upload_id, filename, media_id, state, transport_ok, published_ok, digest_ok, gallery_ok, source_gone` and property `ok`; `TERMINAL_STATES: set[str]`; `classify(state) -> bool`; `verdict_for(...) -> ItemVerdict`

- [ ] **Step 1: Write the failing test**

```python
# loadtest/wedding/tests/test_oracle.py
import unittest
from loadtest.wedding.oracle import ItemVerdict, TERMINAL_STATES, classify

def _passing(**over):
    base = dict(upload_id="u1", filename="f.jpg", media_id="m1", state="published",
                transport_ok=True, published_ok=True, digest_ok=True,
                gallery_ok=True, source_gone=True)
    base.update(over)
    return ItemVerdict(**base)

class TestVerdict(unittest.TestCase):
    def test_all_five_criteria_must_hold(self):
        self.assertTrue(_passing().ok)

    def test_any_single_criterion_failing_fails_the_item(self):
        for field in ("transport_ok", "published_ok", "digest_ok", "gallery_ok", "source_gone"):
            with self.subTest(field=field):
                self.assertFalse(_passing(**{field: False}).ok,
                                 f"{field} was allowed to fail without failing the item")

    def test_transport_success_alone_never_passes(self):
        v = _passing(published_ok=False, digest_ok=False, gallery_ok=False, source_gone=False)
        self.assertFalse(v.ok)

    def test_terminal_states(self):
        self.assertEqual(TERMINAL_STATES, {"published", "duplicate", "failed", "cancelled"})
        self.assertTrue(classify("published"))
        self.assertTrue(classify("duplicate"))
        self.assertFalse(classify("failed"))
        self.assertFalse(classify("processing"))
```

- [ ] **Step 2: Run it and watch it fail**

Run: `python -m unittest loadtest.wedding.tests.test_oracle -v`
Expected: `ModuleNotFoundError: No module named 'loadtest.wedding.oracle'`

- [ ] **Step 3: Implement the verdict half of `oracle.py`**

```python
# loadtest/wedding/oracle.py
"""The completion oracle.

Transport success is never sufficient. The July incident reported every upload
successful while seventeen sources were deleted, because the only check was
`offset == size`. Criterion 5 -- the tus source is gone -- is what would have
caught it, so it is a first-class requirement here.
"""
from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path

TERMINAL_STATES = {"published", "duplicate", "failed", "cancelled"}
SUCCESS_STATES = {"published", "duplicate"}


def classify(state: str) -> bool:
    """True when the state means the upload made it into the gallery."""
    return state in SUCCESS_STATES


@dataclass
class ItemVerdict:
    upload_id: str
    filename: str
    media_id: str
    state: str
    transport_ok: bool
    published_ok: bool
    digest_ok: bool
    gallery_ok: bool
    source_gone: bool

    @property
    def ok(self) -> bool:
        return (self.transport_ok and self.published_ok and self.digest_ok
                and self.gallery_ok and self.source_gone)

    @property
    def failed_criteria(self) -> list[str]:
        names = ("transport_ok", "published_ok", "digest_ok", "gallery_ok", "source_gone")
        return [n for n in names if not getattr(self, n)]
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `python -m unittest loadtest.wedding.tests.test_oracle -v`
Expected: 4 tests PASS.

- [ ] **Step 5: Write the failing test for the HTTP-facing checks**

Append to `test_oracle.py`:

```python
import hashlib, http.server, json, tempfile, threading, unittest
from pathlib import Path
from loadtest.wedding.oracle import poll_status, verify_download, gallery_filenames, source_removed

BODY = b"the exact bytes we uploaded"
GOOD_SHA = hashlib.sha256(BODY).hexdigest()

class _Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):  # keep test output pristine
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        ids = json.loads(self.rfile.read(n))["uploadIds"]
        results = {i: {"state": "published", "mediaId": f"media-{i}"} for i in ids}
        self._json({"results": results})

    def do_GET(self):
        if self.path.startswith("/api/media/"):
            payload = BODY if "good" in self.path else b"CORRUPTED"
            self.send_response(200)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
        elif self.path.startswith("/api/gallery"):
            self._json({"items": [{"originalFilename": "present.jpg"}], "nextCursor": None})
        else:
            self.send_error(404)

    def _json(self, obj):
        raw = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

class TestHTTPChecks(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = http.server.HTTPServer(("127.0.0.1", 0), _Handler)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.base = f"http://127.0.0.1:{cls.srv.server_port}"

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def test_poll_status_batches_and_returns_states(self):
        got = poll_status(self.base, [f"u{i}" for i in range(150)], batch_size=100)
        self.assertEqual(len(got), 150)
        self.assertEqual(got["u7"]["state"], "published")

    def test_verify_download_accepts_matching_bytes(self):
        ok, _ = verify_download(self.base, "good-1", GOOD_SHA)
        self.assertTrue(ok)

    def test_verify_download_rejects_corrupted_bytes(self):
        ok, detail = verify_download(self.base, "bad-1", GOOD_SHA)
        self.assertFalse(ok, "a corrupted download was accepted")
        self.assertIn("digest", detail)

    def test_gallery_filenames(self):
        self.assertIn("present.jpg", gallery_filenames(self.base))

    def test_source_removed_is_false_while_the_file_remains(self):
        tmp = Path(tempfile.mkdtemp())
        (tmp / "u1").write_bytes(b"x")
        self.assertFalse(source_removed(tmp, "u1"))
        (tmp / "u1").unlink()
        self.assertTrue(source_removed(tmp, "u1"))
```

- [ ] **Step 6: Run it and watch it fail**

Run: `python -m unittest loadtest.wedding.tests.test_oracle.TestHTTPChecks -v`
Expected: `ImportError: cannot import name 'poll_status'`

- [ ] **Step 7: Implement the HTTP-facing checks**

Append to `oracle.py`:

```python
import urllib.request

STATUS_BATCH_MAX = 100


def poll_status(base_url: str, upload_ids: list[str], batch_size: int = STATUS_BATCH_MAX,
                timeout: float = 30.0) -> dict[str, dict]:
    """Batched status poll. The endpoint caps a batch at 100 ids."""
    if batch_size > STATUS_BATCH_MAX:
        raise ValueError(f"batch_size must be <= {STATUS_BATCH_MAX}")
    out: dict[str, dict] = {}
    for start in range(0, len(upload_ids), batch_size):
        chunk = upload_ids[start:start + batch_size]
        body = json.dumps({"uploadIds": chunk}).encode()
        req = urllib.request.Request(
            f"{base_url}/api/uploads/status", data=body,
            headers={"Content-Type": "application/json"}, method="POST")
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            out.update(json.loads(resp.read()).get("results", {}))
    return out


def verify_download(base_url: str, media_id: str, expected_sha: str,
                    timeout: float = 600.0) -> tuple[bool, str]:
    """Re-fetch the stored original and compare its digest with what we sent."""
    url = f"{base_url}/api/media/{media_id}/download"
    h = hashlib.sha256()
    got = 0
    with urllib.request.urlopen(url, timeout=timeout) as resp:
        while True:
            block = resp.read(1 << 20)
            if not block:
                break
            h.update(block)
            got += len(block)
    actual = h.hexdigest()
    if actual != expected_sha:
        return False, f"digest mismatch: sent {expected_sha[:12]}, stored {actual[:12]} ({got} bytes)"
    return True, ""


def gallery_filenames(base_url: str, timeout: float = 60.0) -> set[str]:
    """Every original filename the public gallery lists, following pagination."""
    names: set[str] = set()
    cursor = None
    while True:
        url = f"{base_url}/api/gallery" + (f"?cursor={cursor}" if cursor else "")
        with urllib.request.urlopen(url, timeout=timeout) as resp:
            page = json.loads(resp.read())
        for item in page.get("items", []):
            name = item.get("originalFilename") or item.get("filename")
            if name:
                names.add(name)
        cursor = page.get("nextCursor")
        if not cursor:
            return names


def source_removed(upload_dir: Path, upload_id: str) -> bool:
    """Criterion 5: publication must have removed the tus source and its sidecar."""
    return not (upload_dir / upload_id).exists() and not (upload_dir / f"{upload_id}.info").exists()
```

- [ ] **Step 8: Run the whole oracle suite**

Run: `python -m unittest loadtest.wedding.tests.test_oracle -v`
Expected: 9 tests PASS.

- [ ] **Step 9: Falsify the corruption check**

Temporarily change `verify_download` to `return True, ""` unconditionally.
Run: `python -m unittest loadtest.wedding.tests.test_oracle.TestHTTPChecks.test_verify_download_rejects_corrupted_bytes -v`
Expected: FAIL with "a corrupted download was accepted". This is the assertion the whole proof rests on. Revert.

- [ ] **Step 10: Commit**

```bash
git add loadtest/wedding/oracle.py loadtest/wedding/tests/test_oracle.py
git commit -m "test(loadtest): five-criteria completion oracle with byte verification"
```

---

### Task 3: Arrival schedules

**Files:**
- Create: `loadtest/wedding/arrivals.py`
- Test: `loadtest/wedding/tests/test_arrivals.py`

**Interfaces:**
- Consumes: nothing
- Produces: `poisson_schedule(n, rate_per_min, seed, cluster_fraction=0.15) -> list[float]` (seconds from start, sorted), `herd_schedule(n) -> list[float]`, `split_items(n_items, n_guests, seed) -> list[int]`

- [ ] **Step 1: Write the failing test**

```python
# loadtest/wedding/tests/test_arrivals.py
import statistics, unittest
from loadtest.wedding.arrivals import poisson_schedule, herd_schedule, split_items

class TestArrivals(unittest.TestCase):
    def test_poisson_mean_gap_approximates_one_over_rate(self):
        rate = 12.0  # per minute -> expected gap 5s
        times = poisson_schedule(4000, rate_per_min=rate, seed=1, cluster_fraction=0.0)
        gaps = [b - a for a, b in zip(times, times[1:])]
        self.assertAlmostEqual(statistics.mean(gaps), 60.0 / rate, delta=0.6)

    def test_schedule_is_sorted_and_deterministic(self):
        a = poisson_schedule(500, rate_per_min=30, seed=42)
        b = poisson_schedule(500, rate_per_min=30, seed=42)
        self.assertEqual(a, b)
        self.assertEqual(a, sorted(a))

    def test_clustering_makes_some_arrivals_simultaneous(self):
        spread = poisson_schedule(500, rate_per_min=30, seed=3, cluster_fraction=0.0)
        clumped = poisson_schedule(500, rate_per_min=30, seed=3, cluster_fraction=0.4)
        def tight(ts):
            return sum(1 for a, b in zip(ts, ts[1:]) if b - a < 0.05)
        self.assertGreater(tight(clumped), tight(spread),
                           "cluster_fraction did not actually cluster anything")

    def test_herd_is_simultaneous(self):
        self.assertEqual(herd_schedule(50), [0.0] * 50)

    def test_split_items_conserves_the_total(self):
        parts = split_items(5000, 120, seed=9)
        self.assertEqual(sum(parts), 5000)
        self.assertEqual(len(parts), 120)
        self.assertTrue(all(p >= 1 for p in parts))
```

- [ ] **Step 2: Run it and watch it fail**

Run: `python -m unittest loadtest.wedding.tests.test_arrivals -v`
Expected: `ModuleNotFoundError: No module named 'loadtest.wedding.arrivals'`

- [ ] **Step 3: Implement `arrivals.py`**

```python
# loadtest/wedding/arrivals.py
"""Arrival schedules.

Guests do not coordinate, though a few cluster -- so the realistic model is a
Poisson process with a minority of arrivals collapsed onto shared instants. The
herd schedule is strictly more stressful, so passing it implies the realistic
case.
"""
from __future__ import annotations

import random


def poisson_schedule(n: int, rate_per_min: float, seed: int,
                     cluster_fraction: float = 0.15) -> list[float]:
    if rate_per_min <= 0:
        raise ValueError("rate_per_min must be positive")
    if not 0.0 <= cluster_fraction < 1.0:
        raise ValueError("cluster_fraction must be in [0, 1)")
    rng = random.Random(seed)
    mean_gap = 60.0 / rate_per_min
    times: list[float] = []
    t = 0.0
    for _ in range(n):
        t += rng.expovariate(1.0 / mean_gap)
        times.append(t)

    n_clustered = int(n * cluster_fraction)
    for _ in range(n_clustered):
        src = rng.randrange(n)
        dst = rng.randrange(n)
        times[src] = times[dst]

    times.sort()
    return times


def herd_schedule(n: int) -> list[float]:
    return [0.0] * n


def split_items(n_items: int, n_guests: int, seed: int) -> list[int]:
    """Distribute items across guests, everyone contributing at least one."""
    if n_guests > n_items:
        raise ValueError("more guests than items")
    rng = random.Random(seed)
    parts = [1] * n_guests
    for _ in range(n_items - n_guests):
        parts[rng.randrange(n_guests)] += 1
    return parts
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `python -m unittest loadtest.wedding.tests.test_arrivals -v`
Expected: 5 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add loadtest/wedding/arrivals.py loadtest/wedding/tests/test_arrivals.py
git commit -m "test(loadtest): poisson, clustered, and herd arrival schedules"
```

---

### Task 4: Observability collector

**Files:**
- Create: `loadtest/wedding/observe.py`
- Test: `loadtest/wedding/tests/test_observe.py`

**Interfaces:**
- Consumes: nothing
- Produces: `QueueSample(time, fields)`, `parse_queue_samples(lines) -> list[QueueSample]`, `count_levels(lines) -> dict[str, int]`, `disk_free_bytes(path) -> int`, `thumbnail_exists(media_dir, media_id) -> bool`

- [ ] **Step 1: Write the failing test**

```python
# loadtest/wedding/tests/test_observe.py
import tempfile, unittest
from pathlib import Path
from loadtest.wedding.observe import parse_queue_samples, count_levels, disk_free_bytes, thumbnail_exists

LINES = [
    '{"time":"2026-08-13T14:12:15Z","level":"INFO","msg":"server listening","addr":":8080"}',
    '{"time":"2026-08-13T14:12:30Z","level":"INFO","msg":"ingest queue","pending":42,"processing":16,"oldest_pending_age_seconds":9,"max_processing_failures":0}',
    'not json at all',
    '{"time":"2026-08-13T14:12:45Z","level":"WARN","msg":"ingest attempt failed, retrying"}',
    '{"time":"2026-08-13T14:13:00Z","level":"ERROR","msg":"something real"}',
    '{"time":"2026-08-13T14:13:15Z","level":"INFO","msg":"ingest queue","pending":0,"processing":0,"oldest_pending_age_seconds":0,"max_processing_failures":0}',
]

class TestObserve(unittest.TestCase):
    def test_parses_only_queue_lines_and_keeps_numeric_fields(self):
        samples = parse_queue_samples(LINES)
        self.assertEqual(len(samples), 2)
        self.assertEqual(samples[0].fields["pending"], 42)
        self.assertEqual(samples[1].fields["pending"], 0)

    def test_malformed_lines_do_not_raise(self):
        self.assertEqual(len(parse_queue_samples(["", "garbage", "{"])), 0)

    def test_counts_levels(self):
        counts = count_levels(LINES)
        self.assertEqual(counts["ERROR"], 1)
        self.assertEqual(counts["WARN"], 1)
        self.assertEqual(counts["INFO"], 4)

    def test_disk_free_is_positive(self):
        self.assertGreater(disk_free_bytes(Path(tempfile.gettempdir())), 0)

    def test_thumbnail_detection(self):
        tmp = Path(tempfile.mkdtemp())
        (tmp / "thumbs").mkdir()
        (tmp / "thumbs" / "m1.jpg").write_bytes(b"x")
        self.assertTrue(thumbnail_exists(tmp, "m1"))
        self.assertFalse(thumbnail_exists(tmp, "m2"))
```

- [ ] **Step 2: Run it and watch it fail**

Run: `python -m unittest loadtest.wedding.tests.test_observe -v`
Expected: `ModuleNotFoundError: No module named 'loadtest.wedding.observe'`

- [ ] **Step 3: Implement `observe.py`**

```python
# loadtest/wedding/observe.py
"""Instrumentation.

The reconciler already logs `ingest queue` once per pass with counts by status
and the oldest pending age, so the drain curve is read directly from the app's
own output rather than inferred from client-side timings.

Fields are not hardcoded: every numeric key on the line is captured, so the
parser survives the app renaming or adding one.
"""
from __future__ import annotations

import json
import shutil
from dataclasses import dataclass
from pathlib import Path

QUEUE_MSG = "ingest queue"


@dataclass
class QueueSample:
    time: str
    fields: dict[str, float]


def parse_queue_samples(lines) -> list[QueueSample]:
    out: list[QueueSample] = []
    for line in lines:
        rec = _load(line)
        if rec is None or rec.get("msg") != QUEUE_MSG:
            continue
        fields = {k: v for k, v in rec.items() if isinstance(v, (int, float)) and not isinstance(v, bool)}
        out.append(QueueSample(time=rec.get("time", ""), fields=fields))
    return out


def count_levels(lines) -> dict[str, int]:
    counts: dict[str, int] = {}
    for line in lines:
        rec = _load(line)
        if rec is None:
            continue
        level = rec.get("level")
        if level:
            counts[level] = counts.get(level, 0) + 1
    return counts


def disk_free_bytes(path: Path) -> int:
    return shutil.disk_usage(path).free


def thumbnail_exists(media_dir: Path, media_id: str) -> bool:
    """A published item with no thumbnail is a reported finding, not a failure."""
    thumbs = media_dir / "thumbs"
    if not thumbs.is_dir():
        return False
    return any(p.stem == media_id for p in thumbs.iterdir())


def _load(line: str):
    line = (line or "").strip()
    if not line.startswith("{"):
        return None
    try:
        rec = json.loads(line)
    except json.JSONDecodeError:
        return None
    return rec if isinstance(rec, dict) else None
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `python -m unittest loadtest.wedding.tests.test_observe -v`
Expected: 5 tests PASS.

- [ ] **Step 5: Confirm the parser matches the app's real output**

Run: `docker logs wedding-gallery-app-1 2>&1 | Select-String -Pattern '"msg":"ingest queue"' | Select-Object -Last 3`

If any lines are returned, save one into `test_observe.py` as an additional literal in `LINES` and re-run the suite. If none are returned the queue has simply been idle — that is expected on an empty gallery, and Task 8 will confirm the format once load is running. Record which case applied.

- [ ] **Step 6: Verify the real thumbnail directory layout before trusting `thumbnail_exists`**

Run: `docker exec wedding-gallery-app-1 sh -c 'ls /data/media; echo ---; ls /data/media/* | head'`
Expected: reveals the actual subdirectory names. If thumbnails are not under `thumbs/`, correct the constant in `observe.py` and update the test to match the real layout.

- [ ] **Step 7: Commit**

```bash
git add loadtest/wedding/observe.py loadtest/wedding/tests/test_observe.py
git commit -m "test(loadtest): parse the app's own queue samples for the drain curve"
```

---

### Task 5: tus client, runner, and report

**Files:**
- Create: `loadtest/wedding/tusclient.py`
- Create: `loadtest/wedding/runner.py`
- Create: `loadtest/run_wedding.ps1`
- Test: `loadtest/wedding/tests/test_runner.py`
- Modify: `loadtest/README.md`

**Interfaces:**
- Consumes: `corpus.Payload`, `oracle.*`, `arrivals.*`, `observe.*`
- Produces: `run_stage(cfg: StageConfig) -> dict` writing a JSON report; `StageConfig` with fields `name, base_url, upload_dir, media_dir, payloads, schedule, chunk_bytes, deadline_seconds, min_free_bytes, out_path`

- [ ] **Step 1: Write the failing test for the abort guard and report shape**

```python
# loadtest/wedding/tests/test_runner.py
import unittest
from loadtest.wedding.runner import DiskGuard, summarize
from loadtest.wedding.oracle import ItemVerdict

def _v(ok=True, **over):
    base = dict(upload_id="u", filename="f.jpg", media_id="m", state="published",
                transport_ok=True, published_ok=True, digest_ok=True,
                gallery_ok=True, source_gone=True)
    if not ok:
        base["digest_ok"] = False
    base.update(over)
    return ItemVerdict(**base)

class TestDiskGuard(unittest.TestCase):
    def test_trips_below_the_floor(self):
        g = DiskGuard(min_free_bytes=100, probe=lambda: 99)
        with self.assertRaises(RuntimeError):
            g.check()

    def test_passes_above_the_floor(self):
        DiskGuard(min_free_bytes=100, probe=lambda: 101).check()

class TestSummary(unittest.TestCase):
    def test_a_single_failed_item_fails_the_stage(self):
        report = summarize("s", [_v(), _v(ok=False)], backpressure={}, unexpected_5xx=0,
                           levels={"ERROR": 0}, queue=[], disk=(1, 1))
        self.assertFalse(report["passed"])
        self.assertEqual(report["items"]["verified"], 1)
        self.assertEqual(len(report["oracle_failures"]), 1)

    def test_an_error_log_line_fails_the_stage(self):
        report = summarize("s", [_v()], backpressure={}, unexpected_5xx=0,
                           levels={"ERROR": 1}, queue=[], disk=(1, 1))
        self.assertFalse(report["passed"])

    def test_backpressure_alone_does_not_fail_the_stage(self):
        report = summarize("s", [_v()], backpressure={"503": 40, "429": 5},
                           unexpected_5xx=0, levels={"ERROR": 0}, queue=[], disk=(1, 1))
        self.assertTrue(report["passed"])

    def test_unexpected_5xx_fails_the_stage(self):
        report = summarize("s", [_v()], backpressure={}, unexpected_5xx=1,
                           levels={"ERROR": 0}, queue=[], disk=(1, 1))
        self.assertFalse(report["passed"])
```

- [ ] **Step 2: Run it and watch it fail**

Run: `python -m unittest loadtest.wedding.tests.test_runner -v`
Expected: `ModuleNotFoundError: No module named 'loadtest.wedding.runner'`

- [ ] **Step 3: Implement `tusclient.py`**

```python
# loadtest/wedding/tusclient.py
"""Minimal tus client.

Threads plus urllib rather than asyncio: the stdlib ships no async HTTP client,
and at the 50-concurrent ceiling the app enforces per IP, a thread pool is both
adequate and far easier to reason about.

Deliberately separate from tus_battle.py: this one schedules by arrival time and
streams a digest, neither of which that harness has any notion of.
"""
from __future__ import annotations

import base64
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field

RETRYABLE = {408, 409, 423, 425, 429, 500, 502, 503, 504}
BACKPRESSURE = {429, 503}


@dataclass
class Attempt:
    upload_id: str = ""
    statuses: list[int] = field(default_factory=list)
    retries: int = 0
    transport_seconds: float = 0.0
    error: str = ""

    @property
    def transport_ok(self) -> bool:
        return not self.error and bool(self.upload_id)


def _request(method: str, url: str, headers: dict, body: bytes | None = None,
             timeout: float = 600.0):
    req = urllib.request.Request(url, data=body, method=method)
    for key, value in headers.items():
        req.add_header(key, value)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, {k.lower(): v for k, v in resp.headers.items()}, resp.read()
    except urllib.error.HTTPError as exc:
        return exc.code, {k.lower(): v for k, v in exc.headers.items()}, exc.read()


def _metadata(payload) -> str:
    parts = {
        "filename": payload.filename,
        "filetype": payload.mime,
        "sha256": payload.sha256_hex,
        "name": "load-test",
    }
    return ",".join(f"{k} {base64.b64encode(v.encode()).decode()}" for k, v in parts.items())


def upload(base_url: str, payload, chunk_bytes: int, max_retries: int = 5) -> Attempt:
    """POST create, then PATCH to completion. One payload, one thread."""
    attempt = Attempt()
    started = time.monotonic()

    status, headers, _ = _request("POST", f"{base_url}/api/tus", {
        "Tus-Resumable": "1.0.0",
        "Upload-Length": str(payload.size),
        "Upload-Metadata": _metadata(payload),
        "Content-Length": "0",
    })
    attempt.statuses.append(status)
    if status != 201:
        attempt.error = f"create returned {status}"
        return attempt
    attempt.upload_id = headers.get("location", "").rstrip("/").rsplit("/", 1)[-1]
    if not attempt.upload_id:
        attempt.error = "create returned no Location header"
        return attempt

    offset = 0
    for block in _blocks(payload, chunk_bytes):
        moved = _patch(base_url, attempt, offset, block, max_retries)
        if moved == offset:
            break
        offset = moved

    attempt.transport_seconds = time.monotonic() - started
    if offset != payload.size and not attempt.error:
        attempt.error = f"final offset {offset} != {payload.size}"
    return attempt


def _blocks(payload, chunk_bytes: int):
    """Re-chunk the payload's stream to exactly chunk_bytes, tail included."""
    buf = bytearray()
    for piece in payload.chunks(chunk_bytes):
        buf += piece
        while len(buf) >= chunk_bytes:
            yield bytes(buf[:chunk_bytes])
            del buf[:chunk_bytes]
    if buf:
        yield bytes(buf)


def _patch(base_url: str, attempt: Attempt, offset: int, body: bytes, max_retries: int) -> int:
    for i in range(max_retries):
        status, headers, _ = _request("PATCH", f"{base_url}/api/tus/{attempt.upload_id}", {
            "Tus-Resumable": "1.0.0",
            "Upload-Offset": str(offset),
            "Content-Type": "application/offset+octet-stream",
        }, body=body)
        attempt.statuses.append(status)
        if status == 204:
            return int(headers.get("upload-offset", offset + len(body)))
        if status not in RETRYABLE:
            attempt.error = f"PATCH returned {status}"
            return offset
        attempt.retries += 1
        time.sleep(min(2 ** i, 10))
    attempt.error = "PATCH retries exhausted"
    return offset
```

- [ ] **Step 4: Implement `runner.py`**

```python
# loadtest/wedding/runner.py
"""Stage orchestration and reporting."""
from __future__ import annotations

import json
import statistics
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from .oracle import ItemVerdict


@dataclass
class DiskGuard:
    """Peak need is ~118 GB against 363 GB free; this stops a runaway stage."""
    min_free_bytes: int
    probe: Callable[[], int]

    def check(self) -> None:
        free = self.probe()
        if free < self.min_free_bytes:
            raise RuntimeError(
                f"aborting: {free / 1e9:.1f} GB free is below the "
                f"{self.min_free_bytes / 1e9:.1f} GB floor")


def _pct(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    k = max(0, min(len(ordered) - 1, int(round((len(ordered) - 1) * p))))
    return round(ordered[k], 3)


def decide_passed(report: dict) -> bool:
    """The single definition of a passing stage. Both summarize and finalize
    use it, so the provisional and authoritative verdicts cannot diverge."""
    return (not report["oracle_failures"]
            and report["unexpected_5xx"] == 0
            and report["log_levels"].get("ERROR", 0) == 0
            and report["items"]["verified"] == report["items"]["total"])


def summarize(stage: str, verdicts: list[ItemVerdict], backpressure: dict[str, int],
              unexpected_5xx: int, levels: dict[str, int], queue: list,
              disk: tuple[int, int], to_published: list[float] | None = None,
              drain_seconds: float | None = None) -> dict:
    to_published = to_published or []
    verified = [v for v in verdicts if v.ok]
    failures = [
        {"upload_id": v.upload_id, "filename": v.filename, "state": v.state,
         "failed": v.failed_criteria}
        for v in verdicts if not v.ok
    ]
    report = {
        "stage": stage,
        "items": {
            "total": len(verdicts),
            "verified": len(verified),
            "published": sum(1 for v in verdicts if v.state == "published"),
            "duplicate": sum(1 for v in verdicts if v.state == "duplicate"),
            "failed": sum(1 for v in verdicts if v.state == "failed"),
        },
        "oracle_failures": failures[:50],
        "backpressure": backpressure,
        "unexpected_5xx": unexpected_5xx,
        "log_levels": levels,
        "timings": {
            "to_published_p50": _pct(to_published, 0.50),
            "to_published_p95": _pct(to_published, 0.95),
            "to_published_max": round(max(to_published), 3) if to_published else 0.0,
        },
        "drain_seconds": drain_seconds,
        "queue_samples": [{"time": q.time, **q.fields} for q in queue],
        "disk": {"free_before": disk[0], "free_after": disk[1]},
    }
    report["passed"] = decide_passed(report)
    return report


def write_report(report: dict, out_path: Path) -> None:
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(report, indent=2))
```

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `python -m unittest loadtest.wedding.tests.test_runner -v`
Expected: 6 tests PASS.

- [ ] **Step 6: Falsify the pass logic**

Temporarily change `passed` to `len(failures) == 0` only, dropping the ERROR and 5xx terms.
Run: `python -m unittest loadtest.wedding.tests.test_runner -v`
Expected: `test_an_error_log_line_fails_the_stage` and `test_unexpected_5xx_fails_the_stage` both FAIL. Revert.

- [ ] **Step 7: Implement `stage.py` — the orchestration**

```python
# loadtest/wedding/stage.py
"""CLI entry point: run one named stage end to end and write its report.

Log-derived fields are left empty here and filled by `finalize` on the host:
this process runs in a sidecar with no Docker socket, so it cannot read the
app's logs itself. The `passed` value it prints is therefore provisional.
"""
from __future__ import annotations

import argparse
import json
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from . import arrivals, corpus, observe, oracle, runner, tusclient

# photos, videos, guests, arrivals/min, cluster fraction, concurrency
STAGES = {
    "smoke":              (3, 3, 6, 600.0, 0.0, 3),
    "calibrate-serial":   (92, 8, 100, 6000.0, 0.0, 1),
    "calibrate-parallel": (92, 8, 100, 6000.0, 0.0, 16),
    "wedding":            (4600, 400, 120, 12.0, 0.15, 40),
    "overload":           (900, 100, 120, 36.0, 0.15, 50),
    "tunnel":             (280, 20, 30, 20.0, 0.15, 8),
}

MIN_FREE_BYTES = 50 * 1024 ** 3
CHUNK_BYTES = 8 * 1024 * 1024
POLL_SECONDS = 10.0


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--stage", required=True, choices=sorted(STAGES))
    ap.add_argument("--base-url", required=True)
    ap.add_argument("--upload-dir", required=True)
    ap.add_argument("--media-dir", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--assets", default="loadtest/assets")
    ap.add_argument("--deadline", type=float, default=7200.0)
    args = ap.parse_args()

    photos, videos, _guests, rate, cluster, concurrency = STAGES[args.stage]
    upload_dir, media_dir = Path(args.upload_dir), Path(args.media_dir)

    guard = runner.DiskGuard(MIN_FREE_BYTES, lambda: observe.disk_free_bytes(upload_dir))
    guard.check()
    free_before = observe.disk_free_bytes(upload_dir)

    assets = corpus.build_assets(Path(args.assets))
    payloads = corpus.make_payloads(assets, photos, videos, seed=1234)
    schedule = arrivals.poisson_schedule(len(payloads), rate, seed=1234,
                                         cluster_fraction=cluster)

    started = time.monotonic()
    landed: dict[str, tuple] = {}

    def run_one(index: int):
        wait = schedule[index] - (time.monotonic() - started)
        if wait > 0:
            time.sleep(wait)
        guard.check()
        payload = payloads[index]
        return payload, tusclient.upload(args.base_url, payload, CHUNK_BYTES), time.monotonic()

    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        for payload, attempt, finished_at in pool.map(run_one, range(len(payloads))):
            key = attempt.upload_id or f"nocreate-{payload.filename}"
            landed[key] = (payload, attempt, finished_at)

    last_upload_at = max((f for _, _, f in landed.values()), default=time.monotonic())
    live = [k for k, (_, a, _) in landed.items() if a.upload_id]
    states = _await_terminal(args.base_url, live, args.deadline)
    drain_seconds = time.monotonic() - last_upload_at

    verdicts, to_published = _verify(args.base_url, upload_dir, landed, states)

    report = runner.summarize(
        args.stage, verdicts,
        backpressure=_backpressure(landed),
        unexpected_5xx=_unexpected_5xx(landed),
        levels={},          # filled by finalize on the host
        queue=[],           # filled by finalize on the host
        disk=(free_before, observe.disk_free_bytes(upload_dir)),
        to_published=to_published,
        drain_seconds=round(drain_seconds, 1),
    )
    report["by_type"] = _by_type(landed, states, media_dir)
    report["provisional"] = True
    runner.write_report(report, Path(args.out) / f"{args.stage}.json")
    print(json.dumps({k: report[k] for k in
                      ("stage", "items", "drain_seconds", "backpressure")}, indent=2))
    return 0


def _await_terminal(base_url: str, upload_ids: list[str], deadline: float) -> dict[str, dict]:
    """Poll until every upload is terminal or the deadline passes."""
    states: dict[str, dict] = {}
    started = time.monotonic()
    pending = list(upload_ids)
    while pending and (time.monotonic() - started) < deadline:
        fresh = oracle.poll_status(base_url, pending)
        states.update(fresh)
        pending = [u for u in pending
                   if fresh.get(u, {}).get("state") not in oracle.TERMINAL_STATES]
        if pending:
            time.sleep(POLL_SECONDS)
    for u in pending:
        states.setdefault(u, {"state": "timeout"})
    return states


def _verify(base_url, upload_dir, landed, states):
    gallery = oracle.gallery_filenames(base_url)
    verdicts, to_published = [], []
    for key, (payload, attempt, finished_at) in landed.items():
        info = states.get(key, {"state": "unknown"})
        state = info.get("state", "unknown")
        media_id = info.get("mediaId") or ""
        published_ok = oracle.classify(state)
        digest_ok = False
        if published_ok and media_id:
            digest_ok, _ = oracle.verify_download(base_url, media_id, payload.sha256_hex)
        verdicts.append(oracle.ItemVerdict(
            upload_id=attempt.upload_id, filename=payload.filename, media_id=media_id,
            state=state, transport_ok=attempt.transport_ok, published_ok=published_ok,
            digest_ok=digest_ok, gallery_ok=payload.filename in gallery,
            source_gone=(oracle.source_removed(upload_dir, attempt.upload_id)
                         if attempt.upload_id else False),
        ))
        if published_ok:
            to_published.append(time.monotonic() - finished_at)
    return verdicts, to_published


def _backpressure(landed) -> dict[str, int]:
    counts: dict[str, int] = {}
    for _, attempt, _ in landed.values():
        for status in attempt.statuses:
            if status in tusclient.BACKPRESSURE:
                counts[str(status)] = counts.get(str(status), 0) + 1
    return counts


def _unexpected_5xx(landed) -> int:
    return sum(1 for _, attempt, _ in landed.values() for status in attempt.statuses
               if status >= 500 and status not in tusclient.BACKPRESSURE)


def _by_type(landed, states, media_dir) -> dict[str, dict]:
    out: dict[str, dict] = {}
    for key, (payload, _attempt, _) in landed.items():
        row = out.setdefault(payload.mime, {"uploaded": 0, "published": 0, "with_thumbnail": 0})
        row["uploaded"] += 1
        info = states.get(key, {})
        if oracle.classify(info.get("state", "")):
            row["published"] += 1
            media_id = info.get("mediaId") or ""
            if media_id and observe.thumbnail_exists(media_dir, media_id):
                row["with_thumbnail"] += 1
    return out


if __name__ == "__main__":
    raise SystemExit(main())
```

- [ ] **Step 8: Implement `finalize.py` — the authoritative verdict**

```python
# loadtest/wedding/finalize.py
"""Merge host-collected app logs into a stage report and decide pass/fail.

Runs on the host because the sidecar has no Docker socket. This, not stage.py,
produces the authoritative `passed`.
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path

from . import observe
from .runner import decide_passed


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--report", required=True)
    ap.add_argument("--logs", required=True)
    args = ap.parse_args()

    report_path = Path(args.report)
    report = json.loads(report_path.read_text())
    lines = Path(args.logs).read_text(errors="replace").splitlines()

    report["log_levels"] = observe.count_levels(lines)
    report["queue_samples"] = [{"time": q.time, **q.fields}
                               for q in observe.parse_queue_samples(lines)]
    report["passed"] = decide_passed(report)
    report["provisional"] = False
    report_path.write_text(json.dumps(report, indent=2))

    print(json.dumps({
        "stage": report["stage"], "passed": report["passed"],
        "verified": report["items"]["verified"], "total": report["items"]["total"],
        "drain_seconds": report["drain_seconds"],
        "errors": report["log_levels"].get("ERROR", 0),
    }, indent=2))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
```

- [ ] **Step 9: Write the host entry script**

```powershell
# loadtest/run_wedding.ps1
# Generates base assets on the host, runs a stage inside a sidecar container on
# the app's own network (the app publishes no host port), then merges the app's
# logs in and decides pass/fail.
param(
    [Parameter(Mandatory = $true)][string]$Stage,
    [string]$Network   = 'wedding-gallery_edge',
    [string]$BaseUrl   = 'http://app:8080',
    [string]$UploadDir = '<data-dir>\uploads',
    [string]$MediaDir  = '<data-dir>\media',
    [string]$Container = 'wedding-gallery-app-1'
)
$ErrorActionPreference = 'Stop'
$repo    = Split-Path -Parent $PSScriptRoot
$results = Join-Path $repo 'loadtest\results'
New-Item -ItemType Directory -Force -Path $results | Out-Null
Push-Location $repo   # `python -m loadtest.wedding.*` resolves from the repo root
try {

Write-Host '==> generating base assets on the host (idempotent)'
python -c "import sys; sys.path.insert(0, r'$repo'); from pathlib import Path; from loadtest.wedding.corpus import build_assets; build_assets(Path(r'$repo\loadtest\assets'))"

$since = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')

Write-Host "==> running stage $Stage"
docker run --rm `
    --network $Network `
    -v "${repo}:/work:ro" `
    -v "${results}:/results" `
    -v "${UploadDir}:/uploads:ro" `
    -v "${MediaDir}:/media:ro" `
    -w /work `
    python:3.13-slim `
    python -m loadtest.wedding.stage --stage $Stage --base-url $BaseUrl `
        --upload-dir /uploads --media-dir /media --out /results

Write-Host '==> collecting app logs and finalizing'
$logFile = Join-Path $results "$Stage.log"
docker logs --since $since $Container 2>&1 | Out-File -FilePath $logFile -Encoding utf8

python -m loadtest.wedding.finalize --report (Join-Path $results "$Stage.json") --logs $logFile
$code = $LASTEXITCODE

} finally { Pop-Location }
exit $code
```

- [ ] **Step 10: Document it**

Add a section to `loadtest/README.md` headed `## Wedding load proof`, stating: the five-criteria oracle and why transport success is insufficient; that assets are generated once into `loadtest/assets/` (add that path to `loadtest/.gitignore`); that the harness runs as a sidecar because the app publishes no host port; that `finalize` produces the authoritative verdict; and the 50 GB abort floor.

- [ ] **Step 11: Run the whole harness suite**

Run: `python -m unittest discover -s loadtest/wedding/tests -t . -v`
Expected: every test from Tasks 1-5 PASSES.

- [ ] **Step 12: Commit**

```bash
git add loadtest/wedding/tusclient.py loadtest/wedding/runner.py loadtest/wedding/stage.py loadtest/wedding/finalize.py loadtest/wedding/tests/test_runner.py loadtest/run_wedding.ps1 loadtest/README.md loadtest/.gitignore
git commit -m "feat(loadtest): wedding stage runner with a publication-based oracle"
```

---

### Task 6: Apply the configuration change and run Stage 0

**Files:**
- Modify: Portainer stack environment for stack `wedding-gallery` (via `stack.env`, and the Portainer UI for permanence)
- Create: `loadtest/results/00-baseline.json`

**Interfaces:**
- Consumes: the harness from Tasks 1-5
- Produces: a recorded baseline, and a running stack with 16 media workers

- [ ] **Step 1: Record the baseline before changing anything**

Run and save the output verbatim into `loadtest/results/00-baseline.json`:

```powershell
$free = (Get-PSDrive D).Free
$gallery = docker exec wedding-gallery-app-1 sh -c 'wget -qO- http://127.0.0.1:8080/api/gallery'
$levels  = docker logs wedding-gallery-app-1 2>&1 | Select-String -Pattern '"level":"ERROR"' | Measure-Object | Select-Object -ExpandProperty Count
[pscustomobject]@{ free_bytes = $free; error_lines = $levels; gallery = $gallery } | ConvertTo-Json -Depth 5
```

Expected: `free_bytes` ≈ 390000000000, `error_lines` 0.

- [ ] **Step 2: Verify the quiescence witness is live in production**

This directly exercises the most serious defect found in review — a rowless, sidecar-less partial must not be adopted until it is older than the retention policy *and* unchanged across two reconcile passes.

```powershell
# A fresh, rowless partial with no sidecar.
$id = 'battlewitness' + (Get-Random -Maximum 99999)
[IO.File]::WriteAllBytes("<data-dir>\uploads\$id", (New-Object byte[] 1048576))
Start-Sleep -Seconds 45   # three reconcile passes at 15s
docker exec wedding-gallery-app-1 sh -c "wget -qO- --post-data='{\"uploadIds\":[\"$id\"]}' --header='Content-Type: application/json' http://127.0.0.1:8080/api/uploads/status"
Test-Path "<data-dir>\uploads\$id"
```

Expected: status reports `unknown` (never adopted), and the file still exists. If instead it reports `published`, **stop the campaign** — the deployed build predates the fix and would publish truncated files while deleting their sources.

Then remove it: `Remove-Item "<data-dir>\uploads\$id"`

- [ ] **Step 3: Change the two worker settings**

Edit `stack.env` in place without reading it — it contains the admin password, hook secret, and tunnel token:

```powershell
docker run --rm -v portainer_data:/pd alpine sh -c "
  sed -i '/^MEDIA_PROCESSING_WORKERS=/d;/^UPLOAD_DURABILITY_WORKERS=/d' /pd/compose/8/stack.env
  printf 'MEDIA_PROCESSING_WORKERS=16\nUPLOAD_DURABILITY_WORKERS=8\n' >> /pd/compose/8/stack.env
"
```

- [ ] **Step 4: Recreate the app so the new values take effect**

```powershell
docker run --rm -v portainer_data:/pd -v /var/run/docker.sock:/var/run/docker.sock `
  -w /pd/compose/8 docker:cli `
  docker compose -p wedding-gallery --env-file stack.env up -d --force-recreate app
```

- [ ] **Step 5: Verify the change took**

Run: `docker inspect wedding-gallery-app-1 --format '{{range .Config.Env}}{{println .}}{{end}}' | Select-String 'MEDIA_PROCESSING_WORKERS|UPLOAD_DURABILITY_WORKERS'`
Expected: `MEDIA_PROCESSING_WORKERS=16` and `UPLOAD_DURABILITY_WORKERS=8`.

Then confirm readiness: `docker exec wedding-gallery-app-1 sh -c 'wget -qO- http://127.0.0.1:8080/readyz'` → `{"status":"ready"}`.

- [ ] **Step 6: Flag the permanence caveat**

A Portainer redeploy regenerates `stack.env` from `portainer.db` and would revert Step 3. Record in the task report that the same two variables must be set in the Portainer UI under stack `wedding-gallery` → Environment variables for the change to survive. Do not write to `portainer.db` directly.

- [ ] **Step 7: End-to-end smoke of one item per media type**

Run: `pwsh loadtest/run_wedding.ps1 -Stage smoke`

Expected: one JPEG, PNG, WebP, HEIC, MP4 and MOV each pass all five oracle criteria. Record which types produced a thumbnail — HEIC is expected not to, and that is a reported finding rather than a failure.

- [ ] **Step 8: Commit the baseline**

```bash
git add loadtest/results/00-baseline.json
git commit -m "test(loadtest): record the pre-run baseline and verify the quiescence witness"
```

---

### Task 7: Stage 1 — calibration

**Files:**
- Create: `loadtest/results/01-calibration.json`

**Interfaces:**
- Consumes: Task 6's configured stack
- Produces: measured per-item derive cost and bind-mount throughput; the evidence that settles the I/O-bound hypothesis

- [ ] **Step 1: Run 100 items at concurrency 1**

Run: `pwsh loadtest/run_wedding.ps1 -Stage calibrate-serial`

Records per-item transport seconds and time-to-published for ~92 photos and ~8 videos, one at a time.

- [ ] **Step 2: Run 100 items at concurrency 16**

Run: `pwsh loadtest/run_wedding.ps1 -Stage calibrate-parallel`

- [ ] **Step 3: Compute the scaling efficiency and decide**

From the two reports, compute:

- `serial_rate` = items / total seconds at concurrency 1
- `parallel_rate` = items / total seconds at concurrency 16
- `efficiency` = `parallel_rate / (serial_rate * 16)`

Interpretation, recorded explicitly in the report:

- efficiency > 0.6 → CPU-bound and scaling well; keep 16 workers.
- efficiency 0.3-0.6 → partially I/O-bound; keep 16 but expect diminishing returns.
- efficiency < 0.3 → **the hypothesis holds and the bind mount is the bottleneck**; reduce `MEDIA_PROCESSING_WORKERS` to 8 and re-run this stage once, because more workers past the I/O ceiling only add contention.

- [ ] **Step 4: Project the wedding drain time**

Compute `projected_drain_seconds = 5000 / parallel_rate` and record it against the one-hour threshold. If the projection exceeds one hour, say so **now**, before Stage 2 — a predicted failure recorded in advance is evidence; one discovered afterwards looks like an excuse.

- [ ] **Step 5: Commit**

```bash
git add loadtest/results/01-calibration.json
git commit -m "test(loadtest): calibration measurements and the worker-count decision"
```

---

### Task 8: Stage 2 — the wedding

**Files:**
- Create: `loadtest/results/02-wedding.json`

**Interfaces:**
- Consumes: the worker count chosen in Task 7
- Produces: the headline result

- [ ] **Step 1: Confirm the disk floor before starting**

Run: `(Get-PSDrive D).Free / 1GB`
Expected: greater than 150. If not, stop — peak need is ~118 GB.

- [ ] **Step 2: Run the wedding stage**

Run: `pwsh loadtest/run_wedding.ps1 -Stage wedding`

5,000 items (4,600 photos, 400 videos), 120 guests, Poisson arrivals with `cluster_fraction=0.15`, direct to the container.

- [ ] **Step 3: Watch the drain curve live**

In a second terminal: `docker logs -f wedding-gallery-app-1 2>&1 | Select-String '"msg":"ingest queue"'`

Expected: `pending` rises during upload, peaks near the end of transport, then falls monotonically to zero.

- [ ] **Step 4: Assert the thresholds**

From `02-wedding.json`:

- `passed` is `true`
- `items.verified == items.total == 5000`
- `oracle_failures` is empty
- `unexpected_5xx == 0`
- `log_levels.ERROR == 0`
- `drain_seconds <= 3600`

- [ ] **Step 5: Record the per-type matrix**

Report publish success and thumbnail presence per MIME type. HEIC publishing without a thumbnail is a finding, not a failure.

- [ ] **Step 6: Commit**

```bash
git add loadtest/results/02-wedding.json
git commit -m "test(loadtest): wedding-profile run at 5000 items and 108 GB"
```

---

### Task 9: Stages 3 and 4 — overload and the tunnel

**Files:**
- Create: `loadtest/results/03-overload.json`
- Create: `loadtest/results/04-tunnel.json`

**Interfaces:**
- Consumes: a drained queue from Task 8
- Produces: the headroom multiple and the real-path result

- [ ] **Step 1: Wait for the queue to be fully drained**

Run: `docker logs --tail 5 wedding-gallery-app-1 2>&1 | Select-String '"msg":"ingest queue"'`
Expected: no line, or `pending: 0`. The summary line is suppressed when nothing is in flight, so its absence means idle.

- [ ] **Step 2: Run the overload stage**

Run: `pwsh loadtest/run_wedding.ps1 -Stage overload`

1,000 items at 3× the Stage 2 arrival rate, followed by a 200-item herd with `herd_schedule`.

- [ ] **Step 3: Assert overload behaviour**

Backpressure is *expected* here. Assert:

- every accepted upload still reaches `published` — `oracle_failures` empty
- `unexpected_5xx == 0` — 503s carrying `Retry-After` are counted under `backpressure`, not here
- `log_levels.ERROR == 0`

Record the backpressure counts: they are the headroom measurement.

- [ ] **Step 4: Run the tunnel stage**

First discover the public hostname rather than guessing it:

```powershell
docker exec wedding-gallery-app-1 sh -c 'wget -qO- http://127.0.0.1:8080/api/config' | ConvertFrom-Json
```

If that does not carry the hostname, read it from the Cloudflare dashboard for
the tunnel that `wedding-gallery-cloudflared-1` runs. Then:

Run: `pwsh loadtest/run_wedding.ps1 -Stage tunnel -BaseUrl https://THE-HOSTNAME`

300 items, modest concurrency, through Cloudflare.

- [ ] **Step 5: Assert the timeout chain**

The final PATCH of each upload carries the pre-finish durability barrier. Assert no request exceeded ~100s and that the barrier stayed inside its 75s budget. A request severed at the edge would show as a transport error rather than a documented status, so any such error is a finding.

- [ ] **Step 6: Commit**

```bash
git add loadtest/results/03-overload.json loadtest/results/04-tunnel.json
git commit -m "test(loadtest): overload headroom and tunnel-path results"
```

---

### Task 10: Cleanup, verification, and the final report

**Files:**
- Create: `loadtest/results/05-cleanup.json`
- Create: `docs/superpowers/reports/2026-08-13-wedding-load-proof-results.md`

**Interfaces:**
- Consumes: every stage report
- Produces: the answer to the original question

- [ ] **Step 1: Soft-delete every test item**

Run: `python loadtest/cleanup_battle.py` with `BASE_URL` and `ADMIN_PASSWORD` set as the existing README documents.

- [ ] **Step 2: Permanently delete from Admin Trash**

Use the admin UI's **Delete permanently** on the trashed battle items. Do not edit SQLite or the media directory by hand — purge runs through the storage-health gate and the in-flight upload-job guard, and bypassing it would invalidate the cleanup as a test of that path.

- [ ] **Step 3: Verify the bytes actually came back**

```powershell
$after = (Get-PSDrive D).Free
$before = (Get-Content loadtest/results/00-baseline.json | ConvertFrom-Json).free_bytes
"reclaimed: {0:N1} GB, delta vs baseline: {1:N1} GB" -f (($after) / 1GB), (($after - $before) / 1GB)
```

Expected: delta within ~2 GB of zero. A large shortfall means originals or thumbnails were orphaned — a real finding worth investigating.

- [ ] **Step 4: Verify no residue in the upload directory**

Run: `(Get-ChildItem '<data-dir>\uploads' | Measure-Object).Count`
Expected: 0.

- [ ] **Step 5: Write the results report**

Create `docs/superpowers/reports/2026-08-13-wedding-load-proof-results.md` covering: the headline verdict against the one-hour threshold; measured drain time and throughput; whether the I/O-bound hypothesis held and the final recommended worker counts; the headroom multiple from Stage 3; the tunnel result; the per-type matrix including HEIC thumbnails; and anything that failed, stated plainly rather than explained away.

- [ ] **Step 6: Commit**

```bash
git add loadtest/results/05-cleanup.json docs/superpowers/reports/2026-08-13-wedding-load-proof-results.md
git commit -m "docs: wedding load proof results"
```
