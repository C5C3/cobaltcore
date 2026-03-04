<!--
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
-->

---
title: CI Workflow
quadrant: implementation
feature: CC-0003
---

# CI Workflow

Reference documentation for the GitHub Actions CI workflow at
`.github/workflows/ci.yaml` (CC-0003).

## Trigger Events

The workflow runs on two event types:

| Event | Scope | Purpose |
| --- | --- | --- |
| `push` | `branches: [main]` | Validate code after merge to the default branch |
| `pull_request` | all branches | Gate pull requests before merge |

The `"on"` key is quoted in the YAML source to prevent YAML boolean
interpretation of the bare word `on`.

## Jobs

All three jobs run **in parallel** (no `needs:` dependencies). Each job runs
on `ubuntu-latest`.

### lint

Runs golangci-lint using the repository's `.golangci.yml` configuration
(created in CC-0001).

| Step | Action |
| --- | --- |
| Checkout | `actions/checkout@<sha>` (v4.3.1) |
| Lint | `golangci/golangci-lint-action@<sha>` (v9.2.0) with `version: v2.10` |

All actions are pinned to full commit SHAs for supply-chain security, with
the human-readable version in a trailing comment (e.g., `# v4.3.1`).
Dependabot or Renovate can update these pins automatically.

The golangci-lint-action handles Go installation internally, so no separate
`actions/setup-go` step is needed. The pinned version `v2.10` matches the
version specified in `architecture/docs/09-implementation/07-ci-cd-and-packaging.md`.

### test

Runs unit tests via the Makefile `test` target (created in CC-0001).

| Step | Action |
| --- | --- |
| Checkout | `actions/checkout@<sha>` (v4.3.1) |
| Setup Go | `actions/setup-go@<sha>` (v5.6.0) with `go-version-file: go.work` |
| Run unit tests | `make test` |

### test-integration

Runs envtest-based integration tests via the Makefile `test-integration`
target (created in CC-0001).

| Step | Action |
| --- | --- |
| Checkout | `actions/checkout@<sha>` (v4.3.1) |
| Setup Go | `actions/setup-go@<sha>` (v5.6.0) with `go-version-file: go.work` |
| Run integration tests | `make test-integration` |

## Go Setup Convention

Both the `test` and `test-integration` jobs use `go-version-file: go.work`
instead of a hardcoded `go-version` value. This reads the Go version from the
workspace file at the repository root, ensuring CI always uses the same Go
version declared in the project. The `actions/setup-go` action (v5.6.0) enables
module caching by default when `go-version-file` is set.

## Concurrency

```yaml
concurrency:
  group: ${{ github.ref }}-${{ github.workflow }}
  cancel-in-progress: true
```

The concurrency group is scoped per-branch and per-workflow. Pushing a new
commit to a branch cancels any in-progress CI run for that same branch,
preventing wasted resources on outdated code. Different branches do not
interfere with each other. This pattern matches the convention planned for `mega-linter.yml`.

## Permissions

```yaml
permissions:
  contents: read
```

The workflow requests only `contents: read` at the top level (least-privilege).
No individual job overrides this setting. This follows the convention
planned for `reuse.yaml`.

## File Conventions

| Convention | Value | Rationale |
| --- | --- | --- |
| Extension | `.yaml` | Matches planned `reuse.yaml` and `deploy-docs.yaml` |
| SPDX header | `Copyright 2026 SAP SE or an SAP affiliate company`, `Apache-2.0` | Matches planned `deploy-docs.yaml` (current year) |
| Trigger quoting | `"on"` | Prevents YAML boolean interpretation |

> **Note:** The workflow files `reuse.yaml`, `deploy-docs.yaml`, and
> `mega-linter.yml` referenced above do not yet exist on `main`. They are
> planned conventions documented here as design intent for future workflows.

## Dependencies on CC-0001

The CI workflow depends on artifacts created by CC-0001:

| Artifact | Used by | Purpose |
| --- | --- | --- |
| `go.work` | `test`, `test-integration` jobs | Go version source for `actions/setup-go` |
| `Makefile` (`test` target) | `test` job | Runs unit tests across all modules |
| `Makefile` (`test-integration` target) | `test-integration` job | Runs envtest integration tests |
| `.golangci.yml` | `lint` job | Linter configuration consumed by `golangci-lint-action` |

> **Note:** The `test-integration` Makefile target is currently a stub
> (`$(error)`) in CC-0001. The CI workflow will not pass for integration tests
> until a future feature implements that target. This is expected and documented
> in the feature's dependency list.
