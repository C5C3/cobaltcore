# Review Pattern: Iterate all matched resources, not just first item

**Review-Area**: testing
**Detection-Hint**: When a shell script or test extracts a single element from a Kubernetes resource list (e.g. `jsonpath='{.items[0]...'`), check whether the assertion must hold across all matched resources. This is especially important when the target workload can scale beyond one replica.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Any shell script or test that extracts a single element from a list of Kubernetes resources (e.g. `jsonpath='{.items[0]...'`) should be reviewed for whether the assertion must hold across all matched resources, especially when the workload can scale beyond one replica. If the assertion is a correctness invariant (e.g. zero restarts, ready status), it must be checked for every item in the list.

## Why it matters

Inspecting only the first item in a list silently ignores failures in other items. If a deployment scales beyond one replica, a single-item check can report success while other pods are crash-looping or degraded. This creates a false-green signal that masks partial instability.

## Examples from external reviews

### CC-0049 — sourcery-ai[bot]
- **Feedback**: The restart-count validation script in the network-latency test only inspects the first matching operator pod; if the deployment ever scales beyond one replica, consider iterating over all matched pods and failing if any restartCount is non-zero to avoid missing partial instability.
- **What was missed**: Any shell script or test that extracts a single element from a list of Kubernetes resources (e.g. `jsonpath='{.items[0]...'`) should be reviewed for whether the assertion must hold across all matched resources, especially when the workload can scale beyond one replica.
- **Fix**: Refactored the restart-count check to iterate over all matched operator pods instead of only inspecting items[0], failing if any pod has a non-zero restartCount.
