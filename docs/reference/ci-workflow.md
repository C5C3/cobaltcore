---
title: CI Workflow
quadrant: infrastructure
feature: CC-0003, CC-0018, CC-0041, CC-0050
---

::: v-pre

# CI Workflow

Reference documentation for the GitHub Actions CI workflow (CC-0003, CC-0018, CC-0041, CC-0050).

CC-0050 refactored repeated E2E logic into reusable shell scripts (`hack/ci-*.sh`) and a
composite GitHub Action (`.github/actions/setup-e2e-infra/`), reducing duplication across
the `e2e-infra`, `e2e-operator`, and `tempest` jobs.

## File Location

`.github/workflows/ci.yaml`

The file uses the `.yaml` extension (matching `reuse.yaml` and `deploy-docs.yaml`) and
quotes the trigger key as `"on"` to prevent YAML boolean interpretation (REQ-001).

## Trigger Events

The workflow triggers on three event types (CC-0003 REQ-008, CC-0018 REQ-001):

| Event | Scope | Description |
| --- | --- | --- |
| `push` | `branches: [main]` | Runs on every push to the main branch |
| `push` | `tags: ["v*"]` | Runs on every v-prefixed tag push (triggers publish and release jobs) |
| `pull_request` | `branches: [main]` | Runs on every pull request targeting main |

Tag pushes (`v*`) enable the full release pipeline: gate jobs, E2E tests, image/chart
publishing, and GitHub Release creation. Pull requests and main-branch pushes run only
gate and E2E jobs (publish jobs are skipped via `if` conditions).

## Environment Variables

Top-level environment variables centralise registry configuration and pin tool versions
for CI reproducibility (CC-0018):

```yaml
env:
  REGISTRY: ghcr.io
  IMAGE_PREFIX: ghcr.io/c5c3
  CONTROLLER_GEN_VERSION: v0.20.1
```

`REGISTRY` and `IMAGE_PREFIX` are referenced by the `build-and-push`, `helm-push`,
`e2e-operator`, and `tempest` jobs to construct image names and registry URLs.
`CONTROLLER_GEN_VERSION` is used by `verify-codegen` to pin controller-gen to a specific
version. `setup-envtest` is installed via `@release-0.23` because the sub-module does not
publish its own release tags.

## Permissions

Top-level permissions are restricted to least privilege (CC-0003 REQ-007):

```yaml
permissions:
  contents: read
```

Jobs that need elevated access declare per-job `permissions:` blocks:

| Job | Additional Permissions | Reason |
| --- | --- | --- |
| `build-and-push` | `packages: write` | Push per-platform operator image digests to GHCR |
| `merge-operator-images` | `packages: write` | Push final multi-arch operator image manifest list |
| `helm-push` | `packages: write` | Push Helm charts to GHCR OCI registry |
| `github-release` | `contents: write` | Create GitHub Releases |

## Job Dependency DAG

The workflow defines 14 jobs organised in a directed acyclic graph (CC-0018, CC-0041,
CC-0050):

```
Gate Jobs (always run):
  lint ─────────────┐
  shellcheck ───────┤
  verify-codegen ───┤
  test (matrix) ────┼──> E2E Jobs + Publish Jobs
  test-integration ─┘

Conditional Jobs (path-filtered via changes job):
  helm-validate ──> needs: [changes], if: helm == 'true'
  docs ──────────> needs: [changes], if: docs == 'true'

E2E Jobs (depends on gates):
  e2e-infra ──────> needs: [changes], if: e2e-infra == 'true'
  e2e-operator ───> needs: [changes, lint, shellcheck, test, test-integration, verify-codegen]
  tempest ────────> needs: [changes, lint, shellcheck, test, test-integration, verify-codegen]

Publish Jobs (main/tags only, depends on E2E):
  build-and-push (matrix: operator × platform) ──> needs: [changes, e2e-operator], if: push event
    └──> merge-operator-images ──> needs: [changes, build-and-push], if: push event
  helm-push ──> needs: [changes, e2e-operator], if: push event

Release Job (v* tags only, depends on publish):
  github-release ──> needs: [changes, merge-operator-images, helm-push], if: v* tag
```

The three E2E jobs (`e2e-infra`, `e2e-operator`, `tempest`) share infrastructure setup via
the `setup-e2e-infra` composite action and diagnostic teardown via `hack/ci-dump-diagnostics.sh`
(CC-0050).

## Jobs

### lint

Runs golangci-lint using the project's `.golangci.yml` configuration (CC-0003 REQ-002).

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `golangci/golangci-lint-action@v9` | Installs golangci-lint binary (`install-only: true`); version pinned to `v2.11.4` |
| 4 | `make lint` | Runs golangci-lint per module via the Makefile |

