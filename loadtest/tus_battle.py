#!/usr/bin/env python3
"""Dependency-free production tus load/resume harness.

Creates valid PNG uploads with sparse generated payloads, sends standard tus
POST/HEAD/PATCH/DELETE requests through the public endpoint, reconciles offsets
on retry, and emits a machine-readable stage report.
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import json
import math
import os
import ssl
import statistics
import sys
import time
import uuid
from collections import Counter
from dataclasses import asdict, dataclass, field
from pathlib import Path
from urllib.parse import urljoin, urlparse

BASE_PNG = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
)
ZERO_BLOCK = bytes(64 * 1024)
TUS_VERSION = "1.0.0"
DEFAULT_CHUNK_SIZE = 8 * 1024 * 1024
RETRYABLE = {408, 409, 423, 425, 429, 500, 502, 503, 504}


@dataclass
class Response:
    status: int
    headers: dict[str, str]
    body: bytes
    duration: float


@dataclass
class UploadResult:
    name: str
    success: bool = False
    upload_url: str = ""
    final_offset: int = 0
    retries: int = 0
    bytes_sent: int = 0
    create_seconds: float = 0.0
    patch_seconds: list[float] = field(default_factory=list)
    head_seconds: list[float] = field(default_factory=list)
    statuses: dict[str, int] = field(default_factory=dict)
    error: str = ""

    def add_status(self, status: int) -> None:
        key = str(status)
        self.statuses[key] = self.statuses.get(key, 0) + 1


class TusBattle:
    def __init__(self, base_url: str, timeout: float, chunk_size: int, state_path: Path):
        self.base = base_url.rstrip("/")
        parsed = urlparse(self.base)
        if parsed.scheme != "https" or not parsed.hostname:
            raise ValueError("BASE_URL must be an https URL")
        self.host = parsed.hostname
        self.port = parsed.port or 443
        self.base_path = parsed.path.rstrip("")
        self.timeout = timeout
        self.chunk_size = chunk_size
        self.ssl_context = ssl.create_default_context()
        self.state_path = state_path
        self.state_lock = asyncio.Lock()
        self.state: dict[str, dict[str, object]] = {}

    async def save_state(self, key: str, **values: object) -> None:
        async with self.state_lock:
            current = self.state.setdefault(key, {})
            current.update(values)
            tmp = self.state_path.with_suffix(".tmp")
            tmp.parent.mkdir(parents=True, exist_ok=True)
            tmp.write_text(json.dumps(self.state, indent=2, sort_keys=True))
            tmp.replace(self.state_path)

    async def request(
        self,
        method: str,
        url_or_path: str,
        headers: dict[str, str] | None = None,
        body_length: int = 0,
        body_factory=None,
    ) -> Response:
        parsed = urlparse(url_or_path if "://" in url_or_path else urljoin(self.base + "/", url_or_path.lstrip("/")))
        if parsed.hostname != self.host:
            raise RuntimeError(f"refusing unexpected upload host {parsed.hostname!r}")
        path = parsed.path or "/"
        if parsed.query:
            path += "?" + parsed.query
        request_headers = {
            "Host": self.host,
            "User-Agent": "event-gallery-tus-battle/1.0",
            "Accept": "application/json, */*",
            "Connection": "close",
            "Content-Length": str(body_length),
        }
        if headers:
            request_headers.update(headers)
        raw_headers = "".join(f"{key}: {value}\r\n" for key, value in request_headers.items())
        started = time.perf_counter()

        async def exchange() -> Response:
            reader, writer = await asyncio.open_connection(
                self.host, self.port, ssl=self.ssl_context, server_hostname=self.host
            )
            try:
                writer.write(f"{method} {path} HTTP/1.1\r\n{raw_headers}\r\n".encode("ascii"))
                await writer.drain()
                if body_factory is not None:
                    async for block in body_factory():
                        writer.write(block)
                        await writer.drain()
                status_line = await reader.readline()
                if not status_line:
                    raise ConnectionError("empty HTTP response")
                parts = status_line.decode("iso-8859-1").rstrip().split(" ", 2)
                status = int(parts[1])
                response_headers: dict[str, str] = {}
                while True:
                    line = await reader.readline()
                    if line in (b"\r\n", b"\n", b""):
                        break
                    key, value = line.decode("iso-8859-1").split(":", 1)
                    response_headers[key.lower().strip()] = value.strip()
                body = b""
                if method != "HEAD":
                    if "content-length" in response_headers:
                        length = int(response_headers["content-length"])
                        body = await reader.readexactly(length) if length else b""
                    elif response_headers.get("transfer-encoding", "").lower() == "chunked":
                        chunks = []
                        while True:
                            size_line = await reader.readline()
                            size = int(size_line.split(b";", 1)[0], 16)
                            if size == 0:
                                await reader.readline()
                                break
                            chunks.append(await reader.readexactly(size))
                            await reader.readexactly(2)
                        body = b"".join(chunks)
                    else:
                        body = await reader.read()
                return Response(status, response_headers, body, time.perf_counter() - started)
            finally:
                writer.close()
                try:
                    await writer.wait_closed()
                except Exception:
                    pass

        return await asyncio.wait_for(exchange(), timeout=self.timeout)

    @staticmethod
    def metadata(filename: str, guest: str) -> str:
        enc = lambda value: base64.b64encode(value.encode("utf-8")).decode("ascii")
        return f"filename {enc(filename)},guestName {enc(guest)}"

    @staticmethod
    def payload_factory(total_size: int, offset: int, length: int, marker: bytes):
        async def payload():
            end = offset + length
            position = offset
            marker_start = total_size - len(marker)
            while position < end:
                block_end = min(position + len(ZERO_BLOCK), end)
                block_length = block_end - position
                overlaps_png = position < len(BASE_PNG) and block_end > 0
                overlaps_marker = position < total_size and block_end > marker_start
                if overlaps_png or overlaps_marker:
                    block = bytearray(block_length)
                    png_from = max(position, 0)
                    png_to = min(block_end, len(BASE_PNG))
                    if png_to > png_from:
                        block[png_from - position : png_to - position] = BASE_PNG[png_from:png_to]
                    mark_from = max(position, marker_start)
                    mark_to = min(block_end, total_size)
                    if mark_to > mark_from:
                        source_start = mark_from - marker_start
                        block[mark_from - position : mark_to - position] = marker[source_start : source_start + mark_to - mark_from]
                    yield bytes(block)
                else:
                    yield ZERO_BLOCK[:block_length]
                position = block_end
                await asyncio.sleep(0)

        return payload

    async def create(self, filename: str, size: int, guest: str, result: UploadResult) -> str:
        for attempt in range(7):
            response = await self.request(
                "POST",
                "/api/tus/",
                {
                    "Tus-Resumable": TUS_VERSION,
                    "Upload-Length": str(size),
                    "Upload-Metadata": self.metadata(filename, guest),
                },
            )
            result.add_status(response.status)
            result.create_seconds += response.duration
            if response.status == 201:
                location = response.headers.get("location", "")
                if not location:
                    raise RuntimeError("create response missing Location")
                absolute = urljoin(self.base + "/", location)
                if not urlparse(absolute).path.startswith("/api/tus/"):
                    raise RuntimeError(f"unsafe Location path {absolute}")
                return absolute
            if response.status not in RETRYABLE:
                raise RuntimeError(f"create failed HTTP {response.status}: {response.body[:200]!r}")
            result.retries += 1
            await asyncio.sleep(min(2**attempt, 10))
        raise RuntimeError("create retry budget exhausted")

    async def head(self, upload_url: str, result: UploadResult) -> int:
        response = await self.request("HEAD", upload_url, {"Tus-Resumable": TUS_VERSION})
        result.add_status(response.status)
        result.head_seconds.append(response.duration)
        if response.status != 200:
            raise RuntimeError(f"HEAD failed HTTP {response.status}")
        return int(response.headers.get("upload-offset", "-1"))

    async def delete(self, upload_url: str, result: UploadResult) -> None:
        try:
            response = await self.request("DELETE", upload_url, {"Tus-Resumable": TUS_VERSION})
            result.add_status(response.status)
        except Exception:
            pass

    async def patch(self, upload_url: str, total_size: int, offset: int, length: int, marker: bytes, result: UploadResult) -> int:
        for attempt in range(9):
            try:
                response = await self.request(
                    "PATCH",
                    upload_url,
                    {
                        "Tus-Resumable": TUS_VERSION,
                        "Upload-Offset": str(offset),
                        "Content-Type": "application/offset+octet-stream",
                    },
                    body_length=length,
                    body_factory=self.payload_factory(total_size, offset, length, marker),
                )
                result.add_status(response.status)
                result.patch_seconds.append(response.duration)
                result.bytes_sent += length
                if response.status == 204:
                    next_offset = int(response.headers.get("upload-offset", "-1"))
                    if next_offset < offset or next_offset > total_size:
                        raise RuntimeError(f"invalid Upload-Offset {next_offset}")
                    return next_offset
                if response.status not in RETRYABLE:
                    raise RuntimeError(f"PATCH failed HTTP {response.status}: {response.body[:200]!r}")
            except (OSError, asyncio.TimeoutError, ConnectionError) as exc:
                result.error = f"transient PATCH error: {exc}"
            result.retries += 1
            await asyncio.sleep(min(2**attempt, 10))
            try:
                reconciled = await self.head(upload_url, result)
                if reconciled != offset:
                    return reconciled
            except Exception:
                pass
        raise RuntimeError("PATCH retry budget exhausted")

    async def upload_one(self, stage: str, index: int, size: int, force_resume: bool) -> UploadResult:
        token = f"{stage}-{index}-{uuid.uuid4().hex[:12]}"
        filename = f"event-gallery-battle-{token}.png"
        marker = ("EVENT-GALLERY-BATTLE:" + token).encode("ascii")
        result = UploadResult(name=filename)
        state_key = token
        try:
            upload_url = await self.create(filename, size, "load-test", result)
            result.upload_url = upload_url
            await self.save_state(state_key, filename=filename, url=upload_url, size=size, offset=0, done=False)
            offset = 0
            chunk_number = 0
            while offset < size:
                length = min(self.chunk_size, size - offset)
                next_offset = await self.patch(upload_url, size, offset, length, marker, result)
                if next_offset == offset:
                    raise RuntimeError("server offset did not advance")
                offset = next_offset
                chunk_number += 1
                result.final_offset = offset
                await self.save_state(state_key, offset=offset)
                if force_resume and chunk_number == 1 and offset < size:
                    # Prove that a separate HEAD request discovers the durable
                    # offset before continuing on fresh connections.
                    resumed = await self.head(upload_url, result)
                    if resumed != offset:
                        raise RuntimeError(f"resume offset mismatch: HEAD={resumed}, local={offset}")
                    await asyncio.sleep(1)
            result.success = offset == size
            result.error = ""
            await self.save_state(state_key, offset=offset, done=True)
        except Exception as exc:
            result.error = str(exc)
            if result.upload_url:
                await self.delete(result.upload_url, result)
            await self.save_state(state_key, error=result.error, done=False)
        return result


def percentile(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, math.ceil(p * len(ordered)) - 1))
    return ordered[index]


async def run(args: argparse.Namespace) -> int:
    state_path = Path(args.state)
    battle = TusBattle(args.base_url, args.timeout, args.chunk_mb * 1024 * 1024, state_path)
    size = args.size_mb * 1024 * 1024
    started = time.perf_counter()
    tasks = [battle.upload_one(args.stage, index, size, args.resume and index == 0) for index in range(args.count)]
    results = await asyncio.gather(*tasks)
    elapsed = time.perf_counter() - started
    patches = [duration for result in results for duration in result.patch_seconds]
    heads = [duration for result in results for duration in result.head_seconds]
    status_counts: Counter[str] = Counter()
    for result in results:
        status_counts.update(result.statuses)
    success_count = sum(result.success for result in results)
    report = {
        "stage": args.stage,
        "base_url": args.base_url,
        "count": args.count,
        "size_mb_each": args.size_mb,
        "chunk_mb": args.chunk_mb,
        "elapsed_seconds": round(elapsed, 3),
        "success": success_count,
        "failed": len(results) - success_count,
        "success_rate": round(success_count / len(results), 4),
        "logical_megabytes": args.count * args.size_mb,
        "wire_megabytes_sent": round(sum(result.bytes_sent for result in results) / 1024 / 1024, 3),
        "throughput_mib_s": round((args.count * args.size_mb) / elapsed, 3) if elapsed else 0,
        "retries": sum(result.retries for result in results),
        "http_statuses": dict(sorted(status_counts.items())),
        "patch_seconds": {
            "count": len(patches),
            "median": round(statistics.median(patches), 3) if patches else 0,
            "p95": round(percentile(patches, 0.95), 3),
            "max": round(max(patches), 3) if patches else 0,
        },
        "head_seconds": {
            "count": len(heads),
            "p95": round(percentile(heads, 0.95), 3),
        },
        "failures": [{"name": result.name, "error": result.error, "statuses": result.statuses} for result in results if not result.success],
        "files": [result.name for result in results],
    }
    print(json.dumps(report, indent=2, sort_keys=True))
    if args.json_out:
        Path(args.json_out).parent.mkdir(parents=True, exist_ok=True)
        Path(args.json_out).write_text(json.dumps(report, indent=2, sort_keys=True))

    retryable_failures = sum(int(status_counts.get(str(code), 0)) for code in (500, 502, 503, 504))
    request_count = max(1, sum(status_counts.values()))
    passed = report["success_rate"] >= args.min_success_rate and retryable_failures / request_count <= args.max_5xx_rate
    return 0 if passed else 2


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", default=os.getenv("BASE_URL"))
    parser.add_argument("--stage", required=True)
    parser.add_argument("--count", type=int, required=True)
    parser.add_argument("--size-mb", type=int, required=True)
    parser.add_argument("--chunk-mb", type=int, default=8)
    parser.add_argument("--resume", action="store_true")
    parser.add_argument("--timeout", type=float, default=600)
    parser.add_argument("--min-success-rate", type=float, default=0.95)
    parser.add_argument("--max-5xx-rate", type=float, default=0.02)
    parser.add_argument("--state", default="loadtest/results/state.json")
    parser.add_argument("--json-out", default="")
    args = parser.parse_args()
    if not args.base_url:
        parser.error("set BASE_URL or pass --base-url")
    if args.count < 1 or args.size_mb < 1 or args.chunk_mb < 1:
        parser.error("count, size-mb, and chunk-mb must be positive")
    return args


if __name__ == "__main__":
    try:
        raise SystemExit(asyncio.run(run(parse_args())))
    except KeyboardInterrupt:
        print("interrupted", file=sys.stderr)
        raise SystemExit(130)
