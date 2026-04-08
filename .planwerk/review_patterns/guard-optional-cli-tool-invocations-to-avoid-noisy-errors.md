# Review Pattern: Guard optional CLI tool invocations to avoid noisy errors

**Review-Area**: error-handling
**Detection-Hint**: When reviewing scripts that call external CLI tools (flux, helm, istioctl, etc.), check whether the tool is guaranteed to be installed in all execution contexts. A bare `|| true` suppresses the exit code but still prints 'command not found' to stderr.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Is the external tool guaranteed to exist in all environments where this script runs? If not, wrap the call with `if command -v <tool> >/dev/null 2>&1; then ... fi` instead of relying on `|| true`.

## Why it matters

'command not found' messages in CI logs create noise and false alarms during debugging. ci-dump-diagnostics.sh line 46 calls `flux logs` with `|| true`, which avoids failure but still prints a confusing error when Flux isn't installed.

## Examples from external reviews

### CC-0050 — sourcery-ai[bot]
- **Feedback**: Calling flux unconditionally can produce noisy 'command not found' output when Flux isn't installed. Consider guarding this with `if command -v flux >/dev/null 2>&1; then ... fi`.
- **What was missed**: Is the external tool guaranteed to exist in all environments where this script runs? If not, wrap the call with `if command -v <tool> >/dev/null 2>&1; then ... fi` instead of relying on `|| true`.
- **Fix**: Replace `flux logs --all-namespaces 2>/dev/null || true` with `if command -v flux >/dev/null 2>&1; then flux logs --all-namespaces 2>/dev/null || true; else echo 'flux CLI not found, skipping'; fi`
