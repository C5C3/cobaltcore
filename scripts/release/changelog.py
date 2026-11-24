# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Generate CobaltCore release notes for a version."""

import argparse
import re
import subprocess

from github_api import GitHub

NUMBER = r"(?:0|[1-9][0-9]*)"
STABLE_TAG = re.compile(rf"v{NUMBER}\.{NUMBER}\.{NUMBER}")
RELEASE_TAG = re.compile(rf"({STABLE_TAG.pattern})(?:-rc\.([1-9][0-9]*))?")


def git(*args):
    return subprocess.check_output(["git", *args], encoding="utf-8")


def tag_commit(tag):
    return git("rev-parse", "--verify", f"refs/tags/{tag}^{{commit}}").strip()


def version(tag):
    return tuple(map(int, tag[1:].split(".")))


def candidate_tags(base, *args):
    prefix = f"{base}-rc."
    return {
        name: int(name.removeprefix(prefix))
        for name in git("tag", "--list", f"{prefix}*", *args).splitlines()
        if RELEASE_TAG.fullmatch(name)
    }


def generate_changelog(github, tag, commit):
    base, rc = RELEASE_TAG.fullmatch(tag).groups()
    published = {
        release["tag_name"]: release["prerelease"]
        for release in github.items("releases")
        if not release["draft"]
    }

    base_version = version(base)
    stable_tags = (
        name
        for name, prerelease in published.items()
        if not prerelease and STABLE_TAG.fullmatch(name) and version(name) < base_version
    )
    previous = max(stable_tags, key=version, default=None)
    if previous:
        result = github.json(
            "releases/generate-notes",
            method="POST",
            body={
                "tag_name": tag,
                "target_commitish": commit,
                "previous_tag_name": previous,
            },
        )
        notes = result["body"] + "\n"
    else:
        notes = "## Recent changes in the first release series\n"
        notes += git("log", "-100", "--first-parent", "--format=- %s (%h)", commit)
        notes += f"\n[Full commit history]({github.repo_url}/commits/{commit})\n"

    if rc:
        candidates = candidate_tags(base, "--merged", commit)
        current_rc = int(rc)
        earlier = (
            name
            for name, number in candidates.items()
            if number < current_rc and published.get(name)
        )
        previous_rc = max(earlier, key=candidates.get, default=None)
        if previous_rc:
            compare_url = f"{github.repo_url}/compare/{previous_rc}...{tag}"
            notes += f"\n[Changes since {previous_rc}]({compare_url})\n"
    return notes


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version", help="Stable or RC version, with or without a v prefix")
    args = parser.parse_args()
    tag = "v" + args.version.removeprefix("v")
    if not RELEASE_TAG.fullmatch(tag):
        parser.error(f"invalid distribution version: {args.version}")

    if git("tag", "--list", tag).strip():
        commit = tag_commit(tag)
    else:
        commit = git("rev-parse", "HEAD").strip()
    print(generate_changelog(GitHub(), tag, commit), end="")


if __name__ == "__main__":
    main()
