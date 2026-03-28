---
title: Tempest Test Infrastructure
quadrant: infrastructure
feature: CC-0035
---

::: v-pre

# Tempest Test Infrastructure

Reference documentation for the Tempest API test infrastructure (CC-0035). This covers
the container image build, test configuration, orchestration script, Makefile target,
CI pipeline integration, and supply-chain security for the Tempest testing framework.

Tempest is an OpenStack integration test framework that validates API behavior against
deployed services. The infrastructure described here enables running Tempest identity API
tests against the Keystone service deployed in a kind cluster — both locally via
`make tempest-test` and automatically in CI after Chainsaw E2E tests.

For the Keystone Chainsaw E2E test suites, see
[Keystone E2E Test Suites](./keystone-e2e-tests.md). For the container image build
hierarchy, see [Container Images](./container-images.md). For the build-images CI
workflow, see [Build Images Workflow](./build-images-workflow.md). For the CI workflow
structure, see [CI Workflow](./ci-workflow.md).

## Overview

The Tempest test infrastructure has four layers: version pinning, container image,
test configuration, and execution orchestration. Versions flow from a single source
of truth (`test-refs.yaml`) through the Dockerfile and CI workflows, ensuring
consistency across local and CI execution.

```text
                          releases/2025.2/test-refs.yaml
                            tempest: "45.0.0"
                            keystone-tempest-plugin: "0.19.0"
                                      │
                    ┌─────────────────┼──────────────────┐
                    ▼                 ▼                   ▼
           images/tempest/     hack/run-tempest.sh    .github/workflows/
           Dockerfile          (local execution)      ci.yaml  build-images.yaml
                │                     │                  │            │
                ▼                     ▼                  ▼            ▼
         ┌─────────────┐    ┌──────────────┐    ┌──────────┐  ┌───────────────┐
         │ tempest:tag  │───▶ kind cluster  │    │ e2e-     │  │ build-tempest │
         │ (OCI image)  │    │ (local pod)  │    │ keystone │  │ (GHCR push)   │
         └─────────────┘    └──────────────┘    │ (CI pod) │  │ SBOM + Grype  │
                                    │            └──────────┘  │ Attest+Cosign │
                                    ▼                  │       └───────────────┘
                           _output/reports/            ▼
                           tempest-keystone-     JUnit XML artifact
                           results.xml           (14-day retention)
```

## File Layout

```text
releases/2025.2/
└── test-refs.yaml                   Version pins for test tooling (CC-0035 REQ-001)

images/tempest/
└── Dockerfile                       2-stage build: venv-builder → python-base (CC-0035 REQ-002)

tests/tempest/keystone/
├── tempest.conf                     Keystone-specific Tempest configuration (CC-0035 REQ-003)
├── include-tests.txt                Regex patterns for tests to run (CC-0035 REQ-004)
└── exclude-tests.txt                Regex patterns for tests to skip (CC-0035 REQ-004)

tests/container-images/
└── verify_tempest.sh                Image verification script (CC-0035 REQ-005)

hack/
└── run-tempest.sh                   Local orchestration script (CC-0035 REQ-006)

Makefile                             tempest-test target (CC-0035 REQ-007)

.github/workflows/
├── ci.yaml                          Tempest steps in e2e-keystone job (CC-0035 REQ-008)
└── build-images.yaml                build-tempest-image job + lint matrix (CC-0035 REQ-009, REQ-010)
```

## Version Pinning

**File:** `releases/2025.2/test-refs.yaml`

Test tooling versions are pinned in `test-refs.yaml`, separate from `source-refs.yaml`
which tracks upstream OpenStack git refs. This separation allows test framework versions
to evolve independently of service versions (CC-0035 REQ-001).

```yaml
tempest: "45.0.0"
keystone-tempest-plugin: "0.19.0"
```

| Key | Value | Purpose |
| --- | --- | --- |
| `tempest` | `"45.0.0"` | Tempest framework version installed from PyPI |
| `keystone-tempest-plugin` | `"0.19.0"` | Keystone-specific Tempest test plugin from PyPI |

