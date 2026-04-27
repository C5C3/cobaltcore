# Pattern: Sub-reconciler condition contract with ObservedGeneration

**Component**: operators/keystone/internal/controller
**Category**: service-structure
**Applies-When**: Setting status conditions in any sub-reconciler method of a KeystoneReconciler (or future operator reconcilers following the same pattern); Implementing a sub-reconciler for an optional spec field (pointer type) that creates a Kubernetes sub-resource when set and deletes it when nil

## Description

Every sub-reconciler sets conditions via conditions.SetCondition with all 5 fields: Type (string matching the sub-reconciler's responsibility, e.g. 'DatabaseReady'), Status (metav1.ConditionTrue/False), ObservedGeneration (keystone.Generation), Reason (CamelCase action state, e.g. 'WaitingForDatabase'), and Message (human-readable description). The ObservedGeneration field MUST be set on every condition to track which CR generation the condition reflects. On requeue, the condition is set to False with an appropriate Reason before returning. On success, the condition is set to True.

Optional sub-reconcilers follow a three-path lifecycle: (1) spec field nil → delete any existing sub-resource + set condition True with reason '{Resource}NotRequired', (2) spec field set → build desired resource + ensure (create-or-update) + set condition True with reason '{Resource}Ready', (3) error → propagate wrapped error. The condition type is '{Resource}Ready' and uses ObservedGeneration. The build function is named build{CR}{Resource} and the ensure/delete helpers follow the established Get+Create/Update pattern. Both reconcileHPA (CC-0038) and reconcileNetworkPolicy (CC-0039) follow this structure.

## Examples

### `operators/keystone/internal/controller/reconcile_database.go:42-49`

```go
conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
	Type:               "DatabaseReady",
	Status:             metav1.ConditionFalse,
	ObservedGeneration: keystone.Generation,
	Reason:             "WaitingForDatabase",
	Message:            "MariaDB Database CR is not ready",
})
```

### `operators/keystone/internal/controller/reconcile_bootstrap.go:53-59`

```go
conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
	Type:               "BootstrapReady",
	Status:             metav1.ConditionTrue,
	ObservedGeneration: keystone.Generation,
	Reason:             "BootstrapComplete",
	Message:            "Keystone bootstrap completed successfully",
})
```
### `operators/keystone/internal/controller/reconcile_hpa.go:35-66`

```go
func (r *KeystoneReconciler) reconcileHPA(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	hpaName := subResourceName(keystone)
	if keystone.Spec.Autoscaling == nil {
		if err := deployment.DeleteHPA(ctx, r.Client, keystone.Namespace, hpaName); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting HorizontalPodAutoscaler: %w", err)
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type: "HPAReady", Status: metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason: "HPANotRequired", Message: "Autoscaling is not configured",
		})
		return ctrl.Result{}, nil
	}
	hpa := buildKeystoneHPA(keystone)
	if err := deployment.EnsureHPA(ctx, r.Client, r.Scheme, keystone, hpa); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring HorizontalPodAutoscaler: %w", err)
	}
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type: "HPAReady", Status: metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason: "HPAReady", Message: "HorizontalPodAutoscaler is configured",
	})
	return ctrl.Result{}, nil
}
```

### `operators/keystone/internal/controller/reconcile_networkpolicy.go:32-63`

```go
func (r *KeystoneReconciler) reconcileNetworkPolicy(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error) {
	npName := subResourceName(keystone)
	if keystone.Spec.NetworkPolicy == nil {
		if err := deleteNetworkPolicy(ctx, r.Client, keystone.Namespace, npName); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting NetworkPolicy: %w", err)
		}
		conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
			Type: "NetworkPolicyReady", Status: metav1.ConditionTrue,
			ObservedGeneration: keystone.Generation,
			Reason: "NetworkPolicyNotRequired", Message: "Network isolation is not configured",
		})
		return ctrl.Result{}, nil
	}
	np := buildKeystoneNetworkPolicy(keystone)
	if err := ensureNetworkPolicy(ctx, r.Client, r.Scheme, keystone, np); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring NetworkPolicy: %w", err)
	}
	conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
		Type: "NetworkPolicyReady", Status: metav1.ConditionTrue,
		ObservedGeneration: keystone.Generation,
		Reason: "NetworkPolicyReady", Message: "NetworkPolicy is configured",
	})
	return ctrl.Result{}, nil
}
```



