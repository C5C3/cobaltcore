---
title: CI Workflow
quadrant: infrastructure
---

::: v-pre

# CI Workflow

Reference documentation for the GitHub Actions CI workflow.

Repeated E2E logic is factored into reusable shell scripts (`hack/ci-*.sh`) and a
composite GitHub Action (`.github/actions/setup-e2e-infra/`), reducing duplication across
the `e2e-infra`, `e2e-operator`, and `tempest` jobs.

The `build-e2e-images` job centralises E2E image builds: it builds all Docker images
(operator, service, tempest) once and pushes them to GHCR with run-scoped tags
(`e2e-${run_id}-<orig_tag>`). The `e2e-operator`, `e2e-chaos`, and `tempest` jobs
`docker pull` from GHCR via the `load-e2e-images` composite action and re-tag the
images to their canonical local references, saving ~5-10 min per CI run versus
rebuilding. The build always includes `keystone` (required by tempest) regardless of
which operator triggered the pipeline. The `cleanup-e2e-tags` job prunes the
run-scoped tags at the end of the workflow, with a nightly safety net in
`cleanup-images.yaml` for cancelled runs (GH-310).

## File Location

`.github/workflows/ci.yaml`

The file uses the `.yaml` extension (matching `reuse.yaml` and `deploy-docs.yaml`) and
quotes the trigger key as `"on"` to prevent YAML boolean interpretation.

## Trigger Events

The workflow triggers on three event types:

| Event | Scope | Description |
| --- | --- | --- |
| `push` | `branches: [main]` | Runs on every push to the main branch |
| `push` | `tags: ["v*"]` | Runs on every v-prefixed tag push (triggers publish and release jobs) |
| `pull_request` | `branches: [main]`, `types: [opened, synchronize, reopened, labeled]` | Runs on every pull request targeting main; includes `labeled` type to support on-demand chaos via `run-chaos` label |

Gate, test, and E2E jobs run **only on `pull_request` events** — every one of them
carries a `github.event_name == 'pull_request'` guard. Pushes to `main` and tag pushes
(`v*`) run only the publish and release jobs (`build-and-push`,
`merge-operator-images`, `helm-push`, `github-release`): the merged commit's PR was
already green, so the E2E suite is not re-run on push
("publish-only-on-merge"). On tag pushes the `changes` job forces all areas and all
operators active, so every operator's images and charts are published regardless of
which files the tagged commit touched.

## Environment Variables

Top-level environment variables centralise registry configuration and pin tool versions
for CI reproducibility:

```yaml
env:
  REGISTRY: ghcr.io
  IMAGE_PREFIX: ghcr.io/c5c3
  KIND_CLUSTER: forge
  CONTROLLER_GEN_VERSION: v0.21.0
  GOFUMPT_VERSION: v0.10.0
  GOLANGCI_LINT_VERSION: v2.12.2
```

`REGISTRY` and `IMAGE_PREFIX` are referenced by the `build-and-push`, `helm-push`,
`e2e-operator`, and `tempest` jobs to construct image names and registry URLs.
`KIND_CLUSTER` is the single source of truth for every E2E job's kind cluster name
(mirroring the `CLUSTER_NAME` default in `hack/deploy-infra.sh`).
`CONTROLLER_GEN_VERSION` is used by `verify-codegen` to pin controller-gen to a specific
version. `GOFUMPT_VERSION` is used by `format-check` to pin gofumpt to a specific version; the same version is mirrored in the Makefile (`GOFUMPT_VERSION ?= v0.10.0`) so
that `make fmt` and `make format-check` use a consistent version locally.
`GOLANGCI_LINT_VERSION` pins the golangci-lint binary installed by the `lint` job and
keys its analysis cache. `setup-envtest` is installed via `@release-0.23` because the
sub-module does not publish its own release tags.

## Permissions

Top-level permissions are restricted to least privilege:

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

The workflow defines 27 jobs organised in a directed acyclic graph. Every gate, test,
and E2E job additionally carries a `github.event_name == 'pull_request'` guard; the
publish and release jobs run only on push events:

```
Gate Jobs (pull requests only):
  lint ────────────────────────┐   (go == 'true')
  format-check                 │   (go == 'true')
  shellcheck ──────────────────┤   (always on PRs)
  feature-ids                  │   (always on PRs)
  test-shell                   │   (always on PRs)
  verify-codegen ──────────────┤   (go == 'true')
  verify-invalid-cr-fixtures ──┤   (always on PRs)
  chainsaw-lint ───────────────┤   (always on PRs)
  test (matrix) ───────────────┼──> build-e2e-images ──> E2E Jobs
  test-integration ────────────┘

Conditional Jobs (pull requests only, path-filtered via changes job):
  test-race ────> needs: [changes], if: needs.changes.outputs.go == 'true'
  govulncheck ─> needs: [changes], if: needs.changes.outputs.go == 'true'
  helm-validate ──> needs: [changes], if: needs.changes.outputs.helm == 'true'
  docs ──────────> needs: [changes], if: needs.changes.outputs.docs == 'true'

Image Build (pull requests only, depends on gates):
  build-e2e-images ──> needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, verify-invalid-cr-fixtures, chainsaw-lint]

E2E Jobs (pull requests only, depend on build-e2e-images):
  e2e-infra ──────> needs: [changes], if: needs.changes.outputs.e2e-infra == 'true'
  e2e-operator ───> needs: [changes, build-e2e-images]
  e2e-operator-upgrade > needs: [changes, build-e2e-images], if: needs.changes.outputs.has-e2e-operators == 'true'
  e2e-chaos ──────> needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images, e2e-operator]
  e2e-prometheus ─> needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]
                     if: needs.changes.outputs.e2e-prometheus == 'true'
  e2e-controlplane > needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]
                     if: needs.changes.outputs.e2e-controlplane == 'true'
  e2e-controlplane-sso > needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]
                     if: needs.changes.outputs.e2e-controlplane == 'true'
  e2e-external-keystone > needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]
                     if: needs.changes.outputs.e2e-controlplane == 'true'
  tempest ────────> needs: [changes, build-e2e-images, e2e-infra, e2e-operator, e2e-chaos, e2e-prometheus]
  cleanup-e2e-tags > needs: [build-e2e-images, e2e-operator, e2e-operator-upgrade, e2e-chaos, tempest]

Publish Jobs (push events only — main and v* tags; publish-only-on-merge):
  build-and-push (matrix: operator × platform) ──> needs: [changes], if: push && has-e2e-operators == 'true'
    └──> merge-operator-images ──> needs: [changes, build-and-push], if: push event
  helm-push ──> needs: [changes], if: push && has-e2e-operators == 'true'

Release Job (v* tags only, depends on publish):
  github-release ──> needs: [changes, merge-operator-images, helm-push], if: v* tag
```

The E2E jobs (`e2e-infra`, `e2e-operator`, `e2e-operator-upgrade`, `e2e-chaos`,
`e2e-prometheus`, `e2e-controlplane`, `e2e-external-keystone`, `tempest`) share
infrastructure setup via
the `setup-e2e-infra` composite action and diagnostic teardown via
`hack/ci-dump-diagnostics.sh`. They run on `blacksmith-4vcpu-ubuntu-2404` runners
(as does `test-integration`), except the `e2e-chaos` network suite, which uses a
GitHub-hosted `ubuntu-24.04` runner for its kernel-module requirements.

## Jobs

### lint

Runs golangci-lint using the project's `.golangci.yml` configuration.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `actions/cache@v5` | Persists the golangci-lint analysis cache, keyed on the Go and golangci-lint versions |
| 4 | `golangci/golangci-lint-action@v9` | Installs golangci-lint binary (`install-only: true`); version pinned via `GOLANGCI_LINT_VERSION` (`v2.12.2`) |
| 5 | `make lint` | Runs golangci-lint per module via the Makefile |

