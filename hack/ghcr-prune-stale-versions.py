#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Delete GHCR package versions that carry no tag worth keeping.

Retention is decided per package version from its *whole tag set*, not from a
"keep the newest N" counter:

  keep    the version carries at least one keeper tag -- `latest`, or a bare
          version / release tag such as `32.0.0`, `2026.1`, `1.2.0-rc1`
  delete  every other version older than --min-age-hours, including untagged
          ones: `<sha7>`, `<sha40>`, `sha-<sha40>`, `<release>-<sha40>`,
          `e2e-<run_id>-*`, `dev`, and composite tags
          (`32.0.0-p0-main-a1b2c3d`) whose manifest carries no version tag

A manifest that holds both a composite tag and its release tag is therefore left
alone: the release tag keeps it. Once a later build moves `32.0.0` and `2026.1`
onto a new manifest, the old one is left with only its composite and SHA tags and
becomes deletable. That is the property a `keep-n-tagged` counter cannot express.

Two things make this less trivial than filtering a tag list:

  * A multi-arch image is an OCI index whose per-platform and buildx attestation
    manifests appear in GHCR as separate, *untagged* versions. Deleting those
    breaks the tagged index that points at them, so every keeper's manifest is
    fetched from the registry and its children are kept too.
  * Cosign and SBOM artifacts are attached as a `sha256-<64hex>` tag naming their
    subject digest. Such a version is kept exactly when its subject is kept.

Modes:

  full sweep         (default) every version is a candidate, untagged included
  --only-tag-pattern narrows candidates to versions whose tags *all* match one of
                     the given patterns. Untagged versions are then out of scope
                     by construction, which is what makes this mode safe to run
                     next to an in-flight `push-by-digest` upload (GH-312).

Usage:

  # nightly sweep for one package
  ghcr-prune-stale-versions.py --org c5c3 --package glance

  # prune one CI run's scoped tags, mid-flight
  ghcr-prune-stale-versions.py --org c5c3 --package glance \\
      --only-tag-pattern "^e2e-${GITHUB_RUN_ID}-" --min-age-hours 0

  # show what would happen, touch nothing
  ghcr-prune-stale-versions.py --org c5c3 --package glance --dry-run
