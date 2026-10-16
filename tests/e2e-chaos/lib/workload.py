#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Workload and consistency harness of the chaos suites.

``run`` is the workload. tests/e2e-chaos/lib/workload.sh starts it with
``python3 -c`` in a pod on the Tempest image, so it uses the standard library
only and keeps to Python 3.10 syntax. It runs one thread per stream and writes
one JSON record per operation to stdout. Once the stop file exists it deletes
every resource it named, checks the state of each service it used and prints
DONE. ``--resolve HOST:ADDR`` connects every URL that names HOST to ADDR and
keeps HOST in the Host header and the TLS server name, so a stream can reach a
Gateway listener by its hostname; ``--insecure-tls`` skips the certificate
check of HTTPS endpoints.

``evaluate`` runs on the CI runner. It reads the log of a ``run`` together with
the fault marks and the error budget of the suite, prints one summary line per
stream and exits 0 when the run kept to the budget, 1 when it did not and 2 on
an input error.

docs/reference/testing/chaos-e2e-tests.md describes both.
"""

from __future__ import annotations

import argparse
import http.client
import ipaddress
import json
import os
import socket
import ssl
import sys
import threading
import time
import traceback
import urllib.error
import urllib.request
from typing import Any, Callable, NamedTuple

HTTP_TIMEOUT = 15
CYCLE_PAUSE = 1
POLL_INTERVAL = 2
STOP_POLL = 0.5
SERVER_ACTIVE_SECONDS = 300
STATUS_WAIT_SECONDS = 120
SWEEP_SECONDS = 180
SWEEP_RETRY = 5
SETTLE_SECONDS = 120
SETTLE_INTERVAL = 5
MSG_LIMIT = 200
DETAIL_LIMIT = 10

# The microversions of the pinned releases: nova 33.0.0 accepts
# "networks": "none" from 2.37, cinder 28.0.0 defines NEW_ATTACH = '3.27'
# and NEW_ATTACH_COMPLETION = '3.44' (cinder/api/microversions.py).
NOVA_VERSION = {"OpenStack-API-Version": "compute 2.37"}
CINDER_ATTACH = {"OpenStack-API-Version": "volume 3.27"}
CINDER_COMPLETE = {"OpenStack-API-Version": "volume 3.44"}

# Cinder runs auth_strategy = noauth in every chaos suite and takes this token.
CINDER_NOAUTH_TOKEN = "admin:admin"

CONNECTOR = {
    "host": "chaos-workload",
    "initiator": "iqn.2026-10.io.c5c3:chaos-workload",
    "platform": "x86_64",
    "os_type": "linux",
    "multipath": False,
}

IMAGE_PAYLOAD = b"\0" * (1024 * 1024)

SERVER_BUSY = ("BUILD", "ERROR")
VOLUME_BUSY = (
    "creating", "attaching", "detaching", "deleting", "extending",
    "reserved", "downloading", "uploading", "retyping", "backing-up",
    "restoring-backup", "maintenance", "awaiting-transfer",
)
IMAGE_BUSY = ("saving", "queued")

# HTTPError, URLError and socket.timeout are all OSErrors. http.client raises
# its own HTTPException family (IncompleteRead, BadStatusLine) when a pod dies
# in the middle of a response, which is as much a failed request as a reset.
TRANSPORT_ERRORS = (OSError, http.client.HTTPException)


class OpFailed(Exception):
    """An operation that got an answer, but not the one it needs."""

    def __init__(self, msg: str, code: int | None = None) -> None:
        """Keep the HTTP status of the answer, or None when there was none."""
        super().__init__(msg)
        self.code = code


# Every way a request can fail: a wrong answer or no answer.
REQUEST_ERRORS = (OpFailed,) + TRANSPORT_ERRORS


class Abandoned(Exception):
    """The stop file ended a wait or an unstarted operation: no record."""


class CycleFailed(Exception):
    """An operation failed and was recorded; the rest of the cycle is left."""


class Response(NamedTuple):
    """One HTTP answer: its status, its headers and its body.

    The body is the parsed JSON, or the raw bytes of a call that asked for no
    parsing.
    """

    status: int
    headers: Any
    body: Any


class Recorder:
    """Write records and plain lines to stdout, one whole line at a time."""

    def __init__(self) -> None:
        """Create the lock the stream threads share."""
        self._lock = threading.Lock()

    def line(self, text: str) -> None:
        """Print one plain line such as READY, CONSISTENCY-OK or DONE."""
        with self._lock:
            sys.stdout.write(text + "\n")
            sys.stdout.flush()

    def record(self, start: float, stream: str, op: str, ok: bool,
               code: int | None, msg: str = "") -> None:
        """Print the JSON record of one operation that began at start."""
        self.line(json.dumps({
            "t": round(start, 3),
            "s": stream,
            "op": op,
            "ok": ok,
            "code": code,
            "ms": int((time.time() - start) * 1000),
            "msg": msg[:MSG_LIMIT],
        }))


def http_detail(exc: urllib.error.HTTPError) -> str:
    """Describe an HTTP error by its status and the start of its body."""
    try:
        body = exc.read(MSG_LIMIT).decode("utf-8", "replace")
    except TRANSPORT_ERRORS:
        body = ""
    finally:
        exc.close()
    return f"HTTP {exc.code} {exc.reason}: {body}".strip()


def failure(exc: Exception) -> tuple[int | None, str]:
    """Return the code and the message a failed request is recorded with."""
    if isinstance(exc, urllib.error.HTTPError):
        return exc.code, http_detail(exc)
    if isinstance(exc, OpFailed):
        return exc.code, str(exc)
    return None, repr(exc)


def need(resp: Response, *keys: str) -> Any:
    """Return the value at keys in the body, or fail the operation."""
    value = resp.body
    for key in keys:
        if not isinstance(value, dict) or key not in value:
            raise OpFailed(f"answer has no {'.'.join(keys)}", resp.status)
        value = value[key]
    return value


def token_body(password: str) -> dict:
    """Build the project-scoped password request for the bootstrap admin."""
    return {"auth": {
        "identity": {"methods": ["password"], "password": {"user": {
            "name": "admin", "domain": {"id": "default"},
            "password": password}}},
        "scope": {"project": {"name": "admin", "domain": {"id": "default"}}},
    }}


class Client:
    """Send HTTP requests with a 15 s timeout and an optional token."""

    # The TLS context of every HTTPS request, set once by --insecure-tls
    # before the first stream starts. None verifies against the system store.
    tls: ssl.SSLContext | None = None

    def __init__(self, tokens: TokenSource | None = None,
                 static_token: str | None = None) -> None:
        """Use tokens from a TokenSource, a fixed token, or none at all."""
        # In-cluster Service names never go through a proxy, and a proxy
        # variable on the runner would otherwise reroute the unit test.
        handlers: list[Any] = [urllib.request.ProxyHandler({})]
        if Client.tls is not None:
            handlers.append(urllib.request.HTTPSHandler(context=Client.tls))
        self._opener = urllib.request.build_opener(*handlers)
        self.tokens = tokens
        self._static = static_token

    def call(self, method: str, url: str, expect: Any,
             body: Any = None, data: bytes | None = None,
             headers: dict | None = None, parse: bool = True) -> Response:
        """Send one request and return the answer when its status is expected.

        A status outside expect raises OpFailed, an error status raises
        urllib's HTTPError. A 401 to a request carrying a TokenSource token
        issues a new token and sends the request once more.
        """
        retried = False
        while True:
            sent = dict(headers or {})
            token = None
            if self.tokens is not None:
                token = self.tokens.token()
                sent["X-Auth-Token"] = token
            elif self._static is not None:
                sent["X-Auth-Token"] = self._static
            try:
                return self.send(method, url, expect, body, data, sent, parse)
            except urllib.error.HTTPError as exc:
                if exc.code != 401 or token is None or retried:
                    raise
                exc.close()
                self.tokens.invalidate(token)
                retried = True

    def send(self, method: str, url: str, expect: Any, body: Any,
             data: bytes | None, headers: dict, parse: bool) -> Response:
        """Send one request as it is, without the token handling of call.

        Without parse the answer keeps its raw bytes as its body.
        """
        headers.setdefault("Accept", "application/json")
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(url, data=data, method=method,
                                     headers=headers)
        with self._opener.open(req, timeout=HTTP_TIMEOUT) as resp:
            raw = resp.read()
            status = resp.status
            resp_headers = resp.headers
        if status not in expect:
            raise OpFailed(f"{method} {url} answered {status}", status)
        if not parse:
            return Response(status, resp_headers, raw)
        parsed = None
        if raw:
            try:
                parsed = json.loads(raw)
            except ValueError as exc:
                raise OpFailed(f"{method} {url} answered no JSON: {exc}",
                               status) from exc
        return Response(status, resp_headers, parsed)


class TokenSource:
    """Hold one project-scoped admin token for one stream."""

    def __init__(self, keystone: str, password: str) -> None:
        """Remember where and as whom to authenticate."""
        self._keystone = keystone
        self._password = password
        self._client = Client()
        self._token: str | None = None
        self.project_id = ""

    def token(self) -> str:
        """Return the current token, issuing one when there is none."""
        if self._token is None:
            resp = issue_token(self._client, self._keystone, self._password)
            self._token = resp.headers["X-Subject-Token"]
            self.project_id = need(resp, "token", "project", "id")
        return self._token

    def invalidate(self, token: str) -> None:
        """Drop the token a request was refused with."""
        if self._token == token:
            self._token = None


def issue_token(client: Client, keystone: str, password: str) -> Response:
    """POST /auth/tokens and fail unless the answer carries a token."""
    resp = client.call("POST", f"{keystone}/auth/tokens", expect=(201,),
                       body=token_body(password))
    if not resp.headers.get("X-Subject-Token"):
        raise OpFailed("POST /auth/tokens answered without X-Subject-Token",
                       resp.status)
    return resp


class Stream:
    """One stream: a thread that runs cycles until the stop file exists.

    Subclasses define cycle(). A subclass that creates resources also defines
    sweep(), which deletes every resource named with the workload prefix, and
    checks(), which names the consistency checks of its service.
    """

    name = ""
    # The run options the stream needs, by their argparse names.
    requires: tuple[str, ...] = ()
    # A stream that uses a token takes it from --keystone. Without
    # --keystone it sends noauth_token, or no token when that is None.
    uses_token = False
    noauth_token: str | None = None

    def __init__(self, ctx: Context) -> None:
        """Bind the stream to the shared recorder, stop event and options."""
        self.ctx = ctx
        self.rec = ctx.rec
        self.stop = ctx.stop
        self.client = ctx.client(self)
        self.sweeping = False
        # What the current cycle created and has not deleted yet:
        # (kind, url, expected delete status, headers).
        self.created: list[tuple[str, str, tuple[int, ...], dict | None]] = []

    def resource_name(self, n: int) -> str:
        """Name the resources of cycle n."""
        return f"{self.ctx.prefix}{self.name}-{n}"

    def pause(self, seconds: float) -> None:
        """Wait; a wait the stop file interrupts abandons its operation."""
        if self.sweeping:
            time.sleep(seconds)
        elif self.stop.wait(seconds):
            raise Abandoned()

    def measure(self, op: str, fn: Callable[[], tuple[Response, Any]]) -> Any:
        """Run one operation, write its record and return its value."""
        if self.stop.is_set() and not self.sweeping:
            raise Abandoned()
        start = time.time()
        try:
            resp, value = fn()
        except REQUEST_ERRORS as exc:
            self.rec.record(start, self.name, op, False, *failure(exc))
            raise CycleFailed() from exc
        self.rec.record(start, self.name, op, True, resp.status)
        return value

    def loop(self) -> None:
        """Run cycles one second apart and print READY after the first ok."""
        ready = False
        n = 0
        while not self.stop.is_set():
            n += 1
            try:
                self.cycle(n)
            except CycleFailed:
                self.clean_up()
            except Abandoned:
                pass
            else:
                if not ready:
                    self.rec.line(f"READY {self.name}")
                    ready = True
            if self.stop.wait(CYCLE_PAUSE):
                break

    def cycle(self, n: int) -> None:
        """Run the operations of one cycle."""
        raise NotImplementedError

    def track(self, kind: str, url: str, expect: tuple[int, ...],
              headers: dict | None = None) -> None:
        """Remember a resource the cycle created until the cycle deletes it."""
        self.created.append((kind, url, expect, headers))

    def remove(self, op: str, url: str) -> None:
        """Run the cycle's own delete of a tracked resource."""
        _, _, expect, headers = next(c for c in self.created if c[1] == url)
        self.measure(op, lambda: (self.client.call(
            "DELETE", url, expect=expect, headers=headers, parse=False), None))
        self.created = [c for c in self.created if c[1] != url]

    def clean_up(self) -> None:
        """Delete what a failed cycle left behind, newest first.

        Without it every failed cycle would leave a volume, a server or a
        network behind, and a fault of a few minutes would exhaust a quota of
        the admin project long before the sweep runs. Each delete writes a
        cleanup-<kind> record; a failed one leaves the resource to the sweep.
        """
        leftovers, self.created = self.created, []
        for kind, url, expect, headers in reversed(leftovers):
            try:
                self.measure(f"cleanup-{kind}",
                             lambda u=url, e=expect, h=headers: (
                                 self.delete_once(u, e, h), None))
            except CycleFailed:
                continue
            except Abandoned:
                return

    def delete_once(self, url: str, expect: tuple[int, ...],
                    headers: dict | None = None) -> Response:
        """DELETE url once; a 404 means it is gone already."""
        try:
            return self.client.call("DELETE", url, expect=expect,
                                    headers=headers, parse=False)
        except urllib.error.HTTPError as exc:
            if exc.code != 404:
                raise
            exc.close()
            return Response(404, exc.headers, None)

    def sweep(self) -> None:
        """Delete every leftover resource; streams without any do nothing."""

    def checks(self) -> list[tuple[str, Callable[[], list[str]]]]:
        """Name the consistency checks of the stream's service."""
        return []

    def poll(self, url: str, field: tuple[str, ...], want: str,
             fails: Callable[[str], bool], timeout: float,
             headers: dict | None = None) -> Response:
        """GET url every 2 s until field reads want; fail on fails(status)."""
        deadline = time.monotonic() + timeout
        while True:
            resp = self.client.call("GET", url, expect=(200,),
                                    headers=headers)
            status = need(resp, *field)
            if status == want:
                return resp
            if fails(status):
                fault = fault_of(resp.body)
                raise OpFailed(f"{field[0]} reached {status}"
                               + (f": {fault}" if fault else ""))
            if time.monotonic() >= deadline:
                raise OpFailed(f"{field[0]} still {status}, not {want}, "
                               f"after {timeout:g}s")
            self.pause(POLL_INTERVAL)

    def wait_gone(self, url: str, timeout: float,
                  headers: dict | None = None) -> Response:
        """GET url every 2 s until it answers 404."""
        deadline = time.monotonic() + timeout
        while True:
            try:
                self.client.call("GET", url, expect=(200,), headers=headers)
            except urllib.error.HTTPError as exc:
                if exc.code != 404:
                    raise
                exc.close()
                return Response(404, exc.headers, None)
            if time.monotonic() >= deadline:
                raise OpFailed(f"{url} still present after {timeout:g}s")
            self.pause(POLL_INTERVAL)

    def sweep_delete(self, url: str, expect: tuple[int, ...], deadline: float,
                     headers: dict | None = None) -> Response:
        """DELETE url for the sweep: 404 is gone, 400 and 409 are retried."""
        while True:
            try:
                self.delete_once(url, expect, headers)
            except urllib.error.HTTPError as exc:
                if exc.code not in (400, 409) or \
                        time.monotonic() + SWEEP_RETRY >= deadline:
                    raise
                exc.close()
                self.pause(SWEEP_RETRY)
                continue
            remaining = max(deadline - time.monotonic(), 0)
            return self.wait_gone(url, remaining, headers)

    def sweep_each(self, kind: str, items: list[dict], url: str,
                   expect: tuple[int, ...], deadline: float,
                   headers: dict | None = None,
                   before: Callable[[dict], None] | None = None) -> None:
        """Delete every item whose name carries the prefix, one record each."""
        for item in items:
            if not str(item.get("name") or "").startswith(self.ctx.prefix):
                continue
            if before is not None:
                before(item)
            try:
                self.measure(f"sweep-{kind}", lambda i=item: (
                    self.sweep_delete(url.format(id=i["id"]), expect,
                                      deadline, headers), None))
            except CycleFailed:
                pass

    def sweep_list(self, kind: str, fn: Callable[[], list[dict]]) -> list[dict]:
        """List what the sweep looks at; only a failed listing is recorded."""
        start = time.time()
        try:
            return fn()
        except REQUEST_ERRORS as exc:
            self.rec.record(start, self.name, f"sweep-{kind}", False,
                            *failure(exc))
        return []


