# Review Pattern: Await goroutines in test cleanup

**Review-Area**: testing
**Detection-Hint**: When reviewing test setup/teardown code, look for goroutines started with `go func()` that are stopped via context cancellation in `t.Cleanup`. Check whether the cleanup blocks until the goroutine actually exits before releasing shared resources (ports, files, connections).
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Any goroutine launched in test setup must have a synchronization point (channel, WaitGroup, errgroup) that the cleanup function waits on before proceeding with teardown. A bare `cancel()` without joining the goroutine creates a race where resources (network ports, temp files) may still be held when the next test starts.

## Why it matters

Without joining the goroutine, sequential tests can fail with port-in-use errors or other resource conflicts. These failures are intermittent and hard to diagnose, especially in CI where test ordering or parallelism varies.

## Examples from external reviews

### CC-0012 — greptile-apps[bot]
- **Feedback**: The cleanup calls `cancel()` and then `env.Stop()`, but it does not wait for the `mgr.Start(ctx)` goroutine to exit. Because each integration test starts its own manager, the previous test's manager may still be holding the metrics port (`:8080`) and the health-probe port (`:8081`) when the next test calls `ctrl.NewManager`.
- **What was missed**: Any goroutine launched in test setup must have a synchronization point (channel, WaitGroup, errgroup) that the cleanup function waits on before proceeding with teardown. A bare `cancel()` without joining the goroutine creates a race where resources (network ports, temp files) may still be held when the next test starts.
- **Fix**: Added a `mgrStopped := make(chan struct{})` channel with `defer close(mgrStopped)` in the goroutine, and `<-mgrStopped` in the cleanup function before calling `env.Stop()`.
