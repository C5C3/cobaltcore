# Review Pattern: Distinguish collapsed failure modes in preflight checks

**Review-Area**: error-handling
**Detection-Hint**: When a preflight or guard check emits a single error message, verify whether multiple distinct failure causes (e.g., unreachable cluster vs. missing namespace) are being collapsed into one message that points users to a remediation that won't work for all causes.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Check that error messages in preflight/guard checks correspond to a single failure mode. If one check (e.g., `kubectl get ns X`) can fail for multiple reasons (cluster unreachable, kubeconfig missing, namespace absent), the script should probe each cause separately and emit a distinct, actionable message per cause.

## Why it matters

A single 'catch-all' error message sends users down the wrong remediation path (e.g., telling them to run a deploy command when the real problem is a missing kubeconfig), wasting time and producing confusing secondary failures.

## Examples from external reviews

### CC-0097 — berendt
- **Feedback**: The preflight in the e2e-chaos target collapses two distinct failure modes into the same error message: 'kubectl can't reach the cluster' ... and 'the cluster is reachable but the chaos-mesh namespace doesn't exist'. A developer hitting the first case is told to run `WITH_CHAOS_MESH=true make deploy-infra`, which itself would fail
- **What was missed**: Check that error messages in preflight/guard checks correspond to a single failure mode. If one check (e.g., `kubectl get ns X`) can fail for multiple reasons (cluster unreachable, kubeconfig missing, namespace absent), the script should probe each cause separately and emit a distinct, actionable message per cause.
- **Fix**: Added a `kubectl version --request-timeout=2s` preflight before `kubectl get ns chaos-mesh` so the 'cluster unreachable' case produces a different, actionable message than the 'namespace missing' case.