def fault_of(body: Any) -> str:
    """Return the fault message of a Nova server body, if it has one."""
    if isinstance(body, dict):
        server = body.get("server")
        if isinstance(server, dict) and isinstance(server.get("fault"), dict):
            return str(server["fault"].get("message", ""))
    return ""


def prefixed(items: list[dict], prefix: str, what: str) -> list[str]:
    """Report every item whose name carries the workload prefix."""
    return [f"{what} {i.get('name')} ({i.get('id')}) is left over"
            for i in items if str(i.get("name") or "").startswith(prefix)]


def stuck(items: list[dict], busy: tuple[str, ...], what: str) -> list[str]:
    """Report every item whose status is a transition that should be over."""
    return [f"{what} {i.get('name')} ({i.get('id')}) is {i.get('status')}"
            for i in items if i.get("status") in busy]


class TokenStream(Stream):
    """Issue a token and validate it, a fresh token every cycle."""

    name = "token"
    requires = ("keystone",)

    def cycle(self, n: int) -> None:
        """Run issue and validate."""
        keystone = self.ctx.args.keystone
        token = self.measure("issue", lambda: self.issue(keystone))
        self.measure("validate", lambda: (self.client.call(
            "GET", f"{keystone}/auth/tokens", expect=(200,),
            headers={"X-Auth-Token": token, "X-Subject-Token": token}), None))

    def issue(self, keystone: str) -> tuple[Response, str]:
        """Issue one token and return it."""
        resp = issue_token(self.client, keystone, self.ctx.password)
        return resp, resp.headers["X-Subject-Token"]


