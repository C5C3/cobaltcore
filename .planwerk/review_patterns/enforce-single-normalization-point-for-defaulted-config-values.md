# Review Pattern: Enforce single normalization point for defaulted config values

**Review-Area**: validation
**Detection-Hint**: When a spec field has a default/minimum enforced at one layer (e.g., `max(value, 3)` in the controller), check every other place that reads the same field—validation webhook, config reconciliation, CronJob env injection—to ensure they all use the same normalized value rather than the raw spec value.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Trace every read-site of a spec field that has a floor/default applied somewhere. If the floor is applied only in one code path but the raw value is passed elsewhere, the system has an inconsistency bug. The normalization should happen in exactly one place (validation/defaulting webhook or a shared helper) and all consumers should use the normalized result.

## Why it matters

Here, `createCredentialKeysSecret` enforces `max(MaxActiveKeys, 3)` creating 3 keys, but `reconcile_config` and the rotation CronJob pass the raw `MaxActiveKeys` (possibly 0 or 1) to Keystone. This means Keystone could be told to keep 0 active keys while the secret has 3, leading to runtime misbehavior or silent data loss in credential rotation.

## Examples from external reviews

### CC-0036 — sourcery-ai[bot]
- **Feedback**: The handling of `CredentialKeys.MaxActiveKeys == 0` is inconsistent: validation explicitly allows 0, `createCredentialKeysSecret` still enforces a minimum of 3 keys, while both `reconcile_config` and the rotation CronJob env pass the literal value (possibly 0) into Keystone; consider normalizing 0 to 3 (or rejecting 0) in one place and using that normalized value everywhere.
- **What was missed**: Trace every read-site of a spec field that has a floor/default applied somewhere. If the floor is applied only in one code path but the raw value is passed elsewhere, the system has an inconsistency bug. The normalization should happen in exactly one place (validation/defaulting webhook or a shared helper) and all consumers should use the normalized result.
- **Fix**: Normalize `MaxActiveKeys` to a minimum of 3 in one authoritative location (the defaulting webhook or a shared helper), and have all consumers—secret creation, config reconciliation, and CronJob env—reference that normalized value.
