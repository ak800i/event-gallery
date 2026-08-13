import hashlib
import subprocess
import tempfile
import unittest
from pathlib import Path

from loadtest.wedding.corpus import Asset, Payload, make_payloads


class TestPayload(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.base = self.tmp / "base.bin"
        self.base.write_bytes(b"A" * 1024)
        self.asset = Asset(name="base", path=self.base, mime="image/jpeg", kind="image")

    def test_digest_matches_the_bytes_actually_streamed(self):
        p = Payload(self.asset, marker=b"M" * 64, filename="x.jpg")
        streamed = b"".join(p.chunks(100))
        self.assertEqual(p.sha256_hex, hashlib.sha256(streamed).hexdigest())
        self.assertEqual(p.size, len(streamed))

    def test_same_base_yields_distinct_digests(self):
        a = Payload(self.asset, marker=b"A" * 64, filename="a.jpg")
        b = Payload(self.asset, marker=b"B" * 64, filename="b.jpg")
        self.assertNotEqual(a.sha256_hex, b.sha256_hex)

    def test_marker_is_exactly_64_bytes_so_size_is_predictable(self):
        p = Payload(self.asset, marker=b"M" * 64, filename="x.jpg")
        self.assertEqual(p.size, self.base.stat().st_size + 64)

    def test_make_payloads_is_deterministic_and_correctly_proportioned(self):
        assets = [self.asset]
        one = make_payloads(assets, n_photos=10, n_videos=0, seed=7)
        two = make_payloads(assets, n_photos=10, n_videos=0, seed=7)
        self.assertEqual([p.sha256_hex for p in one], [p.sha256_hex for p in two])
        self.assertEqual(len(one), 10)
        self.assertEqual(len({p.filename for p in one}), 10)


class TestAssetsRemainDecodable(unittest.TestCase):
    """A marked asset must still probe, or derivation cost is not representative."""

    def test_marked_jpeg_and_mp4_still_probe(self):
        from loadtest.wedding.corpus import build_assets

        tmp = Path(tempfile.mkdtemp())
        # Small and short on purpose: this test is about decodability, not size.
        assets = build_assets(
            tmp, photo_size="320x240", video_seconds=1, video_bytes=200_000
        )
        self.assertTrue(any(a.kind == "image" for a in assets))
        self.assertTrue(any(a.kind == "video" for a in assets))
        for asset in assets:
            p = Payload(asset, marker=b"Z" * 64, filename="probe")
            marked = tmp / f"marked-{asset.name}"
            with marked.open("wb") as fh:
                for block in p.chunks(1 << 16):
                    fh.write(block)
            proc = subprocess.run(
                [
                    "ffprobe", "-v", "error", "-show_entries", "stream=width,height",
                    "-of", "csv=p=0", str(marked),
                ],
                capture_output=True,
                text=True,
            )
            self.assertEqual(
                proc.returncode,
                0,
                f"{asset.name} stopped probing once marked: {proc.stderr}",
            )
            self.assertTrue(
                proc.stdout.strip(), f"{asset.name} reported no dimensions"
            )
