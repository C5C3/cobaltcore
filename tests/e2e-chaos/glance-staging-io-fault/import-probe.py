#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""The import probe of glance-staging-io-fault.

The suite runs it once per phase in a probe pod with ``python3 -u -c`` and
the phase in PHASE. It issues an admin token, creates the raw/bare image
stage-<phase>, prints ``IMAGE-ID <id>`` and stages 4 MiB of zero bytes with
PUT /v2/images/{id}/stage.

- latency and recovered: the stage must answer 204; the probe then posts
  /import with the glance-direct method and polls the image for up to 180 s.
  ``active`` prints LATENCY-IMPORT-ACTIVE or RECOVERED-OK.
- enospc and eio: the stage must be refused and the image must read queued:
  ``ENOSPC-STAGE-REFUSED <code>`` or ``EIO-STAGE-REFUSED <code>``. A refusal
  is a non-2xx status, or a connection Glance dropped before the status
  arrived (``<code>`` then reads ``dropped``): Glance answers as soon as its
  first write fails, with most of the body still unsent, and the close with
  unread data resets the connection. A 2xx prints ``FAIL: stage answered
  <code> while the staging volume refused writes``.
- check: no image reads uploading or importing (NO-STUCK-IMPORT-OK).
- cleanup: delete every image named stage-* (CLEANUP-OK).

Every other outcome prints a line that starts with ``FAIL:`` and exits 1.
"""

from __future__ import annotations

import http.client
import json
import os
import sys
import time
import urllib.error
import urllib.request

KEYSTONE = os.environ["KEYSTONE"].rstrip("/")
GLANCE = os.environ["GLANCE"].rstrip("/")
PHASE = os.environ["PHASE"]
PAYLOAD = b"\0" * (4 * 1024 * 1024)
IMPORT_SECONDS = 180
NOANSWER = (OSError, http.client.HTTPException)
# The server closed the connection before answering: a reset while the status
# is read (http.client's RemoteDisconnected is a ConnectionResetError), or a
# reset or broken pipe while the body is still being sent, which urllib wraps
# in a URLError.
DROPPED = (ConnectionResetError, BrokenPipeError)


def fail(message: str) -> None:
    """Print the failure and exit 1."""
    print(f"FAIL: {message}", flush=True)
    sys.exit(1)


def call(method: str, path: str, token: str, body: bytes | None = None,
         content_type: str = "application/json",
         timeout: float = 30) -> tuple[int | None, bytes]:
    """Send one request to Glance.

    The status is 0 when the server dropped the connection before answering
    and None when nothing answered at all.
    """
    headers = {"X-Auth-Token": token}
    if body is not None:
        headers["Content-Type"] = content_type
    req = urllib.request.Request(f"{GLANCE}{path}", data=body, method=method,
                                 headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as exc:
        data = exc.read()
        return exc.code, data
    except NOANSWER as exc:
        cause = exc.reason if isinstance(exc, urllib.error.URLError) else exc
        if isinstance(cause, DROPPED):
            return 0, repr(exc).encode()
        return None, repr(exc).encode()


def issue() -> str:
    """Issue a project-scoped admin token."""
    body = json.dumps({"auth": {
        "identity": {"methods": ["password"], "password": {"user": {
            "name": "admin", "domain": {"id": "default"},
            "password": os.environ["OS_PASSWORD"]}}},
        "scope": {"project": {"name": "admin",
                              "domain": {"id": "default"}}}}}).encode()
    req = urllib.request.Request(f"{KEYSTONE}/auth/tokens", data=body,
                                 method="POST",
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            return resp.headers["X-Subject-Token"]
    except urllib.error.HTTPError as exc:
        fail(f"POST /auth/tokens answered HTTP {exc.code}")
    except NOANSWER as exc:
        fail(f"POST /auth/tokens got no answer: {exc!r}")
    return ""


def status(token: str, image: str) -> str:
    """Return the status of an image."""
    code, body = call("GET", f"/v2/images/{image}", token)
    if code != 200:
        fail(f"GET /v2/images/{image} answered {code}: {body[:200]!r}")
    return json.loads(body)["status"]


def images(token: str, query: str) -> list[dict]:
    """List the images a query selects, following Glance's next links."""
    found: list[dict] = []
    path = f"/v2/images?{query}"
    while path:
        code, body = call("GET", path, token)
        if code != 200:
            fail(f"GET {path} answered {code}: {body[:200]!r}")
        page = json.loads(body)
        found.extend(page.get("images", []))
        path = page.get("next") or ""
    return found


