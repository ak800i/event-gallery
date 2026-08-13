import base64
import contextlib
import http.client
import io
import json
import socket
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
from pathlib import Path
from unittest import mock

from loadtest.wedding import backoff, finalize, runner, stage, tusclient
from loadtest.wedding.backoff import retry_after_seconds
from loadtest.wedding.observe import QueueSample
from loadtest.wedding.oracle import ItemVerdict
from loadtest.wedding.runner import (DiskGuard, SourceWatch, call_with_backpressure,
                                     decide_passed, log_evidence_ok, run_schedule,
                                     summarize, summarize_queue)

# What stage.py hands summarize once it has watched the tus upload directory
# during the upload phase and seen sources come and go.
SOURCES_SEEN = {"samples": 40, "max_seen": 7, "observed": True, "errors": 0, "error": ""}


def _v(ok=True, **over):
    base = dict(upload_id="u", filename="f.jpg", media_id="m", state="published",
                transport_ok=True, published_ok=True, digest_ok=True,
                gallery_ok=True, source_gone=True)
    if not ok:
        base["digest_ok"] = False
    base.update(over)
    return ItemVerdict(**base)


def _http_error(code, retry_after=None):
    headers = {} if retry_after is None else {"Retry-After": retry_after}
    # A real fp: HTTPError opens a tempfile when handed None, which then warns
    # on collection and buries the rest of the suite's output.
    return urllib.error.HTTPError("http://app:8080/x", code, "nope", headers, io.BytesIO(b""))


class TestDiskGuard(unittest.TestCase):
    def test_trips_below_the_floor(self):
        g = DiskGuard(min_free_bytes=100, probe=lambda: 99)
        with self.assertRaises(RuntimeError):
            g.check()

    def test_passes_above_the_floor(self):
        DiskGuard(min_free_bytes=100, probe=lambda: 101).check()

    def test_stays_tripped_without_probing_again(self):
        readings = iter([99, 10 ** 12])
        g = DiskGuard(min_free_bytes=100, probe=lambda: next(readings))
        with self.assertRaises(RuntimeError):
            g.check()
        self.assertTrue(g.tripped)
        with self.assertRaises(RuntimeError):
            g.check()


class TestSourceWatch(unittest.TestCase):
    """Criterion 5 is satisfied by an absence, so a directory that never held a
    tus source -- a wrong bind mount above all -- reports every item clean. The
    watch is what turns "the sources are gone" into something observed."""

    def test_a_directory_that_never_holds_a_source_proves_nothing(self):
        watch = SourceWatch(lambda: 0)
        for _ in range(3):
            watch.sample()
        snap = watch.snapshot()
        self.assertEqual(snap["samples"], 3)
        self.assertFalse(snap["observed"])

    def test_a_source_seen_once_stays_observed_after_it_is_cleaned_up(self):
        readings = iter([0, 4, 0])
        watch = SourceWatch(lambda: next(readings))
        for _ in range(3):
            watch.sample()
        snap = watch.snapshot()
        self.assertTrue(snap["observed"], "uploads that complete and are cleaned up "
                                          "between samples must not erase the evidence")
        self.assertEqual(snap["max_seen"], 4)

    def test_a_directory_it_cannot_read_is_recorded_and_still_proves_nothing(self):
        def probe():
            raise PermissionError(13, "Permission denied")

        watch = SourceWatch(probe)
        watch.sample()
        snap = watch.snapshot()
        self.assertEqual(snap["samples"], 0)
        self.assertEqual(snap["errors"], 1)
        self.assertFalse(snap["observed"])
        self.assertIn("PermissionError", snap["error"])

    def test_it_samples_for_the_upload_phase_and_not_past_it(self):
        calls = []

        def probe():
            calls.append(1)
            return 1

        watch = SourceWatch(probe, interval=0.01)
        with watch:
            time.sleep(0.15)
        stopped_at = len(calls)
        self.assertGreater(stopped_at, 1)
        self.assertTrue(watch.snapshot()["observed"])
        time.sleep(0.05)
        self.assertEqual(len(calls), stopped_at,
                         "the watch must not outlive the upload phase")


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

    def test_a_stage_that_uploaded_nothing_does_not_pass(self):
        report = summarize("s", [], backpressure={}, unexpected_5xx=0,
                           levels={"ERROR": 0}, queue=[], disk=(1, 1))
        self.assertFalse(report["passed"])

    def test_a_tripped_disk_guard_fails_the_stage(self):
        report = summarize("s", [_v()], backpressure={}, unexpected_5xx=0,
                           levels={"ERROR": 0}, queue=[], disk=(1, 1),
                           aborted="aborting: 1.0 GB free is below the 50.0 GB floor")
        self.assertFalse(report["passed"])

    def test_a_stage_that_never_saw_a_tus_source_cannot_pass(self):
        # Every source_gone here is true because the directory was empty all
        # along -- the wrong-bind-mount case. Nothing was proven, so nothing passes.
        report = summarize("s", [_v()], backpressure={}, unexpected_5xx=0,
                           levels={"ERROR": 0}, queue=[], disk=(1, 1),
                           source_observation={"samples": 40, "max_seen": 0,
                                               "observed": False, "errors": 0, "error": ""})
        self.assertFalse(report["passed"])

    def test_having_watched_sources_come_and_go_leaves_the_stage_passing(self):
        report = summarize("s", [_v()], backpressure={}, unexpected_5xx=0,
                           levels={"ERROR": 0}, queue=[], disk=(1, 1),
                           source_observation=SOURCES_SEEN)
        self.assertTrue(report["passed"])