Both CI workflows and the orchestration script resolve versions from this file via `yq`,
following the same pattern as `source-refs.yaml` resolution in existing workflows:

```bash
tempest_version=$(yq '.tempest' releases/2025.2/test-refs.yaml)
```

Each resolution includes a null/empty guard that fails the step with a descriptive error
if the version is missing or malformed.

## Container Image

**File:** `images/tempest/Dockerfile`

The Tempest image uses the same two-stage build pattern as service images (e.g., Keystone),
but differs in that it installs from PyPI rather than from a git source mount.

### Stage 1: Build (extends `venv-builder`)

Installs Tempest and test dependencies into the shared virtualenv at `/var/lib/openstack`
using `uv pip install` with upper-constraints:

| Package | Source | Purpose |
| --- | --- | --- |
| `tempest==${TEMPEST_VERSION}` | PyPI | OpenStack integration testing framework |
| `keystone-tempest-plugin==${KEYSTONE_TEMPEST_PLUGIN_VERSION}` | PyPI | Keystone-specific Tempest test plugins |
| `python-subunit` | PyPI | Subunit test result stream library |
| `junitxml` | PyPI | Subunit-to-JUnit XML conversion (`subunit2junitxml` CLI) |

Build arguments (`TEMPEST_VERSION`, `KEYSTONE_TEMPEST_PLUGIN_VERSION`) are resolved from
`test-refs.yaml` and passed via `--build-arg`. The named build context
`upper-constraints` supplies `releases/2025.2/upper-constraints.txt` for dependency
pinning.

### Stage 2: Runtime (extends `python-base`)

Copies the virtualenv from the build stage and sets static OCI labels. No additional
runtime apt packages are needed (unlike Keystone which requires LDAP and XML libraries).

| Property | Value |
| --- | --- |
| Base image | `python-base` (local) |
| User | `openstack` (UID 42424, GID 42424) |
| Virtualenv | `/var/lib/openstack` (copied from build stage via `--link`) |
| OCI labels | title=`tempest`, description, licenses=Apache-2.0, vendor=SAP SE |

### Differences from Service Images (e.g., Keystone)

| Aspect | Keystone | Tempest |
| --- | --- | --- |
| Source | Git clone via named build context | PyPI via `uv pip install` |
| `ARG PIP_EXTRAS` | Yes (e.g., `ldap,oauth1`) | No (no optional extras) |
| `ARG EXTRA_APT_PACKAGES` | Yes (LDAP, XML libs) | No (no runtime apt deps) |
| Version source | `source-refs.yaml` (git ref) | `test-refs.yaml` (PyPI version) |
| Entry point | WSGI server (`uwsgi`) | CLI tool (`tempest run`) |

### Local Build

```bash
# Build base images (if not already built)
docker build images/python-base -t python-base
docker build images/venv-builder -t venv-builder

# Build Tempest image
docker build images/tempest \
  -t c5c3/tempest:45.0.0 \
  --build-arg TEMPEST_VERSION=45.0.0 \
  --build-arg KEYSTONE_TEMPEST_PLUGIN_VERSION=0.19.0 \
  --build-context upper-constraints=releases/2025.2/
```

## Test Configuration

Per-service test configuration lives in `tests/tempest/<service>/`. Each service
directory contains three files that are mounted into the Tempest pod as a ConfigMap.

### tempest.conf

**File:** `tests/tempest/keystone/tempest.conf`

INI-format configuration targeting the Keystone service deployed by Chainsaw E2E in the
kind cluster.

| Section | Key | Value | Purpose |
| --- | --- | --- | --- |
| `[DEFAULT]` | `log_dir` | `/tmp/tempest-logs` | Tempest log output directory |
| `[DEFAULT]` | `log_file` | `tempest.log` | Log filename |
| `[identity]` | `uri_v3` | `http://keystone-basic-api.openstack.svc:5000/v3` | Keystone v3 API endpoint (cluster DNS) |
| `[auth]` | `admin_username` | `admin` | Admin user for API authentication |
| `[auth]` | `admin_password` | `${KEYSTONE_ADMIN_PASSWORD}` | Placeholder substituted at pod runtime |
| `[auth]` | `admin_project_name` | `admin` | Admin project scope |
| `[auth]` | `admin_domain_name` | `Default` | Admin domain scope |
| `[auth]` | `use_dynamic_credentials` | `false` | Disables dynamic credential creation |
| `[service_available]` | `identity` | `true` | Only Keystone is deployed |
| `[service_available]` | `compute`, `network`, `volume`, `image`, `object-storage` | `false` | Services not deployed in test cluster |
| `[identity-feature-enabled]` | `api_v3` | `true` | Enable v3 API tests |

