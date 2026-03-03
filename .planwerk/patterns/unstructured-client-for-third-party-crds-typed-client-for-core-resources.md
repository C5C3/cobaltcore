# Pattern: Unstructured client for third-party CRDs, typed client for core resources

**Component**: internal/common/ K8s-interacting packages
**Category**: data-access
**Applies-When**: Creating or reading Kubernetes resources where no typed Go client exists (MariaDB, ESO, cert-manager CRDs)

## Description

Third-party CRDs (MariaDB Database/User/Grant, ExternalSecret, PushSecret, Certificate) are managed via unstructured.Unstructured objects with explicit GVK, SetNestedField, and NestedSlice operations. Core K8s resources (ConfigMap, Secret, Deployment, Service, Job, CronJob) use typed clients from k8s.io/api. This avoids importing typed client libraries for operators that are not under our control.

## Examples

### `internal/common/database/database.go:48-69`

```go
func EnsureDatabase(ctx context.Context, c client.Client, name, namespace, mariadbRef string, owners ...metav1.OwnerReference) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(databaseGVK)
	obj.SetName(name)
	obj.SetNamespace(namespace)
	if err := unstructured.SetNestedField(obj.Object, mariaDbRefMap, "spec", "mariaDbRef"); err != nil {
		return fmt.Errorf("setting Database spec.mariaDbRef: %w", err)
	}
	if err := c.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) { return nil }
		return fmt.Errorf("creating Database %s/%s: %w", namespace, name, err)
	}
	return nil
}
```

### `internal/common/config/configmap.go:30-63`

```go
func CreateImmutableConfigMap(ctx context.Context, c client.Client, name, namespace string, data map[string]string, owners ...metav1.OwnerReference) (*corev1.ConfigMap, error) {
	hashedName := fmt.Sprintf("%s-%s", name, contentHash(data))
	existing := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Name: hashedName, Namespace: namespace}, existing)
	if err == nil { return existing, nil }
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: hashedName, Namespace: namespace, OwnerReferences: owners},
		Immutable: ptr.To(true), Data: data,
	}
	if err := c.Create(ctx, cm); err != nil {
		return nil, fmt.Errorf("creating ConfigMap %s/%s: %w", namespace, hashedName, err)
	}
	return cm, nil
}
```

