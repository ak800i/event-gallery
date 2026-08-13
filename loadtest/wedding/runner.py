"""Stage orchestration and reporting."""
from __future__ import annotations

import json
import re
import threading
import time
import urllib.error
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Callable

from .backoff import retry_after_seconds
from .oracle import ItemVerdict

# Documented backpressure. /api/uploads/status answers 503 + Retry-After behind
# its own per-IP limiter; /api/gallery and /api/media/{id}/download share
# another and answer 429 + Retry-After. The tus proxy also answers 429, without
# a Retry-After, which is why every caller needs a backoff fallback too.
BACKPRESSURE_STATUSES = (429, 503)

# A zero-ERROR verdict is only worth anything if the log actually parsed.
LOG_PARSE_FLOOR = 0.95

# A sample gap this many times the median is reported rather than read as idle.
QUEUE_GAP_FACTOR = 3.0


@dataclass
class DiskGuard:
    """Peak need is ~118 GB against 363 GB free; this stops a runaway stage.

    Latching: once tripped, every later check refuses without probing, and
    workers are expected to consult ``tripped`` and return rather than raise.
    Not because raising would fail to stop the pool -- ThreadPoolExecutor.map
    cancels its pending futures from a generator ``finally`` while the exception
    unwinds, so it halts within roughly ``concurrency`` more items -- but
    because that exception propagates out of main() and takes the entire report
    with it, leaving no verdict at all and no record that the stage aborted.
    """
    min_free_bytes: int
    probe: Callable[[], int]
    _lock: threading.Lock = field(default_factory=threading.Lock, repr=False)
    _tripped: str = field(default="", repr=False)

    @property
    def tripped(self) -> str:
        with self._lock:
            return self._tripped

    def check(self) -> None:
        with self._lock:
            if self._tripped:
                raise RuntimeError(self._tripped)
        free = self.probe()
        if free < self.min_free_bytes:
            reason = (f"aborting: {free / 1e9:.1f} GB free is below the "
                      f"{self.min_free_bytes / 1e9:.1f} GB floor")
            with self._lock:
                self._tripped = self._tripped or reason
            raise RuntimeError(reason)


class SourceWatch:
    """Evidence that ``--upload-dir`` really is the app's tus data directory.

    Criterion 5 is satisfied by an *absence*, so a directory that never held a
    source -- the wrong bind mount, or the right path on the wrong host --
    reports every item as clean and the campaign proves nothing. Sampling while
    uploads are in flight is what turns that assumption into an observation.

    ``max_seen`` latches, so an upload that completes and is cleaned up between
    two samples cannot erase evidence an earlier sample already recorded. Only a
    phase in which no sample ever saw a source counts as unverified.
    """

    def __init__(self, probe: Callable[[], int], interval: float = 0.25):
        self._probe = probe
        self._interval = interval
        self._lock = threading.Lock()
        self._samples = 0
        self._max_seen = 0
        self._errors = 0
        self._error = ""
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    def sample(self) -> None:
        try:
            seen = self._probe()
        except OSError as exc:
            with self._lock:
                self._errors += 1
                self._error = f"{type(exc).__name__}: {exc}"
            return
        with self._lock:
            self._samples += 1
            self._max_seen = max(self._max_seen, seen)

    def snapshot(self) -> dict:
        with self._lock:
            return {"samples": self._samples, "max_seen": self._max_seen,
                    "observed": self._max_seen > 0, "errors": self._errors,
                    "error": self._error}

    def __enter__(self) -> "SourceWatch":
        self._stop.clear()
        self._thread = threading.Thread(target=self._loop, name="source-watch", daemon=True)
        self._thread.start()
        return self

    def __exit__(self, *_exc) -> None:
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=self._interval * 10 + 5.0)
            self._thread = None

    def _loop(self) -> None:
        while True:
            self.sample()
            if self._stop.wait(self._interval):
                return


@dataclass
class Ran:
    """One scheduled slot: what it produced, or why it never ran."""
    index: int
    started_at: float
    finished_at: float
    result: object = None
    skipped: str = ""
    lag: float = 0.0


