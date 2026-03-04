# Pattern: envtest skip guard pattern

**Component**: internal/common/testutil/envtest
**Category**: testing
**Applies-When**: Writing integration tests that require envtest (KUBEBUILDER_ASSETS)

## Description

Integration tests that depend on envtest use the single, centralized exported `SkipIfEnvTestUnavailable` helper from the `envtest` package. It checks both the KUBEBUILDER_ASSETS env var and the presence of the etcd binary. Tests using the `//go:build integration` tag separate integration tests from unit tests. The guard accepts `testing.TB` (not `*testing.T`) for compatibility with both `*testing.T` and `*testing.B`. All envtest-based integration tests — including those in other packages like `simulators` — import and call this single function rather than defining local skip helpers.

## Examples

### Definition: `internal/common/testutil/envtest/setup.go:70`

```go
// SkipIfEnvTestUnavailable skips the calling test if the KUBEBUILDER_ASSETS
// environment variable is not set or the expected etcd binary is not present.
// This is the single, authoritative skip guard for all envtest-based
// integration tests (CC-0002).
func SkipIfEnvTestUnavailable(t testing.TB) {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS not set, skipping integration test")
	}
	if _, err := os.Stat(filepath.Join(assets, "etcd")); err != nil {
		t.Skipf("envtest binaries not found at %s, skipping integration test", assets)
	}
}
```

### Usage within the same package: `internal/common/testutil/envtest/integration_test.go`

```go
func TestSetupEnvTestStartsAndStops(t *testing.T) {
	SkipIfEnvTestUnavailable(t)
	// ...
}
```

### Usage from another package: `internal/common/testutil/simulators/integration_test.go`

```go
import envtestutil "github.com/c5c3/forge/internal/common/testutil/envtest"

func TestSimulateMariaDBReady(t *testing.T) {
	envtestutil.SkipIfEnvTestUnavailable(t)
	// ...
}
```

