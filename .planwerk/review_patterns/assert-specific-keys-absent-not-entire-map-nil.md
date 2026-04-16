# Review Pattern: Assert specific keys absent, not entire map nil

**Review-Area**: testing
**Detection-Hint**: Look for assertions that check an entire map/slice is nil or empty when the test intent is only about specific entries being absent. Ask: 'Would this test break if an unrelated key were added?'
**Severity**: WARNING
**Occurrences**: 1

## What to check

When a test verifies that certain keys/fields are NOT present, check whether the assertion targets only those specific keys (NotTo(HaveKey(...))) or over-constrains by requiring the whole container to be nil/empty. The latter makes tests brittle to unrelated changes.

## Why it matters

Asserting the entire annotations map is nil means any future annotation added for a legitimate reason will break this test, even though the actual requirement (no hash annotations triggering rollouts) is still satisfied. This creates false failures and maintenance burden.

## Examples from external reviews

### CC-0074 — sourcery-ai[bot]
- **Feedback**: In `TestBuildKeystoneDeployment_NoHashAnnotations` you assert that `.Spec.Template.Annotations` is `nil`, which forbids any future annotations on the pod template; it would be safer to assert that the specific hash keys are absent.
- **What was missed**: When a test verifies that certain keys/fields are NOT present, check whether the assertion targets only those specific keys (NotTo(HaveKey(...))) or over-constrains by requiring the whole container to be nil/empty. The latter makes tests brittle to unrelated changes.
- **Fix**: Replaced `g.Expect(deploy.Spec.Template.Annotations).To(BeNil())` with `g.Expect(deploy.Spec.Template.Annotations).NotTo(HaveKey("keystone.c5c3.io/fernet-keys-hash"))` and `g.Expect(deploy.Spec.Template.Annotations).NotTo(HaveKey("keystone.c5c3.io/credential-keys-hash"))`.
