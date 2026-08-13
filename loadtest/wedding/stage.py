"""CLI entry point: run one named stage end to end and write its report.

Log-derived fields are left empty here and filled by `finalize` on the host:
this process runs in a sidecar with no Docker socket, so it cannot read the
app's logs itself. The `passed` value it prints is therefore provisional.
"""
from __future__ import annotations

import argparse
import http.client
import json
import time
import urllib.error
from pathlib import Path

from . import arrivals, corpus, observe, oracle, runner, tusclient

# photos, videos, guests, arrivals/min, cluster fraction, concurrency
STAGES = {
    "smoke":              (3, 3, 6, 600.0, 0.0, 3),
    "calibrate-serial":   (92, 8, 100, 6000.0, 0.0, 1),
    "calibrate-parallel": (92, 8, 100, 6000.0, 0.0, 16),
    # 60/min compresses a busy evening into ~83 minutes of arrivals. Slower
    # would never build a backlog and so would prove nothing; the ceiling is
    # what the overload and herd stages are for.
    "wedding":            (4600, 400, 120, 60.0, 0.15, 40),
    "overload":           (900, 100, 120, 180.0, 0.15, 50),
    "herd":               (180, 20, 200, 1.0, 0.0, 50),
    "tunnel":             (280, 20, 30, 20.0, 0.15, 8),
}

MIN_FREE_BYTES = 50 * 1024 ** 3
CHUNK_BYTES = 8 * 1024 * 1024
POLL_SECONDS = 10.0

# `unknown` means the server holds no job row for this id. It is permanently
# terminal, but deliberately absent from oracle.TERMINAL_STATES because it is a
# lost id rather than an outcome -- so a wait loop that stops only on
# TERMINAL_STATES spins on it until the deadline. `recovering` is the opposite:
# it resolves on its own once the startup inventory finishes, so it must stay
# non-terminal.
WAIT_TERMINAL = oracle.TERMINAL_STATES | {"unknown"}

# What a streaming HTTP read actually raises. http.client.IncompleteRead -- a
# truncated body, the likeliest fault in a 134 GB verification pass -- is an
# HTTPException, which is neither OSError nor URLError, so catching only those
# two lets it escape and destroy the report after the expensive part.
NETWORK_ERRORS = (urllib.error.URLError, OSError, http.client.HTTPException)

