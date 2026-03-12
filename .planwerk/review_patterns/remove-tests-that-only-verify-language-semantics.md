# Review Pattern: Remove tests that only verify language semantics

**Review-Area**: testing
**Detection-Hint**: Look for tests that create a struct, immediately assign values, then assert those same values are present — without invoking any production logic, transformation, or side effect.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Each test case should exercise actual production code behavior (a function, reconciliation loop, or side effect). A test that only verifies Go's struct literal assignment provides zero coverage of application logic.

## Why it matters

Useless tests inflate test counts, give false confidence in coverage, and add maintenance cost without catching any real bugs.

## Examples from external reviews

### CC-0013 — gndrmnn
- **Feedback**: Useless test. We certainly do not need to test, if a struct we just created has values we assign to it. This tests go-lang more than it helps us in any way testing the controller.
- **What was missed**: Each test case should exercise actual production code behavior (a function, reconciliation loop, or side effect). A test that only verifies Go's struct literal assignment provides zero coverage of application logic.
- **Fix**: Removed the test that only verified struct field assignment without exercising any controller logic.