"""

import argparse
import base64
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone

API_ROOT = "https://api.github.com"
REGISTRY_ROOT = "https://ghcr.io"

# Tags that make a version worth keeping forever. Deliberately narrow: anything
# not listed here is a build artifact with a successor.
DEFAULT_KEEP_PATTERNS = (
    r"^latest$",
    # 32.0.0, 2026.1, 0.9.0 -- upstream version, OpenStack release, chart semver
    r"^[0-9]+(\.[0-9]+)+$",
    # 1.2.0-rc1 -- semver prerelease from a v* tag push
    r"^[0-9]+(\.[0-9]+)+-(alpha|beta|rc)[0-9.]*$",
)

# Commit-SHA tag shapes across the four publishing paths in this repo:
# service images (short SHA), base images (long SHA), operator images
# (docker/metadata-action type=sha,format=long), tempest (<release>-<long SHA>).
SHA_TAG_PATTERNS = (
    r"^[0-9a-f]{7}$",
    r"^[0-9a-f]{40}$",
    r"^sha-[0-9a-f]{40}$",
    r"^[0-9]+(\.[0-9]+)+-[0-9a-f]{40}$",
)

# Cosign / SBOM artifacts attach themselves as a tag naming the subject digest.
REFERRER_TAG_RE = re.compile(r"^sha256-([0-9a-f]{64})(\..+)?$")

MANIFEST_ACCEPT = ", ".join(
    (
        "application/vnd.oci.image.index.v1+json",
        "application/vnd.docker.distribution.manifest.list.v2+json",
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    )
)

RETRY_STATUSES = (429, 500, 502, 503, 504)
MAX_ATTEMPTS = 5


# ---------------------------------------------------------------------------
# Logging (GitHub Actions workflow commands when running in CI, plain otherwise)
# ---------------------------------------------------------------------------


def log(message):
    # Diagnostics go to stderr so stdout carries nothing but the --plan-json
    # document; GitHub Actions folds both streams into the job log.
    print(message, file=sys.stderr, flush=True)


def warn(message):
    print(f"::warning::{message}" if in_actions() else f"WARNING: {message}", file=sys.stderr, flush=True)


def error(message):
    print(f"::error::{message}" if in_actions() else f"ERROR: {message}", file=sys.stderr, flush=True)


def in_actions():
    return os.environ.get("GITHUB_ACTIONS") == "true"


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------


def request(url, token=None, method="GET", accept="application/vnd.github+json"):
    """Perform one HTTP request, retrying throttled and transient failures.

    Returns (status, headers, body_bytes), with headers as the case-insensitive
    HTTPMessage rather than a plain dict. A 404 is returned rather than raised:
    callers decide whether a missing manifest or an already-deleted version is
    fatal. The dataaxiom action this script replaces treats a 404 mid-listing as
    fatal, which is what breaks the nightly run on the larger packages.
    """
    for attempt in range(1, MAX_ATTEMPTS + 1):
        req = urllib.request.Request(url, method=method)
        req.add_header("Accept", accept)
        if token:
            req.add_header("Authorization", f"Bearer {token}")
        req.add_header("X-GitHub-Api-Version", "2022-11-28")
        req.add_header("User-Agent", "cobaltcore-ghcr-prune")
        try:
            with urllib.request.urlopen(req) as response:
                return response.status, response.headers, response.read()
        except urllib.error.HTTPError as exc:
            if exc.code == 404:
                return 404, exc.headers, exc.read()
            if (exc.code in RETRY_STATUSES or throttled(exc)) and attempt < MAX_ATTEMPTS:
                delay = retry_delay(exc.headers, attempt)
                warn(f"{method} {url} -> {exc.code}; retrying in {delay}s ({attempt}/{MAX_ATTEMPTS})")
                time.sleep(delay)
                continue
            raise
        except urllib.error.URLError as exc:
            if attempt == MAX_ATTEMPTS:
                raise
            delay = 2**attempt
            warn(f"{method} {url} failed ({exc.reason}); retrying in {delay}s ({attempt}/{MAX_ATTEMPTS})")
            time.sleep(delay)
    raise RuntimeError(f"unreachable: {method} {url}")


def throttled(exc):
    """True for a 403 that is a rate limit rather than a permissions problem.

    GitHub answers both with 403, and retrying a missing token scope five times
    only delays the real error message.
    """
    if exc.code != 403:
        return False
    if exc.headers.get("x-ratelimit-remaining") == "0":
        return True
    return "rate limit" in (exc.headers.get("x-github-message") or "").lower()


def retry_delay(headers, attempt):
    """Honour Retry-After / x-ratelimit-reset, else exponential backoff."""
    retry_after = headers.get("Retry-After")
    if retry_after and retry_after.isdigit():
        return min(int(retry_after), 60)
    reset = headers.get("x-ratelimit-reset")
    if reset and reset.isdigit():
        wait = int(reset) - int(time.time())
        if 0 < wait <= 60:
            return wait
    return 2**attempt


# ---------------------------------------------------------------------------
# GitHub packages API
# ---------------------------------------------------------------------------


def list_versions(org, package, token):
    """Return every container version of the package, newest first."""
    versions = []
    page = 1
    quoted = urllib.parse.quote(package, safe="")
    while True:
        url = f"{API_ROOT}/orgs/{org}/packages/container/{quoted}/versions?per_page=100&page={page}"
        status, _, body = request(url, token=token)
        if status == 404:
            raise SystemExit(f"package not found: {org}/{package}")
        batch = json.loads(body)
        if not batch:
            break
        versions.extend(batch)
        if len(batch) < 100:
            break
        page += 1
    return versions


def delete_version(org, package, version_id, token):
    quoted = urllib.parse.quote(package, safe="")
    url = f"{API_ROOT}/orgs/{org}/packages/container/{quoted}/versions/{version_id}"
    status, _, body = request(url, token=token, method="DELETE")
    if status == 404:
        warn(f"version {version_id} already gone")
        return True
    if status not in (204, 200):
        error(f"deleting version {version_id} failed with {status}: {body[:200]!r}")
        return False
    return True


# ---------------------------------------------------------------------------
# Registry manifests
# ---------------------------------------------------------------------------


class Registry:
    """Fetches manifests from ghcr.io, caching by digest."""

    def __init__(self, owner, package, token):
        self.repo = f"{owner.lower()}/{package}"
        self.token = self._registry_token(token)
        self._cache = {}

    def _registry_token(self, github_token):
        url = f"{REGISTRY_ROOT}/token?service=ghcr.io&scope=repository:{self.repo}:pull"
        req = urllib.request.Request(url)
        req.add_header("User-Agent", "cobaltcore-ghcr-prune")
        if github_token:
            basic = base64.b64encode(f"token:{github_token}".encode()).decode()
            req.add_header("Authorization", f"Basic {basic}")
        try:
            with urllib.request.urlopen(req) as response:
                return json.loads(response.read()).get("token")
        except urllib.error.HTTPError as exc:
            warn(f"registry token request returned {exc.code}; falling back to the GitHub token")
            return base64.b64encode((github_token or "").encode()).decode()

    def children(self, digest):
        """Digests referenced by this manifest, empty for a plain manifest.

        A digest that no longer resolves yields an empty list plus a warning --
        a ghost version cannot be keeping anything alive.
        """
        if digest in self._cache:
            return self._cache[digest]
        url = f"{REGISTRY_ROOT}/v2/{self.repo}/manifests/{digest}"
        try:
            status, _, body = request(url, token=self.token, accept=MANIFEST_ACCEPT)
        except urllib.error.HTTPError as exc:
            warn(f"manifest {digest} unreadable ({exc.code}); treating it as childless")
            self._cache[digest] = []
            return []
        if status == 404:
            warn(f"manifest {digest} not found in the registry; treating it as childless")
            self._cache[digest] = []
            return []
        children = [m["digest"] for m in json.loads(body).get("manifests", []) if m.get("digest")]
        self._cache[digest] = children
        return children


# ---------------------------------------------------------------------------
# Planning
# ---------------------------------------------------------------------------


def tags_of(version):
    return version.get("metadata", {}).get("container", {}).get("tags", []) or []


def parse_timestamp(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def referrer_subjects(tags):
    """Subject digests named by cosign/SBOM referrer tags, if tags are all such."""
    subjects = set()
    for tag in tags:
        match = REFERRER_TAG_RE.match(tag)
        if not match:
            return set()
        subjects.add(f"sha256:{match.group(1)}")
    return subjects


def build_plan(versions, children_of, now, min_age_hours, keep_patterns, only_patterns, max_deletions):
    """Split versions into what stays and what goes.

    `children_of` is a callable digest -> [digest]; injected so the offline
    --plan-from mode can drive the same logic from a fixture.
    """
    keep_res = [re.compile(p) for p in keep_patterns]
    only_res = [re.compile(p) for p in only_patterns]

    by_digest = {v["name"]: v for v in versions}
    keep = set()
    referrers = []

    for version in versions:
        tags = tags_of(version)
        if any(rx.match(tag) for tag in tags for rx in keep_res):
            keep.add(version["name"])
        elif tags:
            subjects = referrer_subjects(tags)
            if subjects:
                referrers.append((subjects, version["name"]))

    # Expand: children of kept manifests, then referrers onto anything kept, until
    # nothing new shows up. Both directions can feed each other -- a referrer may
    # hang off a per-platform child of a kept index.
    expanded = set()
    changed = True
    while changed:
        changed = False
        for digest in list(keep):
            if digest in expanded or digest not in by_digest:
                expanded.add(digest)
                continue
            expanded.add(digest)
            for child in children_of(digest):
                if child not in keep:
                    keep.add(child)
                    changed = True
        for subjects, digest in referrers:
            if digest not in keep and subjects & keep:
                keep.add(digest)
                changed = True

    delete, too_young = [], 0
    for version in versions:
        if version["name"] in keep:
            continue
        tags = tags_of(version)
        if only_res:
            # Narrowed mode: every tag must match, and untagged versions are out
            # of scope entirely (they may be an in-flight push-by-digest upload).
            if not tags or not all(any(rx.search(tag) for rx in only_res) for tag in tags):
                continue
        age_hours = (now - parse_timestamp(version["created_at"])).total_seconds() / 3600
        if age_hours < min_age_hours:
            too_young += 1
            continue
        delete.append(version)

    delete.sort(key=lambda v: v["created_at"])

    capped = 0
    if max_deletions and len(delete) > max_deletions:
        capped = len(delete) - max_deletions
        delete = delete[:max_deletions]

    # GHCR refuses to delete a package's last version, and a package with no
    # versions left is worse than a few stale tags either way.
    if len(delete) >= len(versions) and delete:
        held = delete.pop()
        warn(f"holding back {held['name']} so the package keeps at least one version")

    return {
        "keep": sorted(keep),
        "delete": [
            {"id": v["id"], "digest": v["name"], "tags": tags_of(v), "created_at": v["created_at"]}
            for v in delete
        ],
        "too_young": too_young,
        "capped": capped,
    }


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--org", default=os.environ.get("GITHUB_REPOSITORY_OWNER"), help="GitHub organization")
    parser.add_argument("--package", help="container package name (e.g. glance)")
    parser.add_argument("--token", default=os.environ.get("GITHUB_TOKEN"), help="token with packages:write")
    parser.add_argument("--min-age-hours", type=float, default=24.0, help="skip versions younger than this")
    parser.add_argument(
        "--max-deletions",
        type=int,
        default=500,
        help="stop after this many deletions per run; 0 disables the cap",
    )
    parser.add_argument(
        "--keep-tag-pattern",
        action="append",
        default=[],
        metavar="REGEX",
        help="additional keeper pattern, on top of the built-in ones (repeatable)",
    )
    parser.add_argument(
        "--only-tag-pattern",
        action="append",
        default=[],
        metavar="REGEX",
        help="only consider versions whose tags all match one of these (repeatable)",
    )
    parser.add_argument(
        "--only-sha-tags",
        action="store_true",
        help="shorthand for the four commit-SHA tag shapes this repo publishes",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        default=os.environ.get("GHCR_PRUNE_DRY_RUN", "").lower() in ("1", "true", "yes"),
        help="print the plan, delete nothing (also settable via GHCR_PRUNE_DRY_RUN)",
    )
    parser.add_argument("--plan-from", metavar="FILE", help="offline: read versions and manifests from a fixture")
    parser.add_argument("--plan-json", action="store_true", help="print the plan as JSON on stdout")
    args = parser.parse_args(argv)

    if args.only_sha_tags:
        args.only_tag_pattern = list(args.only_tag_pattern) + list(SHA_TAG_PATTERNS)

    if not args.plan_from:
        if not args.org:
            parser.error("--org is required (or set GITHUB_REPOSITORY_OWNER)")
        if not args.package:
            parser.error("--package is required")
        if not args.token:
            parser.error("--token is required (or set GITHUB_TOKEN)")
    return args


def load_fixture(path):
    """Offline input: {"versions": [...], "manifests": {digest: {...}}, "now": ...}."""
    with open(path, encoding="utf-8") as handle:
        fixture = json.load(handle)
    manifests = fixture.get("manifests", {})

    def children_of(digest):
        return [m["digest"] for m in manifests.get(digest, {}).get("manifests", []) if m.get("digest")]

    now = parse_timestamp(fixture["now"]) if "now" in fixture else datetime.now(timezone.utc)
    return fixture["versions"], children_of, now


def summarize(package, versions, plan, only_mode):
    log(f"[{package}] {len(versions)} versions, {len(plan['keep'])} kept, {len(plan['delete'])} to delete")
    if plan["too_young"]:
        log(f"[{package}] {plan['too_young']} candidates skipped as too young")
    if plan["capped"]:
        # Never let a cap look like "everything was covered".
        warn(f"[{package}] deletion cap reached: {plan['capped']} candidates left for the next run")
    if only_mode:
        log(f"[{package}] narrowed mode: untagged versions were not considered")


def main(argv=None):
    args = parse_args(argv)
    keep_patterns = list(DEFAULT_KEEP_PATTERNS) + args.keep_tag_pattern

    if args.plan_from:
        versions, children_of, now = load_fixture(args.plan_from)
        package = args.package or "fixture"
    else:
        package = args.package
        versions = list_versions(args.org, package, args.token)
        children_of = Registry(args.org, package, args.token).children
        now = datetime.now(timezone.utc)

    if not versions:
        log(f"[{package}] no versions")
        return 0

    plan = build_plan(
        versions,
        children_of,
        now,
        args.min_age_hours,
        keep_patterns,
        args.only_tag_pattern,
        max(args.max_deletions, 0),
    )

    # An empty keep set in full-sweep mode means either the classification is
    # broken or the API returned junk -- either way, deleting every version of a
    # package is not the right response. Narrowed mode is scoped by its patterns,
    # so a package that legitimately has no keeper tag yet (a brand-new operator
    # image before its first main publish) only warns.
    if not plan["keep"]:
        if args.only_tag_pattern:
            warn(f"[{package}] no version carries a keeper tag")
        else:
            error(f"[{package}] no version carries a keeper tag; refusing to sweep the whole package")
            return 1

    summarize(package, versions, plan, bool(args.only_tag_pattern))
    if args.plan_json:
        print(json.dumps(plan, indent=2, sort_keys=True))

    failures = 0
    for entry in plan["delete"]:
        label = ", ".join(entry["tags"]) if entry["tags"] else "<untagged>"
        if args.dry_run or args.plan_from:
            log(f"[{package}] would delete {entry['digest'][:19]} ({label})")
            continue
        log(f"[{package}] deleting {entry['digest'][:19]} ({label})")
        if not delete_version(args.org, package, entry["id"], args.token):
            failures += 1

    if failures:
        error(f"[{package}] {failures} deletions failed")
        return 1
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except urllib.error.HTTPError as exc:
        detail = ""
        try:
            detail = json.loads(exc.read()).get("message", "")
        except (ValueError, OSError):
            pass
        error(f"{exc.code} {exc.reason} for {exc.url}{': ' + detail if detail else ''}")
        if exc.code == 403:
            error("the token needs the packages:write permission (read:packages for --dry-run)")
        sys.exit(1)
