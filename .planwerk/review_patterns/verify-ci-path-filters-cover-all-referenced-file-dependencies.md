# Review Pattern: Verify CI path filters cover all referenced file dependencies

**Review-Area**: testing
**Detection-Hint**: For each CI job, trace all file references (e.g., `uses: ./.github/actions/...`, script paths) and confirm every referenced directory appears in the job's path filter list.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When a workflow job uses composite actions or scripts from specific directories, check that those directories are included in the path-filter trigger so that changes to dependencies actually trigger the job.

## Why it matters

A missing path filter means changes to a shared composite action or script will not trigger the tests that depend on it, allowing broken changes to pass CI silently.

## Examples from external reviews

### CC-0054 — berendt
- **Feedback**: The `e2e_chaos` path filter does not include `.github/actions/**`. The `e2e-chaos` job references `./.github/actions/setup-e2e-infra` (ci.yaml:493). A change to the composite action will not trigger chaos E2E tests.
- **What was missed**: When a workflow job uses composite actions or scripts from specific directories, check that those directories are included in the path-filter trigger so that changes to dependencies actually trigger the job.
- **Fix**: Add `- '.github/actions/**'` to the `e2e_chaos` path filter list.
