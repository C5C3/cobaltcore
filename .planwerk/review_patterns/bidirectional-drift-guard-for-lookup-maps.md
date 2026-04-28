# Review Pattern: Bidirectional drift guard for lookup maps

**Review-Area**: testing
**Detection-Hint**: When a test asserts that a map's keys/values are a subset of some set, check whether the reverse direction is also enforced. Look for fallback expressions like `m[name]` used at call sites where a missing key would silently produce a zero value.
**Severity**: WARNING
**Occurrences**: 1

## What to check

For any lookup map used to enrich logs, metrics, or labels, verify there is a drift guard test in BOTH directions: (a) map entries are valid, AND (b) every call site that indexes into the map has a corresponding key. A subset-only test leaves the other direction unguarded.

## Why it matters

One-directional drift guards give a false sense of safety. A contributor adding a new call site without updating the map causes silent emission of empty/zero-valued labels, degrading alerting and observability without any test failure.

## Examples from external reviews

### CC-0089 — berendt
- **Feedback**: The drift guard TestSubReconcilerConditionTypesCoversAllNames only enforces that map values are a subset of subConditionTypes. There is no test asserting that every instrumentSubReconciler call site in Reconcile has a corresponding key in subReconcilerConditionTypes. The fallback at line 85 silently uses an empty condition_type label.
- **What was missed**: For any lookup map used to enrich logs, metrics, or labels, verify there is a drift guard test in BOTH directions: (a) map entries are valid, AND (b) every call site that indexes into the map has a corresponding key. A subset-only test leaves the other direction unguarded.
- **Fix**: Added TestSubReconcilerConditionTypesCoversAllCallSites which walks the package AST to verify every instrumentSubReconciler call literal and every parallelSubReconciler.name field has a corresponding key in subReconcilerConditionTypes.
