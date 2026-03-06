---
title: Container Images
quadrant: infrastructure
feature: CC-0006
---

# Container Images

Reference documentation for the container image build system (CC-0006). This covers
the Dockerfile hierarchy, base image contents, release configuration file formats,
named build context patterns, constraint override tooling, and local build instructions.

## Dockerfile Hierarchy

Container images follow a three-layer hierarchy. Each layer builds on the previous one,
separating concerns between runtime base, build tooling, and service-specific code:

```text
ubuntu:noble
├── python-base          Runtime base: Python 3.12, system libs, openstack user
│   ├── venv-builder     Build stage: compilers, uv, virtualenv with common packages
│   │   └── keystone     Stage 1 (build): install Keystone into virtualenv
│   └── keystone          Stage 2 (runtime): copy virtualenv, add runtime apt packages
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

### venv-builder

**Location:** `images/venv-builder/Dockerfile`

Build-stage image that extends `python-base` with compilation tools and a prepared
Python virtualenv. This image is never deployed — it exists only as a `FROM` target
for multi-stage service builds.

| Property | Value |
| --- | --- |
| Base image | `python-base` (local) |
| Package manager | `uv` 0.6.3 (copied from `ghcr.io/astral-sh/uv:0.6.3`) |
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

The virtualenv includes four packages shared by all OpenStack services:

| Package | Purpose |
| --- | --- |
| `cryptography` | TLS, token encryption, Fernet keys |
| `pymysql` | MySQL/MariaDB database driver |
| `python-memcached` | Memcached client for caching |
| `uwsgi` | WSGI application server |

These packages are installed **without** the `--constraint` flag. The `venv-builder` image
is release-independent — version constraints are release-specific and applied only in
service Dockerfiles when installing the actual service.

## Service Images

### keystone

**Location:** `images/keystone/Dockerfile`

The Keystone identity service image uses a two-stage build:

**Stage 1 (`build`)** — extends `venv-builder`:

- Mounts `upper-constraints.txt` and the Keystone source tree via named build contexts
- Installs Keystone with extras (`ldap`, `memcache_pool`, `oauth1`) into the virtualenv
  using `uv pip install --constraint`

**Stage 2 (runtime)** — extends `python-base`:

- Copies `/var/lib/openstack` from the build stage using `COPY --from=build --link`
  (the `--link` flag enables parallel layer extraction and deduplication)
- Installs runtime system packages required by Keystone's Python dependencies
- Sets `USER openstack` for non-root execution

**Runtime packages:**

| Package | Purpose |
| --- | --- |
| `libapache2-mod-wsgi-py3` | Apache WSGI module for serving Keystone |
| `libldap-2.5-0` | LDAP client library (python-ldap runtime dependency) |
| `libsasl2-2` | SASL authentication library (LDAP SASL bind support) |
| `libxml2` | XML parsing library (lxml runtime dependency) |

**Final image properties:**

- Runs as `openstack` user (UID 42424, GID 42424)
- Contains no build tools (`gcc`, `python3-dev`, `build-essential`, `uv` are absent)
- Virtualenv at `/var/lib/openstack` with all Keystone dependencies
- `keystone-manage` CLI available via `PATH`

## Named Build Contexts

Service Dockerfiles use Docker's named build context feature (`--build-context`) to inject
release-specific files without embedding them in the Dockerfile or using `COPY` from the
build directory. This keeps Dockerfiles release-independent.

The Keystone build requires two named build contexts:

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

Inside the Dockerfile, named build contexts are consumed via `--mount=type=bind,from=`:

```dockerfile
RUN --mount=type=bind,from=upper-constraints,source=upper-constraints.txt,target=/tmp/upper-constraints.txt \
    --mount=type=bind,from=keystone,target=/tmp/keystone \
    uv pip install --prefix /var/lib/openstack \
        --constraint /tmp/upper-constraints.txt \
        "/tmp/keystone[ldap,memcache_pool,oauth1]"
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
  pip_packages:
    - "keystone[ldap]"
    - "keystone[memcache_pool]"
    - "keystone[oauth1]"
  apt_packages:
    - libapache2-mod-wsgi-py3
    - libldap-2.5-0
    - libsasl2-2
    - libxml2
```

| Key | Purpose |
| --- | --- |
| `<service>.pip_packages` | Python extras installed via `uv pip install` in the build stage |
| `<service>.apt_packages` | Runtime system packages installed via `apt` in the final image |

To add packages for a new service, add a new top-level key matching the service name
with both `pip_packages` and `apt_packages` lists.

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

```bash
docker build images/keystone \
  -t c5c3/keystone:28.0.0 \
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

The `# DEVIATION` comment appears in both `images/python-base/Dockerfile` (where the
user is created) and `images/keystone/Dockerfile` (where it is used instead of a
per-service user).
