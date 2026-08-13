"""Tests for the arrival schedules."""

import statistics
import unittest

from loadtest.wedding.arrivals import herd_schedule, poisson_schedule, split_items


def tight(ts: list[float], window: float = 0.05) -> int:
    """Count gaps small enough to count as the same instant."""
    return sum(1 for a, b in zip(ts, ts[1:]) if b - a < window)


class TestArrivals(unittest.TestCase):
    def test_poisson_mean_gap_approximates_one_over_rate(self):
        rate = 12.0  # per minute -> expected gap 5s
        times = poisson_schedule(4000, rate_per_min=rate, seed=1, cluster_fraction=0.0)
        gaps = [b - a for a, b in zip(times, times[1:])]
        self.assertAlmostEqual(statistics.mean(gaps), 60.0 / rate, delta=0.6)

    def test_schedule_is_sorted_and_deterministic(self):
        a = poisson_schedule(500, rate_per_min=30, seed=42)
        b = poisson_schedule(500, rate_per_min=30, seed=42)
        self.assertEqual(a, b)
        self.assertEqual(a, sorted(a))

    def test_clustering_makes_some_arrivals_simultaneous(self):
        # cluster_fraction draws no random numbers until after the gaps are
        # generated, so for a fixed seed the two schedules share their base
        # times exactly. The difference below is caused by clustering alone,
        # not by two independent samples happening to differ.
        for seed in (3, 17, 101, 2029):
            with self.subTest(seed=seed):
                spread = poisson_schedule(500, rate_per_min=30, seed=seed,
                                          cluster_fraction=0.0)
                clumped = poisson_schedule(500, rate_per_min=30, seed=seed,
                                           cluster_fraction=0.4)
                self.assertGreater(
                    tight(clumped), tight(spread) + 100,
                    "cluster_fraction did not actually cluster anything")

    def test_clustering_preserves_the_arrival_count(self):
        # The runner indexes the schedule positionally against the payloads.
        for fraction in (0.0, 0.15, 0.4, 0.99):
            with self.subTest(cluster_fraction=fraction):
                times = poisson_schedule(300, rate_per_min=30, seed=7,
                                         cluster_fraction=fraction)
                self.assertEqual(len(times), 300)
                self.assertEqual(times, sorted(times))

    def test_rejects_a_rate_that_produces_no_arrivals(self):
        for rate in (0.0, -1.0):
            with self.subTest(rate=rate):
                with self.assertRaisesRegex(ValueError, "rate_per_min must be positive"):
                    poisson_schedule(10, rate_per_min=rate, seed=1)

    def test_rejects_a_cluster_fraction_outside_the_unit_interval(self):
        for fraction in (-0.1, 1.0, 1.5):
            with self.subTest(cluster_fraction=fraction):
                with self.assertRaisesRegex(ValueError, r"cluster_fraction must be in \[0, 1\)"):
                    poisson_schedule(10, rate_per_min=30, seed=1,
                                     cluster_fraction=fraction)

    def test_rejects_a_negative_arrival_count(self):
        with self.assertRaisesRegex(ValueError, "n must not be negative"):
            poisson_schedule(-1, rate_per_min=30, seed=1)

    def test_herd_is_simultaneous(self):
        self.assertEqual(herd_schedule(50), [0.0] * 50)

    def test_split_items_conserves_the_total(self):
        parts = split_items(5000, 120, seed=9)
        self.assertEqual(sum(parts), 5000)
        self.assertEqual(len(parts), 120)
        self.assertTrue(all(p >= 1 for p in parts))

    def test_split_items_seats_every_guest_when_items_are_scarce(self):
        # At 5000/120 the >= 1 floor above is unfalsifiable: handing out all
        # 5000 items from a zero floor leaves a guest empty roughly once in
        # 1e18 runs (measured: 0 of 2000). Just above n_guests it happens every
        # time, so this is where the floor is actually load-bearing.
        parts = split_items(125, 120, seed=4)
        self.assertEqual(sum(parts), 125)
        self.assertEqual(len(parts), 120)
        self.assertEqual(min(parts), 1, "a guest was left with nothing to upload")

    def test_split_items_refuses_to_leave_a_guest_empty(self):
        with self.assertRaisesRegex(ValueError, "more guests than items"):
            split_items(10, 11, seed=9)


if __name__ == "__main__":
    unittest.main()