def run_schedule(schedule: list[float], concurrency: int, work: Callable[[int], object],
                 *, guard: DiskGuard | None = None, clock: Callable[[], float] = time.monotonic,
                 sleep: Callable[[float], None] = time.sleep) -> list[Ran]:
    """Run ``work(i)`` for each arrival, no more than ``concurrency`` at once.

    A worker sleeps until its own arrival time and so occupies a slot while
    idle. That is only harmless because the schedule is ascending and the pool
    dequeues in submission order: everything behind a sleeping worker is due
    strictly later, so nothing that should already have started is held up. An
    unsorted schedule would break that, hence the check.

    The consequence to keep in mind when reading a report: concurrency is a
    ceiling on offered load, not a target. When uploads take longer than the
    gaps between arrivals the queue backs up, later waits compute negative and
    those items start late. Arrivals then degrade to "as fast as the pool can
    go" -- they are never reordered or bunched.
    """
    if any(later < earlier for earlier, later in zip(schedule, schedule[1:])):
        raise ValueError("schedule must be sorted ascending")
    started = clock()

    def run_one(index: int) -> Ran:
        if guard is not None and guard.tripped:
            now = clock()
            return Ran(index, now, now, skipped=guard.tripped)
        wait = schedule[index] - (clock() - started)
        if wait > 0:
            sleep(wait)
        if guard is not None:
            try:
                guard.check()
            except RuntimeError as exc:
                now = clock()
                return Ran(index, now, now, skipped=str(exc))
        began = clock()
        result = work(index)
        # How far behind its arrival this item actually started. Rising lag is
        # the signal that concurrency, not the schedule, is setting the rate.
        return Ran(index, began, clock(), result=result,
                   lag=max(0.0, began - (started + schedule[index])))

    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        return list(pool.map(run_one, range(len(schedule))))


def call_with_backpressure(fn: Callable[[], object], *, attempts: int = 8,
                           sleep: Callable[[float], None] = time.sleep,
                           on_backpressure: Callable[[int], None] | None = None):
    """Run one oracle network call, treating documented backpressure as pacing.

    A single-IP verification pass over thousands of items is exactly the traffic
    shape the per-IP limiters exist to throttle, so a 429 or 503 is the server
    pacing us and must not be recorded as a failed item. Every other status is
    the item's own verdict and propagates -- a 404 from a download above all,
    which means the media the server told us it published is not there.
    """
    last: urllib.error.HTTPError | None = None
    attempts = max(1, attempts)
    for i in range(attempts):
        try:
            return fn()
        except urllib.error.HTTPError as exc:
            if exc.code not in BACKPRESSURE_STATUSES:
                raise
            # An HTTPError wraps a live response. Dropping one without closing
            # it holds the connection until the collector notices, and a long
            # stage discards thousands of them.
            if last is not None:
                last.close()
            last = exc
            if on_backpressure is not None:
                on_backpressure(exc.code)
            if i == attempts - 1:
                break
            sleep(retry_after_seconds(getattr(exc, "headers", None), min(2.0 ** i, 30.0)))
    if last is None:  # unreachable: attempts >= 1, and a non-backpressure status raises
        raise RuntimeError("call_with_backpressure made no attempt")
    raise last


_FRACTION = re.compile(r"\.(\d{6})\d+")


def _parse_time(stamp: str) -> float | None:
    """App timestamps are RFC3339Nano with a real offset (`+02:00`), and nine
    fractional digits are more than fromisoformat accepts on every version."""
    if not stamp:
        return None
    try:
        return datetime.fromisoformat(_FRACTION.sub(r".\1", stamp)).timestamp()
    except ValueError:
        return None


