# Pattern: Fluent builder with DeepCopy isolation

**Component**: internal/common/testutil/builders
**Category**: testing
**Applies-When**: Adding a new test resource builder (e.g., ConfigMapBuilder, DeploymentBuilder, KeystoneBuilder, ControlPlaneBuilder) in the testutil/builders package

## Description

Test builders follow the fluent pattern: constructor takes required fields (name, namespace), With* methods return *Builder for chaining, Build() returns a deep copy via DeepCopy() to prevent mutation between test cases. The builder struct holds the resource directly (not a pointer). WithOwner uses controllerutil.SetOwnerReference and panics on error since invalid owner setup indicates a test bug.

**Defensive map-copying convention (CC-0002):** Any With* method that accepts a `map[string]string` parameter (e.g., `WithLabels`, `WithAnnotations`) must defensively copy the caller-provided map before storing it. This prevents the caller from mutating builder state after calling the method. A `nil` input sets the field to `nil`. Future builders (KeystoneBuilder, ControlPlaneBuilder, etc.) must follow this same convention for all map-typed setters.

## Examples

### `internal/common/testutil/builders/secret_builder.go:23`

```go
func NewSecretBuilder(name, namespace string) *SecretBuilder {
	return &SecretBuilder{
		secret: corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Type: corev1.SecretTypeOpaque,
		},
	}
}
```

### Defensive map-copying: `internal/common/testutil/builders/secret_builder.go:37`

```go
func (b *SecretBuilder) WithLabels(labels map[string]string) *SecretBuilder {
	if labels == nil {
		b.secret.Labels = nil
		return b
	}
	copied := make(map[string]string, len(labels))
	for k, v := range labels {
		copied[k] = v
	}
	b.secret.Labels = copied
	return b
}
```

### `internal/common/testutil/builders/secret_builder.go:81`

```go
func (b *SecretBuilder) Build() *corev1.Secret {
	return b.secret.DeepCopy()
}
```