The `golangci-lint-action@v9` step is used with `install-only: true`, which installs the
pinned golangci-lint binary (and caches it) without running lint. The actual linting is
delegated to `make lint`, which `cd`s into each module directory and runs
`golangci-lint run ./...` — a necessary pattern for Go multi-module workspaces. The
`actions/setup-go@v6` step is required because `install-only` mode does not set up Go
internally.

### shellcheck

Validates shell scripts with shellcheck to catch scripting issues early (CC-0010).
The shellcheck binary is pre-installed on `ubuntu-latest` runners.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `shellcheck --severity=warning hack/*.sh` | Lints all shell scripts in `hack/` |

Timeout: 5 minutes.

### test

Runs unit tests with a matrix strategy over `[common, keystone, c5c3]` (CC-0018 REQ-002).
Each matrix leg tests a single target — either `internal/common` or one operator — producing
a single coverage profile uploaded to Codecov under a dedicated flag (REQ-004).

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `make test-common` or `make test-operator` | Runs unit tests for the matrix target |
| 4 | `codecov/codecov-action@v5` | Uploads coverage profile with target-specific flag |

**Matrix strategy:**

```yaml
strategy:
  fail-fast: false
  matrix:
    target: [common, keystone, c5c3]
```

The `common` leg runs `make test-common` (producing `cover-unit-common.out`). Operator legs
run `make test-operator OPERATOR=<target>` (producing `cover-unit-<operator>.out`). This
deduplicates common coverage into a single leg instead of uploading it under each operator
flag.

**Coverage upload:**

```yaml
files: cover-unit-${{ matrix.target }}.out
flags: unit-${{ matrix.target }}
```

The `if: always()` condition ensures coverage is uploaded even when tests fail, so partial
coverage data is not lost.

### test-integration

Runs envtest-based integration tests with a matrix strategy over `[common, keystone, c5c3]`
and coverage uploaded to Codecov (CC-0018 REQ-003, REQ-004). Requires `setup-envtest` to
download kubebuilder assets (kube-apiserver, etcd) for the test API server.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `go install setup-envtest@release-0.23` | Installs envtest asset downloader (pinned to release branch) |
| 4 | `make test-integration-common` or `make test-integration` | Runs integration tests for the matrix target |
| 5 | `codecov/codecov-action@v5` | Uploads coverage with `integration-<target>` flag |

**Matrix strategy:**

```yaml
strategy:
  fail-fast: false
  matrix:
    target: [common, keystone, c5c3]
```

The `common` leg runs `make test-integration-common` (producing
`cover-integration-common.out`), which tests `./internal/common/...` with
`-tags=integration`. Operator legs run `make test-integration OPERATOR=<target>` (producing
`cover-integration-<operator>.out`). Both targets set `KUBEBUILDER_ASSETS` via
`$(SETUP_ENVTEST) use <pinned-k8s-version> -p path`.

Timeout: 15 minutes (longer than unit tests to account for envtest startup).

### verify-codegen

Verifies that generated code (CRD manifests, deepcopy functions) is committed and
up-to-date (CC-0018 REQ-009). This is a gate job — it blocks merge alongside `lint`,
`test`, and `shellcheck`.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `go install controller-gen@${{ env.CONTROLLER_GEN_VERSION }}` | Installs the pinned code generator |
| 4 | `make manifests && make generate` | Regenerates CRD manifests and deepcopy functions |
| 5 | `make verify-crd-sync` | Verifies Helm chart CRD copies match controller-gen output |
| 6 | `git diff --exit-code` | Fails if any files changed (stale generated code) |

When the diff check fails, the job produces a GitHub Actions `::error::` annotation with
instructions to run `make manifests && make generate` locally and commit the result.

### docs

Builds the VitePress documentation site to catch broken links and build errors (CC-0003).

**Dependencies:** `needs: [changes]`
**Condition:** `if: needs.changes.outputs.docs == 'true'`
**Path filter:** `docs/**`, `package.json`, `package-lock.json`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Full history (`fetch-depth: 0`) for git-based features |
| 2 | `actions/setup-node@v6` | Node.js 24, npm cache enabled |
| 3 | `npm ci` | Installs dependencies from lockfile |
| 4 | `npm run docs:build` | Builds the documentation site |

### helm-validate

Validates Helm chart structure, template rendering, and unit tests without requiring a
cluster (CC-0041). Runs `helm lint`, `helm template` with five value override scenarios,
and `helm unittest` to catch chart regressions at PR time.

