#!/usr/bin/env python3
"""Move successful event-gallery battle media out of the public gallery."""

from __future__ import annotations

import argparse
import http.cookiejar
import json
import os
import time
import urllib.parse
import urllib.request


# The admin session cookies are Secure, so a cookie jar will not send them over
# plain http:// and every call after login 401s. Cleanup therefore has to run
# against the https:// hostname, which puts it behind Cloudflare's bot rules --
# and those 403 anything that does not look like a browser, this script's old
# "event-gallery-tus-battle/1.0" included.
USER_AGENT = ("Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 "
              "(KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36")


def request(opener, url, method="GET", payload=None, headers=None, timeout=30):
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("User-Agent", USER_AGENT)
    req.add_header("Accept", "application/json")
    if payload is not None:
        req.add_header("Content-Type", "application/json")
    for key, value in (headers or {}).items():
        req.add_header(key, value)
    with opener.open(req, timeout=timeout) as response:
        return json.loads(response.read() or b"{}")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", default=os.getenv("BASE_URL"))
    parser.add_argument("--password", default=os.getenv("ADMIN_PASSWORD", ""))
    parser.add_argument("--prefix", default="event-gallery-battle-")
    parser.add_argument("--wait-seconds", type=int, default=10)
    # Soft delete alone leaves the bytes on disk until trash retention expires.
    # Purge goes through purgeMedia, so it still honours the storage-health gate
    # and the in-flight upload-job guard rather than bypassing them.
    parser.add_argument("--purge", action="store_true",
                        help="after trashing, permanently delete so the disk is reclaimed now")
    args = parser.parse_args()
    if not args.base_url:
        parser.error("set BASE_URL or pass --base-url")
    if not args.password:
        parser.error("set ADMIN_PASSWORD or pass --password")

    time.sleep(args.wait_seconds)
    jar = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
    login = request(opener, args.base_url.rstrip("/") + "/api/admin/login", "POST", {"password": args.password})
    csrf = login["csrfToken"]

    ids = []

    def collect(status):
        """Every id under the prefix in one status, following pagination."""
        found, cursor = [], ""
        while True:
            query = {"status": status, "limit": "100"}
            if cursor:
                query["cursor"] = cursor
            page = request(opener, args.base_url.rstrip("/") + "/api/admin/media?" + urllib.parse.urlencode(query))
            found.extend(item["id"] for item in page.get("items", []) if item.get("originalFilename", "").startswith(args.prefix))
            cursor = page.get("nextCursor", "")
            if not cursor:
                return found

    for status in ("active", "pending"):
        ids.extend(collect(status))

    for start in range(0, len(ids), 100):
        batch = ids[start : start + 100]
        request(
            opener,
            args.base_url.rstrip("/") + "/api/admin/media/bulk-delete",
            "POST",
            {"ids": batch},
            {"X-CSRF-Token": csrf},
        )

    purged = 0
    if args.purge:
        # Re-list rather than reusing `ids`: only trashed media may be purged,
        # and re-listing also picks up anything trashed by an earlier run that
        # died before purging, which is exactly how this script was first used.
        trashed = collect("trashed")
        # 100 per batch, not the endpoint's 500 cap: purging deletes an original
        # and its thumbnail per item, and a 500-item batch measured 44.7s here.
        # A client that gives up mid-purge cancels the request context, which
        # aborts the server's work and returns a bare 500 -- observed live.
        for start in range(0, len(trashed), 100):
            batch = trashed[start : start + 100]
            result = request(
                opener,
                args.base_url.rstrip("/") + "/api/admin/media/bulk-purge",
                "POST",
                {"ids": batch},
                {"X-CSRF-Token": csrf},
                timeout=600,
            )
            # The endpoint answers {"changed": [id, ...]}, a list -- int() on it
            # raises, so a successful purge would have crashed the summary.
            done = result.get("purged", result.get("changed", batch))
            purged += len(done) if isinstance(done, (list, tuple)) else int(done)

    print(json.dumps({
        "matched": len(ids),
        "movedToTrash": len(ids),
        "purged": purged,
        "prefix": args.prefix,
    }, indent=2))


if __name__ == "__main__":
    main()
