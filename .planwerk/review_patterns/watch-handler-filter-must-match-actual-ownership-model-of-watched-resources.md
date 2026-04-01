# Review Pattern: Watch handler filter must match actual ownership model of watched resources

**Review-Area**: architecture
**Detection-Hint**: When Watches() is declared with EnqueueRequestForOwner, trace the ownership chain of the watched resource. Ask: who actually creates and owns this object? Does its ownerReferences point to our CR type, or to a third-party controller (e.g., ExternalSecret, Helm, ArgoCD)?
**Severity**: BLOCKING
**Occurrences**: 2

## What to check

For each Watches() call using handler.EnqueueRequestForOwner, verify that the watched objects will actually have ownerReferences[].kind matching the controller's CR type. If the objects are created by an external controller (ESO, cert-manager, etc.), EnqueueRequestForOwner will never fire and the watch is dead code. Use EnqueueRequestsFromMapFunc with a lookup instead.

## Why it matters

A non-functional watch gives the false impression of event-driven reconciliation while actually relying on polling requeues. This masks latency issues, makes the code misleading, and can cause delayed reactions to critical secret changes (e.g., credential rotation).

## Examples from external reviews

### CC-0013 — greptile-apps[bot]
- **Feedback**: ESO-managed secrets (`spec.database.secretRef`, `spec.bootstrap.adminPasswordSecretRef`) are created and owned by the `ExternalSecret` controller — their `ownerReferences[].kind` is `ExternalSecret`, not `Keystone`. So these secrets will never match the owner filter, and the corresponding Keystone reconcile will never be enqueued reactively.
- **What was missed**: For each Watches() call using handler.EnqueueRequestForOwner, verify that the watched objects will actually have ownerReferences[].kind matching the controller's CR type. If the objects are created by an external controller (ESO, cert-manager, etc.), EnqueueRequestForOwner will never fire and the watch is dead code. Use EnqueueRequestsFromMapFunc with a lookup instead.
- **Fix**: The Secret watch was replaced with EnqueueRequestsFromMapFunc using a namespace-scoped lookup of Keystone CRs that reference the changed Secret.

### CC-0037 — sourcery-ai[bot]
- **Feedback**: `EnsurePDB` only sets the controller owner reference on create; if the owner ref is removed or changed out-of-band, it will never be restored, so you may want to enforce/refresh the owner reference on the update path as well.
- **What was missed**: In any create-or-update reconciliation function, verify that owner references, labels, and annotations are enforced on the update path, not just the create path. If out-of-band changes remove the owner reference, the object becomes orphaned and will not be garbage-collected.
- **Fix**: Added `controllerutil.SetControllerReference(owner, existing, scheme)` on the update path, plus merging desired labels and annotations into the existing object before updating.