The `golangci-lint-action@v9` step is used with `install-only: true`, which installs the
pinned golangci-lint binary (and caches it) without running lint. The actual linting is
delegated to `make lint`, which `cd`s into each module directory and runs
`golangci-lint run ./...` — a necessary pattern for Go multi-module workspaces. The
`actions/setup-go@v6` step is required because `install-only` mode does not set up Go
internally.

**Enabled linters** (12 total, configured in `.golangci.yml`):

| Linter | Category | Description |
| --- | --- | --- |
| `errcheck` | correctness | Checks for unchecked errors in Go code |
| `gocritic` | style | Provides diagnostics for bugs, performance, and style issues |
| `govet` | correctness | Reports suspicious constructs, roughly equivalent to `go vet` |
| `ineffassign` | correctness | Detects assignments to existing variables that are never used |
| `staticcheck` | correctness | Comprehensive static analysis rules from the staticcheck suite |
| `unused` | correctness | Checks for unused constants, variables, functions, and types |
| `bodyclose` | resource-leak | Checks whether HTTP response bodies are closed successfully |
| `errorlint` | correctness | Validates Go 1.13+ error wrapping patterns (`%w`, `errors.Is`, `errors.As`) |
| `exhaustive` | correctness | Checks exhaustiveness of enum switch statements |
| `gosec` | security | Inspects source code for security problems (hardcoded credentials, weak crypto, unsafe operations) |
| `nilerr` | correctness | Finds code that returns nil even after checking that an error is not nil |
| `noctx` | correctness | Detects HTTP requests and TLS dials missing `context.Context` propagation |

Generated code matching `zz_generated.*.go` is excluded from all lint checks via the
`exclusions.paths` configuration.

### format-check

Verifies all Go files conform to gofumpt formatting. gofumpt is a strict superset
of gofmt — it applies all standard gofmt rules plus additional formatting conventions for
consistency. Detects non-conforming files and prints a unified diff showing the required
changes, so developers can identify and fix formatting issues without guessing.

Only git-tracked Go files are checked (`git ls-files '*.go'`) to avoid unexpected failures
on generated, vendored, or tooling code that may not follow gofumpt conventions.

The same version and check logic are available locally via the Makefile: `make install-gofumpt`
installs the pinned version, `make format-check` mirrors the CI check, and `make fmt` applies
formatting to all tracked Go files. The Makefile targets use `xargs` without the `-r` flag
(unlike CI) for macOS portability — BSD `xargs` does not support `-r`. This is safe because
the repository always contains tracked `.go` files.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'pull_request' && needs.changes.outputs.go == 'true'`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `go install mvdan.cc/gofumpt@${{ env.GOFUMPT_VERSION }}` | Installs gofumpt at the pinned version (`v0.10.0`) |
| 4 | `git ls-files '*.go' \| xargs -r gofumpt -l` | Lists non-conforming tracked Go files; on failure, prints unified diff and exits 1 |

The check uses `git ls-files '*.go' | xargs -r gofumpt -l` to collect non-conforming files
from tracked sources only. If any are found, their paths are printed along with a unified
diff (`gofumpt -d`), and the job exits 1. The `-r` flag prevents `xargs` from running
`gofumpt` when no Go files are piped (GNU coreutils, available on `ubuntu-latest`).

Timeout: 5 minutes.

### shellcheck

Validates shell scripts with shellcheck to catch scripting issues early.
The shellcheck binary is pre-installed on `ubuntu-latest` runners.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `make shellcheck` | Runs `shellcheck --severity=warning` over `hack/*.sh` and the operator rotation scripts (`operators/*/internal/controller/scripts/*.sh`) |

Timeout: 5 minutes.

### feature-ids

Verifies the whole tracked tree (code, tests, CI, scripts, docs) is free of internal
feature/requirement IDs. Runs unconditionally on every pull request — not
path-filtered — so a stray ID added anywhere is caught. This job folds in the former
docs-only check.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `make check-feature-ids` | Greps the tracked tree for internal feature/requirement ID patterns and fails on any hit |

Timeout: 5 minutes.

### verify-invalid-cr-fixtures

Enforces the canonical-scaffold contract for the invalid-CR Chainsaw fixtures.
Runs `_generate.py --check` (drift mode) and the `test_generate.py` unit suite
(FIXTURES count + `chainsaw-test.yaml` cross-reference) so a hand-edit to any
`02-…/03-…/…/12-*.yaml` fixture, or a rename or removal that desynchronises FIXTURES
from `chainsaw-test.yaml`, fails the build before the heavy cluster-bound `e2e-operator`
job runs. Always-on because the check is sub-second and `python3` is preinstalled on
`ubuntu-latest` runners.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `make verify-invalid-cr-fixtures` | Runs `_generate.py --check` and `test_generate.py` |

Timeout: 5 minutes.

### chainsaw-lint

Schema-lints every Chainsaw test (`tests/**/chainsaw-test.yaml`) and configuration
(`tests/{e2e,e2e-chaos}/chainsaw-config.yaml`) via `chainsaw lint` so typos, removed
fields, or schema drift after a chainsaw version bump fail fast — before the
cluster-bound `e2e-operator` and `e2e-chaos` jobs spin up a kind cluster. Always-on
because no cluster is needed: chainsaw is restored from the shared testdeps cache via
the `setup-test-deps` composite action, the same one consumed internally by
`setup-e2e-infra`. A schema break therefore surfaces in `needs.*.result` for both
`build-e2e-images` and `e2e-chaos`.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `./.github/actions/setup-test-deps` | Restores the testdeps cache and runs `make install-test-deps` (puts `chainsaw` on `PATH`) |
| 3 | `make chainsaw-lint` | Runs `chainsaw lint test -f` and `chainsaw lint configuration -f` over every matching file under `tests/` |

Timeout: 5 minutes.

### test-shell

Runs every shell unit test under `tests/unit/` (hack/, deploy/, docs/,
renovate/). Tests read repo files only — no cluster, no untrusted input —
so the job is unconditional and finishes in well under a minute on a cold
runner. Tests that depend on `yq` or `kustomize` are written to skip
gracefully when those tools are missing; the job installs `kustomize`
explicitly so the deploy/ overlay assertions run their full check set
(`yq` is preinstalled on ubuntu-latest).

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | Install kustomize | Downloads the pinned kustomize binary into `/usr/local/bin` |
| 3 | `make test-shell` | Iterates every `tests/unit/**/*_test.sh` and aggregates exit status |

Timeout: 5 minutes.

### test

Runs unit tests with a matrix strategy over `[common, keystone, c5c3]`.
Each matrix leg tests a single target — either `internal/common` or one operator — producing
a single coverage profile uploaded to Codecov under a dedicated flag.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
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
and coverage uploaded to Codecov. Requires `setup-envtest` to
download kubebuilder assets (kube-apiserver, etcd) for the test API server.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
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

### test-race

Runs all Go unit tests with the race detector enabled to catch data races in concurrent
operator code — reconcilers, watches, informer caches. Separate from the main
`test` job because the race detector adds 2–5x overhead. Uses `-count=1` to disable test
caching, since race conditions are non-deterministic and cached results could mask real
races.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'pull_request' && needs.changes.outputs.go == 'true'`
**Path filter:** Go source files (same filter as `test` and `test-integration`)

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `make test-race RACE_FLAGS="-count=1"` | Delegates to the Makefile so the module list stays in sync |

