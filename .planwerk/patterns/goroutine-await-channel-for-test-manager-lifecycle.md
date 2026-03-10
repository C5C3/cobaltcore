# Pattern: Goroutine-await channel for test manager lifecycle

**Component**: operators/*/internal/testutil
**Category**: testing
**Applies-When**: Starting a controller-runtime manager in a background goroutine within test setup code (e.g., for webhook testing with envtest)

## Description

When a ctrl.Manager is started in a background goroutine during test setup, a 'mgrStopped' channel (make(chan struct{})) is created before the goroutine and closed via defer in the goroutine body. All teardown paths (t.Cleanup and error branches) must receive on this channel after calling cancel() and before calling env.Stop(), ensuring the manager has fully released its ports. This prevents sequential-test port-conflict races. Additionally, test managers must disable default listeners with Metrics: metricsserver.Options{BindAddress: "0"} and HealthProbeBindAddress: "0" to avoid port collisions.

## Examples

### `operators/keystone/internal/testutil/envtest_setup.go:103`

```go
mgrStopped := make(chan struct{})
go func() {
	defer close(mgrStopped)
	if err := mgr.Start(ctx); err != nil {
		t.Errorf("manager exited with error: %v", err)
	}
}()
```

### `operators/keystone/internal/testutil/envtest_setup.go:141`

```go
t.Cleanup(func() {
	cancel()
	<-mgrStopped // ensure manager has fully stopped and ports are released
	if err := env.Stop(); err != nil {
		t.Errorf("failed to stop Keystone envtest environment: %v", err)
	}
})
```