class HttpStream(Stream):
    """GET one URL without a token."""

    name = "http"
    requires = ("http",)

    def cycle(self, n: int) -> None:
        """Run get."""
        self.measure("get", self.get)

    def get(self) -> tuple[Response, None]:
        """GET the URL; any final status below 400 is an answer."""
        try:
            return self.client.call("GET", self.ctx.args.http,
                                    expect=range(200, 400), parse=False), None
        except urllib.error.HTTPError as exc:
            # urllib follows 301, 302, 303, 307 and 308 and raises on every
            # other status outside 2xx, Glance's 300 among them.
            if exc.code >= 400:
                raise
            exc.close()
            return Response(exc.code, exc.headers, None), None


class NetworkStream(Stream):
    """Create a network and a port on it, then delete both."""

    name = "network"
    requires = ("neutron", "keystone")
    uses_token = True

    def cycle(self, n: int) -> None:
        """Run create-network, create-port, delete-port, delete-network."""
        base = f"{self.ctx.args.neutron}/v2.0"
        name = self.resource_name(n)
        net = self.measure("create-network", lambda: self.create(
            f"{base}/networks", {"network": {"name": name}}, "network"))
        self.track("network", f"{base}/networks/{net}", (204,))
        port = self.measure("create-port", lambda: self.create(
            f"{base}/ports", {"port": {"name": name, "network_id": net}},
            "port"))
        self.track("port", f"{base}/ports/{port}", (204,))
        self.remove("delete-port", f"{base}/ports/{port}")
        self.remove("delete-network", f"{base}/networks/{net}")

    def create(self, url: str, body: dict, kind: str) -> tuple[Response, str]:
        """POST one resource and return its id."""
        resp = self.client.call("POST", url, expect=(201,), body=body)
        return resp, need(resp, kind, "id")

    def listing(self, kind: str) -> list[dict]:
        """List every network or port the admin token sees."""
        resp = self.client.call("GET", f"{self.ctx.args.neutron}/v2.0/{kind}",
                                expect=(200,))
        return need(resp, kind)

    def sweep(self) -> None:
        """Delete leftover ports first, then leftover networks."""
        deadline = time.monotonic() + SWEEP_SECONDS
        base = f"{self.ctx.args.neutron}/v2.0"
        for kind in ("ports", "networks"):
            items = self.sweep_list(kind, lambda k=kind: self.listing(k))
            self.sweep_each(kind, items, f"{base}/{kind}/{{id}}", (204,),
                            deadline)

    def checks(self) -> list[tuple[str, Callable[[], list[str]]]]:
        """Check that no network and no port carries the prefix."""
        def networks() -> list[str]:
            return (prefixed(self.listing("networks"), self.ctx.prefix,
                             "network")
                    + prefixed(self.listing("ports"), self.ctx.prefix, "port"))
        return [("networks", networks)]


