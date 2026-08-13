import hashlib
import shutil
import subprocess
import tempfile
import unittest
from collections import Counter
from pathlib import Path
from unittest import mock

from loadtest.wedding import corpus
from loadtest.wedding.corpus import (
    PHOTO_VARIANT_WEIGHTS,
    Asset,
    Payload,
    make_payloads,
)


class TestPayload(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
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

    def test_photo_variants_are_drawn_at_the_declared_weights(self):
        images = [
            Asset(name=mime, path=self.base, mime=mime, kind="image")
            for mime in PHOTO_VARIANT_WEIGHTS
        ]
        n = 4000
        drawn = Counter(
            p.mime for p in make_payloads(images, n_photos=n, n_videos=0, seed=99)
        )
        for mime, want in PHOTO_VARIANT_WEIGHTS.items():
            with self.subTest(mime=mime):
                self.assertAlmostEqual(drawn[mime] / n, want, delta=0.03)

    def test_no_image_assets_raises_like_the_video_path_does(self):
        videos = [Asset(name="v", path=self.base, mime="video/mp4", kind="video")]
        with self.assertRaises(ValueError):
            make_payloads(videos, n_photos=1, n_videos=0, seed=7)


class TestAssetsRemainDecodable(unittest.TestCase):
    """A marked asset must still probe, or derivation cost is not representative."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def test_marked_assets_probe_and_keep_their_magic_at_offset_zero(self):
        # Small and short on purpose: this test is about decodability, not size.
        assets = corpus.build_assets(
            self.tmp, photo_size="320x240", video_seconds=1, video_bytes=200_000
        )
        self.assertTrue(any(a.kind == "image" for a in assets))
        self.assertTrue(any(a.kind == "video" for a in assets))
        for asset in assets:
            with self.subTest(asset=asset.name):
                p = Payload(asset, marker=b"Z" * 64, filename="probe")
                marked = self.tmp / f"marked-{asset.name}"
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
                # media.Sniff matches magic bytes at offset 0, which is stricter
                # than ffprobe: it scans for JPEG's SOI rather than requiring it
                # first. Pin the property the server actually enforces.
                with marked.open("rb") as fh:
                    head = fh.read(12)
                with asset.path.open("rb") as fh:
                    want = fh.read(12)
                self.assertEqual(head, want, f"{asset.name} moved its magic bytes")

    def test_an_interrupted_generation_leaves_nothing_a_later_run_would_reuse(self):
        def sabotage(cmd):
            Path(cmd[-1]).write_bytes(b"truncated")
            raise RuntimeError("ffmpeg interrupted")

        with mock.patch.object(corpus, "_run", sabotage):
            with self.assertRaises(RuntimeError):
                corpus.build_assets(
                    self.tmp, photo_size="320x240", video_seconds=1, video_bytes=200_000
                )
        self.assertFalse(
            (self.tmp / "photo-jpeg.jpg").exists(),
            "a truncated asset was left where build_assets would silently reuse it",
        )
