# Pattern: Idempotent create with AlreadyExists swallow (create-once resources)

**Component**: internal/common/ K8s-interacting packages
**Category**: data-access
**Applies-When**: Creating Kubernetes resources that should be created once and not updated (unstructured CRDs, immutable ConfigMaps, Jobs)

## Description

For resources that are created once and not updated (CRDs managed by external operators, immutable ConfigMaps, Jobs), the canonical pattern is: attempt Create, if apierrors.IsAlreadyExists(err) return nil. For resources that need updates (Deployment, Service, CronJob), the pattern is Get+Create/Update with ResourceVersion preservation for optimistic concurrency.

## Examples

### `internal/common/tls/certificate.go:63-69`

```go
if err := c.Create(ctx, obj); err != nil {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return fmt.Errorf("creating Certificate %s/%s: %w", namespace, name, err)
}
return nil
```

### `internal/common/job/job.go:31-37`

```go
if err := c.Create(ctx, job); err != nil {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return fmt.Errorf("creating Job %s/%s: %w", job.Namespace, job.Name, err)
}
return nil
```

