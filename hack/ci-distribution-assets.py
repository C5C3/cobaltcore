# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Collect signed CI artifacts and promote a verified candidate in GHCR."""

import argparse
import hashlib
import json
import os
import re
import subprocess
import tarfile
import tempfile
from pathlib import Path

ASSETS = Path("assets")
MANIFEST = "SHA256SUMS"
IMAGE_DIGESTS = "IMAGE_DIGESTS.json"
NOTES = "RELEASE_NOTES.md"
IMAGE_ARTIFACTS = Path("image-digests")
OCI_DIGEST = re.compile(r"sha256:[0-9a-f]{64}")


def sha256(path):
    with path.open("rb") as file:
        return f"sha256:{hashlib.file_digest(file, 'sha256').hexdigest()}"


def verify_image_digests(digests, operators):
    if not isinstance(digests, dict) or digests.keys() != set(operators):
        raise SystemExit(f"Expected image digests for {sorted(operators)}")
    for name, digest in digests.items():
        if not isinstance(digest, str) or not OCI_DIGEST.fullmatch(digest):
            raise SystemExit(f"Invalid image digest for {name}: {digest!r}")


def prepare_candidate(tag, commit):
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
    write_manifest([*expected, IMAGE_DIGESTS], commit)


def write_manifest(filenames, commit):
    plan = json.loads(Path("release-plan.json").read_text(encoding="utf-8"))["nextRelease"]
    (ASSETS / NOTES).write_text(plan["notes"], encoding="utf-8")
    digests = {name: sha256(ASSETS / name) for name in [*filenames, NOTES]}
    checksums = "".join(
        f"{digest.removeprefix('sha256:')}  {name}\n" for name, digest in digests.items()
    )
    manifest = ASSETS / MANIFEST
    manifest.write_text(f"# Source commit {commit}\n{checksums}", encoding="utf-8")


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


def prepare_stable(tag, commit):
    tags = subprocess.check_output(["git", "tag", "--list", f"{tag}-rc.*"], text=True).splitlines()
    candidates = [name for name in tags if re.fullmatch(re.escape(tag) + r"-rc\.[1-9][0-9]*", name)]
    if not candidates:
        raise SystemExit(f"No release candidate for {tag}")
    rc = max(candidates, key=lambda name: int(name.rsplit(".", 1)[1]))
    sha = subprocess.check_output(["git", "rev-parse", f"{rc}^{{commit}}"], text=True).strip()
    if sha != commit:
        raise SystemExit(f"{rc} points to {sha}, selected commit is {commit}")
    candidate = json.loads(subprocess.check_output([
        "gh", "release", "view", rc, "--json", "isDraft,isPrerelease,assets",
    ]))
    if candidate["isDraft"] or not candidate["isPrerelease"]:
        raise SystemExit(f"{rc} must be a published prerelease")
    ASSETS.mkdir(exist_ok=True)
    assets = {asset["name"]: asset["digest"] for asset in candidate["assets"]}
    operators = {path.parent.name for path in Path("operators").glob("*/helm/*-operator/Chart.yaml")}
    expected = {f"{name}-{rc[1:]}.tgz" for name in {*operators, "target-cluster-access"}}
    if not operators or assets.keys() != expected | {MANIFEST, IMAGE_DIGESTS, NOTES}:
        raise SystemExit("Candidate is missing a distribution asset or includes an unexpected file")
    with tempfile.TemporaryDirectory() as tmp:
        directory = Path(tmp)
        charts = directory / "charts"
        charts.mkdir()
        subprocess.run(["gh", "release", "download", rc, "--dir", directory], check=True)
        for name, digest in assets.items():
            if sha256(directory / name) != digest:
                raise SystemExit(f"Candidate asset digest differs: {name}")
        verify_manifest(directory, assets, commit)

        stable = tag[1:]
        epoch = int(subprocess.check_output(["git", "show", "-s", "--format=%ct", commit]))
        packages = [
            package_stable_chart(directory / archive, charts, rc, stable, epoch)
            for archive in sorted(expected)
        ]
        images = directory / IMAGE_DIGESTS
        verify_image_digests(json.loads(images.read_text(encoding="utf-8")), operators)
        subprocess.run(
            ["bash", "hack/ci-promote-distribution.sh", stable, commit, images, *packages],
            check=True,
        )
        (ASSETS / IMAGE_DIGESTS).write_bytes(images.read_bytes())
    write_manifest([*(package.name for package in packages), IMAGE_DIGESTS], commit)
    with Path(os.environ["GITHUB_ENV"]).open("a", encoding="utf-8") as env:
        env.write(f"CANDIDATE_TAG={rc}\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("candidate", "stable"))
    args = parser.parse_args()
    version = os.environ["EXPECTED_VERSION"]
    tag = f"v{version}"
    commit = os.environ["GITHUB_SHA"]
    if args.mode == "candidate":
        prepare_candidate(tag, commit)
    else:
        prepare_stable(tag, commit)
