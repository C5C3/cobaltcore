# Review Pattern: Reject vacuous test assertions behind conditional guards

**Review-Area**: testing
**Detection-Hint**: Look for test assertions wrapped in `if err != nil` or similar conditionals. If the happy path means the condition is false, the assertion body is never executed and the test passes without proving anything.
**Severity**: BLOCKING
**Occurrences**: 4

## What to check

Every test assertion must be unconditional. If a test intends to verify that an operation succeeds, it should assert `Expect(err).NotTo(HaveOccurred())` directly, not wrap the check in `if err != nil { ... }`. Also verify that comments describing expected behavior match what the code actually tests.

## Why it matters

A vacuous test gives false confidence — it always passes regardless of whether the behavior is correct, so regressions go undetected. It is equivalent to having no test at all.

## Examples from external reviews

### CC-0011 — greptile-apps[bot]
- **Feedback**: The assertion is wrapped in an `if err != nil` guard. If `ValidateCreate` returns `nil` (which it should for `validKeystone()` with only `MaxActiveKeys` changed to `0`), the body of the `if` is never evaluated and the test passes vacuously — it doesn't prove anything either way.
- **What was missed**: Every test assertion must be unconditional. If a test intends to verify that an operation succeeds, it should assert `Expect(err).NotTo(HaveOccurred())` directly, not wrap the check in `if err != nil { ... }`. Also verify that comments describing expected behavior match what the code actually tests.
- **Fix**: Replaced the `if err != nil` conditional with an unconditional `g.Expect(err).NotTo(HaveOccurred())` assertion that directly verifies zero is accepted without error.

### CC-0012 — greptile-apps[bot]
- **Feedback**: `mgr.GetClient()` returns the manager's caching (informer-backed) client. In controller-runtime v0.23.x the cache does not fall back to the API server on a cache miss — if the informer has not yet processed a freshly created object, the immediately following `c.Get()` in the tests will return `not found`, making tests intermittently flaky.
- **What was missed**: Test code that performs synchronous Create-then-Get assertions must use a direct API server client (`client.New(cfg, ...)`), not the manager's caching client (`mgr.GetClient()`). Also check for divergence from established test utility patterns already present in the codebase (e.g., a shared `SetupEnvTestEnvironment` helper).
- **Fix**: Replaced `c := mgr.GetClient()` with `c, err := client.New(cfg, client.Options{Scheme: s})` to use a direct API server client, matching the project's existing pattern in `internal/common/testutil/envtest/setup.go`.

### CC-0045 — berendt
- **Feedback**: Add a corresponding assertion to expectRestrictedSecurityContext in security_context_test.go for Capabilities.Drop.
- **What was missed**: The test helper (expectRestrictedSecurityContext) must assert every field that the specification requires. If the Capabilities.Drop field was missing from both the implementation AND the test, the test would still pass — making it useless for catching omissions.
- **Fix**: Added `assert.NotNil(t, sc.Capabilities)` and `assert.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop)` to the test helper.

### CC-0051 — berendt
- **Feedback**: The test assertion was changed from verifying that [test] extra is included in the install spec to only verifying that extra-package... but no assert_not_contains for the old '[test]' pattern was added.
- **What was missed**: When a PR replaces an old test assertion with a new one reflecting changed behavior, verify that the test also explicitly asserts the OLD value is absent. Positive-only assertions can pass even if the old behavior silently persists alongside the new one.
- **Fix**: The old assert_contains for '[test]' was replaced with two new positive assertions (checking 'extra-packages.yaml' and 'install_spec='), though the suggested assert_not_contains for '[test]' was not added.
