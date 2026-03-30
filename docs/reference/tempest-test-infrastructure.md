---
title: Tempest Test Infrastructure
quadrant: infrastructure
feature: CC-0035
---

::: v-pre

# Tempest Test Infrastructure

Reference documentation for the Tempest API test infrastructure (CC-0035). This covers
the Tempest container image, version management, per-service test configuration, the
image verification script, local execution via `hack/run-tempest.sh`, the `make
tempest-test` target, and CI integration in both `ci.yaml` and `build-images.yaml`.

## File Locations

| File | Purpose |
| --- | --- |
| `images/tempest/Dockerfile` | Two-stage Tempest container image (venv-builder → python-base) |
| `releases/2025.2/test-refs.yaml` | PyPI version pins for test tooling (single source of truth) |
| `tests/tempest/keystone/tempest.conf` | Keystone-specific Tempest configuration |
| `tests/tempest/keystone/include-tests.txt` | Regex patterns for tests to run |
| `tests/tempest/keystone/exclude-tests.txt` | Regex patterns for tests to skip |
| `tests/container-images/verify_tempest.sh` | Image verification script (PASS/FAIL counters) |
| `hack/run-tempest.sh` | Local orchestration script for running Tempest against a kind cluster |
| `Makefile` | `tempest-test` target delegates to `hack/run-tempest.sh` |
| `.github/workflows/ci.yaml` | Tempest steps appended to `e2e-keystone` job |
| `.github/workflows/build-images.yaml` | `build-tempest` and `merge-tempest-image` jobs |

## Architecture

```text
releases/2025.2/test-refs.yaml          Version pins (tempest, keystone-tempest-plugin)
        │
        ▼ (yq resolution)
images/tempest/Dockerfile               2-stage build: venv-builder → python-base
        │
        ├──▶ build-images.yaml          Build, scan, sign, push to GHCR
        │       build-tempest job        Per-platform builds (amd64, arm64)
        │       merge-tempest-image job  Multi-arch manifest + SBOM + Grype + cosign
        │
        └──▶ ci.yaml                    Build locally, run tests in e2e-keystone
                e2e-keystone job          After Chainsaw: build image → run Tempest → upload JUnit
                                         │
tests/tempest/keystone/                  │ mounted into container
  tempest.conf                    ──────▶│
  include-tests.txt               ──────▶│
  exclude-tests.txt               ──────▶│
                                         ▼
                              _output/tempest/tempest-results.xml (JUnit XML artifact)
```

## Version Management

### test-refs.yaml

**Location:** `releases/<release>/test-refs.yaml`

Maps each test tool to a PyPI version pin. This file is the single source of truth for
what version of each test tool is installed in the Tempest container image. It is separate
from `source-refs.yaml` (which tracks git refs for OpenStack services) so that test
tooling versions can evolve independently.

**Format:**

```yaml
tempest: "45.0.0"
keystone-tempest-plugin: "0.19.0"
```

Each key is a PyPI package name. Values are quoted strings representing exact version
pins. CI workflows resolve versions from this file via `yq`:

```bash
TEMPEST_VERSION=$(yq -r '.tempest' releases/2025.2/test-refs.yaml)
KTP_VERSION=$(yq -r '.["keystone-tempest-plugin"]' releases/2025.2/test-refs.yaml)
```

Both `ci.yaml` and `build-images.yaml` use the same resolution pattern. A null or empty
result from `yq` causes the CI step to fail with a descriptive error.

To update versions, edit `test-refs.yaml` — no Dockerfile changes are needed.

## Container Image

### Dockerfile

**Location:** `images/tempest/Dockerfile`

The Tempest image uses the same two-stage build pattern as service images but differs
in three ways: (1) it installs from PyPI instead of mounting a git source tree,
(2) it has no WSGI entrypoint, and (3) it requires no `PIP_EXTRAS` or
`EXTRA_APT_PACKAGES`.

**Stage 1 (`build`)** — extends `venv-builder`:

- Declares `ARG TEMPEST_VERSION` and `ARG KEYSTONE_TEMPEST_PLUGIN_VERSION` for
  build-time version injection (resolved from `test-refs.yaml` by CI)
- Mounts `upper-constraints.txt` from the release directory via named build context
- Installs four packages into the shared virtualenv via `uv pip install --constraint`:

| Package | Purpose |
| --- | --- |
| `tempest` | OpenStack Tempest testing framework |
| `keystone-tempest-plugin` | Keystone-specific Tempest test plugins |
| `python-subunit` | Subunit test result streaming protocol |
| `junitxml` | Subunit-to-JUnit XML conversion (`subunit2junitxml`) |

