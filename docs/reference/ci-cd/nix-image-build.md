---
title: Nix Image Build
quadrant: infrastructure
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

::: v-pre

# Nix Image Build

Reference documentation for the container images the root flake builds. A
Nix-built image keeps the name, the tags, the verify script and the merge and
attestation jobs of a Docker-built image; the build step differs. The images
move off their Dockerfiles one phase at a time
([#1401](https://github.com/C5C3/cobaltcore/issues/1401)), and the
[migration status](#migration-status) table lists the build of every image.
The [Build Images Workflow](./build-images-workflow.md) page describes the
jobs around the build, and [Container Images](./container-images.md) the
images that still build from a Dockerfile.

## Flake layout

The root `flake.nix` has two inputs:

| Input | Channel | Feeds |
| --- | --- | --- |
| `nixpkgs` | `nixos-unstable` | the dev shell, `devShells.<system>.default` ([Nix Development Environment](../../contributing/nix-dev-environment.md)) |
| `nixpkgs-images` | `nixos-26.05` | every image output below |

The images follow the stable channel because the nixpkgs security team
backports fixes to it. The dev shell follows `nixos-unstable` for current tool
versions. `flake.lock` pins both revisions.

On `x86_64-linux` and `aarch64-linux`, the two platforms the build jobs run
on, the images input adds these outputs:

| Output | Content |
| --- | --- |
| `packages.<system>.<name>-image` | a `dockerTools.streamLayeredImage`, a script that writes the image as a tar stream |
| `checks.<system>.nix-lint` | nixfmt, statix and deadnix over the Nix files ([Lint](#lint)) |
| `checks.<system>.fhs-layout` | builds `nix/lib/fhs-layout.nix` with a Python environment and an `extraRoots` entry, the two inputs no image passes yet, and checks the paths of the result |
| `devShells.<system>.image-tools` | skopeo and vulnix at the versions of `flake.lock`, for the `build-nix-image` action |

The package name is flat (`backup-shifter-image`) because `nix flake check`
rejects a nested attribute set under `packages`.

| File | Content |
| --- | --- |
| `flake.nix` | the inputs and outputs |
| `flake.lock` | the pinned revisions of both inputs |
| `nix/lib/image-defaults.nix` | what every image shares: user and group 42424, the creation date `1970-01-01T00:00:01Z`, `PATH` and `LANG`, the licence and vendor labels |
| `nix/lib/fhs-layout.nix` | the FHS façade: `/var/lib/openstack`, `/bin/sh` and `/bin/bash` (bash), `/usr/bin/env`, the CA bundle at `/etc/ssl/certs/ca-certificates.crt`, and `/etc/passwd` and `/etc/group` with the `openstack` user. The Python environment is optional; without one, `/var/lib/openstack` is an empty directory |
| `images/<name>/image.nix` | the image, a function of `pkgs` and `fhsLayout` |

The expression of an image lives beside the image's other files, at
`images/<name>/image.nix`. `hack/ci-generate-cleanup-matrix.sh` lists every
`images/*/` directory, and the path filters of `build-images.yaml` and
`ci.yaml` match `images/<name>/**`, so neither changes when an image moves.

`nix/spike/` is the Phase 0 record of the migration (`nix/spike/README.md`,
decisions D1 to D12). No output of the root flake reads it, no CI job builds
it, and the lint check leaves it out.

## Binary cache

The build jobs read and write the Cachix cache `cobaltcore`.
`cachix/cachix-action` adds it as a substituter in every job that installs
Nix, so every build reads it. A push build and a pull request from a branch of
this repository push the paths they build when the repository secret
`CACHIX_AUTH_TOKEN` holds a push token. While the secret is empty the action
pushes nothing (`skipPush: ${{ secrets.CACHIX_AUTH_TOKEN == '' }}`). The step
runs with `continue-on-error: true`, so a cache that does not exist or does
not answer fails no build either. In both cases the builds substitute from
`cache.nixos.org` and build what it lacks.

Cachix signs every path with the cache's own key, and a build trusts that key
through the substituter `cachix-action` configures. A holder of the token can
therefore add paths that every later build substitutes. The token is a
repository secret, which GitHub does not hand to a workflow run of a pull
request from a fork; there it is empty, and the action pushes nothing.

## Build and load locally

```bash
nix build .#backup-shifter-image
./result | docker load
bash tests/container-images/verify_backup_shifter.sh backup-shifter:latest
```

`./result` is the stream script. `docker load` prints
`Loaded image: backup-shifter:latest`, the name and tag `image.nix` sets; the
workflow pushes under the registry name instead. `nix flake check` evaluates
every output of the host's system and builds both checks.

Without `experimental-features = nix-command flakes` in `nix.conf`, add
`--extra-experimental-features 'nix-command flakes'` to every `nix` command,
as for the [dev shell](../../contributing/nix-dev-environment.md). A flake in a
git checkout sees tracked or staged files only, so `git add` a new
`image.nix` before `nix build` can find it.

## The build-nix-image action

`.github/actions/build-nix-image/action.yaml` is the Nix counterpart of
`build-push-image`.

| Input | Default | Description |
| --- | --- | --- |
| `attribute` | required | flake package attribute, e.g. `backup-shifter-image` |
| `image-name` | required | image name without tag, e.g. `ghcr.io/c5c3/backup-shifter` |
| `push-by-digest` | `'true'` | `'true'` pushes by digest (push events), `'false'` loads locally (pull requests) |
| `artifact-name` | `''` | digest artifact; empty skips the export |
| `grype-category` | `''` | enables the pull-request Grype scan |
| `verify-script` | `''` | verify script; empty skips the verify |
| `verify-image-ref` | `''` | the local tag the loaded image gets on pull requests |
| `vulnix-artifact-name` | `''` | artifact of the vulnix report; empty skips the upload |

The action runs `nix build ".#<attribute>"` and scans the store paths of the
image with vulnix (`nix develop .#image-tools --command vulnix --closure
--json`). The paths are the `store_layers` of the image's `conf.json`, not
the closure of the stream script, which also holds the python3 the script
runs on. vulnix exits 2 with findings and 0 without; both
count as a completed scan when vulnix printed its JSON report. Any other
status, and an exit 2 without a report (vulnix's exit on an internal
error), fail the step. The step summary gets the line
`vulnix: N findings in M derivations`, and `vulnix.json` is uploaded as
`vulnix-artifact-name` for 14 days. Findings never fail the build, as with
the Grype scan's `fail-build: false`.

vulnix reads the NVD as the yearly JSON 2.0 feeds of the current year and
the five before it. nvd.nist.gov serves them at a rate a GitHub runner does
not finish inside the job budget, so the `nvd-feeds` step before the scan
fetches the same data from
[fkie-cad/nvd-json-data-feeds](https://github.com/fkie-cad/nvd-json-data-feeds),
a daily reconstruction of the feeds published as GitHub release assets. The
step resolves the release tag once from the `latest` redirect, downloads the
six `CVE-<year>.json.xz` files and their `.meta` files from that tag, and
runs `rewrap-nvd-feed.py` (next to the action) on each: it checks the sha256
of the `.meta` over the decompressed JSON, moves the items from the mirror's
`cve_items` array into the `vulnerabilities[].cve` wrapper of NIST's feeds,
and writes `nvdcve-2.0-<year>.json.gz` under `$RUNNER_TEMP/nvd-mirror`, the
file name vulnix requests from a mirror. The scan step serves that directory
with `python3 -m http.server` on `127.0.0.1:18765`, passes it to vulnix as
`--mirror`, and gives vulnix a fresh `--cache-dir` under `$RUNNER_TEMP`, so no
run depends on a cache and none asks for the `modified` feed, which the
reconstruction does not publish. The step summary gets the line
`nvd-feeds: <tag>, <first year> to <last year>`. Both steps use the `curl`
and `python3` of the runner image.

Then the action takes one of two paths:

- Pull request: `docker load` reads the stream, the image is tagged
  `verify-image-ref`, Grype scans it through `supply-chain-attest`
  (`scan-mode: image`), and the verify script runs against it.
- Push: the stream is written to a tar, and skopeo pushes it by digest to
  `docker://<image-name>@@unknown-digest@@`, the docker transport's form of a
  push without a tag ([containers/image#2209](https://github.com/containers/image/pull/2209)).
  skopeo reads the credentials of the job's `setup-docker-registry` login.
  The digest goes to `export-digest`.

skopeo pushes the image because a `streamLayeredImage` is a tar stream and
there is no BuildKit build to push. Two pushes of one stream give one digest,
so a re-run of the push path is safe. The merge jobs are the ones the
Docker-built images use: `merge-manifest-and-attest` downloads the
`digests-<image>-*` artifacts, joins the platform digests into a manifest
list, and attests and signs it.

## Lint

`checks.<system>.nix-lint` runs three tools over `flake.nix`, `nix/lib/` and
every `*.nix` under `images/`:

- `nixfmt --check` (formatting)
- `statix check` (anti-patterns)
- `deadnix --fail` (unused bindings and arguments)

It is the lint that replaces hadolint for Nix files. The flake defines no
`nix fmt` formatter, and the `image-tools` shell does not carry the three
tools. Run the check before pushing, and format a file with the locked
nixfmt:

```bash
nix build .#checks.x86_64-linux.nix-lint
nix shell --inputs-from . nixpkgs-images#nixfmt --command nixfmt images/<name>/image.nix
```

## Labels and reproducibility

A Nix-built image carries four static labels:
`org.opencontainers.image.title`, `org.opencontainers.image.description`,
`org.opencontainers.image.licenses` (`Apache-2.0`) and
`org.opencontainers.image.vendor` (`SAP SE`). The five labels
`docker/metadata-action` adds to a Docker-built image (`created`, `revision`,
`source`, `url`, `version`) and the `base.name` and `base.digest` pair are
absent. A label that changes with every commit would change the digest with
every commit; without them, the digest of a Nix-built image moves only when
an input moves. The build-provenance attestation of the merge job records the
commit.

The creation date of every image is `1970-01-01T00:00:01Z`.
`nix build --rebuild .#backup-shifter-image` builds the image a second time
and fails when the two outputs differ.

## Renovate

Renovate's `nix` manager re-locks both inputs of `flake.lock` through
`lockFileMaintenance`. The `nix flake inputs` package rule opens the re-lock
as one pull request on the Monday schedule of the ubuntu digest bump, never
automerged; the re-lock is not a version bump, so the three-day release age
cannot gate it. A re-lock of
`nixpkgs-images` changes `flake.lock`, which the `shifter` filter of the
build-images workflow covers, so its pull request rebuilds the image. A
separate rule disables every update under `nix/spike/**`.
[Dependency Management](../../contributing/dependency-management.md) has the
details.

## Migration status

`tests/unit/docs/nix_image_build_status_table_test.sh` checks the table
against the tree: one row per `images/<name>/` directory and per operator
image, and for an image directory a Build value that matches its files
(`image.nix` for Nix, a `Dockerfile` for Docker). The operator images build
from the shared `operators/Dockerfile`, so the test checks that their rows
exist and read Nix or Docker.

| Image | Build | Phase | Issue |
| --- | --- | --- | --- |
| `backup-shifter` | Nix | Phase 1 | [#1404](https://github.com/C5C3/cobaltcore/issues/1404) |
| `python-base` | Docker | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `venv-builder` | Docker | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `keystone` | Docker | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `horizon` | Docker | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `glance` | Docker | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `placement` | Docker | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `barbican` | Docker | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `neutron` | Docker | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `cinder` | Docker | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `nova` | Docker | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `tempest` | Docker | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `ovn` | Docker | Phase 3 | [#1406](https://github.com/C5C3/cobaltcore/issues/1406) |
| `openstack-hypervisor-operator` | Docker | Phase 3 | [#1406](https://github.com/C5C3/cobaltcore/issues/1406) |
| `kvm-node-agent` | Docker | Phase 3 | [#1406](https://github.com/C5C3/cobaltcore/issues/1406) |
| `nova-compute` | Docker | Phase 4 | [#1407](https://github.com/C5C3/cobaltcore/issues/1407) |
| `libvirt` | Docker | Phase 4 | [#1407](https://github.com/C5C3/cobaltcore/issues/1407) |
| `keystone-federation-proxy` | Docker | Phase 5 | [#1408](https://github.com/C5C3/cobaltcore/issues/1408) |
| `barbican-operator` | Docker | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `c5c3-operator` | Docker | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `cinder-operator` | Docker | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `glance-operator` | Docker | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `horizon-operator` | Docker | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `keystone-operator` | Docker | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `neutron-operator` | Docker | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `nova-operator` | Docker | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `ovn-operator` | Docker | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `placement-operator` | Docker | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |

:::