CI delegates to `make test-race` so the list of modules under race testing is defined in one
place (the Makefile's `OPERATORS` variable and `internal/common`). `RACE_FLAGS="-count=1"`
disables test caching — race conditions are non-deterministic, so cached results could mask
real races. No `continue-on-error` or `if: always()` — a detected data race fails the job
immediately.

This job runs independently and does **not** appear in any other job's `needs:` array. It is
not on the critical path for E2E or publish jobs, so race detector overhead does not slow
down the primary feedback loop. The corresponding local command is `make test-race`
(which omits `-count=1` via the default empty `RACE_FLAGS` for developer convenience).

Timeout: 20 minutes (accommodates 2–5x race detector overhead).

### govulncheck

Scans all Go modules for reachable vulnerabilities using govulncheck, the official Go
vulnerability scanner maintained by the Go team. Unlike dependency-list scanners,
govulncheck analyses call graphs to detect only vulnerabilities in code paths that are
actually reachable — reducing false positives. Catches supply-chain vulnerabilities at the
PR stage, before container images are built.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'pull_request' && needs.changes.outputs.go == 'true'`
**Path filter:** Go source files (same filter as `test`, `test-integration`, and `test-race`)

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `go install golang.org/x/vuln/cmd/govulncheck@latest` | Installs the latest govulncheck binary |
| 4 | `make govulncheck` | Delegates to `hack/ci-govulncheck.sh`, which scans `internal/common` and all `$(OPERATORS)` modules with an explicit allowlist |

govulncheck uses `@latest` intentionally — unlike other pinned tools (controller-gen,
gofumpt), pinning govulncheck to an old version defeats the purpose of vulnerability
scanning because the vulnerability database is updated frequently. This is a deliberate
deviation from the general pinning policy, justified by the security tool's nature.

The CI step delegates to `make govulncheck`, which runs `hack/ci-govulncheck.sh` over
`internal/common` and each operator in the `$(OPERATORS)` Makefile variable. govulncheck
has no native suppression flag, so the wrapper runs it in JSON mode per module, keeps
only the *reachable* symbol-level findings (the ones that fail the default text report),
and drops any whose advisory ID appears in the script's `ALLOWLIST` map. The build fails
if, and only if, a reachable finding survives the allowlist — matching govulncheck's
normal failure semantics while letting the project ride out advisories that have no fix
and no real exposure. Every allowlist entry carries a one-line justification, and if an
allowlisted advisory is no longer reported, the wrapper prints a notice so the stale
entry can be removed. Dependencies with known CVEs whose vulnerable functions are not
called in project code are reported as informational but do not fail the job.

This job runs independently and does **not** appear in any other job's `needs:` array. It
is not on the critical path for E2E or publish jobs, matching the `test-race` pattern.
When a new Go module is added to `go.work`, the `OPERATORS` variable in the Makefile must
be updated with the new module name. The verification test
(`tests/ci/verify_govulncheck_modules.sh`) catches drift between `go.work` and the
Makefile automatically.

Timeout: 10 minutes.

### verify-codegen

Verifies that generated code (CRD manifests, deepcopy functions) is committed and
up-to-date. This is a gate job — it blocks merge alongside `lint`,
`test`, and `shellcheck`.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `go install controller-gen@${{ env.CONTROLLER_GEN_VERSION }}` | Installs the pinned code generator |
| 4 | `make manifests && make generate` | Regenerates CRD manifests and deepcopy functions |
| 5 | `make verify-crd-sync` | Verifies Helm chart CRD copies match controller-gen output |
| 6 | `git diff --exit-code` | Fails if any files changed (stale generated code) |

When the diff check fails, the job produces a GitHub Actions `::error::` annotation with
instructions to run `make manifests && make generate` locally and commit the result.

### docs

Builds the VitePress documentation site to catch broken links and build errors.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'pull_request' && needs.changes.outputs.docs == 'true'`
**Path filter:** `docs/**`, `package.json`, `package-lock.json`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Full history (`fetch-depth: 0`) for git-based features |
| 2 | `actions/setup-node@v6` | Node.js 24, npm cache enabled |
| 3 | `npm ci` | Installs dependencies from lockfile |
| 4 | `npm run docs:build` | Builds the documentation site |

### helm-validate

Validates Helm chart structure, template rendering, and unit tests for both
operator charts without requiring a cluster. Verifies the generated
`values.schema.json` is in sync with its shared source, vendors the shared
`operator-library` subchart, then runs `helm lint`, `helm template` with five
value override scenarios, and `helm unittest` for each chart to catch
regressions at PR time.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'pull_request' && needs.changes.outputs.helm == 'true'`
**Path filter:** `operators/keystone/helm/**`, `operators/c5c3/helm/**`, `operators/shared/helm/**`, `hack/gen-helm-values-schema.py`, `Makefile` (forced `true` on `v*` tag pushes)

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `azure/setup-helm@v5` | Installs Helm CLI (SHA-pinned) |
| 3 | `helm plugin install helm-unittest` | Installs helm-unittest plugin (pinned to `v1.0.3`) |
| 4 | `make verify-helm-schema` | Fails if either chart's `values.schema.json` has drifted from the shared generator |
| 5 | `make helm-deps` | Vendors the `operator-library` subchart into each chart's `charts/` |
| 6 | `helm lint` | Validates chart structure and syntax for both operator charts |
| 7 | `helm template` (5 scenarios) | Renders each chart with value overrides to catch broken conditionals and invalid YAML |
| 8 | `helm unittest` | Runs the unit test suites under each chart's `tests/` directory |

**Template scenarios (step 7), run against each operator chart:**

| Scenario | Values | Purpose |
| --- | --- | --- |
| 1 — default values | (none) | Validates baseline rendering with chart defaults |
| 2 — webhook disabled | `webhook.enabled=false` | Validates conditional exclusion of webhook resources |
| 3 — external service account | `serviceAccount.create=false`, `serviceAccount.name=existing-sa` | Validates ServiceAccount conditional logic |
| 4 — custom resources | `resources.limits.cpu=100m`, `resources.limits.memory=64Mi` | Validates resource override wiring |
| 5 — namespace-scoped RBAC | `rbac.namespaceScoped=true`, `webhook.enabled=false` | Validates Role/RoleBinding rendering instead of ClusterRole/ClusterRoleBinding |

**Unit test suites (step 8):** each operator chart runs its own `tests/`
suites; the keystone-operator suites are listed below as representative.

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

End-to-end infrastructure deployment and Chainsaw test. Deploys the full
infrastructure stack (Flux, cert-manager, MariaDB, ESO, OpenBao) to a kind cluster and
validates health of all operators, CRs, and ExternalSecrets.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'pull_request' && needs.changes.outputs.e2e-infra == 'true'`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `helm/kind-action@v1.14.0` | Creates kind cluster (`forge`) |
| 4 | `setup-e2e-infra` composite action | Installs Flux CLI, test deps, and deploys infra stack |
| 5 | `chainsaw test` | Runs E2E tests from `tests/e2e/infrastructure/` |
| 6 | `hack/ci-dump-diagnostics.sh` (on failure) | Dumps HelmReleases, pods, events, Flux logs |
| 7 | Upload JUnit report | Uploads test results as artifact (14-day retention) |

Timeout: 20 minutes.

### build-e2e-images

Centralised image build for E2E test jobs. Builds all Docker images (base, operator,
service, tempest) once and pushes them to GHCR under run-scoped tags
(`e2e-${run_id}-<orig_tag>`). The `e2e-operator`, `e2e-chaos`, and `tempest` jobs
`docker pull` from GHCR via the `load-e2e-images` composite action instead of
rebuilding, saving ~5-10 min per CI run.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, verify-invalid-cr-fixtures, chainsaw-lint]`

**Condition:** Runs only when any of `has-e2e-operators`, `e2e-chaos`,
`e2e-prometheus`, or `e2e-controlplane` is `'true'`
and no gate job failed. Uses `always()` so the job runs when upstream Go jobs are
skipped (e.g. pure E2E test-definition PRs where `go=false`). Skipped on PRs from
forks (the workflow's `GITHUB_TOKEN` is read-only on `packages:` for forked
`pull_request` events, so GHCR push would fail) — see `github.event.pull_request.head.repo.fork`
guard.

**Permissions:** `contents: read`, `packages: write` (required for GHCR push).

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `docker/setup-buildx-action@v4` | Sets up BuildKit for `type=gha` cache support |
| 3 | `docker/login-action@v4` | Authenticates to GHCR with `GITHUB_TOKEN` |
| 4 | Resolve build operators | Unions `e2e-operators` with a fixed `keystone` entry (required by tempest) |
| 5 | Build base images | Builds `python-base` and `venv-builder` (reused by subsequent builds) |
| 6 | Build operator images | Builds `<IMAGE_PREFIX>/<op>-operator:dev` for each resolved operator |
| 7 | Build service images | Builds `<IMAGE_PREFIX>/<op>:<release>` for each operator x release combination |
| 8 | Build Tempest images | Builds `<IMAGE_PREFIX>/tempest:<release>` for all releases |
| 9 | Push E2E images to GHCR | For each image, `docker tag` to `<repo>:e2e-${run_id}-<orig_tag>` and `docker push` |

The "Resolve build operators" step guarantees that `keystone` is always in the build set.
This is required because the `tempest` job hardcodes `keystone-operator:dev` and
`keystone:<release>` — without the union, a pipeline triggered by a different operator
(e.g. glance) would fail tempest due to missing keystone images.

GH-310 replaced the previous `docker save | zstd | upload-artifact` transport with
GHCR push/pull because the 355 MB single-blob artifact intermittently timed out at
the 5-minute `actions/download-artifact` window. Layer-level pull retries plus the
GHCR CDN dramatically reduce the failure rate.

Timeout: 30 minutes.

### e2e-operator

End-to-end operator test using kind cluster and Chainsaw.
Pulls pre-built images from GHCR via the `load-e2e-images`
composite action, deploys the infrastructure stack and operator via Helm, and runs
Chainsaw E2E test suites.

**Dependencies:** `needs: [changes, build-e2e-images]`
**Condition:** Runs only when `has-e2e-operators == 'true'` and `build-e2e-images` succeeded.
**Permissions:** `contents: read`, `packages: read` (required for GHCR pull).

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `helm/kind-action@v1.14.0` | Creates kind cluster (`forge`) |
| 4 | `load-e2e-images` composite action | Pulls run-scoped GHCR tags and re-tags to canonical local refs |
| 5 | `kind load docker-image` | Loads operator, 2025.2 service, 2025.2-upgraded, and 2026.1 service images into kind |
| 6 | `setup-e2e-infra` composite action | Installs Flux CLI, test deps, and deploys infra stack |
| 7 | `hack/ci-deploy-operator.sh` | Installs CRDs and deploys operator via Helm |
| 8 | `chainsaw test` | Runs E2E tests from `tests/e2e/<operator>/` |
| 9 | `hack/ci-dump-diagnostics.sh` (always) | Dumps operator pods, all pods, events, operator logs |
| 10 | Upload JUnit report | Uploads test results as artifact (14-day retention) |

**Matrix strategy:**

```yaml
strategy:
  fail-fast: false
  matrix: ${{ fromJson(needs.changes.outputs.e2e-operators) }}
```

The operator matrix is dynamically constructed by the `changes` job, including only operators
whose code (or shared code) changed. The `imagePullPolicy: Never` Helm value ensures the
kind-loaded image is used instead of attempting a registry pull. Timeout: 45 minutes.

### e2e-operator-upgrade

Operator helm-upgrade-in-place E2E. Installs the last released keystone-operator
chart+image from GHCR as the baseline, brings a Keystone CR to Ready, then
`helm upgrade`s the release to the locally built chart+CRDs and asserts the
deployed Keystone survives the operator upgrade (Ready persists,
`status.installedRelease` unchanged, no re-bootstrap). See
[Operator Upgrade E2E Tests](../testing/operator-upgrade-e2e-tests.md) for suite
details.

**Dependencies:** `needs: [changes, build-e2e-images]`
**Condition:** Runs on `pull_request` when `has-e2e-operators == 'true'`,
`build-e2e-images` succeeded, and no dependency failed or was cancelled.
**Permissions:** `contents: read`, `packages: read` (required for GHCR pull).

Unlike the per-CR `e2e-operator` matrix, this suite manages the operator Helm
release itself, so it runs in its own single job. The job pulls the run-scoped
`:dev` operator and `2025.2` service images, `helm registry login`s GHCR,
fetches the released baseline via `hack/ci-fetch-released-operator.sh`, installs
it via `hack/ci-deploy-operator.sh` (with `CHART_DIR` pointing at the pulled
chart and `IMAGE_TAG=latest`), deploys the infra stack, and runs the suite from
`tests/e2e-operator-upgrade/`. Blocking (no `continue-on-error`). Timeout: 45
minutes.

### e2e-chaos

End-to-end chaos tests using kind cluster, Chaos Mesh, and Chainsaw. Pulls the
keystone operator and service images from GHCR via the `load-e2e-images` composite
action, deploys them alongside Chaos Mesh infrastructure, and runs the chaos test
suites (MariaDB pod kill, Memcached pod kill, OpenBao pod kill, MariaDB network
partition, MariaDB network latency). See
[Chaos E2E Test Suites](../testing/chaos-e2e-tests.md) for test suite details.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images, e2e-operator]`
**Condition:** Runs only when `e2e-chaos == 'true'` or the PR has a `run-chaos` label, `build-e2e-images` succeeded, and no dependency failed or was cancelled.
**Permissions:** `contents: read`, `packages: read` (required for GHCR pull).

The `e2e-chaos` job depends on the standard gate jobs plus `e2e-operator`, so chaos
tests run after the happy-path operator E2E suite has passed. Gating is set per
matrix leg via `continue-on-error: ${{ matrix.suite == 'network' }}`: the `pod`
leg is **blocking** — an operator-restart, PDB, or rotation regression fails the
build — while the `network` leg stays **non-blocking**, because its
`ip_set`/`sch_netem` kernel-module dependency is resolvable only on the
GitHub-hosted runner and remains prone to environment flakiness. On-demand
pre-validation of either leg is available via the `run-chaos` PR label.

The job runs as a two-entry matrix split by chaos type: the `pod` suite (PodChaos
tests) runs on `blacksmith-4vcpu-ubuntu-2404` for speed, while the `network` suite
(NetworkChaos tests) runs on a GitHub-hosted `ubuntu-24.04` runner, where the
`linux-modules-extra` kernel modules required by `ip_set`/`sch_netem` are resolvable
(the Blacksmith Firecracker microVM kernel ships without them). Each matrix entry
lists its per-suite test directories explicitly.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `helm/kind-action@v1.14.0` | Creates kind cluster (`forge`) |
| 3 | `load-e2e-images` composite action | Pulls run-scoped GHCR tags and re-tags to canonical local refs |
| 4 | `kind load docker-image` | Loads keystone operator and 2025.2 service images into kind |
| 5 | `setup-e2e-infra` composite action | Installs Flux CLI, test deps, and deploys infra stack with `WITH_CHAOS_MESH=true` |
| 6 | `hack/ci-deploy-operator.sh` | Installs CRDs and deploys keystone operator via Helm |
| 7 | `chainsaw test` | Runs chaos E2E tests from `tests/e2e-chaos/` with `tests/e2e-chaos/chainsaw-config.yaml` |
| 8 | `hack/ci-dump-diagnostics.sh` (always) | Dumps operator pods, all pods, events, operator logs with `OPERATOR=keystone` |
| 9 | Upload JUnit report | Uploads `_output/reports/` as `e2e-chaos-junit-report-<suite>` artifact (14-day retention) |

**Key differences from `e2e-operator`:**

| Aspect | `e2e-operator` | `e2e-chaos` |
| --- | --- | --- |
| Matrix | Dynamic per-operator | Two suites (`pod` / `network`) on different runners |
| Test config | `tests/e2e/chainsaw-config.yaml` | `tests/e2e-chaos/chainsaw-config.yaml` |
| Test directory | `tests/e2e/<operator>/` | per-suite `test_dirs` under `tests/e2e-chaos/` |
| Timeout | 45 minutes | 60 minutes |
| Blocking | Yes | `pod` leg blocking; `network` leg non-blocking (`continue-on-error: ${{ matrix.suite == 'network' }}`) |
| Dependencies | Gate jobs | Gate jobs + `e2e-operator` |
| Service images | 2025.2 + 2025.2-upgraded + 2026.1 | 2025.2 only |

The chaos test Chainsaw config uses `parallel: 1` (serial execution) because chaos tests
mutate shared infrastructure pod availability. The assert timeout is 300s (vs 120s for
happy-path tests) to allow multiple reconciliation cycles and pod restart time during
fault recovery.

**Path filter:** `tests/e2e-chaos/**`, `hack/**`, `deploy/**`, `.github/workflows/ci.yaml`, `.github/actions/**`
(separate from `e2e_infra` to allow independent gating). Additionally, any Go code change
— operator-specific (e.g., `operators/keystone/**/*.go`) or shared (`internal/common/**/*.go`
via `go_common`) — triggers the job via `go_changed` in `ci-resolve-changes.sh`, since chaos
tests validate operator resilience against the current codebase.

### e2e-prometheus

End-to-end kube-prometheus-stack tests using kind cluster, Flux-managed
`kube-prometheus-stack` HelmRelease, and Chainsaw. Builds the
keystone operator image, deploys it alongside the monitoring stack, and runs
the prometheus suite under `tests/e2e/keystone/prometheus-stack/` to verify
HelmRelease readiness, ServiceMonitor presence, and live Prometheus scraping
of the operator metrics endpoint.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]`
**Condition:** Runs only when `e2e-prometheus == 'true'`, the upstream
`build-e2e-images` job succeeded, and no dependency failed or was cancelled.

The `setup-e2e-infra` composite action is invoked with `WITH_PROMETHEUS: "true"`
in its step `env`, which threads through to `hack/deploy-infra.sh` and gates the
`kube-prometheus-stack` overlay (`deploy/kind/prometheus/`) plus the
post-deploy `enable_operator_servicemonitor` patch (applied to both the
keystone-operator and horizon-operator HelmReleases). The Deploy
operator step runs `hack/ci-deploy-operator.sh` with `WITH_PROMETHEUS: "true"`
in its step `env`, which adds `--set monitoring.serviceMonitor.enabled=true`
to the Helm install command — without this flag the chart's gated
`ServiceMonitor` template renders nothing and the chainsaw step
`servicemonitor-exists` (and the dependent `prometheus-target-up`) cannot
pass. The kind base kustomization keeps the keystone-operator HelmRelease
suspended, so the runtime `kubectl patch` cannot reactively enable the
ServiceMonitor — the install-time flag is the single source of truth.

Unlike `e2e-chaos`, `e2e-prometheus` runs with `continue-on-error: false`:
the kube-prometheus stack is deterministic on kind, so any failure is a
genuine regression of the kind-only Quick Start observability story.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `helm/kind-action@v1.14.0` | Creates kind cluster (`forge`) |
| 3 | `load-e2e-images` composite | Restores prebuilt operator and service images from the build-e2e-images artifact |
| 4 | `kind load docker-image` | Loads operator and service images into kind |
| 5 | `setup-e2e-infra` composite action | Installs Flux CLI, test deps, and deploys infra stack with `WITH_PROMETHEUS: "true"` |
| 6 | `hack/ci-deploy-operator.sh` | Installs CRDs and deploys keystone operator via Helm with `WITH_PROMETHEUS: "true"` (gates `--set monitoring.serviceMonitor.enabled=true`) |
| 7 | `chainsaw test` | Runs the prometheus E2E suite from `tests/e2e/keystone/prometheus-stack/` |
| 8 | `hack/ci-dump-diagnostics.sh` (always) | Dumps operator pods, all pods, events, operator logs with `OPERATOR=keystone` |
| 9 | Upload JUnit report | Uploads `_output/reports/` as `e2e-prometheus-junit-report` artifact (14-day retention) |

**Path filter:** `deploy/kind/prometheus/**`, `tests/e2e/keystone/prometheus-stack/**`,
`hack/**`, `deploy/**`, `.github/workflows/ci.yaml`, `.github/actions/**`. As
with `e2e-chaos`, any Go code change (`go_changed`) or any E2E test change
(`any_e2e_tests`) also triggers the job via `ci-resolve-changes.sh`, since
the prometheus suite scrapes live operator metrics.

### e2e-controlplane

Runs the full c5c3 `ControlPlane` → Keystone chain on kind. It deploys
`keystone-operator` + K-ORC + `c5c3-operator` as local dev images (rather than
the GHCR-published Flux chart) and runs the
`tests/e2e/c5c3/full-controlplane-keystone/` Chainsaw suite, which asserts the
whole orchestration link by link: managed MariaDB/Memcached provisioning, the
projected Keystone CR, the minted restricted K-ORC application credential, the
OpenBao → ESO credential round-trip, the identity catalog, and finally a live
`openstack token issue` / `catalog list` against the Keystone `/v3` endpoint.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]`
**Condition:** Runs only when `e2e-controlplane == 'true'`, the upstream
`build-e2e-images` job succeeded, and no dependency failed or was cancelled.

`setup-e2e-infra` is invoked with `WITH_CONTROLPLANE: "true"`,
`CONTROLPLANE_OPERATORS: external`, and
`CONTROLPLANE_NAME: controlplane-keystone`. Under `CONTROLPLANE_OPERATORS=external`
`hack/deploy-infra.sh` prepares only the shared prerequisites (TLS issuers,
OpenBao with per-CR admin-password seeding, the ESO ClusterSecretStore) and
suspends the Flux ControlPlane stack, so the dev-image operators deployed by the
subsequent steps own the reconcile. K-ORC is applied by `hack/ci-deploy-korc.sh`
at the tag pinned in `deploy/flux-system/sources/k-orc.yaml`.

The suite runs with `E2E_REQUIRE_CONTROLPLANE_STACK: "true"`, which flips its
presence guard from a silent SKIP to a hard failure — so a broken operator/CRD
deployment fails the build instead of going green. Like `e2e-prometheus`, the
job runs with `continue-on-error: false`, and it uses a 60-minute timeout on the
larger runner because a real MariaDB + Memcached + Keystone + three operators +
OpenBao + ESO + K-ORC on one node is resource-heavy.

### e2e-controlplane-sso

Runs the `tests/e2e-controlplane-sso/` Chainsaw suite: the end-user SSO
experience — the Horizon websso projection, the login page's SSO choice and
domain dropdown, the websso round trip through the gateway, and LDAP-domain
login. The suite lives outside `tests/e2e/` so the per-CR `e2e-operator` matrix
leg, which runs `tests/e2e/<operator>/` wholesale, does not sweep it up.

**A sibling job rather than a second suite directory on `e2e-controlplane`.**
The ControlPlane webhook permits one ControlPlane per namespace, and
`openstack-gw` sets `allowedRoutes.namespaces.from: Same` (the operators
deliberately do not manage `ReferenceGrant`), so the two suites can share
neither the `openstack` namespace nor the Gateway. Each therefore needs its own
kind cluster.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]`
**Condition:** Runs only when `e2e-controlplane == 'true'`, the upstream
`build-e2e-images` job succeeded, and no dependency failed or was cancelled.

