# Review Pattern: Verify CI workflows work with fork PRs

**Review-Area**: security
**Detection-Hint**: When a workflow triggers on `pull_request` and uses `push: true` or writes to external resources (registry, deployment), check whether `GITHUB_TOKEN` permissions are sufficient for fork PRs (where the token is always read-only).
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Any workflow step that pushes to a registry, writes to packages, or performs write operations on `pull_request` triggers. Fork PRs get a read-only GITHUB_TOKEN regardless of declared permissions, so these steps will fail with 403.

## Why it matters

External contributors opening fork PRs will hit a hard CI failure mid-build with a cryptic 403 error, creating a broken contributor experience and wasting CI minutes on doomed runs.

## Examples from external reviews

### CC-0007 — greptile-apps[bot]
- **Feedback**: The `build-base-images` job unconditionally pushes to GHCR on every trigger, including `pull_request` events. For PRs opened from forks, GitHub Actions automatically restricts `GITHUB_TOKEN` to read-only, regardless of the `packages: write` permission declared in the job. The `docker push` step will fail with a 403, breaking CI for any external contributor opening a fork PR.
- **What was missed**: Any workflow step that pushes to a registry, writes to packages, or performs write operations on `pull_request` triggers. Fork PRs get a read-only GITHUB_TOKEN regardless of declared permissions, so these steps will fail with 403.
- **Fix**: Added a workflow condition to fail fast with a clear error message for fork PRs rather than failing mid-build.
