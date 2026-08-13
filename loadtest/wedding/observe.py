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

# media.Processor.ThumbnailsDir; files are <id>.jpg.
THUMBNAILS_DIRNAME = "thumbnails"


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
    """Feed this `docker logs <app>`; `docker compose logs` prefixes each line
    with the service name, and every line would then be counted as unparseable."""
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
    thumbs = Path(media_dir) / THUMBNAILS_DIRNAME
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