It mirrors `e2e-controlplane`'s setup with `CONTROLPLANE_NAME: controlplane-sso`
(so the OpenBao bootstrap seeds the per-CR admin-password and Horizon
`SECRET_KEY` paths the chain reads) and additionally loads
`keystone-federation-proxy:dev` into kind, because the suite's ControlPlane CR
pins `services.keystone.federationProxyImage.tag: dev`. Without that override
the suite would validate the sidecar already published on `main` rather than the
one under review — which is why the `e2e_controlplane` path filter also watches
`images/keystone-federation-proxy/**`.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `helm/kind-action@v1.14.0` | Creates kind cluster (`forge`) |
| 3 | `load-e2e-images` composite | Restores `keystone-operator:dev`, `c5c3-operator:dev`, `keystone:2025.2`, `tempest:2025.2` from GHCR |
| 4 | `kind load docker-image` | Loads the four images into kind |
| 5 | `setup-e2e-infra` composite action | Deploys infra with `WITH_CONTROLPLANE=true CONTROLPLANE_OPERATORS=external CONTROLPLANE_NAME=controlplane-keystone` |
| 6 | `hack/ci-deploy-korc.sh` | Applies K-ORC CRDs + controller at the pinned tag |
| 7 | `hack/ci-deploy-operator.sh` (keystone) | Deploys the keystone-operator dev image into `keystone-system` |
| 8 | `hack/ci-deploy-operator.sh` (c5c3) | Deploys the c5c3-operator dev image into `c5c3-system` |
| 9 | `chainsaw test` | Runs the full-chain suite with `E2E_REQUIRE_CONTROLPLANE_STACK=true` |
| 10 | `hack/ci-dump-diagnostics.sh` (always) | Dumps diagnostics with `OPERATOR=c5c3` |
| 11 | Upload JUnit report | Uploads `_output/reports/` as `e2e-controlplane-junit-report` (14-day retention) |

