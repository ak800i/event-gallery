"""Merge host-collected app logs into a stage report and decide pass/fail.

Runs on the host because the sidecar has no Docker socket. This, not stage.py,
produces the authoritative `passed`.
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path

from . import observe
from .runner import decide_passed, log_evidence_ok, summarize_queue


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--report", required=True)
    ap.add_argument("--logs", required=True)
    args = ap.parse_args()

    report_path = Path(args.report)
    # Explicit UTF-8: the report and log are written by a Linux container and
    # read back on a Windows host, whose default text encoding is not UTF-8.
    report = json.loads(report_path.read_text(encoding="utf-8"))
    lines = Path(args.logs).read_text(encoding="utf-8", errors="replace").splitlines()

    report["log_levels"] = observe.count_levels(lines)
    # count_levels answers {} for output it cannot parse, and an empty ERROR
    # count then passes the criterion vacuously against a log full of errors.
    # `docker compose logs` does exactly that -- it prefixes every line with
    # `app-1  | ` -- so the verdict has to know how much of the log it read.
    # Making the parser prefix-tolerant would be worse: compose multiplexes
    # tusd and cloudflared, so their errors would be charged to the app.
    body = [line for line in lines if line.strip()]
    report["log_lines"] = {"total": len(body), "parsed": sum(report["log_levels"].values())}
    samples = observe.parse_queue_samples(lines)
    report["queue_samples"] = [{"time": q.time, **q.fields} for q in samples]
    report["queue_summary"] = summarize_queue(samples)
    report["passed"] = decide_passed(report)
    report["provisional"] = False
    report_path.write_text(json.dumps(report, indent=2), encoding="utf-8")

    print(json.dumps({
        "stage": report["stage"], "passed": report["passed"],
        "verified": report["items"]["verified"], "total": report["items"]["total"],
        "drain_seconds": report["drain_seconds"],
        "errors": report["log_levels"].get("ERROR", 0),
        "warnings": report["log_levels"].get("WARN", 0),
        "log_lines": report["log_lines"],
        "log_trustworthy": log_evidence_ok(report),
        "suspect_queue_gaps": report["queue_summary"]["suspect_gaps_seconds"],
        "aborted": report.get("aborted", ""),
    }, indent=2))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
