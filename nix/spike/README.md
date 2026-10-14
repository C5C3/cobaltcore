<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Nix image-build spike

This directory is the spike of Phase 0 in [#1401]. It builds one example of
each shape the later phases repeat, and this file records what the builds
showed as decisions D1 to D12 plus the Go workspace choice Phase 6 defers to
the spike. Each section has four parts: the decision, the commands that
verified it, their output trimmed to the lines the decision rests on, and what
a later phase takes from it.

`flake.lock` holds `nixos-26.05` at commit
`a033e66ab94a6bedf2c3d58d93a36319d3cbbbaf` (last modified 2026-10-13). The
runs recorded here took place on 2026-10-14 on `x86_64-linux` with Nix 2.34.8
(sandbox on), Docker 29.9.0 and skopeo 1.13.3. Every command runs from the
repository root against the sub-flake, for example
`nix build ./nix/spike#keystone-image`, and needs
`experimental-features = nix-command flakes`. A flake in a git checkout sees
tracked or staged files only, and each evaluation copies the repository into
the store. Tools the host lacks come from the locked channel, for example
`nix shell github:NixOS/nixpkgs/a033e66ab94a6bedf2c3d58d93a36319d3cbbbaf#syft`.
No CI job builds this flake, and Renovate does not re-lock it.

| Path | Contents |
|---|---|
| `flake.nix` | the seven packages and the check listed under [Outputs](#outputs) |
| `generate-lock.sh` | writes `locks/<release>/<service>/{pyproject.toml,uv.lock,sources.json}` |
| `lib/python-env.nix` | the uv2nix venv of one service and release |
| `lib/fhs-layout.nix` | the FHS façade and the `openstack` user |
| `lib/image-defaults.nix` | the user, creation date, environment and labels every image shares |
| `lib/rootwrap-conf.nix` | nova's `rootwrap.conf` with store paths in `exec_dirs` |
| `pkgs/ceph-bindings.nix` | `rados` and `rbd` for Python 3.12 |
| `images/keystone.nix`, `images/nova-rootwrap.nix` | the two images |
| `checks/keystone-tests.nix` | the keystone unit tests as a check |
| `operators/keystone.nix` | the keystone operator, with and without `go.work` |

## D1: Python environment strategy

### Decision

uv2nix over pyproject.nix, with one generated virtual uv project and
`uv.lock` per service and release. The lock is resolved from
`upper-constraints.txt` after `overrides/<release>/constraints.txt`, the
extras and `pip_packages` of `extra-packages.yaml`, and the venv-builder pins.
Rejected: per-library derivations in the style of openstack-nix, which give up
`upper-constraints.txt`, and pyproject.nix without uv2nix, which has no reader
for `uv.lock`.

`lib/python-env.nix` prefers wheels. uv.lock records no build systems, so the
packages a spike output builds from source get theirs from one table in that
file. No sdist needed the fallback to a nixpkgs package of the same version.

| Package | Built from | Needs |
|---|---|---|
| keystone, nova, glance | the commit in `sources.json` | pbr and setuptools, `PBR_VERSION=<ref>`, a throwaway git commit of the tree |
| python-ldap 3.4.5 | sdist | setuptools, `openldap` and `cyrus_sasl`, `PYTHONDONTWRITEBYTECODE=1` |
| uwsgi 2.0.31 | sdist | setuptools |
| ovs 3.7.0 (nova) | sdist | setuptools |
| simplegeneric 0.8.1 (glance) | sdist | setuptools |
| testresources 2.0.2 (test group) | sdist | pbr and setuptools |

`PBR_VERSION` alone gives a broken keystone. keystone 29.0.0's
`pyproject.toml` names only the top-level package
(`[tool.setuptools] packages = ["keystone"]`), and pbr adds the subpackages as
package data from `git ls-files`. Without a git repository the wheel holds 12
files and `keystone.cmd` is missing; a git checkout of the tag gives 604. The
overlay therefore commits the fetched tree to a throwaway repository with a
fixed author and date before the build. python-ldap's `setup.cfg` has
setuptools byte-compile in a subprocess that imports `distutils`, which Python
3.12 removed; with bytecode writing disabled setuptools skips that step, and
the installer compiles bytecode itself.

### Verified by

```bash
nix build ./nix/spike#keystone-venv
./result/bin/keystone-manage --version
nix build --rebuild ./nix/spike#keystone-venv --option substituters ''
nix build ./nix/spike#glance-env
grep -c 'boto3 requires a scheme in endpoint_url' \
  result/lib/python3.12/site-packages/glance/common/store_utils.py
```

The `--rebuild` also ran on the keystone package derivation. Every build runs
in the sandbox without network access.

### Output

```text
29.0.0
checking outputs of '/nix/store/jm363payvz4mya0lmkmvripryhn6f7c7-keystone-29.0.0.drv'...
checking outputs of '/nix/store/fsbk5q5pc0gzxj2ny65ml144rb1h0mg2-cobaltcore-keystone-2026.1-env.drv'...
1
```

### Hands to

[#1405] takes `lib/python-env.nix` and its source-build table, which grows with
each service. A service whose `pyproject.toml` lists only its top-level package
needs the git commit as keystone does; the venv's `pbr.json` then records the
throwaway commit (`c1071f1` for keystone) instead of upstream's.

## D2: Lock regeneration on a Renovate bump

### Decision

A CI check first: it runs `generate-lock.sh` for every committed lock and fails
the pull request when `git diff --exit-code nix/spike/locks` (the locks
directory of the real generator) is not empty. Renovate `postUpgradeTasks`
come later, if at all.

### Verified by

```bash
bash nix/spike/generate-lock.sh keystone 2026.1
bash nix/spike/generate-lock.sh keystone 2026.1
git status --porcelain nix/spike/locks
bash nix/spike/generate-lock.sh keystone 2027.9; echo "exit=$?"
bash nix/spike/generate-lock.sh swift 2026.1; echo "exit=$?"
env PATH=/nonexistent /bin/bash nix/spike/generate-lock.sh keystone 2026.1; echo "exit=$?"
https_proxy=http://127.0.0.1:9 bash nix/spike/generate-lock.sh keystone 2026.1; echo "exit=$?"
bash tests/unit/images/nix_spike_generate_lock_test.sh
```

The second run printed the same line and left `git status` empty. The host
does not permit `unshare -rn`, so the no-network run points the proxy at a
closed port instead.

### Output

```text
generate-lock.sh: wrote nix/spike/locks/2026.1/keystone
generate-lock.sh: releases/2027.9/source-refs.yaml not found: is '2027.9' a release directory?
exit=1
generate-lock.sh: no key 'swift' in releases/2026.1/source-refs.yaml
exit=1
generate-lock.sh: required tool(s) not on PATH: uv nix yq jq
exit=1
       error: unable to download 'https://api.github.com/repos/openstack/keystone/commits/29.0.0': Could not connect to server (7) Failed to connect to api.github.com port 443 via 127.0.0.1 after 0 ms: Could not connect to server
exit=1
Results: 60 passed, 0 failed, 0 skipped
```

### Hands to

[#1405] builds the check and the real generator. Four things it has to handle:

- The committed locks were written by uv 0.11.21 from the locked nixpkgs; the
  check pins the same uv.
- Ten to twelve locked packages per service have no constraint and float with
  PyPI (keystone: bandit, certifi, flake8, hacking, mccabe, pycodestyle,
  pyflakes, setuptools; uwsgi is pinned by the venv-builder file), so a later
  PyPI release changes the lock without any input changing. `exclude-newer`
  in `[tool.uv]`, set from the release date, is one answer.
- Renovate's `pep621` manager reads the generated `pyproject.toml` files. A
  local `renovate --platform=local --dry-run=extract` lists the three files
  with the `uwsgi==2.0.31` pin, the git `rev` under `[tool.uv.sources]` and
  the test-group ranges; it skips `constraint-dependencies`. A Renovate bump
  of a derived file would bypass the generator. A `packageRules` entry in
  `renovate.json` disables every update under `nix/spike/`; the real
  generator's locks need the same.
- `nix flake prefetch` calls the GitHub API; unauthenticated runs can hit its
  rate limit, which `access-tokens` in `nix.conf` lifts.

## D3: Source fetching and hashes

### Decision

`sources.json` beside each lock holds the ref, the commit the ref names, the
NAR hash of its tree and the repo-relative patch paths. The build fetches by
commit and hash with `fetchFromGitHub`, never through the tag.
`source-refs.yaml` stays the file Renovate tracks, and the lock step translates
it with `yq`, so neither a JSON mirror nor a move to TOML is needed.

The generator prefetches with an empty `XDG_CACHE_HOME`. With its usual cache,
an unreachable GitHub makes `nix flake prefetch` warn and answer with the
earlier resolution of the tag ("using cached version").

### Verified by

```bash
jq . nix/spike/locks/2026.1/keystone/sources.json
jq .patches nix/spike/locks/2026.1/glance/sources.json
nix build ./nix/spike#keystone-venv ./nix/spike#glance-env
```

The two builds fetch with the hashes `nix flake prefetch` reported and report
no hash mismatch.

### Output

```text
{
  "ref": "29.0.0",
  "rev": "3526925ae34a66e626bec985628a421a7694168e",
  "hash": "sha256-QrbLn7uH/BrKM6jhFjQ2fIGeA1O2BOo6sLsTvPAT21E=",
  "patches": []
}
[
  "patches/glance/2026.1/0001-normalize-scheme-prefixed-s3-host-in-location-repair.patch"
]
```

### Hands to

[#1405] keeps the `sources.json` format and writes it from the real generator.
The ref-to-commit pinning answers the mutable-tag concern that
`images/ovn/Dockerfile` handles with `OVN_COMMIT`.

## D4: nixpkgs channel for the images

### Decision

The root flake gets two inputs: the dev shell stays on `nixos-unstable`, the
images move to `nixos-26.05`. Every attribute the images need exists on the
stable channel, so there is no exception.

### Verified by

```bash
for rev in a033e66ab94a6bedf2c3d58d93a36319d3cbbbaf 56c02bc00adcf003215cc4bd996d6efaf4cff188; do
  for attr in python312 libvirt qemu ovn ceph uwsgi uv apacheHttpdPackages.mod_wsgi3 \
      go_1_27 skopeo syft sbomnix vulnix; do
    nix eval --raw "github:NixOS/nixpkgs/$rev#$attr.version"
  done
  nix eval --json "github:NixOS/nixpkgs/$rev#ceph.outputs"
  nix eval --raw "github:NixOS/nixpkgs/$rev#ceph.pythonEnv.name"
  for fn in streamLayeredImage fakeNss caCertificates; do
    nix eval "github:NixOS/nixpkgs/$rev#dockerTools.$fn" --apply 'x: builtins.typeOf x'
  done
done
```

### Output

| Attribute | `nixos-26.05` (`a033e66a`, 2026-10-13) | `nixos-unstable` (`56c02bc0`, root `flake.lock`) |
|---|---|---|
| `python312` | 3.12.14 | 3.12.14 |
| `libvirt` | 12.2.0 | 12.6.0 |
| `qemu` | 10.2.4 | 11.1.0 |
| `ovn` | 26.03.3 | 26.03.2 |
| `ceph` | 20.2.4 | 20.2.3 |
| `ceph.outputs` | out, lib, client, dev, doc, man | out, lib, client, dev, doc, man |
| `ceph.pythonEnv` | python3-3.12.14-env | python3-3.12.14-env |
| `uwsgi` | 2.0.31 | 2.0.31 |
| `uv` | 0.11.21 | 0.12.5 |
| `apacheHttpdPackages.mod_wsgi3` | 5.0.2 | 5.0.2 |
| `go_1_27` | 1.27.2 | 1.27.0 |
| `skopeo` | 1.22.2 | 1.24.0 |
| `syft` | 1.44.0 | 1.51.0 |
| `sbomnix` | 1.7.6 | 1.8.0 |
| `vulnix` | 1.12.4 | 1.12.5 |
| `dockerTools.streamLayeredImage` | set (overridable function) | set (overridable function) |
| `dockerTools.fakeNss` | set (derivation) | set (derivation) |
| `dockerTools.caCertificates` | set (derivation) | set (derivation) |

### Hands to

[#1404] adds the second input. Two rows matter beyond the channel choice: the
stable `libvirt` and `qemu` are older than unstable's and newer than noble's
libvirt 10.0.0 ([#1407]), and the unstable `go_1_27` the dev shell pins today is
1.27.0, below the `go 1.27.2` directive of `go.work`, while stable carries
1.27.2.

## D5: Binary cache

### Decision

Cachix, with one cache named for the repository. The Cachix auth token is a
repository secret that only the push path and pull requests from the same
repository receive. Fork pull requests read the public cache and push nothing,
the split the `Reject fork PRs` steps of `.github/workflows/build-images.yaml`
enforce today. Cachix signs with its own key, so no signing key is held in the
repository.

### Verified by

```bash
for p in keystone-venv keystone-image nova-rootwrap-image ceph-bindings glance-env \
    keystone-operator-modroot keystone-operator-workvendor \
    checks.x86_64-linux.keystone-2026_1-tests; do
  nix path-info -S "./nix/spike#$p"
done
```

### Output

| Output | Closure size |
|---|---|
| `keystone-venv` | 369,352,624 bytes (352.2 MiB) |
| `keystone-image` | 514,286,712 bytes (490.5 MiB) |
| `nova-rootwrap-image` | 1,627,660,160 bytes (1552.3 MiB) |
| `ceph-bindings` | 1,160,800,560 bytes (1107.0 MiB) |
| `glance-env` | 410,312,344 bytes (391.3 MiB) |
| `keystone-operator-modroot` | 50,806,088 bytes (48.5 MiB) |
| `keystone-operator-workvendor` | 50,818,888 bytes (48.5 MiB) |
| `checks.x86_64-linux.keystone-2026_1-tests` | 104,961,168 bytes (100.1 MiB) |

The closures overlap. Their union is 339 store paths and 1,921,184,320 bytes
(1.8 GiB), of which `ceph.lib` alone brings 1,024,940,888 bytes (D9).

### Hands to

[#1404] creates the cache, the secret and the substituter configuration. The
union above is the first sizing input for the Cachix plan; it counts runtime
closures only, and a cache that also receives fetched sources and wheels grows
beyond it. The check's closure is its own output, nearly all of it the subunit
stream: `nuke-refs` blanks the store paths its tracebacks name, which Nix would
otherwise record as references to the test venv and the interpreter.

## D6: Image assembly

### Decision

`dockerTools.streamLayeredImage` with `created = "1970-01-01T00:00:01Z"`,
loaded with `docker load` or pushed with `skopeo copy docker-archive:`.
`nix2container` stays the fallback if CI time demands it.

### Verified by

```bash
nix build ./nix/spike#keystone-image
./result > keystone.tar
ls -l keystone.tar
tar -xOf keystone.tar manifest.json | jq '.[0].Layers | length'
```

### Output

```text
-rw-rw-r-- 1 claude claude 408903680 Oct 14 20:44 keystone.tar
100
```

The keystone stream is 408,903,680 bytes (390 MiB) in 100 layers: 99
store-path layers and the customisation layer, the `maxLayers` default. The
nova stream is 1,550,561,280 bytes, also in 100 layers.

### Hands to

[#1404] builds the `build-nix-image` action on `streamLayeredImage`.

## D7: The FHS façade

### Decision

`lib/fhs-layout.nix` gives every image the in-image paths as symlinks into the
store: `/var/lib/openstack` points at a `symlinkJoin` of the venv and the
`extraRoots` entries, `/bin/sh` and `/bin/bash` at bash, `/usr/bin/env` at
coreutils, and `/etc/ssl/certs/ca-certificates.crt` comes from
`dockerTools.caCertificates`. `fakeNss` carries the `openstack` user and group
(42424). The operators change nothing.

The venv and the hand-written `keystone-wsgi-public` reach the image through
`/var/lib/openstack` only. As `contents` entries the venv would also link its
`bin/`, `lib/` and `pyvenv.cfg` into the image root, and the script could not
merge below the symlinked `/var/lib/openstack`; it is an `extraRoots` entry.

### Verified by

```bash
grep -rhoE '"/(var/lib/openstack|usr/share|etc|bin)/[A-Za-z0-9_./-]*' operators \
  --include='*.go' --exclude='*_test.go' | sort | uniq -c | sort -rn
nix build ./nix/spike#keystone-image && ./result | docker load
bash tests/container-images/verify_keystone.sh spike/keystone:2026.1
docker run --rm spike/keystone:2026.1 sh -c 'readlink -f /var/lib/openstack/bin/keystone-wsgi-public;
  test -x /bin/bash; test -f /etc/ssl/certs/ca-certificates.crt; cat /etc/passwd'
```

### Output

The operator grep prints 67 paths, here in its order with a class each. An
image-owned path needs the façade, an `extraRoots` entry or an image-specific
link. The operator projects every mount point, and the image needs nothing
for it. The Dockerfiles write the five `/var/lib/openstack/bin/*` scripts
today; the spike's keystone image adds `keystone-wsgi-public` as an
`extraRoots` entry.

| Count | Path | Class |
|---|---|---|
| 17 | `"/bin/sh` | image-owned: façade |
| 7 | `"/etc/keystone/keystone.conf.d/` | mount point |
| 7 | `"/bin/bash` | image-owned: façade |
| 3 | `"/etc/rabbitmq-ca` | mount point |
| 2 | `"/etc/ovn/tls` | mount point |
| 2 | `"/etc/nvme` | mount point: host path |
| 2 | `"/etc/multipath.conf` | mount point: host path |
| 2 | `"/etc/multipath` | mount point: host path |
| 2 | `"/etc/keystone/fernet-keys/` | mount point |
| 2 | `"/etc/keystone/fernet-keys` | mount point |
| 2 | `"/etc/keystone/credential-keys` | mount point |
| 2 | `"/etc/keystone/` | mount point |
| 2 | `"/etc/iscsi` | mount point: host path |
| 1 | `"/var/lib/openstack/horizon-static` | image-owned: an `extraRoots` entry |
| 1 | `"/var/lib/openstack/etc/nova/api-paste.ini` | image-owned: data file of the venv |
| 1 | `"/var/lib/openstack/etc/neutron/api-paste.ini` | image-owned: data file of the venv |
| 1 | `"/var/lib/openstack/etc/glance/metadefs` | image-owned: data file of the venv |
| 1 | `"/var/lib/openstack/etc/cinder/resource_filters.json` | image-owned: data file of the venv |
| 1 | `"/var/lib/openstack/etc/cinder/api-paste.ini` | image-owned: data file of the venv |
| 1 | `"/var/lib/openstack/bin/placement-api` | image-owned: an `extraRoots` script |
| 1 | `"/var/lib/openstack/bin/nova-amqp-ready` | image-owned: an `extraRoots` script |
| 1 | `"/var/lib/openstack/bin/keystone-wsgi-public` | image-owned: an `extraRoots` script |
| 1 | `"/var/lib/openstack/bin/glance-wsgi-api` | image-owned: an `extraRoots` script |
| 1 | `"/var/lib/openstack/bin/cinder-amqp-ready` | image-owned: an `extraRoots` script |
| 1 | `"/usr/share/ovn/scripts/ovn-ctl` | image-owned: link outside `/var/lib/openstack` |
| 1 | `"/usr/share/novnc` | image-owned: link outside `/var/lib/openstack` |
| 1 | `"/etc/placement/` | mount point |
| 1 | `"/etc/placement-db-tls/` | mount point |
| 1 | `"/etc/ovn-chassis/nodes/` | mount point |
| 1 | `"/etc/ovn-chassis/nodes` | mount point |
| 1 | `"/etc/ovn-chassis/bin` | mount point |
| 1 | `"/etc/ovn-central/bin` | mount point |
| 1 | `"/etc/openstack-dashboard/` | mount point |
| 1 | `"/etc/nova/nova.conf.d` | mount point |
| 1 | `"/etc/nova/compute-pool.conf.d` | mount point |
| 1 | `"/etc/nova/compute-config` | mount point |
| 1 | `"/etc/nova/` | mount point |
| 1 | `"/etc/nova-metadata-ca` | mount point |
| 1 | `"/etc/nova-db-tls/cell/` | mount point |
| 1 | `"/etc/nova-db-tls/api/` | mount point |
| 1 | `"/etc/neutron-db-tls/` | mount point |
| 1 | `"/etc/neutron` | mount point |
| 1 | `"/etc/keystone/keystone.conf.d/sso_callback_template.html` | mount point |
| 1 | `"/etc/keystone/keystone.conf.d/policy.yaml` | mount point |
| 1 | `"/etc/keystone/keystone.conf.d/logging.conf` | mount point |
| 1 | `"/etc/keystone/keystone.conf.d/api-paste.ini` | mount point |
| 1 | `"/etc/keystone/domains` | mount point |
| 1 | `"/etc/keystone/db-tls/` | mount point |
| 1 | `"/etc/keystone/credential-keys/` | mount point |
| 1 | `"/etc/keystone-federation-proxy/metadata` | mount point |
| 1 | `"/etc/keystone-federation-proxy/mellon` | mount point |
| 1 | `"/etc/keystone-federation-proxy/httpd-base.conf` | image-owned: file of the proxy image |
| 1 | `"/etc/keystone-federation-proxy/conf.d` | mount point |
| 1 | `"/etc/glance/healthcheck_disable` | read when present; the image ships none |
| 1 | `"/etc/glance/glance-api.conf.d/` | mount point |
| 1 | `"/etc/glance/db-tls/` | mount point |
| 1 | `"/etc/glance/backends.conf.d/` | mount point |
| 1 | `"/etc/cinder/volume.conf.d` | mount point |
| 1 | `"/etc/cinder/scheduler.conf.d` | mount point |
| 1 | `"/etc/cinder/cinder.conf.d` | mount point |
| 1 | `"/etc/cinder/backup.conf.d` | mount point |
| 1 | `"/etc/cinder/backends.conf.d` | mount point |
| 1 | `"/etc/cinder-db-tls/` | mount point |
| 1 | `"/etc/ceph` | mount point |
| 1 | `"/etc/barbican/` | mount point |
| 1 | `"/etc/barbican-secret-store-ca/` | mount point |
| 1 | `"/etc/barbican-db-tls/` | mount point |

```text
Test: keystone-manage --version succeeds
  PASS: keystone-manage --version exits 0
  PASS: version output is non-empty
Test: container runs as openstack user
  PASS: whoami exits 0
  PASS: whoami outputs openstack
Test: no build tools in final image
  PASS: gcc not found
  PASS: python3-dev not installed
  PASS: uv not found
Test: runtime apt packages are installed
  FAIL: dpkg -l exits 0
    expected: 0
    actual:   127
=== Results: 7 passed, 5 failed ===

/nix/store/9ddb1yp0rbs35mqpf0nz5imls3qax7kh-keystone-wsgi-public
root:x:0:0:root user:/var/empty:/bin/sh
openstack:x:42424:42424::/var/lib/openstack:/usr/sbin/nologin
nobody:x:65534:65534:nobody:/var/empty:/bin/sh
```

The four other failures of test 4 are the four apt package names it looks for
in the missing `dpkg -l` output.

### Hands to

[#1405] adds the `extraRoots` entries of each service (`horizon-static`, the
WSGI scripts uv does not generate) and rewrites test 4 of `verify_keystone.sh`
to probe binaries and modules. The façade has no parameter yet for links outside
`/var/lib/openstack`; `/usr/share/novnc` ([#1405]) and
`/usr/share/ovn/scripts/ovn-ctl` ([#1407]) need one.

## D8: Users, sudo and rootwrap

### Decision

The nova image copies `${pkgs.sudo}/bin/sudo` to `/usr/bin/sudo` with mode 4755
in `fakeRootCommands` with `enableFakechroot`, writes `/etc/sudoers`
(`root ALL=(ALL) ALL` and `@includedir /etc/sudoers.d`), installs
`images/nova-compute/sudoers` verbatim as `/etc/sudoers.d/nova-compute` with
mode 0440 and runs `visudo -csf` on it, gives sudo a PAM stack and creates
`/run/sudo/ts`. `lib/rootwrap-conf.nix` writes `/etc/nova/rootwrap.conf` with
`/var/lib/openstack/bin` and store `bin` directories in `exec_dirs`, and
refuses an empty `execDirs`. nixpkgs' sudo reads `/etc/sudo.conf` and
`/etc/sudoers`, and its PAM library resolves bare module names below its own
`lib/security`.

### Verified by

```bash
nix build ./nix/spike#nova-rootwrap-image && ./result | docker load
bash tests/container-images/verify_nova_compute.sh spike/nova-rootwrap:2026.1
docker run --rm spike/nova-rootwrap:2026.1 cat /etc/nova/rootwrap.conf
docker run --rm spike/nova-rootwrap:2026.1 stat -c '%a %U' /usr/bin/sudo /etc/sudoers.d/nova-compute
nix eval --impure --expr 'import ./nix/spike/lib/rootwrap-conf.nix { filtersPath = "/x"; execDirs = [ ]; }'
printf 'openstack ALL = (root) NOPASSWD /var/lib/openstack/bin/nova-rootwrap\n' > /tmp/spike-scratch/bad-sudoers
visudo -csf /tmp/spike-scratch/bad-sudoers
docker run --name pam-probe --user 0 spike/nova-rootwrap:2026.1 sh -c \
  'echo "openstack ALL=(root) /var/lib/openstack/bin/python" > /etc/sudoers.d/zz-probe; chmod 0440 /etc/sudoers.d/zz-probe'
docker commit pam-probe spike/nova-rootwrap:pam-probe
docker run --rm --user 42424:42424 spike/nova-rootwrap:pam-probe \
  sudo -S /var/lib/openstack/bin/python -c 'import os; print(os.geteuid())' </dev/null
```

The last three commands add a sudoers rule without `NOPASSWD` and run it as
the `openstack` user with an empty password.

### Output

| Test of `verify_nova_compute.sh` | Result | Owner |
|---|---|---|
| 1, the compute console scripts ship and nova-compute runs | 2 PASS | spike |
| 2, the libvirt binding imports at the release's pin | 3 FAIL, no libvirt binding | [#1407] |
| 3, the libvirt driver and the host libraries import | 1 FAIL, os-brick runs `grep` on `/sbin/iscsiadm` at import and the image has neither | [#1407] |
| 4, the host tools run | 11 FAIL, no host tool ships | [#1407] |
| 10, the Ceph client libraries and tools are wired | 6 PASS; 5 FAIL: the two "comes from the apt package" checks, `rbd help export`, `rbd help import`, `ceph --version` | [#1405] (dist-packages), [#1407] (tools) |
| 5, no node identity is baked into the image | 5 PASS | spike |
| 6, nova's default root helper resolves and is confined | 18 PASS | spike |
| 7, container runs as openstack user | 2 PASS | spike |
| 8, no build tools in final image | 5 PASS | spike |
| 9, the state directories ship empty and owned by 42424 | 3 FAIL, no `/var/lib/nova` and no `find` | [#1407] |

```text
Test: nova's default root helper resolves and is confined
  PASS: rootwrap.conf points filters_path at the installed filters
  PASS: rootwrap.conf searches /var/lib/openstack/bin first
  PASS: compute.filters is installed
  PASS: the writability check runs
  PASS: /etc/nova/rootwrap.conf is not writable by the service user
  PASS: /var/lib/openstack/etc/nova/rootwrap.d is not writable by the service user
  PASS: /var/lib/openstack/etc/nova/rootwrap.d/compute.filters is not writable by the service user
  PASS: /var/lib/openstack/bin is not writable by the service user
  PASS: /var/lib/openstack/bin/privsep-helper is not writable by the service user
  PASS: /etc/sudoers.d/nova-compute is not writable by the service user
  PASS: sudo -l lists the nova-rootwrap rule
  PASS: sudo -l lists exactly one NOPASSWD entry
  PASS: sudo -n true is refused
  PASS: sudo nova-rootwrap refuses id with exit 99
  PASS: rootwrap names the refused command
  PASS: privsep-helper runs through rootwrap and exits 1
  PASS: privsep-helper reads its configuration as root
  PASS: as root, sudo nova-rootwrap refuses id with exit 99
Test: container runs as openstack user
  PASS: whoami exits 0
  PASS: whoami outputs openstack
=== Results: 38 passed, 23 failed ===

# Written by nix/spike/lib/rootwrap-conf.nix.
[DEFAULT]
filters_path=/var/lib/openstack/etc/nova/rootwrap.d
exec_dirs=/var/lib/openstack/bin,/nix/store/1gxjykhjly1h91sijz5rawb285vab2y0-cobaltcore-nova-2026.1-env/bin,/nix/store/bhv6xyvqw6gjs3j488gi1z6wdv18bdpa-coreutils-9.11/bin

4755 root
440 root

error: rootwrap-conf: execDirs is empty; rootwrap resolves every filtered command through exec_dirs, so name at least the store bin directory of the tools the filters allow

/tmp/spike-scratch/bad-sudoers:1:69: syntax error

Sorry, try again.
Sorry, try again.
sudo: 3 incorrect password attempts
```

`visudo` exits 1 on the malformed rule; the fakeroot step runs under `bash -e`,
so a malformed `sudoers` fails the image build. The PAM stack denies the auth
phase, so the rule without `NOPASSWD` exits 1 instead of running as root, while
the `NOPASSWD` rule of test 6 skips that phase.

### Hands to

[#1407] takes the sudo and rootwrap shape for nova-compute and adds what
tests 2, 3, 4, 9 and the tool parts of 10 expect. The PAM stack here is
`auth required pam_deny.so`, `account sufficient pam_permit.so` and
`session required pam_permit.so`; the permitted account phase is a spike
shortcut. #1407 decides whether to run the `pam_unix` account check that
noble's `/etc/pam.d/sudo` runs through `common-account`.

## D9: Ceph bindings

### Decision

`pkgs/ceph-bindings.nix` builds only `src/pybind/rados` and `src/pybind/rbd`
from `ceph.src` against the prebuilt `ceph.lib` and the `ceph.dev` headers. An
image puts the modules at `/var/lib/openstack/ceph-bindings` and a
`ceph-bindings.pth` names that directory. The `.pth` is written into the venv
itself: pyproject.nix's `bin/python` is a wrapper that sets `argv[0]` to the
venv's store path, so `sys.prefix` is the store venv and a `.pth` placed only
below the façade's `/var/lib/openstack` is never read.

nixpkgs' ceph is built for Python 3.12 at this revision
(`ceph-python = python312` in `pkgs/by-name/ce/ceph/scope.nix`), not 3.14, and
`ceph.out` ships `rados.cpython-312-x86_64-linux-gnu.so` and
`rbd.cpython-312-x86_64-linux-gnu.so`. The separate derivation stays: its
closure is 1,160,800,560 bytes against 2,190,443,424 for `ceph.out`, and the
image's Python does not follow nixpkgs' choice for ceph. The build needs
`CPPFLAGS=-iquote <source>/src/include` for `err.h`, as ceph's
`cmake/modules/Distutils.cmake` passes it.

### Verified by

```bash
nix build ./nix/spike#ceph-bindings && ls result/lib/python3.12/site-packages/
docker run --rm spike/nova-rootwrap:2026.1 /var/lib/openstack/bin/python \
  -c 'import rados, rbd; print(rados.__file__)'
nix path-info -S ./nix/spike#ceph-bindings
nix path-info -S /nix/store/i7pxk5689ybcxbpdzf89x9b8qkfagx9x-ceph-20.2.4-lib
```

The bytes the bindings add to the nova image are the closure of the image
minus the closure of every image path that does not reach `ceph-bindings`.

### Output

```text
rados.cpython-312-x86_64-linux-gnu.so
rbd.cpython-312-x86_64-linux-gnu.so
/var/lib/openstack/ceph-bindings/rados.cpython-312-x86_64-linux-gnu.so
/nix/store/zrzxy24337qwb76lffp6m9xgi8ygy8ag-python3.12-ceph-bindings-20.2.4	 1160800560
/nix/store/i7pxk5689ybcxbpdzf89x9b8qkfagx9x-ceph-20.2.4-lib	 1024940888
```

The bindings add 112 store paths and 938,907,744 bytes to the nova image's
closure of 1,627,660,160 bytes. `ceph.lib` is one output of 371.5 MiB with all
of ceph's libraries; through it come google-cloud-cpp, arrow-cpp, grpc,
rocksdb and boost, although the modules use only librados and librbd.

### Hands to

[#1405] adds the bindings to glance and cinder and changes the
dist-packages assertion of `assert_ceph_bindings_wired` in
`tests/lib/ceph_bindings.sh`, which checks noble's
`/usr/lib/python3/dist-packages` and fails on any Nix image. [#1407] does the
same for nova-compute. A slimmer client-library output of ceph would remove
most of the 938,907,744 bytes.

## D10: SBOM, scanning, signing

### Decision

syft stays the SBOM source in `supply-chain-attest`, because it emits the PyPI
PURLs Grype matches. `vulnix` runs beside Grype for the nixpkgs-sourced
closure, with `--closure` so it scans runtime paths only. cosign signing and
the two attestations stay as they are. `sbomnix` 1.7.6 has no `--type` option
and errors on it; without it (runtime is its default) it lists store paths as
`pkg:nix` components only, without the PyPI PURLs that carry 51 of Grype's 70
matches.

### Verified by

```bash
./result > keystone.tar   # result from nix build ./nix/spike#keystone-image
syft scan docker-archive:keystone.tar -o cyclonedx-json=keystone.cdx.json
jq '.components | length' keystone.cdx.json
jq '[.components[].purl // "" | select(startswith("pkg:pypi/"))] | length' keystone.cdx.json
sbomnix "$(nix path-info ./nix/spike#keystone-image)" --type runtime --cdx keystone.sbomnix.cdx.json
sbomnix "$(nix path-info ./nix/spike#keystone-image)" --cdx keystone.sbomnix.cdx.json
jq '.components | length' keystone.sbomnix.cdx.json
grype sbom:keystone.cdx.json -o json --file keystone.grype.json
jq '.matches | length' keystone.grype.json
vulnix --json "$(nix path-info ./nix/spike#keystone-image)"
vulnix --closure --json "$(nix path-info ./nix/spike#keystone-image)"
```

syft 1.44.0, sbomnix 1.7.6 and vulnix 1.12.4 come from the locked channel,
grype 0.112.0 as well.

### Output

```text
605
117
sbomnix: error: unrecognized arguments: --type runtime
151
70
```

| Tool | Count |
|---|---|
| syft, CycloneDX components | 605 (335 file, 259 library, 11 application) |
| syft, `pkg:pypi/` PURLs | 117 (also 142 `pkg:nix/`, 3 generic) |
| sbomnix, components | 151, all `pkg:nix/` |
| grype on the syft SBOM, matches | 70 (2 critical, 27 high, 34 medium, 6 low, 1 negligible; 51 PyPI, 16 generic, 3 nix) |
| vulnix, findings | 182 in 47 derivations without `--closure`, which scans the build-time closure (cargo, gcc 4.6.4, go bootstrap); 55 in 16 derivations with `--closure` |

vulnix exits 2 when it finds a vulnerability.

### Hands to

[#1404] adds vulnix with `--closure` to the image pipeline beside Grype and
leaves `.github/actions/supply-chain-attest/action.yaml` as it is.

## D11: Unit tests of the services

### Decision

`checks.x86_64-linux.keystone-2026_1-tests` runs stestr over the patched source
with the venv that carries the test dependency group, offline in the sandbox.
`releases/2026.1/test-excludes/keystone.txt` applies.
A test that fails only in the sandbox goes into the check's own exclude list
with its error; there is one:

| Test | Error in the sandbox |
|---|---|
| `keystone.tests.unit.test_v3_federation.SAMLGenerationTests.test_saml_signing` | `FileNotFoundError: [Errno 2] No such file or directory: 'which'`; `_is_xmlsec1_installed()` probes with `which`, and CI skips the test with "xmlsec1 is not installed" |

With `OS_TEST_DBAPI_ADMIN_CONNECTION` unset, oslo.db's opportunistic MySQL and
PostgreSQL tests skip in the sandbox, as in a run of
`hack/ci-run-unit-tests.sh` with the variable empty, and nothing replaces them
in the check. The `test-service-images` job sets the variable to SQLite,
MySQL and PostgreSQL service containers, so CI runs those tests.

### Verified by

```bash
nix build ./nix/spike#checks.x86_64-linux.keystone-2026_1-tests
cat result/summary.txt
test -s result/testresults.subunit
gh run view 38349763210 --repo C5C3/cobaltcore --job 115141902731 --log |
  grep -E 'Ran: |- (Passed|Skipped|Expected Fail|Failed):'
```

### Output

```text
Totals
======
Ran: 5747 tests in 115.9271 sec.
 - Passed: 5031
 - Skipped: 714
 - Expected Fail: 2
 - Unexpected Success: 0
 - Failed: 0
Sum of execute time for each test: 1780.0878 sec.
```

| Count | Check | `test-service-images (keystone, 2026.1)` on `main` |
|---|---|---|
| Ran | 5747 | 5748 |
| Passed | 5031 | 5153 |
| Skipped | 714 | 593 |
| Expected fail | 2 | 2 |
| Failed | 0 | 0 |

The CI run is [run 38349763210, job 115141902731](https://github.com/C5C3/cobaltcore/actions/runs/38349763210/job/115141902731)
of 2026-10-13. The difference: the check excludes `test_saml_signing`, which
CI skips, and skips 63 MySQL and 63 PostgreSQL tests ("Backend ... is
unavailable: Could not connect") that CI runs; 4 of those skip in CI too.

### Hands to

[#1405] runs one such check per service and release and exports the subunit
stream as `test-service-images` does. Keeping the opportunistic database tests
needs the job's MySQL and PostgreSQL services outside the sandbox, which a
flake check cannot reach.

## D12: Tags and keeper schemes

### Decision

The image and the venv are reproducible: `--rebuild` finds no difference, two
loads of the stream give one image ID, and two pushes to empty registries give
one manifest digest. The service tags stay as
`.github/actions/derive-service-tags/action.yaml` computes them
(`<version>-p<N>-<branch>-<sha>`, `<version>`, `<sha>`, `<release>`). The
libvirt keeper tag becomes `<libvirt-version>-r<N>`, with the version from
`pkgs.libvirt.version` and N the number of commits under the libvirt
derivation's directory, as `hack/ci-tag-libvirt-keeper.sh` counts the commits
under `images/libvirt` today. Its three coupled copies
(`hack/ci-tag-libvirt-keeper.sh:109`, `hack/ghcr-prune-stale-versions.py:72`,
`renovate.json:1006`) then share the regex `^[0-9]+(\.[0-9]+)+-r[0-9]+$`.

### Verified by

```bash
nix build --rebuild ./nix/spike#keystone-image
nix build --rebuild ./nix/spike#keystone-venv
drv=$(nix path-info --derivation ./nix/spike#keystone-image)
for name in keystone-customisation-layer keystone-conf.json keystone-29.0.0 uwsgi-2.0.31 python-ldap-3.4.5; do
  nix build --rebuild --no-link "$(nix-store -qR "$drv" | grep -E "/[a-z0-9]{32}-$name\.drv$")^*"
done
./result | docker load; docker images --no-trunc --format '{{.ID}}' spike/keystone:2026.1
docker rmi spike/keystone:2026.1; ./result | docker load
docker images --no-trunc --format '{{.ID}}' spike/keystone:2026.1
docker run -d -p 5000:5000 --name spike-registry registry:2
./result > keystone.tar
skopeo copy --dest-tls-verify=false docker-archive:keystone.tar docker://localhost:5000/keystone:2026.1
docker buildx imagetools inspect localhost:5000/keystone:2026.1
```

The `--rebuild` of the image derivation checks only the stream script, so the
customisation layer, the configuration and the packages built from source
were rebuilt as well. The push ran twice, each time into a fresh `registry:2`.

### Output

```text
checking outputs of '/nix/store/lqdh5526d3ck7rbi58315qlw8iicsig0-stream-keystone.drv'...
checking outputs of '/nix/store/fsbk5q5pc0gzxj2ny65ml144rb1h0mg2-cobaltcore-keystone-2026.1-env.drv'...
sha256:07e2238ad54e428374a640514a103a8a82d9819ba6d7399b39ebce413d1f48f9
sha256:07e2238ad54e428374a640514a103a8a82d9819ba6d7399b39ebce413d1f48f9
Writing manifest to image destination
Name:      localhost:5000/keystone:2026.1
MediaType: application/vnd.docker.distribution.manifest.v2+json
Digest:    sha256:6d19be21c4fec4c3fb8ca2b13a07d915a20991dcfbea22e5456f79d1bb97d0b3
```

No `--rebuild` printed "may not be deterministic". Two streams of the image
have the sha256 `a4fdb9ff46ee3596f68e14e796dfc5f7843729115bef73d50ddea2fc583f71f5`.

### Hands to

[#1404] pushes by digest with `skopeo copy` and keeps the `merge-*` jobs.
[#1407] applies the keeper tag and rewrites the three copies of its regex
together.

## Go workspace

### Decision

Both builds work. Phase 6 builds per module with `GOWORK=off`: one `vendorHash`
per operator follows that operator's `go.sum`, which Renovate's `gomodTidyAll`
keeps consistent. The module build needs `proxyVendor = true`: a plain
`go mod vendor` copies the locally replaced `internal/common` into `vendor/`,
so its `vendorHash` would change with every `internal/common` commit. The
workspace variant is the fallback; its one hash covers every module of
`go.work`.

### Verified by

```bash
nix build ./nix/spike#keystone-operator-modroot -o result-modroot
nix build ./nix/spike#keystone-operator-workvendor -o result-workvendor
file result-modroot/bin/keystone result-workvendor/bin/keystone
result-modroot/bin/keystone --help 2>&1 | grep -c federation-metadata-allow-cidrs
result-workvendor/bin/keystone --help 2>&1 | grep -c federation-metadata-allow-cidrs
(cd operators/keystone && GOWORK=off GOTOOLCHAIN=local go mod vendor -o /tmp/vend)
grep -n 'internal/common' /tmp/vend/modules.txt | head -1
```

Both hashes were recorded from the first build with `lib.fakeHash`.

### Output

```text
result-modroot/bin/keystone: ELF 64-bit LSB executable, x86-64, version 1 (SYSV), statically linked, stripped
result-workvendor/bin/keystone: ELF 64-bit LSB executable, x86-64, version 1 (SYSV), statically linked, stripped
1
1
4:# github.com/c5c3/cobaltcore/internal/common v0.0.0 => ../../internal/common
```

| Variant | Hash |
|---|---|
| `keystone-operator-modroot`, `vendorHash` (`proxyVendor`) | `sha256-c5+glSNAe5pMFCxqWQYUfp5iO8MoPUPXomYuuwm81AU=` |
| `keystone-operator-workvendor`, `outputHash` of `go work vendor` | `sha256-0/74+cjMXC57jxGgFnSgRqcZL8cRTEvdJVMxqF/mq5o=` |

### Hands to

[#1409] builds each operator with `buildGo127Module`, `modRoot`,
`env.GOWORK = "off"`, `proxyVendor = true`, its own `vendorHash` and a source
of only that operator and `internal/common`, so a commit to another operator
does not rebuild it. The unit tests need envtest binaries, so the spike sets
`doCheck = false`.

## Outputs

| Output | Store path |
|---|---|
| `keystone-venv` | `/nix/store/iidhpknbad1jxmkxj7jf9blm6v3a0q7x-cobaltcore-keystone-2026.1-env` |
| `keystone-image` | `/nix/store/m6dskl4b50lq50ji2b7wq5lsy8v15sxb-stream-keystone` |
| `nova-rootwrap-image` | `/nix/store/dzck4jw5g826d34804qb6y0h5lxliwmk-stream-nova-rootwrap` |
| `ceph-bindings` | `/nix/store/zrzxy24337qwb76lffp6m9xgi8ygy8ag-python3.12-ceph-bindings-20.2.4` |
| `glance-env` | `/nix/store/l293z8896gmfrdrribrp83hddc1i0y1s-cobaltcore-glance-2026.1-env` |
| `keystone-operator-modroot` | `/nix/store/07gmkp08lk74splqwqxv3pk8sw64qhmw-keystone-operator-modroot-spike` |
| `keystone-operator-workvendor` | `/nix/store/kvrzz6r9if899mvp2j4a3kdnbh6g4d8m-keystone-operator-workvendor-spike` |
| `checks.x86_64-linux.keystone-2026_1-tests` | `/nix/store/ayz25dhkdsjv4wddyfsy1nlah9a7is6g-keystone-2026_1-tests` |

[#1401]: https://github.com/C5C3/cobaltcore/issues/1401
[#1404]: https://github.com/C5C3/cobaltcore/issues/1404
[#1405]: https://github.com/C5C3/cobaltcore/issues/1405
[#1407]: https://github.com/C5C3/cobaltcore/issues/1407
[#1409]: https://github.com/C5C3/cobaltcore/issues/1409
