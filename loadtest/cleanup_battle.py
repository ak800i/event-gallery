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


def request(opener, url, method="GET", payload=None, headers=None):
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("User-Agent", "event-gallery-tus-battle/1.0")
    req.add_header("Accept", "application/json")
    if payload is not None:
        req.add_header("Content-Type", "application/json")
    for key, value in (headers or {}).items():
        req.add_header(key, value)
    with opener.open(req, timeout=30) as response:
        return json.loads(response.read() or b"{}")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", default=os.getenv("BASE_URL"))
    parser.add_argument("--password", default=os.getenv("ADMIN_PASSWORD", ""))
    parser.add_argument("--prefix", default="event-gallery-battle-")
    parser.add_argument("--wait-seconds", type=int, default=10)
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
    for status in ("active", "pending"):
        cursor = ""
        while True:
            query = {"status": status, "limit": "100"}
            if cursor:
                query["cursor"] = cursor
            page = request(opener, args.base_url.rstrip("/") + "/api/admin/media?" + urllib.parse.urlencode(query))
            ids.extend(item["id"] for item in page.get("items", []) if item.get("originalFilename", "").startswith(args.prefix))
            cursor = page.get("nextCursor", "")
            if not cursor:
                break

    for start in range(0, len(ids), 100):
        batch = ids[start : start + 100]
        request(
            opener,
            args.base_url.rstrip("/") + "/api/admin/media/bulk-delete",
            "POST",
            {"ids": batch},
            {"X-CSRF-Token": csrf},
        )
    print(json.dumps({"matched": len(ids), "movedToTrash": len(ids), "prefix": args.prefix}, indent=2))


if __name__ == "__main__":
    main()
