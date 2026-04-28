# Review Pattern: Assert rendered output, not just input handling

**Review-Area**: testing
**Detection-Hint**: For tests covering config-rendering features, check that assertions inspect the actual rendered artifact (ConfigMap data, generated file contents) rather than only verifying the spec was accepted or the controller did not error.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Tests for features that transform spec into rendered config should fetch the resulting ConfigMap/Secret/file and assert the specific key-value pairs that prove the feature works end-to-end. Contract keys from external libraries (e.g. JSONFormatter output keys) should also be verified.

## Why it matters

Tests that only check the controller accepted input give false confidence — the rendering logic could be silently broken while the test passes. Asserting the rendered output catches regressions in the actual behavior users depend on.

## Examples from external reviews

### CC-0098 — berendt
- **Feedback**: The REQ-003 test now fetches the rendered ConfigMap and asserts `use_stderr = false`; the chainsaw JSON step now also verifies the oslo.log JSONFormatter contract keys.
- **What was missed**: Tests for features that transform spec into rendered config should fetch the resulting ConfigMap/Secret/file and assert the specific key-value pairs that prove the feature works end-to-end. Contract keys from external libraries (e.g. JSONFormatter output keys) should also be verified.
- **Fix**: Updated REQ-003 to fetch the rendered ConfigMap and assert `use_stderr = false`, and extended the chainsaw JSON step to verify JSONFormatter contract keys.
