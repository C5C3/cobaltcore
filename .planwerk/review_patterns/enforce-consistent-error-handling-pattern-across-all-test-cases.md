# Review Pattern: Enforce consistent error-handling pattern across all test cases

**Review-Area**: testing
**Detection-Hint**: When a file already uses a defensive pattern (e.g., capturing exit codes with `|| exit_code=$?`) in some test cases, scan ALL other test cases in the same file for the same invocation pattern without the guard.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Under `set -euo pipefail`, any command expected to succeed but invoked without exit-code capture will abort the entire test script on unexpected failure — skipping all assertions and the results summary. Check that every invocation of the script-under-test uses the same idiom consistently.

## Why it matters

Silent test abortion means CI reports a failure with zero diagnostic output. Developers waste time debugging the test harness instead of the actual failure. Worse, partial runs can mask which tests actually passed.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: Tests 2, 3, 4, 5, and 7 invoke the script-under-test without `|| exit_code=$?` ... Under `set -euo pipefail`, if the script exits non-zero ... the test script aborts immediately — before any `assert_file_contains` / `assert_file_not_contains` calls run, and before the `=== Results ===` summary is ever printed.
- **What was missed**: Under `set -euo pipefail`, any command expected to succeed but invoked without exit-code capture will abort the entire test script on unexpected failure — skipping all assertions and the results summary. Check that every invocation of the script-under-test uses the same idiom consistently.
- **Fix**: All test cases were updated to use `local exit_code=0; (cd "$workdir" && bash "$SCRIPT_UNDER_TEST" "2025.2") || exit_code=$?; assert_eq "script exits 0" "0" "$exit_code"`, matching the pattern already present in Tests 1 and 6.