**Path filter:** `operators/c5c3/**`, `operators/keystone/**`, `tests/e2e/c5c3/**`,
`deploy/**`, `hack/**`, `.github/actions/**`, `.github/workflows/ci.yaml`. As with
`e2e-prometheus`, any Go code change (`go_changed`) or any E2E test change
(`any_e2e_tests`) also triggers the job via `ci-resolve-changes.sh`. When it
runs, `build-e2e-images` unions `c5c3` into the built operator set so both dev
images exist even for a full-chain-test-only change.

### e2e-external-keystone

Runs the `tests/e2e/c5c3/external-keystone/` Chainsaw suite: an External-mode
`ControlPlane` driven against a plain Keystone the operator does **not** own. The
suite brings up its own operator-free, SQLite-backed Keystone fixture in a
separate namespace, then drives four External `ControlPlane`s against it and
asserts the whole adoption contract — convergence with zero
MariaDB/Memcached/Keystone children, admin and catalog imports, the application
credential minted against the external API and round-tripped through OpenBao, no
catalog pollution (compared against a pre-recorded inventory), service-account
usability and rotation, out-of-band password rotation and drift detection, the
`endpoint_type` failure detection, the wrong-password and ambiguous-catalog
negative paths, and zero-blast-radius deletion.

**A sibling job rather than a second suite directory on `e2e-controlplane`.** The
ControlPlane webhook permits one ControlPlane per namespace, and this suite
stands up a wholly different infrastructure shape (a plain, operator-free
Keystone fixture plus four External ControlPlanes, none provisioning
MariaDB/Memcached), so it needs its own kind cluster.