class TestLogEvidence(unittest.TestCase):
    """Finding 4: count_levels answers {} for output it cannot parse, so a log
    the parser never read reports zero ERRORs and would pass vacuously."""

    def _report(self, **over):
        report = summarize("s", [_v()], backpressure={}, unexpected_5xx=0,
                           levels={"ERROR": 0}, queue=[], disk=(1, 1))
        report.update(over)
        return report

    def test_an_unparsed_log_cannot_certify_zero_errors(self):
        report = self._report(log_lines={"total": 1000, "parsed": 0})
        self.assertFalse(log_evidence_ok(report))
        self.assertFalse(decide_passed(report))

    def test_an_empty_log_cannot_certify_zero_errors(self):
        report = self._report(log_lines={"total": 0, "parsed": 0})
        self.assertFalse(decide_passed(report))

    def test_a_fully_parsed_log_certifies_zero_errors(self):
        report = self._report(log_lines={"total": 1000, "parsed": 1000})
        self.assertTrue(decide_passed(report))

    def test_a_report_without_collected_logs_is_provisional_not_broken(self):
        self.assertTrue(decide_passed(self._report()))


class TestRetryAfter(unittest.TestCase):
    def test_honours_the_header(self):
        self.assertEqual(retry_after_seconds({"Retry-After": "5"}, 99.0), 5.0)

    def test_falls_back_when_absent_or_unparseable(self):
        self.assertEqual(retry_after_seconds({}, 3.0), 3.0)
        self.assertEqual(retry_after_seconds(None, 3.0), 3.0)
        self.assertEqual(
            retry_after_seconds({"Retry-After": "Wed, 21 Oct 2026 07:28:00 GMT"}, 3.0), 3.0)
        self.assertEqual(retry_after_seconds({"Retry-After": "-1"}, 3.0), 3.0)

    def test_clamps_an_absurd_wait(self):
        self.assertEqual(retry_after_seconds({"Retry-After": "86400"}, 3.0),
                         backoff.MAX_RETRY_AFTER_SECONDS)


