"""Arrival schedules.

Guests do not coordinate, though a few cluster -- so the realistic model is a
Poisson process with a minority of arrivals collapsed onto shared instants. The
herd schedule is strictly more stressful, so passing it implies the realistic
case.
"""

from __future__ import annotations

import random


def poisson_schedule(n: int, rate_per_min: float, seed: int,
                     cluster_fraction: float = 0.15) -> list[float]:
    """Offsets in seconds from the start of the run, sorted ascending.

    Always returns exactly ``n`` entries: the runner indexes this positionally
    against its payload list.
    """
    if n < 0:
        raise ValueError("n must not be negative")
    if rate_per_min <= 0:
        raise ValueError("rate_per_min must be positive")
    if not 0.0 <= cluster_fraction < 1.0:
        raise ValueError("cluster_fraction must be in [0, 1)")
    rng = random.Random(seed)
    mean_gap = 60.0 / rate_per_min
    times: list[float] = []
    t = 0.0
    for _ in range(n):
        t += rng.expovariate(1.0 / mean_gap)
        times.append(t)

    # Collapsing onto an existing arrival rather than jittering a new one keeps
    # the clustered guests genuinely simultaneous.
    n_clustered = int(n * cluster_fraction)
    for _ in range(n_clustered):
        src = rng.randrange(n)
        dst = rng.randrange(n)
        times[src] = times[dst]

    times.sort()
    return times


def herd_schedule(n: int) -> list[float]:
    # Same len == n contract as poisson_schedule: a negative n would otherwise
    # return [] and give the runner a silently zero-length campaign.
    if n < 0:
        raise ValueError("n must not be negative")
    return [0.0] * n


def split_items(n_items: int, n_guests: int, seed: int) -> list[int]:
    """Distribute items across guests, everyone contributing at least one."""
    if n_guests > n_items:
        raise ValueError("more guests than items")
    rng = random.Random(seed)
    parts = [1] * n_guests
    for _ in range(n_items - n_guests):
        parts[rng.randrange(n_guests)] += 1
    return parts