**Dependencies:** `needs: [changes, lint, shellcheck, test, test-integration, verify-codegen, chainsaw-lint, build-e2e-images]`
**Condition:** Runs only when `e2e-controlplane == 'true'`, the upstream
`build-e2e-images` job succeeded, and no dependency failed or was cancelled.

It mirrors `e2e-controlplane`'s setup with `WITH_CONTROLPLANE: "true"`,
`CONTROLPLANE_OPERATORS: external`, and `WITH_CONTROLPLANE_CR: "false"`, but
leaves `CONTROLPLANE_NAME` at its default: the OpenBao bootstrap only seeds the
managed-mode admin-password path, which the External ControlPlanes — in their own
namespaces, authenticating from user-supplied Secrets — never read, and the suite
asserts their own per-CR OpenBao paths are never-seeded. It loads the
`keystone:2025.2` and `tempest:2025.2` service images (but **not** `horizon:2025.2`
— External mode never runs a Horizon workload; the horizon-operator is deployed
only for its CRD) and runs with `E2E_REQUIRE_CONTROLPLANE_STACK: "true"` so a
broken deployment fails the build instead of the suite skipping.

**Path filter:** shares the `e2e-controlplane` change-detection output, so the
same `e2e_controlplane` filter (`operators/c5c3/**`, `operators/keystone/**`,
`operators/horizon/**`, `tests/e2e/c5c3/**`, `deploy/**`, `hack/**`,
`.github/actions/**`, `.github/workflows/ci.yaml`) triggers it.

### tempest

Tempest API integration tests. Deploys services into a kind
cluster and runs the OpenStack Tempest test suite against them. Uses a release matrix to validate each OpenStack release independently, with per-release Tempest
configuration, Keystone CRs, and K8s service names. Pulls pre-built images from
GHCR (run-scoped tag) via the `load-e2e-images` composite action.

**Dependencies:** `needs: [changes, build-e2e-images, e2e-infra, e2e-operator, e2e-chaos, e2e-prometheus]`
**Condition:** Runs only when `has-e2e-operators == 'true'`, `build-e2e-images` succeeded, and no other E2E job failed or was cancelled — tempest is the last E2E job in the chain.
**Permissions:** `contents: read`, `packages: read` (required for GHCR pull).

**Matrix strategy:**

```yaml
strategy:
  fail-fast: false
  matrix:
    include:
      - release: "2025.2"
        config-dir: tests/tempest/keystone
        cr-name: keystone-tempest
        service-k8s-name: keystone-tempest-api
      - release: "2026.1"
        config-dir: tests/tempest/keystone-2026-1
        cr-name: keystone-tempest-2026-1
        service-k8s-name: keystone-tempest-2026-1-api
```

Each matrix entry specifies: the release version, the Tempest configuration directory,
the Keystone CR name, and the K8s service name used for port-forwarding. Steps reference
these via `matrix.release`, `matrix.config-dir`, `matrix.cr-name`, and
`matrix.service-k8s-name`.

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `actions/setup-go@v6` | Sets up Go with `go-version-file: go.work` |
| 3 | `helm/kind-action@v1.14.0` | Creates kind cluster (`forge`) |
| 4 | `load-e2e-images` composite action | Pulls run-scoped GHCR tags and re-tags to canonical local refs |
| 5 | `kind load docker-image` | Loads keystone operator and service images into kind |
| 6 | `setup-e2e-infra` composite action | Installs Flux CLI, test deps, and deploys infra stack |
| 7 | `hack/ci-deploy-operator.sh` | Installs CRDs and deploys operator via Helm |
| 8 | Deploy Keystone CR | Applies `matrix.config-dir/00-keystone-cr.yaml` and waits for `matrix.cr-name` Ready |
| 9 | `hack/ci-run-tempest.sh` | Runs Tempest API tests with `CONFIG_DIR=matrix.config-dir`, `SERVICE_K8S_NAME=matrix.service-k8s-name` |
| 10 | Upload Tempest results | Uploads `_output/tempest/` as `tempest-<release>-results` artifact (14-day retention) |
| 11 | `hack/ci-dump-diagnostics.sh` (always) | Dumps diagnostic info with `OPERATOR=keystone` |

Timeout: 45 minutes.

### cleanup-e2e-tags

GH-310. Prunes the run-scoped GHCR tags pushed by `build-e2e-images`
(`e2e-${run_id}-*`) so they don't accumulate on the package page. Runs as a
matrix over each E2E target package (`keystone-operator`, `keystone`,
`c5c3-operator`, `tempest`) after every consumer that might still pull the images
has finished. The
`always() && needs.build-e2e-images.result == 'success'` condition means the
cleanup runs on success, failure, cancelled, or skipped consumer outcomes — but
only when `build-e2e-images` actually pushed something.

**Dependencies:** `needs: [build-e2e-images, e2e-operator, e2e-chaos, tempest]`
**Permissions:** `contents: read`, `packages: write`

The nightly `cleanup-e2e-stale-tags` job in `cleanup-images.yaml` is the safety
net: if a workflow is cancelled before `cleanup-e2e-tags` fires, that job
deletes any `e2e-*` tag older than one day across the same package set.

