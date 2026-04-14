# Review Pattern: Use existing constants instead of duplicating values as inline literals

**Review-Area**: naming
**Detection-Hint**: When a file already defines typed constants (e.g., conditionReason*), check whether new code in the same package passes the same string values as raw literals to a different API (e.g., Recorder.Eventf vs status conditions). Grep for the constant's string value appearing as a literal elsewhere.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Verify that event reason strings, status condition reasons, and annotation keys use the constants already defined in the package rather than duplicating the same value as a string literal. Check both the call site under review and adjacent files that may have been added in the same PR.

## Why it matters

Inline literals that shadow existing constants diverge silently when the constant is renamed or its value is updated. It also obscures the intended semantic link between status conditions and emitted events, making audits harder.

## Examples from external reviews

### CC-0070 — berendt
- **Feedback**: The event reason strings passed to r.Recorder.Eventf are raw string literals duplicating the same values that already exist as conditionReason* constants (defined at lines 32-46).
- **What was missed**: Verify that event reason strings, status condition reasons, and annotation keys use the constants already defined in the package rather than duplicating the same value as a string literal. Check both the call site under review and adjacent files that may have been added in the same PR.
- **Fix**: Replaced inline event reason string literals in reconcile_database.go with conditionReason* constants and added 4 new constants for reasons that had no constant yet.