class TestBackpressure(unittest.TestCase):
    """Finding 1: all three oracle helpers let HTTPError propagate, and the
    endpoints they call answer 429/503 under exactly the traffic a single-IP
    verification pass produces."""

    def test_retries_a_503_and_honours_retry_after(self):
        calls, slept, seen = [], [], []

        def fn():
            calls.append(1)
            if len(calls) < 3:
                raise _http_error(503, "5")
            return "ok"

        self.assertEqual(
            call_with_backpressure(fn, sleep=slept.append, on_backpressure=seen.append), "ok")
        self.assertEqual(slept, [5.0, 5.0])
        self.assertEqual(seen, [503, 503])

    def test_retries_a_429_with_no_retry_after_by_backing_off(self):
        calls, slept = [], []

        def fn():
            calls.append(1)
            if len(calls) < 3:
                raise _http_error(429)
            return "ok"

        self.assertEqual(call_with_backpressure(fn, sleep=slept.append), "ok")
        self.assertEqual(slept, [1.0, 2.0])

    def test_a_404_is_the_items_verdict_and_propagates_at_once(self):
        calls = []

        def fn():
            calls.append(1)
            raise _http_error(404)

        with self.assertRaises(urllib.error.HTTPError) as caught:
            call_with_backpressure(fn, sleep=lambda _s: None)
        caught.exception.close()
        self.assertEqual(caught.exception.code, 404)
        self.assertEqual(len(calls), 1, "a 404 must not be retried as backpressure")

    def test_gives_up_eventually_rather_than_looping_forever(self):
        calls = []

        def fn():
            calls.append(1)
            raise _http_error(503, "1")

        with self.assertRaises(urllib.error.HTTPError) as caught:
            call_with_backpressure(fn, attempts=3, sleep=lambda _s: None)
        caught.exception.close()
        self.assertEqual(len(calls), 3)


class TestQueueSummary(unittest.TestCase):
    """Findings 3 and 5."""

    @staticmethod
    def _sample(when, **fields):
        return QueueSample(time=when, fields=fields)

    def test_survives_samples_without_an_oldest_pending_age(self):
        # The app emits oldest_pending_age_seconds only while a pending row
        # exists, so it is absent exactly when the queue drains -- the end of a
        # successful run. Indexing it would raise there, after the expensive part.
        samples = [self._sample("2026-08-13T20:00:00.000000000+02:00", pending=0.0),
                   self._sample("2026-08-13T20:00:30.000000000+02:00", pending=0.0)]
        summary = summarize_queue(samples)
        self.assertEqual(summary["samples"], 2)
        self.assertIsNone(summary["max_oldest_pending_age_seconds"])
        self.assertEqual(summary["with_oldest_pending"], 0)

    def test_reports_the_peak_pending_age_when_present(self):
        samples = [self._sample("2026-08-13T20:00:00.000000000+02:00",
                                oldest_pending_age_seconds=12.0),
                   self._sample("2026-08-13T20:00:30.000000000+02:00",
                                oldest_pending_age_seconds=41.5)]
        self.assertEqual(summarize_queue(samples)["max_oldest_pending_age_seconds"], 41.5)

    def test_flags_a_hole_in_the_series(self):
        # The summarizer's own failure path logs WARN, not ERROR, so a gap can
        # be a summarizer that fell over while zero-ERROR stays green.
        times = ["2026-08-13T20:00:00.000000000+02:00",
                 "2026-08-13T20:00:30.000000000+02:00",
                 "2026-08-13T20:01:00.000000000+02:00",
                 "2026-08-13T20:01:30.000000000+02:00",
                 "2026-08-13T20:10:00.000000000+02:00"]
        summary = summarize_queue([self._sample(t, pending=1.0) for t in times])
        self.assertEqual(summary["suspect_gaps_seconds"], [510.0])

    def test_an_even_series_has_no_gaps(self):
        times = [f"2026-08-13T20:0{i}:00.000000000+02:00" for i in range(6)]
        summary = summarize_queue([self._sample(t, pending=1.0) for t in times])
        self.assertEqual(summary["suspect_gaps_seconds"], [])


