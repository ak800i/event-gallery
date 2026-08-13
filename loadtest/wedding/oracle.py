"""The completion oracle.

Transport success is never sufficient. The July incident reported every upload
successful while seventeen sources were deleted, because the only check was
`offset == size`. Criterion 5 -- the tus source is gone -- is what would have
caught it, so it is a first-class requirement here.
"""

from __future__ import annotations

import hashlib
import json
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path

TERMINAL_STATES = {"published", "duplicate", "failed", "cancelled"}
SUCCESS_STATES = {"published", "duplicate"}
STATUS_BATCH_MAX = 100


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


def poll_status(base_url: str, upload_ids: list[str], batch_size: int = STATUS_BATCH_MAX,
                timeout: float = 30.0) -> dict[str, dict]:
    """Batched status poll. The endpoint caps a batch at 100 ids."""
    if batch_size > STATUS_BATCH_MAX:
        raise ValueError(f"batch_size must be <= {STATUS_BATCH_MAX}")
    # A non-positive size yields an empty range, so poll_status would report
    # nothing rather than failing -- the one thing an oracle must never do.
    if batch_size < 1:
        raise ValueError("batch_size must be at least 1")
    out: dict[str, dict] = {}
    for start in range(0, len(upload_ids), batch_size):
        chunk = upload_ids[start:start + batch_size]
        body = json.dumps({"uploadIds": chunk}).encode()
        req = urllib.request.Request(
            f"{base_url}/api/uploads/status", data=body,
            headers={"Content-Type": "application/json"}, method="POST")
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            out.update(json.loads(resp.read()).get("results", {}))
    missing = [u for u in upload_ids if u not in out]
    if missing:
        raise RuntimeError(f"status endpoint omitted {len(missing)} of "
                           f"{len(upload_ids)} ids, first {missing[:3]}")
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
    seen: set[str] = set()
    while True:
        # The server defaults to 30 per page and caps at 100; asking for the cap
        # cuts a 5000-item verification from ~167 rate-limited round trips to ~50.
        url = f"{base_url}/api/gallery?limit=100"
        if cursor:
            url += "&cursor=" + urllib.parse.quote(cursor, safe="")
        with urllib.request.urlopen(url, timeout=timeout) as resp:
            page = json.loads(resp.read())
        for item in page.get("items", []):
            name = item.get("originalFilename") or item.get("filename")
            if name:
                names.add(name)
        cursor = page.get("nextCursor")
        # Only a fresh, non-empty string cursor is worth another round trip.
        # Anything else -- absent, malformed, or one we already followed --
        # would otherwise re-request the same page forever.
        if not isinstance(cursor, str) or not cursor or cursor in seen:
            return names
        seen.add(cursor)


def source_removed(upload_dir: Path, upload_id: str) -> bool:
    """Criterion 5: publication must have removed the tus source and its sidecar."""
    return not (upload_dir / upload_id).exists() and not (upload_dir / f"{upload_id}.info").exists()
