# Review Pattern: Tests must not rely on implicit default values for key parameters

**Review-Area**: testing
**Detection-Hint**: When a test asserts behavior that depends on a specific input value (e.g. replica count determining PDB policy), check whether that value is explicitly set in the test or inherited from a shared fixture/helper. If the test would break or become misleading when the helper's defaults change, the value should be set or asserted explicitly in the test.
**Severity**: WARNING
**Occurrences**: 2

## What to check

Verify that test cases explicitly set or assert the values they depend on for their assertions to be meaningful. Shared test fixtures (e.g. `deployTestKeystone()`) can change defaults, silently invalidating test logic without any test failure.

## Why it matters

Implicit coupling to fixture defaults makes tests fragile and misleading. If the default changes, the test may still pass but now validates a different scenario than intended, or it may fail with a confusing error that doesn't point to the actual problem.

## Examples from external reviews

### CC-0037 — sourcery-ai[bot]
- **Feedback**: This test implicitly depends on `deployTestKeystone()` defaulting to `replicas=3`. Either assert the replica count or set `ks.Spec.Replicas` explicitly in the test before reconciling, so the PDB expectations are clearly tied to a known replica value.
- **What was missed**: Verify that test cases explicitly set or assert the values they depend on for their assertions to be meaningful. Shared test fixtures (e.g. `deployTestKeystone()`) can change defaults, silently invalidating test logic without any test failure.
- **Fix**: Set `ks.Spec.Replicas` explicitly in the test setup or added `g.Expect(ks.Spec.Replicas).To(Equal(int32(3)))` as a precondition assertion.

### CC-0037 — berendt
- **Feedback**: TestReconcileDeployment_PDBCreated uses deployTestKeystone() without setting Replicas explicitly. If the fixture default changes from Replicas: 3 to Replicas: 1, the test silently changes which PDB strategy path it exercises.
- **What was missed**: Identify the key input values that drive the behavior under test. Verify those values are explicitly set in the test setup, not inherited from a shared fixture helper whose defaults could change independently.
- **Fix**: Explicitly set Replicas in the test setup so the tested code path is pinned regardless of fixture defaults.
