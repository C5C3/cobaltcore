# Review Pattern: Ensure CRD schema validation mirrors webhook validation semantics

**Review-Area**: validation
**Detection-Hint**: When a PR introduces both XValidation CEL rules and webhook validation for the same field, compare the two: apply edge-case inputs (empty map, empty string, zero-length list) mentally to both paths and verify they produce the same accept/reject outcome.
**Severity**: BLOCKING
**Occurrences**: 2

## What to check

CEL expressions like `self.field != null` only check for null, not for empty. If the corresponding webhook rejects empty values (e.g., `len(field) == 0`), the CEL rule silently allows them through. When the webhook is unreachable, the CRD schema is the only gate.

## Why it matters

If the webhook server is down or not yet registered, the XValidation rule is the sole admission control. A mismatch means invalid resources can be persisted to etcd, causing downstream controller failures that are hard to diagnose.

## Examples from external reviews

### CC-0011 — greptile-apps[bot]
- **Feedback**: The CEL rule `self.rules != null` evaluates to **true** when `rules` is an empty map (`{}`), because an empty map is not null. However, the webhook (keystone_webhook.go line 114) rejects this case via `len(k.Spec.PolicyOverrides.Rules) == 0 && ConfigMapRef == nil`.
- **What was missed**: CEL expressions like `self.field != null` only check for null, not for empty. If the corresponding webhook rejects empty values (e.g., `len(field) == 0`), the CEL rule silently allows them through. When the webhook is unreachable, the CRD schema is the only gate.
- **Fix**: Changed CEL rule from `self.rules != null` to `size(self.rules) > 0 || self.configMapRef != null` to correctly reject empty maps and match the webhook semantics.

### CC-0011 — greptile-apps[bot]
- **Feedback**: The `x-kubernetes-validations` rule guards only the *source-presence* requirement (`size(self.rules) > 0 || self.configMapRef != null`), but it doesn't prevent the empty-key rule that the webhook enforces in REQ-008 (`keystone_webhook.go:123-131`). If the webhook server is unreachable, a payload like `rules: {"": "role:admin"}` will **pass** the CEL rule and be persisted to etcd.
- **What was missed**: For every constraint enforced in the webhook's validate() function, verify a corresponding XValidation CEL rule or kubebuilder marker exists on the CRD type. Pay special attention to map key constraints (empty keys, format rules) which are easy to miss in CEL.
- **Fix**: Added a second XValidation rule: `// +kubebuilder:validation:XValidation:rule="!has(self.rules) || self.rules.all(k, k != '')",message="policy rule name must not be empty"` to mirror the webhook constraint at the CRD schema layer.
