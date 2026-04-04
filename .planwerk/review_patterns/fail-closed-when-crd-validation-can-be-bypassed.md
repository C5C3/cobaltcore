# Review Pattern: Fail closed when CRD validation can be bypassed

**Review-Area**: security
**Detection-Hint**: When controller logic relies on CRD/webhook validation to guarantee safety invariants (e.g., non-empty lists, valid ranges), check whether bypassing that validation (old objects, disabled webhooks, direct etcd writes) would produce an unsafe default.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

For every security-sensitive field consumed by the controller, verify there is a defensive runtime check in the reconciler itself, not just in the CRD validation or XValidation CEL rules. Particularly look for empty slices/maps that change the semantic meaning of a Kubernetes spec (e.g., empty `From` in a NetworkPolicy ingress rule means allow-all).

## Why it matters

CRD validation is an advisory gate, not an enforcement boundary. Old objects created before validation was added, direct etcd writes, or disabled admission webhooks can all produce objects that violate the schema. If the controller trusts the input blindly, it can create resources with dangerously permissive defaults — in this case, a NetworkPolicy that allows all ingress traffic on the target port.

## Examples from external reviews

### CC-0039 — sourcery-ai[bot]
- **Feedback**: Today CRD/XValidation requires `size(self.ingress) > 0`, but if validation is bypassed (old objects, disabled validation, direct etcd writes), `npSpec.Ingress` may be empty. In that case an `Ingress` rule with an empty `From` effectively allows all sources on port 5000, which is an unsafe default for a hardening feature. Please add a defensive check… so we fail closed rather than open.
- **What was missed**: For every security-sensitive field consumed by the controller, verify there is a defensive runtime check in the reconciler itself, not just in the CRD validation or XValidation CEL rules. Particularly look for empty slices/maps that change the semantic meaning of a Kubernetes spec (e.g., empty `From` in a NetworkPolicy ingress rule means allow-all).
- **Fix**: Added a defensive guard at the top of reconcileNetworkPolicy (lines 56-63) that returns an error when len(npSpec.Ingress) == 0, ensuring the controller fails closed rather than creating an allow-all NetworkPolicy.