class TestRunSchedule(unittest.TestCase):
    def test_no_item_starts_before_its_arrival(self):
        schedule = [0.0, 0.0, 0.30, 0.30]
        t0 = time.monotonic()
        for r in run_schedule(schedule, 2, lambda i: i):
            self.assertGreaterEqual(r.started_at - t0, schedule[r.index] - 0.05,
                                    f"item {r.index} started early")

    def test_never_exceeds_the_concurrency_ceiling(self):
        live = peak = 0
        lock = threading.Lock()

        def work(_i):
            nonlocal live, peak
            with lock:
                live += 1
                peak = max(peak, live)
            time.sleep(0.05)
            with lock:
                live -= 1

        run_schedule([0.0] * 8, 2, work)
        self.assertLessEqual(peak, 2)

    def test_refuses_an_unsorted_schedule(self):
        # A worker sleeping until its arrival occupies a slot, which is only
        # harmless while everything queued behind it is due later.
        with self.assertRaises(ValueError):
            run_schedule([5.0, 0.0], 2, lambda i: i)

    def test_a_schedule_the_pool_can_keep_up_with_reports_no_lag(self):
        ran = run_schedule([0.0, 0.1, 0.2], 2, lambda i: None)
        self.assertLess(max(r.lag for r in ran), 0.05)

    def test_lag_grows_when_concurrency_and_not_the_schedule_sets_the_pace(self):
        # Everything is due at once but only one worker exists, so arrivals
        # degrade to pool speed. They are delayed, never reordered.
        ran = run_schedule([0.0] * 3, 1, lambda _i: time.sleep(0.1))
        self.assertLess(ran[0].lag, 0.05)
        self.assertGreater(ran[2].lag, 0.15)
        self.assertEqual([r.index for r in ran], [0, 1, 2])

    def test_a_tripped_guard_stops_the_rest_of_the_campaign(self):
        done = []
        readings = iter([10 ** 12, 10 ** 12, 0])
        guard = DiskGuard(min_free_bytes=1, probe=lambda: next(readings, 0))
        ran = run_schedule([0.0] * 6, 1, done.append, guard=guard)
        self.assertEqual(done, [0, 1], "uploads must stop once the floor is crossed")
        self.assertTrue(guard.tripped)
        self.assertTrue(all(not r.skipped for r in ran[:2]))
        self.assertTrue(all(r.skipped for r in ran[2:]))


class TestVerdictAgreement(unittest.TestCase):
    def test_finalize_and_the_sidecar_share_one_definition(self):
        # decide_passed is the only place a stage is judged: summarize calls it
        # for the provisional verdict and finalize calls it for the real one.
        report = summarize("s", [_v()], backpressure={}, unexpected_5xx=0,
                           levels={"ERROR": 0}, queue=[], disk=(1, 1))
        self.assertEqual(report["passed"], decide_passed(report))
        report["log_levels"] = {"ERROR": 2}
        self.assertFalse(decide_passed(report))


class TestWaitTerminal(unittest.TestCase):
    """Finding 2."""

    def test_unknown_is_terminal_for_the_wait_loop(self):
        # `unknown` means the server holds no job row: permanently terminal, but
        # deliberately not in TERMINAL_STATES, so a loop keyed on that alone
        # spins on a lost id until its deadline.
        self.assertIn("unknown", stage.WAIT_TERMINAL)
        self.assertNotIn("unknown", stage.oracle.TERMINAL_STATES)

    def test_recovering_is_not_terminal(self):
        # It resolves on its own once the startup inventory finishes.
        self.assertNotIn("recovering", stage.WAIT_TERMINAL)

    def test_every_real_terminal_state_still_ends_the_wait(self):
        self.assertTrue(stage.oracle.TERMINAL_STATES <= stage.WAIT_TERMINAL)


APP_LINE = ('{{"time":"2026-08-13T20:0{i}:00.000000000+02:00","level":"{level}",'
            '"msg":"ingest queue","operation":"summary","pending":{i}}}')


