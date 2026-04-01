# Review Pattern: Avoid unconditional updates in reconcile loops

**Review-Area**: performance
**Detection-Hint**: In any reconcile function that calls `client.Update()`, check whether there is a comparison (e.g. `reflect.DeepEqual` or `DeepCopy`-and-compare) guarding the update call. If `Update` is called unconditionally every reconcile, flag it. Search the codebase for existing patterns (e.g. grep for `DeepEqual` or `DeepCopy` in other Ensure functions) and verify the new code follows the same approach.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Verify that the update path compares existing state to desired state before issuing an API write. Unconditional updates on every reconcile cause unnecessary API server load, trigger spurious watch events, and can cause cascading reconciliations in other controllers.

## Why it matters

Kubernetes controllers reconcile frequently. Unconditional writes generate unnecessary API traffic, inflate resource versions, and trigger watch notifications that can cascade into other controllers re-reconciling, degrading cluster performance at scale.

## Examples from external reviews

### CC-0037 — sourcery-ai[bot]
- **Feedback**: Always issuing an Update on the PDB even when the spec is unchanged may cause unnecessary API churn. Consider comparing specs (e.g. `reflect.DeepEqual(existing.Spec, pdb.Spec)`) and only calling `Update` when they differ.
- **What was missed**: Verify that the update path compares existing state to desired state before issuing an API write. Unconditional updates on every reconcile cause unnecessary API server load, trigger spurious watch events, and can cause cascading reconciliations in other controllers.
- **Fix**: Wrapped the `Update` call in `if !reflect.DeepEqual(existing.Spec, pdb.Spec)` to skip no-op updates.