Timeout: 10 minutes.

### build-and-push

Builds operator container images per platform on native runners and pushes each
single-arch image by digest. Runs only on push events (main branch or
v* tags) — skipped on pull requests. The multi-arch manifest list and final tags are
assembled by the subsequent `merge-operator-images` job.

Publish-only-on-merge: the merged commit's PR was already green, so the E2E suite is
not re-run on push. The job depends only on `changes` (for the operator matrix) and
builds the image from its own cache scope, independent of `build-e2e-images`.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'push' && needs.changes.outputs.has-e2e-operators == 'true'`
**Permissions:** `contents: read`, `packages: write`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
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
list, and pushes it with the final tags.

**Dependencies:** `needs: [changes, build-and-push]`
**Condition:** `if: github.event_name == 'push' && needs.build-and-push.result == 'success'`
**Permissions:** `contents: read`, `packages: write`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `docker/setup-buildx-action@v4` + `docker/login-action@v4` | Authenticates to GHCR |
| 3 | `docker/metadata-action@v6` | Generates final image tags |
| 4 | Download digests | `actions/download-artifact@v8` | Downloads all `digests-operator-<operator>-*` artifacts |
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

Packages and pushes operator Helm charts to the GHCR OCI registry.
Runs only on push events — skipped on pull requests. Like `build-and-push`, this is
publish-only-on-merge: the chart is packaged and pushed for every changed operator on
push without re-running the E2E suite.

**Dependencies:** `needs: [changes]`
**Condition:** `if: github.event_name == 'push' && needs.changes.outputs.has-e2e-operators == 'true'`
**Permissions:** `contents: read`, `packages: write`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `azure/setup-helm@v5` | Installs Helm CLI |
| 3 | Helm registry login | Authenticates to GHCR via `helm registry login` |
| 4 | Package and push | Packages chart and pushes to `oci://ghcr.io/c5c3/charts/` |

**Chart version derivation:**

| Trigger | Version |
| --- | --- |
| Push to main | Default version from `Chart.yaml` |
| Push v* tag | SemVer derived from tag (v prefix stripped, e.g. `v0.1.0` → `0.1.0`) |

**Matrix strategy:** Same `operator` dimension as `build-and-push` (via
`fromJson(needs.changes.outputs.e2e-operators)`), so every changed operator's chart is
packaged and pushed.

The `make helm-package` target packages `operators/<operator>/helm/<operator>-operator/`.
When `CHART_VERSION` is set (for tag pushes), it overrides the version in `Chart.yaml`.

### github-release

Creates a GitHub Release with auto-generated release notes on v* tag pushes.

**Dependencies:** `needs: [changes, merge-operator-images, helm-push]`
**Condition:** `if: startsWith(github.ref, 'refs/tags/v') && needs.merge-operator-images.result == 'success' && needs.helm-push.result == 'success'`
**Permissions:** `contents: write`

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v7` | Checks out the repository (SHA-pinned) |
| 2 | `azure/setup-helm@v5` | Installs Helm CLI for chart packaging |
| 3 | Package Helm charts | Packages operator Helm charts with release version |
| 4 | `softprops/action-gh-release@v3` | Creates release with `generate_release_notes: true` and attaches chart tarballs |

This job runs only after both `merge-operator-images` and `helm-push` complete
successfully, ensuring the final multi-arch manifest list and charts are published before
the release is created. Helm chart tarballs
are attached as release assets for direct download. Timeout: 5 minutes.

## Reusable CI Scripts

Repeated inline shell logic from E2E jobs is extracted into standalone scripts under
`hack/`. Each script uses `set -euo pipefail`, includes an SPDX Apache-2.0 header, and
passes shellcheck. All scripts are designed to work both in CI and locally
against any kubeconfig.

### hack/ci-dump-diagnostics.sh

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

### hack/ci-build-service-image.sh

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

### hack/ci-deploy-operator.sh

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

### hack/ci-build-tempest-image.sh

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

### hack/ci-run-tempest.sh

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
| `SERVICE_K8S_NAME` | No | `<SERVICE>-tempest-api` | K8s Service name for port-forwarding (allows override for release-specific CR names, e.g. `keystone-tempest-2026-1-api`) |

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

## Composite Action: setup-test-deps

`.github/actions/setup-test-deps/action.yaml`

A composite GitHub Action that encapsulates the shared cache + `make install-test-deps`
step used by every job that needs the pinned `chainsaw`/`flux`/`kind`/`kubectl` binaries.
Extracted so the cache key, `restore-keys:`, and `PATH` wiring live in one place:
`setup-e2e-infra` (cluster-bound jobs) and the lightweight `chainsaw-lint` job both
consume this and inherit any future tweaks (key bump, additional pinned tool) for free.

| Step | Description |
| --- | --- |
| 1 | Restores `$HOME/.local/bin` from cache, keyed on the hash of `hack/install-test-deps.sh` (auto-invalidates when any pinned tool version changes) |
| 2 | Runs `make install-test-deps` (no-op on cache hit thanks to the script's skip-if-correct-version logic) and appends `~/.local/bin` to `GITHUB_PATH` |

The action takes no inputs.

## Composite Action: setup-e2e-infra

`.github/actions/setup-e2e-infra/action.yaml`

A composite GitHub Action that encapsulates the shared Flux CLI + test dependencies +
infrastructure deployment sequence used by `e2e-infra`, `e2e-operator`, and `tempest` jobs.
This replaces three duplicated step sequences with a single `uses:` reference.

**Prerequisite:** A kind cluster must already exist (the action sets `SKIP_KIND_CREATE=true`
internally).

| Step | Description |
| --- | --- |
| 1 | Installs Flux CLI via `fluxcd/flux2/action@v2.9.0` (SHA-pinned) |
| 2 | Delegates to the `setup-test-deps` composite action (cache restore + `make install-test-deps` + `PATH` wiring) |
| 3 | Runs `make deploy-infra` with `SKIP_KIND_CREATE=true` |

Usage in a workflow job:

```yaml
- name: Setup E2E infrastructure
  uses: ./.github/actions/setup-e2e-infra
```

The action takes no inputs. All configuration is handled by existing Makefile targets and
environment variables.

## Composite Action: load-e2e-images

`.github/actions/load-e2e-images/action.yaml`

A composite GitHub Action that pulls pre-built E2E images from GHCR (under the
run-scoped tag pushed by `build-e2e-images`) and re-tags them to their canonical
local references so downstream `kind load docker-image` calls work unchanged.
Shared between `e2e-operator`, `e2e-chaos`, and `tempest` jobs.

| Step | Description |
| --- | --- |
| 1 | `docker/login-action@v4` authenticates to GHCR using the workflow's `GITHUB_TOKEN` |
| 2 | For each input ref, `docker pull <repo>:e2e-${run_id}-<orig_tag>` then `docker tag` to the canonical local ref |

| Input | Default | Description |
| --- | --- | --- |
| `run-id` | `${{ github.run_id }}` | Run ID used as the tag prefix (`e2e-<run-id>-`) |
| `images` | (required) | Multiline list of canonical local refs (e.g. `ghcr.io/c5c3/keystone:2025.2`); blank/comment lines are ignored |
| `registry` | `ghcr.io` | Registry to authenticate against |
| `username` | `${{ github.actor }}` | Login user |
| `password` | `${{ github.token }}` | Login token |

Usage in a workflow job:

```yaml
- name: Load E2E images
  uses: ./.github/actions/load-e2e-images
  with:
    images: |
      ${{ env.IMAGE_PREFIX }}/keystone-operator:dev
      ${{ env.IMAGE_PREFIX }}/keystone:2025.2
