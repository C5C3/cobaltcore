# Review Pattern: Reject vacuous test assertions behind conditional guards

**Review-Area**: testing
**Detection-Hint**: Look for test assertions wrapped in `if err != nil` or similar conditionals. If the happy path means the condition is false, the assertion body is never executed and the test passes without proving anything.
**Severity**: BLOCKING
**Occurrences**: 8

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

### CC-0081 — gndrmnn
- **Feedback**: That's too implementation specific. You can remove these checks
- **What was missed**: In tests, check whether assertions verify observable behavior/contract or whether they pin the test to a specific implementation detail (exact substrings, specific function names, internal ordering). Flag assertions that would need updating purely due to a refactor that preserves behavior.
- **Fix**: Removed `isoformat(timespec="seconds")` substring checks and the `migrateIdx < patchIdx` ordering assertion from embedded-script tests, keeping only behavior-level assertions.

### CC-0096 — berendt
- **Feedback**: The bypass log message contains the literal string "defaulting" inside "webhook defaulting did not run". So `ContainSubstring("default")` matches the message text itself, not the namespace value. The assertion would pass even if the keystone.Namespace field were never logged.
- **What was missed**: For every substring/contains assertion on log output: (1) read the actual log statement being asserted against, (2) confirm the substring uniquely identifies the dynamic field, not boilerplate text in the message. Prefer asserting on the structured key/value pair or the exact field value rather than a short fragment.
- **Fix**: Asserted against the structured namespace key/value rather than a short substring that collides with the log message template.

### CC-0099 — berendt
- **Feedback**: If the keystone container started 3+ minutes before the `kubectl logs` call, `--since=120s` discards the startup window entirely, both `grep -cF` calls return 0, and the test passes vacuously even when a regression is present.
- **What was missed**: When a test asserts on logs produced at a one-time event (startup, init), ensure the log retrieval is not bounded by a time window shorter than the maximum plausible delay between that event and the assertion. Prefer `--tail=N` over `--since=Ns` when buffer size is already bounded. Also flag deviations from the established pattern used by sibling tests in the same suite.
- **Fix**: Removed `--since=120s` from both the assertion and catch-block log scrapes, keeping only `--tail=2000`, and updated the catch-block heading accordingly.

### CC-0099 — berendt
- **Feedback**: Applied the optional INFO-3 enhancement by switching from `grep -cF` (count only) to `grep -F` (capture lines) in `chainsaw-test.yaml` for both the fernet and credential paths, so the offending log lines are echoed directly in the assertion error.
- **What was missed**: When a grep assertion can fail, prefer `grep -F` (printing matched lines) over `grep -cF` (count only) so the failure message contains the actual offending log/output, not just a number.
- **Fix**: Replaced `grep -cF` with `grep -F` in both fernet and credential assertion paths.