class TestFinalize(unittest.TestCase):
    """Finding 4, end to end: the authoritative verdict must not certify a log
    it could not read."""

    def _finalize(self, log_text, source_observation=SOURCES_SEEN, encoding="utf-8"):
        with tempfile.TemporaryDirectory() as tmp:
            report = summarize("s", [_v()], backpressure={}, unexpected_5xx=0,
                               levels={}, queue=[], disk=(1, 1),
                               source_observation=source_observation)
            report["provisional"] = True
            report_path = Path(tmp) / "s.json"
            report_path.write_text(json.dumps(report))
            log_path = Path(tmp) / "s.log"
            log_path.write_text(log_text, encoding=encoding)
            argv = ["finalize", "--report", str(report_path), "--logs", str(log_path)]
            with mock.patch.object(sys, "argv", argv), \
                    contextlib.redirect_stdout(io.StringIO()):
                code = finalize.main()
            return code, json.loads(report_path.read_text())

    def test_a_clean_log_certifies_the_stage(self):
        text = "\n".join(APP_LINE.format(i=i, level="INFO") for i in range(5))
        code, report = self._finalize(text)
        self.assertEqual(code, 0)
        self.assertTrue(report["passed"])
        self.assertFalse(report["provisional"])
        self.assertEqual(report["log_lines"], {"total": 5, "parsed": 5})

    def test_a_bom_does_not_corrupt_the_first_line(self):
        # Windows PowerShell 5.1's `Set-Content -Encoding utf8` writes one. On
        # smoke's handful of lines, losing the first is 80% parsed and fails the
        # 95% floor spuriously.
        text = "\n".join(APP_LINE.format(i=i, level="INFO") for i in range(5))
        code, report = self._finalize(text, encoding="utf-8-sig")
        self.assertEqual(report["log_lines"], {"total": 5, "parsed": 5})
        self.assertEqual(code, 0)

    def test_a_report_with_no_source_observation_cannot_be_certified(self):
        # The authoritative verdict refuses to certify criterion 5 for a stage
        # that never watched the upload directory at all.
        text = "\n".join(APP_LINE.format(i=i, level="INFO") for i in range(5))
        code, report = self._finalize(text, source_observation=None)
        self.assertFalse(report["passed"])
        self.assertEqual(code, 1)
        self.assertFalse(report["source_observation"]["observed"])

    def test_an_error_in_the_log_fails_the_stage(self):
        lines = [APP_LINE.format(i=i, level="INFO") for i in range(4)]
        lines.append(APP_LINE.format(i=5, level="ERROR"))
        code, report = self._finalize("\n".join(lines))
        self.assertEqual(code, 1)
        self.assertFalse(report["passed"])

    def test_compose_prefixed_output_cannot_pass_vacuously(self):
        # `docker compose logs` prefixes every line, so nothing parses as JSON,
        # count_levels answers {} and a log full of errors reports zero of them.
        lines = [APP_LINE.format(i=i, level="INFO") for i in range(4)]
        lines.append(APP_LINE.format(i=5, level="ERROR"))
        code, report = self._finalize("\n".join(f"app-1  | {line}" for line in lines))
        self.assertEqual(report["log_levels"].get("ERROR", 0), 0,
                         "the parser must stay strict: compose multiplexes tusd and "
                         "cloudflared, whose errors are not the app's")
        self.assertEqual(report["log_lines"], {"total": 5, "parsed": 0})
        self.assertFalse(report["passed"])
        self.assertEqual(code, 1)

    def test_an_empty_log_cannot_pass(self):
        code, report = self._finalize("")
        self.assertEqual(code, 1)
        self.assertFalse(report["passed"])

    def test_queue_samples_are_merged_from_the_log(self):
        text = "\n".join(APP_LINE.format(i=i, level="INFO") for i in range(5))
        _code, report = self._finalize(text)
        self.assertEqual(len(report["queue_samples"]), 5)
        self.assertEqual(report["queue_summary"]["samples"], 5)


class _FakePayload:
    filename = "event-gallery-battle-x.jpg"
    mime = "image/jpeg"
    sha256_hex = "a" * 64

    def __init__(self, data):
        self._data = data

    @property
    def size(self):
        return len(self._data)

    def chunks(self, n):
        for i in range(0, len(self._data), n):
            yield self._data[i:i + n]


class _FakeServer:
    """Scripted replacement for tusclient._request."""

    def __init__(self, script):
        self.script = list(script)
        self.calls = []

    def __call__(self, method, url, headers, body=None, timeout=600.0):
        self.calls.append((method, headers.get("Upload-Offset"), len(body or b"")))
        status, hdrs = self.script.pop(0)
        note = "connection reset by peer" if status == tusclient.TRANSPORT_ERROR else ""
        return status, hdrs, b"", note


CREATED = (201, {"location": "/api/tus/abc123"})


