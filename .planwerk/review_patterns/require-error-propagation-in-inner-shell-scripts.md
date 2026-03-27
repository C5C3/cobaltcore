# Review Pattern: Require error propagation in inner shell scripts

**Review-Area**: error-handling
**Detection-Hint**: When reviewing shell code passed as a string to `bash -c`, `docker run ... bash -c`, or similar wrappers, check whether `set -e` is present. Look for multi-statement scripts where setup commands (installs, inits, source) precede the main command — a missing `set -e` means setup failures are swallowed.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Any inline `bash -c '...'` script with multiple statements must use `set -e` for setup steps. If the main command's exit code needs to be captured separately, use `set +e` only around that specific command, then re-enable `set -e`.

## Why it matters

Without `set -e`, a failed pip install or init command is silently ignored, causing a confusing downstream error (e.g., ModuleNotFoundError) instead of reporting the actual failure, making CI debugging significantly harder.

## Examples from external reviews

### CC-0034 — berendt
- **Feedback**: The `Run tests` step passes a `bash -c '...'` script to `docker run` with no `set -e`. Setup failures in `source`, `printf`, `uv pip install`, and `stestr init` are silently ignored.
- **What was missed**: Any inline `bash -c '...'` script with multiple statements must use `set -e` for setup steps. If the main command's exit code needs to be captured separately, use `set +e` only around that specific command, then re-enable `set -e`.
- **Fix**: Added `set -e` at the top of the inner bash -c script, then `set +e` before `stestr run` to capture its exit code, followed by `set -e` before exporting subunit results.
