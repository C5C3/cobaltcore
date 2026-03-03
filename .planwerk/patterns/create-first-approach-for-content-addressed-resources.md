# Pattern: Create-first approach for content-addressed resources

**Component**: internal/common/config/
**Category**: data-access
**Applies-When**: Creating resources whose name is derived from content (e.g. hash-suffixed ConfigMaps)

## Description

Attempt Create first; only on AlreadyExists do a Get to return the existing resource. This avoids a redundant lookup in the common case where the resource does not yet exist. Implemented per external reviewer (gndrmnn) feedback.

## Examples

### `internal/common/config/configmap.go:51-63`

```go
if err := c.Create(ctx, cm); err != nil {
	if !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("creating ConfigMap %s/%s: %w", namespace, hashedName, err)
	}
	existing := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: hashedName, Namespace: namespace}, existing); err != nil {
		return nil, fmt.Errorf("getting existing ConfigMap %s/%s: %w", namespace, hashedName, err)
	}
	return existing, nil
}
return cm, nil
```