class ServerStream(Stream):
    """Boot a server without a NIC, wait for ACTIVE, delete it."""

    name = "server"
    requires = ("nova", "image_ref", "flavor", "keystone")
    uses_token = True

    def __init__(self, ctx: Context) -> None:
        """Start without a flavor id; the first create resolves it."""
        super().__init__(ctx)
        self.flavor_id: str | None = None

    def cycle(self, n: int) -> None:
        """Run create, active, delete, gone."""
        nova = self.ctx.args.nova
        name = self.resource_name(n)
        server = self.measure("create", lambda: self.create(name))
        url = f"{nova}/servers/{server}"
        self.track("server", url, (204,), NOVA_VERSION)
        self.measure("active", lambda: (self.poll(
            url, ("server", "status"), "ACTIVE", lambda s: s == "ERROR",
            SERVER_ACTIVE_SECONDS, NOVA_VERSION), None))
        self.remove("delete", url)
        self.measure("gone", lambda: (self.wait_gone(
            url, STATUS_WAIT_SECONDS, NOVA_VERSION), None))

    def create(self, name: str) -> tuple[Response, str]:
        """POST /servers, resolving the flavor name on the first call."""
        nova = self.ctx.args.nova
        if self.flavor_id is None:
            resp = self.client.call("GET", f"{nova}/flavors", expect=(200,),
                                    headers=NOVA_VERSION)
            for flavor in need(resp, "flavors"):
                if flavor.get("name") == self.ctx.args.flavor:
                    self.flavor_id = str(flavor.get("id"))
            if self.flavor_id is None:
                raise OpFailed(f"no flavor {self.ctx.args.flavor} in "
                               "GET /flavors", resp.status)
        resp = self.client.call("POST", f"{nova}/servers", expect=(202,),
                                headers=NOVA_VERSION, body={"server": {
                                    "name": name,
                                    "imageRef": self.ctx.args.image_ref,
                                    "flavorRef": self.flavor_id,
                                    "networks": "none"}})
        return resp, need(resp, "server", "id")

    def listing(self) -> list[dict]:
        """List every server of every project."""
        resp = self.client.call(
            "GET", f"{self.ctx.args.nova}/servers/detail?all_tenants=1",
            expect=(200,), headers=NOVA_VERSION)
        return need(resp, "servers")

    def sweep(self) -> None:
        """Delete every leftover server."""
        deadline = time.monotonic() + SWEEP_SECONDS
        items = self.sweep_list("servers", self.listing)
        self.sweep_each("servers", items, f"{self.ctx.args.nova}/servers/{{id}}",
                        (204,), deadline, NOVA_VERSION)

    def checks(self) -> list[tuple[str, Callable[[], list[str]]]]:
        """Check for servers in BUILD or ERROR and servers left over."""
        def servers() -> list[str]:
            items = self.listing()
            return (stuck(items, SERVER_BUSY, "server")
                    + prefixed(items, self.ctx.prefix, "server"))
        return [("servers", servers)]


