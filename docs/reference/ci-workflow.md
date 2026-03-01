---
title: CI Workflow
quadrant: infrastructure
---

# CI Workflow

## File Location

`.github/workflows/ci.yaml`

## Trigger Events

| Event          | Condition              |
|----------------|------------------------|
| `push`         | `branches: [main]`     |
| `pull_request` | all PR events (default)|

The workflow runs on every push to `main` and on all pull request events (opened, synchronized, reopened).

## Jobs

Both jobs run **in parallel** with no inter-job dependencies (`needs:` is absent on every job).

### lint

Runs `golangci-lint` against the Go workspace root.

| Step | Action                                |
|------|---------------------------------------|
| 1    | `actions/checkout@v4.3.1`            |
| 2    | `golangci/golangci-lint-action@v9.2.0`|

- **golangci-lint version:** `v2.10` (pinned via `version` input)
- **timeout-minutes:** `10`
- **No separate `actions/setup-go` step** — the lint action handles Go setup internally
- Uses `.golangci.yml` from the repository root
- All actions pinned to commit SHAs for supply-chain security

### test

Runs unit tests via `make test`.

| Step | Action / Command           |
|------|----------------------------|
| 1    | `actions/checkout@v4.3.1`  |
| 2    | `actions/setup-go@v5.6.0`  |
| 3    | `make test`                |

- **timeout-minutes:** `15`
- All actions pinned to commit SHAs for supply-chain security

## Go Setup Convention

The `test` job uses `actions/setup-go@v5.6.0` with:

- **`go-version-file: go.work`** — reads the Go version from the workspace file instead of hardcoding a version string
- **Module caching** — enabled by default (`cache: true` is the setup-go default); caches `~/go/pkg/mod` between runs

## Concurrency

```yaml
concurrency:
  group: ${{ github.ref }}-${{ github.workflow }}
  cancel-in-progress: true
```

- Scoped per-branch per-workflow: pushes to different branches do not cancel each other
- New pushes to the same branch cancel any in-progress run
- Matches the pattern used in `mega-linter.yml`

## Permissions

```yaml
permissions:
  contents: read
```

Top-level least-privilege: the `GITHUB_TOKEN` has read-only access to repository contents. No job-level overrides.

## Dependencies

- **CC-0001** — provides the Go workspace (`go.work`), Makefile targets (`test`, `lint`), and `.golangci.yml`