**Dependencies:** `needs: [changes]`
**Condition:** `if: needs.changes.outputs.helm == 'true'`
**Path filter:** `operators/keystone/helm/**` (forced `true` on `v*` tag pushes)

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `azure/setup-helm@v5` | Installs Helm CLI (SHA-pinned) |
| 3 | `helm plugin install helm-unittest` | Installs helm-unittest plugin (pinned to `v1.0.3`) |
| 4 | `helm lint` | Validates chart structure and syntax for `operators/keystone/helm/keystone-operator/` |
| 5 | `helm template` (5 scenarios) | Renders chart with value overrides to catch broken conditionals and invalid YAML |
| 6 | `helm unittest` | Runs unit test suites from `operators/keystone/helm/keystone-operator/tests/` |

**Template scenarios (step 5):**

| Scenario | Values | Purpose |
| --- | --- | --- |
| 1 — default values | (none) | Validates baseline rendering with chart defaults |
| 2 — webhook disabled | `webhook.enabled=false` | Validates conditional exclusion of webhook resources |
| 3 — external service account | `serviceAccount.create=false`, `serviceAccount.name=existing-sa` | Validates ServiceAccount conditional logic |
| 4 — custom resources | `resources.limits.cpu=100m`, `resources.limits.memory=64Mi` | Validates resource override wiring |
| 5 — namespace-scoped RBAC | `rbac.namespaceScoped=true`, `webhook.enabled=false` | Validates Role/RoleBinding rendering instead of ClusterRole/ClusterRoleBinding (CC-0043) |

**Unit test suites (step 6):**

| Test File | Template Under Test | Key Assertions |
| --- | --- | --- |
| `deployment_test.yaml` | `deployment.yaml` | Image, replicas, resources, securityContext, probes, args, conditional webhook volume mount |
| `clusterrole_test.yaml` | `clusterrole.yaml` | All 14 RBAC rule blocks with correct verbs |
| `clusterrolebinding_test.yaml` | `clusterrolebinding.yaml` | roleRef and ServiceAccount subject binding |
| `service_test.yaml` | `service.yaml` | Metrics port (8080), conditional webhook port (443→9443) |
| `serviceaccount_test.yaml` | `serviceaccount.yaml` | Conditional creation (create=true/false), custom name override, standard labels |
| `webhook_test.yaml` | `webhook-configuration.yaml` | Mutating/Validating configs when enabled, absent when disabled, cert-manager annotation |
| `certificate_test.yaml` | `certificate.yaml` | Issuer and Certificate when enabled, absent when disabled, DNS names, issuer reference |

Timeout: 10 minutes.

### e2e-infra

End-to-end infrastructure deployment and Chainsaw test (CC-0010, CC-0050). Deploys the full
infrastructure stack (Flux, cert-manager, MariaDB, ESO, OpenBao) to a kind cluster and
validates health of all operators, CRs, and ExternalSecrets.

**Dependencies:** `needs: [changes]`
**Condition:** `if: needs.changes.outputs.e2e-infra == 'true'`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `helm/kind-action@v1.14.0` | Creates kind cluster (`forge-e2e`) |
| 4 | `setup-e2e-infra` composite action | Installs Flux CLI, test deps, and deploys infra stack (CC-0050 REQ-005) |
| 5 | `chainsaw test` | Runs E2E tests from `tests/e2e/infrastructure/` |
| 6 | `hack/ci-dump-diagnostics.sh` (on failure) | Dumps HelmReleases, pods, events, Flux logs (CC-0050 REQ-001) |
| 7 | Upload JUnit report | Uploads test results as artifact (14-day retention) |

Timeout: 20 minutes.

### e2e-operator

End-to-end operator test using kind cluster and Chainsaw (CC-0018 REQ-005, CC-0050).
Builds the operator and service images locally, loads them into kind, deploys the
infrastructure stack and operator via Helm, and runs Chainsaw E2E test suites.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen]`
**Condition:** Runs only when `has-e2e-operators == 'true'` and all gate jobs succeeded.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `helm/kind-action@v1.14.0` | Creates kind cluster (`forge-e2e`) |
| 4 | `make docker-build` | Builds operator image with tag `<IMAGE_PREFIX>/<operator>-operator:dev` |
| 5 | `hack/ci-build-service-image.sh` | Builds the OpenStack service image chain (CC-0050 REQ-002) |
| 6 | `kind load docker-image` | Loads operator, service, and upgrade-tagged images into kind |
| 7 | `setup-e2e-infra` composite action | Installs Flux CLI, test deps, and deploys infra stack (CC-0050 REQ-005) |
| 8 | `hack/ci-deploy-operator.sh` | Installs CRDs and deploys operator via Helm (CC-0050 REQ-003) |
| 9 | `chainsaw test` | Runs E2E tests from `tests/e2e/<operator>/` |
| 10 | `hack/ci-dump-diagnostics.sh` (always) | Dumps operator pods, all pods, events, operator logs (CC-0050 REQ-001) |
| 11 | Upload JUnit report | Uploads test results as artifact (14-day retention) |

**Matrix strategy:**

```yaml
strategy:
  fail-fast: false
  matrix: ${{ fromJson(needs.changes.outputs.e2e-operators) }}
