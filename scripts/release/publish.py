# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Publish CobaltCore release candidates and promote their assets to stable."""

import argparse
import hashlib
import json
import os
import re
import subprocess
import tarfile
import tempfile
from pathlib import Path

from changelog import RELEASE_TAG, candidate_tags, generate_changelog, git, tag_commit
from github_api import GitHub

ASSETS = Path("assets")
MANIFEST = "SHA256SUMS"
IMAGE_DIGESTS = "IMAGE_DIGESTS.json"
IMAGE_ARTIFACTS = Path("image-digests")
OCI_DIGEST = re.compile(r"sha256:[0-9a-f]{64}")


def sha256(path):
    with path.open("rb") as file:
        return f"sha256:{hashlib.file_digest(file, 'sha256').hexdigest()}"


def remote_digests(assets):
    return {name: asset.get("digest") for name, asset in assets.items()}


def verify_image_digests(digests, operators):
    if not isinstance(digests, dict) or digests.keys() != set(operators):
        raise SystemExit(f"Expected image digests for {sorted(operators)}")
    for name, digest in digests.items():
        if not isinstance(digest, str) or not OCI_DIGEST.fullmatch(digest):
            raise SystemExit(f"Invalid image digest for {name}: {digest!r}")


def prepare_candidate(github, tag, commit):
    operators = [f"{name}-operator" for name in json.loads(os.environ["OPERATORS"])["operator"]]
    charts = ["target-cluster-access", *operators]
    expected = sorted(f"{chart}-{tag[1:]}.tgz" for chart in charts)
    actual = sorted(
        path.relative_to(ASSETS).as_posix() for path in ASSETS.rglob("*") if path.is_file()
    )
    if not operators or actual != expected:
        raise SystemExit(f"Expected chart archives {expected}, found {actual}")

    image_digests = {
        path.stem: path.read_text(encoding="utf-8").strip()
        for path in IMAGE_ARTIFACTS.glob("*.digest")
    }
    verify_image_digests(image_digests, operators)
    (ASSETS / IMAGE_DIGESTS).write_text(
        json.dumps(image_digests, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return generate_changelog(github, tag, commit), write_manifest([*expected, IMAGE_DIGESTS], commit)


def write_manifest(filenames, commit):
    digests = {name: sha256(ASSETS / name) for name in filenames}
    checksums = "".join(
        f"{digest.removeprefix('sha256:')}  {name}\n" for name, digest in digests.items()
    )
    manifest = ASSETS / MANIFEST
    manifest.write_text(f"# Source commit {commit}\n{checksums}", encoding="utf-8")
    digests[MANIFEST] = sha256(manifest)

    return digests


def verify_manifest(directory, digests, commit):
    if MANIFEST not in digests:
        raise SystemExit(f"Candidate is missing {MANIFEST}")
    expected = [
        f"{digest.removeprefix('sha256:')}  {name}"
        for name, digest in digests.items()
        if name != MANIFEST
    ]
    lines = (directory / MANIFEST).read_text(encoding="utf-8").splitlines()
    if not expected or sorted(lines) != sorted([f"# Source commit {commit}", *expected]):
        raise SystemExit("Candidate checksums or source commit differ from the release")


def package_stable_chart(archive, charts, rc, stable, epoch):
    pattern = rf"([a-z0-9-]+-operator|target-cluster-access)-{re.escape(rc[1:])}\.tgz"
    match = re.fullmatch(pattern, archive.name)
    if not match:
        raise SystemExit(f"Unexpected candidate archive: {archive.name}")
    name = match[1]
    with tarfile.open(archive) as tar:
        for member in tar:
            path = Path(member.name)
            if not member.isfile() or not path.is_relative_to(name) or ".." in path.parts:
                raise SystemExit(f"Unexpected file {member.name} in {archive.name}")
        tar.extractall(charts, filter="data")

    chart = charts / name
    metadata = chart / "Chart.yaml"
    fields = json.loads(subprocess.check_output(["yq", "-o=json", ".", metadata]))
    if fields["name"] != name or fields["version"] != rc[1:]:
        raise SystemExit(f"Chart metadata differs from {archive.name}")
    if "artifacthub.io/prerelease" in (fields.get("annotations") or {}):
        subprocess.run(
            ["yq", "-i", 'del(.annotations."artifacthub.io/prerelease")', metadata],
            check=True,
        )
    # Fix both timestamps so a retry reproduces the archive and OCI manifest.
    subprocess.run(
        ["helm", "package", chart, "--version", stable, "--app-version", stable,
         "--destination", ASSETS],
        env={**os.environ, "SOURCE_DATE_EPOCH": str(epoch)},
        check=True,
    )
    package = ASSETS / f"{name}-{stable}.tgz"
    os.utime(package, (epoch, epoch))
    return package


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
    with tempfile.TemporaryDirectory() as tmp:
        directory = Path(tmp)
        charts = directory / "charts"
        charts.mkdir()
        for name, asset in assets.items():
            github.download(asset, directory / name)
            if sha256(directory / name) != digests[name]:
                raise SystemExit(f"Candidate asset digest differs: {name}")
        verify_manifest(directory, digests, commit)

        stable = tag[1:]
        epoch = int(git("show", "-s", "--format=%ct", commit))
        packages = [
            package_stable_chart(directory / archive, charts, rc, stable, epoch)
            for archive in sorted(assets.keys() - {MANIFEST, IMAGE_DIGESTS})
        ]
        target_chart = ASSETS / f"target-cluster-access-{stable}.tgz"
        if target_chart not in packages or len(packages) < 2:
            raise SystemExit("Candidate must include operator and target-cluster-access charts")
        images = directory / IMAGE_DIGESTS
        if not images.is_file():
            raise SystemExit(f"Candidate is missing {IMAGE_DIGESTS}")
        operators = {
            package.name.removesuffix(f"-{stable}.tgz")
            for package in packages
            if package != target_chart
        }
        verify_image_digests(json.loads(images.read_text(encoding="utf-8")), operators)
        subprocess.run(
            ["bash", "scripts/release/promote-charts.sh", rc[1:], stable, commit, images, *packages],
            check=True,
        )
        (ASSETS / IMAGE_DIGESTS).write_bytes(images.read_bytes())
    notes = f"Promoted from [{rc}]({candidate['html_url']}).\n\n{candidate['body']}\n"
    return notes, write_manifest([*(package.name for package in packages), IMAGE_DIGESTS], commit)


def publish_release(github, release, tag, prerelease, notes, digests):
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
    remote_ref = github.json(f"git/ref/tags/{tag}")["object"]["sha"]
    if remote_ref != git("rev-parse", "--verify", f"refs/tags/{tag}").strip():
        raise SystemExit(f"Remote tag {tag} differs from the checked-out tag")
    release = github.release(tag)
    if release and release["prerelease"] != prerelease:
        raise SystemExit(f"Existing release type does not match {tag}")
    published = release is not None and not release["draft"]
    if args.step == "identify":
        with Path(os.environ["GITHUB_OUTPUT"]).open("a", encoding="utf-8") as output:
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