class VolumeStream(Stream):
    """Create a volume, attach it through the attachments API, delete it."""

    name = "volume"
    requires = ("cinder",)
    uses_token = True
    noauth_token = CINDER_NOAUTH_TOKEN

    def base(self) -> str:
        """Return the /v3 root: with the project id when a token is used."""
        if self.client.tokens is None:
            return f"{self.ctx.args.cinder}/v3"
        self.client.tokens.token()
        return f"{self.ctx.args.cinder}/v3/{self.client.tokens.project_id}"

    def cycle(self, n: int) -> None:
        """Run the ten operations of the attach cycle."""
        name = self.resource_name(n)
        vol = self.measure("create", lambda: self.create(name))
        self.track("volume", vol, (202,))
        self.measure("available", lambda: self.wait_status(vol, "available"))
        att = self.measure("attach-create", lambda: self.attach_create(vol))
        self.track("attachment", att, (200,), CINDER_ATTACH)
        self.measure("attach-update", lambda: (self.client.call(
            "PUT", att, expect=(200,), headers=CINDER_ATTACH,
            body={"attachment": {"connector": CONNECTOR}}), None))
        self.measure("attach-complete", lambda: (self.client.call(
            "POST", f"{att}/action", expect=(204,), headers=CINDER_COMPLETE,
            body={"os-complete": None}, parse=False), None))
        self.measure("in-use", lambda: self.wait_status(vol, "in-use"))
        self.remove("detach", att)
        self.measure("detached", lambda: self.wait_status(vol, "available"))
        self.remove("delete", vol)
        self.measure("gone", lambda: (self.wait_gone(
            vol, STATUS_WAIT_SECONDS), None))

    def create(self, name: str) -> tuple[Response, str]:
        """POST /volumes for a 1 GiB volume and return its URL."""
        base = self.base()
        resp = self.client.call("POST", f"{base}/volumes", expect=(202,),
                                body={"volume": {"size": 1, "name": name}})
        return resp, f"{base}/volumes/{need(resp, 'volume', 'id')}"

    def wait_status(self, vol: str, want: str) -> tuple[Response, None]:
        """Poll the volume at its URL for 120 s; an error status fails at once."""
        return self.poll(vol, ("volume", "status"), want,
                         lambda s: s.startswith("error"),
                         STATUS_WAIT_SECONDS), None

    def attach_create(self, vol: str) -> tuple[Response, str]:
        """Reserve the volume at vol and return the URL of the attachment.

        The attachment has no connector yet and names no instance: cinder
        28.0.0 asks Nova before it deletes an attachment that names an
        instance and carries connection info (attachment_deletion_allowed,
        bug #2004555), and the noauth Cinder of the chaos suites has no Nova
        to ask, so it would answer the detach with 500.
        """
        base = self.base()
        resp = self.client.call(
            "POST", f"{base}/attachments", expect=(200,), headers=CINDER_ATTACH,
            body={"attachment": {"volume_uuid": vol.rsplit("/", 1)[1]}})
        return resp, f"{base}/attachments/{need(resp, 'attachment', 'id')}"

    def listing(self) -> list[dict]:
        """List every volume of every project."""
        resp = self.client.call(
            "GET", f"{self.base()}/volumes/detail?all_tenants=1",
            expect=(200,))
        return need(resp, "volumes")

    def sweep(self) -> None:
        """Delete the attachments of every leftover volume, then the volume."""
        deadline = time.monotonic() + SWEEP_SECONDS
        items = self.sweep_list("volumes", self.listing)
        if not items:
            # A failed listing is recorded and returns no items. base() below
            # would then issue the token the listing could not get, outside
            # any record; a listing that succeeded left that token in place.
            return

        def detach(volume: dict) -> None:
            attachments = self.sweep_list("attachments", lambda: need(
                self.client.call(
                    "GET", f"{self.base()}/attachments?volume_id="
                    f"{volume['id']}", expect=(200,), headers=CINDER_ATTACH),
                "attachments"))
            for att in attachments:
                try:
                    self.measure("sweep-attachments", lambda a=att: (
                        self.client.call(
                            "DELETE", f"{self.base()}/attachments/{a['id']}",
                            expect=(200,), headers=CINDER_ATTACH), None))
                except CycleFailed:
                    pass

        self.sweep_each("volumes", items, f"{self.base()}/volumes/{{id}}",
                        (202,), deadline, before=detach)

    def checks(self) -> list[tuple[str, Callable[[], list[str]]]]:
        """Check for volumes in a transition and volumes left over."""
        def volumes() -> list[str]:
            items = self.listing()
            return (stuck(items, VOLUME_BUSY, "volume")
                    + prefixed(items, self.ctx.prefix, "volume"))
        return [("volumes", volumes)]


class ImageStream(Stream):
    """Create an image, upload 1 MiB of zero bytes, delete it."""

    name = "image"
    requires = ("glance", "keystone")
    uses_token = True

    def cycle(self, n: int) -> None:
        """Run create, upload, active, delete."""
        base = f"{self.ctx.args.glance}/v2/images"
        name = self.resource_name(n)
        image = self.measure("create", lambda: self.create(base, name))
        self.track("image", f"{base}/{image}", (204,))
        self.measure("upload", lambda: (self.client.call(
            "PUT", f"{base}/{image}/file", expect=(204,), data=IMAGE_PAYLOAD,
            headers={"Content-Type": "application/octet-stream"},
            parse=False), None))
        self.measure("active", lambda: (self.poll(
            f"{base}/{image}", ("status",), "active",
            lambda s: s == "killed", STATUS_WAIT_SECONDS), None))
        self.remove("delete", f"{base}/{image}")

    def create(self, base: str, name: str) -> tuple[Response, str]:
        """POST a raw/bare image record and return its id."""
        resp = self.client.call("POST", base, expect=(201,), body={
            "name": name, "disk_format": "raw", "container_format": "bare"})
        return resp, need(resp, "id")

    def listing(self) -> list[dict]:
        """List every image, following the next links Glance pages with."""
        items: list[dict] = []
        path = "/v2/images"
        while path:
            resp = self.client.call("GET", f"{self.ctx.args.glance}{path}",
                                    expect=(200,))
            items.extend(need(resp, "images"))
            path = resp.body.get("next") or ""
        return items

    def sweep(self) -> None:
        """Delete every leftover image."""
        deadline = time.monotonic() + SWEEP_SECONDS
        items = self.sweep_list("images", self.listing)
        self.sweep_each("images", items,
                        f"{self.ctx.args.glance}/v2/images/{{id}}", (204,),
                        deadline)

    def checks(self) -> list[tuple[str, Callable[[], list[str]]]]:
        """Check for images in saving or queued and images left over."""
        def images() -> list[str]:
            items = self.listing()
            return (stuck(items, IMAGE_BUSY, "image")
                    + prefixed(items, self.ctx.prefix, "image"))
        return [("images", images)]


