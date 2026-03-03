<!--
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
-->

# CI Workflow Reference (CC-0003)

**File:** `.github/workflows/ci.yaml`

## Trigger Events

| Event | Condition |
|---|---|
| `push` | Branches: `main` only |
| `pull_request` | All pull requests (any branch) |

## Permissions

`contents: read` — read-only access to repository contents. Follows principle of least privilege; no write permissions granted.

## Concurrency

- **Group key:** `${{ github.ref }}-${{ github.workflow }}`
- **Behavior:** `cancel-in-progress: true` cancels any in-progress run for the same ref and workflow when a new run starts, preventing redundant runs on rapid pushes or PR updates.

## Jobs

| Job | Runner | Purpose |
|---|---|---|
| `lint` | `ubuntu-latest` | Runs Go linter via golangci-lint |
| `test` | `ubuntu-latest` | Runs unit tests |
| `test-integration` | `ubuntu-latest` | Runs integration tests |

### lint

Steps:
1. `actions/checkout@v4` — checks out the repository
2. `golangci/golangci-lint-action@v9` — runs golangci-lint at version `v2.10`

### test

Steps:
1. `actions/checkout@v4` — checks out the repository
2. `actions/setup-go@v5` — installs Go using `go-version-file: go.work`
3. `make test` — runs unit tests via Makefile target

### test-integration

Steps:
1. `actions/checkout@v4` — checks out the repository
2. `actions/setup-go@v5` — installs Go using `go-version-file: go.work`
3. `make test-integration` — runs integration tests via Makefile target

## Go Setup Convention

Both `test` and `test-integration` jobs use:

```yaml
- uses: actions/setup-go@v5
  with:
    go-version-file: go.work
```

The Go version is read from the `go.work` workspace file. No hardcoded version in the workflow. Module caching is enabled by default (`cache: true` is the default in `actions/setup-go`).

## Dependencies

| Job | Depends On | Source |
|---|---|---|
| `lint` | `.golangci.yml` configuration file | CC-0001 |
| `test` | Makefile `test` target | CC-0001 |
| `test-integration` | Makefile `test-integration` target | CC-0001 |
