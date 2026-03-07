---
title: Build Images Workflow
quadrant: infrastructure
feature: CC-0007
---

# Build Images Workflow

Reference documentation for the GitHub Actions build-images workflow (CC-0007). This
workflow builds, tags, and publishes container images for OpenStack services to GHCR
(GitHub Container Registry).

## File Location

`.github/workflows/build-images.yaml`

The file uses the `.yaml` extension and quotes the trigger key as `"on"` to prevent
YAML boolean interpretation (REQ-008). It starts with the standard SPDX license header
(matching `ci.yaml`).

## Trigger Events

The workflow triggers on two events (REQ-001):

| Event | Scope | Description |
| --- | --- | --- |
| `push` | `branches: [main, stable/**]` | Runs on every push to `main` or any `stable/**` branch (recursive glob) |
| `pull_request` | all branches | Runs on every pull request |

Push events produce multi-arch images pushed to GHCR. Pull request events produce
single-arch images loaded locally for testing (see [PR vs Push Behavior](#pr-vs-push-behavior)).

> **Fork PRs are not supported.** Base images must be pushed to GHCR on every run
> (because downstream `docker-image://` URIs require registry availability), but fork
> PRs receive a read-only `GITHUB_TOKEN` that cannot write packages. The workflow
> detects fork PRs and fails fast with a clear error message (CC-0007).

## Permissions

Top-level permissions grant least privilege (REQ-008). Registry write access is scoped
to the build jobs that push images:

```yaml
# Top-level (applies to all jobs)
permissions:
  contents: read

# Job-level (build jobs only)
permissions:
  contents: read
  packages: write

# smoke-test job
permissions:
  contents: read
  packages: read
```

`contents: read` allows repository checkout. `packages: write` is granted only to
`build-base-images` and `build-service-images` for pushing images to GHCR. The
`smoke-test` job receives `contents: read` (required for its checkout step to access
`source-refs.yaml` and count patches) and `packages: read` (for pulling the image
from GHCR), following the principle of least privilege.

## Concurrency

The workflow uses a concurrency group scoped per-branch per-workflow (REQ-008):

```yaml
concurrency:
  group: ${{ github.ref }}-${{ github.workflow }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
```

For pull requests, pushing new commits cancels any in-progress build for that same PR
branch. For pushes to `main` or `stable/**`, in-progress runs are **not** cancelled,
ensuring every merge commit produces a complete set of images.

## Jobs

The workflow defines three jobs with a linear dependency chain:

```text
build-base-images  ──>  build-service-images  ──>  smoke-test (push only)
                                │
                                └── Smoke test (PR, inline step)
```

### build-base-images

Builds the two base images (`python-base` and `venv-builder`) sequentially and pushes
them to GHCR (REQ-002). These must always be pushed — even on PRs — because downstream
service builds reference them via `docker-image://` URIs, which require registry
availability.

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `30` |
| `needs` | *(none — first job)* |
| Architectures | `linux/amd64,linux/arm64` (always multi-arch) |
| Push behavior | Always pushes to GHCR (even on PRs) |

**Steps:**

| # | Step | Action / Command | Details |
| --- | --- | --- | --- |
| 1 | Reject fork PRs | Shell (conditional) | Fails fast with `::error::` if the PR originates from a fork (CC-0007) |
| 2 | Checkout | `actions/checkout@v6` | Checks out the repository |
| 3 | Set up Buildx | `docker/setup-buildx-action@v3` | Enables multi-platform builds |
| 4 | Login to GHCR | `docker/login-action@v3` | Authenticates with `GITHUB_TOKEN` |
| 5 | Build python-base | `docker/build-push-action@v6` | Context: `images/python-base`, multi-arch, push: true, tags: `:latest` and `:${{ github.sha }}` |
| 6 | Build venv-builder | `docker/build-push-action@v6` | Context: `images/venv-builder`, multi-arch, push: true, tags: `:latest` and `:${{ github.sha }}`, `--build-context python-base=docker-image://...` |

The `venv-builder` build uses a `docker-image://` build context pointing at the
just-pushed `python-base` image (referenced by digest), ensuring its `FROM python-base`
directive resolves to the exact image built in step 5.

Each base image is tagged with both `:latest` (mutable convenience tag) and
`:${{ github.sha }}` (immutable commit-pinned tag). The SHA tag provides an auditable
mapping from any base image in GHCR back to the commit that produced it (CC-0007).

**Outputs:**

| Output | Format | Example |
| --- | --- | --- |
| `python-base-image` | `ghcr.io/<owner>/python-base@sha256:<digest>` | `ghcr.io/c5c3/python-base@sha256:abc123...` |
| `venv-builder-image` | `ghcr.io/<owner>/venv-builder@sha256:<digest>` | `ghcr.io/c5c3/venv-builder@sha256:def456...` |

These outputs are consumed by `build-service-images` via `needs.build-base-images.outputs`
(REQ-003).

### build-service-images

Builds service images using a matrix strategy over `service x release` (REQ-004).
Depends on `build-base-images` to provide base image references (REQ-003).

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `30` |
| `needs` | `[build-base-images]` |
| Matrix | `service: [keystone]`, `release: ["2025.2"]` |

**Steps:**

| # | Step | Action / Command | Details |
| --- | --- | --- | --- |
| 1 | Checkout | `actions/checkout@v6` | Checks out this repository |
| 2 | Resolve source ref | Shell | Reads `releases/<release>/source-refs.yaml` via `yq` to get the upstream version tag |
| 3 | Checkout service source | `actions/checkout@v6` | Clones `openstack/<service>` at the resolved ref into `src/<service>` |
| 4 | Apply patches | Shell (conditional) | Runs `git apply` for patches in `patches/<service>/<release>/` — skipped if no `.patch` files exist |
| 5 | Apply constraint overrides | Shell | Runs `scripts/apply-constraint-overrides.sh <release>` (idempotent) |
| 6 | Resolve extra packages | Shell | Reads `releases/<release>/extra-packages.yaml` via `yq` to extract `pip_extras` (comma-joined), `pip_packages` (space-joined), and `apt_packages` (space-joined). All three fields tolerate empty values — the Dockerfile handles them via conditional guards (CC-0027). |
| 7 | Derive tags | Shell | Computes three image tags (see [Tag Schema](#tag-schema)) (REQ-005) |
| 8 | Set up Buildx | `docker/setup-buildx-action@v3` | Enables multi-platform builds |
| 9 | Login to GHCR | `docker/login-action@v3` | Authenticates with `GITHUB_TOKEN` |
| 10 | Build service image | `docker/build-push-action@v6` | Builds with four named build contexts and three build args, conditional platform/push/load (REQ-006) |
| 11 | Smoke test (PR) | Shell (conditional) | On PRs only: `docker run --rm <image> <service>-manage --version` — uses `matrix.service` for dynamic dispatch (REQ-007) |

**Build Contexts:**

The service image build passes four named build contexts to resolve `FROM` and
`--mount=type=bind,from=` directives in the Dockerfile:

| Context name | Source | Purpose |
| --- | --- | --- |
| `python-base` | `docker-image://<python-base-image>` | Runtime base image (output already includes `@sha256:<digest>`) |
| `venv-builder` | `docker-image://<venv-builder-image>` | Build stage image (output already includes `@sha256:<digest>`) |
| `<service>` | `src/<service>` | Service source tree (upstream checkout) |
| `upper-constraints` | `releases/<release>/` | Release directory containing `upper-constraints.txt` |

**Build Arguments (CC-0027):**

The service image build passes three build arguments sourced from
`releases/<release>/extra-packages.yaml` by the "Resolve extra packages" step:

| Build arg | Source field | Format | Example |
| --- | --- | --- | --- |
| `PIP_EXTRAS` | `<service>.pip_extras` | Comma-separated | `ldap,oauth1` |
| `PIP_PACKAGES` | `<service>.pip_packages` | Space-separated | *(empty by default)* |
| `EXTRA_APT_PACKAGES` | `<service>.apt_packages` | Space-separated | `libapache2-mod-wsgi-py3 libldap2 libsasl2-2 libxml2` |

`PIP_EXTRAS` and `PIP_PACKAGES` are consumed in the Dockerfile build stage (stage 1).
`EXTRA_APT_PACKAGES` is consumed in the runtime stage (stage 2). See
[Container Images — extra-packages.yaml](container-images.md#extra-packagesyaml) for the
YAML schema.

This job does not declare outputs. The `smoke-test` job derives its own image refs
independently via its own matrix strategy (CC-0007).

### smoke-test

Validates that built service images are functional by running
`<service>-manage --version` (REQ-007). This job runs only on push events (when images
are in GHCR) and uses its own matrix strategy matching `build-service-images` to test
every service independently.

On PRs, the equivalent smoke test runs as an inline step within `build-service-images`
(step 11 above) because `--load` makes the image available only on the same runner.

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `10` |
| `needs` | `[build-service-images]` |
| `if` | `github.event_name != 'pull_request'` |
| Matrix | `service: [keystone]`, `release: ["2025.2"]` |

**Steps:**

| # | Step | Action / Command | Details |
| --- | --- | --- | --- |
| 1 | Checkout | `actions/checkout@v6` | Checks out the repository (needed for `source-refs.yaml` and patch counting) |
| 2 | Login to GHCR | `docker/login-action@v3` | Authenticates to pull the image |
| 3 | Derive image ref | Shell | Reconstructs the composite tag from the same inputs as `build-service-images` |
| 4 | Pull and test | Shell | `docker pull <image-ref>` then `docker run --rm <image-ref> <service>-manage --version` |

The job fails the workflow if `<service>-manage --version` returns a non-zero exit code.
The management command is derived dynamically from `matrix.service`, so new services are
automatically tested when added to the matrix.

## Tag Schema

Each service image build produces two or three tags on push events (REQ-005):

| Tag | Format | Example | Branches |
| --- | --- | --- | --- |
| Composite | `<version>-p<N>-<branch>-<sha>` | `keystone:28.0.0-p0-main-a1b2c3d` | all |
| Version | `<version>` | `keystone:28.0.0` | `main` only |
| SHA | `<sha>` | `keystone:a1b2c3d` | all |

The version-only tag is restricted to the `main` branch to prevent silent overwrites when
multiple branches build the same upstream version. The composite tag already encodes the
branch, so `stable/**` builds remain uniquely identifiable (CC-0007).

**Tag components:**

| Component | Source | Description |
| --- | --- | --- |
| `<version>` | `releases/<release>/source-refs.yaml` | Upstream OpenStack version tag (e.g., `28.0.0`) |
| `p<N>` | Count of `.patch` files in `patches/<service>/<release>/` | Patch count; defaults to `p0` when the directory is absent |
| `<branch>` | `GITHUB_REF_NAME` with `/` replaced by `-` | Branch name, sanitized for Docker tag compatibility (e.g., `stable/2025.2` becomes `stable-2025.2`) |
| `<sha>` | First 7 characters of `GITHUB_SHA` | Short commit SHA |

The composite tag uniquely identifies the exact build: upstream version, patch level,
branch, and commit. The version and SHA tags provide convenient shortcuts for deployment
systems.

## PR vs Push Behavior

The workflow behaves differently depending on the trigger event (REQ-006):

| Aspect | Pull Request | Push (main / stable/**) |
| --- | --- | --- |
| Base images | Multi-arch, pushed to GHCR | Multi-arch, pushed to GHCR |
| Service image platforms | `linux/amd64` only | `linux/amd64,linux/arm64` |
| Service image push | No (`push: false`, `load: true`) | Yes (`push: true`) |
| Service image tags | Computed but not published | Published to GHCR |
| Smoke test location | Inline step in `build-service-images` | Separate `smoke-test` job |
| Smoke test image source | Locally loaded image (same runner) | Pulled from GHCR |

**Why base images are always pushed:** Service Dockerfiles reference base images via
`docker-image://` URIs in build contexts. This Docker BuildKit feature requires the
referenced image to exist in a registry — local images are not sufficient. Pushing
small base images on every PR is a deliberate trade-off to keep Dockerfiles
registry-independent.

**Why PRs use single-arch:** `docker/build-push-action` with `load: true` only supports
single-platform builds. Multi-platform images cannot be loaded into the local Docker
daemon. Since the smoke test needs the image locally, PRs build only `linux/amd64`.

## GHA Caching

All `docker/build-push-action` steps use GitHub Actions cache for Docker layers
(REQ-009):

```yaml
cache-from: type=gha,scope=<scope>
cache-to: type=gha,mode=max,scope=<scope>
```

Each image has a unique cache scope to prevent collisions:

| Image | Scope |
| --- | --- |
| `python-base` | `python-base` |
| `venv-builder` | `venv-builder` |
| Service images | `<service>-<release>` (e.g., `keystone-2025.2`) |

The `mode=max` setting caches all intermediate layers, not just the final image layer.

## Action Pinning

All actions are pinned to full commit SHAs with version comments (REQ-008), matching the
convention in `ci.yaml`:

```yaml
uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6
uses: docker/setup-buildx-action@8d2750c68a42422c14e847fe6c8ac0403b4cbd6f # v3
uses: docker/login-action@c94ce9fb468520275223c153574b00df6fe4bcc9 # v3
uses: docker/build-push-action@10e90e3645eae34f1e60eeb005ba3a3d33f178e8 # v6
```

This prevents supply-chain attacks via tag mutation while remaining auditable through
version comments.

## Adding a New Service

To add a new service (e.g., `nova`) to the build matrix:

### 1. Create the Dockerfile

Add a Dockerfile at `images/<service>/Dockerfile` following the two-stage pattern in
`images/keystone/Dockerfile`. The Dockerfile must use named build contexts
(`python-base`, `venv-builder`, `<service>`, `upper-constraints`) — not hardcoded paths.

### 2. Add the source ref

Add the service to `releases/<release>/source-refs.yaml`:

```yaml
keystone: "28.0.0"
nova: "31.0.0"        # ← new entry
```

### 3. Add extra-packages entry

Add the service to `releases/<release>/extra-packages.yaml` with its Python extras,
additional pip packages, and runtime system packages. This file is the source of truth
for build arguments `PIP_EXTRAS`, `PIP_PACKAGES`, and `EXTRA_APT_PACKAGES` — the
service will not build without an entry here.

```yaml
nova:
  pip_extras:
    - oslo_vmware
  pip_packages: []
  apt_packages:
    - libvirt0
```

### 4. Extend the matrices

Add the service to both the `build-service-images` and `smoke-test` matrices in
`.github/workflows/build-images.yaml`:

```yaml
# build-service-images job:
strategy:
  matrix:
    service: [keystone, nova]    # ← add here
    release: ["2025.2"]

# smoke-test job (must mirror build-service-images):
strategy:
  matrix:
    service: [keystone, nova]    # ← add here too
    release: ["2025.2"]
```

### 5. (Optional) Add patches

If the service requires patches, create patch files at
`patches/<service>/<release>/*.patch`. The workflow applies them automatically when
present; no workflow changes are needed.

### 6. (Optional) Add constraint overrides

If the service requires constraint overrides, add entries to
`overrides/<release>/constraints.txt`. The `apply-constraint-overrides.sh` script
processes them automatically.

The tag derivation, build context resolution, and smoke test commands all use matrix
variables and work automatically for new services. The smoke test dynamically constructs
`<service>-manage --version` from `matrix.service`. The `smoke-test` job derives its own
image refs independently via its own matrix strategy.

## Adding a New Release

To add a new release series (e.g., `2026.1`):

### 1. Create release configuration

Create the release directory with required files:

```text
releases/2026.1/
├── extra-packages.yaml       # Extra pip/apt packages per service (CC-0027)
├── source-refs.yaml          # Service versions for this release
└── upper-constraints.txt     # From openstack/requirements stable/2026.1
```

`extra-packages.yaml` is required — the workflow reads it to resolve `PIP_EXTRAS`,
`PIP_PACKAGES`, and `EXTRA_APT_PACKAGES` build arguments. See
[Container Images — extra-packages.yaml](container-images.md#extra-packagesyaml) for the
YAML schema and `releases/2025.2/extra-packages.yaml` for a working example.

### 2. Extend the matrix

Add the release to the `build-service-images` matrix:

```yaml
strategy:
  matrix:
    service: [keystone]
    release: ["2025.2", "2026.1"]    # ← add here
```

### 3. (Optional) Add patches and overrides

Create `patches/<service>/2026.1/` and `overrides/2026.1/constraints.txt` as needed.

## SPDX Header

The file starts with the standard SPDX license header (REQ-008):

```text
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
---
```

## Dependencies on CC-0006

The build-images workflow depends on artifacts introduced by CC-0006:

| Artifact | Used by | Purpose |
| --- | --- | --- |
| `images/python-base/Dockerfile` | `build-base-images` | Python runtime base image |
| `images/venv-builder/Dockerfile` | `build-base-images` | Build-stage image with uv and compilers |
| `images/keystone/Dockerfile` | `build-service-images` | Keystone service image (two-stage build) |
| `releases/2025.2/source-refs.yaml` | `build-service-images` | Upstream version resolution |
| `releases/2025.2/upper-constraints.txt` | `build-service-images` | Python dependency pins |
| `scripts/apply-constraint-overrides.sh` | `build-service-images` | Constraint override application |
