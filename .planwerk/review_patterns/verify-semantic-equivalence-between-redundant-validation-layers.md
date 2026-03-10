# Review Pattern: Verify semantic equivalence between redundant validation layers

**Review-Area**: validation
**Detection-Hint**: When a PR introduces both CRD-level validation (CEL/XValidation) and webhook validation for the same constraint, compare the exact boolean expressions side-by-side. Specifically check how each layer handles edge-case values for the same field (empty arrays, empty strings, zero values, null vs absent).
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

For each 'exactly one of' or mutual-exclusion constraint, enumerate the truth table across all layers (XValidation CEL rule, webhook, etc.) for at least three cases: field absent/null, field present-and-populated, and field present-but-empty. Confirm identical accept/reject decisions in all rows. CEL `has()` on an array returns `true` for `[]`, while Go `len(slice) > 0` returns `false` — this is a known divergence.

## Why it matters

Defense-in-depth validation only works if all layers agree. A semantic mismatch lets invalid resources persist if the webhook is bypassed (direct etcd write, admission misconfiguration, future webhook removal), causing silent runtime failures that are hard to diagnose.

## Examples from external reviews

### CC-0011 — greptile-apps[bot]
- **Feedback**: The CEL rule uses a *presence* check (`has(self.servers)`) while the webhook uses a *non-empty content* check (`len(k.Spec.Cache.Servers) > 0`). For an `array` field these diverge when the field is explicitly set to an empty list... If the webhook is bypassed, `cache: {servers: []}` would persist with no `clusterRef` and no usable servers — a configuration that would silently fail at reconcile time.
- **What was missed**: For each 'exactly one of' or mutual-exclusion constraint, enumerate the truth table across all layers (XValidation CEL rule, webhook, etc.) for at least three cases: field absent/null, field present-and-populated, and field present-but-empty. Confirm identical accept/reject decisions in all rows. CEL `has()` on an array returns `true` for `[]`, while Go `len(slice) > 0` returns `false` — this is a known divergence.
- **Fix**: Changed CEL rule from `has(self.clusterRef) != has(self.servers)` to `(self.clusterRef != null) != (has(self.servers) && size(self.servers) > 0)` to match the webhook's non-empty content semantics.
