---
title: Build Images Workflow
quadrant: infrastructure
feature: CC-0007, CC-0029, CC-0030, CC-0031, CC-0032
---

::: v-pre

# Build Images Workflow

Reference documentation for the GitHub Actions build-images workflow (CC-0007, CC-0029,
CC-0030, CC-0031, CC-0032) and the verify-container-images workflow (CC-0028). The
build-images workflow builds, tags, and publishes container images for OpenStack services
to GHCR (GitHub Container Registry). Each pushed image receives OCI Image Spec
annotations (CC-0031), a CycloneDX SBOM, a Sigstore-signed attestation (CC-0029), and a
Grype vulnerability scan with SARIF upload to the GitHub Security tab (CC-0032). The
verify-container-images workflow runs static verification tests against container
infrastructure files (Dockerfiles, workflows, release configs) without requiring Docker.

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
  id-token: write         # CC-0029, CC-0030: Sigstore OIDC signing for SBOM attestation and cosign signing
  attestations: write     # CC-0029: GitHub Attestations API
  security-events: write  # CC-0032: GitHub Security tab SARIF upload

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
access to the GitHub Attestations API for storing signed attestations.
`security-events: write` allows uploading Grype vulnerability scan results in SARIF
format to the GitHub Security tab (CC-0032). The verification jobs (`verify-base-images`,
`verify-service-images`) do **not** receive `id-token`, `attestations`, or
`security-events` permissions — they only need `contents: read` (for checkout and test
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
                                                      └── verify_<service>.sh (PR, inline step)
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

::: v-pre

| # | Step | Action / Command | Details |
| --- | --- | --- | --- |
| 1 | Reject fork PRs | Shell (conditional) | Fails fast with `::error::` if the PR originates from a fork (CC-0007) |
| 2 | Checkout | `actions/checkout@v6` | Checks out the repository |
| 3 | Normalize image owner | Shell script | Outputs lowercase `owner` value from `${{ github.repository_owner }}` for use in image references (CC-0007) |
| 4 | Set up Buildx | `docker/setup-buildx-action@v4` | Enables multi-platform builds |
| 5 | Login to GHCR | `docker/login-action@v4` | Authenticates with `GITHUB_TOKEN` |
| 6 | Install cosign | `sigstore/cosign-installer@v4` | Installs cosign for image signing (CC-0030) |
| 7 | Generate metadata for python-base | `docker/metadata-action@v5` | Produces OCI labels (title, description, licenses, vendor) for python-base (CC-0031) |
| 8 | Build python-base | `docker/build-push-action@v7` | Context: `images/python-base`, multi-arch, push: true, tags: `:latest` and `:${{ github.sha }}`, labels from step 7 |
| 9 | Generate SBOM for python-base | `anchore/sbom-action@v0` | Skipped on PRs. Scans the just-pushed python-base image by digest. Output: `sbom-python-base.cyclonedx.json` (CC-0029) |
| 10 | Scan python-base for vulnerabilities | `anchore/scan-action@v7` | Scans via SBOM on push, via image on PR. Reports high/critical CVEs without failing the build (`fail-build: false`). Output: SARIF (CC-0032) |
| 11 | Upload SARIF for python-base | `github/codeql-action/upload-sarif@v3` | Runs always when SARIF output exists (`if: always() && outputs.sarif != ''`). Uploads Grype results to GitHub Security tab with category `grype-python-base` (CC-0032) |
| 12 | Attest SBOM for python-base | `actions/attest@v4` | Skipped on PRs. Signs the SBOM via Sigstore and pushes the attestation to GHCR as an OCI referrer artifact (CC-0029) |
| 13 | Sign python-base | Shell | Skipped on PRs. Signs python-base by digest with cosign keyless OIDC (`cosign sign --yes`) (CC-0030) |
| 14 | Generate metadata for venv-builder | `docker/metadata-action@v5` | Produces OCI labels (title, description, licenses, vendor) for venv-builder (CC-0031) |
| 15 | Build venv-builder | `docker/build-push-action@v7` | Context: `images/venv-builder`, multi-arch, push: true, tags: `:latest` and `:${{ github.sha }}`, labels from step 14, `--build-context python-base=docker-image://...` |
| 16 | Generate SBOM for venv-builder | `anchore/sbom-action@v0` | Skipped on PRs. Scans the just-pushed venv-builder image by digest. Output: `sbom-venv-builder.cyclonedx.json` (CC-0029) |
| 17 | Scan venv-builder for vulnerabilities | `anchore/scan-action@v7` | Scans via SBOM on push, via image on PR. Reports high/critical CVEs without failing the build (`fail-build: false`). Output: SARIF (CC-0032) |
| 18 | Upload SARIF for venv-builder | `github/codeql-action/upload-sarif@v3` | Runs always when SARIF output exists (`if: always() && outputs.sarif != ''`). Uploads Grype results to GitHub Security tab with category `grype-venv-builder` (CC-0032) |
| 19 | Attest SBOM for venv-builder | `actions/attest@v4` | Skipped on PRs. Signs the SBOM via Sigstore and pushes the attestation to GHCR as an OCI referrer artifact (CC-0029) |
| 20 | Sign venv-builder | Shell | Skipped on PRs. Signs venv-builder by digest with cosign keyless OIDC (`cosign sign --yes`) (CC-0030) |

:::

The `venv-builder` build uses a `docker-image://` build context pointing at the
just-pushed `python-base` image (referenced by digest), ensuring its `FROM python-base`
directive resolves to the exact image built in step 7.

::: v-pre

Each base image is tagged with both `:latest` (mutable convenience tag) and
`:${{ github.sha }}` (immutable commit-pinned tag). The SHA tag provides an auditable
mapping from any base image in GHCR back to the commit that produced it (CC-0007).

:::

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

::: v-pre

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
| 10 | Install cosign | `sigstore/cosign-installer@v4` | Installs cosign for image signing (CC-0030) |
| 11 | Generate metadata for service image | `docker/metadata-action@v5` | Produces OCI labels and overrides version to the upstream release ref via `type=raw` strategy (CC-0031) |
| 12 | Build service image | `docker/build-push-action@v7` | Builds with four named build contexts and three build args, conditional platform/push/load, labels from step 11 (REQ-006) |
| 13 | Generate SBOM for service image | `anchore/sbom-action@v0` | Skipped on PRs. Scans the just-pushed service image by digest. Output: `sbom-${{ matrix.service }}.cyclonedx.json` (CC-0029) |
| 14 | Scan service image for vulnerabilities | `anchore/scan-action@v7` | Scans via SBOM on push, via composite tag on PR. Reports high/critical CVEs without failing the build (`fail-build: false`). Output: SARIF (CC-0032) |
| 15 | Upload SARIF for service image | `github/codeql-action/upload-sarif@v3` | Runs always when SARIF output exists (`if: always() && outputs.sarif != ''`). Uploads Grype results to GitHub Security tab with category `grype-${{ matrix.service }}` (CC-0032) |
| 16 | Attest SBOM for service image | `actions/attest@v4` | Skipped on PRs. Signs the SBOM via Sigstore and pushes the attestation to GHCR as an OCI referrer artifact (CC-0029) |
| 17 | Sign service image | Shell | Skipped on PRs. Signs service image by digest with cosign keyless OIDC (`cosign sign --yes`) (CC-0030) |
| 18 | Verify service image (PR) | Shell (conditional) | On PRs only: runs `verify_${{ matrix.service }}.sh` with the locally loaded image ref (CC-0028) |

:::

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

::: v-pre

Validates that built service images are functional by running `verify_${{ matrix.service }}.sh`
(CC-0028). This job replaces the former `smoke-test` job and runs only on push events
(when images are in GHCR). It uses its own matrix strategy matching
`build-service-images` to test every service independently.

:::

On PRs, the equivalent verification runs as an inline step within `build-service-images`
(step 14 above) because `--load` makes the image available only on the same runner.

| Property | Value |
| --- | --- |
| `runs-on` | `ubuntu-latest` |
| `timeout-minutes` | `10` |
| `needs` | `[build-service-images]` |
| `if` | `github.event_name != 'pull_request'` |
| Permissions | `contents: read`, `packages: read` |
| Matrix | `service: [keystone]`, `release: ["2025.2"]` |

**Steps:**

::: v-pre

| # | Step | Action / Command | Details |
| --- | --- | --- | --- |
| 1 | Checkout | `actions/checkout@v6` | Checks out the repository (needed for test scripts, `source-refs.yaml`, and patch counting) |
| 2 | Login to GHCR | `docker/login-action@v4` | Authenticates to pull the image |
| 3 | Derive image ref | Shell | Reconstructs the composite tag from the same inputs as `build-service-images` |
| 4 | Pull and verify | Shell | `docker pull <image-ref>` then runs `verify_${{ matrix.service }}.sh` with the pulled image ref |

:::

**Test script executed:**

| Script | Validates |
| --- | --- |
| `tests/container-images/verify_<service>.sh` | Service-specific checks (e.g. `keystone-manage --version` exits 0), runs as `openstack` user, no build tools (gcc, python3-dev, uv), runtime apt packages installed |

The job fails the workflow if the verify script exits non-zero. The tag derivation
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
| Vulnerability scanning | Image-based scan via `image:` input (CC-0032) | SBOM-based scan via `sbom:` input (CC-0032) |
| SBOM attestation | Skipped (CC-0029) | Sigstore-signed, pushed to GHCR |
| Cosign signing | Skipped (CC-0030) | Keyless signature for every image |
| OIDC token request | None | Requested for Sigstore signing (CC-0029, CC-0030) |
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
uses: docker/metadata-action@c299e40c65443455700f0fdfc63efafe5b349051  # v5 (CC-0031)
uses: docker/build-push-action@d08e5c354a6adb9ed34480a06d141179aa583294  # v7
uses: anchore/sbom-action@17ae1740179002c89186b61233e0f892c3118b11  # v0 (CC-0029)
uses: anchore/scan-action@7037fa011853d5a11690026fb85feee79f4c946c  # v7 (CC-0032)
uses: actions/attest@59d89421af93a897026c735860bf21b6eb4f7b26  # v4 (CC-0029)
uses: github/codeql-action/upload-sarif@820e3160e279568db735cee8ed8f8e77a6da7818  # v3 (CC-0032)
uses: sigstore/cosign-installer@faadad0cce49287aee09b3a48701e75088a2c6ad  # v4 (CC-0030)
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

::: v-pre

| Image | SBOM output file | Job |
| --- | --- | --- |
| `python-base` | `sbom-python-base.cyclonedx.json` | `build-base-images` |
| `venv-builder` | `sbom-venv-builder.cyclonedx.json` | `build-base-images` |
| Service (e.g., `keystone`) | `sbom-${{ matrix.service }}.cyclonedx.json` | `build-service-images` |

:::

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
| `id-token: write` | Allows the GitHub Actions runner to request a short-lived Sigstore OIDC token for keyless signing (CC-0029, CC-0030) |
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

## Cosign Image Signing

Every container image pushed to GHCR on non-PR events is signed with cosign keyless
signing (CC-0030). This provides an independent signature layer alongside the SBOM
attestation (CC-0029), enabling consumers to verify image provenance using standard
Sigstore tooling.

### How It Works

Each build job (`build-base-images`, `build-service-images`) installs cosign via
`sigstore/cosign-installer` and runs `cosign sign --yes` after the image is pushed:

1. **cosign-installer** (`sigstore/cosign-installer@v4`) — Installs the `cosign` binary
   on the runner.

2. **cosign sign** — Signs the image by digest using Sigstore keyless OIDC. The `--yes`
   flag confirms non-interactive mode. No signing keys are managed; the GitHub Actions
   OIDC token binds the signature to the specific workflow run.

This pattern is applied to all three image types:

| Image | Job | Digest source |
| --- | --- | --- |
| `python-base` | `build-base-images` | `steps.build-python-base.outputs.digest` |
| `venv-builder` | `build-base-images` | `steps.build-venv-builder.outputs.digest` |
| Service (e.g., `keystone`) | `build-service-images` | `steps.build-service.outputs.digest` |

### PR Behavior

All cosign sign steps are guarded with `if: github.event_name != 'pull_request'`. On
pull requests:

- No images are signed — the PR guard applies to all cosign sign steps.
- No OIDC token requests occur for signing.
- The `id-token: write` permission (shared with SBOM attestation) is not exercised.

### Required Permissions

Cosign keyless signing reuses the same `id-token: write` permission required by SBOM
attestation (CC-0029). No additional permissions are needed.

### Verifying Signatures

To verify that an image has a valid cosign signature:

```bash
cosign verify \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp "https://github.com/<owner>/<repo>/.github/workflows/build-images.yaml@refs/.*" \
  ghcr.io/<owner>/<service>@sha256:<digest>
```

### Test Coverage

The `verify_build_images_workflow.sh` script validates cosign signing configuration
(CC-0030):

| Test | Validates |
| --- | --- |
| `test_cosign_installer_in_build_base_images` | `sigstore/cosign-installer` step exists in `build-base-images` |
| `test_cosign_installer_in_build_service_images` | `sigstore/cosign-installer` step exists in `build-service-images` |
| `test_cosign_sign_steps_count` | 2 sign steps in `build-base-images`, 1 in `build-service-images` |
| `test_cosign_sign_steps_pr_guard` | All sign steps have `github.event_name != 'pull_request'` guard |
| `test_cosign_sign_steps_reference_digest` | Sign steps reference the correct digest output |
| `test_cosign_sign_uses_yes_flag` | All sign steps use the `--yes` flag |
| `test_cosign_id_token_permission_comment` | `id-token: write` comment references CC-0030 |

## Vulnerability Scanning

Every container image is scanned for known CVEs using Grype (via `anchore/scan-action`)
on every push and pull request (CC-0032). Unlike SBOM generation, attestation, and cosign
signing (which are skipped on PRs), vulnerability scanning runs on **both** event types
to provide immediate feedback on high-severity CVEs before merging.

### How It Works

After each image build (and SBOM generation on push), two additional steps run:

1. **Grype scan** (`anchore/scan-action`) — Scans the image for known CVEs. On push
   events, Grype consumes the CycloneDX SBOM file (faster, offline-capable). On PR
   events, Grype scans the image directly (since SBOM generation is skipped on PRs).
   The scan uses `severity-cutoff: high` with `fail-build: false`. All CVEs at or above
   high severity are reported in SARIF but do not currently fail the build. Build failure
   on high/critical CVEs will be activated later.

2. **SARIF upload** (`github/codeql-action/upload-sarif`) — Uploads Grype results to the
   GitHub Security tab in SARIF format. Each upload uses a unique `category` value to
   distinguish findings per image. The upload step runs with `if: always()` and a guard
   that checks for non-empty SARIF output, ensuring the upload is skipped cleanly if the
   scan step crashes without producing output.

This pattern is applied to all three image types:

| Image | Step ID (SBOM / push) | Step ID (image / PR) | SARIF category | Job |
| --- | --- | --- | --- | --- |
| `python-base` | `grype-python-base-sbom` | `grype-python-base-image` | `grype-python-base` | `build-base-images` |
| `venv-builder` | `grype-venv-builder-sbom` | `grype-venv-builder-image` | `grype-venv-builder` | `build-base-images` |
| Service (e.g., `keystone`) | `grype-service-sbom` | `grype-service-image` | <code v-pre>grype-${{ matrix.service }}</code> | `build-service-images` |

### Scan Input: PR vs Push

Each image has two separate Grype scan steps — one for push events (SBOM-based) and one
for PR events (image-based) — because `anchore/scan-action` documents `sbom` and `image`
as mutually exclusive inputs. Each step uses an `if:` guard to run in the correct context:

| Step suffix | `if:` guard | Input | Source |
| --- | --- | --- | --- |
| `-sbom` | `github.event_name != 'pull_request'` | `sbom:` | CycloneDX SBOM file (e.g., `sbom-python-base.cyclonedx.json`) |
| `-image` | `github.event_name == 'pull_request'` | `image:` | Image reference (registry digest for base images, composite tag for service images) |

The SARIF upload step uses a fallback expression (`steps.<id>-sbom.outputs.sarif ||
steps.<id>-image.outputs.sarif`) to reference whichever step produced output, since
exactly one of the two steps runs per event type.

For base images on PRs, the image is referenced by digest from GHCR (base images are
always pushed). For service images on PRs, the image is referenced by the composite tag
from `steps.tags.outputs.composite` (service images use `load: true` on PRs, making them
available locally).

### Severity Threshold

All Grype scan steps use `severity-cutoff: high` with `fail-build: false`:

- **Critical** and **High** severity CVEs are reported in SARIF but do not currently
  fail the build (build failure will be activated later)
- **Medium** and **Low** severity CVEs are reported in SARIF but do not block the build
- All findings are visible in the GitHub Security tab regardless of severity

### CVE Suppression

A `.grype.yaml` configuration file at the repository root allows suppression of
known-accepted CVEs (CC-0032). Grype automatically reads this file from the working
directory (default behavior — no explicit configuration needed).

```yaml
# .grype.yaml — add entries to suppress false positives
ignore:
  - id: CVE-YYYY-NNNNN
    reason: "<justification>"
    fix-state: "not-fixed"
```

The ignore list is initially empty. To add a CVE suppression, append an entry with the
CVE identifier, a justification explaining why the CVE is acceptable, and optionally a
`fix-state` field (`fixed`, `not-fixed`, `wont-fix`, or `unknown`). This prevents
false-positive build failures on unfixable base-image vulnerabilities without disabling
scanning entirely.

### SARIF Integration

Grype scan results are uploaded to the GitHub Security tab via
`github/codeql-action/upload-sarif`:

- Each image has a unique SARIF `category` value (e.g., `grype-python-base`,
  `grype-venv-builder`, `grype-<service>`) for per-image categorization in the Security
  dashboard
- Upload steps use `if: always()` with a guard for non-empty SARIF output, ensuring
  clean skip when a scan step crashes without producing output
- Results appear in the repository's **Security > Code scanning alerts** tab

### Required Permissions

SARIF upload requires `security-events: write` permission on both `build-base-images`
and `build-service-images` jobs (CC-0032). Verification jobs (`verify-base-images`,
`verify-service-images`) do not receive this permission (least privilege).

### Test Coverage

The `verify_build_images_workflow.sh` script validates vulnerability scanning
configuration (CC-0032):

| Test | Validates |
| --- | --- |
| `test_grype_scan_steps_in_build_base_images` | 4 `anchore/scan-action` steps exist in `build-base-images` (2 per image: SBOM + image) |
| `test_grype_scan_step_in_build_service_images` | 2 `anchore/scan-action` steps exist in `build-service-images` (SBOM + image) |
| `test_grype_scan_action_sha_pinned` | `anchore/scan-action` is SHA-pinned with `# v7` version comment |
| `test_grype_scan_steps_cover_both_contexts` | Each image has both a push-context (SBOM) and PR-context (image) scan step with appropriate `if:` guards |
| `test_grype_sbom_input_wiring` | SBOM input references correct filenames (`sbom-python-base.cyclonedx.json`, etc.) |
| `test_grype_image_input_wiring` | Image input references correct image refs for PR context |
| `test_grype_severity_threshold` | All Grype steps use `severity-cutoff: high` |
| `test_grype_fail_build_false` | All Grype steps use `fail-build: false` |
| `test_grype_output_format_sarif` | All Grype steps use `output-format: sarif` |
| `test_sarif_upload_steps_exist` | 2 `upload-sarif` steps in `build-base-images`, 1 in `build-service-images` |
| `test_sarif_upload_categories` | SARIF upload categories match image names (`grype-python-base`, etc.) |
| `test_sarif_upload_always_condition` | All SARIF upload steps have `if: always()` with SARIF output guard |
| `test_sarif_upload_action_sha_pinned` | `github/codeql-action/upload-sarif` is SHA-pinned with `# v3` version comment |
| `test_sarif_upload_references_grype_output` | SARIF upload `sarif_file` references Grype step output |
| `test_security_events_permission_build_base_images` | `build-base-images` has `security-events: write` |
| `test_security_events_permission_build_service_images` | `build-service-images` has `security-events: write` |
| `test_verify_jobs_no_security_events_permission` | Verify jobs do **not** have `security-events` permission |
| `test_security_events_permission_comment` | `security-events` permission comment references CC-0032 |

## OCI Annotations

Every container image receives OCI Image Spec annotations (CC-0031) via a two-layer
approach: static `LABEL` instructions in Dockerfiles provide baseline metadata for local
builds, while `docker/metadata-action` in CI generates dynamic labels that supplement the
static ones at push time.

### Static Dockerfile Labels

Each Dockerfile includes a `LABEL` instruction with four OCI annotations that are always
present, regardless of whether the image is built locally or in CI:

| Label | Value (example for keystone) |
| --- | --- |
| `org.opencontainers.image.title` | `keystone` |
| `org.opencontainers.image.description` | `OpenStack keystone service` |
| `org.opencontainers.image.licenses` | `Apache-2.0` |
| `org.opencontainers.image.vendor` | `SAP SE` |

In `python-base` and `venv-builder`, the `LABEL` instruction is placed after the last
`RUN` instruction. In `keystone`, the `LABEL` is placed in Stage 2 (runtime, `FROM
python-base`) before the `USER` instruction — Stage 1 (build) labels are discarded by
Docker's multi-stage build process.

### CI Metadata Action

In CI, each image has a `docker/metadata-action` step that generates OCI-compliant
labels. The action auto-generates dynamic labels from the GitHub context:

| Auto-generated label | Source |
| --- | --- |
| `org.opencontainers.image.created` | Build timestamp (ISO 8601) |
| `org.opencontainers.image.revision` | `GITHUB_SHA` (40-character Git SHA) |
| `org.opencontainers.image.source` | GitHub repository URL |
| `org.opencontainers.image.url` | GitHub repository URL |
| `org.opencontainers.image.version` | Git-derived version (or raw override for keystone) |

The `labels` input of each metadata-action step provides the four custom labels (title,
description, licenses, vendor). These supplement the auto-generated labels.

**Metadata-action steps:**

| Step ID | Job | Image input |
| --- | --- | --- |
| `meta-python-base` | `build-base-images` | <code v-pre>ghcr.io/${{ steps.meta.outputs.owner }}/python-base</code> |
| `meta-venv-builder` | `build-base-images` | <code v-pre>ghcr.io/${{ steps.meta.outputs.owner }}/venv-builder</code> |
| `meta-service` | `build-service-images` | <code v-pre>${{ steps.tags.outputs.image }}</code> |

Each metadata-action step's `outputs.labels` is wired into the corresponding
`build-push-action` step via the `labels` input. At push time, CI-generated labels
(created, revision, source, url, version) override any matching static Dockerfile labels,
while the static labels serve as fallback for local builds where no metadata-action runs.

### Keystone Version Override

By default, `docker/metadata-action` derives `org.opencontainers.image.version` from the
Git context (branch name or tag). For keystone, this would produce a Git-derived version
rather than the upstream OpenStack release version (e.g., `28.0.0`).

To ensure the OCI version annotation reflects the actual software version, the
`meta-service` step uses a `type=raw` tag strategy:

```yaml
tags: |
  type=raw,value=${{ steps.source-ref.outputs.ref }}
```

This overrides the version to match the value from `source-refs.yaml` (e.g., `28.0.0`).
The base images (`python-base`, `venv-builder`) do not specify a `tags` input and use the
default Git-derived version strategy, which is appropriate for infrastructure images
without an upstream software version.

> **Note:** The `tags` input on `docker/metadata-action` controls the
> `org.opencontainers.image.version` label, not the image tags passed to
> `docker/build-push-action`. Image tags continue to be computed by the "Derive tags"
> step (see [Tag Schema](#tag-schema)). The metadata-action's tag strategies only
> influence the OCI version annotation.

### Test Coverage

The `verify_build_images_workflow.sh` script validates OCI annotation configuration
(CC-0031):

| Test | Validates |
| --- | --- |
| `test_metadata_action_steps_exist_in_build_base_images` | `build-base-images` has `meta-python-base` and `meta-venv-builder` steps using `docker/metadata-action` |
| `test_metadata_action_step_exists_in_build_service_images` | `build-service-images` has `meta-service` step using `docker/metadata-action` |
| `test_service_metadata_uses_raw_version_strategy` | `meta-service` step uses `type=raw` with `steps.source-ref.outputs.ref` |
| `test_base_metadata_steps_have_no_tags_override` | `meta-python-base` and `meta-venv-builder` do not specify a `tags` input |
| `test_python_base_build_push_has_labels_input` | `build-python-base` step wires `steps.meta-python-base.outputs.labels` |
| `test_venv_builder_build_push_has_labels_input` | `build-venv-builder` step wires `steps.meta-venv-builder.outputs.labels` |
| `test_service_build_push_has_labels_input` | `build-service` step wires `steps.meta-service.outputs.labels` |
| `test_metadata_action_labels_include_oci_title` | All 3 metadata-action steps include `org.opencontainers.image.title` |
| `test_metadata_action_labels_include_oci_description` | All 3 metadata-action steps include `org.opencontainers.image.description` |
| `test_metadata_action_labels_include_oci_licenses` | All 3 metadata-action steps include `org.opencontainers.image.licenses=Apache-2.0` |
| `test_metadata_action_labels_include_oci_vendor` | All 3 metadata-action steps include `org.opencontainers.image.vendor` |
| `test_dockerfile_static_labels_python_base` | `images/python-base/Dockerfile` has `LABEL` for title, description, licenses, vendor |
| `test_dockerfile_static_labels_venv_builder` | `images/venv-builder/Dockerfile` has `LABEL` for title, description, licenses, vendor |
| `test_dockerfile_static_labels_keystone` | `images/keystone/Dockerfile` has `LABEL` for title, description, licenses, vendor in Stage 2 |

## Adding a New Service

To add a new service (e.g., `nova`) to the build matrix:

### 1. Create the Dockerfile

Add a Dockerfile at `images/<service>/Dockerfile` following the two-stage pattern in
`images/keystone/Dockerfile`. The Dockerfile must use named build contexts
(`python-base`, `venv-builder`, `<service>`, `upper-constraints`) — not hardcoded paths.
Include a `LABEL` instruction in the runtime stage with OCI annotations (title,
description, licenses, vendor) — see [OCI Annotations](#oci-annotations) (CC-0031).

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
follows the standard pattern: <code v-pre>${{ github.ref }}-${{ github.workflow }}</code> with
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
| `verify_build_images_workflow.sh` | Workflow structure: job names, dependency chain, trigger events, permissions, action pinning, concurrency, matrix strategy, SBOM/attestation configuration (CC-0029), cosign signing configuration (CC-0030), OCI annotation configuration (CC-0031), vulnerability scanning configuration (CC-0032) |
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
(`verify_python_base.sh`, `verify_venv_builder.sh`, `verify_<service>.sh`) run inside
`build-images.yaml` where the images are actually built.

## Verification Coverage Summary

The following table summarizes which test scripts run where (CC-0028, CC-0029, CC-0032):

| Test Script | verify-container-images.yaml | build-images.yaml | Requires Docker |
| --- | --- | --- | --- |
| `verify_build_images_workflow.sh` | verify-static-tests | — | No |
| `verify_deviation_comments.sh` | verify-static-tests | — | No |
| `verify_release_config.sh` | verify-static-tests | — | No |
| `verify_spdx_headers.sh` | verify-static-tests | — | No |
| `test_apply_constraint_overrides.sh` | verify-static-tests | — | No |
| `verify_python_base.sh` | — | verify-base-images | Yes |
| `verify_venv_builder.sh` | — | verify-base-images | Yes |
| `verify_<service>.sh` | — | build-service-images (PR) / verify-service-images (push) | Yes |

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

:::
