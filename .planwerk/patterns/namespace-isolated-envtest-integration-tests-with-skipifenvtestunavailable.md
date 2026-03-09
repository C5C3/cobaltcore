# Pattern: Namespace-isolated envtest integration tests with SkipIfEnvTestUnavailable

**Component**: internal/common/*/integration_test.go
**Category**: testing
**Applies-When**: Writing envtest integration tests for any package in internal/common/ that interacts with the Kubernetes API

## Description

Integration tests use //go:build integration tag, call envtestutil.SkipIfEnvTestUnavailable(t) as the first line, and call envtestutil.SetupEnvTest(t) to get a client+context. Each test function creates a unique namespace (e.g., 'test-db-ensure', 'test-deploy-create') for resource isolation, preventing cross-test interference. Tests verify: resource creation, owner references, status handling after simulator calls, idempotency (call Ensure* twice, verify single resource), and not-found error paths.

## Examples

### `internal/common/database/integration_test.go:25`

```go
func TestIntegration_EnsureDatabase_creates(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := envtestutil.SetupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-db-ensure"}}
	g := NewGomegaWithT(t)
	g.Expect(c.Create(ctx, ns)).To(Succeed())

	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-owner", Namespace: ns.Name, UID: "test-uid"},
	}
	g.Expect(c.Create(ctx, owner)).To(Succeed())
	// ... test body
}
```

### `internal/common/tls/integration_test.go:25`

```go
func TestIntegration_EnsureCertificate_creates(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	c, ctx, _ := envtestutil.SetupEnvTest(t)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-cert-ensure"}}
	g := NewGomegaWithT(t)
	g.Expect(c.Create(ctx, ns)).To(Succeed())
	// ... test body
}
```

