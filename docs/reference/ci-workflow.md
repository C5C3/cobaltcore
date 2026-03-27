---
title: CI Workflow
quadrant: infrastructure
feature: CC-0003, CC-0018
---

::: v-pre

# CI Workflow

Reference documentation for the GitHub Actions CI workflow (CC-0003, CC-0018).

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

`REGISTRY` and `IMAGE_PREFIX` are referenced by the `build-and-push`, `helm-push`, and
`e2e-keystone` jobs to construct image names and registry URLs. `CONTROLLER_GEN_VERSION`
is used by `verify-codegen` to pin controller-gen to a specific version. `setup-envtest`
is installed via `@latest` because the sub-module does not publish its own release tags.

## Permissions

Top-level permissions are restricted to least privilege (CC-0003 REQ-007):

```yaml
permissions:
  contents: read
```

Jobs that need elevated access declare per-job `permissions:` blocks:

| Job | Additional Permissions | Reason |
| --- | --- | --- |
| `build-and-push` | `packages: write` | Push container images to GHCR |
| `helm-push` | `packages: write` | Push Helm charts to GHCR OCI registry |
| `github-release` | `contents: write` | Create GitHub Releases |

## Job Dependency DAG

The workflow defines 11 jobs organised in a directed acyclic graph (CC-0018):

```
Gate Jobs (always run):
  lint ─────────────┐
  shellcheck ───────┤
  verify-codegen ───┤
  test (matrix) ────┼──> E2E Jobs + Publish Jobs
  test-integration ─┘

E2E Jobs (depends on gates):
  e2e-infra (independent — no gate dependency)
  e2e-keystone ──> needs: [lint, shellcheck, test, test-integration, verify-codegen]

Publish Jobs (main/tags only, depends on E2E):
  build-and-push ──> needs: [e2e-keystone], if: push event
  helm-push ────────> needs: [e2e-keystone], if: push event

Release Job (v* tags only, depends on publish):
  github-release ──> needs: [build-and-push, helm-push], if: v* tag

Independent:
  docs (no dependencies, no gates)
```

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
| 3 | `go install setup-envtest@latest` | Installs envtest asset downloader (sub-module has no release tags) |
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

Builds the VitePress documentation site to catch broken links and build errors.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Full history (`fetch-depth: 0`) for git-based features |
| 2 | `actions/setup-node@v6` | Node.js 24, npm cache enabled |
| 3 | `npm ci` | Installs dependencies from lockfile |
| 4 | `npm run docs:build` | Builds the documentation site |

This job runs independently with no gate dependencies.

### e2e-infra

End-to-end infrastructure deployment and Chainsaw test (CC-0010). Deploys the full
infrastructure stack (Flux, cert-manager, MariaDB, ESO, OpenBao) to a kind cluster and
validates health of all operators, CRs, and ExternalSecrets.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `helm/kind-action@v1.14.0` | Creates kind cluster (`forge-e2e`) |
| 4 | `fluxcd/flux2/action@v2.8.3` | Installs Flux CLI |
| 5 | `make install-test-deps` | Installs Chainsaw and other test dependencies |
| 6 | `make deploy-infra` | Deploys infrastructure stack (`SKIP_KIND_CREATE=true`) |
| 7 | `chainsaw test` | Runs E2E tests from `tests/e2e/infrastructure/` |
| 8 | Diagnostic dump (on failure) | Dumps HelmReleases, pods, events, Flux logs |
| 9 | Upload JUnit report | Uploads test results as artifact (14-day retention) |

This job runs independently with no gate dependencies. Timeout: 20 minutes.

### e2e-keystone

End-to-end operator test using kind cluster and Chainsaw (CC-0018 REQ-005). Builds the
operator image locally, loads it into kind, deploys via Helm, and runs Chainsaw E2E test
suites.

**Dependencies:** `needs: [lint, shellcheck, test, test-integration, verify-codegen]`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `helm/kind-action@v1.14.0` | Creates kind cluster (`forge-e2e`) |
| 4 | `make docker-build` | Builds operator image with tag `<IMAGE_PREFIX>/<operator>-operator:dev` |
| 5 | `kind load docker-image` | Loads the built image into the kind cluster |
| 6 | `make install-test-deps` | Installs Chainsaw and other test dependencies |
| 7 | `helm install` | Deploys operator from local chart with `image.tag=dev`, `image.pullPolicy=Never` |
| 8 | `chainsaw test` | Runs E2E tests from `tests/e2e/<operator>/` |
| 9 | Diagnostic dump (on failure) | Dumps operator pods, all pods, events, operator logs |
| 10 | Upload JUnit report | Uploads test results as artifact (14-day retention) |

