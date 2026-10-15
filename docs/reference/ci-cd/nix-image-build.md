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
contents of every image.

## Flake layout

The root `flake.nix` has five inputs:

| Input | Channel | Feeds |
| --- | --- | --- |
| `nixpkgs` | `nixos-unstable` | the dev shell, `devShells.<system>.default` ([Nix Development Environment](../../contributing/nix-dev-environment.md)) |
| `nixpkgs-images` | `nixos-26.05` | every image output below and the `lock-tools` shell |
| `pyproject-nix` | default branch of `pyproject-nix/pyproject.nix` | the Python package builders of the image environments |
| `uv2nix` | default branch of `pyproject-nix/uv2nix` | the reader of `uv.lock` that drives those builders |
| `pyproject-build-systems` | default branch of `pyproject-nix/build-system-pkgs` | the build systems of the packages built from an sdist |

The images follow the stable channel because the nixpkgs security team
backports fixes to it. The dev shell follows `nixos-unstable` for current tool
versions. The three pyproject-nix inputs follow `nixpkgs-images`, so one
nixpkgs builds every image. `flake.lock` pins all five revisions.

On `x86_64-linux` and `aarch64-linux`, the two platforms the build jobs run
on, the images input adds these outputs:

| Output | Content |
| --- | --- |
| `packages.<system>.<name>-image` | a `dockerTools.streamLayeredImage`, a script that writes the image as a tar stream (`backup-shifter-image`) |
| `packages.<system>.<name>-<rel_>-image` | the image of a Python service or of tempest for one release, built from the lock `nix/locks/<release>/<name>/` (`keystone-2026_1-image`) |
| `packages.<system>.<name>-<rel_>-test-run` | the unit tests of a service lock, a build that succeeds whenever the suite ran ([Unit tests](#unit-tests)) |
| `checks.<system>.<name>-<rel_>-tests` | fails unless that run passed |
| `checks.<system>.nix-lint` | nixfmt, statix and deadnix over the Nix files ([Lint](#lint)) |
| `checks.<system>.fhs-layout` | builds `nix/lib/fhs-layout.nix` with a Python environment, an `extraRoots` and a `rootLinks` entry, and checks the paths of the result |
| `checks.<system>.service-tests` | runs `nix/lib/service-tests.nix` over the fixture suite under `nix/lib/fixtures/service-tests/`, which has one failing test, and checks that the run still writes its results |
| `devShells.<system>.image-tools` | skopeo and vulnix at the versions of `flake.lock`, for the `build-nix-image` action |

The package names are flat because `nix flake check` rejects a nested
attribute set under `packages`. `<rel_>` is the release with its dot replaced
by `_`, since an unquoted flake attribute cannot contain a dot. The
directories under `nix/locks/` are the inventory of the Python images: one
output per `<release>/<name>/` directory.

On all four systems of the flake, `devShells.<system>.lock-tools` carries the
uv, yq, jq, git, Python 3.12 and bash 5 of `nixpkgs-images` that
`hack/nix-lock.sh` runs ([Python environments and locks](#python-environments-and-locks)).

| File | Content |
| --- | --- |
| `flake.nix` | the inputs and outputs |
| `flake.lock` | the pinned revisions of the five inputs |
| `nix/lib/image-defaults.nix` | what every image shares: user and group 42424, the creation date `1970-01-01T00:00:01Z`, `PATH` and `LANG`, the licence and vendor labels |
| `nix/lib/fhs-layout.nix` | the FHS façade: `/var/lib/openstack`, `/bin/sh` and `/bin/bash` (bash), `/usr/bin/env`, the CA bundle at `/etc/ssl/certs/ca-certificates.crt`, and `/etc/passwd` and `/etc/group` with the `openstack` user. The Python environment is optional; without one, `/var/lib/openstack` is an empty directory. `extraRoots` adds store paths below `/var/lib/openstack`, `rootLinks` below the image root (`usr/share/novnc`) |
| `nix/lib/python-env.nix` | the uv2nix environment of one lock: the venv, the venv with the test group, and the patched source |
| `nix/lib/service-image.nix` | the image of a Python service or of tempest around that venv ([Service images](#service-images)) |
| `nix/lib/service-tests.nix` | the unit-test run and check of a lock |
| `nix/lib/subunit-crc.py` | recomputes the packet checksums of a subunit stream after `nuke-refs` |
| `nix/pkgs/ceph-bindings.nix` | the `rados` and `rbd` modules for Python 3.12, built from the ceph source of `nixpkgs-images` |
| `nix/locks/<release>/<name>/` | the lock of one service or tempest release, written by `hack/nix-lock.sh` |
| `images/<name>/image.nix` | the image; backup-shifter's is a function of `pkgs` and `fhsLayout`, a Python image's of `serviceImage`, its environment `env` and what else it names |

The expression of an image lives beside the image's other files, at
`images/<name>/image.nix`. `hack/ci-generate-cleanup-matrix.sh` lists every
`images/*/` directory, and the path filters of `build-images.yaml` and
`ci.yaml` match `images/<name>/**`, so neither changes when an image moves.

`nix/spike/` is the Phase 0 record of the migration (`nix/spike/README.md`,
decisions D1 to D12). No output of the root flake reads it, no CI job builds
it, and the lint check leaves it out.

## Python environments and locks

`hack/nix-lock.sh` translates the release pins into one lock per service and
release, plus one per release for tempest. A lock is the directory
`nix/locks/<release>/<name>/` with three files:

| File | Content |
| --- | --- |
| `sources.json` | for a service: the ref `releases/<release>/source-refs.yaml` pins, the commit that ref names, the NAR hash of its tree, the project name and the paths of `patches/<name>/<release>/*.patch`; for tempest: the tempest version and the project name |
| `pyproject.toml` | a virtual uv project: the service as a git source at that commit with its `pip_extras`, the `pip_packages` of `releases/<release>/extra-packages.yaml`, the pins of `images/venv-builder/requirements.txt`, the constraints of `releases/<release>/upper-constraints.txt` after `overrides/<release>/constraints.txt`, and a `test` dependency group |
| `uv.lock` | what `uv lock` resolves from `pyproject.toml` |

The build fetches the source by commit and hash and applies the patches with
`pkgs.applyPatches`, never through the tag. The project name comes from the
fetched tree, so the placement lock names `openstack-placement`. The test
group holds `stestr` and `testtools` for a suite with a `.stestr.conf` and
`pytest` with the `hacking` pin of horizon's tox environment for the one
without, each unless `test-requirements.txt` lists it, followed by that
file's lines. The tempest lock pins the five keys of
`releases/<release>/test-refs.yaml` and the `openstack` client with its
placement and barbican plugins, and fetches nothing.

```bash
make nix-lock            # hack/nix-lock.sh --all in the lock-tools shell
make nix-lock-check      # hack/nix-lock.sh --check
hack/nix-lock.sh keystone 2026.1
```

`--all` writes every lock the release files pin, writes the hash of the noVNC
commit into `images/nova/novnc.json`, and deletes every directory under
`nix/locks/` the release files do not name. `--check` runs `--all` and fails
with `nix-lock.sh: the locks are stale; run make nix-lock and commit the
result` when `git status` reports a change under `nix/locks/` or in
`novnc.json`. The `nix-lock-check` job of the build-images workflow runs it on
every run.

The committed `uv.lock` seeds `uv lock`, so uv keeps every locked version the
inputs still allow. A rerun with unchanged inputs therefore writes the same
lock, also for the packages no constraint pins. The locks are written with the
uv of `nixpkgs-images`, which the `lock-tools` shell carries. A failed step
leaves the previous lock as it was, and a patch that edits `requirements.txt`
or `test-requirements.txt` is refused, because the lock is resolved from the
upstream files.

`nix/lib/python-env.nix` builds a lock with uv2nix, preferring wheels.
`uv.lock` records no build systems, so the packages built from an sdist get
theirs from one table in that file, each row with the error that made it
necessary. The project itself is built from its source with pbr and a
throwaway git commit of the fetched tree, which gives pbr its version and its
file list.

## Service images

`nix/lib/service-image.nix` builds the image of a Python service or of tempest
around the venv of its lock. The image holds the FHS façade, the coreutils,
findutils, grep, sed and awk the operators and the e2e suites run in the
containers, `/etc/services` and `/etc/protocols`, and `/usr/share/zoneinfo`.
`/tmp` and `/var/tmp` have mode 1777 and `/var/run` links to `/run`. sudo is
installed setuid at `/usr/bin/sudo` with no rule for the service user. The
venv's `bin/uwsgi` is a copy of the uwsgi binary. uWSGI names its embedded
interpreter after the resolved path of `/proc/self/exe`, and Python finds the
venv's `pyvenv.cfg`, and with it the service package, only beside that path.
The `uwsgi --pyrun` probe of `tests/lib/uwsgi.sh` checks that import in each
service image. The image runs as 42424, sets `PATH` to
`/var/lib/openstack/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`,
and has no `Cmd` and no `Entrypoint`.

Each `images/<name>/image.nix` adds what its image needs: a WSGI script, the
packages its processes run, the state directories, the `rados` and `rbd`
modules of `nix/pkgs/ceph-bindings.nix` through a `ceph-bindings.pth` in the
venv, or a link outside `/var/lib/openstack`. [Container Images](./container-images.md)
lists them per service. `docker load` names the image `<name>:<release>`.

## Unit tests

`nix/lib/service-tests.nix` turns a lock into two derivations:

| Derivation | Content |
| --- | --- |
| `<name>-<rel_>-test-run` | runs the suite over the patched source with the venv of the test group; it succeeds whenever the suite ran and writes `exit-status`, and `testresults.subunit` and `summary.txt` for a stestr suite or the JUnit XML files for horizon's pytest suite |
| `checks.<name>-<rel_>-tests` | prints `summary.txt` and fails with `<name> <release> unit tests failed (exit N)` unless `exit-status` holds 0; it names the run, which `nix store delete <run>` discards once no result link points at it (`nix-store --query --roots <run>` lists them), so the next build runs the suite again |

A failed check has no output, while the `test-service-images` job has to
upload the subunit stream of a failed run, so the run and the check are apart.
The Cachix step of that job pushes no `-test-run` path, so a failed run never
reaches the cache.

The run starts MySQL 8.4 on 127.0.0.1:3306 and PostgreSQL 18 on
127.0.0.1:5432 inside the build sandbox and sets
`OS_TEST_DBAPI_ADMIN_CONNECTION` to them, so oslo.db's opportunistic database
tests run instead of skipping; horizon's run starts neither. A server that
does not start fails the run before the suite starts, with the reason in the
build log (`service-tests: mysqld did not answer within 60 s`). The store then
keeps no output of the run, so the next build starts the servers again instead
of reusing a cached failure.

A stestr suite runs with the release exclude list
`releases/<release>/test-excludes/<name>.txt` and the sandbox exclude list in
`nix/lib/service-tests.nix`. A test that fails only in the sandbox goes into
the sandbox list, with the error it fails with there and the releases it fails
in, never into `releases/`.

Nix's syscall filter (`filter-syscalls`, on by default) fails every `setxattr`
in the sandbox with `EOPNOTSUPP`. glance's xattr image-cache tests probe for
that and skip themselves with `filesystem does not support xattr`: 20 tests per
release that the Docker test container of the build-images workflow ran. nova
34.0.0 skips `test_inject_admin_password` with `libcrypt is not available`, the
error the 2026.1 entry of the sandbox list records: without an ldconfig cache,
`ctypes.util.find_library` finds no libcrypt.

```bash
nix build .#keystone-2026_1-test-run && cat result/summary.txt
nix build .#checks.x86_64-linux.keystone-2026_1-tests
```

## e2e builds

The `build-e2e-images` job of the CI workflow builds the changed service and
Tempest images on its self-hosted runner without installing Nix there. Its
`Build Nix images` step runs `hack/ci-nix-build-e2e-images.sh` in the
`nixos/nix` container image, pinned by tag and digest. The script builds the
`<name>-<rel_>-image` attributes of every image in one `nix build` with one job
per CPU, then writes each stream to `.nix-e2e/<name>-<release>.tar`; the next
step loads and tags the tars on the runner.

The container is unprivileged and cannot create the namespaces of the build
sandbox, so the builds run with `--option sandbox false`. The builders, which
run the setup code of every sdist the locks build, therefore run as build users
of their own, never as the container's root: the runner mounts its Docker
socket and the workspace into a container action, and a build user can neither
open the socket nor write the workspace. A build user can read the runner's
temporary directory, mounted at `/github/runner_temp`, so the job's checkout
sets `persist-credentials: false` and leaves no token there. The script
writes the group `e2ebld` (gid 40000) and one user per job
(uids from 40001) to the container's account files and passes
`--option build-users-group e2ebld`. It does not use the image's `nixbld`
users: they have the uids of a host Nix daemon's build users, and that daemon
kills every process of a build uid when one of its own builds ends. Nix substitutes from `cache.nixos.org` and,
once the script's `CACHIX_PUBLIC_KEY` is set, from the `cobaltcore` cache; an
unreachable cache only warns. [CI Workflow](./ci-workflow.md) describes the job
and the images it builds.

## Binary cache

The build jobs read and write the Cachix cache `cobaltcore`. The
`.github/actions/setup-nix` composite action installs Nix and runs
`cachix/cachix-action`, which adds the cache as a substituter, in every job
that builds from the flake, so every build reads it. A push build and a pull request from a branch of
this repository push the paths they build when the repository secret
`CACHIX_AUTH_TOKEN` holds a push token. While the secret is empty the action
pushes nothing (`skipPush: ${{ inputs.cachix-auth-token == '' }}`, the input
the jobs fill from the secret). The step
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

nix build .#keystone-2026_1-image && ./result | docker load
bash tests/container-images/verify_keystone.sh keystone:2026.1
```

`./result` is the stream script. `docker load` prints
`Loaded image: backup-shifter:latest` or `Loaded image: keystone:2026.1`, the
name and tag the image expression sets; the workflow pushes under the
registry name instead. `nix flake check --no-build` evaluates every output of
the host's system; without `--no-build` it also builds every check, the unit
tests of all locks included.

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

`checks.<system>.nix-lint` runs three tools over `flake.nix`, `nix/lib/`,
`nix/pkgs/` and every `*.nix` under `images/`:

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
`org.opencontainers.image.vendor` (`SAP SE`). A service or tempest image adds
`org.opencontainers.image.version`, the ref its lock pins (`29.0.0` for
keystone 2026.1), which changes only with the lock. The five labels
`docker/metadata-action` adds to a Docker-built image (`created`, `revision`,
`source`, `url`, `version`) and the `base.name` and `base.digest` pair are
absent. A label that changes with every commit would change the digest with
every commit; without them, the digest of a Nix-built image moves only when
an input moves. The build-provenance attestation of the merge job records the
commit.

The creation date of every image is `1970-01-01T00:00:01Z`.
`nix build --rebuild .#backup-shifter-image` builds the image a second time
and fails when the two outputs differ; the same holds for every other image
attribute.

## Renovate

Renovate's `nix` manager re-locks the five inputs of `flake.lock` through
`lockFileMaintenance`. The `nix flake inputs` package rule opens the re-lock
as one pull request on the Monday schedule of the ubuntu digest bump, never
automerged; the re-lock is not a version bump, so the three-day release age
cannot gate it. A re-lock changes `flake.lock`, which the `shifter` and
`base` filters of the build-images workflow cover, so its pull request
rebuilds the images. Separate rules disable every update under
`nix/spike/**` and under `nix/locks/**`: the locks are derived files that only
`hack/nix-lock.sh` writes. `ignorePaths` also keeps `nix/locks/**` out of
extraction, and repeats the paths of the `config:recommended` preset, which a
repository value replaces.

A Renovate pull request that moves a pin the locks are derived from (under
`releases/`, `overrides/`, `patches/` or `images/venv-builder/requirements.txt`,
or the noVNC pin in `images/nova/novnc.json`) fails `nix-lock-check` until
someone runs `make nix-lock` on its branch and pushes the result.
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
| `python-base` | Docker | Phase 4 | [#1407](https://github.com/C5C3/cobaltcore/issues/1407) |
| `venv-builder` | Docker | Phase 4 | [#1407](https://github.com/C5C3/cobaltcore/issues/1407) |
| `keystone` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `horizon` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `glance` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `placement` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `barbican` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `neutron` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `cinder` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `nova` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `tempest` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
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
