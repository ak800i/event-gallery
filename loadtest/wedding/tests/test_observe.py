"""Tests for the observability collector."""

import tempfile
import unittest
from pathlib import Path

from loadtest.wedding.observe import (
    count_levels,
    count_tus_sources,
    disk_free_bytes,
    parse_queue_samples,
    thumbnail_exists,
)

LINES = [
    '{"time":"2026-08-13T14:12:15Z","level":"INFO","msg":"server listening","addr":":8080"}',
    '{"time":"2026-08-13T14:12:30Z","level":"INFO","msg":"ingest queue","pending":42,"processing":16,"oldest_pending_age_seconds":9,"max_processing_failures":0}',
    'not json at all',
    '{"time":"2026-08-13T14:12:45Z","level":"WARN","msg":"ingest attempt failed, retrying"}',
    '{"time":"2026-08-13T14:13:00Z","level":"ERROR","msg":"something real"}',
    '{"time":"2026-08-13T14:13:15Z","level":"INFO","msg":"ingest queue","pending":0,"processing":0,"oldest_pending_age_seconds":0,"max_processing_failures":0}',
]

# The exact attribute set emitted by Manager.logQueueSummary, with the
# offset-bearing timestamp the container really writes.
REAL_QUEUE_LINE = (
    '{"time":"2026-08-13T23:42:28.349067197+02:00","level":"INFO","msg":"ingest queue",'
    '"operation":"queue_summary","uploading":7,"pending":42,"processing":16,'
    '"cleanup":1,"discarding":0,"max_processing_failures":2,'
    '"oldest_pending_age_seconds":9}'
)


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
        self.assertEqual(counts["INFO"], 3)
        # The unparseable line contributes no level at all.
        self.assertEqual(set(counts), {"INFO", "WARN", "ERROR"})

    def test_counting_levels_survives_a_non_json_line(self):
        # docker logs interleaves stderr and can hand us a truncated line. A
        # parser that raises here destroys the report after the expensive part
        # of the run has already happened.
        noise = ["", "   ", "panic: runtime error", "{", '{"level":', "]not json[",
                 '{"time":"t","msg":"a record carrying no level at all"}']
        self.assertEqual(count_levels(noise), {})
        self.assertEqual(count_levels(noise + LINES), count_levels(LINES))

    def test_real_queue_line_yields_every_status_count(self):
        [sample] = parse_queue_samples([REAL_QUEUE_LINE])
        self.assertEqual(sample.time, "2026-08-13T23:42:28.349067197+02:00")
        self.assertEqual(
            sample.fields,
            {
                "uploading": 7,
                "pending": 42,
                "processing": 16,
                "cleanup": 1,
                "discarding": 0,
                "max_processing_failures": 2,
                "oldest_pending_age_seconds": 9,
            },
        )

    def test_non_numeric_fields_are_not_sampled(self):
        # isinstance(True, int) is true in Python, so a boolean field would
        # otherwise arrive as a 1/0 data point on the drain curve.
        line = ('{"time":"t","level":"INFO","msg":"ingest queue",'
                '"pending":3,"stalled":true,"idle":false,"operation":"queue_summary"}')
        [sample] = parse_queue_samples([line])
        self.assertEqual(sample.fields, {"pending": 3})

    def test_disk_free_is_positive(self):
        self.assertGreater(disk_free_bytes(Path(tempfile.gettempdir())), 0)

    def test_thumbnail_detection(self):
        # Real layout, confirmed against the running container and
        # media.Processor.ThumbnailPath: <media>/thumbnails/<id>.jpg
        tmp = Path(tempfile.mkdtemp())
        (tmp / "thumbnails").mkdir()
        (tmp / "thumbnails" / "m1.jpg").write_bytes(b"x")
        self.assertTrue(thumbnail_exists(tmp, "m1"))
        self.assertFalse(thumbnail_exists(tmp, "m2"))

    def test_thumbnail_is_absent_when_only_the_original_was_written(self):
        # The failure this guards against is looking in the wrong directory,
        # which reports every published item as thumbnail-less.
        tmp = Path(tempfile.mkdtemp())
        (tmp / "originals").mkdir()
        (tmp / "originals" / "m1.jpg").write_bytes(b"x")
        self.assertFalse(thumbnail_exists(tmp, "m1"))  # no thumbnails dir yet
        (tmp / "thumbnails").mkdir()
        self.assertFalse(thumbnail_exists(tmp, "m1"))

    def test_tus_sources_are_counted_by_their_sidecars(self):
        # tusd's filestore writes <id> and <id>.info together at create, and the
        # sidecar is what makes the directory recognisably a tus data dir rather
        # than any other non-empty path someone bind-mounted by mistake.
        tmp = Path(tempfile.mkdtemp())
        self.assertEqual(count_tus_sources(tmp), 0)
        (tmp / "abc123").write_bytes(b"x")
        self.assertEqual(count_tus_sources(tmp), 0, "a bare file is not evidence")
        (tmp / "abc123.info").write_bytes(b"{}")
        (tmp / "def456.info").write_bytes(b"{}")
        self.assertEqual(count_tus_sources(tmp), 2)

    def test_a_missing_upload_directory_counts_zero_rather_than_raising(self):
        # The count runs on a sampling thread during the upload phase; a probe
        # that raised would end the phase instead of recording the absence.
        self.assertEqual(count_tus_sources(Path(tempfile.mkdtemp()) / "nope"), 0)


if __name__ == "__main__":
    unittest.main()
