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
- **Feedback**: This PR adds seven new webhook validations but TestValidateCreate_RunsAllValidations — the sentinel test whose stated purpose is simultaneous exercise of every validation rule to prove error accumulation with no short-circuiting — was not updated. A future regression that short-circuits before reaching the new rules will go undetected.
- **What was missed**: For every new validation path added to a webhook, verify the aggregate test that intentionally breaks all fields at once includes setup that triggers the new rule AND a matching ContainSubstring assertion for the new field name.
- **Fix**: Added setup breaking TerminationGracePeriodSeconds, PreStopSleepSeconds, Harakiri, HTTPKeepAlive, HTTPKeepAliveTimeout, and Strategy=Recreate+RollingUpdate, plus matching ContainSubstring assertions for each new field in the aggregated error-message checks.