**Credential injection:** The `${KEYSTONE_ADMIN_PASSWORD}` placeholder is not a shell
variable — it is a literal string in the config file. At pod startup, the orchestration
script uses `sed` to replace it with the actual password from the `keystone-admin`
Kubernetes Secret (injected as an environment variable via `secretKeyRef`).

### include-tests.txt

**File:** `tests/tempest/keystone/include-tests.txt`

One regex pattern per line. Only identity-related tests are included since only Keystone
is deployed in the test cluster.

| Pattern | Scope |
| --- | --- |
| `tempest.api.identity` | All Tempest core identity API tests |
| `keystone_tempest_plugin.tests` | All keystone-tempest-plugin tests |

### exclude-tests.txt

**File:** `tests/tempest/keystone/exclude-tests.txt`

One regex pattern per line. Excludes tests that depend on services or infrastructure not
available in the test cluster.

| Pattern | Reason |
| --- | --- |
| `tempest\.api\.identity\..*compute` | Requires Nova (not deployed) |
| `tempest\.api\.identity\..*network` | Requires Neutron (not deployed) |
| `keystone_tempest_plugin\.tests\..*ldap` | Requires external LDAP server |
| `keystone_tempest_plugin\.tests\..*federation` | Requires external IdP (SAML2) |
| `keystone_tempest_plugin\.tests\..*oauth2` | Requires external IdP (OIDC) |

### Adding a New Service

To add Tempest tests for a new service (e.g., `glance`):

1. Create `tests/tempest/glance/` with `tempest.conf`, `include-tests.txt`, and
   `exclude-tests.txt`
2. Set `uri` to the service's cluster-internal DNS name
3. Enable the service in `[service_available]`
4. Add include/exclude patterns for the service's test scope
5. Run with `make tempest-test SERVICE=glance`

## Image Verification

**File:** `tests/container-images/verify_tempest.sh`

Validates that the built Tempest image meets requirements. Follows the established
PASS/FAIL counter pattern from existing `verify_*.sh` scripts (CC-0035 REQ-005).

**Usage:** `bash tests/container-images/verify_tempest.sh [image_name]`

**Default image:** `c5c3/tempest:45.0.0`

| Test | Assertion | Validates |
| --- | --- | --- |
| `test_tempest_version` | `tempest --version` exits 0, output non-empty | Tempest framework installed and runnable |
| `test_keystone_tempest_plugin_importable` | `python3 -c 'import keystone_tempest_plugin'` exits 0 | Plugin package installed correctly |
| `test_subunit2junitxml_available` | `which subunit2junitxml` exits 0, path non-empty | JUnit XML converter on PATH |
| `test_runs_as_openstack_user` | `whoami` outputs `openstack` | Non-root execution (UID 42424) |
| `test_no_build_tools_in_final_image` | `gcc`, `python3-dev`, `uv` absent | Build tools not leaked to runtime image |

The script sources `tests/lib/assertions.sh` for `assert_eq`, `assert_not_empty`, and
`assert_nonzero_exit` helpers. Each command substitution uses the `|| exit_code=$?`
guard pattern to prevent `set -e` from aborting before assertions run.

## Orchestration Script

**File:** `hack/run-tempest.sh`

Orchestrates the full Tempest test lifecycle for local execution. Follows the pattern
from `hack/deploy-infra.sh` (CC-0010): `set -euo pipefail`, `log()` with ISO 8601
timestamps, `SCRIPT_DIR`/`REPO_ROOT` resolution (CC-0035 REQ-006).

