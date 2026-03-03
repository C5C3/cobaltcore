# Pattern: Idempotent create with AlreadyExists swallow

**Component**: internal/common/ K8s-interacting packages
**Category**: data-access
**Applies-When**: Creating Kubernetes resources that should be created once and not updated (unstructured CRDs, immutable ConfigMaps, Jobs)

## Description

For resources that are created once and not updated (CRDs managed by external operators, immutable ConfigMaps, Jobs), the pattern is: attempt Create, if apierrors.IsAlreadyExists(err) return nil. For ConfigMap, a preliminary Get check is used since the content-hash name makes it a lookup. For resources that need updates (Deployment, Service, CronJob), the pattern is Get+Create/Update with ResourceVersion preservation.

## Examples

### `internal/common/tls/certificate.go:26-67`

```go
func EnsureCertificate(ctx context.Context, c client.Client, name, namespace, issuerName, commonName string, dnsNames []string, owners ...metav1.OwnerReference) error {
	// ... construct unstructured object ...
	if err := c.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating Certificate %s/%s: %w", namespace, name, err)
	}
	return nil
}
```

### `internal/common/deployment/deployment.go:17-33`

```go
func EnsureDeployment(ctx context.Context, c client.Client, deployment *appsv1.Deployment) error {
	existing := &appsv1.Deployment{}
	err := c.Get(ctx, client.ObjectKeyFromObject(deployment), existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if createErr := c.Create(ctx, deployment); createErr != nil {
				return fmt.Errorf("creating Deployment %s/%s: %w", deployment.Namespace, deployment.Name, createErr)
			}
			return nil
		}
		return fmt.Errorf("getting Deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
	}
	deployment.ResourceVersion = existing.ResourceVersion
	if err := c.Update(ctx, deployment); err != nil {
		return fmt.Errorf("updating Deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
	}
	return nil
}
```

