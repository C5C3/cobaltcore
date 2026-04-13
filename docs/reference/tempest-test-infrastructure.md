---
title: Tempest Test Infrastructure
quadrant: infrastructure
feature: CC-0035, CC-0051
---

::: v-pre

# Tempest Test Infrastructure

Reference documentation for the Tempest API test infrastructure (CC-0035, CC-0051). This
covers the Tempest container image, version management, per-service test configuration,
the image verification script, local execution via `hack/run-tempest.sh`, the `make
tempest-test` target, and CI integration in both `ci.yaml` and `build-images.yaml`.
CC-0051 extended the infrastructure with multi-release support: the `tempest` job in
`ci.yaml` now uses a release matrix to validate each OpenStack release independently,
and `build-images.yaml` dynamically discovers releases for the Tempest image pipeline.

## File Locations

| File | Purpose |
| --- | --- |
| `images/tempest/Dockerfile` | Two-stage Tempest container image (venv-builder → python-base) |
| `releases/<release>/test-refs.yaml` | PyPI version pins for test tooling (single source of truth), per release |
| `tests/tempest/keystone-2025-2/` | Keystone 2025.2 Tempest configuration (`tempest.conf`, `include-tests.txt`, `exclude-tests.txt`) |
| `tests/tempest/keystone-2026-1/` | Keystone 2026.1 Tempest configuration (CC-0051) |
| `tests/container-images/verify_tempest.sh` | Image verification script (PASS/FAIL counters) |
| `hack/run-tempest.sh` | Local orchestration script for running Tempest against a kind cluster |
| `hack/ci-run-tempest.sh` | CI-specific Tempest wrapper with port-forwarding and config generation (CC-0050) |
| `hack/tempest/extract-failed.py` | Print anchored regex patterns for failed testcases in a JUnit report (used to build the retry include-list) |
| `hack/tempest/merge-retry-junit.py` | Merge a retry subunit stream into a JUnit report, rewriting resolved failures as flakes |
| `hack/tempest/run-tests.sh` | Shared in-container runner invoked by both runners; holds the phase + retry + exit-code logic so it stays identical between CI and local runs |
| `Makefile` | `tempest-test` target delegates to `hack/run-tempest.sh` |
| `.github/workflows/ci.yaml` | `tempest` job with release matrix (CC-0051) |
| `.github/workflows/build-images.yaml` | `build-tempest` and `merge-tempest-image` jobs (release-parameterized via `generate-matrix`) |

## Architecture