```

The operator matrix is dynamically constructed by the `changes` job, including only operators
whose code (or shared code) changed. The `imagePullPolicy: Never` Helm value ensures the
kind-loaded image is used instead of attempting a registry pull. Timeout: 45 minutes.

### tempest

Tempest API integration tests (CC-0035, CC-0050). Deploys services into a kind cluster and
runs the OpenStack Tempest test suite against them. Currently tests Keystone; designed so
additional services can be added to this single job.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen]`
**Condition:** Runs only when `has-e2e-operators == 'true'` and all gate jobs succeeded.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `helm/kind-action@v1.14.0` | Creates kind cluster (`forge-e2e`) |
| 4 | `make docker-build` | Builds operator image (keystone) |
| 5 | `hack/ci-build-service-image.sh` | Builds the OpenStack service image (CC-0050 REQ-002) |
| 6 | `hack/ci-build-tempest-image.sh` | Builds Tempest Docker image with pinned versions from `releases/` config (CC-0050 REQ-002) |
| 7 | `kind load docker-image` | Loads operator and service images into kind |
| 8 | `setup-e2e-infra` composite action | Installs Flux CLI, test deps, and deploys infra stack (CC-0050 REQ-005) |
| 9 | `hack/ci-deploy-operator.sh` | Installs CRDs and deploys operator via Helm (CC-0050 REQ-003) |
| 10 | Deploy Keystone CR | Applies `tests/tempest/keystone/00-keystone-cr.yaml` and waits for Ready |
| 11 | `hack/ci-run-tempest.sh` | Runs Tempest API tests via Docker with port-forwarding (CC-0050 REQ-004) |
| 12 | Upload Tempest results | Uploads subunit and JUnit results as artifact (14-day retention) |
| 13 | `hack/ci-dump-diagnostics.sh` (always) | Dumps diagnostic info with `OPERATOR=keystone` (CC-0050 REQ-001) |

Timeout: 45 minutes.

### build-and-push

Builds operator container images per platform on native runners and pushes each
single-arch image by digest (CC-0018 REQ-006). Runs only on push events (main branch or
v* tags) — skipped on pull requests. The multi-arch manifest list and final tags are
assembled by the subsequent `merge-operator-images` job.

**Dependencies:** `needs: [changes, e2e-operator]` (renamed from `e2e-keystone` in CC-0050)
**Condition:** `if: github.event_name == 'push' && needs.e2e-operator.result == 'success'`
**Permissions:** `contents: read`, `packages: write`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | Prepare platform pair | Shell | Converts `linux/amd64` → `linux-amd64` for artifact names and cache scopes |
| 3 | `docker/setup-buildx-action@v4` | Sets up Docker Buildx |
| 4 | `docker/login-action@v4` | Authenticates to GHCR (`github.actor` / `GITHUB_TOKEN`) |
| 5 | `docker/metadata-action@v6` | Generates OCI labels (two-layer annotation pattern) |
| 6 | `docker/build-push-action@v7` | Builds single-platform image; `push-by-digest=true`; digest exported as artifact |
| 7 | Export digest | Shell | Writes digest filename to `/tmp/digests/` |
| 8 | Upload digest | `actions/upload-artifact@v7` | Artifact name: `digests-operator-<operator>-<platform-pair>`, retention: 1 day |

**Matrix strategy:**

```yaml
strategy:
  fail-fast: false
  matrix:
    operator: ${{ fromJson(needs.changes.outputs.e2e-operators).operator }}
    platform: [linux/amd64, linux/arm64]
    include:
      - platform: linux/amd64
        runner: ubuntu-latest
      - platform: linux/arm64
        runner: ubuntu-24.04-arm
```

Build context is the repository root (required by `go.work`), with the Dockerfile at
`operators/<operator>/Dockerfile`. GitHub Actions cache (`type=gha`) is scoped per
platform (`<operator>-operator-linux-amd64` / `<operator>-operator-linux-arm64`).

### merge-operator-images

Downloads per-platform digests from `build-and-push`, assembles the multi-arch manifest
list, and pushes it with the final tags (CC-0018 REQ-006).