**Stage 2 (runtime)** — extends `python-base`:

- Copies `/var/lib/openstack` virtualenv from the build stage via `COPY --from=build --link`
- Sets static OCI labels (title, description, licenses, vendor) following the
  two-layer annotation pattern (CC-0031)
- Runs as `openstack` user (UID 42424, GID 42424)

**Final image properties:**

| Property | Value |
| --- | --- |
| User | `openstack` (UID 42424) |
| `tempest` CLI | Available via `PATH` (`/var/lib/openstack/bin/tempest`) |
| `subunit2junitxml` | Available via `PATH` |
| Build tools | Absent (`gcc`, `python3-dev`, `uv` are not in the final image) |

**Named build contexts:**

| Context name | Contents | Mounted as |
| --- | --- | --- |
| `python-base` | Runtime base image | `FROM python-base` |
| `venv-builder` | Build stage base image | `FROM venv-builder AS build` |
| `upper-constraints` | Release directory containing `upper-constraints.txt` | `/tmp/upper-constraints.txt` |

**Build args:**

| Arg | Default | Source |
| --- | --- | --- |
| `TEMPEST_VERSION` | `45.0.0` | `test-refs.yaml` → `.tempest` |
| `KEYSTONE_TEMPEST_PLUGIN_VERSION` | `0.19.0` | `test-refs.yaml` → `.["keystone-tempest-plugin"]` |

### Local Build

Build the Tempest image locally (requires `python-base` and `venv-builder` images):

```bash
# Build base images first (if not already available)
docker build images/python-base -t python-base
docker build images/venv-builder -t venv-builder

# Build Tempest image
docker build images/tempest \
  -t c5c3/tempest:45.0.0 \
  --build-arg TEMPEST_VERSION=45.0.0 \
  --build-arg KEYSTONE_TEMPEST_PLUGIN_VERSION=0.19.0 \
  --build-context python-base=docker-image://python-base \
  --build-context venv-builder=docker-image://venv-builder \
  --build-context upper-constraints=releases/2025.2/
```

## Test Configuration

Per-service Tempest configuration lives under `tests/tempest/<service>/`. Each service
directory contains three files: a Tempest configuration file, an include list, and an
exclude list.

### tempest.conf

**Location:** `tests/tempest/<service>/tempest.conf`

INI-format configuration file for the Tempest testing framework. Key sections for the
Keystone service:

| Section | Key | Value | Purpose |
| --- | --- | --- | --- |
| `[DEFAULT]` | `log_dir` | `/tmp/tempest-logs` | Log output directory inside container |
| `[identity]` | `uri_v3` | `http://keystone-tempest-api.openstack.svc:5000/v3` | Keystone v3 API endpoint (in-cluster DNS) |
| `[auth]` | `use_dynamic_credentials` | `false` | Use static admin credentials (no tenant creation) |
| `[auth]` | `admin_username` | `admin` | Admin user for API authentication |
| `[auth]` | `admin_password` | `${KEYSTONE_ADMIN_PASSWORD}` | Injected at runtime from K8s secret |
| `[auth]` | `admin_project_name` | `admin` | Admin project scope |
| `[auth]` | `admin_domain_name` | `Default` | Admin domain scope |
| `[identity-feature-enabled]` | `api_v3` | `true` | Enable v3 identity API tests |
| `[service_available]` | `identity` | `true` | Identity service is deployed |
| `[service_available]` | `compute` | `false` | Nova is not deployed |
| `[service_available]` | `network` | `false` | Neutron is not deployed |
| `[service_available]` | `volume` | `false` | Cinder is not deployed |
| `[service_available]` | `image` | `false` | Glance is not deployed |
| `[service_available]` | `object-storage` | `false` | Swift is not deployed |

The `admin_password` placeholder `${KEYSTONE_ADMIN_PASSWORD}` is resolved at runtime:
- **Local execution:** `hack/run-tempest.sh` extracts it from the `keystone-admin` K8s
  secret and passes it as an environment variable to the container
- **CI execution:** `ci.yaml` extracts it via `kubectl get secret` and substitutes it
  into a generated copy of `tempest.conf` using `sed`

### include-tests.txt

**Location:** `tests/tempest/<service>/include-tests.txt`

One regex pattern per line. Lines starting with `#` are comments. Blank lines are
ignored. Patterns are matched against Tempest test IDs.

For Keystone, two patterns include all identity-related tests:

| Pattern | Matches |
| --- | --- |
| `tempest.api.identity` | Core Tempest identity API tests |
| `keystone_tempest_plugin.tests` | Keystone-specific plugin tests |

