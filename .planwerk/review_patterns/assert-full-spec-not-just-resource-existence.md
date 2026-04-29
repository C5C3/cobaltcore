# Review Pattern: Assert full spec, not just resource existence

**Review-Area**: testing
**Detection-Hint**: In tests that fetch a managed resource (CronJob, Deployment, etc.), check whether assertions only verify existence/one field, while other relevant spec fields driving the behavior under test go unverified.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When a test exercises behavior that should preserve or set specific fields (e.g. schedule, suspend), assert those exact fields against expected values — not just that the resource exists or that one unrelated field is correct.

## Why it matters

Tests that only check existence let regressions slip through where reconciliation accidentally mutates other fields (e.g. changing schedule when toggling suspend). Tying assertions to the spec under test makes the contract explicit.

## Examples from external reviews

### CC-0096 — sourcery-ai[bot]
- **Feedback**: To better enforce the "pause without changing cadence" behavior, also assert that the CronJob's spec.schedule remains equal to the original value. This will help catch regressions where the reconciliation logic might accidentally change the schedule when toggling suspend.
- **What was missed**: When a test exercises behavior that should preserve or set specific fields (e.g. schedule, suspend), assert those exact fields against expected values — not just that the resource exists or that one unrelated field is correct.
- **Fix**: Add g.Expect(cronJob.Spec.Schedule).To(Equal(DefaultTrustFlushSchedule)) and assert Suspend matches the defaulted spec in the suspend-preservation test.