# How often the upload directory is sampled while uploads are in flight.
SOURCE_SAMPLE_SECONDS = 0.25


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
    schedule = (arrivals.herd_schedule(len(payloads)) if args.stage == "herd"
                else arrivals.poisson_schedule(len(payloads), rate, seed=1234,
                                               cluster_fraction=cluster))

    def do_upload(index: int):
        return tusclient.upload(args.base_url, payloads[index], CHUNK_BYTES)

    landed: dict[str, tuple] = {}
    lags: list[float] = []
    # Criterion 5 passes on an absence, so the upload directory has to be
    # watched while there is something in it to see. Sampling stops with the
    # upload phase: afterwards the app is meant to have emptied it.
    watch = runner.SourceWatch(lambda: observe.count_tus_sources(upload_dir),
                               interval=SOURCE_SAMPLE_SECONDS)
    with watch:
        for ran in runner.run_schedule(schedule, concurrency, do_upload, guard=guard):
            payload = payloads[ran.index]
            attempt = ran.result if ran.result is not None else tusclient.skipped(ran.skipped)
            key = attempt.upload_id or f"nocreate-{payload.filename}"
            landed[key] = (payload, attempt, ran.finished_at)
            if not ran.skipped:
                lags.append(ran.lag)
    sources = watch.snapshot()

    arrival_lag = {"p50": runner.percentile(lags, 0.50),
                   "p95": runner.percentile(lags, 0.95),
                   "max": round(max(lags), 3) if lags else 0.0}
    transport_errors = sum(
        1 for _, attempt, _ in landed.values()
        for status in attempt.statuses if status == tusclient.TRANSPORT_ERROR)
    # Everything the upload phase paid for, on disk before the verification pass
    # starts. Verification re-reads 134 GB over the network and takes longer than
    # the uploads did; a fault there must not erase what the uploads established.
    runner.write_report({
        "stage": args.stage,
        "phase": "uploads",
        "aborted": guard.tripped,
        "created": len(live_ids(landed)),
        "attempted": len(landed),
        "backpressure": _backpressure(landed),
        "unexpected_5xx": _unexpected_5xx(landed),
        "transport_errors": transport_errors,
        "arrival_lag": arrival_lag,
        "source_observation": sources,
        "disk": {"free_before": free_before},
    }, Path(args.out) / f"{args.stage}-uploads.json")

    last_upload_at = max((f for _, _, f in landed.values()), default=time.monotonic())
    live = live_ids(landed)
    paced: dict[str, int] = {}
    problems: dict[str, int] = {}
    states, terminal_at = _await_terminal(args.base_url, live, args.deadline, paced, problems)
    drain_seconds = time.monotonic() - last_upload_at

    verdicts, to_published, gallery_error = _verify(
        args.base_url, upload_dir, landed, states, terminal_at, paced)

    report = runner.summarize(
        args.stage, verdicts,
        backpressure=_backpressure(landed),
        unexpected_5xx=_unexpected_5xx(landed),
        levels={},          # filled by finalize on the host
        queue=[],           # filled by finalize on the host
        disk=(free_before, observe.disk_free_bytes(upload_dir)),
        to_published=to_published,
        drain_seconds=round(drain_seconds, 1),
        aborted=guard.tripped,
        source_observation=sources,
    )
    report["by_type"] = _by_type(landed, states, media_dir)
    report["verification_backpressure"] = paced
    report["poll_problems"] = problems
    report["gallery_unavailable"] = gallery_error
    # Concurrency is a ceiling on offered load, not a target. Lag near zero means
    # the schedule set the pace; lag that grows means the pool did.
    report["arrival_lag"] = arrival_lag
    report["transport_errors"] = transport_errors
    report["provisional"] = True
    runner.write_report(report, Path(args.out) / f"{args.stage}.json")
    print(json.dumps({k: report[k] for k in
                      ("stage", "items", "drain_seconds", "backpressure", "aborted",
                       "source_observation")}, indent=2))
    return 0


def live_ids(landed) -> list[str]:
    return [key for key, (_p, attempt, _f) in landed.items() if attempt.upload_id]


def _count(paced: dict[str, int]):
    def note(status: int) -> None:
        paced[str(status)] = paced.get(str(status), 0) + 1
    return note


def _discard(exc: BaseException) -> None:
    """An HTTPError wraps a live response. Dropping one without closing it holds
    the connection until the collector notices, and a verification pass across
    thousands of items discards a lot of them."""
    closer = getattr(exc, "close", None)
    if closer is not None:
        closer()


