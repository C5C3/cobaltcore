#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""The probes of the fault suites of #1391.

A suite runs it in a probe pod on the Tempest image with ``python3 -u -c``
(tests/e2e-chaos/lib/faults.sh, probe_run), so it uses the standard library
only and keeps to Python 3.10 syntax, as lib/workload.py does. Every verdict
is one line on stdout: the sentinel the caller names on success, a line that
starts with ``FAIL:`` and exit 1 otherwise. Exit 2 is a usage error.

``expect`` sends requests and compares each answer with what the check
wants: a status code, ``none`` (no HTTP status: a reset, a closed connection
or a timeout) or ``silent`` (no answer before the request's own timeout).
With ``--keystone`` it first issues a fresh project-scoped admin token with
the password in OS_PASSWORD and sends it as X-Auth-Token; ``--subject`` sends
it as X-Subject-Token too, which validates the token itself.

``cinder`` creates a volume or a backup, waits for a status, holds it,
watches it or deletes it, against a Cinder with Keystone auth or with the
noauth token of the chaos suites.
"""

from __future__ import annotations

import argparse
import http.client
import json
import os
import socket
import sys
import time
import urllib.error
import urllib.request
from typing import Any, NamedTuple

POLL_INTERVAL = 5
TOKEN_SECONDS = 120
DETAIL_LIMIT = 300

# A transport failure: no HTTP status came back. HTTPError is an OSError too
# and is caught before this tuple wherever both can occur.
NOANSWER = (OSError, http.client.HTTPException)

CINDER_NOAUTH_TOKEN = "admin:admin"
# The prefix of the status Cinder.status returns for an answer other than 200
# or 404, a no-answer among them.
UNKNOWN = "unknown ("


class Failed(Exception):
    """A verdict that fails the probe; its text follows ``FAIL: ``."""


class Answer(NamedTuple):
    """The outcome of one request: a status, or none with what happened."""

    code: int | None
    detail: str
    body: bytes
    silent: bool


def request(method: str, url: str, headers: dict[str, str] | None = None,
            body: bytes | None = None, timeout: float = 15) -> Answer:
    """Send one request and return its answer without raising."""
    req = urllib.request.Request(url, data=body, method=method,
                                 headers=dict(headers or {}))
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            data = resp.read()
            return Answer(resp.status, f"HTTP {resp.status}", data, False)
    except urllib.error.HTTPError as exc:
        data = exc.read()
        text = data[:DETAIL_LIMIT].decode("utf-8", "replace")
        return Answer(exc.code, f"HTTP {exc.code}: {text}", data, False)
    except NOANSWER as exc:
        reason = getattr(exc, "reason", exc)
        silent = isinstance(reason, (socket.timeout, TimeoutError))
        return Answer(None, repr(exc), b"", silent)


def matches(answer: Answer, want: str) -> bool:
    """Tell whether an answer is the one a check wants."""
    if want == "none":
        return answer.code is None
    if want == "silent":
        return answer.silent
    return answer.code == int(want)


class Token(NamedTuple):
    """A project-scoped token and the project it is scoped to."""

    value: str
    project: str


def issue_token(keystone: str) -> Token:
    """Issue an admin token, retrying a Keystone that does not answer yet."""
    password = os.environ.get("OS_PASSWORD", "")
    if not password:
        raise Failed("OS_PASSWORD is empty; the probe cannot issue a token")
    body = json.dumps({"auth": {
        "identity": {"methods": ["password"], "password": {"user": {
            "name": "admin", "domain": {"id": "default"},
            "password": password}}},
        "scope": {"project": {"name": "admin",
                              "domain": {"id": "default"}}}}}).encode()
    deadline = time.monotonic() + TOKEN_SECONDS
    while True:
        req = urllib.request.Request(
            f"{keystone}/auth/tokens", data=body, method="POST",
            headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                token = resp.headers["X-Subject-Token"]
                project = json.loads(resp.read())["token"]["project"]["id"]
                print(f"POST {keystone}/auth/tokens answered {resp.status}")
                return Token(token, project)
        except urllib.error.HTTPError as exc:
            detail = f"HTTP {exc.code}: {exc.read(DETAIL_LIMIT)!r}"
            if exc.code < 500:
                raise Failed(f"POST {keystone}/auth/tokens answered {detail}")
        except NOANSWER as exc:
            detail = repr(exc)
        if time.monotonic() >= deadline:
            raise Failed(f"no token from {keystone} within {TOKEN_SECONDS}s: "
                         f"{detail}")
        print(f"retry for the token: {detail}")
        time.sleep(POLL_INTERVAL)


class Check(NamedTuple):
    """One request of ``expect`` and the answer it wants."""

    method: str
    url: str
    want: str
    timeout: float


def parse_check(spec: str, timeout: float) -> Check:
    """Parse ``METHOD URL WANT[@SECONDS]``; SECONDS is the request timeout."""
    parts = spec.split()
    if len(parts) != 3:
        usage(f"--check '{spec}' is not METHOD URL WANT[@SECONDS]")
    want, _, secs = parts[2].partition("@")
    if want not in ("none", "silent") and not want.isdigit():
        usage(f"--check '{spec}': WANT is a status code, none or silent")
    try:
        limit = float(secs) if secs else timeout
    except ValueError:
        usage(f"--check '{spec}': {secs} is not a number of seconds")
    return Check(parts[0].upper(), parts[1], want, limit)


def usage(reason: str) -> None:
    """Print a usage error and exit 2."""
    print(f"usage: {reason}", file=sys.stderr)
    sys.exit(2)


def expect(args: argparse.Namespace) -> None:
    """Run every check in turn; each must get the answer it wants."""
    checks = [parse_check(spec, args.timeout) for spec in args.check]
    headers = {}
    for item in args.header:
        name, sep, value = item.partition(":")
        if not sep:
            usage(f"--header '{item}' is not NAME:VALUE")
        headers[name.strip()] = value.strip()
    project = ""
    if args.keystone:
        token = issue_token(args.keystone.rstrip("/"))
        headers["X-Auth-Token"] = token.value
        if args.subject:
            headers["X-Subject-Token"] = token.value
        project = token.project
    body = args.body.encode() if args.body else None
    for check in checks:
        url = check.url.replace("{project_id}", project)
        data = body if check.method not in ("GET", "HEAD", "DELETE") else None
        send = dict(headers)
        if data is not None:
            send["Content-Type"] = "application/json"
        deadline = time.monotonic() + args.within
        while True:
            answer = request(check.method, url, send, data, check.timeout)
            if matches(answer, check.want):
                print(f"{check.method} {url} answered {answer.detail}, "
                      f"want {check.want}")
                break
            if (args.refuse_2xx and answer.code is not None
                    and 200 <= answer.code < 300):
                raise Failed(args.refuse_2xx.replace("{code}",
                                                     str(answer.code)))
            if time.monotonic() >= deadline:
                seen = f"{check.method} {url} answered {answer.detail}"
                if args.canary:
                    raise Failed(f"{args.canary} reports AllInjected but "
                                 f"{seen}, want {check.want}")
                raise Failed(f"{seen}, want {check.want}")
            print(f"retry: {check.method} {url} answered {answer.detail}")
            time.sleep(POLL_INTERVAL)
    if args.ok:
        print(args.ok)


class Cinder:
    """The volumes and backups API of one Cinder."""

    def __init__(self, root: str, keystone: str | None) -> None:
        """Authenticate with a Keystone token, or with noauth without one."""
        root = root.rstrip("/")
        if keystone:
            token = issue_token(keystone.rstrip("/"))
            self.base = f"{root}/v3/{token.project}"
            self.headers = {"X-Auth-Token": token.value}
        else:
            self.base = f"{root}/v3"
            self.headers = {"X-Auth-Token": CINDER_NOAUTH_TOKEN}

    def call(self, method: str, path: str, body: Any = None,
             want: tuple[int, ...] = (200,)) -> Any:
        """Send one request; a status outside want fails the probe."""
        data = json.dumps(body).encode() if body is not None else None
        headers = dict(self.headers)
        if data is not None:
            headers["Content-Type"] = "application/json"
        answer = request(method, f"{self.base}{path}", headers, data, 30)
        if answer.code not in want:
            raise Failed(f"{method} {self.base}{path} answered "
                         f"{answer.detail}")
        return json.loads(answer.body) if answer.body else None

    def find(self, kind: str, name: str) -> str:
        """Return the id of the one volume or backup named name."""
        items = self.call("GET", f"/{kind}s?name={name}")[f"{kind}s"]
        ids = [i["id"] for i in items if i.get("name") == name]
        if len(ids) != 1:
            raise Failed(f"{len(ids)} {kind}s named {name}, want one")
        return ids[0]

    def status(self, kind: str, ident: str) -> str | None:
        """Return the status, or None once the item is gone."""
        answer = request("GET", f"{self.base}/{kind}s/{ident}",
                         self.headers, None, 30)
        if answer.code == 404:
            return None
        if answer.code != 200:
            return f"{UNKNOWN}{answer.detail})"
        return json.loads(answer.body)[kind]["status"]


def cinder(args: argparse.Namespace) -> None:
    """Create, wait for, hold, watch or delete one volume or backup."""
    api = Cinder(args.cinder, args.keystone)
    kind, name = args.kind, args.name
    if args.create:
        if kind == "volume":
            spec: dict[str, Any] = {"name": name, "size": args.size}
            if args.image:
                spec["imageRef"] = args.image
            ident = api.call("POST", "/volumes", {"volume": spec},
                             (202,))["volume"]["id"]
        else:
            volume = api.find("volume", args.volume)
            ident = api.call("POST", "/backups", {"backup": {
                "name": name, "volume_id": volume}}, (202,))["backup"]["id"]
        print(f"CREATED {kind} {name} {ident}")
    else:
        ident = api.find(kind, name)
    if args.watch:
        watch(api, kind, ident, args.watch)
    if args.want:
        wait_status(api, kind, name, ident, args)
    if args.delete:
        api.call("DELETE", f"/{kind}s/{ident}", None, (202,))
        deadline = time.monotonic() + args.within
        while api.status(kind, ident) is not None:
            if time.monotonic() >= deadline:
                raise Failed(f"{kind} {name} is not gone {args.within:.0f}s "
                             "after its delete")
            time.sleep(POLL_INTERVAL)
        print(f"DELETED {kind} {name}")
    if args.ok:
        print(args.ok)


def watch(api: Cinder, kind: str, ident: str, seconds: float) -> None:
    """Print STATUS <status> on every change until a final status."""
    deadline = time.monotonic() + seconds
    last = ""
    while time.monotonic() < deadline:
        status = api.status(kind, ident) or "gone"
        if status != last:
            print(f"STATUS {status}", flush=True)
            last = status
        if status in ("available", "error", "gone"):
            return
        time.sleep(POLL_INTERVAL)


def wait_status(api: Cinder, kind: str, name: str, ident: str,
                args: argparse.Namespace) -> None:
    """Wait for args.want, then require it for args.hold seconds."""
    deadline = time.monotonic() + args.within
    while True:
        status = api.status(kind, ident)
        if args.never is not None and status == args.never:
            raise Failed(args.never_message
                         or f"{kind} {name} reached {status}")
        if status == args.want:
            print(f"{kind} {name} is {status}")
            break
        if status is None or (status.startswith("error")
                              and not args.want.startswith("error")):
            raise Failed(f"{kind} {name} reached {status or 'gone'}, "
                         f"want {args.want}")
        if time.monotonic() >= deadline:
            raise Failed(f"{kind} {name} is {status} after "
                         f"{args.within:.0f}s, want {args.want}")
        time.sleep(POLL_INTERVAL)
    # The hold ends at the first read after its end that names args.want.
    # No answer, or one that names no status, neither breaks nor ends the
    # hold; past its end such a read is retried for up to args.within.
    held = time.monotonic() + args.hold
    give_up = held + args.within
    while args.hold:
        time.sleep(POLL_INTERVAL)
        status = api.status(kind, ident)
        now = time.monotonic()
        if status is not None and status.startswith(UNKNOWN):
            print(f"retry during the hold: {status}")
            if now >= give_up:
                raise Failed(f"{kind} {name}: no read in the "
                             f"{args.within:.0f}s after the {args.hold:.0f}s "
                             f"hold named a status, want {args.want}")
            continue
        if status != args.want:
            if (args.never is not None and status == args.never
                    and args.never_message):
                raise Failed(args.never_message)
            raise Failed(f"{kind} {name} turned {status or 'gone'} within "
                         f"{args.hold:.0f}s of reading {args.want}")
        if now >= held:
            break
    if args.hold:
        print(f"{kind} {name} stayed {args.want} for {args.hold:.0f}s")


def main(argv: list[str] | None = None) -> int:
    """Parse the command line and run one probe."""
    parser = argparse.ArgumentParser(prog="probe.py")
    sub = parser.add_subparsers(dest="command", required=True)

    exp = sub.add_parser("expect")
    exp.add_argument("--check", action="append", required=True,
                     help="METHOD URL WANT[@SECONDS], in order")
    exp.add_argument("--keystone", help="the Keystone /v3 root for a token")
    exp.add_argument("--subject", action="store_true",
                     help="send the token as X-Subject-Token too")
    exp.add_argument("--header", action="append", default=[],
                     help="NAME:VALUE for every request")
    exp.add_argument("--body", help="the JSON body of a non-GET request")
    exp.add_argument("--timeout", type=float, default=15)
    exp.add_argument("--within", type=float, default=0,
                     help="retry every 5 s for this long")
    exp.add_argument("--refuse-2xx",
                     help="fail with this text, {code} filled, on a 2xx")
    exp.add_argument("--canary", help="<kind>/<name> of the fault a "
                     "canary proves; it names the object on failure")
    exp.add_argument("--ok", help="the line printed when every check passed")

    cin = sub.add_parser("cinder")
    cin.add_argument("--cinder", required=True, help="the Cinder root")
    cin.add_argument("--keystone", help="the Keystone /v3 root; noauth "
                     "without it")
    cin.add_argument("kind", choices=("volume", "backup"))
    cin.add_argument("name")
    cin.add_argument("--create", action="store_true")
    cin.add_argument("--size", type=int, default=1)
    cin.add_argument("--image", help="the image a created volume is from")
    cin.add_argument("--volume", help="the volume a created backup is of")
    cin.add_argument("--want", help="the status to wait for")
    cin.add_argument("--within", type=float, default=120)
    cin.add_argument("--hold", type=float, default=0)
    cin.add_argument("--never", help="a status that fails at once")
    cin.add_argument("--never-message", help="the FAIL text of --never")
    cin.add_argument("--watch", type=float, default=0,
                     help="print every status change for this long")
    cin.add_argument("--delete", action="store_true")
    cin.add_argument("--ok")

    args = parser.parse_args(argv)
    if args.command == "cinder" and args.create and args.kind == "backup" \
            and not args.volume:
        usage("cinder backup --create needs --volume")
    try:
        if args.command == "expect":
            expect(args)
        else:
            cinder(args)
    except Failed as exc:
        print(f"FAIL: {exc}", flush=True)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
