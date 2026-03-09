# Review Pattern: Assert absence of old state in mutation tests

**Review-Area**: testing
**Detection-Hint**: When a test verifies that a value was replaced/updated, check if it asserts both the presence of the new value AND the absence of the old value. Compare with similar tests in the same file for consistency.
**Severity**: WARNING
**Occurrences**: 1

## What to check

For tests covering update/replace/delete operations, verify that assert_not_contains (or equivalent negative assertions) are used alongside positive assertions. Check whether sibling tests for similar operations have stronger assertions that this test is missing.

## Why it matters

Without negative assertions, a bug where the old value is never deleted goes undetected — the new value is appended and positive checks pass, but duplicates remain, masking a real defect.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: Test 2 (single replacement) asserts both that the new value is present *and* that the old value is absent. Test 5 (multiple overrides) only checks for the new values' presence... Without these negative assertions, a bug where `sed -i` silently fails to delete existing pins would not be caught by this test.
- **What was missed**: For tests covering update/replace/delete operations, verify that assert_not_contains (or equivalent negative assertions) are used alongside positive assertions. Check whether sibling tests for similar operations have stronger assertions that this test is missing.
- **Fix**: Added assert_file_not_contains checks for 'cryptography===44.0.0' and 'keystoneauth1===5.10.0' to verify old versions were actually removed, matching the assertion pattern already used in Test 2.
