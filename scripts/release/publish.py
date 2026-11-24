# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Publish CobaltCore release candidates and promote their assets to stable."""

import argparse
import hashlib
import json
import os
import re
from pathlib import Path

from changelog import RELEASE_TAG, candidate_tags, generate_changelog, git, tag_commit
from github_api import GitHub

ASSETS = Path("assets")
MANIFEST = "SHA256SUMS"


def sha256(path):
    with path.open("rb") as file:
        return f"sha256:{hashlib.file_digest(file, 'sha256').hexdigest()}"


def remote_digests(assets):
    return {name: asset.get("digest") for name, asset in assets.items()}


def prepare_candidate(github, tag, commit):
    operators = json.loads(os.environ["OPERATORS"])["operator"]
    expected = sorted(f"{operator}-operator-{tag[1:]}.tgz" for operator in operators)
    actual = sorted(
        path.relative_to(ASSETS).as_posix() for path in ASSETS.rglob("*") if path.is_file()
    )
    if not expected or actual != expected:
        raise SystemExit(f"Expected chart archives {expected}, found {actual}")

    digests = {name: sha256(ASSETS / name) for name in expected}
    checksums = "".join(
        f"{digest.removeprefix('sha256:')}  {name}\n" for name, digest in digests.items()
    )
    manifest = ASSETS / MANIFEST
    manifest.write_text(f"# Source commit {commit}\n{checksums}", encoding="utf-8")
    digests[MANIFEST] = sha256(manifest)

    return generate_changelog(github, tag, commit), digests


def verify_manifest(digests, commit):
    if MANIFEST not in digests:
        raise SystemExit(f"Candidate is missing {MANIFEST}")
    lines = (ASSETS / MANIFEST).read_text(encoding="utf-8").splitlines()
    if f"# Source commit {commit}" not in lines:
        raise SystemExit(f"Candidate {MANIFEST} does not name source commit {commit}")
    checksums = [line for line in lines if not line.startswith("#")]
    if not checksums:
        raise SystemExit(f"Candidate {MANIFEST} contains no checksums")
    for line in checksums:
        match = re.fullmatch(r"([0-9a-fA-F]{64}) [ *](.+)", line)
        if not match or digests.get(match[2]) != f"sha256:{match[1].lower()}":
            raise SystemExit(f"Invalid candidate checksum: {line!r}")


def prepare_stable(github, tag, commit):
    candidates = candidate_tags(tag)
    rc = max(candidates, key=candidates.get, default=None)
    if not rc:
        raise SystemExit(f"No RC for {tag}")
    if tag_commit(rc) != commit:
        raise SystemExit(f"{tag} must point to the same commit as {rc}")
    candidate = github.release(rc)
    if not candidate or (
        candidate["draft"] or not candidate["prerelease"] or not candidate["body"]
    ):
        raise SystemExit(f"{rc} must be a published prerelease with a changelog")

    ASSETS.mkdir(exist_ok=True)
    assets = github.assets(candidate)
    digests = remote_digests(assets)
    for name, asset in assets.items():
        github.download(asset, ASSETS / name)
        if sha256(ASSETS / name) != digests[name]:
            raise SystemExit(f"Candidate asset digest differs: {name}")
    verify_manifest(digests, commit)
    notes = f"Promoted from [{rc}]({candidate['html_url']}).\n\n{candidate['body']}\n"
    return notes, digests


def publish_release(github, release, tag, prerelease, notes, digests):
    remote_ref = github.json(f"git/ref/tags/{tag}")["object"]["sha"]
    if remote_ref != git("rev-parse", "--verify", f"refs/tags/{tag}").strip():
        raise SystemExit(f"Remote tag {tag} differs from the checked-out tag")

    fields = {"body": notes, "prerelease": prerelease}
    if not release:
        release = github.json(
            "releases",
            method="POST",
            body={**fields, "tag_name": tag, "name": tag, "draft": True},
        )

    # Resume a matching draft without replacing assets from an earlier run.
    uploaded = remote_digests(github.assets(release))
    for name, digest in uploaded.items():
        if name not in digests or digest != digests[name]:
            raise SystemExit(f"Draft asset differs: {name}")
    for name in digests:
        if name not in uploaded:
            github.upload(release, ASSETS / name)

    if remote_digests(github.assets(release)) != digests:
        raise SystemExit("Draft assets do not match the expected files and digests")
    github.json(
        f"releases/{release['id']}",
        method="PATCH",
        body={**fields, "draft": False, "make_latest": "false" if prerelease else "true"},
    )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("step", choices=("identify", "publish"))
    args = parser.parse_args()

    tag = os.environ["GITHUB_REF_NAME"]
    match = RELEASE_TAG.fullmatch(tag)
    if not match:
        raise SystemExit(f"Invalid distribution tag: {tag}")
    prerelease = match[2] is not None
    commit = tag_commit(tag)
    event_commit = git("rev-parse", "--verify", f"{os.environ['GITHUB_SHA']}^{{commit}}").strip()
    if commit != event_commit:
        raise SystemExit(f"{tag} does not point to the event commit")

    github = GitHub()
    release = github.release(tag)
    if release and release["prerelease"] != prerelease:
        raise SystemExit(f"Existing release type does not match {tag}")
    published = release is not None and not release["draft"]
    if args.step == "identify":
        with Path(os.environ["GITHUB_OUTPUT"]).open("a", encoding="utf-8") as output:
            output.write(f"kind={'rc' if prerelease else 'stable'}\n")
            if published:
                output.write("published=true\n")
        return

    if published:
        return
    if prerelease:
        notes, digests = prepare_candidate(github, tag, commit)
    else:
        notes, digests = prepare_stable(github, tag, commit)
    publish_release(github, release, tag, prerelease, notes, digests)


if __name__ == "__main__":
    main()
