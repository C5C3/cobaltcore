#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for hack/tempest/keystone-inventory.py.

The helper is loaded by path and its main() runs in-process with stdout and
stderr captured. A fake Keystone served by http.server.ThreadingHTTPServer on
127.0.0.1 answers each test from a route table and records every request, so
the tests need nothing beyond the standard library.
"""
from __future__ import annotations

import contextlib
import http.server
import importlib.util
import io
import json
import os
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

_PROJECT_ROOT = Path(__file__).resolve().parent.parent.parent
_HELPER = _PROJECT_ROOT / "hack" / "tempest" / "keystone-inventory.py"


def _load(mod_name: str, path: Path):
    spec = importlib.util.spec_from_file_location(mod_name, path)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


inventory = _load("keystone_inventory", _HELPER)

TOKEN = "gAAAAA-system-token"
DOMAIN_IDS = ("default", "d2")

# The collections of the issue's table: kind, path below /v3, list key and
# the label field (None: the label is the empty string).
COLLECTIONS = (
    ("service", "/services", "services", "type"),
    ("endpoint", "/endpoints", "endpoints", "url"),
    ("region", "/regions", "regions", "description"),
    ("policy", "/policies", "policies", "type"),
    ("consumer", "/OS-OAUTH1/consumers", "consumers", "description"),
    ("domain", "/domains", "domains", "name"),
    ("project", "/projects", "projects", "name"),
    ("user", "/users", "users", "name"),
    ("group", "/groups", "groups", "name"),
    ("role", "/roles", "roles", "name"),
    ("credential", "/credentials", "credentials", "type"),
    ("identity_provider", "/OS-FEDERATION/identity_providers",
     "identity_providers", "domain_id"),
    ("mapping", "/OS-FEDERATION/mappings", "mappings", None),
    ("service_provider", "/OS-FEDERATION/service_providers",
     "service_providers", None),
    ("endpoint_group", "/OS-EP-FILTER/endpoint_groups", "endpoint_groups",
     "name"),
    ("registered_limit", "/registered_limits", "registered_limits",
     "resource_name"),
    ("limit", "/limits", "limits", "resource_name"),
    ("trust", "/OS-TRUST/trusts", "trusts", None),
)


def _object(kind: str, n: int, label: str | None) -> dict:
    """Return one object of kind with an id, its label and an extra field."""
    obj = {"id": f"{kind}-{n}", "links": {"self": "http://example/x"}}
    if label is not None:
        obj[label] = f"{kind} label {n}"
    return obj


def _routes() -> dict:
    """Return the route table of a Keystone with two objects per kind."""
    routes = {("POST", "/v3/auth/tokens"): (201, {"token": {}}, 0)}
    for kind, path, key, label in COLLECTIONS:
        objects = [_object(kind, n, label) for n in (1, 2)]
        if kind == "domain":
            for obj, domain_id in zip(objects, DOMAIN_IDS):
                obj["id"] = domain_id
        routes[("GET", "/v3" + path)] = (200, {key: objects}, 0)
    for domain_id in DOMAIN_IDS:
        routes[("GET", f"/v3/roles?domain_id={domain_id}")] = (200, {
            "roles": [{"id": f"role-of-{domain_id}", "name": "dr",
                       "domain_id": domain_id}]}, 0)
    return routes


class _FakeKeystone(http.server.BaseHTTPRequestHandler):
    """Answer from the server's route table and record every request."""

    def do_GET(self) -> None:
        self._answer()

    def do_POST(self) -> None:
        self._answer()

    def _answer(self) -> None:
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        self.server.requests.append(
            (self.command, self.path, dict(self.headers), body))
        status, payload, delay = self.server.routes.get(
            (self.command, self.path), (404, {"error": "no route"}, 0))
        if delay:
            time.sleep(delay)
        data = payload if isinstance(payload, bytes) else json.dumps(
            payload).encode()
        try:
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            if self.path == "/v3/auth/tokens" and status == 201:
                self.send_header("X-Subject-Token", TOKEN)
            self.end_headers()
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            # The timeout test's client gave up before the answer.
            pass

    def log_message(self, *args) -> None:
        pass


