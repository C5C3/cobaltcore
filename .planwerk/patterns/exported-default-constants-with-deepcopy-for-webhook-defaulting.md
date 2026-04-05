# Pattern: Exported default constants with DeepCopy for webhook defaulting

**Component**: operators/*/api/v1alpha1/
**Category**: configuration
**Applies-When**: Adding a new defaulting webhook that injects Kubernetes resource types (e.g., ResourceRequirements, resource.Quantity) as defaults — export the default values as package-level vars and use DeepCopy() when assigning them to prevent mutation of the shared variables

## Description

Default values for webhook-injected Kubernetes types are declared as exported package-level vars (e.g., DefaultMemoryRequest, DefaultCPULimit) using resource.MustParse(). The defaulting webhook uses DeepCopy() when assigning these to the spec to prevent mutation of the package-level variables. Tests reference the exported constants directly rather than duplicating literal values, establishing a single source of truth. This pattern is currently used in one operator (Keystone) but is an architectural decision that will recur as new operators add resource defaulting.

## Examples

### `operators/keystone/api/v1alpha1/keystone_webhook.go:25-30`

```go
var (
	DefaultMemoryRequest = resource.MustParse("256Mi")
	DefaultCPURequest    = resource.MustParse("100m")
	DefaultMemoryLimit   = resource.MustParse("512Mi")
	DefaultCPULimit      = resource.MustParse("500m")
)
```

### `operators/keystone/api/v1alpha1/keystone_webhook.go:82-88`

```go
		obj.Spec.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: DefaultMemoryRequest.DeepCopy(),
				corev1.ResourceCPU:    DefaultCPURequest.DeepCopy(),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: DefaultMemoryLimit.DeepCopy(),
				corev1.ResourceCPU:    DefaultCPULimit.DeepCopy(),
			},
```

