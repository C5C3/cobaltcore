# Review Pattern: Flag platform-specific CLI flags in shell scripts

**Review-Area**: validation
**Detection-Hint**: When reviewing shell scripts, check any flags passed to common utilities (base64, sed, grep, find, date, etc.) against both GNU and BSD/macOS variants. Look for flags like `base64 -w`, `sed -i ''` vs `sed -i`, `grep -P`, etc.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Any shell script that may run on developer workstations (not exclusively in containers) uses only POSIX-compatible flags, or uses portable alternatives like piping through `tr` or `awk`.

## Why it matters

Scripts that work only on Linux but are run from macOS developer workstations will fail silently or with cryptic errors at the worst possible time — in this case, during the most critical bootstrap step (storing unseal keys).

## Examples from external reviews

### CC-0009 — greptile-apps[bot]
- **Feedback**: `base64 -w 0` (disable line wrapping) is a GNU coreutils flag. On macOS/BSD the `base64` binary does not recognise `-w` and will exit with an error: `base64: illegal option -- w`
- **What was missed**: Any shell script that may run on developer workstations (not exclusively in containers) uses only POSIX-compatible flags, or uses portable alternatives like piping through `tr` or `awk`.
- **Fix**: Replaced `base64 -w 0` with `base64 | tr -d '\n'` which works on both GNU and BSD.