**Dependencies:** `needs: [changes, build-and-push]`
**Condition:** `if: github.event_name == 'push' && needs.build-and-push.result == 'success'`
**Permissions:** `contents: read`, `packages: write`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `docker/setup-buildx-action@v4` + `docker/login-action@v4` | Authenticates to GHCR |
| 3 | `docker/metadata-action@v6` | Generates final image tags |
| 4 | Download digests | `actions/download-artifact@v4` | Downloads all `digests-operator-<operator>-*` artifacts |
| 5 | Create and push manifest list | Shell | `docker buildx imagetools create` assembles per-platform digests under the final tags from step 3 |

**Matrix strategy:** Same `operator` dimension as `build-and-push` (via `fromJson(needs.changes.outputs.e2e-operators)`).

**Image tagging strategy:**

| Trigger | Tags Applied |
| --- | --- |
| Push to main | `sha-<full-sha>`, `latest` |
| Push v* tag (from main) | `sha-<full-sha>`, `latest`, `<version>` (e.g. `0.1.0`, v prefix stripped) |
| Push v* tag (from non-main) | `sha-<full-sha>`, `<version>` (no `latest` — restricted to default branch) |

Images are published at `ghcr.io/c5c3/<operator>-operator:<tag>`.

### helm-push

Packages and pushes operator Helm charts to the GHCR OCI registry (CC-0018 REQ-007).
Runs only on push events — skipped on pull requests.

**Dependencies:** `needs: [changes, e2e-operator]` (renamed from `e2e-keystone` in CC-0050)
**Condition:** `if: github.event_name == 'push' && needs.e2e-operator.result == 'success'`
**Permissions:** `contents: read`, `packages: write`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `azure/setup-helm@v4` | Installs Helm CLI |
| 3 | Helm registry login | Authenticates to GHCR via `helm registry login` |
| 4 | Package and push | Packages chart and pushes to `oci://ghcr.io/c5c3/charts/` |

**Chart version derivation:**

| Trigger | Version |
| --- | --- |
| Push to main | Default version from `Chart.yaml` |
| Push v* tag | SemVer derived from tag (v prefix stripped, e.g. `v0.1.0` → `0.1.0`) |

**Matrix strategy:**

```yaml
strategy:
  matrix:
    operator: [keystone]
```

The `make helm-package` target packages `operators/<operator>/helm/<operator>-operator/`.
When `CHART_VERSION` is set (for tag pushes), it overrides the version in `Chart.yaml`.

### github-release

Creates a GitHub Release with auto-generated release notes on v* tag pushes (CC-0018
REQ-008).

**Dependencies:** `needs: [changes, merge-operator-images, helm-push]`
**Condition:** `if: startsWith(github.ref, 'refs/tags/v') && needs.merge-operator-images.result == 'success' && needs.helm-push.result == 'success'`
**Permissions:** `contents: write`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `azure/setup-helm@v4` | Installs Helm CLI for chart packaging |
| 3 | Package Helm charts | Packages operator Helm charts with release version |
| 4 | `softprops/action-gh-release@v2` | Creates release with `generate_release_notes: true` and attaches chart tarballs |

This job runs only after both `merge-operator-images` and `helm-push` complete
successfully, ensuring the final multi-arch manifest list and charts are published before
the release is created. Helm chart tarballs
are attached as release assets for direct download. Timeout: 5 minutes.

## Reusable CI Scripts (CC-0050)

CC-0050 extracted repeated inline shell logic from E2E jobs into standalone scripts under
`hack/`. Each script uses `set -euo pipefail`, includes an SPDX Apache-2.0 header, and
passes shellcheck (REQ-007). All scripts are designed to work both in CI and locally
against any kubeconfig.

### hack/ci-dump-diagnostics.sh (REQ-001)

Dumps diagnostic information after E2E failures. Shared across `e2e-infra`, `e2e-operator`,
and `tempest` jobs.

| Environment Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `OPERATOR` | No | (empty) | When set, emits operator-specific diagnostics (pod logs, CR status, job logs) |
| `NAMESPACE` | No | `openstack` | Kubernetes namespace for operator-specific queries |

**Infrastructure diagnostics (always emitted):** HelmReleases, pods, DaemonSets, events
(last 50), and Flux logs across all namespaces.

**Operator diagnostics (when `OPERATOR` is set):** Operator pods and logs, job descriptions
and logs in the target namespace, all pod logs (current and previous) in the namespace,
operator CR status conditions, and ConfigMaps.

Usage:

```bash
hack/ci-dump-diagnostics.sh                    # infra-only diagnostics
OPERATOR=keystone hack/ci-dump-diagnostics.sh   # + operator-specific diagnostics
```

### hack/ci-build-service-image.sh (REQ-002)

