# Review Pattern: Verify framework helper semantics for zero values

**Review-Area**: validation
**Detection-Hint**: When code passes a value derived from input (e.g. obj.GetNamespace(), obj.GetName()) into a framework helper like client.InNamespace, check what the helper does when given the zero value. Many helpers treat empty string as 'no filter' / cluster-wide.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Calls like client.InNamespace(x), client.MatchingLabels(m), or similar selector helpers where x could plausibly be empty. Confirm the empty case is either guarded or documented as relying on an external invariant.

## Why it matters

Selector helpers that silently widen scope on empty input (cluster-wide list, unfiltered match) can leak data or trigger unintended side effects if the upstream invariant ever breaks.

## Examples from external reviews

### CC-0092 — berendt
- **Feedback**: controller-runtime treats client.InNamespace("") as cluster-wide, so the docs describe a defensive behaviour the code does not actually enforce.
- **What was missed**: Calls like client.InNamespace(x), client.MatchingLabels(m), or similar selector helpers where x could plausibly be empty. Confirm the empty case is either guarded or documented as relying on an external invariant.
- **Fix**: Documentation was updated to acknowledge the dependency on the apiserver invariant rather than claiming local enforcement.
