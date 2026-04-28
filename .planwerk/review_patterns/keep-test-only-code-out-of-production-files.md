# Review Pattern: Keep test-only code out of production files

**Review-Area**: testing
**Detection-Hint**: Scan non-_test.go files for constructors, helpers, or methods whose only callers are in _test.go files (e.g. names like newXxxForTest, exported solely for tests).
**Severity**: WARNING
**Occurrences**: 1

## What to check

Any function/constructor in a production source file that is only used by tests should live in the corresponding _test.go file instead. Verify by checking call sites of each helper.

## Why it matters

Test-only code in production files inflates the production surface area, can leak test affordances into real binaries, and obscures which APIs are actually used at runtime.

## Examples from external reviews

### CC-0089 — gndrmnn
- **Feedback**: If this constructor is test-only, move it to the test file `collectors_test.go`
- **What was missed**: Any function/constructor in a production source file that is only used by tests should live in the corresponding _test.go file instead. Verify by checking call sites of each helper.
- **Fix**: newCollectorsForTest was moved from collectors.go into collectors_test.go; the cardinality test was merged into collectors_test.go.