Builds an OpenStack service container image by resolving upstream source refs, cloning the
project at the pinned ref, applying constraint overrides, and building the full image chain
(`python-base` -> `venv-builder` -> service image).

| Environment Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `OPERATOR` | Yes | - | OpenStack service name (e.g. `keystone`) |
| `IMAGE_PREFIX` | Yes | - | Container image prefix (e.g. `ghcr.io/c5c3`) |
| `RELEASE` | No | `2025.2` | Release directory name under `releases/` |

The script reads `releases/<RELEASE>/source-refs.yaml` for the upstream Git ref and
`releases/<RELEASE>/extra-packages.yaml` for additional pip/apt packages. The final image
is tagged `<IMAGE_PREFIX>/<OPERATOR>:<RELEASE>`.

Usage:

```bash
OPERATOR=keystone IMAGE_PREFIX=ghcr.io/c5c3 hack/ci-build-service-image.sh
```

### hack/ci-deploy-operator.sh (REQ-003)

Deploys an operator into a kind cluster by installing CRDs, waiting for establishment, and
deploying the operator via Helm with the specified container image.

| Environment Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `OPERATOR` | Yes | - | Operator name (e.g. `keystone`) |
| `IMAGE_REPO` | Yes | - | Full image repository (e.g. `ghcr.io/c5c3/keystone-operator`) |
| `IMAGE_TAG` | No | `dev` | Image tag |

The script runs `kubectl apply -f <chart>/crds/`, waits for CRD establishment, then runs
`helm install` with `image.pullPolicy=Never` (suitable for kind-loaded images).

Usage:

```bash
OPERATOR=keystone IMAGE_REPO=ghcr.io/c5c3/keystone-operator hack/ci-deploy-operator.sh
```

### hack/ci-build-tempest-image.sh (REQ-002)

Builds the Tempest test container image by resolving Tempest and plugin version refs from
the release config, then running `docker build` with the pinned versions.

| Environment Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `RELEASE` | No | `2025.2` | Release directory name under `releases/` |
| `TEMPEST_IMAGE` | No | `c5c3/tempest:local` | Target image name:tag |

The script reads `releases/<RELEASE>/test-refs.yaml` to resolve `tempest` and
`keystone-tempest-plugin` versions, then builds `images/tempest/Dockerfile` with the
appropriate build args and `upper-constraints` build context.

Usage:

```bash
hack/ci-build-tempest-image.sh
RELEASE=2025.2 TEMPEST_IMAGE=c5c3/tempest:local hack/ci-build-tempest-image.sh
```

### hack/ci-run-tempest.sh (REQ-004)

CI-specific Tempest execution wrapper that handles port-forwarding, config generation, and
Docker-based test execution. This is the CI counterpart to `hack/run-tempest.sh` (which
handles local execution including image building).

| Environment Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `SERVICE` | No | `keystone` | Service under test |
| `CONFIG_DIR` | No | `tests/tempest/<SERVICE>` | Directory containing `tempest.conf` template and include/exclude lists |
| `NAMESPACE` | No | `openstack` | Kubernetes namespace |
| `ADMIN_SECRET` | No | `keystone-admin` | Secret name holding admin password |
| `OUTPUT_DIR` | No | `_output/tempest` | Test output directory |
| `TEMPEST_IMAGE` | No | `c5c3/tempest:local` | Tempest container image |

The script:
1. Extracts the admin password from the Kubernetes secret
2. Sets up `kubectl port-forward` to the service and waits for readiness
3. Generates `tempest.conf` from the template, substituting endpoint and credentials
4. Runs Tempest in a Docker container with `--network host` and host-alias DNS entries
5. Converts subunit output to JUnit XML and checks for failures

Usage:

```bash
hack/ci-run-tempest.sh
SERVICE=keystone OUTPUT_DIR=_output/tempest hack/ci-run-tempest.sh
```

## Composite Action: setup-e2e-infra (CC-0050 REQ-005)

`.github/actions/setup-e2e-infra/action.yaml`

A composite GitHub Action that encapsulates the shared Flux CLI + test dependencies +
infrastructure deployment sequence used by `e2e-infra`, `e2e-operator`, and `tempest` jobs.
This replaces three duplicated step sequences with a single `uses:` reference.

**Prerequisite:** A kind cluster must already exist (the action sets `SKIP_KIND_CREATE=true`
internally).

| Step | Description |
| --- | --- |
| 1 | Installs Flux CLI via `fluxcd/flux2/action@v2.8.3` (SHA-pinned) |
| 2 | Runs `make install-test-deps` and adds `~/.local/bin` to `GITHUB_PATH` |
| 3 | Runs `make deploy-infra` with `SKIP_KIND_CREATE=true` |