### exclude-tests.txt

**Location:** `tests/tempest/<service>/exclude-tests.txt`

Same format as `include-tests.txt`. Patterns exclude tests that require services or
infrastructure not available in the CI kind cluster:

| Pattern | Reason for exclusion |
| --- | --- |
| `keystone_tempest_plugin\.tests\..*ldap` | Requires a live LDAP server |
| `keystone_tempest_plugin\.tests\..*federation` | Requires an external IdP (SAML2/OAuth2) |
| `keystone_tempest_plugin\.tests\..*oauth2` | Requires an external authorization server |

### Adding a New Service

To add Tempest tests for a new service (e.g., `glance`):

1. Create `tests/tempest/glance/` with `tempest.conf`, `include-tests.txt`, and
   `exclude-tests.txt`
2. Set `[service_available]` flags to match the deployed services
3. Update `[identity]` URI to point to the service endpoint
4. Run `make tempest-test SERVICE=glance` to test locally

No changes to the Dockerfile are needed. However, both `hack/run-tempest.sh` and
`ci.yaml` currently hardcode Keystone-specific values that require updates for new
services:

- **`hack/run-tempest.sh`**: The `extract_admin_password()` function hardcodes the
  `keystone-admin` secret name. A new service requires a different secret name.
- **`ci.yaml`** (`e2e-keystone` job): Hardcodes the `keystone-admin` secret name,
  the `keystone-tempest-api` service name, and port `5000` for port-forwarding.

The `e2e-keystone` job dynamically checks for `tests/tempest/<operator>/` directories
to decide whether to run Tempest tests.

## Image Verification

**Location:** `tests/container-images/verify_tempest.sh`

Validates that the built Tempest container image meets requirements. Uses the same
PASS/FAIL counter pattern as other `verify_*.sh` scripts and sources
`tests/lib/assertions.sh` for assertion helpers.

**Usage:**

```bash
bash tests/container-images/verify_tempest.sh [image_name]
# Default image: c5c3/tempest:45.0.0
```

**Test cases:**

| Test | Assertion | Validates |
| --- | --- | --- |
| `test_tempest_version` | `tempest --version` exits 0 with non-empty output | Tempest CLI is installed and functional |
| `test_keystone_tempest_plugin_importable` | `python3 -c 'import keystone_tempest_plugin'` exits 0 | Plugin is installed in the virtualenv |
| `test_subunit2junitxml_available` | `which subunit2junitxml` exits 0 with non-empty path | JUnit XML converter is on PATH |
| `test_runs_as_openstack_user` | `whoami` outputs `openstack` | Container runs as non-root user |
| `test_no_build_tools_in_final_image` | `which gcc`, `dpkg -s python3-dev`, `which uv` all fail | Build tools are not in the runtime image |

All test functions use the exit-code guard pattern (`|| exit_code=$?`) to prevent
`set -e` from aborting the script before assertions run.

In CI, this script runs during the `build-tempest` job on pull requests to catch image
build regressions independently of the full E2E pipeline.

## Local Execution

### hack/run-tempest.sh

**Location:** `hack/run-tempest.sh`

Orchestration script for running Tempest API tests against a deployed OpenStack service
in a local kind cluster. Follows the infrastructure deployment script pattern:
`set -euo pipefail`, `log()` with ISO 8601 timestamps, `SCRIPT_DIR`/`REPO_ROOT`
resolution, and configurable variables.

**Usage:**

```bash
SERVICE=keystone hack/run-tempest.sh
```

**Environment variables:**

| Variable | Default | Description |
| --- | --- | --- |
| `SERVICE` | *(required)* | OpenStack service to test (e.g., `keystone`) |
| `RELEASE` | `2025.2` | Release version (selects `test-refs.yaml` and `upper-constraints.txt`) |
| `TEMPEST_IMAGE` | `c5c3/tempest:local` | Docker image name for the Tempest container |
| `OUTPUT_DIR` | `_output/tempest` | Directory for test results (JUnit XML, subunit stream) |
| `TEMPEST_TIMEOUT` | `1800` | Timeout for Tempest execution in seconds |
| `NAMESPACE` | `openstack` | Kubernetes namespace for the service under test |

**Execution steps:**

