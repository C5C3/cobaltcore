# Review Pattern: Assert absence of old state in mutation tests

**Review-Area**: testing
**Detection-Hint**: When a test verifies that a value was replaced/updated, check if it asserts both the presence of the new value AND the absence of the old value. Compare with similar tests in the same file for consistency.
**Severity**: WARNING
**Occurrences**: 3

## What to check

For tests covering update/replace/delete operations, verify that assert_not_contains (or equivalent negative assertions) are used alongside positive assertions. Check whether sibling tests for similar operations have stronger assertions that this test is missing.

## Why it matters

Without negative assertions, a bug where the old value is never deleted goes undetected — the new value is appended and positive checks pass, but duplicates remain, masking a real defect.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: Test 2 (single replacement) asserts both that the new value is present *and* that the old value is absent. Test 5 (multiple overrides) only checks for the new values' presence... Without these negative assertions, a bug where `sed -i` silently fails to delete existing pins would not be caught by this test.
- **What was missed**: For tests covering update/replace/delete operations, verify that assert_not_contains (or equivalent negative assertions) are used alongside positive assertions. Check whether sibling tests for similar operations have stronger assertions that this test is missing.
- **Fix**: Added assert_file_not_contains checks for 'cryptography===44.0.0' and 'keystoneauth1===5.10.0' to verify old versions were actually removed, matching the assertion pattern already used in Test 2.

### CC-0032 — greptile-apps[bot]
- **Feedback**: It would still pass if the push-conditional was accidentally dropped. In that scenario this test passes, but every PR build would fail at runtime because `sbom-python-base.cyclonedx.json` does not exist on PRs.
- **What was missed**: Tests for conditional workflow expressions must validate both the value AND the condition that guards it. A test that only checks 'does filename X appear in the expression' will pass even if the conditional guard is accidentally removed, leading to runtime failures in the unguarded path.
- **Fix**: Add an assertion for the conditional expression itself, e.g.: `assert_contains "python-base Grype sbom input has push-only conditional" "$python_sbom" "event_name != 'pull_request'"`

### CC-0016 — berendt
- **Feedback**: The test's Step 4 script only checks the desired spec, not the actual running container image. Combined with replicas: 1 and default RollingUpdate strategy, the old pod remains available during the stalled rollout, so availableReplicas > 0 passes against the OLD pod — making this test either flaky (passes vacuously against old pods) or fails after 5m timeout.
- **What was missed**: For every test that mutates a Kubernetes resource, verify that at least one assertion checks a status field (status.updatedReplicas, status.containerStatuses[].image, etc.) that proves the change was actually applied, not just requested.
- **Fix**: Added an `updatedReplicas == replicas` assertion in image-upgrade Step 4 to prove new pods actually started, rather than only checking spec.image.tag.
