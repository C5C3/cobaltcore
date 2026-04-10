# Review Pattern: Use ptr.To() instead of address-of on value-type struct fields

**Review-Area**: type-safety
**Detection-Hint**: Search for `&someStruct.Field` where Field is a value type (bool, int, string). In Kubernetes controller code, look specifically in ensure/reconcile functions that build child resources from live CR state.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When constructing a child resource (CronJob, Deployment, etc.) from a parent CR's spec, verify that value-type fields are copied via ptr.To() rather than referenced via &. Check whether the resulting pointer could alias live CR memory.

## Why it matters

Taking &cr.Spec.Field creates a pointer into the live CR struct. If the consuming function ever defers usage or the CR is mutated, the pointer aliases live state and causes silent data corruption. ptr.To() creates a safe, independent copy.

## Examples from external reviews

### CC-0057 — berendt
- **Feedback**: TrustFlushSpec.Suspend is a bool, not a *bool. Taking &keystone.Spec.TrustFlush.Suspend creates a pointer into the live Keystone CR struct.
- **What was missed**: When constructing a child resource (CronJob, Deployment, etc.) from a parent CR's spec, verify that value-type fields are copied via ptr.To() rather than referenced via &. Check whether the resulting pointer could alias live CR memory.
- **Fix**: Replaced &keystone.Spec.TrustFlush.Suspend with ptr.To(keystone.Spec.TrustFlush.Suspend) and added the k8s.io/utils/ptr import.
