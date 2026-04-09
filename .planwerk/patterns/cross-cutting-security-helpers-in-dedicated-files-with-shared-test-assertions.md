# Pattern: Cross-cutting security helpers in dedicated files with shared test assertions

**Component**: operators/keystone/internal/controller
**Category**: testing
**Applies-When**: Adding a new cross-cutting security or configuration helper that is used by multiple reconciler files, with corresponding test assertions that would otherwise be duplicated

## Description

Cross-cutting helpers (e.g., restrictedSecurityContext) are placed in a dedicated file named after their concern (security_context.go), not in a domain-specific reconciler file. The corresponding test file (security_context_test.go) co-locates the unit test for the helper alongside shared test assertion functions (expectRestrictedSecurityContext) and utility functions (findContainerByName) that consumer tests import. This prevents assertion drift across test files and keeps the helper's location neutral.

## Examples

### `operators/keystone/internal/controller/security_context.go:17`

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

### `operators/keystone/internal/controller/security_context_test.go:33`

```go
func expectRestrictedSecurityContext(g Gomega, container *corev1.Container) {
	g.Expect(container).NotTo(BeNil(), "container must exist")
	sc := container.SecurityContext
	g.Expect(sc).NotTo(BeNil(), "SecurityContext must be set on container %q", container.Name)
	g.Expect(sc.AllowPrivilegeEscalation).NotTo(BeNil())
	g.Expect(*sc.AllowPrivilegeEscalation).To(BeFalse())
	g.Expect(sc.RunAsNonRoot).NotTo(BeNil())
	g.Expect(*sc.RunAsNonRoot).To(BeTrue())
	g.Expect(sc.ReadOnlyRootFilesystem).NotTo(BeNil())
	g.Expect(*sc.ReadOnlyRootFilesystem).To(BeTrue())
	g.Expect(sc.SeccompProfile).NotTo(BeNil())
	g.Expect(sc.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
}
```