class TestTusClient(unittest.TestCase):
    def _upload(self, script, data=b"x" * 20, chunk=8):
        server = _FakeServer(script)
        with mock.patch.object(tusclient, "_request", server), \
                mock.patch.object(tusclient.time, "sleep"):
            attempt = tusclient.upload("http://app:8080", _FakePayload(data), chunk)
        return attempt, server

    def test_blocks_rechunk_exactly_with_the_tail_included(self):
        blocks = list(tusclient._blocks(_FakePayload(b"x" * 20), 8))
        self.assertEqual([len(b) for b in blocks], [8, 8, 4])
        self.assertEqual(b"".join(blocks), b"x" * 20)

    def test_a_clean_upload_reports_transport_ok(self):
        script = [CREATED] + [(204, {"upload-offset": str(o)}) for o in (8, 16, 20)]
        attempt, _server = self._upload(script)
        self.assertEqual(attempt.upload_id, "abc123")
        self.assertEqual(attempt.error, "")
        self.assertTrue(attempt.transport_ok)

    def test_a_refused_create_is_retried_rather_than_failing_the_item(self):
        # The tus proxy 429s new uploads at exactly the concurrency the overload
        # and herd stages exist to exceed.
        script = [(429, {}), (429, {}), CREATED] + \
                 [(204, {"upload-offset": str(o)}) for o in (8, 16, 20)]
        attempt, _server = self._upload(script)
        self.assertTrue(attempt.transport_ok)
        self.assertEqual(attempt.retries, 2)
        self.assertEqual(attempt.statuses[:3], [429, 429, 201])

    def test_a_permanently_refused_create_fails_the_item_without_raising(self):
        attempt, _server = self._upload([(403, {})])
        self.assertFalse(attempt.transport_ok)
        self.assertEqual(attempt.error, "create returned 403")

    def test_a_lost_204_is_reconciled_by_head_instead_of_looping_on_409(self):
        # The bytes are durable; re-sending them draws the 409 tus mandates on
        # an Upload-Offset mismatch, until the retries run out and report a
        # false transport failure on an upload the server has.
        script = [CREATED,
                  (409, {}),                          # PATCH: reply lost, bytes landed
                  (200, {"upload-offset": "8"}),      # HEAD: it landed anyway
                  (204, {"upload-offset": "16"}),
                  (204, {"upload-offset": "20"})]
        attempt, server = self._upload(script)
        self.assertEqual(attempt.error, "")
        self.assertTrue(attempt.transport_ok)
        self.assertIn("HEAD", [c[0] for c in server.calls])
        # The reconciled block is not sent a second time.
        self.assertEqual([c[1] for c in server.calls if c[0] == "PATCH"], ["0", "8", "16"])

    def test_a_transport_error_fails_one_item_and_never_escapes(self):
        # A reset must not unwind the worker and take the rest of the pool with it.
        attempt, _server = self._upload([(tusclient.TRANSPORT_ERROR, {})] * 5)
        self.assertFalse(attempt.transport_ok)
        self.assertIn("create retries exhausted", attempt.error)
        self.assertIn("connection reset", attempt.error)

    def test_a_refused_connection_becomes_a_status_rather_than_an_exception(self):
        # The real _request, not the fake: this is the clause that keeps one
        # dead connection from unwinding pool.map and ending the campaign.
        probe = socket.socket()
        probe.bind(("127.0.0.1", 0))
        closed_port = probe.getsockname()[1]
        probe.close()
        status, _headers, _body, note = tusclient._request(
            "HEAD", f"http://127.0.0.1:{closed_port}/api/tus/x", {}, timeout=5.0)
        self.assertEqual(status, tusclient.TRANSPORT_ERROR)
        self.assertTrue(note, "the failure reason must survive for diagnosis")

    def test_a_short_upload_is_reported_even_when_every_call_succeeded(self):
        # The server acknowledged less than was sent. Both branches of _patch
        # check the offset they are given, so this is caught where it happens
        # rather than inferred from the total at the end.
        script = [CREATED, (204, {"upload-offset": "8"}), (204, {"upload-offset": "8"})]
        attempt, _server = self._upload(script, data=b"x" * 16, chunk=8)
        self.assertFalse(attempt.transport_ok)
        self.assertEqual(attempt.error, "offset diverged: server 8, client 16")

    def test_a_204_reporting_an_offset_ahead_of_the_block_fails_the_item(self):
        # The HEAD branch guarded divergence and this one trusted the server
        # blindly. An offset ahead of what we sent desynchronizes _blocks'
        # sequential cursor and writes the following block at the wrong place.
        script = [CREATED, (204, {"upload-offset": "12"})]
        attempt, _server = self._upload(script, data=b"x" * 20, chunk=8)
        self.assertFalse(attempt.transport_ok)
        self.assertEqual(attempt.error, "offset diverged: server 12, client 8")

    def test_an_unreadable_asset_fails_one_item_rather_than_the_pool(self):
        # payload.chunks() opens the base asset and is consumed outside every
        # try in _request, so an IO error there used to escape the worker,
        # pool.map and main().
        class _Unreadable(_FakePayload):
            def chunks(self, _n):
                raise OSError(5, "Input/output error")
                yield b""  # pragma: no cover - makes this a generator

        server = _FakeServer([CREATED])
        with mock.patch.object(tusclient, "_request", server), \
                mock.patch.object(tusclient.time, "sleep"):
            attempt = tusclient.upload("http://app:8080", _Unreadable(b"x" * 20), 8)
        self.assertFalse(attempt.transport_ok)
        self.assertIn("Input/output error", attempt.error)

    def test_metadata_carries_the_filename_criterion_4_matches_on(self):
        payload = _FakePayload(b"x")
        pairs = dict(part.split(" ", 1) for part in tusclient._metadata(payload).split(","))
        self.assertEqual(base64.b64decode(pairs["filename"]).decode(), payload.filename)
        self.assertEqual(base64.b64decode(pairs["filetype"]).decode(), payload.mime)
        self.assertEqual(base64.b64decode(pairs["sha256"]).decode(), payload.sha256_hex)


