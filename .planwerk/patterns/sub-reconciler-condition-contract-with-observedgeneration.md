# Pattern: Sub-reconciler condition contract with ObservedGeneration

**Component**: operators/keystone/internal/controller
**Category**: service-structure
**Applies-When**: Setting status conditions in any sub-reconciler method of a KeystoneReconciler (or future operator reconcilers following the same pattern)

## Description

Every sub-reconciler sets conditions via conditions.SetCondition with all 5 fields: Type (string matching the sub-reconciler's responsibility, e.g. 'DatabaseReady'), Status (metav1.ConditionTrue/False), ObservedGeneration (keystone.Generation), Reason (CamelCase action state, e.g. 'WaitingForDatabase'), and Message (human-readable description). The ObservedGeneration field MUST be set on every condition to track which CR generation the condition reflects. On requeue, the condition is set to False with an appropriate Reason before returning. On success, the condition is set to True.

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