Usage in a workflow job:

```yaml
- name: Setup E2E infrastructure
  uses: ./.github/actions/setup-e2e-infra
```

The action takes no inputs. All configuration is handled by existing Makefile targets and
environment variables.

## How the Pieces Fit Together

The E2E jobs follow a common pattern, with shared components extracted by CC-0050:

```
1. Checkout + Go setup + kind cluster creation     (workflow steps)
2. Build operator image                             (make docker-build)
3. Build service image                              (hack/ci-build-service-image.sh)
4. Load images into kind                            (workflow steps)
5. Deploy infrastructure                            (setup-e2e-infra composite action)
6. Deploy operator                                  (hack/ci-deploy-operator.sh)
7. Run tests                                        (chainsaw / hack/ci-run-tempest.sh)
8. Dump diagnostics                                 (hack/ci-dump-diagnostics.sh)
9. Upload artifacts                                 (workflow steps)
```

The `e2e-infra` job uses steps 1, 5, 7-9 (no operator or service images needed). The
`e2e-operator` job uses all steps. The `tempest` job uses all steps plus an additional
Tempest image build and Keystone CR deployment before running `hack/ci-run-tempest.sh`
instead of Chainsaw.

## Go Setup Convention

All Go-based jobs use `actions/setup-go@v6` with (CC-0003 REQ-005):

```yaml
go-version-file: go.work
```

This reads the Go version from `go.work` (currently Go 1.25.0) rather than hardcoding a
`go-version` value. The repository root contains `go.work` (not `go.mod`) because the
project uses a Go Workspace with multiple modules (`internal/common`, `operators/keystone`,
`operators/c5c3`). Module dependency caching is enabled by default in `actions/setup-go@v6`.

## Concurrency

The workflow uses a concurrency group scoped per-branch per-workflow (CC-0003 REQ-006):

```yaml
concurrency:
  group: ${{ github.ref }}-${{ github.workflow }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
```

For pull requests, pushing new commits cancels any in-progress CI run for that same PR
branch, preventing wasted CI resources on outdated code. For pushes to `main`, in-progress
runs are **not** cancelled, ensuring every merge commit is fully validated. Different
branches do not cancel each other's runs.

## Action Pinning

All GitHub Actions are referenced by full SHA hash with a trailing version comment
(CC-0003 REQ-001):

```yaml
- uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd # v6
```

This prevents supply chain attacks via mutable tag retargeting and provides audit
traceability. The version comment preserves human readability.

## SPDX Header

The file starts with the standard SPDX license header (CC-0003 REQ-001):

```text
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
---
```

## Codecov Configuration

`.codecov.yml` defines coverage status checks and component-level thresholds (CC-0018
REQ-012).

### Status Checks

| Check | Target | Description |
| --- | --- | --- |
| Project | `auto` (threshold: 1%) | Overall coverage must not decrease by more than 1% |
| Patch | `90%` | New/changed lines in a PR must meet 90% coverage |

`fail_ci_if_error: false` is set on each `codecov/codecov-action` step in the workflow
(not in `.codecov.yml`, where it is not a valid key) because fork PRs do not have access
to `CODECOV_TOKEN`. This prevents CI from failing due to upload issues on forks.

### Flag Management

The `flag_management` section in `.codecov.yml` links CI-uploaded flags to coverage tracking
rules. Flags follow the `[unit|integration]-<target>` naming convention, matching the CI
matrix targets (`common`, `keystone`, `c5c3`). Each flag has `carryforward: true`, which
ensures that when only a subset of flags is uploaded (e.g., only one operator changed), the
missing flags carry forward their last-known coverage instead of reducing the total.

Defined flags:

| Flag | Paths | Source |
| --- | --- | --- |
| `unit-common` | `internal/common/` | `test` job, `common` matrix leg |
| `unit-keystone` | `operators/keystone/` | `test` job, `keystone` matrix leg |
| `unit-c5c3` | `operators/c5c3/` | `test` job, `c5c3` matrix leg |
| `integration-common` | `internal/common/` | `test-integration` job, `common` matrix leg |
| `integration-keystone` | `operators/keystone/` | `test-integration` job, `keystone` matrix leg |
| `integration-c5c3` | `operators/c5c3/` | `test-integration` job, `c5c3` matrix leg |

### Component Thresholds

Each component is tracked independently on the Codecov dashboard:

| Component | Paths | Target | Rationale |
| --- | --- | --- | --- |
| `common` | `internal/common/**` | 80% | Shared library code underpinning all operators |
| `controllers` | `operators/*/internal/controller/**` | 70% | Controller reconciliation logic (envtest-dependent paths harder to cover) |
| `webhooks` | `operators/*/api/**` | 90% | Webhook validation/defaulting (incorrect admission logic causes silent data corruption) |

