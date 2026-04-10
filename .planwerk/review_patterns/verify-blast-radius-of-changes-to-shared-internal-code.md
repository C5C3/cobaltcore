# Review Pattern: Verify blast radius of changes to shared internal code

**Review-Area**: architecture
**Detection-Hint**: If the PR modifies files under shared paths like internal/common/, check who else calls the changed function. A quick grep for callers across the monorepo reveals the impact scope.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Grep for all callers of the modified function across the entire monorepo. Document whether the behavioral change is isolated to the current operator or affects others. If isolated, add a comment stating so; if not, require explicit sign-off from affected teams.

## Why it matters

Shared infrastructure changes can have unintended side effects on other operators in a monorepo. Even when the change is correct, the reviewer must confirm the scope is understood and documented to prevent silent regressions.

## Examples from external reviews

### CC-0056 — berendt
- **Feedback**: The change to RunJob sets explicit DeletePropagationBackground for Job deletion. This is in shared infrastructure (internal/common/job/job.go) and affects all operators in the monorepo, not just keystone.
- **What was missed**: Grep for all callers of the modified function across the entire monorepo. Document whether the behavioral change is isolated to the current operator or affects others. If isolated, add a comment stating so; if not, require explicit sign-off from affected teams.
- **Fix**: Expanded the inline comment at internal/common/job/job.go:75-81 to explicitly acknowledge monorepo scope, document that it is a no-op in production, and note that only keystone's reconcile_database.go calls RunJob.
