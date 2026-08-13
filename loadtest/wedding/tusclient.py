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

from .backoff import retry_after_seconds

# A connection reset, DNS blip or read timeout, recorded as a status so it is
# retried like any other transient refusal instead of unwinding the pool.
TRANSPORT_ERROR = 0

# 410 Gone is deliberately absent: the completion fence answers it for a
# deterministic final refusal, so retrying could only ever fail again.
RETRYABLE = {TRANSPORT_ERROR, 408, 409, 423, 425, 429, 500, 502, 503, 504}
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


def skipped(reason: str) -> Attempt:
    """An arrival the runner refused to send, e.g. after the disk guard tripped."""
    return Attempt(error=reason)


def _request(method: str, url: str, headers: dict, body: bytes | None = None,
             timeout: float = 600.0):
    """Returns (status, headers, body, note). Never raises: one reset among five
    thousand uploads must cost one item, not the whole campaign, and an
    exception here would unwind the worker and the pool with it."""
    req = urllib.request.Request(url, data=body, method=method)
    for key, value in headers.items():
        req.add_header(key, value)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, {k.lower(): v for k, v in resp.headers.items()}, resp.read(), ""
    except urllib.error.HTTPError as exc:
        return exc.code, {k.lower(): v for k, v in exc.headers.items()}, exc.read(), ""
    except (urllib.error.URLError, OSError) as exc:
        return TRANSPORT_ERROR, {}, b"", f"{method} {type(exc).__name__}: {exc}"


def _backoff(headers: dict, attempt_index: int) -> float:
    """The tus proxy answers 429 with no Retry-After, while the completion fence
    answers 503 with one. Honour the header when it is there, back off when not."""
    return retry_after_seconds({"Retry-After": headers.get("retry-after")},
                               min(2.0 ** attempt_index, 10.0))


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

    if not _create(base_url, payload, attempt, max_retries):
        attempt.transport_seconds = time.monotonic() - started
        return attempt

    offset = 0
    try:
        for block in _blocks(payload, chunk_bytes):
            moved = _patch(base_url, attempt, offset, block, max_retries)
            if moved == offset:
                break
            offset = moved
    except OSError as exc:
        # payload.chunks() reads the base asset from disk, and it is consumed
        # here rather than inside _request, so nothing else catches this. One
        # unreadable asset must cost one item, not the worker and the pool.
        attempt.error = f"reading {payload.filename}: {type(exc).__name__}: {exc}"

    attempt.transport_seconds = time.monotonic() - started
    if offset != payload.size and not attempt.error:
        attempt.error = f"final offset {offset} != {payload.size}"
    return attempt


def _create(base_url: str, payload, attempt: Attempt, max_retries: int) -> bool:
    """A create refused under load is the server pacing us, not a failed item:
    the tus proxy 429s new uploads at exactly the concurrency the overload and
    herd stages are built to exceed. Only a status we received is retried, so a
    refusal can never leave an orphan upload behind."""
    note = ""
    for i in range(max_retries):
        status, headers, _, note = _request("POST", f"{base_url}/api/tus", {
            "Tus-Resumable": "1.0.0",
            "Upload-Length": str(payload.size),
            "Upload-Metadata": _metadata(payload),
            "Content-Length": "0",
        })
        attempt.statuses.append(status)
        if status == 201:
            attempt.upload_id = headers.get("location", "").rstrip("/").rsplit("/", 1)[-1]
            if not attempt.upload_id:
                attempt.error = "create returned no Location header"
                return False
            return True
        if status not in RETRYABLE:
            attempt.error = f"create returned {status}"
            return False
        attempt.retries += 1
        if i < max_retries - 1:
            time.sleep(_backoff(headers, i))
    attempt.error = f"create retries exhausted{_because(note)}"
    return False


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


def _head_offset(base_url: str, upload_id: str) -> int | None:
    """The durable offset tusd will accept next, or None if it would not say."""
    status, headers, _, _note = _request("HEAD", f"{base_url}/api/tus/{upload_id}",
                                         {"Tus-Resumable": "1.0.0"}, timeout=60.0)
    if status not in (200, 204):
        return None
    try:
        return int(headers.get("upload-offset", ""))
    except (TypeError, ValueError):
        return None


def _because(note: str) -> str:
    return f" ({note})" if note else ""


def _int_or(raw, fallback: int) -> int:
    try:
        return int(raw)
    except (TypeError, ValueError):
        return fallback


def _patch(base_url: str, attempt: Attempt, offset: int, body: bytes, max_retries: int) -> int:
    note = ""
    expected = offset + len(body)
    for i in range(max_retries):
        status, headers, _, note = _request(
            "PATCH", f"{base_url}/api/tus/{attempt.upload_id}", {
                "Tus-Resumable": "1.0.0",
                "Upload-Offset": str(offset),
                "Content-Type": "application/offset+octet-stream",
            }, body=body)
        attempt.statuses.append(status)
        if status == 204:
            # Same divergence guard as the HEAD branch below. An acknowledged
            # offset that is not exactly what we sent desynchronizes _blocks'
            # sequential cursor, and every following block lands in the wrong
            # place -- caught downstream by the digest, but only after 5000
            # uploads have been written wrong.
            moved = _int_or(headers.get("upload-offset"), expected)
            if moved != expected:
                attempt.error = f"offset diverged: server {moved}, client {expected}"
                return offset
            return expected
        if status not in RETRYABLE:
            attempt.error = f"PATCH returned {status}"
            return offset
        attempt.retries += 1
        if i < max_retries - 1:
            time.sleep(_backoff(headers, i))
        # Reconcile against the durable offset before re-sending. A 204 lost on
        # the way back leaves the bytes written, and re-sending them would draw
        # 409 until the retries ran out -- a false transport failure on an
        # upload the server has.
        durable = _head_offset(base_url, attempt.upload_id)
        if durable == expected:
            return durable
        if durable is not None and durable != offset:
            attempt.error = f"offset diverged: server {durable}, client {offset}"
            return offset
    attempt.error = f"PATCH retries exhausted{_because(note)}"
    return offset
