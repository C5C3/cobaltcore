# Review Pattern: Replace fixed sleeps with condition-based waits in tests

**Review-Area**: testing
**Detection-Hint**: Search for `sleep` directives in test manifests or test code. Any hard-coded sleep used as a synchronization mechanism before an assertion is a flakiness risk that should be flagged.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Check whether the test framework supports native wait/poll/condition primitives. If it does, a fixed sleep before an assertion should be replaced with a condition-based wait (e.g., wait for Ready=false then Ready=true) with an explicit timeout.

## Why it matters

Fixed sleeps are either too short (causing flakes on slow CI/clusters) or too long (wasting CI time). Condition-based waits are both faster on average and more reliable, directly asserting the state transition the test cares about.

## Examples from external reviews

### CC-0047 — sourcery-ai[bot]
- **Feedback**: the fixed `sleep: 30s` before asserting conditions could be fragile on slower clusters—if Chainsaw supports it, consider replacing the fixed sleep with a more robust wait/condition pattern
- **What was missed**: Check whether the test framework supports native wait/poll/condition primitives. If it does, a fixed sleep before an assertion should be replaced with a condition-based wait (e.g., wait for Ready=false then Ready=true) with an explicit timeout.
- **Fix**: Replaced `sleep: 30s` with two Chainsaw `wait` operations: first waiting for Pod condition Ready=false (chaos took effect), then Ready=true (recovery complete), both with 2m timeouts.