class PlacementStream(Stream):
    """Create a resource provider, find it by name, delete it."""

    name = "placement"
    requires = ("placement", "keystone")
    uses_token = True

    def cycle(self, n: int) -> None:
        """Run create-rp, list-rp, delete-rp."""
        base = f"{self.ctx.args.placement}/resource_providers"
        name = self.resource_name(n)
        rp = self.measure("create-rp", lambda: self.create(base, name))
        self.track("resource-provider", f"{base}/{rp}", (204,))
        self.measure("list-rp", lambda: self.find(base, name, rp))
        self.remove("delete-rp", f"{base}/{rp}")

    def create(self, base: str, name: str) -> tuple[Response, str]:
        """POST one resource provider and return its uuid.

        The stream sends no microversion, and below 1.20 Placement answers 201
        with an empty body and the provider in the Location header.
        """
        resp = self.client.call("POST", base, expect=(201,),
                                body={"name": name}, parse=False)
        location = resp.headers.get("Location")
        if not location:
            raise OpFailed(f"POST {base} answered without a Location",
                           resp.status)
        return resp, last_segment(location)

    def find(self, base: str, name: str, rp: str) -> tuple[Response, None]:
        """List the providers named name: exactly the one created as rp."""
        resp = self.client.call("GET", f"{base}?name={name}", expect=(200,))
        found = [p.get("uuid") for p in need(resp, "resource_providers")]
        if found != [rp]:
            raise OpFailed(f"GET {base}?name={name} listed {found}",
                           resp.status)
        return resp, None

    def listing(self) -> list[dict]:
        """List every resource provider by its uuid and name."""
        resp = self.client.call(
            "GET", f"{self.ctx.args.placement}/resource_providers",
            expect=(200,))
        return [{"id": p.get("uuid"), "name": p.get("name")}
                for p in need(resp, "resource_providers")]

    def sweep(self) -> None:
        """Delete every leftover resource provider."""
        deadline = time.monotonic() + SWEEP_SECONDS
        items = self.sweep_list("resource-providers", self.listing)
        self.sweep_each(
            "resource-providers", items,
            f"{self.ctx.args.placement}/resource_providers/{{id}}", (204,),
            deadline)

    def checks(self) -> list[tuple[str, Callable[[], list[str]]]]:
        """Check that no resource provider carries the prefix."""
        def providers() -> list[str]:
            return prefixed(self.listing(), self.ctx.prefix,
                            "resource provider")
        return [("resource-providers", providers)]


class SecretStream(Stream):
    """Store a passphrase, read its payload back, delete it."""

    name = "secret"
    requires = ("barbican", "keystone")
    uses_token = True

    def cycle(self, n: int) -> None:
        """Run create-secret, read-payload, delete-secret."""
        base = f"{self.ctx.args.barbican}/v1/secrets"
        name = self.resource_name(n)
        url = self.measure("create-secret", lambda: self.create(base, name))
        self.track("secret", url, (204,))
        self.measure("read-payload", lambda: self.read(url, name))
        self.remove("delete-secret", url)

    def create(self, base: str, name: str) -> tuple[Response, str]:
        """POST a passphrase whose payload is its name and return its URL.

        Barbican writes secret_ref under its host_href, so the URL keeps the
        uuid of the reference and the root the stream was given.
        """
        resp = self.client.call("POST", base, expect=(201,), body={
            "name": name, "payload": name,
            "payload_content_type": "text/plain",
            "secret_type": "passphrase"})
        return resp, f"{base}/{last_segment(need(resp, 'secret_ref'))}"

    def read(self, url: str, name: str) -> tuple[Response, None]:
        """GET the payload and fail unless it is the name, byte for byte."""
        resp = self.client.call("GET", f"{url}/payload", expect=(200,),
                                headers={"Accept": "text/plain"}, parse=False)
        if resp.body != name.encode():
            raise OpFailed(f"GET {url}/payload answered another payload",
                           resp.status)
        return resp, None

    def listing(self) -> list[dict]:
        """List every secret by its uuid and name, page by page."""
        items: list[dict] = []
        while True:
            resp = self.client.call(
                "GET", f"{self.ctx.args.barbican}/v1/secrets?limit=100"
                f"&offset={len(items)}", expect=(200,))
            page = need(resp, "secrets")
            items.extend({"id": last_segment(s.get("secret_ref")),
                          "name": s.get("name")} for s in page)
            if not page or len(items) >= need(resp, "total"):
                return items

    def sweep(self) -> None:
        """Delete every leftover secret."""
        deadline = time.monotonic() + SWEEP_SECONDS
        items = self.sweep_list("secrets", self.listing)
        self.sweep_each("secrets", items,
                        f"{self.ctx.args.barbican}/v1/secrets/{{id}}", (204,),
                        deadline)

    def checks(self) -> list[tuple[str, Callable[[], list[str]]]]:
        """Check that no secret carries the prefix."""
        def secrets() -> list[str]:
            return prefixed(self.listing(), self.ctx.prefix, "secret")
        return [("secrets", secrets)]


def last_segment(ref: Any) -> str:
    """Return the last path segment of a reference URL, the uuid it names."""
    return str(ref or "").rstrip("/").rsplit("/", 1)[-1]


STREAM_CLASSES = {cls.name: cls for cls in (
    TokenStream, HttpStream, NetworkStream, ServerStream, VolumeStream,
    ImageStream, PlacementStream, SecretStream)}
STREAMS = tuple(STREAM_CLASSES)


class Context:
    """What every stream of one run shares."""

    def __init__(self, args: argparse.Namespace, password: str) -> None:
        """Hold the options, the admin password, the recorder and the stop."""
        self.args = args
        self.password = password
        self.prefix = f"chaos-wl-{args.id}-"
        self.rec = Recorder()
        self.stop = threading.Event()

    def client(self, stream: Stream) -> Client:
        """Build the client of a stream: own token, noauth or none."""
        if stream.uses_token and self.args.keystone:
            return Client(TokenSource(self.args.keystone, self.password))
        return Client(static_token=stream.noauth_token)