def _await_terminal(base_url: str, upload_ids: list[str], deadline: float,
                    paced: dict[str, int], problems: dict[str, int]
                    ) -> tuple[dict[str, dict], dict[str, float]]:
    """Poll until every upload is terminal or the deadline passes.

    Returns the last state seen for each id and when each first went terminal,
    which is the only honest measure of publication latency available: reading
    the clock during the verification pass instead would time the verifier.

    It carries a systematic positive bias: an item is recorded as terminal at
    the poll that observed it, so up to POLL_SECONDS plus one full sweep of
    50-id batches later than it really became terminal. Publication latency is
    therefore an upper bound, never an optimistic one.
    """
    states: dict[str, dict] = {}
    terminal_at: dict[str, float] = {}
    started = time.monotonic()
    pending = list(upload_ids)
    while pending and (time.monotonic() - started) < deadline:
        try:
            fresh = runner.call_with_backpressure(
                lambda: oracle.poll_status(base_url, pending),
                on_backpressure=_count(paced))
        except Exception as exc:  # noqa: BLE001 - see below
            # This loop is deadline-bounded and retries by design, so riding out
            # a failure beats discarding a run that has already paid for its
            # uploads. It is recorded rather than swallowed: HTTPError subclasses
            # URLError, so a persistent 4xx would otherwise spin here silently
            # for the full deadline and then report every item as a timeout.
            key = f"{type(exc).__name__}:{getattr(exc, 'code', '')}".rstrip(":")
            problems[key] = problems.get(key, 0) + 1
            time.sleep(POLL_SECONDS)
            continue
        states.update(fresh)
        now = time.monotonic()
        for upload_id, info in fresh.items():
            if info.get("state") in WAIT_TERMINAL:
                terminal_at.setdefault(upload_id, now)
        pending = [u for u in pending
                   if fresh.get(u, {}).get("state") not in WAIT_TERMINAL]
        if pending:
            time.sleep(POLL_SECONDS)
    for u in pending:
        states.setdefault(u, {"state": "timeout"})
    return states, terminal_at


def _verify(base_url, upload_dir, landed, states, terminal_at, paced):
    """Judge every landed item against the five criteria.

    A gallery listing that fails for anything but backpressure fails criterion 4
    for every item and is recorded as the reason. That fails the stage, which is
    honest -- nobody read the gallery, so nothing is verified -- while keeping
    the hour of uploads the run has already paid for. Letting it propagate would
    have thrown the evidence away as well.
    """
    gallery: set[str] = set()
    gallery_error = ""
    try:
        gallery = runner.call_with_backpressure(
            # Pagination restarts from the first page on a retry. The listing is
            # an idempotent read and the public budget is 12000/min, so paying
            # for a repeat is cheaper than carrying a half-read gallery into the
            # verdict.
            lambda: oracle.gallery_filenames(base_url), on_backpressure=_count(paced))
    except NETWORK_ERRORS + (json.JSONDecodeError,) as exc:
        gallery_error = f"{type(exc).__name__}: {exc}"
        _discard(exc)
    verdicts, to_published = [], []
    for key, (payload, attempt, finished_at) in landed.items():
        info = states.get(key, {"state": "unknown"})
        state = info.get("state", "unknown")
        media_id = info.get("mediaId") or ""
        published_ok = oracle.classify(state)
        digest_ok = False
        if published_ok and media_id:
            digest_ok = _digest_ok(base_url, media_id, payload.sha256_hex, paced)
        verdicts.append(oracle.ItemVerdict(
            upload_id=attempt.upload_id, filename=payload.filename, media_id=media_id,
            state=state, transport_ok=attempt.transport_ok, published_ok=published_ok,
            digest_ok=digest_ok, gallery_ok=payload.filename in gallery,
            source_gone=(oracle.source_removed(upload_dir, attempt.upload_id)
                         if attempt.upload_id else False),
        ))
        if published_ok and key in terminal_at:
            to_published.append(terminal_at[key] - finished_at)
    return verdicts, to_published, gallery_error


def _digest_ok(base_url: str, media_id: str, expected_sha: str, paced: dict[str, int]) -> bool:
    """A 404 here is this item's verdict, not backpressure: the server said it
    published this media and the download does not have it. HTTPError subclasses
    URLError, so that arrives in the catch below along with every connection
    fault and, crucially, http.client.IncompleteRead -- a truncated body on a
    200 MB video, which is neither an OSError nor a URLError. Any of them fails
    this item and leaves the rest of the campaign to finish reporting."""
    try:
        ok, _ = runner.call_with_backpressure(
            lambda: oracle.verify_download(base_url, media_id, expected_sha),
            on_backpressure=_count(paced))
        return ok
    except NETWORK_ERRORS as exc:
        _discard(exc)
        return False


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
