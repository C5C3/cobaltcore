---
title: Container Images
quadrant: infrastructure
---

# Container Images

Reference documentation for the container image build system. This covers
the Dockerfile hierarchy, base image contents, release configuration file formats,
named build context patterns, constraint override tooling, and local build instructions.

## Dockerfile Hierarchy

The OpenStack service images follow a three-layer hierarchy. Each layer builds on the
previous one, separating concerns between runtime base, build tooling, and
service-specific code:

```text
ubuntu:noble
├── python-base          Runtime base: Python 3.12, system libs, openstack user
│   ├── venv-builder     Build stage: compilers, uv, virtualenv with common packages
│   │   ├── keystone     Stage 1 (build): install Keystone into virtualenv
│   │   ├── horizon      Stage 1 (build): install Horizon, pre-build static assets
│   │   ├── glance       Stage 1 (build): install Glance + glance_store[s3]
│   │   ├── placement    Stage 1 (build): install Placement, write WSGI entry
│   │   ├── barbican     Stage 1 (build): install Barbican into virtualenv
│   │   └── neutron      Stage 1 (build): install Neutron into virtualenv
│   ├── keystone         Stage 2 (runtime): copy virtualenv, add runtime apt packages
│   ├── horizon          Stage 2 (runtime): copy virtualenv + static assets
│   ├── glance           Stage 2 (runtime): copy virtualenv, add runtime apt packages
│   ├── placement        Stage 2 (runtime): copy virtualenv, add runtime apt packages
│   ├── barbican         Stage 2 (runtime): copy virtualenv, add runtime apt packages
│   └── neutron          Stage 2 (runtime): copy virtualenv, add runtime apt packages
```

The `venv-builder` image is used only as a build stage — it never runs in production.
Service images (e.g., `keystone`) use a multi-stage build: stage 1 extends `venv-builder`
to install the service, then stage 2 extends `python-base` and copies only the virtualenv
from stage 1. This ensures the final image contains no build tools.

Three images sit outside that lineage. They carry no OpenStack code and build
straight on `ubuntu:noble`:

```text
ubuntu:noble
├── ovn                        Stage 1 (build): compile OVS + OVN from pinned upstream git
├── ovn                        Stage 2 (runtime): copy binaries, schemas and ctl scripts, add runtime apt packages
├── keystone-federation-proxy  Single stage: distro apache2 + mod_auth_openidc + mod_auth_mellon
└── backup-shifter             Single stage: distro rclone
```

