# Review Pattern: Apply symmetric validation to all similar inputs

**Review-Area**: validation
**Detection-Hint**: When a script validates one input file with an existence check and clear error message, scan for other file inputs in the same script that lack the same guard.
**Severity**: WARNING
**Occurrences**: 1

## What to check

For each pre-flight validation (file existence, variable non-empty, permission check), verify that all analogous inputs in the same script receive the same treatment. Look for bare filenames or paths used directly in commands without prior validation.

## Why it matters

Asymmetric validation leads to cryptic errors from downstream commands (e.g., 'sed: can't read file') instead of actionable messages pointing to the root cause, making debugging harder especially in CI pipelines.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: A similar guard already exists for `$OVERRIDES` (line 19). Applying the same pattern to `$CONSTRAINTS` makes the script's preconditions symmetric and easier to diagnose.
- **What was missed**: For each pre-flight validation (file existence, variable non-empty, permission check), verify that all analogous inputs in the same script receive the same treatment. Look for bare filenames or paths used directly in commands without prior validation.
- **Fix**: Added an explicit `[ ! -f "$CONSTRAINTS" ]` check with a clear error message mirroring the existing check for the overrides file.
