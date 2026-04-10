# Review Pattern: Centralize duplicated status condition strings

**Review-Area**: architecture
**Detection-Hint**: Search the PR diff for repeated calls to SetCondition (or equivalent status-update functions). If the same reason or message string literals appear in more than two call sites, flag for centralization.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Count inline SetCondition/SetStatusCondition calls across the PR. If reason/message strings are duplicated across multiple functions (e.g. reconcileExpand, reconcileMigrate, reconcileContract), they should be consolidated into small helpers.

## Why it matters

Scattered condition-setting with duplicated string literals leads to inconsistent user-facing status messages when one call site is updated but others are missed, making debugging and operator UX harder.

## Examples from external reviews

### CC-0056 — sourcery-ai[bot]
- **Feedback**: Upgrade-related condition setting for `DatabaseReady` and `DeploymentReady` is currently spread across multiple functions with duplicated reason/message strings; centralizing this into small helpers (e.g., `setUpgradeCondition(phase, status, err)`) would make it easier to keep messages consistent.
- **What was missed**: Count inline SetCondition/SetStatusCondition calls across the PR. If reason/message strings are duplicated across multiple functions (e.g. reconcileExpand, reconcileMigrate, reconcileContract), they should be consolidated into small helpers.
- **Fix**: Introduced `setUpgradePhaseRunning` and `setUpgradeJobFailed` helpers that replaced six inline SetCondition calls across reconcileExpand, reconcileMigrate, and reconcileContract.