def usage(reason: str) -> None:
    """Print the reason a run cannot start and exit 2."""
    print(f"usage: {reason}", file=sys.stderr)
    sys.exit(2)


def parse_streams(args: argparse.Namespace) -> list[str]:
    """Validate --streams and the options each stream needs."""
    names = [s.strip() for s in args.streams.split(",") if s.strip()]
    if not names:
        usage("--streams is empty; name a subset of " + ",".join(STREAMS))
    for name in names:
        if name not in STREAM_CLASSES:
            usage(f"unknown stream {name!r}; known streams are "
                  + ",".join(STREAMS))
        for option in STREAM_CLASSES[name].requires:
            if not getattr(args, option):
                usage(f"stream {name} needs --{option.replace('_', '-')}")
    if len(set(names)) != len(names):
        usage(f"--streams names a stream twice: {args.streams}")
    return names


def parse_resolve(values: list[str]) -> dict[str, str]:
    """Map the HOST of every --resolve HOST:ADDR to its ADDR.

    The value splits on its first colon, so ADDR may be an IPv6 address. An
    empty HOST and an ADDR that is no IP address exit 2.
    """
    mapping = {}
    for value in values:
        host, _, addr = value.partition(":")
        try:
            ipaddress.ip_address(addr)
        except ValueError:
            usage("--resolve wants HOST:ADDR")
        if not host:
            usage("--resolve wants HOST:ADDR")
        mapping[host] = addr
    return mapping


def install_resolver(mapping: dict[str, str]) -> None:
    """Resolve each mapped host to its address; other names resolve as before.

    urllib still sends the host of the URL in the Host header and as the TLS
    server name, so a Gateway listener that matches that hostname routes the
    request.
    """
    if not mapping:
        return
    original = socket.getaddrinfo

    def getaddrinfo(host: Any, *args: Any, **kwargs: Any) -> Any:
        return original(mapping.get(host, host), *args, **kwargs)

    socket.getaddrinfo = getaddrinfo


def unverified_tls() -> ssl.SSLContext:
    """Return a TLS context that checks neither the certificate nor its name."""
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx


def settle_seconds() -> float:
    """Read CHAOS_WORKLOAD_SETTLE_SECONDS, the re-read window of a check."""
    raw = os.environ.get("CHAOS_WORKLOAD_SETTLE_SECONDS", "")
    if not raw:
        return SETTLE_SECONDS
    try:
        value = float(raw)
    except ValueError:
        value = -1
    if value < 0:
        usage(f"CHAOS_WORKLOAD_SETTLE_SECONDS={raw} is not a number of "
              "seconds")
    return value


def consistency(rec: Recorder, name: str, read: Callable[[], list[str]],
                settle: float) -> None:
    """Re-read one check until it is clean or the settle window is over."""
    deadline = time.monotonic() + settle
    while True:
        try:
            problems = read()
        except REQUEST_ERRORS as exc:
            problems = [failure(exc)[1]]
        if not problems:
            rec.line(f"CONSISTENCY-OK {name}")
            return
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            more = len(problems) - DETAIL_LIMIT
            detail = "; ".join(problems[:DETAIL_LIMIT])
            if more > 0:
                detail += f"; and {more} more"
            rec.line(f"CONSISTENCY-FAIL {name}: {detail}")
            return
        time.sleep(min(SETTLE_INTERVAL, remaining))


def run(args: argparse.Namespace) -> int:
    """Run the streams until the stop file exists, then sweep and check."""
    names = parse_streams(args)
    resolve = parse_resolve(args.resolve)
    password = ""
    if args.keystone:
        password = os.environ.get("OS_PASSWORD", "")
        if not password:
            usage("--keystone needs the admin password in OS_PASSWORD")
    settle = settle_seconds()
    # The service roots get their API paths appended; the http URL is used
    # as it is.
    for option in ("keystone", "neutron", "nova", "cinder", "glance",
                   "placement", "barbican"):
        if getattr(args, option):
            setattr(args, option, getattr(args, option).rstrip("/"))
    install_resolver(resolve)
    if args.insecure_tls:
        Client.tls = unverified_tls()

    ctx = Context(args, password)
    streams = [STREAM_CLASSES[name](ctx) for name in names]
    crashes: list[Any] = []

    def guard(stream: Stream) -> None:
        # Anything but a request failure is a defect of the workload: it ends
        # the run with the traceback instead of a stream that quietly stops.
        try:
            stream.loop()
        except Exception:
            crashes.append(sys.exc_info())
            ctx.stop.set()

    threads = [threading.Thread(target=guard, args=(s,), name=s.name,
                                daemon=True) for s in streams]
    for thread in threads:
        thread.start()
    while not ctx.stop.is_set() and not os.path.exists(args.stop_file):
        time.sleep(STOP_POLL)
    ctx.stop.set()
    for thread in threads:
        thread.join()
    if crashes:
        traceback.print_exception(*crashes[0])
        return 1

    for stream in streams:
        stream.sweeping = True
        stream.sweep()
    for stream in streams:
        for name, read in stream.checks():
            consistency(ctx.rec, name, read, settle)
    ctx.rec.line("DONE")
    return 0


def input_error(reason: str) -> None:
    """Print the reason evaluate cannot judge its input and exit 2."""
    print(f"evaluate: {reason}", file=sys.stderr)
    sys.exit(2)