def summarize_queue(samples, gap_factor: float = QUEUE_GAP_FACTOR) -> dict:
    """Digest of the app's own drain curve.

    `oldest_pending_age_seconds` is read with .get because the app emits it only
    while a pending row exists. Indexing it would raise exactly when the queue
    empties -- the end of a successful run, after the expensive part.

    Gaps are reported rather than read as idle. The summarizer is silent while
    the queue is empty, but its own failure path logs WARN, not ERROR, so a
    missing stretch can equally be a summarizer that fell over without
    disturbing the zero-ERROR criterion.
    """
    ages = [s.fields["oldest_pending_age_seconds"] for s in samples
            if s.fields.get("oldest_pending_age_seconds") is not None]
    times = [t for t in (_parse_time(s.time) for s in samples) if t is not None]
    deltas = sorted(later - earlier for earlier, later in zip(times, times[1:]))
    gaps = []
    if len(deltas) >= 3:
        median = deltas[len(deltas) // 2]
        limit = median * gap_factor
        for earlier, later in zip(times, times[1:]):
            if median > 0 and later - earlier > limit:
                gaps.append(round(later - earlier, 1))
    return {
        "samples": len(samples),
        "with_oldest_pending": len(ages),
        "max_oldest_pending_age_seconds": round(max(ages), 1) if ages else None,
        "suspect_gaps_seconds": gaps,
    }


def percentile(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    k = max(0, min(len(ordered) - 1, int(round((len(ordered) - 1) * p))))
    return round(ordered[k], 3)


def log_evidence_ok(report: dict) -> bool:
    """Whether the ERROR count in this report is worth believing.

    `count_levels` yields {} for output it cannot parse, so a log it could not
    read at all reports zero ERRORs and passes vacuously. `docker compose logs`
    does exactly that: it prefixes every line with `app-1  | `. A report with no
    `log_lines` at all has not reached finalize yet and is provisional.
    """
    logs = report.get("log_lines")
    if logs is None:
        return True
    total = logs.get("total", 0)
    return total > 0 and logs.get("parsed", 0) >= total * LOG_PARSE_FLOOR


def source_evidence_ok(report: dict) -> bool:
    """Whether criterion 5's verdict in this report was actually observed.

    `source_gone` is true when nothing is there, so an upload directory that
    never held a tus source passes every item without proving anything. A
    report with no `source_observation` at all has not reached finalize yet and
    is provisional; finalize fills in a failing one rather than trusting it.
    """
    probe = report.get("source_observation")
    if probe is None:
        return True
    return bool(probe.get("observed"))


def decide_passed(report: dict) -> bool:
    """The single definition of a passing stage. Both summarize and finalize
    use it, so the provisional and authoritative verdicts cannot diverge."""
    return (not report["oracle_failures"]
            and not report.get("aborted")
            and report["unexpected_5xx"] == 0
            and report["log_levels"].get("ERROR", 0) == 0
            and report["items"]["total"] > 0
            and report["items"]["verified"] == report["items"]["total"]
            and log_evidence_ok(report)
            and source_evidence_ok(report))


def summarize(stage: str, verdicts: list[ItemVerdict], backpressure: dict[str, int],
              unexpected_5xx: int, levels: dict[str, int], queue: list,
              disk: tuple[int, int], to_published: list[float] | None = None,
              drain_seconds: float | None = None, aborted: str = "",
              source_observation: dict | None = None) -> dict:
    to_published = to_published or []
    verified = [v for v in verdicts if v.ok]
    failures = [
        {"upload_id": v.upload_id, "filename": v.filename, "state": v.state,
         "failed": v.failed_criteria}
        for v in verdicts if not v.ok
    ]
    report = {
        "stage": stage,
        "aborted": aborted,
        "items": {
            "total": len(verdicts),
            "verified": len(verified),
            "published": sum(1 for v in verdicts if v.state == "published"),
            "duplicate": sum(1 for v in verdicts if v.state == "duplicate"),
            "failed": sum(1 for v in verdicts if v.state == "failed"),
        },
        "oracle_failures": failures[:50],
        "oracle_failure_count": len(failures),
        "backpressure": backpressure,
        "unexpected_5xx": unexpected_5xx,
        "log_levels": levels,
        "timings": {
            "to_published_p50": percentile(to_published, 0.50),
            "to_published_p95": percentile(to_published, 0.95),
            "to_published_max": round(max(to_published), 3) if to_published else 0.0,
        },
        "drain_seconds": drain_seconds,
        "queue_summary": summarize_queue(queue),
        "queue_samples": [{"time": q.time, **q.fields} for q in queue],
        "disk": {"free_before": disk[0], "free_after": disk[1]},
    }
    if source_observation is not None:
        report["source_observation"] = source_observation
    report["passed"] = decide_passed(report)
    return report


def write_report(report: dict, out_path: Path) -> None:
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(report, indent=2), encoding="utf-8")
