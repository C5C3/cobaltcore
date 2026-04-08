# Review Pattern: Check new resources for all standard fields present in siblings

**Review-Area**: validation
**Detection-Hint**: When a new resource of the same kind is added (e.g., a new HelmRelease alongside 8 existing ones), diff its spec fields against siblings. Any field present in all existing siblings but absent in the new one is a red flag.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Does the new resource include every field that is consistently present across all existing sibling resources? For example, if all 8 existing HelmReleases set `install.createNamespace: true`, a new HelmRelease missing it is likely an omission, not an intentional choice.

## Why it matters

Omitting a field that every sibling has removes a safety net (in this case, namespace auto-creation during FluxCD reconciliation edge cases) and creates an inconsistency that makes the fleet harder to reason about and operate.

## Examples from external reviews

### CC-0046 — berendt
- **Feedback**: All 8 other HelmReleases set `install.createNamespace: true`. [...] Omitting it removes a safety net every sibling release has.
- **What was missed**: Does the new resource include every field that is consistently present across all existing sibling resources? For example, if all 8 existing HelmReleases set `install.createNamespace: true`, a new HelmRelease missing it is likely an omission, not an intentional choice.
- **Fix**: Added `createNamespace: true` to the install block to match all sibling HelmReleases.
