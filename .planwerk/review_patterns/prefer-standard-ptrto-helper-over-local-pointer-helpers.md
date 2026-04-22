# Review Pattern: Prefer standard ptr.To helper over local pointer helpers

**Review-Area**: architecture
**Detection-Hint**: Grep for locally-defined ptrInt32/ptrInt64/ptrString/ptrBool helpers and flag if k8s.io/utils/ptr is already available in the module.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When PRs introduce or use local ptrXxx helper functions, check whether k8s.io/utils/ptr.To is already imported/usable in the codebase and recommend replacing the local helpers with the standard generic helper.

## Why it matters

Local pointer helpers duplicate a well-known standard utility, proliferate per-type variants, and drift from codebase conventions. Using ptr.To keeps pointer construction uniform and discoverable.

## Examples from external reviews

### CC-0084 — berendt
- **Feedback**: The local ptrInt32/ptrInt64 helpers were removed and all call sites now use k8s.io/utils/ptr.To.
- **What was missed**: When PRs introduce or use local ptrXxx helper functions, check whether k8s.io/utils/ptr.To is already imported/usable in the codebase and recommend replacing the local helpers with the standard generic helper.
- **Fix**: Removed local ptrInt32/ptrInt64 helpers and migrated call sites to k8s.io/utils/ptr.To.
