# Review Pattern: Validate implicit defaults, not just explicit fields

**Review-Area**: validation
**Detection-Hint**: When a validation function checks a field that has a default/fallback value assigned elsewhere (e.g., in a builder or reconciler), trace the default path and verify validation covers it. Look for patterns like `if field != nil { validate(field) }` where nil triggers a default that could also be invalid.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

When reviewing webhook/validation code alongside reconciler/builder code, check whether every implicit default used in the reconciler is also validated. Specifically: if a nil optional field falls back to another spec field, ensure the validation rejects combinations where the fallback value would violate constraints (e.g., `spec.replicas` used as effective `minReplicas` must not exceed `maxReplicas`).

## Why it matters

The webhook allows a spec that the downstream API server (Kubernetes HPA) will reject at runtime, causing a reconciliation failure that is invisible at admission time. Users get no clear error message and the operator silently fails to create the HPA.

## Examples from external reviews

### CC-0038 — sourcery-ai[bot]
- **Feedback**: Validation does not cover the implicit MinReplicas default used in the reconciler, which can lead to an invalid HPA spec (minReplicas > maxReplicas). This check only enforces `minReplicas <= maxReplicas` when `MinReplicas` is explicitly set. In `buildKeystoneHPA`, `MinReplicas` defaults to `spec.replicas` when `autoscaling.minReplicas` is nil, so a user can set `spec.autoscaling.maxReplicas < spec.replicas` and omit `minReplicas`, yielding `minReplicas > maxReplicas`.
- **What was missed**: When reviewing webhook/validation code alongside reconciler/builder code, check whether every implicit default used in the reconciler is also validated. Specifically: if a nil optional field falls back to another spec field, ensure the validation rejects combinations where the fallback value would violate constraints (e.g., `spec.replicas` used as effective `minReplicas` must not exceed `maxReplicas`).
- **Fix**: Extended the webhook validation to treat `spec.replicas` as the effective minimum when `autoscaling.minReplicas` is nil, and reject the spec if that effective minimum exceeds `maxReplicas`.
