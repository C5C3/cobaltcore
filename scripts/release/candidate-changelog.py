# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Prepare the chart checksums and changelog for a CobaltCore release candidate."""

import hashlib
import json
import os
import re
import subprocess
from pathlib import Path
from urllib.request import Request, urlopen

STABLE_TAG = re.compile(r"v(?:0|[1-9][0-9]*)(?:\.(?:0|[1-9][0-9]*)){2}")
RC_NUMBER = re.compile(r"[1-9][0-9]*")


def git(*args):
    return subprocess.check_output(["git", *args], encoding="utf-8")


def version(tag):
    return tuple(map(int, tag[1:].split(".")))


def write_checksums(tag, assets):
    operators = json.loads(os.environ["OPERATORS"])["operator"]
    expected = sorted(f"{operator}-operator-{tag[1:]}.tgz" for operator in operators)
    actual = sorted(
        path.relative_to(assets).as_posix() for path in assets.rglob("*") if path.is_file()
    )
    if not expected or actual != expected:
        raise SystemExit(f"Expected chart archives {expected}, found {actual}")

    source = git("rev-parse", f"refs/tags/{tag}^{{commit}}").strip()
    checksums = [f"# Source commit {source}\n"]
    for name in expected:
        with (assets / name).open("rb") as archive:
            digest = hashlib.file_digest(archive, "sha256").hexdigest()
        checksums.append(f"{digest}  {name}\n")
    (assets / "SHA256SUMS").write_text("".join(checksums), encoding="utf-8")


def github_notes(repo, tag, previous):
    request = Request(
        f"{os.environ['GITHUB_API_URL']}/repos/{repo}/releases/generate-notes",
        data=json.dumps({"tag_name": tag, "previous_tag_name": previous}).encode("utf-8"),
        headers={
            "Accept": "application/vnd.github+json",
            "Authorization": f"Bearer {os.environ['GH_TOKEN']}",
            "Content-Type": "application/json",
            "X-GitHub-Api-Version": "2022-11-28",
        },
    )
    with urlopen(request, timeout=30) as response:
        return json.load(response)["body"] + "\n"


def main():
    tag = os.environ["TAG"]
    commit = os.environ["GITHUB_SHA"]
    repo = os.environ["GITHUB_REPOSITORY"]
    repo_url = f"{os.environ['GITHUB_SERVER_URL']}/{repo}"
    write_checksums(tag, Path("assets"))

    base, rc = tag.split("-rc.")
    base_version = version(base)
    # Include stable tags from release branches that may contain backports.
    stable_tags = (
        name
        for name in git("tag", "--list", "v*").splitlines()
        if STABLE_TAG.fullmatch(name) and version(name) < base_version
    )
    previous = max(stable_tags, key=version, default=None)
    if previous:
        notes = github_notes(repo, tag, previous)
    else:
        notes = "## Recent changes in the first release series\n"
        notes += git("log", "-100", "--first-parent", "--format=- %s (%h)", commit)
        notes += f"\n[Full commit history]({repo_url}/commits/{commit})\n"

    prefix = f"{base}-rc."
    merged = git("tag", "--merged", commit, "--list", f"{prefix}*").splitlines()
    suffixes = (name.removeprefix(prefix) for name in merged)
    rc_numbers = (int(suffix) for suffix in suffixes if RC_NUMBER.fullmatch(suffix))
    current_rc = int(rc)
    previous_rc = max((number for number in rc_numbers if number < current_rc), default=0)
    if previous_rc:
        previous_tag = f"{prefix}{previous_rc}"
        compare_url = f"{repo_url}/compare/{previous_tag}...{tag}"
        notes += f"\n[Changes since {previous_tag}]({compare_url})\n"

    Path("notes.md").write_text(notes, encoding="utf-8")


if __name__ == "__main__":
    main()
