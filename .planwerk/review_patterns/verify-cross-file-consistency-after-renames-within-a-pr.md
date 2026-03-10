# Review Pattern: Verify cross-file consistency after renames within a PR

**Review-Area**: documentation
**Detection-Hint**: When a PR contains a rename (function, test, variable, type), search the entire PR diff for all occurrences of the old name. Documentation files, comments, and string literals are the most commonly missed.
**Severity**: WARNING
**Occurrences**: 1

## What to check

After any identifier rename in a PR, grep the full changeset for the old name to ensure no stale references remain — especially in markdown docs, tables, comments, and generated reference files.

## Why it matters

Stale references in documentation mislead readers and erode trust in docs accuracy. When a rename is done to address one review comment but docs aren't updated, it signals the change was applied mechanically without checking for ripple effects.

## Examples from external reviews

### CC-0012 — greptile-apps[bot]
- **Feedback**: The table entry references `TestIntegration_CELRejectsReplicasZero`, but this test was renamed to `TestIntegration_CELRejectsReplicasBelowMinimum` in `operators/keystone/api/v1alpha1/integration_test.go` as part of this very PR. The documentation wasn't updated to match.
- **What was missed**: After any identifier rename in a PR, grep the full changeset for the old name to ensure no stale references remain — especially in markdown docs, tables, comments, and generated reference files.
- **Fix**: Updated the documentation table in `docs/reference/keystone-crd.md` to use the new test name `TestIntegration_CELRejectsReplicasBelowMinimum` and adjusted the description to reflect the actual test behavior (using `-1` instead of `0`).