class TestVerify(unittest.TestCase):
    """stage._verify is where all five criteria are assigned, so each of them is
    exercised through it rather than by hand-building an ItemVerdict."""

    def _verify(self, *, state="published", media_id="m1", attempt=None,
                gallery=(_FakePayload.filename,), download=None, sources=(),
                terminal_at=130.0):
        payload = _FakePayload(b"x" * 4)
        attempt = attempt if attempt is not None else tusclient.Attempt(upload_id="abc123")
        landed = {"abc123": (payload, attempt, 100.0)}
        states = {"abc123": {"state": state, "mediaId": media_id}}

        def listing(*_a, **_kw):
            if isinstance(gallery, BaseException):
                raise gallery
            return set(gallery)

        with tempfile.TemporaryDirectory() as tmp:
            for name in sources:
                (Path(tmp) / name).write_bytes(b"")
            with mock.patch.object(stage.oracle, "gallery_filenames", listing), \
                    mock.patch.object(stage.oracle, "verify_download",
                                      download or (lambda *a, **kw: (True, ""))):
                return stage._verify("http://app:8080", Path(tmp), landed, states,
                                     {"abc123": terminal_at}, {})

    def test_a_clean_item_meets_all_five_criteria(self):
        verdicts, to_published, gallery_error = self._verify()
        self.assertEqual(verdicts[0].failed_criteria, [])
        self.assertTrue(verdicts[0].ok)
        self.assertEqual(gallery_error, "")
        # Upload finish to first observed terminal, not the verification pass.
        self.assertEqual(to_published, [30.0])

    def test_a_transport_failure_alone_fails_the_item(self):
        attempt = tusclient.Attempt(upload_id="abc123")
        attempt.error = "PATCH retries exhausted"
        verdicts, _t, _e = self._verify(attempt=attempt)
        self.assertEqual(verdicts[0].failed_criteria, ["transport_ok"])

    def test_a_non_success_state_fails_the_item(self):
        verdicts, to_published, _e = self._verify(state="failed")
        self.assertIn("published_ok", verdicts[0].failed_criteria)
        self.assertEqual(to_published, [], "an unpublished item has no publication latency")

    def test_a_digest_mismatch_alone_fails_the_item(self):
        verdicts, _t, _e = self._verify(
            download=lambda *a, **kw: (False, "digest mismatch: sent aaa, stored bbb"))
        self.assertEqual(verdicts[0].failed_criteria, ["digest_ok"])

    def test_an_item_absent_from_the_gallery_alone_fails_the_item(self):
        verdicts, _t, _e = self._verify(gallery=())
        self.assertEqual(verdicts[0].failed_criteria, ["gallery_ok"])

    def test_a_surviving_tus_source_alone_fails_the_item(self):
        verdicts, _t, _e = self._verify(sources=("abc123",))
        self.assertEqual(verdicts[0].failed_criteria, ["source_gone"])

    def test_a_surviving_info_sidecar_alone_fails_the_item(self):
        # tusd's filestore resolves an upload through its sidecar, so an orphan
        # sidecar is a leaked source even with the data file gone.
        verdicts, _t, _e = self._verify(sources=("abc123.info",))
        self.assertEqual(verdicts[0].failed_criteria, ["source_gone"])

    def test_an_item_that_never_got_an_id_cannot_claim_its_source_is_gone(self):
        verdicts, _t, _e = self._verify(attempt=tusclient.skipped("aborting: disk floor"))
        self.assertIn("transport_ok", verdicts[0].failed_criteria)
        self.assertIn("source_gone", verdicts[0].failed_criteria)

    def test_an_unreadable_gallery_fails_every_item_rather_than_the_run(self):
        # Losing the listing is not a reason to lose an hour of uploads: every
        # gallery_ok goes false, the reason is recorded, and the stage fails.
        verdicts, _t, gallery_error = self._verify(
            gallery=urllib.error.URLError("connection reset"))
        self.assertFalse(verdicts[0].gallery_ok)
        self.assertIn("URLError", gallery_error)
        report = summarize("s", verdicts, backpressure={}, unexpected_5xx=0,
                           levels={"ERROR": 0}, queue=[], disk=(1, 1),
                           source_observation=SOURCES_SEEN)
        self.assertFalse(report["passed"])

    def test_a_truncated_gallery_listing_is_recorded_rather_than_raised(self):
        verdicts, _t, gallery_error = self._verify(
            gallery=http.client.IncompleteRead(b"partial", 4096))
        self.assertFalse(verdicts[0].gallery_ok)
        self.assertIn("IncompleteRead", gallery_error)

    def test_a_gallery_page_that_is_not_json_is_recorded_rather_than_raised(self):
        verdicts, _t, gallery_error = self._verify(
            gallery=json.JSONDecodeError("Expecting value", "<html>", 0))
        self.assertIn("JSONDecodeError", gallery_error)
        self.assertFalse(verdicts[0].gallery_ok)