```text
releases/<release>/test-refs.yaml       Version pins (tempest, keystone-tempest-plugin)
        │
        ▼ (yq resolution)
images/tempest/Dockerfile               2-stage build: venv-builder → python-base
        │
        ├──▶ build-images.yaml          Build, scan, sign, push to GHCR
        │       generate-matrix job      Discovers releases from releases/*/
        │       build-tempest job        Per-release × per-platform builds (amd64, arm64)
        │       merge-tempest-image job  Per-release multi-arch manifest + SBOM + Grype + cosign
        │
        └──▶ ci.yaml                    Build locally, run tests per release
                tempest job (matrix)      Per-release: build image → run Tempest → upload JUnit
                                         │
tests/tempest/<config-dir>/              │ mounted into container (per-release config)
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
TEMPEST_VERSION=$(yq -r '.tempest' releases/<release>/test-refs.yaml)
KTP_VERSION=$(yq -r '.["keystone-tempest-plugin"]' releases/<release>/test-refs.yaml)
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

# Build Tempest image for a specific release (e.g., 2025.2 or 2026.1).
# Resolve versions from the release's test-refs.yaml:
RELEASE=2025.2   # or 2026.1
TEMPEST_VERSION=$(yq -r '.tempest' releases/${RELEASE}/test-refs.yaml)
KTP_VERSION=$(yq -r '.["keystone-tempest-plugin"]' releases/${RELEASE}/test-refs.yaml)

docker build images/tempest \
  -t c5c3/tempest:${RELEASE} \
  --build-arg TEMPEST_VERSION=${TEMPEST_VERSION} \
  --build-arg KEYSTONE_TEMPEST_PLUGIN_VERSION=${KTP_VERSION} \
  --build-context python-base=docker-image://python-base \
  --build-context venv-builder=docker-image://venv-builder \
  --build-context upper-constraints=releases/${RELEASE}/
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

#### Scope-split invariant

Both runners (`hack/ci-run-tempest.sh` and `hack/run-tempest.sh`) split this
include list into two phase files at runtime and run `stestr` twice in the
same workspace:

1. `phase-1-core.txt` — every non-comment line that starts with `tempest.`
2. `phase-2-plugin.txt` — every non-comment line that starts with
   `keystone_tempest_plugin.`

The two phases run **sequentially**; within each phase `stestr` runs at
`TEMPEST_CONCURRENCY` (default 4).

Each runner enforces that every non-comment, non-empty line in
`include-tests.txt` lands in exactly one phase. A line with any other prefix
causes the runner to abort with a clear error — this guards against silent
drops when new include patterns are added. Both phases must be non-empty.

**Why sequential.** Keystone re-resolves the list of enabled federation
service providers on every `POST /v3/auth/tokens` and `GET /v3/auth/tokens`
call and injects it into the response body; the token itself does not cache
it. `tempest.api.identity.v3.test_tokens.TokensV3Test.test_validate_token`
issues a token via POST, validates it via GET, and asserts the two response
bodies are equal. When `keystone_tempest_plugin.tests.api.identity.v3.
test_service_providers.ServiceProvidersTest` runs concurrently on another
stestr worker, its per-test `addCleanup` deletes the service provider it
created — and if that cleanup lands in the ~20 ms window between
`test_validate_token`'s POST and GET, the two responses diverge on the
`service_providers` key and the assertion fails.

Upstream's `openstack/keystone` `keystone-tempest` gate job sets
`tempest_test_regex: "keystone_tempest_plugin"` and therefore only runs
`keystone_tempest_plugin.*` tests — the core `tempest.api.identity.*` suite
(which contains `test_validate_token`) never runs in the same Tempest
invocation as the service-providers tests, so upstream never observes this
race. We run both suites for fuller coverage and replicate the isolation by
running them in two separate stestr invocations.

The two phases each emit a subunit stream (`phase-1-core.subunit`,
`phase-2-plugin.subunit`); the runner concatenates them into
`tempest.subunit` and converts that to JUnit XML. Subunit v2 is
stream-concatenation safe by design.

#### Serial retry of failing tests

After both phases, the runner inspects the JUnit report: if any test is
marked failed or errored, those test IDs are extracted and rerun once in a
third `stestr run` invocation with `--concurrency 1`. The retry output is
written to `retry.subunit`, appended to the combined subunit stream, and
merged into the JUnit report: tests that pass on retry have their
`<failure>`/`<error>` children removed, the enclosing `<testsuite>` counters
are decremented, and a `<system-out>` note records `flaky: failed on first
run, passed on retry`. Tests that still fail after retry stay as failures.

The two helpers live at `hack/tempest/extract-failed.py` (reads the JUnit
report, prints anchored regex patterns for each failed `classname.method`)
and `hack/tempest/merge-retry-junit.py` (rewrites the JUnit report from the
retry subunit stream). The phase + retry + exit-code sequence itself lives
in `hack/tempest/run-tests.sh`, which is invoked inside the container by
both `hack/ci-run-tempest.sh` and `hack/run-tempest.sh`. All three files
are staged next to `tempest.conf` so they are available at `/etc/tempest/`
inside the container. A failure inside either Python helper (missing
dependency, parse error) is caught by the runner and falls back to the
original stestr exit code rather than aborting the whole Tempest run.

The final exit code is derived from the (possibly retry-adjusted) JUnit
report: any remaining failures or errors fail the job. If the retry
resolved every failure the runner exits 0. If the initial `stestr` process
crashed hard enough that no JUnit report was produced, the runner still
exits non-zero via the captured phase exit code.

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

No changes to the Dockerfile are needed. The `tempest` job in `ci.yaml` and
`hack/ci-run-tempest.sh` accept environment variables for service-specific values
(`SERVICE`, `CONFIG_DIR`, `ADMIN_SECRET`, `SERVICE_K8S_NAME`), so adding a new service
requires adding a matrix entry to the `tempest` job with the appropriate values.
`hack/run-tempest.sh` (local execution) also accepts `SERVICE` and `ADMIN_SECRET`
overrides.

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
| Run Tempest | Mounts `tempest.conf`, `phases/phase-1-core.txt`, `phases/phase-2-plugin.txt`, `exclude-tests.txt`, `extract-failed.py`, `merge-retry-junit.py` into container; initializes Tempest workspace; runs `stestr run --subunit` once per phase; concatenates the subunit streams; converts to JUnit XML; reruns any failed tests serially; merges the retry outcome into the JUnit report | Non-zero if any failure remains after retry, or on hard `stestr` crash |

**Output files:**

| File | Format | Description |
| --- | --- | --- |
| `_output/tempest/tempest-results.xml` | JUnit XML | Test results for CI artifact upload (retry-adjusted) |
| `_output/tempest/tempest.subunit` | Subunit v2 | Raw test result stream (phase 1 + phase 2 + retry) |
| `_output/tempest/retry.subunit` | Subunit v2 | Retry stream (only present if any tests failed on first run) |

The script exits non-zero if the retry-adjusted JUnit report still lists
failures or errors, and otherwise exits zero. Both phases always run, so a
failure in phase 1 does not short-circuit phase 2. If the initial `stestr`
invocations crashed hard enough that no JUnit report was produced, the
captured phase exit code is used as a fallback so infra failures are still
reported.

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

### ci.yaml — tempest Job

The `tempest` job (CC-0050, CC-0051) is a dedicated job that deploys services into a kind
cluster and runs the OpenStack Tempest test suite. CC-0051 added a release matrix so each
OpenStack release is validated independently with its own Tempest configuration, Keystone
CR, and K8s service name.

**Release matrix (CC-0051):**

| Release | Config directory | CR name | K8s service name |
| --- | --- | --- | --- |
| `2025.2` | `tests/tempest/keystone-2025-2` | `keystone-tempest-2025-2` | `keystone-tempest-2025-2-api` |
| `2026.1` | `tests/tempest/keystone-2026-1` | `keystone-tempest-2026-1` | `keystone-tempest-2026-1-api` |

**Step sequence:**

| Step | Description |
| --- | --- |
| Build service image | `hack/ci-build-service-image.sh` with `RELEASE=matrix.release` |
| Build Tempest image | `hack/ci-build-tempest-image.sh` with `RELEASE=matrix.release`, image tagged `c5c3/tempest:<release>` |
| Load images into kind | Loads operator and release-specific service images |
| Deploy Keystone CR | Applies `matrix.config-dir/00-keystone-cr.yaml`, waits for `matrix.cr-name` Ready |
| Run Tempest API tests | `hack/ci-run-tempest.sh` with `CONFIG_DIR`, `TEMPEST_IMAGE`, and `SERVICE_K8S_NAME` from matrix |
| Upload Tempest results | Uploads `_output/tempest/` as artifact with 14-day retention |

**CI-specific adaptations** (compared to local execution):

| Aspect | Local (`hack/run-tempest.sh`) | CI (`hack/ci-run-tempest.sh`) |
| --- | --- | --- |
| Service endpoint | In-cluster DNS (`<service-k8s-name>.openstack.svc:5000`) | Port-forwarded to `localhost:5000` |
| Credential injection | Environment variable passed to container | `sed` substitution into generated config copy |
| Base images | Pulled from GHCR (`docker-image://ghcr.io/...`) | Built locally in prior CI steps (no `--build-context` for bases) |
| Artifact upload | Manual inspection of `_output/` | `actions/upload-artifact` with `tempest-<release>-results` name |

