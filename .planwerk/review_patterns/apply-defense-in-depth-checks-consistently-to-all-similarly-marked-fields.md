# Review Pattern: Apply defense-in-depth checks consistently to all similarly-marked fields

**Review-Area**: validation
**Detection-Hint**: When a webhook validate() function has an explicit defense-in-depth check for one field's kubebuilder marker (e.g., Minimum), scan for all other fields with the same type of marker. If any lack a corresponding runtime check, flag the inconsistency.
**Severity**: WARNING
**Occurrences**: 3

## What to check

List all fields with `+kubebuilder:validation:Minimum` (or similar constraint markers). For each, verify the webhook's validate() function contains a matching runtime guard. Look especially at fields that also have a Default() value, as the defaulting may mask the missing check in normal flow but not in bypass scenarios.

## Why it matters

An established defense-in-depth pattern applied to some fields but not others creates a false sense of security. If CRD admission is bypassed (direct etcd writes, admission misconfiguration), unguarded fields accept invalid values silently — here, maxActiveKeys < 3 would cause Fernet token validation failures.

## Examples from external reviews

### CC-0011 — greptile-apps[bot]
- **Feedback**: `Replicas` gets an explicit comment — *"Defense-in-depth replicas check alongside the `+kubebuilder:validation:Minimum=1` marker"* — and a corresponding `< 1` guard here. `FernetSpec.MaxActiveKeys` also carries a `+kubebuilder:validation:Minimum=3` CRD marker and is auto-defaulted to `3` by `Default()`, but the `validate()` function has no corresponding `< 3` check.
- **What was missed**: List all fields with `+kubebuilder:validation:Minimum` (or similar constraint markers). For each, verify the webhook's validate() function contains a matching runtime guard. Look especially at fields that also have a Default() value, as the defaulting may mask the missing check in normal flow but not in bypass scenarios.
- **Fix**: Added a defense-in-depth check: `if k.Spec.Fernet.MaxActiveKeys < 3 && k.Spec.Fernet.MaxActiveKeys != 0 { allErrs = append(allErrs, field.Invalid(...)) }` mirroring the existing pattern used for Replicas.

### CC-0011 — greptile-apps[bot]
- **Feedback**: `Cache` has both a `+kubebuilder:validation:XValidation` CEL rule and a matching runtime guard in `validate()`. `Database` has the same CEL XOR constraint but no corresponding defense-in-depth check exists in `validate()`. If CRD admission is bypassed, a `Keystone` with both `database.clusterRef` and `database.host` set is accepted silently.
- **What was missed**: For every struct field carrying a +kubebuilder:validation:XValidation (or similar declarative constraint), verify that the same logical check is duplicated in the runtime validation function. Compare the set of CEL-guarded fields against the set of runtime-guarded fields and ensure they match.
- **Fix**: Added a runtime mutual-exclusivity check for Database (REQ-010) immediately after the existing Cache check, using the same pattern: `if (k.Spec.Database.ClusterRef != nil) == (k.Spec.Database.Host != "")`. Added corresponding rejection tests mirroring the Cache test cases.

### CC-0039 — berendt
- **Feedback**: The webhook validates every other optional spec field with defense-in-depth checks (autoscaling, cache, database, policyOverrides), but the networkPolicy field has no webhook validation.
- **What was missed**: Open the webhook file and confirm the new field has an admission-time validation block that mirrors the reconciler guard. Compare the set of fields validated in the webhook against the set validated in the reconciler; they should match.
- **Fix**: Added a webhook validation block for networkPolicy after the autoscaling block, rejecting empty ingress at admission time, plus three dedicated unit tests (nil-valid, with-ingress-valid, empty-ingress-rejected) and an update to the aggregate-validation test.