class TestDigestOk(unittest.TestCase):
    """The re-download is the only criterion that reads the stored bytes, and it
    is the one most likely to meet a network fault: 134 GB of it."""

    def _digest(self, download):
        with mock.patch.object(stage.oracle, "verify_download", download):
            return stage._digest_ok("http://app:8080", "m1", "a" * 64, {})

    def test_a_matching_digest_verifies_the_item(self):
        self.assertTrue(self._digest(lambda *a, **kw: (True, "")))

    def test_a_truncated_download_fails_the_item_rather_than_the_run(self):
        # http.client.IncompleteRead derives from HTTPException, which is
        # neither OSError nor URLError, so it used to escape _digest_ok,
        # _verify and main() -- destroying the report after the expensive part.
        def boom(*_a, **_kw):
            raise http.client.IncompleteRead(b"partial", 200_000_000)

        self.assertFalse(self._digest(boom))

    def test_a_404_is_this_items_verdict_and_not_the_runs(self):
        def boom(*_a, **_kw):
            raise _http_error(404)

        self.assertFalse(self._digest(boom))

    def test_a_reset_connection_fails_the_item_rather_than_the_run(self):
        def boom(*_a, **_kw):
            raise ConnectionResetError(104, "Connection reset by peer")

        self.assertFalse(self._digest(boom))