**Artifact name:** `tempest-<release>-results` (e.g., `tempest-2025.2-results`, `tempest-2026.1-results`)
**Retention:** 14 days

### build-images.yaml — build-tempest Job

Builds the Tempest container image per release and per platform, runs verification on PRs,
and pushes by digest on push events. CC-0051 parameterized this job by release via the
`generate-matrix` job, which discovers all releases from `releases/*/` directories.

**Dependencies:** `needs: [lint-dockerfiles, merge-base-images, verify-base-images, generate-matrix]`

**Matrix strategy:** `release × platform × runner` (from `generate-matrix.tempest-matrix`; ARM64 excluded on PRs)

**Step sequence:**

| Step | Condition | Description |
| --- | --- | --- |
| Resolve Tempest versions | Always | Reads `releases/<release>/test-refs.yaml` via `yq` |
| Generate metadata | Always | OCI labels via `docker/metadata-action` (CC-0031) |
| Build Tempest image | PR: amd64 only; push: both platforms | `docker/build-push-action` with named build contexts, `upper-constraints` from `releases/<release>/` |
| Export digest | Push only | Writes digest to `/tmp/digests/` for merge job |
| Upload digest artifact | Push only | `digests-tempest-<release>-<platform-pair>`, 1-day retention |
| Scan for vulnerabilities | PR, amd64 only | Grype scan against loaded image (CC-0032) |
| Upload SARIF | PR, if scan produced output | GitHub Security tab under `grype-tempest-<release>-<platform>` |
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
security. CC-0051 parameterized this job by release via the `generate-matrix` job.

