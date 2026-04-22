# Review Pattern: Update aggregate validation tests when adding new validation paths

**Review-Area**: testing
**Detection-Hint**: Search for test names containing 'AllValidations', 'RunsAll', or 'accumulate' in the test file. If the PR adds a new validation rule, check whether the aggregate test that proves no short-circuiting has been updated to include the new path.
**Severity**: WARNING
**Occurrences**: 2

## What to check

When a PR adds new webhook or validation logic, verify that any existing comprehensive test designed to prove error accumulation (no short-circuit) is updated with the new validation's error case and substring assertion.

## Why it matters

An aggregate validation test exists specifically to prove all rules fire simultaneously. Leaving it stale means a future regression that short-circuits validation will go undetected for the new fields.

## Examples from external reviews

### CC-0075 — berendt
- **Feedback**: TestValidateCreate_RunsAllValidations exercises every validation rule simultaneously to prove error accumulation (no short-circ[uit]) — [it] does not cover new validation paths.
- **What was missed**: When a PR adds new webhook or validation logic, verify that any existing comprehensive test designed to prove error accumulation (no short-circuit) is updated with the new validation's error case and substring assertion.
- **Fix**: Added PriorityClassName and TopologySpreadConstraints violations to TestValidateCreate_RunsAllValidations with corresponding substring assertions and injected a fake Client.

### CC-0084 — berendt
- **Feedback**: The PR adds 8 new validation paths ... but none of them are exercised in TestValidateCreate_RunsAllValidations, which is explicitly designed to prove every validation rule fires simultaneously (no short-circuit). A future regression that short-circuits the webhook on, say, the first replica error will silently skip all the new CC-0084 checks and no test catches it.
- **What was missed**: For every new validation rule added to a validating webhook, verify that the aggregate test (e.g. TestValidateCreate_RunsAllValidations) breaks that rule's inputs and asserts the corresponding error substring appears, so a future short-circuit regression cannot silently skip the new check.
- **Fix**: Extended TestValidateCreate_RunsAllValidations to break graceful-termination, uWSGI drain, keep-alive, and strategy axes, with substring assertions for terminationGracePeriodSeconds, preStopSleepSeconds, harakiri, httpKeepAliveTimeout, and strategy.