All three are described under [Release-independent images](#release-independent-images).

## Base Images

### python-base

**Location:** `images/python-base/Dockerfile`

The foundational runtime image for all OpenStack service containers.

| Property | Value |
| --- | --- |
| Base image | `ubuntu:noble` (Ubuntu 24.04 LTS) |
| Python | 3.12 (from Ubuntu Noble package repository) |
| User | `openstack` (UID 42424, GID 42424, shell `/usr/sbin/nologin`) |
| Home directory | `/var/lib/openstack` |

**Environment variables:**

| Variable | Value | Purpose |
| --- | --- | --- |
| `PATH` | `/var/lib/openstack/bin:$PATH` | Ensures virtualenv binaries take precedence |
| `LANG` | `C.UTF-8` | Consistent locale for Python string handling |

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `ca-certificates` | TLS certificate verification |
| `netbase` | `/etc/protocols` and `/etc/services` for network operations |
| `python3` | Python 3.12 runtime |
| `sudo` | Privilege escalation for entrypoint scripts |
| `tzdata` | Timezone data for datetime operations |

**User convention:** All service images share a single `openstack` user (UID/GID 42424)
rather than creating per-service users. This is a deliberate deviation from the architecture
document — see [Design Deviations](#design-deviations) for rationale.

**OCI labels:** The Dockerfile includes static `LABEL` instructions for baseline
OCI Image Spec annotations (`title`, `description`, `licenses`, `vendor`). These are
always present on locally-built images. In CI, `docker/metadata-action` supplements these
with dynamic labels (created, revision, source, url, version) — see
[Build Images Workflow — OCI Annotations](build-images-workflow.md#oci-annotations).

### venv-builder

**Location:** `images/venv-builder/Dockerfile`

Build-stage image that extends `python-base` with compilation tools and a prepared
Python virtualenv. This image is never deployed — it exists only as a `FROM` target
for multi-stage service builds.

| Property | Value |
| --- | --- |
| Base image | `python-base` (local) |
| Package manager | `uv` 0.11.24 (copied from the digest-pinned `ghcr.io/astral-sh/uv:0.11.24`; tracked by Renovate) |
| Virtualenv path | `/var/lib/openstack` |

**Build-time packages:**

| Package | Purpose |
| --- | --- |
| `build-essential` | C compiler and make (for building Python C extensions) |
| `git` | Fetching Python packages from git repositories |
| `libffi-dev` | cffi/cryptography compilation |
| `libpq-dev` | psycopg2 compilation (PostgreSQL client) |
| `libssl-dev` | cryptography/pyOpenSSL compilation |
| `python3-dev` | Python headers for C extensions |
| `python3-venv` | `venv` module for virtualenv creation |

**Pre-installed common packages:**

The virtualenv includes five packages shared by all OpenStack services, version-pinned in
`images/venv-builder/requirements.txt`:

| Package | Purpose |
| --- | --- |
| `cryptography` | TLS, token encryption, Fernet keys |
| `pymemcache` | Memcached client (pure-Python `pymemcache` backend) |
| `pymysql` | MySQL/MariaDB database driver |
| `python-memcached` | Memcached client for caching |
| `uwsgi` | WSGI application server |

These packages are **version-pinned** in `requirements.txt` so the `venv-builder` image is
reproducible — without pins they would resolve to whatever is latest on PyPI at build time.
The image stays release-independent: the pins are deliberately not taken from any single
release's `upper-constraints.txt`. The OpenStack-dependency subset (`cryptography`,
`pymemcache`, `pymysql`, `python-memcached`) is authoritatively re-pinned per release by
service Dockerfiles via `uv pip install --constraint upper-constraints.txt`; `uwsgi` is not
an OpenStack dependency (it is absent from `upper-constraints.txt`), so its version is fixed
here. Renovate tracks these pins through its native `pip_requirements` manager — major bumps
are gated for manual review, minor/patch are automerged after a three-day soak.

**OCI labels:** Same static `LABEL` pattern as `python-base` — title, description,
licenses, and vendor are embedded in the Dockerfile for local build visibility.

## Service Images

### keystone

**Location:** `images/keystone/Dockerfile`

The Keystone identity service image uses a two-stage build:

**Stage 1 (`build`)** — extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` and `ARG PIP_PACKAGES` for build-time injection of extras
  and additional packages from `extra-packages.yaml` (passed by the CI workflow)
- Mounts `upper-constraints.txt` and the Keystone source tree via named build contexts
- Installs Keystone with extras into the virtualenv using `uv pip install --constraint`

**Stage 2 (runtime)** — extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES` for build-time injection of runtime system packages
  from `extra-packages.yaml` (passed by the CI workflow)
- Copies `/var/lib/openstack` from the build stage using `COPY --from=build --link`
  (the `--link` flag enables parallel layer extraction and deduplication)
- Installs runtime system packages via `apt-get install ${EXTRA_APT_PACKAGES}`
- Sets `USER openstack` for non-root execution

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `libapache2-mod-wsgi-py3` | Apache WSGI module for serving Keystone |
| `libldap2` | LDAP client library (python-ldap runtime dependency) |
| `libsasl2-2` | SASL authentication library (LDAP SASL bind support) |
| `libxml2` | XML parsing library (lxml runtime dependency) |

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Virtualenv at `/var/lib/openstack` with all Keystone dependencies
- `keystone-manage` CLI available via `PATH`

**OCI labels:** The `LABEL` instruction is placed in Stage 2 (runtime) before
the `USER` instruction. Labels added in Stage 1 (build) are discarded by Docker's
multi-stage build process — only the runtime stage labels appear on the final image. In
CI, `docker/metadata-action` overrides `org.opencontainers.image.version` with the
upstream OpenStack release version from `source-refs.yaml` via a `type=raw` tag strategy.

### horizon

**Location:** `images/horizon/Dockerfile`

The Horizon dashboard image uses the same two-stage build as Keystone, with two
horizon-specific twists: static assets are pre-built at image-build time, and the
`horizon===` pin in `upper-constraints.txt` must be stripped before the build
(see [Constraint Overrides](#constraint-overrides)).

**Stage 1 (`build`)** — extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` and `ARG PIP_PACKAGES` (both empty for horizon today;
  the wiring mirrors keystone so `extra-packages.yaml` stays the single edit point)
- Mounts `upper-constraints.txt` and the Horizon source tree via named build contexts
  (`--build-context horizon=...` / `--build-context upper-constraints=...`)
- Installs Horizon into the virtualenv using `uv pip install --constraint`
- Pre-builds static assets: a throwaway `local_settings.py` is written into the
  installed `openstack_dashboard/local/` package, `collectstatic --noinput` and
  `compress --force` (django-compressor offline compression) run against it, and the
  throwaway file is removed. Assets land in `/var/lib/openstack/horizon-static` with
  the offline manifest at `dashboard/manifest.json`

**Stage 2 (runtime)** — extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES` (empty for horizon today — the dashboard is pure
  Python; the pymemcache session-cache client comes from the venv-builder base venv)
- Copies `/var/lib/openstack` (virtualenv plus pre-built static assets) from the build
  stage using `COPY --from=build --link`
- Creates `/etc/openstack-dashboard/` and symlinks the packaged
  `openstack_dashboard/local/local_settings.py` to
  `/etc/openstack-dashboard/local_settings.py`, where the horizon-operator mounts the
  rendered Django settings ConfigMap. The symlink dangles at build time by design
- Sets `USER openstack` for non-root execution

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Serves via uWSGI loading `openstack_dashboard.wsgi` directly (the module ships
  `application`) — no hand-written wsgi script, and static assets are served through
  `uwsgi --static-map /static=/var/lib/openstack/horizon-static`
- i18n message catalogs are not compiled (`compilemessages` needs gettext at build
  time); the dashboard renders in English. Deferred until a locale requirement lands

**Unit tests:** horizon ships no `.stestr.conf` — its Django suite runs under pytest.
`hack/ci-run-unit-tests.sh` branches on `.stestr.conf` presence and delegates to
horizon's upstream `tools/unit_tests.sh` driver in the pytest path.

### glance

**Location:** `images/glance/Dockerfile`

The Glance service image uses the same two-stage build as Keystone. Both launch
modes ship in one image — 2025.2 starts the eventlet `glance-api` console
script, and 2026.1+ runs uWSGI with the hand-shipped
`/var/lib/openstack/bin/glance-wsgi-api` entry script. Glance's stock module
path (`glance.wsgi.api:application`) is unusable under the operator's config
layout: `wsgi_app.init_app()` ignores `sys.argv` (and so uWSGI's `--pyargv`)
and reads only `$OS_GLANCE_CONFIG_DIR/glance-api.conf`, so the shim redirects
config discovery to the two mounted `--config-dir` roots instead.

**Stage 1 (`build`)** — extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` (unused by glance today; kept for parity) and
  `ARG PIP_PACKAGES`, which carries `glance_store[s3]` — the S3 store driver's
  extra lives on `glance_store`, not `glance`, and pulls `boto3`, `botocore`,
  and `s3transfer` (all pinned in `upper-constraints.txt`)
- Mounts `upper-constraints.txt` and the Glance source tree via named build
  contexts (`--build-context glance=...` / `--build-context upper-constraints=...`)
- Installs Glance into the virtualenv using `uv pip install --constraint`. The
  `--prefix` install generates the `glance-api` and `glance-manage` console
  scripts from `setup.cfg` (it only skips PBR `wsgi_scripts`); the uWSGI entry
  script is not generated but copied in during the runtime stage

**Stage 2 (runtime)** — extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES`, which carries `libpython3.12t64`: the
  venv-builder-compiled uwsgi binary links `libpython3.12.so.1.0`, which
  python-base does not ship (the same rationale as horizon). Glance is otherwise
  pure Python at runtime
- Copies `/var/lib/openstack` from the build stage using `COPY --from=build --link`
- Copies the `glance-wsgi-api` uWSGI entry script to
  `/var/lib/openstack/bin/glance-wsgi-api` (the path the glance-operator's
  `--wsgi-file` flag references)
- Sets `USER openstack` for non-root execution

The image stays config-free: the glance-operator mounts `glance-api.conf`,
`glance-api-paste.ini`, and policy, and provides the staging/tasks paths as
`emptyDir` mounts.

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `libpython3.12t64` | Shared `libpython3.12.so.1.0` for the venv-builder-compiled uwsgi |

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Virtualenv at `/var/lib/openstack` with all Glance dependencies and the S3 store driver
- `glance-manage` and `glance-api` CLIs available via `PATH`

**Unit tests:** glance ships a `.stestr.conf`, so `hack/ci-run-unit-tests.sh`
runs its suite under stestr (the default path, as for keystone).

**Image contract check:** `tests/container-images/verify_glance.sh` is the hard
gate — it verifies the CLIs, importability, the uWSGI entry script, the S3 store
driver's boto3 resolution, non-root execution, and the absence of build tools.

### placement

**Location:** `images/placement/Dockerfile`

The Placement service image uses the same two-stage build as Keystone. Its one
service-specific twist is the WSGI entry script: upstream ships no usable one
for either release. 2025.2 declares `placement-api` as a PBR `wsgi_scripts`
entry in `setup.cfg`, which uv's `--prefix` install mode does not generate;
2026.1 moved packaging to `pyproject.toml` and declares no WSGI script at all.
The entry is therefore written by hand in the build stage under the upstream
name, for both releases, with no release conditional.

**Stage 1 (`build`)** — extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` and `ARG PIP_PACKAGES` (both empty for placement
  today; the wiring mirrors the other service images so `extra-packages.yaml`
  stays the single edit point)
- Mounts `upper-constraints.txt` and the Placement source tree via named build
  contexts (`--build-context placement=...` /
  `--build-context upper-constraints=...`)
- Installs Placement into the virtualenv using `uv pip install --constraint`.
  The `--prefix` install generates the `placement-manage` and `placement-status`
  console scripts (it only skips PBR `wsgi_scripts`)
- Writes `/var/lib/openstack/bin/placement-api` — a two-line entry that calls
  `placement.wsgi.init_application()` — and marks it executable

**Stage 2 (runtime)** — extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES`, which carries `libpython3.12t64`: the
  venv-builder-compiled uwsgi binary links `libpython3.12.so.1.0`, which
  python-base does not ship (the same rationale as glance and horizon).
  Placement is otherwise pure Python at runtime
- Copies `/var/lib/openstack` from the build stage using `COPY --from=build --link`
- Sets `USER openstack` for non-root execution

The image stays config-free: placement locates its configuration via the
`OS_PLACEMENT_CONFIG_DIR` environment variable set by the operator.

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `libpython3.12t64` | Shared `libpython3.12.so.1.0` for the venv-builder-compiled uwsgi |

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Virtualenv at `/var/lib/openstack` with all Placement dependencies
- `placement-manage` and `placement-status` CLIs available via `PATH`
- The uWSGI entry script at `/var/lib/openstack/bin/placement-api`

**Image contract check:** `tests/container-images/verify_placement.sh` is the
hard gate — it verifies the CLIs, importability, the uWSGI entry script
(present, executable, parsable, and its import target resolvable), that uwsgi
runs, non-root execution, and the absence of build tools.

### barbican

**Location:** `images/barbican/Dockerfile`

The Barbican service image uses the same two-stage build as Keystone. It ships
no WSGI entry script at all. Both releases ship `barbican/wsgi/api.py` with a
module-level `application`, so the barbican-operator launches uWSGI with the
stock module path `barbican.wsgi.api:application` against config mounted at
`/etc/barbican/`. Placement's entry script is written by hand because upstream
ships no usable one, and glance carries the `glance-wsgi-api` shim because its
stock module path ignores the operator's config layout. Barbican needs neither.

**Stage 1 (`build`)** — extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` and `ARG PIP_PACKAGES` (both empty for barbican
  today; the wiring mirrors the other service images so `extra-packages.yaml`
  stays the single edit point)
- Mounts `upper-constraints.txt` and the Barbican source tree via named build
  contexts (`--build-context barbican=...` /
  `--build-context upper-constraints=...`)
- Installs Barbican into the virtualenv using `uv pip install --constraint`.
  The `--prefix` install generates the console scripts from `setup.cfg`. It
  skips only the PBR `wsgi_scripts` entry `barbican-wsgi-api`, which no
  hand-written script replaces

**Stage 2 (runtime)** — extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES`, which carries `libpython3.12t64`: the
  venv-builder-compiled uwsgi binary links `libpython3.12.so.1.0`, which
  python-base does not ship (the same rationale as glance, horizon, and
  placement). Barbican is otherwise pure Python at runtime
- Copies `/var/lib/openstack` from the build stage using `COPY --from=build --link`
- Sets `USER openstack` for non-root execution

The image stays config-free: the barbican-operator mounts `barbican.conf` and
`barbican-api-paste.ini` under `/etc/barbican/`.

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `libpython3.12t64` | Shared `libpython3.12.so.1.0` for the venv-builder-compiled uwsgi |

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Virtualenv at `/var/lib/openstack` with all Barbican dependencies, including
  the in-tree vault secret-store plugin and castellan
- `barbican-manage` and `barbican-status` CLIs available via `PATH`
- No WSGI entry script; the service is launched via the
  `barbican.wsgi.api:application` module path

**Unit tests:** barbican ships a `.stestr.conf`, so `hack/ci-run-unit-tests.sh`
runs its suite under stestr (the default path, as for keystone).

**Image contract check:** `tests/container-images/verify_barbican.sh` is the
hard gate — it verifies the CLIs, importability, that the `vault_plugin` and
`castellan.drivers` `vault` stevedore entry points load, that
`barbican.wsgi.api` resolves and binds `application` to something other than
the `None` sentinel, that uwsgi runs, non-root execution, and the absence of
build tools. Both plugin halves are loaded through their entry-point metadata
rather than imported by module path, because that is how barbican and
castellan resolve them at runtime — an import would go green off the `.py`
file on disk while the runtime lookup raises stevedore `NoMatches`. The WSGI
check inspects the module instead of importing it: importing
`barbican.wsgi.api` executes `get_api_wsgi_script()`, which reads paste config
from `/etc/barbican/barbican-api-paste.ini`, absent from the bare image. So it
pairs `importlib.util.find_spec` for the module path with an `ast.parse` of the
module source for the `application` symbol. Both pinned releases open with
`application = None` and only rebind it inside a `threading.Lock()` block, so
the check walks into nested statement bodies but stops at every function,
class and lambda boundary — uWSGI looks `application` up as a module global,
and a binding in a nested scope is a different symbol. A module whose only
module-level binding is that sentinel is rejected; otherwise uWSGI binds
`application` to `None` and every request fails.

### neutron

**Location:** `images/neutron/Dockerfile`

The Neutron service image uses the same two-stage build as Keystone. It ships
no WSGI entry script. Neither release ships a `neutron-server` script, and
`neutron/wsgi/api.py` is byte-identical at 27.0.3 (2025.2) and 28.0.1
(2026.1). Both tags bind a module-level `application` inside a
`threading.Lock()` block, so the neutron-operator launches uWSGI with
`--module neutron.wsgi.api` and lets the process find its configuration
through the `OS_NEUTRON_CONFIG_DIR` and `OS_NEUTRON_CONFIG_FILES` environment
variables. 27.0.3 also generates the PBR `wsgi_scripts` entry `neutron-api`
from `neutron.cmd.server:main_api_uwsgi`, which nothing calls; 28.0.1 declares
no WSGI script.

**Stage 1 (`build`)** extends `venv-builder`:

- Declares `ARG PIP_EXTRAS` and `ARG PIP_PACKAGES` (both empty for neutron
  today; the wiring mirrors the other service images so `extra-packages.yaml`
  stays the single edit point)
- Mounts `upper-constraints.txt` and the Neutron source tree via named build
  contexts (`--build-context neutron=...` /
  `--build-context upper-constraints=...`)
- Installs Neutron into the virtualenv using `uv pip install --constraint`.
  The `--prefix` install generates the console scripts declared in `setup.cfg`
  (27.0.3) and `pyproject.toml` (28.0.1): `neutron-db-manage`,
  `neutron-status`, `neutron-periodic-workers`,
  `neutron-ovn-maintenance-worker`, `neutron-ovn-metadata-agent` and
  `neutron-ovn-db-sync-util`

**Stage 2 (runtime)** extends `python-base`:

- Declares `ARG EXTRA_APT_PACKAGES`, which carries four packages: the shared
  libpython the venv-builder-compiled uwsgi links against, plus the
  `ovsdb-client`, `haproxy` and `ip` binaries neutron shells out to
- Copies `/var/lib/openstack` from the build stage using `COPY --from=build --link`
- Sets `USER openstack` for non-root execution

The image stays config-free. The neutron-operator supplies the configuration
through `OS_NEUTRON_CONFIG_DIR` and `OS_NEUTRON_CONFIG_FILES`, and either
points `[DEFAULT] api_paste_config` at the shipped package data file
`/var/lib/openstack/etc/neutron/api-paste.ini` or mounts that file to
`/etc/neutron/api-paste.ini`. That file is byte-identical at both tags.

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `haproxy` | The metadata agent spawns `haproxy -f <cfg>` per network (`neutron/agent/metadata/driver_base.py`) |
| `iproute2` | `ip`, which that agent uses for `ip netns exec` (`neutron/agent/linux/ip_lib.py`) |
| `libpython3.12t64` | Shared `libpython3.12.so.1.0` for the venv-builder-compiled uwsgi |
| `openvswitch-common` | `ovsdb-client`, which `pre_fork_initialize` shells out to (`neutron/common/ovn/utils.py`); without it every API worker dies with `[Errno 2] No such file or directory: 'ovsdb-client'` |

Noble ships Open vSwitch 3.3.9. The OVSDB wire protocol is schema-independent,
so that client talks to the OVN 26.03 server.

The virtualenv holds `ovs` 3.5.1 (2025.2) and 3.7.0 (2026.1), whose
`ovs/dns_resolve.py` resolves no hostname without the `unbound` Python module.
The image ships no `python3-unbound`: the distribution package installs into
the system interpreter, which the virtualenv under `/var/lib/openstack` does
not see, so `import unbound` inside the image raises `ModuleNotFoundError`.
Per decision D9 of issue #898, `OVNCentral` publishes IP addresses in
`status.dbAddress` and `status.internalDbAddress`, and the neutron-operator
renders `[ovn] ovn_nb_connection` and `ovn_sb_connection` from those.

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Virtualenv at `/var/lib/openstack` with all Neutron dependencies
- The console scripts listed above available via `PATH`
- No WSGI entry script; the service is launched via the `neutron.wsgi.api`
  module path

**Unit tests:** neutron ships a `.stestr.conf`, so `hack/ci-run-unit-tests.sh`
runs its suite under stestr (the default path, as for keystone). The script
appends `--exclude-list /workspace/test-excludes/neutron.txt` only when
`releases/<release>/test-excludes/neutron.txt` exists, and neither release
excludes a test: the first runs at 27.0.3 and 28.0.1 hit no
environment-dependent failure. At roughly 21,000 tests the suite is the
largest in the tree.

**Image contract check:** `tests/container-images/verify_neutron.sh` is the
hard gate. Its 12 tests cover `neutron-db-manage --help` and
`neutron-status --help`, the importability of `neutron` and of the `ovs` and
`ovsdbapp` client libraries, the presence of `api-paste.ini`, and `--help` on
the four companion console scripts. `ovsdb-client --version` naming Open
vSwitch, `haproxy -v` and `ip -V` prove the apt wiring. The
remaining tests check non-root execution, the absence of build tools, and that
uwsgi runs. The WSGI check inspects the module instead of importing it:
importing `neutron.wsgi.api` executes `server.boot_server(api.api_server)`,
which reads the configuration the operator mounts and the bare image does not
carry. So it pairs `importlib.util.find_spec` for the module path with an
`ast.parse` of the module source, and rejects a module whose only module-level
binding of `application` is the `None` sentinel. Pointed at a barbican image,
the script fails in each of its first nine tests.

## Release-independent images

These images ship software from outside the OpenStack release matrix. They have
no key in `releases/*/source-refs.yaml`, take no `PIP_EXTRAS` or
`EXTRA_APT_PACKAGES` build args, and are built once per upstream version
instead of once per release.

### ovn

**Location:** `images/ovn/Dockerfile`

OVN and Open vSwitch daemons and client tools, compiled from upstream git in a
two-stage build. Both stages start from the same digest-pinned `ubuntu:noble`.

| Property | Value |
| --- | --- |
| Base image | `ubuntu:noble` (Ubuntu 24.04 LTS), both stages, pinned by digest |
| Version pin | `ARG OVN_VERSION=v26.03.2`, the only version input, content-pinned by `ARG OVN_COMMIT` |
| Open vSwitch | The commit the `ovs` submodule gitlink names at that OVN commit, pinned by `ARG OVS_COMMIT` |
| User | `openstack` (UID 42424, GID 42424), created in this Dockerfile |
| Entrypoint | None; every workload names the daemon it runs |

**Stage 1 (`build`):**

- Installs the build requirements from OVN's
  `Documentation/intro/install/general.rst`: `autoconf`, `automake`,
  `build-essential`, `libtool`, `libssl-dev`, `libunbound-dev`,
  `libcap-ng-dev` and `pkg-config`, plus `git`, `ca-certificates` and the
  `python3` that the OVS code generation needs
- Fetches `$OVN_COMMIT` from `https://github.com/ovn-org/ovn.git` — the
  commit, not the tag — reads the OVS commit out of that checkout with
  `git -C /src/ovn rev-parse HEAD:ovs`, aborts the build when it differs from
  `$OVS_COMMIT`, and fetches that commit from
  `https://github.com/openvswitch/ovs.git`
- Builds OVS, then OVN against the OVS source tree
  (`--with-ovs-source=/src/ovs`). Both configure with
  `--prefix=/usr --localstatedir=/var --sysconfdir=/etc` and stage their
  install into `/out` via `make install DESTDIR=/out`

**Stage 2 (runtime):**

- Installs the runtime packages listed below
- Creates the `openstack` user and group
- Copies `/out/usr/bin`, `/out/usr/sbin`, `/out/usr/share/openvswitch` and
  `/out/usr/share/ovn`. Headers, static libraries and man pages stay in the
  build stage
- Creates `/var/run/openvswitch`, `/var/run/ovn`, `/var/lib/openvswitch`,
  `/var/lib/ovn`, `/var/log/openvswitch`, `/var/log/ovn`, `/etc/openvswitch`
  and `/etc/ovn`, all owned by `openstack`
- Sets the OCI labels (title `ovn`, description "OVN and Open vSwitch daemons
  and client tools built from pinned upstream sources", licenses `Apache-2.0`,
  vendor `SAP SE`) and `USER openstack`

**Installed paths:**

| Path | Contents |
| --- | --- |
| `/usr/bin` | `ovn-northd`, `ovn-controller`, `ovn-nbctl`, `ovn-sbctl`, `ovn-appctl`, `ovsdb-tool`, `ovsdb-client`, `ovs-vsctl`, `ovs-appctl`, `ovs-ofctl` |
| `/usr/sbin` | `ovs-vswitchd`, `ovsdb-server` |
| `/usr/share/ovn` | OVN's OVSDB schemas and `scripts/ovn-ctl` |
| `/usr/share/openvswitch` | The OVS OVSDB schemas and `scripts/ovs-ctl` |

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `ca-certificates` | TLS trust store |
| `iproute2` | `ip`, which the chassis DaemonSets of issue #903 use to inspect interfaces |
| `kmod` | `modprobe`, which those DaemonSets use to load host kernel modules |
| `libcap-ng0` | Privilege-drop library the daemons link against |
| `libssl3t64` | OpenSSL 3 runtime for the TLS-protected OVSDB connections |
| `libunbound8` | DNS resolution |

**Version output:** OVN programs print `<prog> 26.03.2` on `--version`,
followed by `Open vSwitch Library 3.7.0`, the OVS revision they were built
against. OVS programs print `<prog> (Open vSwitch) 3.7.0`. Those two numbers
agreeing is what the single pin buys, and
`tests/container-images/verify_ovn.sh` compares them.

**Non-root by default:** `ovsdb-server`, `ovn-northd` and the client tools need
no privileges, so the image runs as `openstack`. The chassis pods of issue #903
set `runAsUser: 0` and the capabilities they need in their own pod spec.

**Version pin:** Open vSwitch has no version of its own to choose. It follows
the `ovs` submodule gitlink (decision D1 of issue #898): one version line to
bump, one Renovate rule. OVN's `Documentation/intro/install/general.rst` notes
under Build Requirements that the submodule is "not recommended to be used as a
source for OVS build"; ovn-kubernetes builds its OVS from that gitlink in
`dist/images/Dockerfile.fedora`, and this image does the same.

`ARG OVN_VERSION` names a git tag, which is a mutable ref: upstream can move or
delete it, and no check on the built image would notice. So each half of the
source tree carries a content pin next to it, standing to `OVN_VERSION` as the
`sha256` digest stands to `ubuntu:noble` — `ARG OVN_COMMIT` for the commit the
tag resolves to, `ARG OVS_COMMIT` for the SHA the `ovs` gitlink names at that
commit. Like a digest, both are what the build fetches: it never resolves the
tag, and it fails when the gitlink at `$OVN_COMMIT` disagrees with
`$OVS_COMMIT`. Neither a moved OVN tag nor a changed gitlink can alter the image
without a change in this repository. Bump both together with `OVN_VERSION`; an
`OVN_COMMIT` left behind by a tag bump surfaces in the version
`tests/container-images/verify_ovn.sh` reads off the binaries, and a stale
`OVS_COMMIT` in the build error.

A Renovate `customManager` tracks `ovn-org/ovn` github-tags on the
`ARG OVN_VERSION` line. Major and minor bumps are disabled to hold the image on
the 26.03 LTS line; patch bumps wait a three-day cooldown and are **not**
automerged, because no datasource can rewrite the two content pins and the
reviewer of the Renovate PR carries them across. See
[Dependency Management](../../contributing/dependency-management.md).

**Image contract check:** `tests/container-images/verify_ovn.sh` runs nine
tests against a built image. Five cover the software: the pinned OVN version on
`ovn-northd`, `ovn-controller`, `ovn-nbctl` and `ovn-sbctl`, the OVS daemons
and clients, the shipped OVS matching the revision OVN links against,
`ovn-ctl` and `ovs-ctl` running under the image's shell, and `ovsdb-tool
create` building a database from each of `ovn-nb.ovsschema`, `ovn-sb.ovsschema`
and `vswitch.ovsschema` — the schemas are the runtime artifact this image
exists to serve, and no other assertion here reads them. The other four cover
the packaging: `ldd` free of unresolved libraries with `libssl.so.3` linked
into the TLS-speaking daemons, no build toolchain and no development headers,
UID 42424 with writable state directories, and `modprobe` and `ip` on `PATH`.

### keystone-federation-proxy

**Location:** `images/keystone-federation-proxy/Dockerfile`

The Apache reverse proxy that terminates OIDC and SAML in front of Keystone: a
single stage on `ubuntu:noble` with the distro `apache2`,
`libapache2-mod-auth-openidc` and `libapache2-mod-auth-mellon`. Every component
comes from the Ubuntu archive, so the image has no version pin of its own and
follows the `noble` package set. Its build, verification and tag scheme are
described in
[build-keystone-federation-proxy / merge-keystone-federation-proxy-image](./build-images-workflow.md#build-keystone-federation-proxy-merge-keystone-federation-proxy-image).

### backup-shifter

**Location:** `images/backup-shifter/Dockerfile`

The rclone shifter for the OVN database backups: a single stage on
`ubuntu:noble` with the distro `rclone` and `ca-certificates`. It is the
`shifter` container of the `OVNCentral` backup CronJob and runs only when
`spec.backup.s3` is set, copying the northbound and southbound snapshots the
`backup` init container left on the PVC to the configured bucket. rclone comes
from the Ubuntu archive, so the image has no version pin of its own and follows
the `noble` package set. The Dockerfile sets no `ENTRYPOINT`: the operator
renders the `rclone copy` command and the `RCLONE_S3_*` environment onto the
CronJob container.

```bash
docker build -t c5c3/backup-shifter:latest images/backup-shifter/

# Run the full image contract check
bash tests/container-images/verify_backup_shifter.sh c5c3/backup-shifter:latest
```

Its build, verification and tag scheme are described in
[build-backup-shifter / merge-backup-shifter-image](./build-images-workflow.md#build-backup-shifter-merge-backup-shifter-image).

## Named Build Contexts

Service Dockerfiles use Docker's named build context feature (`--build-context`) to inject
release-specific files without embedding them in the Dockerfile or using `COPY` from the
build directory. This keeps Dockerfiles release-independent.

Each service build requires two named build contexts (shown here for Keystone; the
Horizon build is identical with `horizon` in place of `keystone`):

| Context name | Contents | Mounted as |
| --- | --- | --- |
| `upper-constraints` | Release directory containing `upper-constraints.txt` | `/tmp/upper-constraints.txt` |
| `keystone` | Keystone source tree (git checkout at the version from `source-refs.yaml`) | `/tmp/keystone` |

These are passed to `docker build` via `--build-context` flags:

```bash
docker build images/keystone \
  --build-context keystone=src/keystone \
  --build-context upper-constraints=releases/2025.2/
```

Inside the Dockerfile, named build contexts are consumed via `--mount=type=bind,from=`.
Extras are injected via `ARG PIP_EXTRAS` (comma-separated, e.g. `ldap,oauth1`)
which the CI workflow reads from `extra-packages.yaml`:

```dockerfile
ARG PIP_EXTRAS=""
ARG PIP_PACKAGES=""

RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    --mount=type=bind,from=keystone,target=/tmp/keystone \
    PKG="/tmp/keystone" && \
    if [ -n "$PIP_EXTRAS" ]; then PKG="${PKG}[${PIP_EXTRAS}]"; fi && \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        "$PKG" $PIP_PACKAGES
```

The `from=upper-constraints` directive tells Docker to resolve the file from the named
build context rather than the Dockerfile's primary build context. The `source=` parameter
selects a specific file within that context.

## Release Configuration

All release-specific configuration lives under `releases/<release>/` (e.g.,
`releases/2025.2/`). These files are the single source of truth for what gets built.
Adding a new service or updating a version requires editing only these files — not
Dockerfiles.

### source-refs.yaml

**Location:** `releases/<release>/source-refs.yaml`

Maps each OpenStack component to a git ref (tag, branch, or commit SHA) specifying
the version to build.

**Format:**

```yaml
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

keystone: "28.0.0"
```

Each key is a service name matching the Dockerfile directory under `images/`. Values
are quoted strings representing git refs — typically release tags (e.g., `"28.0.0"`).

To add a new service, add a single line: `<service>: "<git-ref>"`.

### upper-constraints.txt

**Location:** `releases/<release>/upper-constraints.txt`

Contains pinned Python dependency versions from the OpenStack requirements repository
(`stable/<release>` branch). This file is committed as-is from the upstream repository
to enable Renovate tracking and `git diff` for constraint changes.

**Format:**

```text
cryptography===44.0.0
oslo.limit===2.8.0
keystonemiddleware===10.9.0
```

Each line pins a single package using the `===` (arbitrary equality) operator. This is
the standard format used by OpenStack's global requirements process.

**Source:** `https://raw.githubusercontent.com/openstack/requirements/stable/<release>/upper-constraints.txt`

### extra-packages.yaml

**Location:** `releases/<release>/extra-packages.yaml`

Defines per-service Python extras and runtime system packages that are not part of the
core OpenStack package.

**Format:**

```yaml
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

keystone:
  pip_extras:
    - ldap
    - oauth1
  pip_packages: []
  apt_packages:
    - libapache2-mod-wsgi-py3
    - libldap2
    - libsasl2-2
    - libxml2
```

| Key | Purpose |
| --- | --- |
| `<service>.pip_extras` | Bare Python extra names combined with the service name to form install arguments (e.g. `keystone[ldap,oauth1]`). Passed as the `PIP_EXTRAS` build arg. |
| `<service>.pip_packages` | Additional pip packages to install alongside the service (space-separated in the build arg `PIP_PACKAGES`). Use an empty list (`[]`) when none are needed. |
| `<service>.apt_packages` | Runtime system packages installed via `apt` in the final image. Passed as the `EXTRA_APT_PACKAGES` build arg. |

To add packages for a new service, add a new top-level key matching the service name
with both `pip_extras` and `apt_packages` lists.

## Constraint Overrides

The constraint override system allows selective modification of individual package
pins in `upper-constraints.txt` without replacing the entire file. This is useful for
applying security fixes or version bumps for individual packages.

### Override Format

Override files are placed at `overrides/<release>/constraints.txt`. Each line is one
of three types:

| Syntax | Action | Example |
| --- | --- | --- |
| `package===version` | Replace the existing pin for `package` | `cryptography===44.0.1` |
| `-package` | Remove `package` from constraints entirely | `-oslo.messaging` |
| `# comment` or blank | Skipped (no action) | `# Security fix for CVE-2025-1234` |

**Example override file** (`overrides/2025.2/constraints.txt`):

```text
# Security fix: bump cryptography for CVE-2025-1234
cryptography===44.0.1

# Remove oslo.messaging pin to allow newer version
-oslo.messaging
```

**Real-world use — the horizon self-pin:** `upper-constraints.txt` pins `horizon===`
itself (unlike keystone, which never appears there). A source install with
`--constraint` refuses to install the horizon source tree against its own pin, so
`overrides/<release>/constraints.txt` ships a `-horizon` removal line for every
release. The git ref in `source-refs.yaml` stays the single source of truth for what
is built, independent of the upstream pin.

### Script Usage

**Location:** `scripts/apply-constraint-overrides.sh`

```bash
# Apply overrides for the 2025.2 release
./scripts/apply-constraint-overrides.sh 2025.2
```

**Behavior:**

| Condition | Result |
| --- | --- |
| `overrides/<release>/constraints.txt` exists | Each line is processed: replacements via `sed`, removals via `sed -d` |
| `overrides/<release>/constraints.txt` does not exist | Script exits with code 0, no changes made (idempotent) |

The script reads `releases/<release>/upper-constraints.txt` relative to the current working
directory (must be invoked from the repository root) and modifies it in-place. It uses GNU
`sed -i` for modifications (default on Ubuntu/CI runners — BSD `sed` is not supported).

**Arguments:**

| Argument | Required | Description |
| --- | --- | --- |
| `<release>` | Yes | Release identifier (e.g., `2025.2`), used to locate `overrides/<release>/constraints.txt` |

## Local Build Instructions

Build the complete image chain locally for development and verification:

### Step 1: Build base images

```bash
# Build python-base (tag must match FROM python-base in downstream Dockerfiles)
docker build images/python-base -t python-base

# Build venv-builder (tag must match FROM venv-builder in keystone Stage 1)
docker build images/venv-builder -t venv-builder
```

The tag names (`python-base`, `venv-builder`) must match the `FROM` directives in
downstream Dockerfiles. Docker resolves `FROM python-base` to the local image.

To also apply canonical registry tags, add a second `-t` flag:

```bash
docker build images/python-base -t python-base -t c5c3/python-base:3.12-noble
docker build images/venv-builder -t venv-builder -t c5c3/venv-builder:3.12-noble
```

### Step 2: Clone the service source

```bash
git clone --branch 28.0.0 --depth 1 \
  https://github.com/openstack/keystone.git src/keystone
```

The branch/tag must match the version specified in `releases/2025.2/source-refs.yaml`.

### Step 3: Build the service image

Extras are read from `extra-packages.yaml` and passed as `--build-arg`:

```bash
docker build images/keystone \
  -t c5c3/keystone:28.0.0 \
  --build-arg PIP_EXTRAS=ldap,oauth1 \
  --build-arg "EXTRA_APT_PACKAGES=libapache2-mod-wsgi-py3 libldap2 libsasl2-2 libxml2" \
  --build-context keystone=src/keystone \
  --build-context upper-constraints=releases/2025.2/
```

### Step 4: Verify the image

```bash
# Verify Keystone CLI is functional
docker run --rm c5c3/keystone:28.0.0 keystone-manage --version

# Verify non-root execution
docker run --rm c5c3/keystone:28.0.0 whoami
# Expected output: openstack

# Verify no build tools in final image
docker run --rm c5c3/keystone:28.0.0 which gcc \
  && echo "FAIL: gcc found in image" \
  || echo "PASS: gcc not found"
```

### Building horizon locally

The horizon build follows the same steps with two differences: the constraint
override must be applied first (it strips the `horizon===` self-pin in-place), and
no build args are needed today (all `extra-packages.yaml` lists are empty):

```bash
# Strip the horizon=== pin from upper-constraints.txt (GNU sed; run on Linux/CI)
./scripts/apply-constraint-overrides.sh 2025.2

git clone --branch 25.5.1 --depth 1 \
  https://opendev.org/openstack/horizon.git src/horizon

docker build images/horizon \
  -t c5c3/horizon:25.5.1 \
  --build-context horizon=src/horizon \
  --build-context upper-constraints=releases/2025.2/

# Run the full image contract check
bash tests/container-images/verify_horizon.sh c5c3/horizon:25.5.1
```

### Building ovn locally

The ovn build needs no source checkout and no build args: the Dockerfile clones
OVN at the pinned tag itself and derives Open vSwitch from that tag. One script
covers it. Set `GITHUB_TOKEN` when the anonymous fetch inside the build fails
with `could not read Username for 'https://github.com'`; the script mounts it
as a BuildKit secret and the fetch is authenticated (CI always does).

```bash
# Builds c5c3/ovn:<pinned version>
hack/ci-build-ovn-image.sh

# Same build under a different name
OVN_IMAGE=ghcr.io/c5c3/ovn:dev hack/ci-build-ovn-image.sh

# Print the pin the build used (26.03.2, without the leading v)
hack/ci-resolve-ovn-version.sh

# Run the full image contract check
bash tests/container-images/verify_ovn.sh c5c3/ovn:$(hack/ci-resolve-ovn-version.sh)
```

Both projects are compiled from source, so the first build takes a while. The
Dockerfile's BuildKit cache mounts keep the apt steps of a rebuild short, but
the two `make -j"$(nproc)"` runs dominate either way.

## Design Deviations

The implementation deviates from the original design document in one area,
documented with `# DEVIATION` comments in the affected Dockerfiles:

**Generic `openstack` user instead of per-service users:**

The original design's Keystone Dockerfile example creates a per-service user
(e.g., `groupadd keystone` / `useradd keystone`). The implementation uses a single
generic `openstack` user (UID/GID 42424) defined in `python-base` and shared by all
service images. This reduces complexity and image layers — each service image inherits
the user via `USER openstack` without needing its own user creation step.

The `# DEVIATION` comment appears in `images/python-base/Dockerfile` (where the
user is created) and in every service Dockerfile that uses it instead of a
per-service user (`images/keystone/Dockerfile`, `images/horizon/Dockerfile`,
`images/glance/Dockerfile`, `images/placement/Dockerfile`,
`images/barbican/Dockerfile`, `images/neutron/Dockerfile`).

`images/ovn/Dockerfile` and `images/backup-shifter/Dockerfile` carry the comment
for the other half of the same decision. Neither derives from `python-base`, so
each creates the `openstack` user and group itself. For `ovn` the distro
packages would install separate `openvswitch` and `ovn` users, and the source
build carries no such packaging. The `rclone` package that `backup-shifter`
installs brings no service account at all. One identity across all images keeps
the pod security contexts uniform, and in the `OVNCentral` backup CronJob it
lets the `backup` init container and the `shifter` container share a single pod
security context.