**Usage:** `hack/run-tempest.sh <SERVICE>`

### Configuration

All configuration is via environment variables with sensible defaults:

| Variable | Default | Purpose |
| --- | --- | --- |
| `CLUSTER_NAME` | `forge-e2e` | Kind cluster name |
| `TEMPEST_TIMEOUT` | `600` | Timeout in seconds for Tempest pod completion |
| `TEMPEST_IMAGE` | `c5c3/tempest:local` | Docker image tag for the Tempest image |
| `TEMPEST_NAMESPACE` | `openstack` | Kubernetes namespace for Tempest pod and ConfigMap |
| `OUTPUT_DIR` | `_output/reports` | Directory for JUnit XML output |
| `SKIP_BUILD` | `false` | Set to `true` to skip building the image (CI uses this) |

### Lifecycle Stages

The script executes seven stages sequentially:

```text
validate() ──▶ build_tempest_image() ──▶ load_image() ──▶ create_configmap()
                                                                  │
                  cleanup() ◀── collect_results() ◀── run_tempest()
```

| Stage | Function | Details |
| --- | --- | --- |
| 1. Validate | `validate()` | Checks SERVICE arg, config files exist, required CLI tools (`docker`, `kind`, `kubectl`, `yq`), kind cluster exists |
| 2. Build | `build_tempest_image()` | Builds the full image chain (python-base, venv-builder, tempest) with versions from `test-refs.yaml`. Skipped when `SKIP_BUILD=true` |
| 3. Load | `load_image()` | Loads the Docker image into the kind cluster via `kind load docker-image` |
| 4. ConfigMap | `create_configmap()` | Creates a Kubernetes ConfigMap (`tempest-config-<service>`) from `tempest.conf`, `include-tests.txt`, `exclude-tests.txt` |
| 5. Run | `run_tempest()` | Applies an inline Pod manifest that substitutes credentials, initializes a Tempest workspace, runs tests, and converts results to JUnit XML |
| 6. Collect | `collect_results()` | Copies JUnit XML from the pod to `OUTPUT_DIR`, prints pod logs for visibility |
| 7. Cleanup | `cleanup()` | Deletes the Tempest pod and ConfigMap to prevent resource accumulation across local retries |

Results and cleanup always run regardless of test outcome (these calls are
outside the error-checking block). The script exits non-zero if any Tempest test fails.

### Pod Specification

The Tempest pod created by `run_tempest()`:

| Property | Value |
| --- | --- |
| Name | `tempest-<service>` |
| Image | `${TEMPEST_IMAGE}` with `imagePullPolicy: Never` |
| Restart policy | `Never` |
| Config mount | `/mnt/tempest-config` (read-only, from ConfigMap) |
| Credential source | `KEYSTONE_ADMIN_PASSWORD` env var from `keystone-admin` Secret |
| Result location | `/tmp/tempest-results.xml` (JUnit XML) |

The pod's inline script:

1. Initializes a Tempest workspace with `tempest init workspace`
2. Substitutes `${KEYSTONE_ADMIN_PASSWORD}` in `tempest.conf` via `sed`
3. Runs `tempest run --include-list --exclude-list`
4. Converts subunit output to JUnit XML via `tempest last --subunit | subunit2junitxml`
5. Exits with the Tempest exit code (non-zero on test failure)

### Error Handling

| Failure Mode | Behavior |
| --- | --- |
| Missing SERVICE argument | Prints usage message, exits 1 |
| Missing config files | Logs specific missing file path, exits 1 |
| Missing CLI tools | Logs which tool is missing, exits 1 |
| Kind cluster not found | Logs cluster name and suggests `make deploy-infra`, exits 1 |
| `test-refs.yaml` version null/empty | Logs descriptive error, exits 1 |
| Tempest pod timeout | Logs timeout duration and pod phase, exits non-zero |
| Tempest test failure | Logs failure, collects results, exits non-zero |
| JUnit XML not found in pod | Logs warning, continues (best-effort collection) |

## Makefile Target

**File:** `Makefile` (lines 242-248)