```

GH-310 replaced the previous `actions/download-artifact` + `zstd | docker load`
sequence: the 355 MB single-blob artifact intermittently timed out at the
five-minute window (`actions/download-artifact` has no built-in retry on a stalled
download). Layer-level pull retries plus the GHCR CDN dramatically reduce the
failure rate.

## How the Pieces Fit Together

The E2E jobs follow a common pattern with shared components:

```
1. Checkout + Go setup + kind cluster creation     (workflow steps)
2. Pull pre-built images from GHCR                  (load-e2e-images composite action)
3. Load images into kind                            (workflow steps)
4. Deploy infrastructure                            (setup-e2e-infra composite action)
5. Deploy operator                                  (hack/ci-deploy-operator.sh)
6. Run tests                                        (chainsaw / hack/ci-run-tempest.sh)
7. Dump diagnostics                                 (hack/ci-dump-diagnostics.sh)
8. Upload artifacts                                 (workflow steps)
```

Image building is centralised in `build-e2e-images`, which runs once before the E2E jobs
and pushes every image to GHCR under a run-scoped tag. The `e2e-infra` job uses steps 1,
4, 6-8 (no operator or service images needed). The `e2e-operator`, `e2e-chaos`, and
`tempest` jobs use all steps, pulling their required images from GHCR via
`load-e2e-images`. The `e2e-chaos` job uses a chaos-specific Chainsaw config
(`tests/e2e-chaos/chainsaw-config.yaml`) and test directory (`tests/e2e-chaos/`). The
`tempest` job additionally deploys a Keystone CR before running `hack/ci-run-tempest.sh`
instead of Chainsaw. The `cleanup-e2e-tags` job prunes the run-scoped tags after every
consumer finishes.

## Go Setup Convention

All Go-based jobs use `actions/setup-go@v6` with:

```yaml
go-version-file: go.work
```

This reads the Go version from `go.work` (currently Go 1.26.5) rather than hardcoding a
`go-version` value. The repository root contains `go.work` (not `go.mod`) because the
project uses a Go Workspace with multiple modules (`internal/common`, `operators/keystone`,
`operators/c5c3`). Module dependency caching is enabled by default in `actions/setup-go@v6`.

## Concurrency

The workflow uses a concurrency group scoped per-branch per-workflow:

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

All GitHub Actions are referenced by full SHA hash with a trailing version comment:

```yaml
- uses: actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0 # v7
```

This prevents supply chain attacks via mutable tag retargeting and provides audit
traceability. The version comment preserves human readability.

## SPDX Header

The file starts with the standard SPDX license header:

```text
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
---
```

## Codecov Configuration

`.codecov.yml` defines coverage status checks and component-level thresholds.

### Ignored Paths

The top-level `ignore` block drops generated code from every coverage
denominator (project, patch, and component targets):

| Pattern | Reason |
| --- | --- |
| `**/zz_generated*.go` | controller-gen DeepCopy plumbing — mechanically generated, no hand-written logic to test. Counting it understates real coverage, notably for the `webhooks` component (target 90%), whose `api/` paths include the generated deepcopy file. |

This mirrors the lint exclusion of the same files (see the note above the
`verify-codegen` job).

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

The CI workflow depends on several Makefile targets:

### docker-build

Builds the operator Docker image from `operators/<operator>/Dockerfile` with the
repository root as build context (required by `go.work`).

```
make docker-build OPERATOR=keystone [IMG=custom:tag]
```

The `IMG` variable controls the image tag, defaulting to
`ghcr.io/c5c3/<operator>-operator:latest`. The `OPERATOR` variable is required.

### helm-package

Packages the operator Helm chart from
`operators/<operator>/helm/<operator>-operator/`.

```
make helm-package OPERATOR=keystone [CHART_VERSION=1.2.3]
```

When `CHART_VERSION` is set, it overrides the version in the chart's `Chart.yaml`. The
packaged `.tgz` is output to the current directory. The `OPERATOR` variable is required.

### test-common

Runs unit tests for `internal/common` only, producing a single coverage profile.

```
make test-common
```

Produces `cover-unit-common.out`. Used by the `common` matrix leg in the `test` CI job to
deduplicate common coverage into a single upload.

### test-operator

Runs unit tests for a single operator without `internal/common`.

```
make test-operator OPERATOR=keystone
```

Produces `cover-unit-<operator>.out`. Used by operator matrix legs in the `test` CI job.
The `OPERATOR` variable is required.

### test-integration

Runs envtest-based integration tests (tagged with `//go:build integration`) for operators.
Requires `setup-envtest` to be installed.

```
make test-integration [OPERATOR=keystone]
```

Sets `KUBEBUILDER_ASSETS` via `setup-envtest use <pinned-k8s-version> -p path`, then runs
`go test -tags=integration` for each operator module. Produces
`cover-integration-<operator>.out` files. Without `OPERATOR`, runs for all operators in
the `OPERATORS` list.

### test-integration-common

Runs envtest-based integration tests for `internal/common` only.

```
make test-integration-common
```

Sets `KUBEBUILDER_ASSETS` via `setup-envtest use <pinned-k8s-version> -p path`, then runs
`go test -tags=integration ./internal/common/...`. Produces `cover-integration-common.out`.
Used by the `common` matrix leg in CI to meet the 80% codecov target for `internal/common/`.

## Dependencies on Prior Features

The CI workflow depends on the following artifacts:

| Artifact | Used by | Purpose |
| --- | --- | --- |
| `Makefile` (`lint` target) | `lint` job | Iterates over `OPERATORS` variable to run golangci-lint per module |
| `Makefile` (`test-common` target) | `test` job (`common` leg) | Runs unit tests for `internal/common` with coverage profile |
| `Makefile` (`test-operator` target) | `test` job (operator legs) | Runs unit tests for a single operator with coverage profile |
| `Makefile` (`test-integration` target) | `test-integration` job (operator legs) | Runs envtest integration tests per operator with coverage profiles |
| `Makefile` (`test-integration-common` target) | `test-integration` job (`common` leg) | Runs envtest integration tests for `internal/common` with coverage profile |
| `Makefile` (`docker-build` target) | `build-e2e-images`, `e2e-chaos`, `build-and-push` jobs | Builds operator Docker images |
| `Makefile` (`helm-package` target) | `helm-push` job | Packages operator Helm charts |
| `.golangci.yml` | `lint` job | Provides linter configuration (enabled linters, exclusion rules, timeout) |
| `go.work` | All Go-based jobs | Provides the Go version for `actions/setup-go@v6` |
| `hack/*.sh` | `shellcheck` job | Shell scripts validated by shellcheck |
| `.codecov.yml` | Codecov integration | Component-level coverage thresholds |
| `hack/ci-dump-diagnostics.sh` | `e2e-infra`, `e2e-operator`, `e2e-chaos`, `tempest` jobs | Shared diagnostic dump |
| `hack/ci-build-service-image.sh` | `e2e-operator`, `e2e-chaos`, `tempest` jobs | Builds OpenStack service images |
| `hack/ci-deploy-operator.sh` | `e2e-operator`, `e2e-chaos`, `tempest` jobs | Deploys operator via Helm |
| `hack/ci-run-tempest.sh` | `tempest` job | Runs Tempest API tests |
| `.github/actions/setup-test-deps/` | `chainsaw-lint` job, `setup-e2e-infra` composite action | Composite action for testdeps cache + `make install-test-deps` |
| `.github/actions/setup-e2e-infra/` | `e2e-infra`, `e2e-operator`, `e2e-chaos`, `tempest` jobs | Composite action for infra setup |
| `.github/actions/load-e2e-images/` | `e2e-operator`, `e2e-chaos`, `tempest` jobs | Composite action that pulls run-scoped GHCR tags and re-tags them to canonical local refs (GH-310) |
| `.github/actions/cleanup-ghcr-package/` | `cleanup-e2e-tags` job, `cleanup-images.yaml` | Wraps `dataaxiom/ghcr-cleanup-action` for delete-by-pattern and delete-by-exclusion modes |
| `tests/e2e-chaos/chainsaw-config.yaml` | `e2e-chaos` job | Chaos-specific Chainsaw configuration |

:::
