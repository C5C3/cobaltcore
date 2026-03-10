# Review Pattern: Disable default listeners in test managers

**Review-Area**: testing
**Detection-Hint**: When reviewing `ctrl.NewManager` calls in test code, check whether `Metrics.BindAddress` and `HealthProbeBindAddress` are explicitly set to `"0"` to disable default port bindings. Especially critical when each test creates its own manager instance.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Test manager options must disable metrics (`:8080`) and health-probe (`:8081`) servers by setting `Metrics: metricsserver.Options{BindAddress: "0"}` and `HealthProbeBindAddress: "0"`. Without this, multiple test managers will fight over hardcoded ports.

## Why it matters

Default port bindings cause tests to fail with bind errors in CI or when running multiple tests in sequence/parallel. This is a common source of flaky tests that wastes significant debugging time.

## Examples from external reviews

### CC-0012 — greptile-apps[bot]
- **Feedback**: The manager is created without disabling the default metrics server (`:8080`) and health-probe server (`:8081`). In CI environments where either port is already in use, `ctrl.NewManager` will fail.
- **What was missed**: Test manager options must disable metrics (`:8080`) and health-probe (`:8081`) servers by setting `Metrics: metricsserver.Options{BindAddress: "0"}` and `HealthProbeBindAddress: "0"`. Without this, multiple test managers will fight over hardcoded ports.
- **Fix**: Added `Metrics: metricsserver.Options{BindAddress: "0"}` and `HealthProbeBindAddress: "0"` to the `ctrl.Options` struct in `ctrl.NewManager`.
