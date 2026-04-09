# Pattern: Shared restrictedSecurityContext helper for PSS Restricted compliance

**Component**: operators/keystone/internal/controller
**Category**: configuration
**Applies-When**: Adding a new Job or CronJob builder function that creates pod specs for the Keystone operator (or future operators following the same pattern)

## Description

All Job and CronJob container specs must include SecurityContext: restrictedSecurityContext() on every container (including init containers). The restrictedSecurityContext() helper in reconcile_deployment.go returns a *corev1.SecurityContext with AllowPrivilegeEscalation=ptr.To(false), RunAsNonRoot=ptr.To(true), ReadOnlyRootFilesystem=ptr.To(true), and SeccompProfile={Type: RuntimeDefault}. This ensures Pod Security Standards Restricted profile compliance. The helper is used by 4 builder functions across 6 containers.

## Examples

### `operators/keystone/internal/controller/reconcile_deployment.go:290`

```go
func restrictedSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		RunAsNonRoot:             ptr.To(true),
		ReadOnlyRootFilesystem:   ptr.To(true),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}
```

### `operators/keystone/internal/controller/reconcile_fernet.go:253`

```go
InitContainers: []corev1.Container{{
	Name:            "copy-keys",
	Image:           image,
	Command:         []string{"sh", "-c", "cp /fernet-keys-src/* /etc/keystone/fernet-keys/"},
	SecurityContext: restrictedSecurityContext(),
	VolumeMounts: []corev1.VolumeMount{...},
}}
```

