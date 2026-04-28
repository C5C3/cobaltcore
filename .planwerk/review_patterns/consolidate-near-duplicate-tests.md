# Review Pattern: Consolidate near-duplicate tests

**Review-Area**: testing
**Detection-Hint**: When reviewing new tests, diff them against sibling tests in the same file. If the body is >80% identical with only literals/names differing, flag for table-driven refactor.
**Severity**: WARNING
**Occurrences**: 2

## What to check

New integration/unit tests that mirror an existing test with only a few substitutions (e.g., fernet vs credential). Check whether a shared helper or table-driven form would keep per-case assertions in one place.

## Why it matters

Duplicated test code drifts over time: assertions get updated in one copy and not the other, hiding regressions. Consolidation keeps behavior guarantees aligned and shrinks future change cost.

## Examples from external reviews

### CC-0093 — sourcery-ai[bot]
- **Feedback**: The new fernet and credential RemoteKey integration tests in `integration_test.go` are nearly identical; consider extracting a shared helper or table-driven test to reduce duplication and keep the per-CR assertions in one place.
- **What was missed**: New integration/unit tests that mirror an existing test with only a few substitutions (e.g., fernet vs credential). Check whether a shared helper or table-driven form would keep per-case assertions in one place.
- **Fix**: Refactor the two near-identical integration tests into a single table-driven test (or shared helper) parameterized by key type, so per-CR assertions live in one place.

### CC-0089 — gndrmnn
- **Feedback**: Move this test to `collectors_test.go`
- **What was missed**: Tests for a single source file should live in its paired _test.go file rather than being split into ad-hoc topical test files when the split adds no real organizational value.
- **Fix**: cardinality_test.go was merged into collectors_test.go as TestSubReconcilerMetricsHaveNoCRLabels.
