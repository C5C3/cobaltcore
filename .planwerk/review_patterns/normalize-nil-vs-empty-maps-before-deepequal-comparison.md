# Review Pattern: Normalize nil vs empty maps before DeepEqual comparison

**Review-Area**: performance
**Detection-Hint**: When reviewing update-detection logic that uses `DeepEqual` on map fields (Labels, Annotations), check whether earlier code paths can convert a nil map to an empty map (e.g., via `make(map[string]string)`). If so, `DeepEqual(nil, map[]{})` returns false, causing spurious updates.
**Severity**: WARNING
**Occurrences**: 1

## What to check

In reconcile functions that merge maps into an existing object and then compare with a `before` snapshot, verify that nil-vs-empty-map differences cannot cause `DeepEqual` to report a false diff. Either only allocate maps when there are entries to merge, or normalize both sides before comparison.

## Why it matters

Spurious API updates generate unnecessary writes, events, and potential watch notifications across the cluster, increasing API server load and confusing debugging/audit trails with no-op updates.

## Examples from external reviews

### CC-0038 — sourcery-ai[bot]
- **Feedback**: Because `before.Labels`/`Annotations` can be nil while `existing.Labels`/`Annotations` are set to an empty map, `DeepEqual` will see them as different and trigger an Update even when there is no semantic change for Kubernetes.
- **What was missed**: In reconcile functions that merge maps into an existing object and then compare with a `before` snapshot, verify that nil-vs-empty-map differences cannot cause `DeepEqual` to report a false diff. Either only allocate maps when there are entries to merge, or normalize both sides before comparison.
- **Fix**: Added a `normalizeMap` helper that converts empty maps to nil before comparison, so `DeepEqual` only detects real semantic changes.
