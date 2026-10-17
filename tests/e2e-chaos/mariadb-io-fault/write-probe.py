#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""The write probe of mariadb-io-fault.

The suite starts it in pod mariadb-io-writer with ``python3 -u -c`` before the
first fault. Every 2 s it creates the Keystone project io-chaos-<n> with a
30 s timeout and logs ``WRITE <n> <code|timeout|error>``. It issues its admin
token again every 60 s and on a 401; a token it cannot get logs the write as
error and is retried on the next one.

Once the suite creates /tmp/stop, it lists the projects named io-chaos-* and
requires every write that answered 201 to be among them: ``ACKED-OK <count>``,
or ``ACKED-MISSING <names>`` and exit 1. A refused or timed-out write may be
there or not. A listing that keeps failing for 60 s prints
``FAIL: cannot list io-chaos projects: <error>`` and exits 1. It then deletes
the io-chaos-* projects and prints DONE.
"""

from __future__ import annotations

import http.client
import json
import os
import socket
import sys
import time
import urllib.error
import urllib.request

KEYSTONE = os.environ["KEYSTONE"].rstrip("/")
STOP = "/tmp/stop"
PREFIX = "io-chaos-"
PERIOD = 2
WRITE_TIMEOUT = 30
TOKEN_AGE = 60
LIST_SECONDS = 60
NOANSWER = (OSError, http.client.HTTPException)


def call(method: str, path: str, token: str | None = None,
         body: dict | None = None, timeout: float = 15) -> tuple[int, dict]:
    """Send one request; return the status and the JSON body. Raises OSError."""
    headers = {"Content-Type": "application/json"}
    if token:
        headers["X-Auth-Token"] = token
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(f"{KEYSTONE}{path}", data=data,
                                 method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            token_header = resp.headers.get("X-Subject-Token")
            parsed = json.loads(raw) if raw else {}
            if token_header:
                parsed["_token"] = token_header
            return resp.status, parsed
    except urllib.error.HTTPError as exc:
        exc.close()
        return exc.code, {}


def issue() -> str | None:
    """Issue a project-scoped admin token, or None when Keystone cannot."""
    body = {"auth": {
        "identity": {"methods": ["password"], "password": {"user": {
            "name": "admin", "domain": {"id": "default"},
            "password": os.environ["OS_PASSWORD"]}}},
        "scope": {"project": {"name": "admin",
                              "domain": {"id": "default"}}}}}
    try:
        code, parsed = call("POST", "/auth/tokens", body=body)
    except NOANSWER as exc:
        print(f"token: {exc!r}", flush=True)
        return None
    if code != 201:
        print(f"token: HTTP {code}", flush=True)
        return None
    return parsed.get("_token")


def write(n: int, token: str) -> str:
    """Create io-chaos-<n>; return the status code, timeout or error."""
    body = {"project": {"name": f"{PREFIX}{n}", "domain_id": "default"}}
    try:
        code, _ = call("POST", "/projects", token, body, WRITE_TIMEOUT)
        return str(code)
    except NOANSWER as exc:
        reason = getattr(exc, "reason", exc)
        if isinstance(reason, (socket.timeout, TimeoutError)):
            return "timeout"
        return "error"


def projects(token: str) -> dict[str, str]:
    """List the io-chaos-* projects by name and id."""
    code, parsed = call("GET", "/projects", token)
    if code != 200:
        raise OSError(f"GET /projects answered HTTP {code}")
    return {p["name"]: p["id"] for p in parsed.get("projects", [])
            if p.get("name", "").startswith(PREFIX)}


def main() -> None:
    """Write until /tmp/stop exists, then check and clean up."""
    acked: list[int] = []
    token, token_at = None, 0.0
    n = 0
    while not os.path.exists(STOP):
        n += 1
        if token is None or time.monotonic() - token_at > TOKEN_AGE:
            token, token_at = issue(), time.monotonic()
        outcome = write(n, token) if token else "error"
        if outcome == "401":
            token = None
        if outcome == "201":
            acked.append(n)
        print(f"WRITE {n} {outcome}", flush=True)
        time.sleep(PERIOD)

    deadline = time.monotonic() + LIST_SECONDS
    while True:
        token = issue()
        try:
            if token is None:
                raise OSError("no token from Keystone")
            found = projects(token)
            break
        except NOANSWER as exc:
            if time.monotonic() >= deadline:
                print(f"FAIL: cannot list io-chaos projects: {exc}", flush=True)
                sys.exit(1)
            time.sleep(5)
    missing = [f"{PREFIX}{i}" for i in acked if f"{PREFIX}{i}" not in found]
    if missing:
        print(f"ACKED-MISSING {' '.join(missing)}", flush=True)
        sys.exit(1)
    print(f"ACKED-OK {len(acked)}", flush=True)
    for name, ident in sorted(found.items()):
        try:
            call("DELETE", f"/projects/{ident}", token)
        except NOANSWER as exc:
            print(f"delete {name}: {exc!r}", flush=True)
    print("DONE", flush=True)


if __name__ == "__main__":
    main()