class _Server(http.server.ThreadingHTTPServer):
    daemon_threads = True


class _InventoryCase(unittest.TestCase):
    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.tmp = tmp.name
        self.server = _Server(("127.0.0.1", 0), _FakeKeystone)
        self.server.routes = _routes()
        self.server.requests = []
        thread = threading.Thread(target=self.server.serve_forever,
                                  kwargs={"poll_interval": 0.05},
                                  daemon=True)
        thread.start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.uri_v3 = f"http://127.0.0.1:{self.server.server_port}/v3"
        self.out = os.path.join(self.tmp, "snapshot.json")

    def _conf(self, uri_v3: str | None = None, password: str = "secret",
              omit: tuple[str, str] | None = None) -> str:
        """Write a tempest.conf and return its path."""
        options = (
            ("identity", "uri_v3", uri_v3 or self.uri_v3),
            ("auth", "admin_username", "admin"),
            ("auth", "admin_password", password),
            ("auth", "admin_domain_name", "Default"),
        )
        lines = ["[DEFAULT]", "log_dir = /tmp/tempest-logs", ""]
        for section in ("identity", "auth"):
            lines.append(f"[{section}]")
            lines.extend(f"{option} = {value}"
                         for sec, option, value in options
                         if sec == section and (sec, option) != omit)
            lines.append("")
        path = os.path.join(self.tmp, "tempest.conf")
        Path(path).write_text("\n".join(lines), encoding="utf-8")
        return path

    def _run(self, *argv: str) -> tuple[int, str, str]:
        stdout, stderr = io.StringIO(), io.StringIO()
        with (contextlib.redirect_stdout(stdout),
              contextlib.redirect_stderr(stderr)):
            rc = inventory.main(list(argv))
        return rc, stdout.getvalue(), stderr.getvalue()

    def _snapshot(self, conf: str | None = None) -> tuple[int, str, str]:
        return self._run("snapshot", "--conf", conf or self._conf(),
                         "--out", self.out)

    def _read_snapshot(self) -> dict:
        return json.loads(Path(self.out).read_text(encoding="utf-8"))

    def assertOneErrorLine(self, err: str, line: str) -> None:
        self.assertEqual(err, f"keystone-inventory: {line}\n")


