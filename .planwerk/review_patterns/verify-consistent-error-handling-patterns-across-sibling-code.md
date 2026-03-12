# Review Pattern: Verify consistent error-handling patterns across sibling code

**Review-Area**: error-handling
**Detection-Hint**: When a file or group of related files uses a specific error-handling idiom (e.g., '|| exit_code=$?') in some functions but not others, flag the inconsistency. Search for command substitution assignments under 'set -e' that lack the guard pattern already used elsewhere in the same file.
**Severity**: BLOCKING
**Occurrences**: 2

## What to check

In bash scripts with 'set -euo pipefail', every command substitution assignment ('var=$(cmd)') where the command can fail must use '|| exit_code=$?' to prevent silent script abort. When reviewing, scan ALL test functions in the file — not just the changed ones — and verify the pattern is applied uniformly.

## Why it matters

Under 'set -e', an unguarded failing command substitution silently aborts the script before any assertion runs. The test suite reports 0 failures (false pass) instead of recording the actual failure, making regressions invisible in CI.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: `set -e` aborts Tests 1 and 3 before recording FAIL. With `set -euo pipefail` active, if `docker run` exits non-zero inside a command substitution assignment, bash exits the script immediately — before `assert_contains` ever runs. This means the tests silently abort instead of recording a FAIL. This is the same root cause fixed in Test 2 of this file (via `|| exit_code=$?`) and in `verify_python_base.sh` Test 1, but the fix was not applied here.
- **What was missed**: In bash scripts with 'set -euo pipefail', every command substitution assignment ('var=$(cmd)') where the command can fail must use '|| exit_code=$?' to prevent silent script abort. When reviewing, scan ALL test functions in the file — not just the changed ones — and verify the pattern is applied uniformly.
- **Fix**: Added '|| exit_code=$?' after each command substitution assignment and added 'assert_eq' checks for exit code 0 before the existing content assertions, matching the pattern already used in sibling test functions.

### CC-0009 — greptile-apps[bot]
- **Feedback**: In OpenBao (and Vault), `bao status` returns exit code `2` for **both** `initialized=true, sealed=true` and `initialized=false, sealed=true` (a brand-new, uninitialized pod). The current logic treats both as "already initialized" and skips the `initialize()` call.
- **What was missed**: Look for patterns like `cmd; rc=$?; if [[ $rc -eq X ]]` and ask: does exit code X always mean what the comment says? For Vault/OpenBao, `bao status` returns exit code 2 for both initialized+sealed and uninitialized+sealed. The script must parse JSON output (`-format=json`) and inspect the `initialized` field to distinguish these states.
- **Fix**: Rewrote `check_initialized` to parse `bao status -format=json` and inspect the `.initialized` field with jq instead of relying on ambiguous exit codes.
