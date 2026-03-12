# Review Pattern: Distinguish connectivity failures from valid negative responses

**Review-Area**: error-handling
**Detection-Hint**: Look for patterns where a command's failure is suppressed with `|| true`, `|| :`, or `set +e`, and the output is then parsed without checking whether output is empty or the command actually succeeded. Especially in init/bootstrap scripts where the next step is a destructive or non-idempotent operation.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

When a command's exit code is swallowed (e.g., `|| true`), verify that the code distinguishes between 'command failed / no output' and 'command succeeded with a negative result'. An empty or missing response should not silently fall through to the same branch as a valid 'false' response.

## Why it matters

Conflating connectivity failure with a valid negative status causes the script to proceed with an inappropriate action (e.g., attempting initialization when the pod is simply unreachable), producing confusing downstream errors instead of a clear diagnostic at the point of failure.

## Examples from external reviews

### CC-0009 — greptile-apps[bot]
- **Feedback**: `check_initialized` silently treats connectivity failure as 'not initialized'. When `kube_exec` fails due to connectivity issues, `status_json` is set to an empty string because of `|| true`. The subsequent `jq` on an empty string fails silently, causing `check_initialized` to return 1 ('not initialized').
- **What was missed**: When a command's exit code is swallowed (e.g., `|| true`), verify that the code distinguishes between 'command failed / no output' and 'command succeeded with a negative result'. An empty or missing response should not silently fall through to the same branch as a valid 'false' response.
- **Fix**: Added an explicit `[[ -z "${status_json}" ]]` check after the command call that logs a clear diagnostic ('Could not reach openbao-0') and exits before attempting initialization.