**Matrix strategy:**

```yaml
strategy:
  matrix:
    operator: [keystone]
```

The `imagePullPolicy: Never` Helm value ensures the kind-loaded image is used instead of
attempting a registry pull. Timeout: 30 minutes (accounts for kind cluster creation, image
build, Helm deploy, and Chainsaw test execution).

### build-and-push

Builds and pushes operator container images to GHCR (CC-0018 REQ-006). Runs only on push
events (main branch or v* tags) — skipped on pull requests.

**Dependencies:** `needs: [e2e-keystone]`
**Condition:** `if: github.event_name == 'push'`
**Permissions:** `contents: read`, `packages: write`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `docker/setup-buildx-action@v4` | Sets up Docker Buildx for multi-platform builds |
| 3 | `docker/login-action@v4` | Authenticates to GHCR (`github.actor` / `GITHUB_TOKEN`) |
| 4 | `docker/metadata-action@v6` | Generates OCI labels and image tags (two-layer annotation pattern) |
| 5 | `docker/build-push-action@v7` | Builds and pushes image with GHA cache, OCI labels |

**Image tagging strategy:**

| Trigger | Tags Applied |
| --- | --- |
| Push to main | `sha-<full-sha>`, `latest` |
| Push v* tag (from main) | `sha-<full-sha>`, `latest`, `<version>` (e.g. `0.1.0`, v prefix stripped) |
| Push v* tag (from non-main) | `sha-<full-sha>`, `<version>` (no `latest` — restricted to default branch) |

**Matrix strategy:**

```yaml
strategy:
  matrix:
    operator: [keystone]
```

Images are pushed to `ghcr.io/c5c3/<operator>-operator:<tag>`. Build context is the
repository root (required by `go.work`), with the Dockerfile at
`operators/<operator>/Dockerfile`. GitHub Actions cache (`type=gha`) is used for layer
caching.

### helm-push

Packages and pushes operator Helm charts to the GHCR OCI registry (CC-0018 REQ-007).
Runs only on push events — skipped on pull requests.

**Dependencies:** `needs: [e2e-keystone]`
**Condition:** `if: github.event_name == 'push'`
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

**Dependencies:** `needs: [build-and-push, helm-push]`
**Condition:** `if: startsWith(github.ref, 'refs/tags/v')`
**Permissions:** `contents: write`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v6` | Checks out the repository (SHA-pinned) |
| 2 | `azure/setup-helm@v4` | Installs Helm CLI for chart packaging |
| 3 | Package Helm charts | Packages operator Helm charts with release version |
| 4 | `softprops/action-gh-release@v2` | Creates release with `generate_release_notes: true` and attaches chart tarballs |

This job runs only after both `build-and-push` and `helm-push` complete successfully,
ensuring all artifacts are published before the release is created. Helm chart tarballs
are attached as release assets for direct download. Timeout: 5 minutes.

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

The CI workflow depends on artifacts introduced by CC-0001 and CC-0010:

| Artifact | Used by | Purpose |
| --- | --- | --- |
| `Makefile` (`lint` target) | `lint` job | Iterates over `OPERATORS` variable to run golangci-lint per module |
| `Makefile` (`test-common` target) | `test` job (`common` leg) | Runs unit tests for `internal/common` with coverage profile |
| `Makefile` (`test-operator` target) | `test` job (operator legs) | Runs unit tests for a single operator with coverage profile |
| `Makefile` (`test-integration` target) | `test-integration` job (operator legs) | Runs envtest integration tests per operator with coverage profiles |
| `Makefile` (`test-integration-common` target) | `test-integration` job (`common` leg) | Runs envtest integration tests for `internal/common` with coverage profile |
| `Makefile` (`docker-build` target) | `e2e-keystone`, `build-and-push` jobs | Builds operator Docker images |
| `Makefile` (`helm-package` target) | `helm-push` job | Packages operator Helm charts |
| `.golangci.yml` | `lint` job | Provides linter configuration (enabled linters, exclusion rules, timeout) |
| `go.work` | All Go-based jobs | Provides the Go version for `actions/setup-go@v6` |
| `hack/*.sh` | `shellcheck` job | Shell scripts validated by shellcheck |
| `.codecov.yml` | Codecov integration | Component-level coverage thresholds |

:::
