# Review Pattern: Update aggregate validation tests when adding new validation paths

**Review-Area**: testing
**Detection-Hint**: Search for test names containing 'AllValidations', 'RunsAll', or 'accumulate' in the test file. If the PR adds a new validation rule, check whether the aggregate test that proves no short-circuiting has been updated to include the new path.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When a PR adds new webhook or validation logic, verify that any existing comprehensive test designed to prove error accumulation (no short-circuit) is updated with the new validation's error case and substring assertion.

## Why it matters

An aggregate validation test exists specifically to prove all rules fire simultaneously. Leaving it stale means a future regression that short-circuits validation will go undetected for the new fields.

## Examples from external reviews

### CC-0075 — berendt
- **Feedback**: TestValidateCreate_RunsAllValidations exercises every validation rule simultaneously to prove error accumulation (no short-circ[uit]) — [it] does not cover new validation paths.
- **What was missed**: When a PR adds new webhook or validation logic, verify that any existing comprehensive test designed to prove error accumulation (no short-circuit) is updated with the new validation's error case and substring assertion.
- **Fix**: Added PriorityClassName and TopologySpreadConstraints violations to TestValidateCreate_RunsAllValidations with corresponding substring assertions and injected a fake Client.
