# Pattern: Missing-CRD safety via meta.IsNoMatchError

**Component**: operators/c5c3/internal/controller
**Category**: error-handling
**Applies-When**: A sub-reconciler create-or-updates a CR whose CRD is owned by a third-party operator that may not be installed yet (e.g. K-ORC ApplicationCredential/Service/Endpoint)

## Description

When create-or-update of a third-party CR fails, the error is checked with meta.IsNoMatchError. On a no-match (CRD absent) the sub-reconciler logs, sets its *Ready condition to False with a stable Reason ('KORCCRDNotInstalled') and a short RequeueAfter, and returns nil — so a missing optional CRD degrades to a clear condition instead of crash-looping the operator. Used consistently across the K-ORC mint and catalog phases; future operators that own optional third-party CRDs should follow the same shape.

## Examples

### `operators/c5c3/internal/controller/reconcile_korc.go:186-196`

```go
if meta.IsNoMatchError(err) {
	logger.Info("K-ORC ApplicationCredential CRD not installed; KORCReady=False")
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeKORCReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cp.Generation,
		Reason:             "KORCCRDNotInstalled",
		Message:            "K-ORC ApplicationCredential CRD is not installed",
	})
	return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
}
```

### `operators/c5c3/internal/controller/reconcile_korc.go:610-620`

```go
func (r *ControlPlaneReconciler) catalogCRDMissing(cp *c5c3v1alpha1.ControlPlane, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("K-ORC Service/Endpoint CRD not installed; CatalogReady=False")
	conditions.SetCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditionTypeCatalogReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cp.Generation,
		Reason:             "KORCCRDNotInstalled",
		Message:            "K-ORC Service/Endpoint CRD is not installed",
	})
	return ctrl.Result{RequeueAfter: korcRequeueAfter}, nil
}
```

