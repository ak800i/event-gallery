import contextlib
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

from loadtest.wedding import finalize, runner, stage, tusclient
from loadtest.wedding.observe import QueueSample
from loadtest.wedding.oracle import ItemVerdict
from loadtest.wedding.runner import (DiskGuard, call_with_backpressure, decide_passed,
                                     log_evidence_ok, retry_after_seconds, run_schedule,
                                     summarize, summarize_queue)


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
                         runner.MAX_RETRY_AFTER_SECONDS)


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

    def _finalize(self, log_text):
        with tempfile.TemporaryDirectory() as tmp:
            report = summarize("s", [_v()], backpressure={}, unexpected_5xx=0,
                               levels={}, queue=[], disk=(1, 1))
            report["provisional"] = True
            report_path = Path(tmp) / "s.json"
            report_path.write_text(json.dumps(report))
            log_path = Path(tmp) / "s.log"
            log_path.write_text(log_text, encoding="utf-8")
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
        # The bytes are durable; re-sending them would draw 409 until the
        # retries ran out and report a false transport failure on an upload the
        # server has.
        script = [CREATED,
                  (500, {}),                          # PATCH: reply lost
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
        # The server acknowledged less than was sent: offset != size is its own
        # failure, independent of any status code.
        script = [CREATED, (204, {"upload-offset": "8"}), (204, {"upload-offset": "8"})]
        attempt, _server = self._upload(script, data=b"x" * 16, chunk=8)
        self.assertFalse(attempt.transport_ok)
        self.assertEqual(attempt.error, "final offset 8 != 16")


