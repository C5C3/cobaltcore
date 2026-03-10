# Pattern: Operator-specific envtest helper with webhook server and callback-based scheme registration

**Component**: operators/*/internal/testutil/
**Category**: testing
**Applies-When**: Adding envtest integration tests for a new operator that has defaulting/validating webhooks and needs to test through the full admission pipeline (not just unit-testing webhook functions)

## Description

Each operator that needs webhook integration testing creates a dedicated envtest setup helper in operators/<name>/internal/testutil/envtest_setup.go. The helper: (1) resolves CRD and webhook manifest paths via runtime.Caller(0) relative to the helper file, (2) starts envtest.Environment with CRDDirectoryPaths and WebhookInstallOptions, (3) builds a local scheme using callbacks (addToScheme, registerWebhooks) to avoid import cycles with the api/v1alpha1 package, (4) creates a controller-runtime manager with a webhook server bound to envtest-allocated host/port/certDir, (5) waits for the webhook server TLS endpoint to accept connections, (6) returns the manager's client. This differs from the common SetupEnvTest (which has no webhook support) and keeps SharedScheme() unmodified. Re-exports SkipIfEnvTestUnavailable from the common envtest package.

## Examples

### `operators/keystone/internal/testutil/envtest_setup.go:47`

```go
func SetupKeystoneEnvTest(
	t testing.TB,
	addToScheme func(*k8sruntime.Scheme) error,
	registerWebhooks func(ctrl.Manager) error,
) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	crdDir, webhookDir := keystonePaths()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{webhookDir},
		},
	}
```

### `operators/keystone/api/v1alpha1/integration_test.go:35`

```go
func setupEnvTest(t testing.TB) (client.Client, context.Context, context.CancelFunc) {
	t.Helper()
	return testutil.SetupKeystoneEnvTest(t, AddToScheme, func(mgr ctrl.Manager) error {
		return (&KeystoneWebhook{}).SetupWebhookWithManager(mgr)
	})
}
```