## Makefile Targets

The CI workflow depends on several Makefile targets (CC-0018):

### docker-build (REQ-010)

Builds the operator Docker image from `operators/<operator>/Dockerfile` with the
repository root as build context (required by `go.work`).

```
make docker-build OPERATOR=keystone [IMG=custom:tag]
```

The `IMG` variable controls the image tag, defaulting to
`ghcr.io/c5c3/<operator>-operator:latest`. The `OPERATOR` variable is required.

### helm-package (REQ-011)

Packages the operator Helm chart from
`operators/<operator>/helm/<operator>-operator/`.

```
make helm-package OPERATOR=keystone [CHART_VERSION=1.2.3]
```

When `CHART_VERSION` is set, it overrides the version in the chart's `Chart.yaml`. The
packaged `.tgz` is output to the current directory. The `OPERATOR` variable is required.

### test-common (CC-0018)

Runs unit tests for `internal/common` only, producing a single coverage profile.

```
make test-common
```

Produces `cover-unit-common.out`. Used by the `common` matrix leg in the `test` CI job to
deduplicate common coverage into a single upload.

### test-operator (CC-0018)

Runs unit tests for a single operator without `internal/common`.

```
make test-operator OPERATOR=keystone
```

Produces `cover-unit-<operator>.out`. Used by operator matrix legs in the `test` CI job.
The `OPERATOR` variable is required.

### test-integration (REQ-003)

Runs envtest-based integration tests (tagged with `//go:build integration`) for operators.
Requires `setup-envtest` to be installed.

```
make test-integration [OPERATOR=keystone]
```

Sets `KUBEBUILDER_ASSETS` via `setup-envtest use <pinned-k8s-version> -p path`, then runs
`go test -tags=integration` for each operator module. Produces
`cover-integration-<operator>.out` files. Without `OPERATOR`, runs for all operators in
the `OPERATORS` list.

### test-integration-common (CC-0018)

Runs envtest-based integration tests for `internal/common` only.

```
make test-integration-common
```

Sets `KUBEBUILDER_ASSETS` via `setup-envtest use <pinned-k8s-version> -p path`, then runs
`go test -tags=integration ./internal/common/...`. Produces `cover-integration-common.out`.
Used by the `common` matrix leg in CI to meet the 80% codecov target for `internal/common/`.

## Dependencies on Prior Features

The CI workflow depends on artifacts introduced by CC-0001, CC-0010, and CC-0050:

| Artifact | Used by | Purpose |
| --- | --- | --- |
| `Makefile` (`lint` target) | `lint` job | Iterates over `OPERATORS` variable to run golangci-lint per module |
| `Makefile` (`test-common` target) | `test` job (`common` leg) | Runs unit tests for `internal/common` with coverage profile |
| `Makefile` (`test-operator` target) | `test` job (operator legs) | Runs unit tests for a single operator with coverage profile |
| `Makefile` (`test-integration` target) | `test-integration` job (operator legs) | Runs envtest integration tests per operator with coverage profiles |
| `Makefile` (`test-integration-common` target) | `test-integration` job (`common` leg) | Runs envtest integration tests for `internal/common` with coverage profile |
| `Makefile` (`docker-build` target) | `e2e-operator`, `tempest`, `build-and-push` jobs | Builds operator Docker images |
| `Makefile` (`helm-package` target) | `helm-push` job | Packages operator Helm charts |
| `.golangci.yml` | `lint` job | Provides linter configuration (enabled linters, exclusion rules, timeout) |
| `go.work` | All Go-based jobs | Provides the Go version for `actions/setup-go@v6` |
| `hack/*.sh` | `shellcheck` job | Shell scripts validated by shellcheck |
| `.codecov.yml` | Codecov integration | Component-level coverage thresholds |
| `hack/ci-dump-diagnostics.sh` | `e2e-infra`, `e2e-operator`, `tempest` jobs | Shared diagnostic dump (CC-0050) |
| `hack/ci-build-service-image.sh` | `e2e-operator`, `tempest` jobs | Builds OpenStack service images (CC-0050) |
| `hack/ci-deploy-operator.sh` | `e2e-operator`, `tempest` jobs | Deploys operator via Helm (CC-0050) |
| `hack/ci-run-tempest.sh` | `tempest` job | Runs Tempest API tests (CC-0050) |
| `.github/actions/setup-e2e-infra/` | `e2e-infra`, `e2e-operator`, `tempest` jobs | Composite action for infra setup (CC-0050) |

:::
