# Review Pattern: Log non-NotFound errors on fallthrough paths

**Review-Area**: error-handling
**Detection-Hint**: Look for error branches that intentionally fall through (e.g., on transient errors) without logging, especially when sibling branches in the same function do log.
**Severity**: WARNING
**Occurrences**: 1

## What to check

In mapper/handler functions that enqueue on error, check whether non-NotFound (or other non-terminal) errors are logged before falling through. Compare against sibling branches for consistency.

## Why it matters

Silent fallthrough on cache/informer errors produces zero operational signal, making unhealthy informer caches, unregistered GVKs, or mid-sync states invisible during incident debugging.

## Examples from external reviews

### CC-0087 — berendt
- **Feedback**: The owner-ref Get path intentionally falls through to enqueue on non-NotFound errors, but unlike the indexed-List branch at line 603 which logs on error, this path produces zero operational signal.
- **What was missed**: In mapper/handler functions that enqueue on error, check whether non-NotFound (or other non-terminal) errors are logged before falling through. Compare against sibling branches for consistency.
- **Fix**: Added a V(1) log including secret key, ownerRef key, and error before falling through to enqueue when Get returns a non-NotFound error.