```makefile
.PHONY: tempest-test
tempest-test:
	$(if $(SERVICE),,$(error tempest-test requires SERVICE, e.g. make tempest-test SERVICE=keystone))
	hack/run-tempest.sh $(SERVICE)
```

The `$(if)` guard validates the `SERVICE` parameter before delegation. This matches the
existing `$(if)` pattern used by other parameterized Makefile targets (CC-0035 REQ-007).

**Usage:**

```bash
# Run Tempest identity API tests against Keystone
make tempest-test SERVICE=keystone

# Override defaults
TEMPEST_TIMEOUT=900 make tempest-test SERVICE=keystone

# Skip image build (image already loaded)
SKIP_BUILD=true make tempest-test SERVICE=keystone
```

## CI Integration

### e2e-keystone Job (ci.yaml)

Three steps are appended to the `e2e-keystone` job **after** the Chainsaw E2E test step
(CC-0035 REQ-008). This ensures Tempest API tests run only after the operator and
Keystone service are fully deployed and validated by Chainsaw.

| Step | Name | Details |
| --- | --- | --- |
| 1 | Build Tempest image | Resolves versions from `test-refs.yaml` via `yq`, builds `c5c3/tempest:local` using Docker layer cache (base images already built by earlier steps) |
| 2 | Run Tempest API tests | Invokes `hack/run-tempest.sh ${{ matrix.operator }}` with `SKIP_BUILD=true` (reuses the image built in step 1) |
| 3 | Upload Tempest report | Uploads `_output/reports/tempest-*` as artifact `tempest-<operator>-junit-report` with 14-day retention. Runs with `if: always()` to capture results even on failure |

**Version resolution guard:** If `yq` returns null or empty for a version, the build step
fails with a descriptive `::error::` annotation before attempting the Docker build.

### build-tempest-image Job (build-images.yaml)

A dedicated job that builds, scans, signs, and pushes the Tempest image to GHCR. Follows
the same supply-chain security pattern as service images (CC-0035 REQ-009).

**Dependencies:** `needs: [build-base-images, verify-base-images]`

**Permissions:** `contents: read`, `packages: write`, `id-token: write`,
`attestations: write`, `security-events: write`

| Step | Action | PR Behavior | Push Behavior |
| --- | --- | --- | --- |
| Resolve versions | `yq` from `test-refs.yaml` | Same | Same |
| Derive tags | Composite tag: `<version>-<branch>-<sha>` | Same | Same |
| Build image | `docker/build-push-action` | `load: true` (local, amd64 only) | `push: true` (linux/amd64,linux/arm64) |
| Generate SBOM | `anchore/sbom-action` (cyclonedx-json) | Skipped | Generated from pushed image |
| Grype scan | `anchore/scan-action` (severity: high) | Scans local image | Scans SBOM |
| Upload SARIF | `github/codeql-action/upload-sarif` | Category: `grype-tempest` | Category: `grype-tempest` |
| SBOM attestation | `actions/attest` | Skipped | Pushed to registry |
| Cosign signing | `cosign sign --yes` | Skipped | Keyless signing |
| Verify image | `verify_tempest.sh` | Runs against local image | Skipped (done by verify job) |

**Image tagging:**

| Tag | Example | When Applied |
| --- | --- | --- |
| Composite | `45.0.0-main-abc1234` | Always |
| Version | `45.0.0` | Push to main only |
| SHA | `abc1234` | Always |

**Base image resolution:** On push events, the build uses
`docker-image://<registry-image>` URIs for `python-base` and `venv-builder` build
contexts, consuming the images pushed by the `build-base-images` job. On PRs, images
are loaded locally.

### lint-dockerfiles Job (build-images.yaml)

`images/tempest/Dockerfile` is included in the hadolint matrix alongside all other
Dockerfiles (CC-0035 REQ-010):

```yaml
strategy:
  matrix:
    dockerfile:
      - images/python-base/Dockerfile
      - images/venv-builder/Dockerfile
      - images/keystone/Dockerfile
      - images/tempest/Dockerfile
      - operators/keystone/Dockerfile
```

