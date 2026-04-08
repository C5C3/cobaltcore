# Review Pattern: Enforce use of shared helpers instead of inline reimplementation

**Review-Area**: architecture
**Detection-Hint**: When reviewing new test code, check if the codebase already provides shared assertion helpers or utility functions (e.g., in a lib/ directory). If the new code uses raw if/grep/echo instead of existing helpers like assert_contains, flag it.
**Severity**: WARNING
**Occurrences**: 2

## What to check

New test functions should use the same assertion helpers (e.g., assert_contains from tests/lib/assertions.sh) that the rest of the test suite uses, rather than reimplementing the same logic with inline if/grep/PASS/FAIL patterns.

## Why it matters

Inline reimplementations bypass centralized error reporting and formatting, make tests harder to maintain, and create inconsistency that erodes the value of having shared helpers in the first place.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: Inline if/grep pattern is inconsistent with `assert_contains` used elsewhere. Test 4 duplicates the if/grep/PASS/FAIL idiom that `assertions.sh` was introduced to eliminate.
- **What was missed**: New test functions should use the same assertion helpers (e.g., assert_contains from tests/lib/assertions.sh) that the rest of the test suite uses, rather than reimplementing the same logic with inline if/grep/PASS/FAIL patterns.
- **Fix**: The inline if/grep/echo PASS/FAIL block was replaced with a loop calling assert_contains for each expected package.

### CC-0047 — sourcery-ai[bot]
- **Feedback**: The catch blocks across the three chaos tests contain a lot of duplicated kubectl diagnostics; consider extracting a shared helper script
- **What was missed**: Compare catch/finally/script blocks across test files in the same suite. If the same set of kubectl commands (or any shell logic) appears in more than two places, flag it for extraction into a shared script.
- **Fix**: Created `tests/e2e-chaos/diagnostics.sh` with two modes (baseline/chaos) and options (--dep-label, --dep-ns, --log-label, --eso). All three test files' catch blocks now call this script instead of inline kubectl commands.
