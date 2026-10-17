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
| `packages.<system>.<name>-image` | a `dockerTools.streamLayeredImage`, a script that writes the image as a tar stream (`backup-shifter-image`, `keystone-federation-proxy-image`, `ovn-image`, `openstack-hypervisor-operator-image`, `kvm-node-agent-image`, `libvirt-image`) |
| `packages.<system>.<name>-<rel_>-image` | the image of a Python service or of tempest for one release, built from the lock `nix/locks/<release>/<name>/` (`keystone-2026_1-image`) |
| `packages.<system>.nova-compute-<rel_>-image` | the compute image for each nova lock, built from that lock with its `nova-compute` dependency group ([Hypervisor images](#hypervisor-images)) |
| `packages.<system>.<name>-<rel_>-test-run` | the unit tests of a service lock, a build that succeeds whenever the suite ran ([Unit tests](#unit-tests)) |
| `checks.<system>.<name>-<rel_>-tests` | fails unless that run passed |
| `checks.<system>.nix-lint` | nixfmt, statix and deadnix over the Nix files ([Lint](#lint)) |
| `checks.<system>.fhs-layout` | builds `nix/lib/fhs-layout.nix` with a Python environment, an `extraRoots` and a `rootLinks` entry, and checks the paths of the result |
| `checks.<system>.service-tests` | runs `nix/lib/service-tests.nix` over the fixture suite under `nix/lib/fixtures/service-tests/`, which has one failing test, and checks that the run still writes its results |
| `checks.<system>.libvirt-pairing` | fails evaluation unless the `libvirt-python` of every nova lock builds against the libvirt `libvirt-image` runs ([Hypervisor images](#hypervisor-images)) |
| `checks.<system>.libvirt-pairing-rejects-foreign` | fails evaluation unless the same comparison against nixpkgs' own libvirt throws |
| `devShells.<system>.image-tools` | skopeo and vulnix at the versions of `flake.lock`, for the `build-nix-image` action |

The package names are flat because `nix flake check` rejects a nested
attribute set under `packages`. The four images built from an upstream commit
pass their inner build through `passthru`: `nix build .#ovn-image.ovn` builds
OVS and OVN, `nix build .#openstack-hypervisor-operator-image.manager` or
`.#kvm-node-agent-image.manager` the manager binary, and
`nix build .#keystone-federation-proxy-image.modAuthOpenidc` the
mod_auth_openidc module ([Source-built images](#source-built-images)).
`.#keystone-federation-proxy-image.modAuthMellon` is the stripped
mod_auth_mellon of the same image. `.#libvirt-image.libvirt` and
`.#libvirt-image.qemu` are the libvirt and the QEMU of the libvirt image. `<rel_>` is the release with its dot replaced
by `_`, since an unquoted flake attribute cannot contain a dot. The
directories under `nix/locks/` are the inventory of the Python images: one
output per `<release>/<name>/` directory.

On all four systems of the flake, `devShells.<system>.lock-tools` carries the
uv, yq, jq, git, Python 3.12 and bash 5 of `nixpkgs-images` that
`hack/nix-lock.sh` runs ([Python environments and locks](#python-environments-and-locks)),
and the pkg-config and libvirt that the setup script of the `libvirt-python`
sdist asks for before uv can read its metadata.

| File | Content |
| --- | --- |
| `flake.nix` | the inputs and outputs |
| `flake.lock` | the pinned revisions of the five inputs |
| `nix/lib/image-defaults.nix` | what every image shares: user and group 42424, the creation date `1970-01-01T00:00:01Z`, `PATH` and `LANG`, the licence and vendor labels |
| `nix/lib/fhs-layout.nix` | the FHS façade: `/var/lib/openstack`, `/bin/sh` and `/bin/bash` (bash), `/usr/bin/env`, the CA bundle at `/etc/ssl/certs/ca-certificates.crt`, and `/etc/passwd` and `/etc/group` with the `openstack` user. The Python environment is optional; without one, `/var/lib/openstack` is an empty directory. `extraRoots` adds store paths below `/var/lib/openstack`, `rootLinks` below the image root (`usr/share/novnc`) |
| `nix/lib/python-env.nix` | the uv2nix environment of one lock: the venv, `venvWith`, the venv with named dependency groups (`test`, `nova-compute`), and the patched source |
| `nix/lib/service-image.nix` | the image of a Python service or of tempest around that venv ([Service images](#service-images)) |
| `nix/lib/static-image.nix` | the image of a static Go binary, the stand-in for the distroless base of the Docker builds ([Source-built images](#source-built-images)) |
| `nix/lib/service-tests.nix` | the unit-test run and check of a lock |
| `nix/lib/subunit-crc.py` | recomputes the packet checksums of a subunit stream after `nuke-refs` |
| `nix/lib/rootwrap-conf.nix` | the text of nova-compute's `/etc/nova/rootwrap.conf`, with the image's store bin directories as `exec_dirs` |
| `nix/pkgs/ceph-bindings.nix` | the `rados` and `rbd` modules for Python 3.12, built from the ceph source of `nixpkgs-images` |
| `nix/pkgs/libvirt.nix` | nixpkgs' libvirt with `--sysconfdir=/etc` and `-Dqemu_datadir=/usr/share/qemu`, the one libvirt of both hypervisor images |
| `nix/base-requirements.txt` | the release-independent pins `hack/nix-lock.sh` adds to every service and tempest lock, `uwsgi` among them |
| `nix/locks/<release>/<name>/` | the lock of one service or tempest release, written by `hack/nix-lock.sh` |
| `images/<name>/image.nix` | the image; backup-shifter's, keystone-federation-proxy's and ovn's are functions of `pkgs` and `fhsLayout`, the hvo and kna images' of `pkgs` and `staticImage`, libvirt's of `pkgs` and `libvirt`, a Python image's of `serviceImage`, its environment `env` and what else it names (nova-compute's also `libvirt`) |
| `images/libvirt/qemu.nix` | the QEMU of the libvirt image |
| `images/<name>/source.json` | the pin of an image built from an upstream commit (ovn, keystone-federation-proxy, openstack-hypervisor-operator, kvm-node-agent), written by `hack/nix-lock.sh` |
| `images/ovn/ovn.nix` | OVS and OVN compiled from the commits of `images/ovn/source.json` |
| `images/keystone-federation-proxy/mod_auth_openidc.nix` | mod_auth_openidc compiled from the commit of `images/keystone-federation-proxy/source.json` |
| `images/keystone-federation-proxy/httpd-base.conf` | the Apache scaffold of the federation proxy, whose `LoadModule` lines `image.nix` fills with store paths |
| `images/<name>/manager.nix` | the manager binary of hvo or kna, built with `buildGo127Module` from the commit of its `source.json` with the patches under `images/<name>/patches/` |
| `images/<name>/patch-checks.sh` | the checks of those patches, which the `checkPhase` of `manager.nix` runs |

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
| `pyproject.toml` | a virtual uv project: the service as a git source at that commit with its `pip_extras`, the `pip_packages` of `releases/<release>/extra-packages.yaml`, the pins of `nix/base-requirements.txt`, the constraints of `releases/<release>/upper-constraints.txt` after `overrides/<release>/constraints.txt`, and a `test` dependency group; the nova lock also has a `nova-compute` group |
| `uv.lock` | what `uv lock` resolves from `pyproject.toml` |

The build fetches the source by commit and hash and applies the patches with
`pkgs.applyPatches`, never through the tag. The project name comes from the
fetched tree, so the placement lock names `openstack-placement`. The test
group holds `stestr` and `testtools` for a suite with a `.stestr.conf` and
`pytest` with the `hacking` pin of horizon's tox environment for the one
without, each unless `test-requirements.txt` lists it, followed by that
file's lines. `DERIVED_IMAGES` in `hack/nix-lock.sh` names the images built
from another key's source: the `nova-compute` block of
`extra-packages.yaml` becomes the `nova-compute` group of the nova lock, and
a release without that block stops the run with `nix-lock.sh: no block
'nova-compute' in releases/<release>/extra-packages.yaml` before anything is
written. The tempest lock pins the five keys of
`releases/<release>/test-refs.yaml` and the `openstack` client with its
placement and barbican plugins, and fetches nothing.

```bash
make nix-lock            # hack/nix-lock.sh --all in the lock-tools shell
make nix-lock-check      # hack/nix-lock.sh --check
hack/nix-lock.sh keystone 2026.1
```

`--all` writes every lock the release files pin, writes the hash of the noVNC
commit into `images/nova/novnc.json` and the hashes of the four image pins
`images/<name>/source.json`, including the OVS commit the `ovs` gitlink names
at the pinned OVN commit ([Source-built images](#source-built-images)), and
deletes every directory under `nix/locks/` the release files do not name.
`--check` runs `--all` and fails with `nix-lock.sh: the locks are stale; run
make nix-lock and commit the result` when `git status` reports a change under
`nix/locks/`, in `novnc.json` or in one of the four pins. The
`nix-lock-check` job of the build-images workflow runs it on every run.

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

## Source-built images

Four images compile upstream code at a pinned commit: `ovn` (Open vSwitch and
OVN), `keystone-federation-proxy` (mod_auth_openidc),
`openstack-hypervisor-operator` (hvo) and `kvm-node-agent` (kna). Each pin is
`images/<name>/source.json`, and `make nix-lock` writes every hash in it:

| Pin | Keys | Written by `make nix-lock` |
| --- | --- | --- |
| `images/ovn/source.json` | `version`, `rev`, `hash`, `ovs.rev`, `ovs.hash` | `hash`, `ovs.rev` and `ovs.hash` |
| `images/keystone-federation-proxy/source.json` | `version`, `rev`, `hash` | `hash` |
| `images/openstack-hypervisor-operator/source.json` | `rev`, `hash`, `vendorHash` | `hash` and `vendorHash` |
| `images/kvm-node-agent/source.json` | `rev`, `hash`, `vendorHash` | `hash` and `vendorHash` |

`hash` is the NAR hash of the tree at `rev`, and the build fetches by commit
and hash with `fetchFromGitHub`, never through a tag, so no token enters the
build. `hack/nix-lock.sh` reads `ovs.rev` from the `ovs` gitlink at the pinned
OVN commit, through a shallow, blobless fetch of that one commit. The vendor
hash of a Go pin is the hash of its module download; the generator recomputes
it only when `hash` moved or `vendorHash` is empty, from the hash mismatch the
`goModules` build prints for an empty one. A nixpkgs update that changes the
module download fails `build-hvo` or `build-kna` with a hash mismatch; empty
`vendorHash` and run `make nix-lock` to recover.

`images/ovn/ovn.nix` compiles OVS and then OVN against the OVS tree, with
`--prefix=$out --localstatedir=/var --sysconfdir=/etc`. The binaries and the
`ovs-ctl` and `ovn-ctl` scripts run from the store, while the compiled-in
runtime paths stay `/var/run/ovn`, `/var/run/openvswitch`,
`/var/log/openvswitch` and `/etc`. `images/ovn/image.nix` links
`/usr/share/ovn` and `/usr/share/openvswitch` into the store path through
`rootLinks`: `ovn-ctl` rewrites its own path to find `ovs-lib`, so both trees
sit side by side under `/usr/share`. The install drops the headers, the static
libraries, the man pages, `$out/etc` (a read-only `/etc/openvswitch` in the
image otherwise), the OVS Python package, the bugtool plugins and every script
whose first line names python3. The Docker image shipped those scripts without
a Python to run them, and `disallowedRequisites` fails the build on a
reference to python3 a later version adds. When `ovn-northd --version` names
another version than the pin, the `installCheckPhase` fails with this message,
the guard that the tag and the commit still agree:

```text
ovn.nix: images/ovn/source.json names <version>, but the tree at <rev> builds ovn-northd <built version>
```

The OVS and OVN testsuites do not run in the build; `verify_ovn.sh` is the
contract of the image.

`images/keystone-federation-proxy/mod_auth_openidc.nix` compiles
mod_auth_openidc, which `nixpkgs-images` does not package, against nixpkgs'
Apache with `--with-apxs` and `--with-hiredis`. The explicit `--with-hiredis`
makes configure fail when hiredis is missing. Brotli and jq claim expressions
stay off, as in the module the Docker image shipped. Upstream's `make check`
runs in the build. The `installPhase` copies `src/.libs/mod_auth_openidc.so`
to `$out/modules/`, because upstream's `make install` writes into the
read-only store path of httpd. stdenv strips `lib/` and `bin/` but not
`modules/`, and the debug information of an unstripped module keeps httpd's
`dev` output, every `-dev` input and gcc in the closure. `stripDebugList = [
"modules" ]` strips it, and `disallowedRequisites` fails the build when a
later nixpkgs brings those references back. `image.nix` gives nixpkgs'
mod_auth_mellon the same two attributes. When the `AC_INIT` line of
`configure.ac` names another version than the pin, `postPatch` fails with this
message:

```text
mod_auth_openidc.nix: images/keystone-federation-proxy/source.json names <version>, but configure.ac at <rev> declares AC_INIT([mod_auth_openidc],[<declared version>],[<address>])
```

The image is the FHS façade with coreutils and two root links.
`/usr/sbin/apache2`, the command the keystone operator runs, is nixpkgs'
`httpd`. `/etc/keystone-federation-proxy/httpd-base.conf` is the scaffold of
the image directory, whose `LoadModule` lines `replaceVars` fills with the
store paths of httpd and both modules; a placeholder it cannot fill fails the
build. nixpkgs builds every httpd module shared, so the scaffold loads
`mod_unixd`: without it `apache2 -t` passes and the server refuses to start
with `AH00136`. `/etc/keystone-federation-proxy/` is a real directory with
real `conf.d/` and `metadata/` mount points, so the container runtime can
create the `mellon/` mount point inside it. `/var/www/html` exists because
`apache2 -t` fails without the `DocumentRoot`, and `PATH` includes
`/usr/sbin`.

`images/<name>/manager.nix` builds the manager of hvo or kna with
`buildGo127Module`, since both upstream `go.mod` files name Go 1.27. The
patches under `images/<name>/patches/` apply to the module download and the
build alike, with `--fuzz=0`, so a hunk whose context changed fails the patch
phase. The ldflags are upstream's with the version set to `sha-<commit>`. The
`checkPhase` runs `images/<name>/patch-checks.sh` in the patched tree: hvo greps
for `servers.LiveMigrateOpts` outside the tests and the `vendor/` directory and
runs the tests of its four patches, kna runs the two tests of its patch. Each
check requires the test's top-level `--- PASS:` line, because a missing test
passes `go test` with no tests to run. kna's script refuses to run as root,
because `TestUpdateTLSCertificateKeyGroupNotPermitted` skips as root; Nix builds
as a build user. The unit tests under `tests/unit/images/` source the two
scripts.

`nix/lib/static-image.nix` builds the image both managers run in, a stand-in
for `gcr.io/distroless/static:nonroot`. It has the base's `/etc/passwd` and
`/etc/group`, with `root` (home `/root`), `nobody` and `nonroot` (65532), the
CA bundle, `/etc/services` and `/etc/protocols`, `/usr/share/zoneinfo`, `/tmp`
with mode 1777, `/root` with mode 0700 and `/home/nonroot`, and repeats the
base's configuration: `WorkingDir /home/nonroot` and `Env` with `PATH` and
`SSL_CERT_FILE`, plus the `Entrypoint` `/usr/bin/manager`. hvo runs as
`65532:65532`, the default, kna as `0:0`. `tests/lib/static_image.sh`, which
both verify scripts source, checks these files and the configuration.

```bash
nix build .#ovn-image && ./result | docker load
bash tests/container-images/verify_ovn.sh ovn:26.03.2

nix build .#keystone-federation-proxy-image && ./result | docker load
bash tests/container-images/verify_keystone_federation_proxy.sh keystone-federation-proxy:latest

nix build .#openstack-hypervisor-operator-image && ./result | docker load
bash tests/container-images/verify_hvo.sh openstack-hypervisor-operator:sha-<commit>

nix build .#kvm-node-agent-image && ./result | docker load
bash tests/container-images/verify_kna.sh kvm-node-agent:sha-<commit>

nix log .#openstack-hypervisor-operator-image.manager   # the PASS lines of the patch checks
```

`<commit>` is the `rev` of the image's `source.json`.

## Hypervisor images

`nova-compute` and `libvirt` run on the hypervisor nodes and share one libvirt
derivation. `nix/pkgs/libvirt.nix` overrides nixpkgs' libvirt, 12.2.0 at the
locked `nixpkgs-images`, in four meson flags:

| nixpkgs passes | The override passes | Effect |
| --- | --- | --- |
| `--sysconfdir=/var/lib` | `--sysconfdir=/etc` | libvirtd reads `/etc/libvirt/libvirtd.conf` and `/etc/libvirt/qemu.conf`, which the lab and the e2e suite write, keeps its secrets below `/etc/libvirt/secrets`, and finds its TLS files below `/etc/pki` |
| `-Dqemu_datadir=` | `-Dqemu_datadir=/usr/share/qemu` | libvirt reads QEMU's firmware descriptors from `/usr/share/qemu/firmware`, where the libvirt image links them |
| `-Dtests=enabled` | `-Dtests=disabled` | meson compiles none of libvirt's test programs, which the build would not run |
| `-Dexpensive_tests=enabled` | `-Dexpensive_tests=disabled` | meson refuses expensive tests without tests |

A nixpkgs revision that drops one of the flags fails evaluation with this
message:

```text
nix/pkgs/libvirt.nix: nixpkgs' libvirt no longer passes <flag>; re-check the override
```

libvirt's test suite is neither built nor run: Hydra ran it on the same
source. nixpkgs compiles root as the default QEMU user; the lab
and the e2e suite set `user = "root"` in `qemu.conf` themselves.

The libvirt image runs that derivation, and nova-compute's `libvirt-python`
builds against it: PyPI ships the binding as an sdist, and the
`libvirt-python` row of `nix/lib/python-env.nix` gives its `setup.py` the
pkg-config and libvirt it generates the binding from.
`checks.<system>.libvirt-pairing` compares the libvirt among the build inputs
of each nova lock's `libvirt-python` with `libvirt-image.libvirt`, by
derivation path. A nixpkgs libvirt wired into either side fails
`nix flake check --no-build`:

```text
libvirt-pairing: libvirt-python of nova <release> builds against <drv>, the libvirt image runs <drv>
```

`checks.<system>.libvirt-pairing-rejects-foreign` runs the same comparison
against nixpkgs' own libvirt and fails evaluation unless that throws, so
`libvirt-pairing` cannot pass by comparing nothing.

`images/libvirt/qemu.nix` overrides nixpkgs' QEMU, 10.2.4 at the locked
revision, to the `x86_64-softmmu` target, with the rbd block driver and
without libiscsi, curl, GTK, SDL, OpenGL, PulseAudio, PipeWire, JACK, SPICE
and smartcard support, and without the manual, whose Sphinx build the image
would not ship. nixpkgs builds QEMU without modules, so the drivers are
compiled in, and `qemu-img --help` lists rbd and none of curl, http, https,
ftp, ftps, iscsi, nfs or ssh. The rbd driver adds `ceph.lib`, about 1 GB, to
the closure. The UEFI firmware is QEMU's own edk2 build. `/usr/share/qemu`
holds QEMU's `share/qemu` with copies of the firmware descriptors that name
`/usr/share/qemu/<file>` instead of the store path, because libvirt records
the descriptor's path in a UEFI guest's XML and the next image has another
store path. `/usr/share/OVMF/OVMF_CODE_4M.fd` and `OVMF_VARS_4M.fd` link to
`edk2-x86_64-code.fd` and `edk2-i386-vars.fd`, the names noble's ovmf
package used.

The libvirt image writes its own root instead of `nix/lib/fhs-layout.nix`,
whose `/etc/passwd` adds the `openstack` user: it runs as root, carries the
`libvirt-qemu` user and the `libvirt` and `kvm` groups with noble's IDs, and
copies libvirt's default configuration into a writable `/etc/libvirt` without
the autostart link of the `default` network. Its tag is `latest`.

`images/nova-compute/image.nix` builds on `nix/lib/service-image.nix` with
`env.venvWith [ "nova-compute" ]`, the nova venv plus `libvirt-python`, so
neither the nova image nor nova's unit-test venv carries the binding. The
host tools are nixpkgs packages; root links put `/lib/udev/scsi_id`,
`/usr/bin/udevadm`, `/sbin/iscsiadm` and the `mount.nfs` helpers where
mount(8), os-brick and noble put them. `/etc/nova/rootwrap.conf` comes from
`nix/lib/rootwrap-conf.nix`, with the bin directory of every host-tool package
in `exec_dirs`, and `visudo -csf` checks the one sudo rule at build time.
nixpkgs' open-iscsi installs an initiator name it generated at build time;
the image removes it, as the Dockerfile removed the four identity files.
[Container Images](./container-images.md) lists the packages and links of
both images.

`merge-libvirt-image` gives the merged index a keeper tag on main,
`<libvirt-version>-r<N>` such as `12.2.0-r57`, where `N` counts the commits
under `images/libvirt`, `nix/pkgs/libvirt.nix`, `nix/lib/image-defaults.nix`
and `flake.lock`. A `nixpkgs-images` re-lock that rebuilds QEMU, Ceph or
glibc while libvirt keeps its version therefore gets a new tag. The lab
manifests pin the tag by digest, and Renovate moves the eight pins to each
new keeper, a libvirt major included; a pull request that changes the image
leaves them alone
([Dependency Management](../../contributing/dependency-management.md)).

```bash
nix build .#libvirt-image && ./result | docker load
bash tests/container-images/verify_libvirt.sh libvirt:latest

nix build .#nova-compute-2026_1-image && ./result | docker load
NOVA_COMPUTE_RELEASE=2026.1 bash tests/container-images/verify_nova_compute.sh nova-compute:2026.1
```

`cache.nixos.org` holds neither the libvirt override nor the QEMU override,
so the first build per `nixpkgs-images` revision compiles both; the Cachix
cache keeps them after that.

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

The same call builds `ovn-image` when the `changed-ovn` output of the
`changes` job is set, that is when the `image_ovn` filter matches (the ovn and
backup-shifter sources, the flake inputs, `nix/lib/` and the build script), or
when main has not published the pinned version yet. Otherwise the image map of `hack/ci-resolve-e2e-images.sh` names the
digest behind `ghcr.io/c5c3/ovn:<version>`, and the e2e jobs pull that image.
`libvirt-image` follows the same rule through `changed-libvirt`, set when
the `image_libvirt` filter matches (`images/libvirt/`,
`nix/pkgs/libvirt.nix`, the flake inputs, `nix/lib/` and the build script);
otherwise the map resolves `ghcr.io/c5c3/libvirt:latest` to the digest main
published, and `e2e-nova-libvirt` loads that image into kind as `latest`.
`keystone-federation-proxy-image` follows it through `changed-proxy`, set
when the `image_proxy` filter matches (`images/keystone-federation-proxy/`,
the flake inputs, `nix/lib/` and the build script), and the resolver passes
`BUILD_PROXY=true`. The stream loads as `keystone-federation-proxy:latest`,
and the `Tag federation proxy image` step tags it `:dev`, the tag the keystone
federation suites and the push step address. Otherwise the map resolves the
`:dev` reference to the digest behind `ghcr.io/c5c3/keystone-federation-proxy:latest`.
A `BUILD_SERVICE_IMAGES` line with a third field (`nova 2026.1 nova-compute`)
builds the attribute of that derived image, `nova-compute-2026_1-image`.

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

`.github/actions/build-nix-image/action.yaml` builds an image from a flake
attribute.

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
so a re-run of the push path is safe. The merge jobs use
`merge-manifest-and-attest`, which downloads the `digests-<image>-*`
artifacts, joins the platform digests into a manifest list, and attests and
signs it.

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
keystone 2026.1), which changes only with the lock. The ovn, hvo and kna
images add it too, fixed per pin: the OVN version (`26.03.2`) or
`sha-<commit>`. hvo and kna also carry `io.c5c3.upstream-commit`, the commit
of their pin, which `verify_hvo.sh` and `verify_kna.sh` check. The five labels
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
as one pull request on the Monday schedule of the golang and uv bumps, never
automerged; the re-lock is not a version bump, so the three-day release age
cannot gate it. A re-lock changes `flake.lock`, which the `base`, `ovn`, `proxy`,
`shifter`, `libvirt`, `hvo` and `kna` filters of the build-images workflow
cover, so its pull request rebuilds the images. Separate rules disable every update under
`nix/spike/**` and under `nix/locks/**`: the locks are derived files that only
`hack/nix-lock.sh` writes. `ignorePaths` also keeps `nix/locks/**` out of
extraction, and repeats the paths of the `config:recommended` preset, which a
repository value replaces.

A Renovate pull request that moves a pin the locks are derived from (under
`releases/`, `overrides/`, `patches/` or `nix/base-requirements.txt`,
the noVNC pin in `images/nova/novnc.json`, or one of the four
`images/<name>/source.json` pins) fails `nix-lock-check` until someone runs
`make nix-lock` on its branch and pushes the result. The OVN manager moves
`version` and `rev` together; a rule disables its digest updates, so a tag
moved upstream does not arrive as an ungrouped digest pull request. The
mod_auth_openidc manager of the federation proxy does the same, and its
minor and patch bumps wait three days and are never automerged. The hvo
and kna managers move `rev` as a digest of upstream main.
[Dependency Management](../../contributing/dependency-management.md) has the
details.

## Migration status

`tests/unit/docs/nix_image_build_status_table_test.sh` checks the table
against the tree: one row per `images/<name>/` directory and per operator
image, and a Build value that matches the files. For an image directory that
is `image.nix` for Nix and a `Dockerfile` for Docker, for an operator image
`operators/<op>/vendor.json` for Nix.

| Image | Build | Phase | Issue |
| --- | --- | --- | --- |
| `backup-shifter` | Nix | Phase 1 | [#1404](https://github.com/C5C3/cobaltcore/issues/1404) |
| `keystone` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `horizon` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `glance` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `placement` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `barbican` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `neutron` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `cinder` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `nova` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `tempest` | Nix | Phase 2 | [#1405](https://github.com/C5C3/cobaltcore/issues/1405) |
| `ovn` | Nix | Phase 3 | [#1406](https://github.com/C5C3/cobaltcore/issues/1406) |
| `openstack-hypervisor-operator` | Nix | Phase 3 | [#1406](https://github.com/C5C3/cobaltcore/issues/1406) |
| `kvm-node-agent` | Nix | Phase 3 | [#1406](https://github.com/C5C3/cobaltcore/issues/1406) |
| `nova-compute` | Nix | Phase 4 | [#1407](https://github.com/C5C3/cobaltcore/issues/1407) |
| `libvirt` | Nix | Phase 4 | [#1407](https://github.com/C5C3/cobaltcore/issues/1407) |
| `keystone-federation-proxy` | Nix | Phase 5 | [#1408](https://github.com/C5C3/cobaltcore/issues/1408) |
| `barbican-operator` | Nix | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `c5c3-operator` | Nix | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `cinder-operator` | Nix | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `glance-operator` | Nix | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `horizon-operator` | Nix | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `keystone-operator` | Nix | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `neutron-operator` | Nix | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `nova-operator` | Nix | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `ovn-operator` | Nix | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |
| `placement-operator` | Nix | Phase 6 | [#1409](https://github.com/C5C3/cobaltcore/issues/1409) |

:::