Hadolint runs with `failure-threshold: warning` using the shared `.hadolint.yaml`
configuration. The Tempest Dockerfile uses `# hadolint ignore=DL3006` on both `FROM`
directives (consistent with existing Dockerfiles) to suppress the "pin image version"
warning for locally-referenced base images.

## Data Flow (CI — e2e-keystone)

Complete data flow from version pins to artifact upload:

```text
test-refs.yaml ──yq──▶ TEMPEST_VERSION + KEYSTONE_TEMPEST_PLUGIN_VERSION
                                │
                                ▼
                 docker build --build-arg TEMPEST_VERSION=... \
                              --build-arg KEYSTONE_TEMPEST_PLUGIN_VERSION=... \
                              --build-context upper-constraints=releases/2025.2 \
                              images/tempest/
                                │
                                ▼
                 kind load docker-image c5c3/tempest:local --name forge-e2e
                                │
                                ▼
                 kubectl create configmap tempest-config-keystone \
                   --from-file=tempest.conf \
                   --from-file=include-tests.txt \
                   --from-file=exclude-tests.txt
                                │
                                ▼
                 kubectl apply -f <inline-pod-manifest>
                 ┌──────────────────────────────────────────────────┐
                 │  Pod: tempest-keystone                           │
                 │  Image: c5c3/tempest:local (imagePullPolicy:    │
                 │         Never)                                   │
                 │  Env: KEYSTONE_ADMIN_PASSWORD (from              │
                 │       keystone-admin Secret)                     │
                 │  Mount: /mnt/tempest-config (from ConfigMap)      │
                 │                                                  │
                 │  1. tempest init workspace                       │
                 │  2. sed substitute password in tempest.conf      │
                 │  3. tempest run --include-list --exclude-list    │
                 │  4. tempest last --subunit | subunit2junitxml    │
                 │     ▶ /tmp/tempest-results.xml                   │
                 └──────────────────────────────────────────────────┘
                                │
                                ▼
                 kubectl cp tempest-keystone:/tmp/tempest-results.xml
                   ▶ _output/reports/tempest-keystone-results.xml
                                │
                                ▼
                 actions/upload-artifact
                   ▶ tempest-keystone-junit-report (14-day retention)
```

## Prerequisites

### Local Execution

| Prerequisite | Details |
| --- | --- |
| Docker | Required for building the Tempest image chain |
| kind | Kubernetes cluster (`forge-e2e`) must be running |
| kubectl | Kubernetes CLI for pod and ConfigMap management |
| yq | YAML processor for extracting versions from `test-refs.yaml` |
| Infrastructure stack | Deployed via `make deploy-infra` |
| Keystone operator | Deployed with Keystone CR in `openstack` namespace |
| `keystone-admin` Secret | Must exist in `openstack` namespace with `password` key |

### CI Execution

The `e2e-keystone` job handles all prerequisites automatically:

1. Base images (`python-base`, `venv-builder`) are built by the "Build service image"
   step before the Tempest steps execute
2. Infrastructure and Keystone are deployed by the Helm install step
3. Chainsaw E2E tests validate Keystone readiness before Tempest runs
4. The `keystone-admin` Secret is created by the ExternalSecret controller during
   infrastructure deployment

## Related Resources

- [Container Images](./container-images.md) — Dockerfile hierarchy, base images, and release configuration (CC-0006)
- [Build Images Workflow](./build-images-workflow.md) — SBOM, Grype, attestation, and cosign pipeline (CC-0007, CC-0029, CC-0030, CC-0032)
- [CI Workflow](./ci-workflow.md) — CI job dependency DAG and e2e-keystone job structure (CC-0003, CC-0018)
- [Keystone E2E Test Suites](./keystone-e2e-tests.md) — Chainsaw E2E tests that run before Tempest (CC-0016)
- [Infrastructure E2E Deployment](./infrastructure/e2e-deployment.md) — Infrastructure stack deployment (CC-0010)
- `releases/2025.2/test-refs.yaml` — Version pins (single source of truth)
- `hack/run-tempest.sh` — Orchestration script source
- `tests/container-images/verify_tempest.sh` — Image verification script source

:::