| Step | Description | Failure behavior |
| --- | --- | --- |
| Pre-flight checks | Validates `SERVICE` is set, required tools (`docker`, `kubectl`, `yq`) are installed, Docker is running, service config directory exists, `test-refs.yaml` exists | Exits 1 with descriptive error |
| Build Tempest image | Resolves versions from `test-refs.yaml`, builds with `docker build` using named build contexts pointing to GHCR base images | Exits on build failure (set -e) |
| Extract admin password | Reads `keystone-admin` secret from the K8s cluster via `kubectl get secret` | Exits 1 if secret is not found or empty |
| Run Tempest | Mounts `tempest.conf`, `include-tests.txt`, `exclude-tests.txt` into container; initializes Tempest workspace; runs `tempest run --subunit`; converts to JUnit XML via `subunit2junitxml` | Returns Tempest exit code |

**Output files:**

| File | Format | Description |
| --- | --- | --- |
| `_output/tempest/tempest-results.xml` | JUnit XML | Test results for CI artifact upload |
| `_output/tempest/tempest.subunit` | Subunit v2 | Raw test result stream |

The script exits with the same exit code as `tempest run` — non-zero if any test fails.

### make tempest-test

**Location:** `Makefile`

```bash
make tempest-test SERVICE=keystone
```

The `tempest-test` target validates that `SERVICE` is set (using the `$(if)` guard
pattern consistent with other Makefile targets) and delegates to `hack/run-tempest.sh`.
Omitting `SERVICE` produces an error message:

```
*** tempest-test requires SERVICE, e.g. make tempest-test SERVICE=keystone.  Stop.
```

## CI Integration

### ci.yaml — e2e-keystone Job

Tempest steps are appended to the `e2e-keystone` job after the Chainsaw E2E test steps.
They execute conditionally — only when a `tests/tempest/<operator>/` configuration
directory exists for the matrix operator.

**Step sequence:**

| Step | Condition | Description |
| --- | --- | --- |
| Check for Tempest config | Always | Sets `enabled=true/false` output based on directory existence |
| Build Tempest image | `enabled == 'true'` | Resolves versions from `test-refs.yaml`, builds image locally |
| Run Tempest API tests | `enabled == 'true'` | Port-forwards service, generates CI config with resolved credentials, runs Tempest in container |
| Upload Tempest results | `always() && enabled == 'true'` | Uploads `_output/tempest/` as artifact with 14-day retention |

**CI-specific adaptations** (compared to local execution):

| Aspect | Local (`hack/run-tempest.sh`) | CI (`ci.yaml`) |
| --- | --- | --- |
| Service endpoint | In-cluster DNS (`keystone-tempest-api.openstack.svc:5000`) | Port-forwarded to `localhost:5000` |
| Credential injection | Environment variable passed to container | `sed` substitution into generated config copy |
| Base images | Pulled from GHCR (`docker-image://ghcr.io/...`) | Built locally in prior CI steps (no `--build-context` for bases) |
| Artifact upload | Manual inspection of `_output/` | `actions/upload-artifact` with `tempest-<operator>-results` name |

**Artifact name:** `tempest-<operator>-results` (e.g., `tempest-keystone-results`)
**Retention:** 14 days

### build-images.yaml — build-tempest Job

Builds the Tempest container image per platform, runs verification on PRs, and pushes
by digest on push events.

**Dependencies:** `needs: [lint-dockerfiles, merge-base-images, verify-base-images]`

**Matrix strategy:**

| Platform | Runner |
| --- | --- |
| `linux/amd64` | `ubuntu-latest` |
| `linux/arm64` | `ubuntu-24.04-arm` |

**Step sequence:**

| Step | Condition | Description |
| --- | --- | --- |
| Resolve Tempest versions | Always | Reads `test-refs.yaml` via `yq` |
| Generate metadata | Always | OCI labels via `docker/metadata-action` (CC-0031) |
| Build Tempest image | PR: amd64 only; push: both platforms | `docker/build-push-action` with named build contexts |
| Export digest | Push only | Writes digest to `/tmp/digests/` for merge job |
| Upload digest artifact | Push only | `digests-tempest-<platform-pair>`, 1-day retention |
| Scan for vulnerabilities | PR, amd64 only | Grype scan against loaded image (CC-0032) |
| Upload SARIF | PR, if scan produced output | GitHub Security tab under `grype-tempest-<platform>` |
| Verify Tempest image | PR, amd64 only | Runs `verify_tempest.sh` against the built image |

**PR vs push behavior:**

| Behavior | Pull Request | Push to main/stable |
| --- | --- | --- |
| Platforms built | amd64 only | amd64 + arm64 |
| Image destination | Loaded locally (`load: true`) | Pushed by digest to GHCR |
| Verification | `verify_tempest.sh` runs | Deferred to `merge-tempest-image` |
| Vulnerability scan | Against local image | Against SBOM (in merge job) |

