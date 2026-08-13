"""Retry-After parsing, shared by the tus client and the oracle callers.

Its own module because both the transport layer and the orchestration layer
need it: keeping it in `runner` made `tusclient` import the module that
schedules and reports on it, which is backwards.
"""
from __future__ import annotations

# A Retry-After is a hint from the server, not an instruction to park the run.
MAX_RETRY_AFTER_SECONDS = 120.0


def retry_after_seconds(headers, fallback: float) -> float:
    """Seconds to wait per Retry-After, else ``fallback``.

    This server always sends an integer count; the HTTP-date form is legal but
    unparseable here and falls back, which is the safe direction.
    """
    raw = ""
    if headers is not None:
        try:
            raw = headers.get("Retry-After") or ""
        except AttributeError:
            raw = ""
    try:
        wait = float(str(raw).strip())
    except (TypeError, ValueError):
        wait = fallback
    if not wait >= 0:  # also catches NaN
        wait = fallback
    return min(max(float(wait), 0.0), MAX_RETRY_AFTER_SECONDS)
