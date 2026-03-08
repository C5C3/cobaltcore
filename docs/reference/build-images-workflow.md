---
title: Build Images Workflow
quadrant: infrastructure
feature: CC-0007, CC-0029
---

# Build Images Workflow

Reference documentation for the GitHub Actions build-images workflow (CC-0007, CC-0029)
and the verify-container-images workflow (CC-0028). The build-images workflow builds,
tags, and publishes container images for OpenStack services to GHCR (GitHub Container
Registry). Each pushed image receives a CycloneDX SBOM and a Sigstore-signed attestation
(CC-0029). The verify-container-images workflow runs static verification tests against
container infrastructure files (Dockerfiles, workflows, release configs) without
requiring Docker.

## File Locations

| Workflow | Path |
| --- | --- |
| Build Images | `.github/workflows/build-images.yaml` |
| Verify Container Images | `.github/workflows/verify-container-images.yaml` |

Both files use the `.yaml` extension and quote the trigger key as `"on"` to prevent
YAML boolean interpretation (REQ-008). They start with the standard SPDX license header
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

Top-level permissions grant least privilege (REQ-008). Registry write access and
attestation permissions are scoped to the build jobs only:

```yaml
# Top-level (applies to all jobs)
permissions:
  contents: read

# Job-level (build jobs only)
permissions:
  contents: read
  packages: write
  id-token: write       # CC-0029: Sigstore OIDC signing for SBOM attestation
  attestations: write   # CC-0029: GitHub Attestations API

# Verification jobs (verify-base-images, verify-service-images)
permissions:
  contents: read
  packages: read
```

`contents: read` allows repository checkout. `packages: write` is granted only to
`build-base-images` and `build-service-images` for pushing images to GHCR.
`id-token: write` enables Sigstore keyless OIDC signing — the GitHub Actions runner
requests a short-lived OIDC token bound to the workflow identity, which Sigstore uses to
sign the attestation without managing keys (CC-0029). `attestations: write` grants
access to the GitHub Attestations API for storing signed attestations. The verification
jobs (`verify-base-images`, `verify-service-images`) do **not** receive `id-token` or
`attestations` permissions — they only need `contents: read` (for checkout and test
scripts) and `packages: read` (for pulling images from GHCR), following the principle of
least privilege (CC-0028).

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

The workflow defines four jobs with a linear dependency chain (CC-0028):

```text
build-base-images ──> verify-base-images ──> build-service-images ──> verify-service-images (push only)
                                                      │
                                                      └── verify_keystone.sh (PR, inline step)
```

The `verify-base-images` job validates base image properties (Python version, user
UID/GID, PATH, uv version) before service image builds begin. This catches base image
regressions before they cascade into service image failures. On push events, the
`verify-service-images` job validates service images pulled from GHCR. On PRs, the
equivalent verification runs as an inline step within `build-service-images` because
`--load` makes the image available only on the same runner.

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
| 3 | Set up Buildx | `docker/setup-buildx-action@v4` | Enables multi-platform builds |
| 4 | Login to GHCR | `docker/login-action@v4` | Authenticates with `GITHUB_TOKEN` |
| 5 | Build python-base | `docker/build-push-action@v7` | Context: `images/python-base`, multi-arch, push: true, tags: `:latest` and `:${{ github.sha }}` |
| 6 | Generate SBOM for python-base | `anchore/sbom-action@v0` | Skipped on PRs. Scans the just-pushed python-base image by digest. Output: `sbom-python-base.cyclonedx.json` (CC-0029) |
| 7 | Attest SBOM for python-base | `actions/attest@v4` | Skipped on PRs. Signs the SBOM via Sigstore and pushes the attestation to GHCR as an OCI referrer artifact (CC-0029) |
| 8 | Build venv-builder | `docker/build-push-action@v7` | Context: `images/venv-builder`, multi-arch, push: true, tags: `:latest` and `:${{ github.sha }}`, `--build-context python-base=docker-image://...` |
| 9 | Generate SBOM for venv-builder | `anchore/sbom-action@v0` | Skipped on PRs. Scans the just-pushed venv-builder image by digest. Output: `sbom-venv-builder.cyclonedx.json` (CC-0029) |
| 10 | Attest SBOM for venv-builder | `actions/attest@v4` | Skipped on PRs. Signs the SBOM via Sigstore and pushes the attestation to GHCR as an OCI referrer artifact (CC-0029) |

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

