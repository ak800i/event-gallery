import hashlib
import http.server
import json
import tempfile
import threading
import unittest
from pathlib import Path

from loadtest.wedding.oracle import (
    ItemVerdict,
    TERMINAL_STATES,
    classify,
    gallery_filenames,
    poll_status,
    source_removed,
    verify_download,
)


def _passing(**over):
    base = dict(upload_id="u1", filename="f.jpg", media_id="m1", state="published",
                transport_ok=True, published_ok=True, digest_ok=True,
                gallery_ok=True, source_gone=True)
    base.update(over)
    return ItemVerdict(**base)


class TestVerdict(unittest.TestCase):
    def test_all_five_criteria_must_hold(self):
        self.assertTrue(_passing().ok)

    def test_any_single_criterion_failing_fails_the_item(self):
        for field in ("transport_ok", "published_ok", "digest_ok", "gallery_ok", "source_gone"):
            with self.subTest(field=field):
                self.assertFalse(_passing(**{field: False}).ok,
                                 f"{field} was allowed to fail without failing the item")

    def test_transport_success_alone_never_passes(self):
        v = _passing(published_ok=False, digest_ok=False, gallery_ok=False, source_gone=False)
        self.assertFalse(v.ok)

    def test_terminal_states(self):
        self.assertEqual(TERMINAL_STATES, {"published", "duplicate", "failed", "cancelled"})
        self.assertTrue(classify("published"))
        self.assertTrue(classify("duplicate"))
        self.assertFalse(classify("failed"))
        self.assertFalse(classify("processing"))


BODY = b"the exact bytes we uploaded"
GOOD_SHA = hashlib.sha256(BODY).hexdigest()


class _Handler(http.server.BaseHTTPRequestHandler):
    """Stands in for the deployed API. Records what it was actually asked."""

    lock = threading.Lock()
    status_batches: list[int] = []
    gallery_hits: list[str] = []

    def log_message(self, *a):  # keep test output pristine
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        ids = json.loads(self.rfile.read(n))["uploadIds"]
        with self.lock:
            self.status_batches.append(len(ids))
        results = {i: {"state": "published", "mediaId": f"media-{i}"} for i in ids}
        self._json({"results": results})

    def do_GET(self):
        if "/api/media/" in self.path:
            payload = BODY if "good" in self.path else b"CORRUPTED"
            self.send_response(200)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
        elif "/api/gallery" in self.path:
            with self.lock:
                self.gallery_hits.append(self.path)
            self._gallery()
        else:
            self.send_error(404)

    def _gallery(self):
        cursor = ""
        if "?" in self.path:
            for pair in self.path.split("?", 1)[1].split("&"):
                if pair.startswith("cursor="):
                    cursor = pair[len("cursor="):]
        if self.path.startswith("/repeat/"):
            # Never advances: only a client-side guard can end this.
            self._json({"items": [{"originalFilename": "loop.jpg"}], "nextCursor": "same"})
        elif self.path.startswith("/malformed/"):
            self._json({"items": [{"originalFilename": "odd.jpg"}], "nextCursor": {"k": "not-a-string"}})
        elif cursor == "page2":
            self._json({"items": [{"originalFilename": "second.jpg"}], "nextCursor": None})
        else:
            self._json({"items": [{"originalFilename": "present.jpg"}], "nextCursor": "page2"})

    def _json(self, obj):
        raw = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


class TestHTTPChecks(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # Threading: a client that fails to terminate must not wedge the server
        # for every other test in this class.
        cls.srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.base = f"http://127.0.0.1:{cls.srv.server_port}"

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def setUp(self):
        with _Handler.lock:
            _Handler.status_batches.clear()
            _Handler.gallery_hits.clear()

    def test_poll_status_batches_and_returns_states(self):
        got = poll_status(self.base, [f"u{i}" for i in range(150)], batch_size=100)
        self.assertEqual(len(got), 150)
        self.assertEqual(got["u7"]["state"], "published")

    def test_poll_status_really_sent_more_than_one_request(self):
        poll_status(self.base, [f"u{i}" for i in range(150)], batch_size=100)
        with _Handler.lock:
            batches = list(_Handler.status_batches)
        self.assertEqual(batches, [100, 50],
                         f"expected two capped batches, the server saw {batches}")

    def test_poll_status_refuses_a_batch_above_the_server_cap(self):
        with self.assertRaises(ValueError):
            poll_status(self.base, ["u1"], batch_size=101)

    def test_verify_download_accepts_matching_bytes(self):
        ok, _ = verify_download(self.base, "good-1", GOOD_SHA)
        self.assertTrue(ok)

    def test_verify_download_rejects_corrupted_bytes(self):
        ok, detail = verify_download(self.base, "bad-1", GOOD_SHA)
        self.assertFalse(ok, "a corrupted download was accepted")
        self.assertIn("digest", detail)

    def test_gallery_filenames(self):
        self.assertIn("present.jpg", gallery_filenames(self.base))

    def test_gallery_filenames_follows_the_cursor(self):
        self.assertEqual(gallery_filenames(self.base), {"present.jpg", "second.jpg"})
        with _Handler.lock:
            self.assertEqual(len(_Handler.gallery_hits), 2, _Handler.gallery_hits)

    def test_gallery_filenames_terminates_on_a_repeated_cursor(self):
        self._assert_terminates(f"{self.base}/repeat")

    def test_gallery_filenames_terminates_on_a_malformed_cursor(self):
        self._assert_terminates(f"{self.base}/malformed")

    def _assert_terminates(self, base):
        box = {}
        t = threading.Thread(target=lambda: box.setdefault("names", gallery_filenames(base)),
                             daemon=True)
        t.start()
        t.join(timeout=15)
        self.assertFalse(t.is_alive(), "gallery_filenames never stopped paginating")
        self.assertTrue(box["names"])

    def test_source_removed_is_false_while_the_file_remains(self):
        tmp = Path(tempfile.mkdtemp())
        (tmp / "u1").write_bytes(b"x")
        self.assertFalse(source_removed(tmp, "u1"))
        (tmp / "u1").unlink()
        self.assertTrue(source_removed(tmp, "u1"))

    def test_source_removed_is_false_while_only_the_sidecar_remains(self):
        # The data file alone is not the whole source. tusd's filestore resolves
        # an upload through its .info sidecar, so an orphan sidecar is a leak
        # publication was supposed to clean up.
        tmp = Path(tempfile.mkdtemp())
        (tmp / "u2.info").write_text("{}")
        self.assertFalse(source_removed(tmp, "u2"),
                         "an orphan .info sidecar was accepted as a removed source")
        (tmp / "u2.info").unlink()
        self.assertTrue(source_removed(tmp, "u2"))


if __name__ == "__main__":
    unittest.main()
