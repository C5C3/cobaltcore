# Review Pattern: Ensure kubectl commands use explicit namespace flag

**Review-Area**: testing
**Detection-Hint**: Scan kubectl commands in test definitions (especially 'kubectl run', 'kubectl exec', 'kubectl get') for a missing '-n' or '--namespace' flag. Compare against the namespace used by the test's resource definitions and catch blocks.
**Severity**: INFO
**Occurrences**: 1

## What to check

For each kubectl command in a test step, verify that it specifies '-n $NAMESPACE' (or '--namespace') explicitly, matching the namespace used by the test's resource definitions and catch blocks. Pay particular attention to 'kubectl run' which defaults to the kubeconfig default namespace (typically 'default'), not the test's target namespace.

## Why it matters

Omitting the namespace flag creates an inconsistency: catch blocks and assertions target one namespace while the command runs in another. Although it may work when using fully-qualified service names and --rm, it scatters test artifacts across namespaces and makes debugging harder.

## Examples from external reviews

### CC-0060 — berendt
- **Feedback**: Missing -n namespace flag on kubectl run commands in Steps 3 and 6.
- **What was missed**: Scan kubectl commands in test definitions (especially 'kubectl run', 'kubectl exec', 'kubectl get') for a missing '-n' or '--namespace' flag. Compare against the namespace used by the test's resource definitions.
- **Fix**: Added '-n $NAMESPACE' to both kubectl run commands in Steps 3 and 6.
