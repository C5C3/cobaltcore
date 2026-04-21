# Review Pattern: Reject tautological constant tests

**Review-Area**: testing
**Detection-Hint**: A test file asserts that a const equals the same literal value used in its declaration. The test has no conditional logic, no computed values, and no external input — it can never fail unless someone intentionally changes the constant AND forgets to update the test, which the test itself does not guard against meaningfully.
**Severity**: WARNING
**Occurrences**: 2

## What to check

When a test simply re-states the value of a compile-time constant (e.g., `assert(MyConst == 5)` where `MyConst` is declared as `5`), ask: what bug could this test ever catch? If the answer is 'none — it only breaks when someone updates the const but not the test', the test adds no value and should be removed.

## Why it matters

Tautological tests inflate coverage metrics without testing real behavior, create false confidence, and add maintenance burden. They also signal to future contributors that trivial assertions are acceptable, lowering the quality bar for the test suite.

## Examples from external reviews

### CC-0067 — gndrmnn
- **Feedback**: Entirely useless test for constant values. Remove entire test file.
- **What was missed**: When a test simply re-states the value of a compile-time constant (e.g., `assert(MyConst == 5)` where `MyConst` is declared as `5`), ask: what bug could this test ever catch? If the answer is 'none — it only breaks when someone updates the const but not the test', the test adds no value and should be removed.
- **Fix**: Delete the test file that only asserted compile-time constant values equal their own literal declarations.

### CC-0078 — gndrmnn
- **Feedback**: Remove unnecessary test which only validates that a constant has a constant value
- **What was missed**: Tests whose body is a single equality assertion between a package constant and a hardcoded literal, providing no behavioral coverage.
- **Fix**: Deleted TestKeystoneFinalizerConstant_HasExpectedValue which only asserted the finalizer constant equals its literal string.
