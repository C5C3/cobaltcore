# Review Pattern: Reject vacuous test assertions behind conditional guards

**Review-Area**: testing
**Detection-Hint**: Look for test assertions wrapped in `if err != nil` or similar conditionals. If the happy path means the condition is false, the assertion body is never executed and the test passes without proving anything.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Every test assertion must be unconditional. If a test intends to verify that an operation succeeds, it should assert `Expect(err).NotTo(HaveOccurred())` directly, not wrap the check in `if err != nil { ... }`. Also verify that comments describing expected behavior match what the code actually tests.

## Why it matters

A vacuous test gives false confidence — it always passes regardless of whether the behavior is correct, so regressions go undetected. It is equivalent to having no test at all.

## Examples from external reviews

### CC-0011 — greptile-apps[bot]
- **Feedback**: The assertion is wrapped in an `if err != nil` guard. If `ValidateCreate` returns `nil` (which it should for `validKeystone()` with only `MaxActiveKeys` changed to `0`), the body of the `if` is never evaluated and the test passes vacuously — it doesn't prove anything either way.
- **What was missed**: Every test assertion must be unconditional. If a test intends to verify that an operation succeeds, it should assert `Expect(err).NotTo(HaveOccurred())` directly, not wrap the check in `if err != nil { ... }`. Also verify that comments describing expected behavior match what the code actually tests.
- **Fix**: Replaced the `if err != nil` conditional with an unconditional `g.Expect(err).NotTo(HaveOccurred())` assertion that directly verifies zero is accepted without error.
