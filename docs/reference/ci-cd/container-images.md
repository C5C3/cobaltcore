---
title: Container Images
quadrant: infrastructure
---

# Container Images

Reference documentation for the container image build system. This covers
the Dockerfile hierarchy, base image contents, release configuration file formats,
named build context patterns, constraint override tooling, and local build instructions.

## Dockerfile Hierarchy

Container images follow a three-layer hierarchy. Each layer builds on the previous one,
separating concerns between runtime base, build tooling, and service-specific code:

```text
ubuntu:noble
├── python-base          Runtime base: Python 3.12, system libs, openstack user
│   ├── venv-builder     Build stage: compilers, uv, virtualenv with common packages
│   │   ├── keystone     Stage 1 (build): install Keystone into virtualenv
│   │   └── horizon      Stage 1 (build): install Horizon, pre-build static assets
│   ├── keystone         Stage 2 (runtime): copy virtualenv, add runtime apt packages
│   └── horizon          Stage 2 (runtime): copy virtualenv + static assets
```

The `venv-builder` image is used only as a build stage — it never runs in production.
Service images (e.g., `keystone`) use a multi-stage build: stage 1 extends `venv-builder`
to install the service, then stage 2 extends `python-base` and copies only the virtualenv
from stage 1. This ensures the final image contains no build tools.

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

## Design Deviations

The implementation deviates from the architecture document
(`architecture/docs/08-container-images/01-build-pipeline.md`) in one area, documented
with `# DEVIATION` comments in the affected Dockerfiles:

**Generic `openstack` user instead of per-service users:**

The architecture document's Keystone Dockerfile example creates a per-service user
(e.g., `groupadd keystone` / `useradd keystone`). The implementation uses a single
generic `openstack` user (UID/GID 42424) defined in `python-base` and shared by all
service images. This reduces complexity and image layers — each service image inherits
the user via `USER openstack` without needing its own user creation step.

The `# DEVIATION` comment appears in `images/python-base/Dockerfile` (where the
user is created) and in every service Dockerfile that uses it instead of a
per-service user (`images/keystone/Dockerfile`, `images/horizon/Dockerfile`).
