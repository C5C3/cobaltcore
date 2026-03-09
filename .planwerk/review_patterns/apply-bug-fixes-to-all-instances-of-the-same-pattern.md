# Review Pattern: Apply bug fixes to all instances of the same pattern

**Review-Area**: error-handling
**Detection-Hint**: When a PR fixes a bug in one file, search the codebase (especially sibling/similar files) for the same pattern. Use grep for the old code pattern to find unfixed instances.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

When a known bug is fixed in one file (e.g., `set -e` defeating `$?` capture), verify that every other file using the same idiom has also been fixed. Grep for the pre-fix pattern across the repo.

## Why it matters

Fixing a bug in one place while leaving identical copies elsewhere means the same failure mode persists silently. The test script aborts on failure instead of recording a FAIL, defeating its purpose entirely.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: `set -e` aborts script before exit code is captured — identical bug to the one fixed in `verify_venv_builder.sh`
- **What was missed**: When a known bug is fixed in one file (e.g., `set -e` defeating `$?` capture), verify that every other file using the same idiom has also been fixed. Grep for the pre-fix pattern across the repo.
- **Fix**: Changed `version=$(docker run ...) \n local exit_code=$?` to `local version exit_code=0 \n version=$(docker run ...) || exit_code=$?` — the same fix already applied in the sibling script.
