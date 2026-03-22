# Review Pattern: Use exact-match assertions instead of loose comparisons

**Review-Area**: testing
**Detection-Hint**: Look for inequality operators (`>=`, `<=`, `!=`) in test assertions and for unanchored grep/regex patterns. Ask: could this assertion pass when the system is in a WRONG state?
**Severity**: WARNING
**Occurrences**: 1

## What to check

For assertions on countable values (replica counts, resource counts), prefer exact equality (`== N`) over range checks (`>= N`) unless the range is intentional. For string matching, ensure patterns are anchored (e.g., `grep '2025\.2$'` not `grep '2025.2'`) to prevent substring false positives.

## Why it matters

A scale-down test asserting `availableReplicas >= 2` passes even if scale-down never happened. An unanchored grep matching `2025.2` also matches `2025.2-upgraded` or `2025.20`. Imprecise assertions silently mask real failures.

## Examples from external reviews

### CC-0016 — berendt
- **Feedback**: W-003: scale-down assertion uses `availableReplicas >= 2` instead of exact match. W-004: grep substring match on version tag.
- **What was missed**: For assertions on countable values (replica counts, resource counts), prefer exact equality (`== N`) over range checks (`>= N`) unless the range is intentional. For string matching, ensure patterns are anchored (e.g., `grep '2025\.2$'` not `grep '2025.2'`) to prevent substring false positives.
- **Fix**: Changed `availableReplicas >= 2` to exact `availableReplicas: 2`. Anchored grep with `grep '2025\.2$'`.
