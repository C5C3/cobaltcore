# Pattern: Unstructured status patching for external operator simulation

**Component**: internal/common/testutil/simulators
**Category**: testing
**Applies-When**: Adding a new simulator for an external operator (e.g., SimulateNovaReady, SimulateCinderReady) in the testutil/simulators package

## Description

External operator simulators use unstructured.Unstructured to avoid importing external Go modules. Each simulator: (1) creates an unstructured object with the target GVK, (2) calls client.Get to retrieve the existing resource, (3) builds a status map with conditions following metav1.Condition structure (type/status/reason/message/lastTransitionTime), (4) calls unstructured.SetNestedField to set status, (5) calls client.Status().Update(). The exception is SimulateJobComplete which uses the typed batchv1.Job since it is a core K8s type. All simulators require the resource to already exist.

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

