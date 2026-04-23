# Review Pattern: Replace fixed sleeps with condition-based waits in tests

**Review-Area**: testing
**Detection-Hint**: Search for `sleep` directives in test manifests or test code. Any hard-coded sleep used as a synchronization mechanism before an assertion is a flakiness risk that should be flagged.
**Severity**: WARNING
**Occurrences**: 3

## What to check

Check whether the test framework supports native wait/poll/condition primitives. If it does, a fixed sleep before an assertion should be replaced with a condition-based wait (e.g., wait for Ready=false then Ready=true) with an explicit timeout.

## Why it matters

Fixed sleeps are either too short (causing flakes on slow CI/clusters) or too long (wasting CI time). Condition-based waits are both faster on average and more reliable, directly asserting the state transition the test cares about.

## Examples from external reviews

### CC-0047 — sourcery-ai[bot]
- **Feedback**: the fixed `sleep: 30s` before asserting conditions could be fragile on slower clusters—if Chainsaw supports it, consider replacing the fixed sleep with a more robust wait/condition pattern
- **What was missed**: Check whether the test framework supports native wait/poll/condition primitives. If it does, a fixed sleep before an assertion should be replaced with a condition-based wait (e.g., wait for Ready=false then Ready=true) with an explicit timeout.
- **Fix**: Replaced `sleep: 30s` with two Chainsaw `wait` operations: first waiting for Pod condition Ready=false (chaos took effect), then Ready=true (recovery complete), both with 2m timeouts.

### CC-0073 — berendt
- **Feedback**: Brittle index-based test assertions — refactored VolumeMounts assertions to use a find-by-name switch pattern matching the existing Volumes assertion style.
- **What was missed**: Test assertions on list-type resources should locate items by a stable identifier (e.g., name) rather than relying on positional indices. Check whether the test file already has a find-by-name pattern used for similar assertions and whether new assertions follow the same style.
- **Fix**: Refactored VolumeMounts assertions in both reconcile_fernet_test.go and reconcile_credential_test.go to use a find-by-name switch pattern matching the existing Volumes assertion style.

### CC-0087 — berendt
- **Feedback**: This negative test's reliability depends on the controller's periodic requeue cadence being longer than 1s. If a future change reduces the period...
- **What was missed**: Negative reconcile tests that assert 'did not reconcile' via a Consistently window should not silently depend on requeue periods. The window should either exceed known periodic cadences or the test should disable periodic requeues.
- **Fix**: No code change; reviewer marked as ASK/optional follow-up.