These outputs are consumed by `verify-base-images` and `build-service-images` via
`needs.build-base-images.outputs` (REQ-003).

### verify-base-images

Validates that just-built base images meet expected properties before service image
builds begin (CC-0028). Runs after `build-base-images` and blocks
`build-service-images`, forming the dependency chain:
`build-base-images` → `verify-base-images` → `build-service-images`.

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `10` |
| `needs` | `[build-base-images]` |
| Permissions | `contents: read`, `packages: read` |

**Steps:**

| # | Step | Action / Command | Details |
| --- | --- | --- | --- |
| 1 | Checkout | `actions/checkout@v6` | Checks out the repository (needed for test scripts) |
| 2 | Login to GHCR | `docker/login-action@v4` | Authenticates to pull base images |
| 3 | Pull base images | Shell | Pulls both `python-base` and `venv-builder` by digest from `build-base-images` outputs |
| 4 | Verify python-base | Shell | Runs `verify_python_base.sh` with the digest-tagged image ref |
| 5 | Verify venv-builder | Shell | Runs `verify_venv_builder.sh` with the digest-tagged image ref |

**Test scripts executed:**

| Script | Validates |
| --- | --- |
| `tests/container-images/verify_python_base.sh` | Python version, `openstack` user (UID/GID 42424), PATH includes `/opt/openstack/bin`, virtualenv at `/opt/openstack` |
| `tests/container-images/verify_venv_builder.sh` | uv version (from Dockerfile), pip available, virtualenv at `/var/lib/openstack` |

The job receives image references via `needs.build-base-images.outputs` (digest-pinned),
ensuring the exact images that were just built are the ones being tested.

### build-service-images

