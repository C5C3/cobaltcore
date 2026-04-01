# Review Pattern: New helper functions must follow sibling patterns in the same package

**Review-Area**: architecture
**Detection-Hint**: When a PR adds a new Ensure/Reconcile function alongside existing ones in the same file or package, diff the structure of the new function against its siblings. Look for behaviors present in one but not the others (e.g., owner ref reconciliation, DeepEqual guards, metadata merging).
**Severity**: WARNING
**Occurrences**: 1

## What to check

Compare the new function's reconciliation strategy (create-or-update logic, metadata handling, equality checks before update) against existing sibling functions in the same package. Flag any behavioral divergence and ask whether it should be backported or justified.

## Why it matters

Inconsistent patterns within a single package create divergent expectations for developers, increase cognitive load, and risk bugs when someone copies the older, less robust pattern assuming all siblings behave the same way.

## Examples from external reviews

### CC-0037 — berendt
- **Feedback**: EnsurePDB introduces owner ref reconciliation, label/annotation merging on update, and DeepEqual guard before Update — behaviors that EnsureDeployment and EnsureService in the same file do not have.
- **What was missed**: Compare the new function's reconciliation strategy (create-or-update logic, metadata handling, equality checks before update) against existing sibling functions in the same package. Flag any behavioral divergence and ask whether it should be backported or justified.
- **Fix**: Track the improved pattern as a follow-up to backport to EnsureDeployment and EnsureService.