**Dependencies:** `needs: [build-tempest, generate-matrix]`
**Condition:** `if: github.event_name != 'pull_request'`
**Matrix strategy:** `release` (from `generate-matrix.tempest-release-matrix`)

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
| `<release>` | `ghcr.io/<owner>/tempest:2025.2` | Release series tag |
| `<tempest-version>` | `ghcr.io/<owner>/tempest:45.0.0` | Tempest PyPI version (main branch only) |
| `<release>-<commit-sha>` | `ghcr.io/<owner>/tempest:2025.2-<sha>` | Release + git commit for traceability |

**Supply chain security steps:**

| Step | Tool | Output |
| --- | --- | --- |
| SBOM generation | `anchore/sbom-action` | `sbom-tempest-<release>.cyclonedx.json` (CycloneDX format) |
| Vulnerability scan | `anchore/scan-action` (Grype) | SARIF uploaded to GitHub Security tab (`grype-tempest-<release>`) |
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
releases/<release>/test-refs.yaml ──yq──▶ TEMPEST_VERSION + KEYSTONE_TEMPEST_PLUGIN_VERSION
                         │
                         ▼
         docker build --build-arg TEMPEST_VERSION=... \
                      --build-arg KEYSTONE_TEMPEST_PLUGIN_VERSION=... \
                      --build-context upper-constraints=releases/<release>/ \
                      images/tempest/
                         │
                         ▼ (tempest job, per matrix.release)
         kubectl port-forward svc/<service-k8s-name> 5000:5000
                         │
                         ▼
         hack/ci-run-tempest.sh (resolve localhost URI + admin password)
                         │
                         ▼
         docker run --network host \
           -v tempest.conf:/etc/tempest/tempest.conf \
           -v phases/phase-1-core.txt:/etc/tempest/phases/phase-1-core.txt \
           -v phases/phase-2-plugin.txt:/etc/tempest/phases/phase-2-plugin.txt \
           -v exclude-tests.txt:/etc/tempest/exclude-tests.txt \
           -v extract-failed.py:/etc/tempest/extract-failed.py \
           -v merge-retry-junit.py:/etc/tempest/merge-retry-junit.py \
           -v run-tests.sh:/etc/tempest/run-tests.sh \
           c5c3/tempest:<release> bash /etc/tempest/run-tests.sh
         #
         # Internal logic of run-tests.sh (kept here for reference; the runner
         # scripts never invoke these steps inline — they always `bash
         # /etc/tempest/run-tests.sh` so CI and local runs share one code path):
         #
         #   tempest init . && cp tempest.conf etc/
         #   stestr run --include-list phases/phase-1-core.txt   --subunit → /output/phase-1-core.subunit
         #   stestr run --include-list phases/phase-2-plugin.txt --subunit → /output/phase-2-plugin.subunit
         #   cat phase-1-core.subunit phase-2-plugin.subunit > tempest.subunit
         #   subunit2junitxml < tempest.subunit > tempest-results.xml
         #   # retry any failed tests once serially, then rewrite the JUnit:
         #   python3 /etc/tempest/extract-failed.py tempest-results.xml > retry-list.txt
         #   stestr run --include-list retry-list.txt --concurrency 1 --subunit → /output/retry.subunit
         #   cat phase-1-core.subunit phase-2-plugin.subunit retry.subunit > tempest.subunit
         #   python3 /etc/tempest/merge-retry-junit.py tempest-results.xml retry.subunit
                         │
                         ▼
         actions/upload-artifact ──▶ tempest-<release>-results (14-day retention)
```

## Dependencies on Prior Features

| Feature | Artifact | Used by Tempest infrastructure |
| --- | --- | --- |
| CC-0006 | `images/python-base/Dockerfile`, `images/venv-builder/Dockerfile` | Base images for the Tempest Dockerfile build chain |
| CC-0006 | `releases/<release>/upper-constraints.txt` | Dependency constraints for PyPI installs |
| CC-0028 | `tests/lib/assertions.sh` | Assertion helpers sourced by `verify_tempest.sh` |
| CC-0029 | SBOM attestation pattern in `build-images.yaml` | Reused by `merge-tempest-image` for Tempest SBOM |
| CC-0030 | Cosign signing pattern in `build-images.yaml` | Reused by `merge-tempest-image` for Tempest signing |
| CC-0031 | Two-layer OCI annotation pattern | Static Dockerfile labels + CI metadata-action |
| CC-0032 | Grype scanning pattern in `build-images.yaml` | Reused by `build-tempest` and `merge-tempest-image` |

:::