def read_log(path: str) -> tuple[list[dict], bool, list[str]]:
    """Return the records, whether DONE was printed and every CONSISTENCY-FAIL.

    Lines starting with { are records; the rest are read only for READY, DONE
    and CONSISTENCY-FAIL. A failed record that comes before the READY line of
    its stream is marked warm-up: start waits for READY, so such a failure
    happened while the service was still coming up.
    """
    try:
        with open(path, encoding="utf-8", errors="replace") as handle:
            text = handle.read()
    except OSError as exc:
        input_error(f"cannot read log {path}: {exc}")
    if not text.strip():
        input_error(f"log {path} is empty")
    lines = text.splitlines()
    ready_at = {}
    for lineno, line in enumerate(lines, 1):
        if line.startswith("READY "):
            ready_at.setdefault(line[len("READY "):].strip(), lineno)
    records = []
    done = False
    consistency_fails = []
    for lineno, line in enumerate(lines, 1):
        if line.startswith("{"):
            try:
                record = json.loads(line)
            except ValueError as exc:
                input_error(f"line {lineno} of {path} starts with {{ but is "
                            f"not JSON: {exc}")
            if not (isinstance(record, dict)
                    and isinstance(record.get("s"), str)
                    and isinstance(record.get("op"), str)
                    and isinstance(record.get("ok"), bool)
                    and isinstance(record.get("t"), (int, float))
                    and isinstance(record.get("ms"), (int, float))):
                input_error(f"line {lineno} of {path} is not a workload "
                            "record")
            record["warmup"] = (not record["ok"]
                                and lineno < ready_at.get(record["s"], 0))
            records.append(record)
        elif line.strip() == "DONE":
            done = True
        elif line.startswith("CONSISTENCY-FAIL"):
            consistency_fails.append(line)
    return records, done, consistency_fails


def evaluate(args: argparse.Namespace) -> int:
    """Judge a workload log against the budget and print the summary."""
    marks = (args.fault_start, args.fault_end)
    if (marks[0] is None) != (marks[1] is None):
        input_error("--fault-start and --fault-end go together; only one "
                    "was given")
    if marks[0] is not None and marks[1] < marks[0]:
        input_error(f"fault_end {marks[1]} is before fault_start {marks[0]}")
    records, done, consistency_fails = read_log(args.log)

    budget = args.budget_failures
    deadline = None
    if marks[0] is not None:
        deadline = marks[1] + args.recovery

    def in_window(record: dict) -> bool:
        # A sweep runs after the stop file, so its failures are never part of
        # what the fault may cost. An operation still in flight when the fault
        # starts can fail because of it, so the lower edge reads the end of the
        # operation and the upper edge its start.
        end = record["t"] + record["ms"] / 1000
        return (deadline is not None and not record["op"].startswith("sweep-")
                and marks[0] <= end and record["t"] <= deadline)

    violations = []
    offending: list[dict] = []
    window_failures: list[dict] = []
    streams = list(dict.fromkeys(r["s"] for r in records))
    for stream in streams:
        warmup = [r for r in records if r["s"] == stream and r["warmup"]]
        own = [r for r in records if r["s"] == stream and not r["warmup"]]
        ok = sum(1 for r in own if r["ok"])
        failed_in = [r for r in own if not r["ok"] and in_window(r)]
        failed_out = [r for r in own if not r["ok"] and not in_window(r)]
        window_failures.extend(failed_in)
        offset = 0.0
        if failed_in:
            offset = max(0.0, max(r["t"] for r in failed_in) - marks[1])
        print(f"stream {stream}: {ok} ok, {len(failed_in)} failed in window "
              f"(budget {budget}), {len(failed_out)} outside; observed "
              f"failures={len(failed_in)} recovery={offset:.1f}s")
        if warmup:
            print(f"stream {stream}: {len(warmup)} failed record(s) before "
                  "READY, not judged")
        if failed_out:
            violations.append(f"stream {stream}: {len(failed_out)} failed "
                              "record(s) outside the fault window")
            offending.extend(failed_out)
        recovered = [r for r in own if r["ok"] and not r["op"].startswith(
            "sweep-") and (deadline is None or r["t"] >= deadline)]
        if not recovered:
            violations.append(
                f"stream {stream}: no ok record after the recovery deadline "
                f"{deadline}" if deadline is not None
                else f"stream {stream}: no ok record")
    if not records:
        violations.append("the log holds no records")
    if len(window_failures) > budget:
        violations.append(f"{len(window_failures)} failed records in the "
                          f"fault window, budget {budget}")
        offending.extend(window_failures)
    for line in consistency_fails:
        violations.append(line)
    if not done:
        violations.append("the workload printed no DONE")

    for violation in violations:
        print(f"FAIL: {violation}")
    for record in offending[:10]:
        shown = {k: v for k, v in record.items() if k != "warmup"}
        print(f"offending: {json.dumps(shown)}")
    if violations:
        print(f"evaluate: {len(violations)} violation(s)")
        return 1
    print("evaluate: the run kept to its budget")
    return 0


def main(argv: list[str] | None = None) -> int:
    """Dispatch run and evaluate."""
    parser = argparse.ArgumentParser(
        prog="workload.py",
        description="Workload and consistency harness of the chaos suites.")
    sub = parser.add_subparsers(dest="command", required=True)

    run_parser = sub.add_parser("run", help="run the workload streams")
    run_parser.add_argument("--id", required=True)
    run_parser.add_argument("--stop-file", required=True)
    run_parser.add_argument("--streams", required=True,
                            help="comma-separated subset of "
                            + ",".join(STREAMS))
    run_parser.add_argument("--keystone", help="the Keystone /v3 root")
    run_parser.add_argument("--http", help="the URL the http stream GETs")
    run_parser.add_argument("--neutron")
    run_parser.add_argument("--nova", help="the compute root with /v2.1")
    run_parser.add_argument("--image-ref")
    run_parser.add_argument("--flavor")
    run_parser.add_argument("--cinder")
    run_parser.add_argument("--glance")
    run_parser.add_argument("--placement",
                            help="the Placement API root, no version path")
    run_parser.add_argument("--barbican",
                            help="the Barbican API root, no version path")
    run_parser.add_argument("--resolve", action="append", default=[],
                            metavar="HOST:ADDR",
                            help="connect to ADDR for every URL that names "
                            "HOST; repeatable")
    run_parser.add_argument("--insecure-tls", action="store_true",
                            help="skip the certificate check of HTTPS "
                            "endpoints")

    eval_parser = sub.add_parser("evaluate", help="judge a workload log")
    eval_parser.add_argument("--log", required=True)
    eval_parser.add_argument("--budget-failures", type=int, required=True)
    eval_parser.add_argument("--recovery", type=float, required=True)
    eval_parser.add_argument("--fault-start", type=float)
    eval_parser.add_argument("--fault-end", type=float)

    args = parser.parse_args(argv)
    if args.command == "run":
        return run(args)
    return evaluate(args)


if __name__ == "__main__":
    sys.exit(main())