class SnapshotTests(_InventoryCase):
    def test_every_kind_is_written_with_its_label(self) -> None:
        rc, out, err = self._snapshot()

        self.assertEqual((rc, out, err), (0, "", ""))
        text = Path(self.out).read_text(encoding="utf-8")
        snapshot = json.loads(text)
        expected = {}
        for kind, _, _, label in COLLECTIONS:
            ids = DOMAIN_IDS if kind == "domain" else (
                f"{kind}-1", f"{kind}-2")
            expected[kind] = {
                obj_id: "" if label is None else f"{kind} label {n}"
                for n, obj_id in zip((1, 2), ids)}
        expected["domain_role"] = {
            "role-of-default": "dr", "role-of-d2": "dr"}
        self.assertEqual(snapshot, expected)
        self.assertEqual(len(snapshot), 19)
        self.assertEqual(list(snapshot), sorted(snapshot))

        requests = self.server.requests
        self.assertEqual(requests[0][:2], ("POST", "/v3/auth/tokens"))
        auth = json.loads(requests[0][3])["auth"]
        self.assertEqual(auth["scope"], {"system": {"all": True}})
        self.assertEqual(auth["identity"]["password"]["user"], {
            "name": "admin", "domain": {"name": "Default"},
            "password": "secret"})
        domain_role_paths = sorted(
            path for _, path, _, _ in requests
            if path.startswith("/v3/roles?"))
        self.assertEqual(domain_role_paths, [
            "/v3/roles?domain_id=d2", "/v3/roles?domain_id=default"])
        gets = [r for r in requests if r[0] == "GET"]
        self.assertEqual(len(gets), 18 + len(DOMAIN_IDS))
        self.assertTrue(all(h.get("X-Auth-Token") == TOKEN
                            for _, _, h, _ in gets))

    def test_empty_collection_is_an_empty_object(self) -> None:
        self.server.routes[("GET", "/v3/policies")] = (
            200, {"policies": []}, 0)

        rc, _, err = self._snapshot()

        self.assertEqual((rc, err), (0, ""))
        self.assertEqual(self._read_snapshot()["policy"], {})

    def test_absent_or_null_label_is_empty(self) -> None:
        self.server.routes[("GET", "/v3/services")] = (200, {"services": [
            {"id": "no-type"}, {"id": "null-type", "type": None}]}, 0)

        rc, _, _ = self._snapshot()

        self.assertEqual(rc, 0)
        self.assertEqual(self._read_snapshot()["service"],
                         {"no-type": "", "null-type": ""})

    def test_blob_value_never_reaches_the_snapshot(self) -> None:
        self.server.routes[("GET", "/v3/credentials")] = (200, {
            "credentials": [{"id": "c1", "type": "ec2", "blob":
                             '{"access": "AKIA-1840", "secret": "S3CR3T"}'}],
        }, 0)

        rc, _, _ = self._snapshot()

        self.assertEqual(rc, 0)
        text = Path(self.out).read_text(encoding="utf-8")
        self.assertEqual(json.loads(text)["credential"], {"c1": "ec2"})
        for secret in ("blob", "AKIA-1840", "S3CR3T"):
            self.assertNotIn(secret, text)

    def test_proxy_variables_are_ignored(self) -> None:
        # Tempest's own client ignores them too. A proxy would receive the
        # admin password, and it cannot reach the runner's localhost. The
        # helper runs as a process of its own: urlopen reads the variables
        # once per process.
        proxy = "http://127.0.0.1:1"
        env = dict(os.environ, http_proxy=proxy, HTTP_PROXY=proxy)
        env.pop("no_proxy", None)
        env.pop("NO_PROXY", None)

        proc = subprocess.run(
            [sys.executable, str(_HELPER), "snapshot", "--conf",
             self._conf(), "--out", self.out],
            env=env, capture_output=True, text=True, timeout=60,
            check=False)

        self.assertEqual((proc.returncode, proc.stderr), (0, ""))
        self.assertEqual(self.server.requests[0][:2],
                         ("POST", "/v3/auth/tokens"))
        self.assertIn("service", self._read_snapshot())

    def test_percent_in_password_reaches_the_token_request(self) -> None:
        rc, _, _ = self._snapshot(self._conf(password="p%w%%d%(x)s"))

        self.assertEqual(rc, 0)
        auth = json.loads(self.server.requests[0][3])["auth"]
        self.assertEqual(auth["identity"]["password"]["user"]["password"],
                         "p%w%%d%(x)s")

    def test_missing_conf_option_exits_2(self) -> None:
        for section, option in (("auth", "admin_password"),
                                ("auth", "admin_username"),
                                ("auth", "admin_domain_name"),
                                ("identity", "uri_v3")):
            with self.subTest(option=option):
                self.server.requests.clear()

                rc, out, err = self._snapshot(
                    self._conf(omit=(section, option)))

                self.assertEqual((rc, out), (2, ""))
                self.assertOneErrorLine(
                    err, f"tempest.conf has no [{section}] {option}")
                self.assertFalse(os.path.exists(self.out))
                self.assertEqual(self.server.requests, [])

    def test_unreadable_conf_exits_2(self) -> None:
        missing = os.path.join(self.tmp, "absent.conf")

        rc, _, err = self._snapshot(missing)

        self.assertEqual(rc, 2)
        self.assertOneErrorLine(
            err, f"cannot read {missing}: No such file or directory")

    def test_token_401_exits_2(self) -> None:
        self.server.routes[("POST", "/v3/auth/tokens")] = (
            401, {"error": {"code": 401}}, 0)

        rc, _, err = self._snapshot()

        self.assertEqual(rc, 2)
        self.assertOneErrorLine(err, "POST /auth/tokens returned 401")
        self.assertFalse(os.path.exists(self.out))

    def test_token_without_subject_token_exits_2(self) -> None:
        self.server.routes[("POST", "/v3/auth/tokens")] = (
            200, {"token": {}}, 0)

        rc, _, err = self._snapshot()

        self.assertEqual(rc, 2)
        self.assertOneErrorLine(
            err, "POST /auth/tokens returned no X-Subject-Token")

    def test_list_error_status_exits_2(self) -> None:
        for status in (403, 500):
            with self.subTest(status=status):
                self.server.routes[("GET", "/v3/services")] = (
                    status, {"error": {"code": status}}, 0)

                rc, _, err = self._snapshot()

                self.assertEqual(rc, 2)
                self.assertOneErrorLine(
                    err, f"GET /services returned {status}")
                self.assertFalse(os.path.exists(self.out))

    def test_answer_without_its_list_exits_2(self) -> None:
        self.server.routes[("GET", "/v3/groups")] = (
            200, {"group": []}, 0)

        rc, _, err = self._snapshot()

        self.assertEqual(rc, 2)
        self.assertOneErrorLine(
            err, "GET /groups answered without a 'groups' list")

    def test_refused_connection_exits_2(self) -> None:
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        uri_v3 = f"http://127.0.0.1:{port}/v3"

        rc, _, err = self._snapshot(self._conf(uri_v3=uri_v3))

        self.assertEqual(rc, 2)
        self.assertTrue(
            err.startswith(f"keystone-inventory: cannot reach {uri_v3}: "),
            err)
        self.assertIn("Connection refused", err)
        self.assertEqual(err.count("\n"), 1)

    def test_timeout_exits_2(self) -> None:
        status, payload, _ = self.server.routes[("GET", "/v3/services")]
        self.server.routes[("GET", "/v3/services")] = (status, payload, 1.5)

        with mock.patch.object(inventory, "TIMEOUT_SECONDS", 0.3):
            rc, _, err = self._snapshot()

        self.assertEqual(rc, 2)
        self.assertOneErrorLine(err, "GET /services timed out")
        self.assertFalse(os.path.exists(self.out))

    def test_truncated_list_exits_2_and_writes_nothing(self) -> None:
        status, payload, _ = self.server.routes[("GET", "/v3/users")]
        self.server.routes[("GET", "/v3/users")] = (
            status, dict(payload, truncated=True), 0)

        rc, _, err = self._snapshot()

        self.assertEqual(rc, 2)
        self.assertOneErrorLine(
            err, "GET /users was truncated by Keystone's list_limit")
        self.assertFalse(os.path.exists(self.out))

    def test_list_at_max_db_limit_exits_2_and_writes_nothing(self) -> None:
        # Keystone cuts a list of users at max_db_limit without "truncated".
        self.server.routes[("GET", "/v3/users")] = (200, {"users": [
            {"id": f"user-{n}", "name": "u"} for n in range(1000)]}, 0)

        rc, _, err = self._snapshot()

        self.assertEqual(rc, 2)
        self.assertOneErrorLine(
            err, "GET /users returned 1000 objects; Keystone's max_db_limit "
                 "of 1000 may have cut the list")
        self.assertFalse(os.path.exists(self.out))

    def test_list_below_max_db_limit_is_written(self) -> None:
        self.server.routes[("GET", "/v3/users")] = (200, {"users": [
            {"id": f"user-{n}", "name": "u"} for n in range(999)]}, 0)

        rc, _, err = self._snapshot()

        self.assertEqual((rc, err), (0, ""))
        self.assertEqual(len(self._read_snapshot()["user"]), 999)


