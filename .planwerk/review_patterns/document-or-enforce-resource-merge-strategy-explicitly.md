# Review Pattern: Document or enforce resource merge strategy explicitly

**Review-Area**: architecture
**Detection-Hint**: In any ensure/update function that patches an existing Kubernetes resource, check how Spec, labels, and annotations are merged. Look for asymmetric merge logic (e.g., Spec fully overwritten but metadata additively merged) that is not documented or tested.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Verify that the merge strategy for each resource field (Spec, labels, annotations, finalizers) is intentional and documented. Specifically check: (1) are removed labels/annotations cleaned up or silently preserved? (2) is the Spec fully replaced or patched? (3) are user-added fields on the live object preserved or dropped? If the operator is meant to be authoritative, the strategy should match that intent.

## Why it matters

An undocumented merge strategy creates silent drift: users or other controllers may add labels or annotations that are never cleaned up, or expect spec fields to persist across reconciliations when they are actually overwritten. This leads to confusing behavior and support burden when the operator does not behave as consumers expect.

## Examples from external reviews

### CC-0039 — sourcery-ai[bot]
- **Feedback**: In ensureNetworkPolicy you overwrite the existing NetworkPolicy.Spec and merge labels/annotations only in one direction (desired -> existing), which means removed labels/annotations and any user-added spec fields will be silently preserved or dropped; if you want the operator to be truly authoritative, it may be worth explicitly documenting or adjusting this merge strategy so consumers understand which mutations are allowed to stick.
- **What was missed**: Verify that the merge strategy for each resource field (Spec, labels, annotations, finalizers) is intentional and documented. Specifically check: (1) are removed labels/annotations cleaned up or silently preserved? (2) is the Spec fully replaced or patched? (3) are user-added fields on the live object preserved or dropped? If the operator is meant to be authoritative, the strategy should match that intent.
- **Fix**: Added an explicit doc comment on ensureNetworkPolicy (lines 199-207) documenting the merge strategy: Spec is fully overwritten to match desired state; labels and annotations are additively merged, preserving any user-added keys.
