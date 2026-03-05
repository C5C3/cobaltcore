# Pattern: Unstructured status patching for external operator simulation

**Component**: internal/common/testutil/simulators
**Category**: testing
**Applies-When**: Adding a new simulator for an external operator (e.g., SimulateNovaReady, SimulateCinderReady) in the testutil/simulators package; Adding a new typed simulator for an external CRD that was added as a typed Go module dependency (i.e., not using unstructured)

## Description

External operator simulators use unstructured.Unstructured to avoid importing external Go modules. Each simulator: (1) creates an unstructured object with the target GVK, (2) calls client.Get to retrieve the existing resource, (3) builds a status map with conditions following metav1.Condition structure (type/status/reason/message/lastTransitionTime), (4) calls unstructured.SetNestedField to set status, (5) calls client.Status().Update(). The exception is SimulateJobComplete which uses the typed batchv1.Job since it is a core K8s type. All simulators require the resource to already exist.

Typed CRD simulators follow two sub-patterns depending on the CRD's condition type: (a) For CRDs using standard metav1.Condition (MariaDB Database/User/Grant), use meta.SetStatusCondition(&obj.Status.Conditions, ...) for idempotent condition management. (b) For CRDs using custom condition types (ESO PushSecret, cert-manager Certificate), use a manual find-or-append loop since meta.SetStatusCondition only works with metav1.Condition slices. All simulators: Get typed object, set Ready/Synced condition to True, call c.Status().Update(). The constant conditionTypeReady is shared across simulators.

## Examples

### `internal/common/testutil/simulators/simulators.go:24`

```go
func SimulateMariaDBReady(ctx context.Context, c client.Client, key client.ObjectKey, replicas int) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "k8s.mariadb.com",
		Version: "v1alpha1",
		Kind:    "MariaDB",
	})
	if err := c.Get(ctx, key, obj); err != nil {
		return fmt.Errorf("getting MariaDB %s: %w", key, err)
	}
	now := metav1.Now().Format(time.RFC3339)
	status := map[string]interface{}{
		"readyReplicas": int64(replicas),
		"conditions": []interface{}{
			map[string]interface{}{"type": "Ready", "status": "True", "reason": "MariaDBReady", ...},
		},
	}
	unstructured.SetNestedField(obj.Object, status, "status")
	return c.Status().Update(ctx, obj)
}
```

### `internal/common/testutil/simulators/simulators.go:103`

```go
func SimulateExternalSecretSync(ctx context.Context, c client.Client, key client.ObjectKey) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1beta1",
		Kind:    "ExternalSecret",
	})
	if err := c.Get(ctx, key, obj); err != nil {
		return fmt.Errorf("getting ExternalSecret %s: %w", key, err)
	}
	now := metav1.Now().Format(time.RFC3339)
	status := map[string]interface{}{"refreshTime": now, "conditions": []interface{}{...}}
	unstructured.SetNestedField(obj.Object, status, "status")
	return c.Status().Update(ctx, obj)
}
```
### `internal/common/testutil/simulators/typed_simulators.go:25-41`

```go
func SimulateDatabaseReady(ctx context.Context, c client.Client, key client.ObjectKey) error {
	db := &mariadbv1alpha1.Database{}
	if err := c.Get(ctx, key, db); err != nil {
		return fmt.Errorf("getting Database %s: %w", key, err)
	}

	now := metav1.Now()
	meta.SetStatusCondition(&db.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             "DatabaseReady",
		Message:            "Database is ready",
		LastTransitionTime: now,
	})

	return c.Status().Update(ctx, db)
}
```

### `internal/common/testutil/simulators/typed_simulators.go:85-120`

```go
func SimulatePushSecretSynced(ctx context.Context, c client.Client, key client.ObjectKey) error {
	ps := &esov1alpha1.PushSecret{}
	if err := c.Get(ctx, key, ps); err != nil {
		return fmt.Errorf("getting PushSecret %s: %w", key, err)
	}

	now := metav1.Now()

	found := false
	for i, cond := range ps.Status.Conditions {
		if cond.Type == esov1alpha1.PushSecretReady {
			ps.Status.Conditions[i] = esov1alpha1.PushSecretStatusCondition{
				Type:               esov1alpha1.PushSecretReady,
				Status:             corev1.ConditionTrue,
				Reason:             esov1alpha1.ReasonSynced,
				Message:            "PushSecret synced successfully",
				LastTransitionTime: now,
			}
			found = true
			break
		}
	}
	if !found {
		ps.Status.Conditions = append(ps.Status.Conditions, esov1alpha1.PushSecretStatusCondition{...})
	}
	ps.Status.RefreshTime = now

	return c.Status().Update(ctx, ps)
}
```


