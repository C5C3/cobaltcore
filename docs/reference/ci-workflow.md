---
title: CI Workflow
quadrant: infrastructure
feature: CC-0003
---

# CI Workflow

Reference documentation for the GitHub Actions CI workflow (CC-0003).

## File Location

`.github/workflows/ci.yaml`

The file uses the `.yaml` extension (matching `reuse.yaml` and `deploy-docs.yaml`) and
quotes the trigger key as `"on"` to prevent YAML boolean interpretation (REQ-001).

## Trigger Events

The workflow triggers on two events (REQ-008):

| Event | Scope | Description |
| --- | --- | --- |
| `push` | `branches: [main]` | Runs on every push to the main branch |
| `pull_request` | `branches: [main]` | Runs on every pull request targeting main |

## Permissions

Top-level permissions are restricted to least privilege (REQ-007):

```yaml
permissions:
  contents: read
```

No individual job defines a `permissions:` block. The top-level setting applies to all
jobs uniformly.

## Jobs

The workflow defines two active jobs (`lint` and `test`), which run in parallel — neither
declares a `needs:` key. A third job (`test-integration`) is currently disabled pending
envtest readiness (see [Disabled Jobs](#disabled-jobs)).

### lint

Runs golangci-lint using the project's `.golangci.yml` configuration (REQ-002).

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v4` | Checks out the repository |
| 2 | `actions/setup-go@v5` | Sets up Go with `go-version-file: go.work` |
| 3 | `golangci/golangci-lint-action@v9` | Installs golangci-lint binary (`install-only: true`); version pinned to `v2.10` |
| 4 | `make lint` | Runs golangci-lint per module via the Makefile |

The `golangci-lint-action@v9` step is used with `install-only: true`, which installs the
pinned golangci-lint binary (and caches it) without running lint. The actual linting is
delegated to `make lint`, which `cd`s into each module directory and runs
`golangci-lint run ./...` — a necessary pattern for Go multi-module workspaces. The
`actions/setup-go@v5` step is required because `install-only` mode does not set up Go
internally.

### test

Runs unit tests via the Makefile `test` target (REQ-003).

| Step | Action | Details |
| --- | --- | --- |
| 1 | `actions/checkout@v4` | Checks out the repository |
| 2 | `actions/setup-go@v5` | Sets up Go with `go-version-file: go.work` |
| 3 | `make test` | Executes unit tests across all operator modules |

### Disabled Jobs

#### test-integration (disabled)

The `test-integration` job is currently commented out in the workflow file. It will be
re-enabled when `make test-integration` runs real envtest-based tests (REQ-004). The
planned configuration mirrors the `test` job with `make test-integration` as the final
step.

## Go Setup Convention

Both active jobs use `actions/setup-go@v5` with (REQ-005):

```yaml
go-version-file: go.work
```

This reads the Go version from `go.work` (currently Go 1.25.0) rather than hardcoding a
`go-version` value. The repository root contains `go.work` (not `go.mod`) because the
project uses a Go Workspace with multiple modules (`internal/common`, `operators/keystone`,
`operators/c5c3`). Module dependency caching is enabled by default in `actions/setup-go@v5`.

## Concurrency

The workflow uses a concurrency group scoped per-branch per-workflow (REQ-006):

```yaml
concurrency:
  group: ${{ github.ref }}-${{ github.workflow }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
```

The concurrency group is scoped per-branch per-workflow. For pull requests, pushing new
commits cancels any in-progress CI run for that same PR branch, preventing wasted CI
resources on outdated code. For pushes to `main`, in-progress runs are **not** cancelled,
ensuring every merge commit is fully validated. Different branches do not cancel each
other's runs.

## SPDX Header

The file starts with the standard SPDX license header (REQ-001):

```text
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
---
```

This follows the `deploy-docs.yaml` convention (Copyright 2026).

## Dependencies on CC-0001

The CI workflow depends on artifacts introduced by CC-0001:

| Artifact | Used by | Purpose |
| --- | --- | --- |
| `Makefile` (`lint` target) | `lint` job | Iterates over `OPERATORS` variable to run golangci-lint per module |
| `Makefile` (`test` target) | `test` job | Iterates over `OPERATORS` variable to run unit tests across all modules |
| `Makefile` (`test-integration` target) | `test-integration` job (disabled) | Will iterate over `OPERATORS` variable to run envtest integration tests |
| `.golangci.yml` | `lint` job | Provides linter configuration (enabled linters, exclusion rules, timeout) |
| `go.work` | Both active jobs | Provides the Go version for `actions/setup-go@v5` |

The CI workflow will not pass until CC-0001 is merged to `main`, as it depends on both
the Makefile targets and the Go workspace configuration.