Builds service images using a matrix strategy over `service x release` (REQ-004).
Depends on `build-base-images` for image references (REQ-003) and on
`verify-base-images` to ensure base images are valid before building on top of them
(CC-0028).

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `30` |
| `needs` | `[build-base-images, verify-base-images]` |
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
| 7 | Derive tags | Shell | Computes three image tags and the lowercase `image` path (see [Tag Schema](#tag-schema)) (REQ-005, CC-0029) |
| 8 | Set up Buildx | `docker/setup-buildx-action@v4` | Enables multi-platform builds |
| 9 | Login to GHCR | `docker/login-action@v4` | Authenticates with `GITHUB_TOKEN` |
| 10 | Build service image | `docker/build-push-action@v7` | Builds with four named build contexts and three build args, conditional platform/push/load (REQ-006) |
| 11 | Generate SBOM for service image | `anchore/sbom-action@v0` | Skipped on PRs. Scans the just-pushed service image by digest. Output: `sbom-${{ matrix.service }}.cyclonedx.json` (CC-0029) |
| 12 | Attest SBOM for service image | `actions/attest@v4` | Skipped on PRs. Signs the SBOM via Sigstore and pushes the attestation to GHCR as an OCI referrer artifact (CC-0029) |
| 13 | Verify service image (PR) | Shell (conditional) | On PRs only: runs `verify_keystone.sh` with the locally loaded image ref (CC-0028) |

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

This job does not declare outputs. The `verify-service-images` job derives its own image
refs independently via its own matrix strategy (CC-0007).

### verify-service-images

Validates that built service images are functional by running `verify_keystone.sh`
(CC-0028). This job replaces the former `smoke-test` job and runs only on push events
(when images are in GHCR). It uses its own matrix strategy matching
`build-service-images` to test every service independently.

On PRs, the equivalent verification runs as an inline step within `build-service-images`
(step 13 above) because `--load` makes the image available only on the same runner.

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `10` |
| `needs` | `[build-service-images]` |
| `if` | `github.event_name != 'pull_request'` |
| Permissions | `contents: read`, `packages: read` |
| Matrix | `service: [keystone]`, `release: ["2025.2"]` |

**Steps:**

| # | Step | Action / Command | Details |
| --- | --- | --- | --- |
| 1 | Checkout | `actions/checkout@v6` | Checks out the repository (needed for test scripts, `source-refs.yaml`, and patch counting) |
| 2 | Login to GHCR | `docker/login-action@v4` | Authenticates to pull the image |
| 3 | Derive image ref | Shell | Reconstructs the composite tag from the same inputs as `build-service-images` |
| 4 | Pull and verify | Shell | `docker pull <image-ref>` then runs `verify_keystone.sh` with the pulled image ref |

**Test script executed:**

| Script | Validates |
| --- | --- |
| `tests/container-images/verify_keystone.sh` | `keystone-manage --version` exits 0, runs as `openstack` user, no build tools (gcc, python3-dev, uv), runtime apt packages installed |

The job fails the workflow if `verify_keystone.sh` exits non-zero. The tag derivation
step must stay in sync with the "Derive tags" step in `build-service-images`.

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
| Base image verification | `verify-base-images` job (always runs) | `verify-base-images` job (always runs) |
| Service image platforms | `linux/amd64` only | `linux/amd64,linux/arm64` |
| Service image push | No (`push: false`, `load: true`) | Yes (`push: true`) |
| Service image tags | Computed but not published | Published to GHCR |
| SBOM generation | Skipped (CC-0029) | CycloneDX JSON for every image |
| SBOM attestation | Skipped (CC-0029) | Sigstore-signed, pushed to GHCR |
| OIDC token request | None | Requested for Sigstore signing |
| Service image verification | Inline step in `build-service-images` | Separate `verify-service-images` job |
| Verification image source | Locally loaded image (same runner) | Pulled from GHCR |

**Why base images are always pushed:** Service Dockerfiles reference base images via
`docker-image://` URIs in build contexts. This Docker BuildKit feature requires the
referenced image to exist in a registry — local images are not sufficient. Pushing
small base images on every PR is a deliberate trade-off to keep Dockerfiles
registry-independent.

**Why PRs use single-arch:** `docker/build-push-action` with `load: true` only supports
single-platform builds. Multi-platform images cannot be loaded into the local Docker
daemon. Since the verification step needs the image locally, PRs build only `linux/amd64`.

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
uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6
uses: docker/setup-buildx-action@4d04d5d9486b7bd6fa91e7baf45bbb4f8b9deedd  # v4
uses: docker/login-action@b45d80f862d83dbcd57f89517bcf500b2ab88fb2  # v4
uses: docker/build-push-action@d08e5c354a6adb9ed34480a06d141179aa583294  # v7
uses: anchore/sbom-action@17ae1740179002c89186b61233e0f892c3118b11  # v0 (CC-0029)
uses: actions/attest@59d89421af93a897026c735860bf21b6eb4f7b26  # v4 (CC-0029)
```

This prevents supply-chain attacks via tag mutation while remaining auditable through
version comments.

## SBOM Generation and Attestation

Every container image pushed to GHCR on non-PR events receives a CycloneDX SBOM and a
Sigstore-signed attestation (CC-0029). This enables consumers to audit image contents,
check for known vulnerabilities, and verify that images have not been tampered with since
build time.

### How It Works

After each `docker/build-push-action` step pushes an image to GHCR, two additional steps
run:

1. **SBOM generation** (`anchore/sbom-action`) — Syft scans the pushed image (referenced
   by digest) and produces a CycloneDX JSON file covering both OS packages (dpkg) and
   Python packages (dist-info).

2. **SBOM attestation** (`actions/attest`) — The CycloneDX file is signed via
   Sigstore keyless OIDC (using the GitHub Actions workflow identity) and pushed to GHCR
   as an OCI referrer artifact alongside the image. No signing keys are managed; the OIDC
   token binds the attestation to the specific workflow run.

This pattern is applied to all three image types:

| Image | SBOM output file | Job |
| --- | --- | --- |
| `python-base` | `sbom-python-base.cyclonedx.json` | `build-base-images` |
| `venv-builder` | `sbom-venv-builder.cyclonedx.json` | `build-base-images` |
| Service (e.g., `keystone`) | `sbom-${{ matrix.service }}.cyclonedx.json` | `build-service-images` |

### PR Behavior

All SBOM generation and attestation steps are guarded with
`if: github.event_name != 'pull_request'`. On pull requests:

- No SBOMs are generated or attested — the `if: github.event_name != 'pull_request'` guard applies uniformly to all SBOM/attestation steps, including for base images which are pushed on PRs.
- No OIDC token requests occur — SBOM/attestation steps are skipped so `id-token: write` is not exercised.
- No attestations are created for ephemeral PR builds.
- PR CI time is not increased by SBOM/attestation steps.

Base images are always pushed to GHCR (even on PRs) because downstream service builds
reference them via `docker-image://` URIs. However, SBOM generation and attestation for
base images are still skipped on PRs — the PR guard applies to all SBOM/attestation
steps uniformly.

### Required Permissions

SBOM attestation requires two additional job-level permissions beyond the existing
`contents: read` and `packages: write`:

| Permission | Purpose |
| --- | --- |
| `id-token: write` | Allows the GitHub Actions runner to request a short-lived Sigstore OIDC token for keyless signing |
| `attestations: write` | Grants access to the GitHub Attestations API for storing signed attestations |

These permissions are granted only to `build-base-images` and `build-service-images`.
Verification jobs (`verify-base-images`, `verify-service-images`) do not receive these
permissions.

### Verifying Attestations

To verify that an image has a valid Sigstore-signed attestation:

```bash
gh attestation verify oci://ghcr.io/<owner>/<service>:<tag> --owner <owner>
```

To extract the SBOM predicate from the attestation, first inspect the raw
output to find the exact `predicateType` URI used in your environment:

```bash
gh attestation verify oci://ghcr.io/<owner>/<service>:<tag> \
  --owner <owner> \
  --format json | jq '.[].verificationResult.statement.predicateType'
```

Then filter for that predicate type (the URI below may differ by action version):

```bash
gh attestation verify oci://ghcr.io/<owner>/<service>:<tag> \
  --owner <owner> \
  --format json | jq -r '
    [ .[] | select(.verificationResult.statement.predicateType | test("cyclonedx")) ]
    | if length == 0 then error("no CycloneDX attestation found") else .[0].verificationResult.statement.predicate end
  '
```

### Test Coverage

The `verify_build_images_workflow.sh` script validates SBOM/attestation configuration
(CC-0029):

| Test | Validates |
| --- | --- |
| `test_sbom_permissions_on_build_base_images` | `id-token: write` and `attestations: write` on `build-base-images` |
| `test_sbom_permissions_on_build_service_images` | `id-token: write` and `attestations: write` on `build-service-images` |
| `test_verify_jobs_no_sbom_permissions` | Verification jobs do **not** have `id-token` or `attestations` permissions |
| `test_sbom_generation_steps_exist` | SBOM generation steps exist in both build jobs |
| `test_sbom_format_cyclonedx_json` | All SBOM steps specify `format: cyclonedx-json` |
| `test_sbom_generation_references_digest` | SBOM steps reference the correct digest output |
| `test_sbom_attestation_steps_exist` | Attestation steps exist in both build jobs |
| `test_sbom_attestation_push_to_registry` | All attestation steps have `push-to-registry: true` |
| `test_sbom_steps_pr_skip_guard` | All SBOM/attestation steps have `github.event_name != 'pull_request'` guard |

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

Add the service to both the `build-service-images` and `verify-service-images` matrices
in `.github/workflows/build-images.yaml`:

```yaml
# build-service-images job:
strategy:
  matrix:
    service: [keystone, nova]    # ← add here
    release: ["2025.2"]

# verify-service-images job (must mirror build-service-images):
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

The tag derivation, build context resolution, and verification steps all use matrix
variables and work automatically for new services. The `verify-service-images` job
derives its own image refs independently via its own matrix strategy. Note that adding a
new service also requires creating a corresponding `verify_<service>.sh` test script in
`tests/container-images/` and updating the inline PR verification step in
`build-service-images` accordingly.

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

## Verify Container Images Workflow

A separate workflow (`.github/workflows/verify-container-images.yaml`) runs static
verification tests against container infrastructure files without requiring Docker
(CC-0028). This workflow validates Dockerfiles, workflow structure, SPDX compliance,
release configuration, and constraint override scripts.

### Trigger Events

The workflow triggers on the same events as `build-images.yaml`:

| Event | Scope | Description |
| --- | --- | --- |
| `push` | `branches: [main, stable/**]` | Runs on every push to `main` or any `stable/**` branch |
| `pull_request` | all branches | Runs on every pull request |

### Permissions and Concurrency

Top-level permissions are `contents: read` (least privilege). The concurrency group
follows the standard pattern: `${{ github.ref }}-${{ github.workflow }}` with
`cancel-in-progress` limited to pull request events.

### Job: verify-static-tests

A single job that runs all static test scripts sequentially. If any script exits
non-zero, the job fails.

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `10` |

**Steps:**

| # | Step | Action / Command | Details |
| --- | --- | --- | --- |
| 1 | Checkout | `actions/checkout@v6` | Checks out the repository |
| 2 | Install yq | Shell | Downloads `yq` binary from GitHub releases (version-pinned via `YQ_VERSION` env var) |
| 3 | Verify build-images workflow structure | Shell | Runs `tests/container-images/verify_build_images_workflow.sh` |
| 4 | Verify deviation comments | Shell | Runs `tests/container-images/verify_deviation_comments.sh` |
| 5 | Verify release config | Shell | Runs `tests/container-images/verify_release_config.sh` |
| 6 | Verify SPDX headers | Shell | Runs `tests/container-images/verify_spdx_headers.sh` |
| 7 | Test apply-constraint-overrides | Shell | Runs `tests/scripts/test_apply_constraint_overrides.sh` |

**Test scripts executed:**

| Script | Validates |
| --- | --- |
| `verify_build_images_workflow.sh` | Workflow structure: job names, dependency chain, trigger events, permissions, action pinning, concurrency, matrix strategy, SBOM/attestation configuration (CC-0029) |
| `verify_deviation_comments.sh` | DEVIATION comments in Dockerfiles cross-reference architecture docs |
| `verify_release_config.sh` | `source-refs.yaml` and `extra-packages.yaml` structure and content validity |
| `verify_spdx_headers.sh` | SPDX Apache-2.0 license headers present on all infrastructure files |
| `test_apply_constraint_overrides.sh` | `scripts/apply-constraint-overrides.sh` correctly applies constraint overrides |

yq is installed before the test scripts because `verify_build_images_workflow.sh` and
`verify_release_config.sh` use `yq` to parse YAML files. The installation uses a direct
binary download from the mikefarah/yq GitHub releases (more reliable across runner
updates than snap).

### Relationship to build-images.yaml

The verify-container-images workflow is intentionally separate from `build-images.yaml`
because it tests container _infrastructure_ (file structure, conventions, configs) rather
than the container _images_ themselves. Separate workflows provide clear, independent
signals in GitHub's check status UI. The Docker-based image verification tests
(`verify_python_base.sh`, `verify_venv_builder.sh`, `verify_keystone.sh`) run inside
`build-images.yaml` where the images are actually built.

## Verification Coverage Summary

The following table summarizes which test scripts run where (CC-0028, CC-0029):

| Test Script | verify-container-images.yaml | build-images.yaml | Requires Docker |
| --- | --- | --- | --- |
| `verify_build_images_workflow.sh` | verify-static-tests | — | No |
| `verify_deviation_comments.sh` | verify-static-tests | — | No |
| `verify_release_config.sh` | verify-static-tests | — | No |
| `verify_spdx_headers.sh` | verify-static-tests | — | No |
| `test_apply_constraint_overrides.sh` | verify-static-tests | — | No |
| `verify_python_base.sh` | — | verify-base-images | Yes |
| `verify_venv_builder.sh` | — | verify-base-images | Yes |
| `verify_keystone.sh` | — | build-service-images (PR) / verify-service-images (push) | Yes |

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