### build-images.yaml — merge-tempest-image Job

Assembles per-platform digests into a multi-arch manifest list with full supply chain
security.

**Dependencies:** `needs: [build-tempest]`
**Condition:** `if: github.event_name != 'pull_request'`

**Permissions:**

| Permission | Purpose |
| --- | --- |
| `contents: read` | Repository checkout |
| `packages: write` | Push manifest list to GHCR |
| `id-token: write` | Sigstore OIDC signing for cosign and build provenance (CC-0030) |
| `attestations: write` | GitHub Attestations API (CC-0029) |
| `security-events: write` | SARIF upload to GitHub Security tab (CC-0032) |

**Image tags applied:**

| Tag | Example | Description |
| --- | --- | --- |
| `latest` | `ghcr.io/<owner>/tempest:latest` | Most recent build |
| `<tempest-version>` | `ghcr.io/<owner>/tempest:45.0.0` | Tempest PyPI version |
| `<commit-sha>` | `ghcr.io/<owner>/tempest:<sha>` | Git commit for traceability |

**Supply chain security steps:**

| Step | Tool | Output |
| --- | --- | --- |
| SBOM generation | `anchore/sbom-action` | `sbom-tempest.cyclonedx.json` (CycloneDX format) |
| Vulnerability scan | `anchore/scan-action` (Grype) | SARIF uploaded to GitHub Security tab (`grype-tempest`) |
| SBOM attestation | `actions/attest` | Signed attestation pushed to GHCR registry |
| Image signing | `sigstore/cosign` | Keyless signature via Sigstore OIDC |

### lint-dockerfiles

The Tempest Dockerfile is included in the `lint-dockerfiles` matrix job alongside other
Dockerfiles:

```yaml
strategy:
  matrix:
    dockerfile:
      - images/python-base/Dockerfile
      - images/venv-builder/Dockerfile
      - images/keystone/Dockerfile
      - images/tempest/Dockerfile      # CC-0035
      - operators/keystone/Dockerfile
```

Hadolint runs with `failure-threshold: warning` and the project's `.hadolint.yaml`
configuration. The Tempest Dockerfile uses `# hadolint ignore=DL3006` on `FROM`
directives (consistent with other Dockerfiles) because named build contexts resolve
the base image at build time.

## Data Flow (CI End-to-End)

```text
test-refs.yaml ──yq──▶ TEMPEST_VERSION + KEYSTONE_TEMPEST_PLUGIN_VERSION
                         │
                         ▼
         docker build --build-arg TEMPEST_VERSION=... \
                      --build-arg KEYSTONE_TEMPEST_PLUGIN_VERSION=... \
                      --build-context upper-constraints=releases/2025.2 \
                      images/tempest/
                         │
                         ▼ (e2e-keystone job)
         kubectl port-forward svc/keystone-tempest-api 5000:5000
                         │
                         ▼
         sed tempest.conf (resolve localhost URI + admin password)
                         │
                         ▼
         docker run --network host \
           -v tempest.conf:/etc/tempest/tempest.conf \
           -v include-tests.txt:/etc/tempest/include-tests.txt \
           -v exclude-tests.txt:/etc/tempest/exclude-tests.txt \
           c5c3/tempest:local bash -c "
             tempest init . && cp tempest.conf etc/ &&
             tempest run --subunit | tee tempest.subunit &&
             subunit2junitxml < tempest.subunit > tempest-results.xml"
                         │
                         ▼
         actions/upload-artifact ──▶ tempest-<operator>-results (14-day retention)
```

## Dependencies on Prior Features

| Feature | Artifact | Used by Tempest infrastructure |
| --- | --- | --- |
| CC-0006 | `images/python-base/Dockerfile`, `images/venv-builder/Dockerfile` | Base images for the Tempest Dockerfile build chain |
| CC-0006 | `releases/2025.2/upper-constraints.txt` | Dependency constraints for PyPI installs |
| CC-0028 | `tests/lib/assertions.sh` | Assertion helpers sourced by `verify_tempest.sh` |
| CC-0029 | SBOM attestation pattern in `build-images.yaml` | Reused by `merge-tempest-image` for Tempest SBOM |
| CC-0030 | Cosign signing pattern in `build-images.yaml` | Reused by `merge-tempest-image` for Tempest signing |
| CC-0031 | Two-layer OCI annotation pattern | Static Dockerfile labels + CI metadata-action |
| CC-0032 | Grype scanning pattern in `build-images.yaml` | Reused by `build-tempest` and `merge-tempest-image` |

:::
