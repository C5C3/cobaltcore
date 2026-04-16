# Review Pattern: Remove tests that only verify language semantics

**Review-Area**: testing
**Detection-Hint**: Look for tests that create a struct, immediately assign values, then assert those same values are present — without invoking any production logic, transformation, or side effect.
**Severity**: WARNING
**Occurrences**: 3

## What to check

Each test case should exercise actual production code behavior (a function, reconciliation loop, or side effect). A test that only verifies Go's struct literal assignment provides zero coverage of application logic.

## Why it matters

Useless tests inflate test counts, give false confidence in coverage, and add maintenance cost without catching any real bugs.

## Examples from external reviews

### CC-0013 — gndrmnn
- **Feedback**: Useless test. We certainly do not need to test, if a struct we just created has values we assign to it. This tests go-lang more than it helps us in any way testing the controller.
- **What was missed**: Each test case should exercise actual production code behavior (a function, reconciliation loop, or side effect). A test that only verifies Go's struct literal assignment provides zero coverage of application logic.
- **Fix**: Removed the test that only verified struct field assignment without exercising any controller logic.

### CC-0071 — gndrmnn
- **Feedback**: I don't see a reason why the check for `r.Requeue` was added here. The linter even complains that `r.Requeue` is deprecated.
- **What was missed**: For every new test added, verify the behavior it locks in is intentional and built on supported APIs. Tests for deprecated code paths cement bad decisions and make future cleanup harder.
- **Fix**: Removed `TestShortestRequeue_RequeueTrue` along with the deprecated `Requeue` early-return branch it was testing.

### CC-0074 — gndrmnn
- **Feedback**: This `TestBuildKeystoneDeployment_NoHashAnnotations` test is pointless. We created the `fernet-keys-hash` and `credential-keys-hash` key mappings ourselves. We control what happens to the mapping. Now that we remove them again, there is no need to add a unit test which tests for their absence.
- **What was missed**: Look for new test functions introduced alongside a deletion. If the test asserts the absence of keys, fields, or behavior that were removed in the same changeset and are fully under the author's control, the test adds no value and increases maintenance burden.
- **Fix**: The test `TestBuildKeystoneDeployment_NoHashAnnotations` was deleted entirely.
