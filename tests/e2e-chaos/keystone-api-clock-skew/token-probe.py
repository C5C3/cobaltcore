#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""The token probe of keystone-api-clock-skew.

The suite starts it once in pod clock-skew-probe with ``python3 -u -c`` and
advances it phase by phase by creating marker files in the pod with
``kubectl exec clock-skew-probe -- touch /tmp/<phase>``. The probe keeps every
token it issues in its own memory, so no token leaves the pod.

Every validation sends a freshly issued admin token in X-Auth-Token and the
token under test in X-Subject-Token, so the request's own token is valid on
whatever clock the Keystone API runs. The probe prints one sentinel per check;
a value outside its expectation prints ``FAIL: <sentinel> <value>`` and exits
1.

Phases and sentinels:

- start: issue T_old, wait past its expires_at plus 30 s on the probe's clock,
  validate it: ``EXPIRED-REJECTED 404``.
- /tmp/behind: the skew of a fresh token's issued_at against the probe's
  clock, at most -3500 (``BEHIND-OFFSET <s>``), then T_old validates:
  ``BEHIND-ACCEPTS-EXPIRED 200``.
- /tmp/pre: issue T_pre: ``PRE-ISSUED``.
- /tmp/ahead: issue T_fwd and read its skew, at least 7100
  (``AHEAD-OFFSET <s>``), then T_pre is rejected: ``AHEAD-REJECTS-FRESH 404``.
- /tmp/after: T_fwd validates with an expires_at at least 6900 s after the
  probe's clock (``SKEWED-TOKEN-OUTLIVES 200``), and a fresh token issues and
  validates: ``RECOVERED-OK``.
"""

from __future__ import annotations

import http.client
import json
import os
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from typing import NamedTuple

KEYSTONE = os.environ["KEYSTONE"].rstrip("/")
MARKER_POLL = 2
RETRY_SECONDS = 120
# The TimeChaos offsets (01-timechaos-behind.yaml -1h, 02-timechaos-ahead.yaml
# 2h) less 100 s of slack, and the 2h offset less the 300 s [token]
# expiration of 00-keystone-cr.yaml for the skewed token's remaining life.
BEHIND_MAX = -3600 + 100
AHEAD_MIN = 7200 - 100
OUTLIVES_MIN = 7200 - 300
NOANSWER = (OSError, http.client.HTTPException)


class Token(NamedTuple):
    """A token and the two times Keystone stamped into it, in epoch seconds."""

    value: str
    issued_at: float
    expires_at: float


def epoch(stamp: str) -> float:
    """Parse a Keystone timestamp such as 2026-10-17T10:00:00.000000Z."""
    return datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%S.%fZ").replace(
        tzinfo=timezone.utc).timestamp()


def fail(sentinel: str, value: object) -> None:
    """Print the failed check and exit 1."""
    print(f"FAIL: {sentinel} {value}", flush=True)
    sys.exit(1)


def issue() -> Token:
    """Issue a project-scoped admin token, retrying a Keystone that restarts."""
    body = json.dumps({"auth": {
        "identity": {"methods": ["password"], "password": {"user": {
            "name": "admin", "domain": {"id": "default"},
            "password": os.environ["OS_PASSWORD"]}}},
        "scope": {"project": {"name": "admin",
                              "domain": {"id": "default"}}}}}).encode()
    deadline = time.monotonic() + RETRY_SECONDS
    while True:
        req = urllib.request.Request(
            f"{KEYSTONE}/auth/tokens", data=body, method="POST",
            headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                token = json.loads(resp.read())["token"]
                return Token(resp.headers["X-Subject-Token"],
                             epoch(token["issued_at"]),
                             epoch(token["expires_at"]))
        except urllib.error.HTTPError as exc:
            detail = f"HTTP {exc.code}"
            exc.close()
        except NOANSWER as exc:
            detail = repr(exc)
        if time.monotonic() >= deadline:
            fail("TOKEN-ISSUE", detail)
        print(f"retry for a token: {detail}", flush=True)
        time.sleep(5)


def validate(subject: str) -> tuple[int, float | None]:
    """Validate subject with a fresh admin token; return the status and expiry."""
    auth = issue().value
    req = urllib.request.Request(
        f"{KEYSTONE}/auth/tokens", method="GET",
        headers={"X-Auth-Token": auth, "X-Subject-Token": subject})
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            token = json.loads(resp.read())["token"]
            return resp.status, epoch(token["expires_at"])
    except urllib.error.HTTPError as exc:
        exc.close()
        return exc.code, None


def wait_marker(name: str) -> None:
    """Block until the suite creates /tmp/<name>."""
    while not os.path.exists(f"/tmp/{name}"):
        time.sleep(MARKER_POLL)


def check(sentinel: str, value: object, ok: bool) -> None:
    """Print the sentinel and its value, or fail with them."""
    if not ok:
        fail(sentinel, value)
    print(f"{sentinel} {value}", flush=True)


def main() -> None:
    """Run the phases in order."""
    old = issue()
    print(f"issued T_old, expires in {old.expires_at - time.time():.0f} s",
          flush=True)
    time.sleep(max(0.0, old.expires_at + 30 - time.time()))
    code, _ = validate(old.value)
    check("EXPIRED-REJECTED", code, code == 404)

    wait_marker("behind")
    offset = round(issue().issued_at - time.time())
    check("BEHIND-OFFSET", offset, offset <= BEHIND_MAX)
    code, _ = validate(old.value)
    check("BEHIND-ACCEPTS-EXPIRED", code, code == 200)

    wait_marker("pre")
    pre = issue()
    print("PRE-ISSUED", flush=True)

    wait_marker("ahead")
    forward = issue()
    offset = round(forward.issued_at - time.time())
    check("AHEAD-OFFSET", offset, offset >= AHEAD_MIN)
    code, _ = validate(pre.value)
    check("AHEAD-REJECTS-FRESH", code, code == 404)

    wait_marker("after")
    code, expires = validate(forward.value)
    left = None if expires is None else round(expires - time.time())
    print(f"T_fwd expires {left} s after the probe's clock", flush=True)
    if code != 200 or left is None or left < OUTLIVES_MIN:
        fail("SKEWED-TOKEN-OUTLIVES", f"{code}, expires_at {left} s ahead")
    print(f"SKEWED-TOKEN-OUTLIVES {code}", flush=True)
    code, _ = validate(issue().value)
    if code != 200:
        fail("RECOVERED", code)
    print("RECOVERED-OK", flush=True)


if __name__ == "__main__":
    main()
