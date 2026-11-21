#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""List Keystone's objects, and report the ones a Tempest run added.

hack/tempest/run-tests.sh runs `snapshot` before and after the run of a
keystone leg and `diff` on the two files when TEMPEST_KEYSTONE_LEAK_CHECK is
`fail`. `snapshot` reads the admin credentials from tempest.conf, takes a
system-scoped token and writes the id and one label of every object of 19
kinds as `{"<kind>": {"<id>": "<label>"}}`. `diff` prints one
`<kind>\\t<id>\\t<label>` line for every id the after snapshot has and the
before snapshot lacks.

Exit codes: 0 for a written snapshot or an empty diff, 1 for a diff with at
least one line, 2 for every error, with one line on stderr that starts with
`keystone-inventory: `. Only the standard library is used, so the helper runs
in the Tempest image and on a bare host.
"""
from __future__ import annotations

import argparse
import configparser
import http.client
import json
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import NoReturn

TIMEOUT_SECONDS = 60

# No proxy, also when http_proxy or HTTP_PROXY is set: Tempest's own client
# ignores those variables, and a proxy would receive the admin password.
_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))

# The default of Keystone's [DEFAULT] max_db_limit. Keystone cuts a list of
# users, groups, projects or domains at it and does not set "truncated", so a
# list this long may be incomplete.
MAX_DB_LIMIT = 1000

# (kind, path below uri_v3, list key, label field). A kind without a label
# field gets the empty string. The domain roles are listed per domain, see
# snapshot().
KINDS = (
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

_CONF_OPTIONS = (
    ("identity", "uri_v3"),
    ("auth", "admin_username"),
    ("auth", "admin_password"),
    ("auth", "admin_domain_name"),
)

Snapshot = dict[str, dict[str, str]]


class InventoryError(Exception):
    """A failure the helper reports as one stderr line and exit code 2."""


class _Parser(argparse.ArgumentParser):
    """An argument parser whose usage errors follow the exit-2 contract."""

    def error(self, message: str) -> NoReturn:
        raise InventoryError(message)


def _reason(exc: Exception) -> str:
    """Return the text of an error without the path an OSError repeats."""
    if isinstance(exc, OSError) and exc.strerror:
        return exc.strerror
    return str(exc)


def _read_conf(path: str) -> tuple[str, str, str, str]:
    """Return uri_v3, admin user, password and user domain of tempest.conf.

    Interpolation is off, so a `%` in the password is read literally.
    """
    parser = configparser.ConfigParser(interpolation=None, strict=False)
    try:
        with open(path, encoding="utf-8") as conf:
            parser.read_file(conf)
    except (OSError, UnicodeDecodeError, configparser.Error) as exc:
        raise InventoryError(f"cannot read {path}: {_reason(exc)}") from exc
    values = []
    for section, option in _CONF_OPTIONS:
        value = parser.get(section, option, fallback="")
        if not value:
            raise InventoryError(f"tempest.conf has no [{section}] {option}")
        values.append(value)
    uri_v3, username, password, domain = values
    return uri_v3.rstrip("/"), username, password, domain


def _request(method: str, uri_v3: str, path: str, token: str | None = None,
             body: object | None = None) -> tuple[object, str | None]:
    """Send one request and return its JSON answer and X-Subject-Token."""
    headers = {"Accept": "application/json"}
    data = None
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    if token is not None:
        headers["X-Auth-Token"] = token
    request = urllib.request.Request(uri_v3 + path, data=data,
                                     headers=headers, method=method)
    try:
        with _OPENER.open(request, timeout=TIMEOUT_SECONDS) as resp:
            raw = resp.read()
            subject_token = resp.headers.get("X-Subject-Token")
    except urllib.error.HTTPError as exc:
        exc.close()
        raise InventoryError(f"{method} {path} returned {exc.code}") from exc
    except TimeoutError as exc:
        raise InventoryError(f"{method} {path} timed out") from exc
    except urllib.error.URLError as exc:
        if isinstance(exc.reason, TimeoutError):
            raise InventoryError(f"{method} {path} timed out") from exc
        raise InventoryError(
            f"cannot reach {uri_v3}: {exc.reason}") from exc
    except (http.client.HTTPException, OSError) as exc:
        raise InventoryError(f"{method} {path} failed: {exc}") from exc
    try:
        return json.loads(raw), subject_token
    except ValueError as exc:
        raise InventoryError(
            f"{method} {path} returned no JSON: {exc}") from exc


def _token(uri_v3: str, username: str, password: str, domain: str) -> str:
    """Return a system-scoped token of the admin user."""
    body = {
        "auth": {
            "identity": {
                "methods": ["password"],
                "password": {
                    "user": {
                        "name": username,
                        "domain": {"name": domain},
                        "password": password,
                    },
                },
            },
            "scope": {"system": {"all": True}},
        },
    }
    _, token = _request("POST", uri_v3, "/auth/tokens", body=body)
    if not token:
        raise InventoryError("POST /auth/tokens returned no X-Subject-Token")
    return token


def _list(uri_v3: str, token: str, path: str, key: str,
          label: str | None) -> dict[str, str]:
    """Return {id: label} of one collection; only the label field is kept."""
    answer, _ = _request("GET", uri_v3, path, token=token)
    if not isinstance(answer, dict) or not isinstance(answer.get(key), list):
        raise InventoryError(f"GET {path} answered without a '{key}' list")
    if answer.get("truncated") is True:
        raise InventoryError(
            f"GET {path} was truncated by Keystone's list_limit")
    if len(answer[key]) >= MAX_DB_LIMIT:
        raise InventoryError(
            f"GET {path} returned {len(answer[key])} objects; Keystone's "
            f"max_db_limit of {MAX_DB_LIMIT} may have cut the list")
    objects = {}
    for item in answer[key]:
        if not isinstance(item, dict) or not isinstance(item.get("id"), str):
            raise InventoryError(f"GET {path} answered an object without id")
        value = item.get(label) if label else None
        objects[item["id"]] = "" if value is None else str(value)
    return objects


def _write(path: str, text: str) -> None:
    """Write text to path, mapping a failure to InventoryError."""
    try:
        with open(path, "w", encoding="utf-8") as out:
            out.write(text)
    except OSError as exc:
        raise InventoryError(f"cannot write {path}: {_reason(exc)}") from exc


def snapshot(conf_path: str, out_path: str) -> int:
    """Write the inventory of the Keystone tempest.conf names to out_path.

    Every request runs before the file is written, so a failed request
    leaves no snapshot behind.
    """
    uri_v3, username, password, domain = _read_conf(conf_path)
    token = _token(uri_v3, username, password, domain)
    inventory: Snapshot = {}
    for kind, path, key, label in KINDS:
        inventory[kind] = _list(uri_v3, token, path, key, label)
    # GET /roles lists the global roles only; a domain role is listed with
    # its domain's id.
    inventory["domain_role"] = {}
    for domain_id in sorted(inventory["domain"]):
        query = urllib.parse.urlencode({"domain_id": domain_id})
        inventory["domain_role"].update(
            _list(uri_v3, token, f"/roles?{query}", "roles", "name"))
    _write(out_path, json.dumps(inventory, indent=2, sort_keys=True) + "\n")
    return 0


def _load(path: str) -> Snapshot:
    """Read a snapshot file and check its shape."""
    try:
        with open(path, encoding="utf-8") as snap:
            data = json.load(snap)
    except (OSError, ValueError) as exc:
        raise InventoryError(f"cannot read {path}: {_reason(exc)}") from exc
    if not isinstance(data, dict) or not all(
            isinstance(objects, dict) and all(
                isinstance(key, str) and isinstance(value, str)
                for key, value in objects.items())
            for objects in data.values()):
        raise InventoryError(
            f"cannot read {path}: not a keystone-inventory snapshot")
    return data


def _one_line(text: str) -> str:
    """Replace the characters that would split a TSV line with a space."""
    return text.translate(str.maketrans("\t\r\n", "   "))


def diff(before_path: str, after_path: str, out_path: str) -> int:
    """Report the ids only the after snapshot has; return 1 if there is one.

    The lines go to out_path and to stdout, sorted by kind and id. An id only
    the before snapshot has is not reported.
    """
    before = _load(before_path)
    after = _load(after_path)
    lines = []
    for kind in sorted(after):
        known = before.get(kind, {})
        for obj_id in sorted(after[kind]):
            if obj_id not in known:
                lines.append(f"{_one_line(kind)}\t{_one_line(obj_id)}\t"
                             f"{_one_line(after[kind][obj_id])}\n")
    _write(out_path, "".join(lines))
    sys.stdout.write("".join(lines))
    return 1 if lines else 0


def _parser() -> _Parser:
    """Build the command-line parser."""
    parser = _Parser(prog="keystone-inventory")
    commands = parser.add_subparsers(dest="command", required=True)
    snap = commands.add_parser("snapshot", help="write Keystone's inventory")
    snap.add_argument("--conf", required=True, help="tempest.conf to read")
    snap.add_argument("--out", required=True, help="snapshot file to write")
    delta = commands.add_parser("diff", help="report what a run added")
    delta.add_argument("--before", required=True, help="snapshot before")
    delta.add_argument("--after", required=True, help="snapshot after")
    delta.add_argument("--out", required=True, help="TSV file to write")
    return parser


def main(argv: list[str] | None = None) -> int:
    """Run one subcommand and return its exit code."""
    try:
        args = _parser().parse_args(argv)
        if args.command == "snapshot":
            return snapshot(args.conf, args.out)
        return diff(args.before, args.after, args.out)
    except InventoryError as exc:
        print(f"keystone-inventory: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
