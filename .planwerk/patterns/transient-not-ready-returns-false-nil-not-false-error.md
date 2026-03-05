# Pattern: Transient not-ready returns (false, nil) not (false, error)

**Component**: internal/common/secrets
**Category**: error-handling
**Applies-When**: Writing a readiness-check function that inspects Kubernetes resource status or data completeness. If the resource exists but is not yet in the desired state, return (false, nil) — reserve (false, error) for unexpected failures only.

## Description

Readiness-check functions (WaitForExternalSecret, IsSecretReady) return (false, nil) for all transient not-yet-ready conditions: resource not found, condition not yet True, expected keys not yet populated. Only unexpected API errors return (false, error). This ensures reconcilers re-queue normally without recording failure events or triggering exponential backoff for conditions that are expected to resolve on their own.

## Examples

### `internal/common/secrets/secrets.go:25`

```go
func WaitForExternalSecret(ctx context.Context, c client.Client, namespace, name string) (bool, error) {
	es := &esov1beta1.ExternalSecret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, es); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting ExternalSecret %s/%s: %w", namespace, name, err)
	}
	for _, cond := range es.Status.Conditions {
		if cond.Type == esov1beta1.ExternalSecretReady && cond.Status == corev1.ConditionTrue {
			return true, nil
		}
	}
	return false, nil
}
```

### `internal/common/secrets/secrets.go:48`

```go
func IsSecretReady(ctx context.Context, c client.Client, namespace, name string, expectedKeys ...string) (bool, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting Secret %s/%s: %w", namespace, name, err)
	}
	for _, key := range expectedKeys {
		if _, ok := secret.Data[key]; !ok {
			return false, nil
		}
	}
	return true, nil
}
```

