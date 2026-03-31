# Pattern: Sub-reconciler condition contract with ObservedGeneration

**Component**: operators/keystone/internal/controller
**Category**: service-structure
**Applies-When**: Setting status conditions in any sub-reconciler method of a KeystoneReconciler (or future operator reconcilers following the same pattern); Adding a new key type (beyond Fernet and credential) that requires a Secret hash for Deployment annotation-based rolling restarts

## Description

Every sub-reconciler sets conditions via conditions.SetCondition with all 5 fields: Type (string matching the sub-reconciler's responsibility, e.g. 'DatabaseReady'), Status (metav1.ConditionTrue/False), ObservedGeneration (keystone.Generation), Reason (CamelCase action state, e.g. 'WaitingForDatabase'), and Message (human-readable description). The ObservedGeneration field MUST be set on every condition to track which CR generation the condition reflects. On requeue, the condition is set to False with an appropriate Reason before returning. On success, the condition is set to True.

When multiple key types (Fernet, credential) need deterministic SHA-256 hashing of their Secret data for Deployment pod-template annotations, a shared keysHash(ctx, keystone, suffix) function accepts a suffix parameter to construct the Secret name as '{name}-{suffix}'. Type-specific wrappers (fernetKeysHash, credentialKeysHash) delegate to the shared function. This avoids duplicating the Get+Marshal+Hash logic while keeping type-specific callers readable.

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
### `operators/keystone/internal/controller/reconcile_deployment.go:34`

```go
func (r *KeystoneReconciler) keysHash(ctx context.Context, keystone *keystonev1alpha1.Keystone, suffix string) (string, error) {
	secretName := fmt.Sprintf("%s-%s", keystone.Name, suffix)
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: keystone.Namespace,
	}, &secret); err != nil {
		return "", fmt.Errorf("getting %s Secret %s/%s: %w", suffix, keystone.Namespace, secretName, err)
	}
	data, _ := json.Marshal(secret.Data)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
```

### `operators/keystone/internal/controller/reconcile_deployment.go:51`

```go
func (r *KeystoneReconciler) fernetKeysHash(ctx context.Context, keystone *keystonev1alpha1.Keystone) (string, error) {
	return r.keysHash(ctx, keystone, "fernet-keys")
}

func (r *KeystoneReconciler) credentialKeysHash(ctx context.Context, keystone *keystonev1alpha1.Keystone) (string, error) {
	return r.keysHash(ctx, keystone, "credential-keys")
}
```


