# Review Pattern: Add explicit checks before using extracted values under set -e

**Review-Area**: error-handling
**Detection-Hint**: In shell scripts with `set -e`, look for multi-step value extraction (kubectl get + pipe to base64, jq, etc.) where an empty or missing intermediate result would cause a cryptic pipeline error instead of an actionable message. Compare against other guards in the same script or sibling scripts.
**Severity**: WARNING
**Occurrences**: 1

## What to check

After extracting a value from an external source (kubectl secret, API call), is there an explicit emptiness check with a descriptive error message? Check for inconsistency: if one extracted variable has a guard but a similar one does not, that's a gap. In ci-run-tempest.sh, the `ADMIN_PASSWORD_B64` extraction (lines 58-66) demonstrates the correct two-stage pattern: a kubectl error trap with `::error::` followed by an emptiness check on the extracted value.

## Why it matters

A kubectl-extracted value (e.g. a base64-encoded secret field) will produce an empty string or fail with an opaque `set -e` abort if the secret or key doesn't exist. The fix in ci-run-tempest.sh (lines 58-66) shows the correct approach: trap the kubectl failure with an `::error::` message, then separately check for an empty value (missing key) with a second `::error::` message.

## Examples from external reviews

### CC-0050 — sourcery-ai[bot]
- **Feedback**: In ci-run-tempest.sh, extracting the admin password will currently fail with a generic set -e error if the secret or key is missing; adding an explicit check with a clearer ::error:: message would make local/debug usage much easier.
- **What was missed**: The `ADMIN_PASSWORD` extraction lacked both a kubectl failure trap and an emptiness check — only `set -e` would catch failures, producing opaque errors.
- **Fix**: Two-stage guard at lines 58-66: (1) kubectl error trap with `|| { echo "::error::..."; exit 1; }`, (2) emptiness check `if [[ -z "${ADMIN_PASSWORD_B64}" ]]; then echo "::error::..."; exit 1; fi`.