def stage_and_import(token: str, sentinel: str) -> None:
    """Create, stage and import one image; it must turn active."""
    image = create(token)
    code, body = call("PUT", f"/v2/images/{image}/stage", token, PAYLOAD,
                      "application/octet-stream", 120)
    if code != 204:
        fail(f"stage of {image} answered {code}: {body[:200]!r}")
    code, body = call("POST", f"/v2/images/{image}/import", token,
                      json.dumps({"method": {"name": "glance-direct"}}).encode())
    if code != 202:
        fail(f"import of {image} answered {code}: {body[:200]!r}")
    deadline = time.monotonic() + IMPORT_SECONDS
    while True:
        current = status(token, image)
        if current == "active":
            print(sentinel, flush=True)
            return
        if current == "killed" or time.monotonic() >= deadline:
            fail(f"image {image} is {current}, want active within "
                 f"{IMPORT_SECONDS}s")
        time.sleep(5)


def stage_refused(token: str, sentinel: str) -> None:
    """Stage one image; the stage must be refused and the image stay queued.

    A dropped connection is a refusal: Glance answers 413 or 500 as soon as
    the first write fails, with most of the 4 MiB still unsent, and its close
    with unread data resets the connection before the status is read.
    """
    image = create(token)
    code, body = call("PUT", f"/v2/images/{image}/stage", token, PAYLOAD,
                      "application/octet-stream", 120)
    if code is None:
        fail(f"stage of {image} got no answer: {body.decode()}")
    if 200 <= code < 300:
        fail(f"stage answered {code} while the staging volume refused writes")
    current = status(token, image)
    if current != "queued":
        fail(f"image {image} is {current} after a refused stage, want queued")
    print(f"{sentinel} {code or 'dropped'}", flush=True)


def create(token: str) -> str:
    """Create the raw/bare image stage-<phase> and print its id."""
    code, body = call("POST", "/v2/images", token, json.dumps({
        "name": f"stage-{PHASE}", "disk_format": "raw",
        "container_format": "bare"}).encode())
    if code != 201:
        fail(f"POST /v2/images answered {code}: {body[:200]!r}")
    image = json.loads(body)["id"]
    print(f"IMAGE-ID {image}", flush=True)
    return image


def main() -> None:
    """Run the phase PHASE names."""
    token = issue()
    if PHASE == "latency":
        stage_and_import(token, "LATENCY-IMPORT-ACTIVE")
    elif PHASE == "recovered":
        stage_and_import(token, "RECOVERED-OK")
    elif PHASE in ("enospc", "eio"):
        stage_refused(token, f"{PHASE.upper()}-STAGE-REFUSED")
    elif PHASE == "check":
        stuck = images(token, "status=uploading") + images(token, "status=importing")
        if stuck:
            fail("images left uploading or importing: "
                 + ", ".join(f"{i['name']} ({i['id']}) {i['status']}" for i in stuck))
        print("NO-STUCK-IMPORT-OK", flush=True)
    elif PHASE == "cleanup":
        for image in images(token, "limit=100"):
            if image.get("name", "").startswith("stage-"):
                code, _ = call("DELETE", f"/v2/images/{image['id']}", token)
                print(f"DELETE {image['name']} ({image['id']}): {code}", flush=True)
        print("CLEANUP-OK", flush=True)
    else:
        fail(f"unknown PHASE {PHASE}")


if __name__ == "__main__":
    main()