class DiffTests(unittest.TestCase):
    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.tmp = tmp.name
        self.out = os.path.join(self.tmp, "keystone-leftovers.tsv")

    def _file(self, name: str, content: object) -> str:
        path = os.path.join(self.tmp, name)
        text = content if isinstance(content, str) else json.dumps(content)
        Path(path).write_text(text, encoding="utf-8")
        return path

    def _diff(self, before: str, after: str) -> tuple[int, str, str]:
        stdout, stderr = io.StringIO(), io.StringIO()
        with (contextlib.redirect_stdout(stdout),
              contextlib.redirect_stderr(stderr)):
            rc = inventory.main(["diff", "--before", before, "--after",
                                 after, "--out", self.out])
        return rc, stdout.getvalue(), stderr.getvalue()

    def test_equal_snapshots_exit_0_and_write_an_empty_file(self) -> None:
        snap = {"role": {"r1": "admin"}, "trust": {}}
        before = self._file("before.json", snap)
        after = self._file("after.json", snap)

        rc, out, err = self._diff(before, after)

        self.assertEqual((rc, out, err), (0, "", ""))
        self.assertEqual(Path(self.out).read_text(encoding="utf-8"), "")

    def test_added_ids_are_reported_sorted(self) -> None:
        before = self._file("before.json", {
            "role": {"r1": "admin"}, "service": {"gone": "identity"}})
        after = self._file("after.json", {
            "role": {"r3": "c", "r1": "admin", "r2": "b"},
            "service": {},
            "domain": {"d9": "tempest-domain"}})

        rc, out, err = self._diff(before, after)

        expected = ("domain\td9\ttempest-domain\n"
                    "role\tr2\tb\n"
                    "role\tr3\tc\n")
        self.assertEqual((rc, err), (1, ""))
        self.assertEqual(out, expected)
        self.assertEqual(Path(self.out).read_text(encoding="utf-8"),
                         expected)
        self.assertNotIn("gone", out)

    def test_label_with_tab_or_newline_stays_one_line(self) -> None:
        before = self._file("before.json", {"region": {}})
        after = self._file("after.json", {
            "region": {"RegionTwo": "a\tdescription\r\nover lines"}})

        rc, out, _ = self._diff(before, after)

        self.assertEqual(rc, 1)
        self.assertEqual(out, "region\tRegionTwo\ta description  over lines\n")

    def test_missing_file_exits_2(self) -> None:
        after = self._file("after.json", {})
        missing = os.path.join(self.tmp, "before.json")

        rc, out, err = self._diff(missing, after)

        self.assertEqual((rc, out), (2, ""))
        self.assertEqual(err, f"keystone-inventory: cannot read {missing}: "
                              "No such file or directory\n")

    def test_not_json_exits_2(self) -> None:
        before = self._file("before.json", {})
        after = self._file("after.json", "Traceback (most recent call")

        rc, _, err = self._diff(before, after)

        self.assertEqual(rc, 2)
        self.assertTrue(err.startswith(
            f"keystone-inventory: cannot read {after}: Expecting value"), err)
        self.assertEqual(err.count("\n"), 1)

    def test_wrong_shape_exits_2(self) -> None:
        before = self._file("before.json", {})
        for content in ([], {"role": ["r1"]}, {"role": {"r1": None}}):
            with self.subTest(content=content):
                after = self._file("after.json", content)

                rc, _, err = self._diff(before, after)

                self.assertEqual(rc, 2)
                self.assertEqual(
                    err, f"keystone-inventory: cannot read {after}: "
                         "not a keystone-inventory snapshot\n")


class CliTests(unittest.TestCase):
    def test_usage_error_is_one_line_exit_2(self) -> None:
        for argv in ([], ["snapshot", "--conf", "tempest.conf"], ["sweep"]):
            with self.subTest(argv=argv):
                stdout, stderr = io.StringIO(), io.StringIO()
                with (contextlib.redirect_stdout(stdout),
                      contextlib.redirect_stderr(stderr)):
                    rc = inventory.main(argv)

                self.assertEqual((rc, stdout.getvalue()), (2, ""))
                err = stderr.getvalue()
                self.assertTrue(err.startswith("keystone-inventory: "), err)
                self.assertEqual(err.count("\n"), 1)


if __name__ == "__main__":
    unittest.main()
