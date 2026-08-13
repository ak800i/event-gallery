"""Payload corpus for the wedding load proof.

Each upload streams a real, decodable base asset followed by a unique 64-byte
marker. Decoders ignore trailing bytes after JPEG's EOI and after MP4's last
atom, so derivation stays as expensive as a real photo while every upload gets
a distinct SHA-256 and so never collapses into the dedupe path.
"""

from __future__ import annotations

import hashlib
import os
import random
import subprocess
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator

MARKER_BYTES = 64
NAME_PREFIX = "event-gallery-battle"

# Real phone output is overwhelmingly JPEG. PNG stays present so the type matrix
# and the offset-0 check keep their teeth, but a third of the corpus being 25 MiB
# PNGs is neither realistic nor affordable.
PHOTO_VARIANT_WEIGHTS = {
    "image/jpeg": 0.70,
    "image/webp": 0.20,
    "image/png": 0.10,
}


@dataclass(frozen=True)
class Asset:
    name: str
    path: Path
    mime: str
    kind: str  # "image" | "video"


class Payload:
    # Hashing every payload from scratch would stream the whole ~125 GiB corpus
    # over five distinct files. Key on identity so a regenerated base is never
    # served a stale digest.
    _base_states: dict[tuple[str, int, int], "hashlib._Hash"] = {}

    def __init__(self, base: Asset, marker: bytes, filename: str):
        if len(marker) != MARKER_BYTES:
            raise ValueError(f"marker must be {MARKER_BYTES} bytes, got {len(marker)}")
        self.base = base
        self.marker = marker
        self.filename = filename
        self.mime = base.mime
        self.kind = base.kind
        stat = base.path.stat()
        self._size = stat.st_size + MARKER_BYTES
        self._digest = self._compute_digest(stat)

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

    def _compute_digest(self, stat: os.stat_result) -> str:
        # The marker is always appended, so copying the base state and updating
        # it with the marker gives the identical digest for a fraction of the IO.
        h = self._base_state(stat).copy()
        h.update(self.marker)
        return h.hexdigest()

    def _base_state(self, stat: os.stat_result):
        key = (str(self.base.path), stat.st_size, stat.st_mtime_ns)
        state = Payload._base_states.get(key)
        if state is None:
            state = hashlib.sha256()
            with self.base.path.open("rb") as fh:
                while True:
                    block = fh.read(1 << 20)
                    if not block:
                        break
                    state.update(block)
            Payload._base_states[key] = state
        return state


def make_payloads(
    assets: list[Asset], n_photos: int, n_videos: int, seed: int
) -> list[Payload]:
    rng = random.Random(seed)
    images = [a for a in assets if a.kind == "image"]
    videos = [a for a in assets if a.kind == "video"]
    if n_photos and not images:
        raise ValueError("no image assets available but n_photos > 0")
    if n_videos and not videos:
        raise ValueError("no video assets available but n_videos > 0")
    weights = [PHOTO_VARIANT_WEIGHTS.get(a.mime, 0.0) for a in images]
    out: list[Payload] = []
    for _ in range(n_photos):
        out.append(_one(rng, rng.choices(images, weights=weights)[0]))
    for _ in range(n_videos):
        out.append(_one(rng, rng.choice(videos)))
    rng.shuffle(out)
    return out


def _one(rng: random.Random, base: Asset) -> Payload:
    token = uuid.UUID(int=rng.getrandbits(128)).hex
    marker = (f"{NAME_PREFIX}:{token}".encode("ascii")).ljust(MARKER_BYTES, b"\0")[
        :MARKER_BYTES
    ]
    ext = ".jpg" if base.kind == "image" else ".mp4"
    if base.mime == "image/png":
        ext = ".png"
    elif base.mime == "image/webp":
        ext = ".webp"
    elif base.mime == "video/quicktime":
        ext = ".mov"
    return Payload(base, marker, f"{NAME_PREFIX}-{token}{ext}")


# Sizes are the wedding profile: ~11 MB mean photos, ~206 MB videos.
PHOTO_SIZE = "4000x3000"
VIDEO_SECONDS = 60
VIDEO_TARGET_BYTES = 200 * 1024 * 1024

# -q:v is per-encoder: mjpeg reads 1-31 (1 = best), libwebp reads 0-100 (100 = best).
# png is lossless and ignores the flag entirely, so its value below is inert.
_PHOTO_VARIANTS = [
    ("photo-jpeg", "image/jpeg", "mjpeg", ".jpg", "1"),
    ("photo-png", "image/png", "png", ".png", "1"),
    ("photo-webp", "image/webp", "libwebp", ".webp", "90"),
]
_VIDEO_VARIANTS = [
    ("video-mp4", "video/mp4", ".mp4"),
    ("video-mov", "video/quicktime", ".mov"),
]


def build_assets(
    assets_dir: Path,
    photo_size: str = PHOTO_SIZE,
    video_seconds: int = VIDEO_SECONDS,
    video_bytes: int = VIDEO_TARGET_BYTES,
) -> list[Asset]:
    """Generate the base assets once. Idempotent: existing files are reused."""
    assets_dir.mkdir(parents=True, exist_ok=True)
    out: list[Asset] = []

    for name, mime, encoder, ext, quality in _PHOTO_VARIANTS:
        path = assets_dir / f"{name}{ext}"
        if not path.exists():
            part = assets_dir / f"{name}.part{ext}"
            # testsrc2 plus heavy noise defeats compression, so the file reaches
            # its target size with genuine image data rather than padding.
            _run([
                "ffmpeg", "-y", "-v", "error",
                "-f", "lavfi", "-i", f"testsrc2=s={photo_size},noise=alls=90:allf=t+u",
                "-frames:v", "1", "-c:v", encoder, "-q:v", quality, str(part),
            ])
            os.replace(part, path)
        out.append(Asset(name=name, path=path, mime=mime, kind="image"))

    for name, mime, ext in _VIDEO_VARIANTS:
        path = assets_dir / f"{name}{ext}"
        if not path.exists():
            part = assets_dir / f"{name}.part{ext}"
            bitrate = max(100_000, (video_bytes * 8) // max(1, video_seconds))
            _run([
                "ffmpeg", "-y", "-v", "error",
                "-f", "lavfi", "-i", "testsrc2=s=1920x1080:r=30",
                "-t", str(video_seconds), "-c:v", "libx264", "-preset", "veryfast",
                "-b:v", str(bitrate), "-pix_fmt", "yuv420p", str(part),
            ])
            os.replace(part, path)
        out.append(Asset(name=name, path=path, mime=mime, kind="video"))

    return out


def _run(cmd: list[str]) -> None:
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        raise RuntimeError(f"{cmd[0]} failed: {proc.stderr[:400]}")
