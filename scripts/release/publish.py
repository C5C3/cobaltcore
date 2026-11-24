# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Publish CobaltCore release candidates and promote their assets to stable."""

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
from itertools import count
from pathlib import Path
from urllib.parse import urlencode
from urllib.request import Request, urlopen

NUMBER = r"(?:0|[1-9][0-9]*)"
RC_NUMBER = r"[1-9][0-9]*"
STABLE_TAG = rf"v{NUMBER}\.{NUMBER}\.{NUMBER}"
RELEASE_TAG = re.compile(rf"({STABLE_TAG})(?:-rc\.({RC_NUMBER}))?")
ASSETS = Path("assets")
MANIFEST = "SHA256SUMS"
BINARY = "application/octet-stream"


class GitHub:
    def __init__(self):
        repo = os.environ["GITHUB_REPOSITORY"]
        self.api = f"{os.environ['GITHUB_API_URL']}/repos/{repo}"
        self.repo_url = f"{os.environ['GITHUB_SERVER_URL']}/{repo}"
        self.token = os.environ["GH_TOKEN"]

    def request(self, url, *, method=None, data=None, headers=None):
        request = Request(
            url,
            data=data,
            method=method,
            headers={
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
                **(headers or {}),
            },
        )
        # Asset downloads can redirect to storage outside the GitHub API.
        request.add_unredirected_header("Authorization", f"Bearer {self.token}")
        return urlopen(request, timeout=60)

    def json(self, path, *, method="GET", body=None):
        data = None if body is None else json.dumps(body).encode("utf-8")
        with self.request(
            f"{self.api}/{path}",
            method=method,
            data=data,
            headers={"Content-Type": "application/json"} if data is not None else None,
        ) as response:
            return json.load(response)

    def items(self, path):
        for page in count(1):
            items = self.json(f"{path}?per_page=100&page={page}")
            yield from items
            if len(items) < 100:
                return

    def release(self, tag):
        # The list endpoint includes drafts, which must survive failed runs.
        return next(
            (release for release in self.items("releases") if release["tag_name"] == tag),
            None,
        )

    def assets(self, release):
        assets = {}
        for asset in self.items(f"releases/{release['id']}/assets"):
            name = asset["name"]
            if name in {"", ".", ".."} or Path(name).name != name or name in assets:
                raise SystemExit(f"Invalid or duplicate release asset name: {name!r}")
            assets[name] = asset
        return assets

    def download(self, asset):
        url = f"{self.api}/releases/assets/{asset['id']}"
        with self.request(url, headers={"Accept": BINARY}) as response:
            with (ASSETS / asset["name"]).open("xb") as file:
                shutil.copyfileobj(response, file)

    def upload(self, release, name):
        path = ASSETS / name
        url = release["upload_url"].split("{", 1)[0]
        with path.open("rb") as file:
            with self.request(
                f"{url}?{urlencode({'name': name})}",
                method="POST",
                data=file,
                headers={
                    "Content-Type": BINARY,
                    "Content-Length": str(path.stat().st_size),
                },
            ) as response:
                response.read()


def git(*args):
    return subprocess.check_output(["git", *args], encoding="utf-8")


def tag_commit(tag):
    return git("rev-parse", "--verify", f"refs/tags/{tag}^{{commit}}").strip()


def version(tag):
    return tuple(map(int, tag[1:].split(".")))


def previous_rc(base, *, commit=None, before=None):
    prefix = f"{base}-rc."
    merged = ["--merged", commit] if commit else []
    names = git("tag", "--list", f"{prefix}*", *merged).splitlines()
    suffixes = (name.removeprefix(prefix) for name in names)
    numbers = (int(suffix) for suffix in suffixes if re.fullmatch(RC_NUMBER, suffix))
    previous = max(
        (number for number in numbers if before is None or number < before),
        default=None,
    )
    return f"{prefix}{previous}" if previous is not None else None


def sha256(path):
    with path.open("rb") as file:
        return f"sha256:{hashlib.file_digest(file, 'sha256').hexdigest()}"


def remote_digests(assets):
    return {name: asset.get("digest") for name, asset in assets.items()}


def prepare_candidate(github, tag, base, rc, commit):
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

    # Include stable tags from release branches that may contain backports.
    base_version = version(base)
    stable_tags = (
        name
        for name in git("tag", "--list", "v*").splitlines()
        if re.fullmatch(STABLE_TAG, name) and version(name) < base_version
    )
    previous = max(stable_tags, key=version, default=None)
    if previous:
        result = github.json(
            "releases/generate-notes",
            method="POST",
            body={"tag_name": tag, "previous_tag_name": previous},
        )
        notes = result["body"] + "\n"
    else:
        notes = "## Recent changes in the first release series\n"
        notes += git("log", "-100", "--first-parent", "--format=- %s (%h)", commit)
        notes += f"\n[Full commit history]({github.repo_url}/commits/{commit})\n"

    previous = previous_rc(base, commit=commit, before=rc)
    if previous:
        compare_url = f"{github.repo_url}/compare/{previous}...{tag}"
        notes += f"\n[Changes since {previous}]({compare_url})\n"
    return notes, digests


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
    rc = previous_rc(tag)
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
        github.download(asset)
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
            github.upload(release, name)

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
    base, rc = match.groups()
    commit = tag_commit(tag)
    event_commit = git("rev-parse", "--verify", f"{os.environ['GITHUB_SHA']}^{{commit}}").strip()
    if commit != event_commit:
        raise SystemExit(f"{tag} does not point to the event commit")

    github = GitHub()
    prerelease = rc is not None
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
        notes, digests = prepare_candidate(github, tag, base, int(rc), commit)
    else:
        notes, digests = prepare_stable(github, tag, commit)
    publish_release(github, release, tag, prerelease, notes, digests)


if __name__ == "__main__":
    main()
