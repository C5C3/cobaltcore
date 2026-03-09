# Pattern: Unstructured status patching for external operator simulation

**Component**: internal/common/testutil/simulators
**Category**: testing
**Applies-When**: Adding a new simulator for an external operator (e.g., SimulateNovaReady, SimulateCinderReady) in the testutil/simulators package; Adding a new simulator for an external operator CRD that has been migrated from unstructured to typed (CC-0005 scope: MariaDB Database/User/Grant, ESO PushSecret, cert-manager Certificate)

## Description

External operator simulators use unstructured.Unstructured to avoid importing external Go modules. Each simulator: (1) creates an unstructured object with the target GVK, (2) calls client.Get to retrieve the existing resource, (3) builds a status map with conditions following metav1.Condition structure (type/status/reason/message/lastTransitionTime), (4) calls unstructured.SetNestedField to set status, (5) calls client.Status().Update(). The exception is SimulateJobComplete which uses the typed batchv1.Job since it is a core K8s type. All simulators require the resource to already exist.

Typed simulators (CC-0005) use the external operator's Go types directly instead of unstructured.Unstructured. Each simulator: (1) creates a typed empty struct, (2) calls client.Get to retrieve the existing resource, (3) sets status conditions using either meta.SetStatusCondition (for metav1.Condition-based types like MariaDB) or direct status field assignment (for operator-specific condition types like ESO/cert-manager), (4) calls client.Status().Update(). Unit tests verify happy path, idempotency (call twice), and not-found error. The unstructured pattern (CC-0002) remains for CRDs without typed Go imports (e.g., Memcached).

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
### `internal/common/testutil/simulators/typed_simulators.go:25`

```go
func SimulateDatabaseReady(ctx context.Context, c client.Client, key client.ObjectKey) error {
	db := &mariadbv1alpha1.Database{}
	if err := c.Get(ctx, key, db); err != nil {
		return fmt.Errorf("getting Database %s: %w", key, err)
	}

	meta.SetStatusCondition(&db.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "DatabaseReady",
		Message: "Database is ready",
	})

	return c.Status().Update(ctx, db)
}
```

### `internal/common/testutil/simulators/typed_simulators.go:99`

```go
func SimulateCertificateReady(ctx context.Context, c client.Client, key client.ObjectKey) error {
	cert := &certmanagerv1.Certificate{}
	if err := c.Get(ctx, key, cert); err != nil {
		return fmt.Errorf("getting Certificate %s: %w", key, err)
	}

	now := metav1.Now()
	cert.Status.Conditions = []certmanagerv1.CertificateCondition{
		{
			Type:               certmanagerv1.CertificateConditionReady,
			Status:             cmmeta.ConditionTrue,
			Reason:             "CertificateReady",
			Message:            "Certificate is ready",
			LastTransitionTime: &now,
		},
	}

	return c.Status().Update(ctx, cert)
}
```


