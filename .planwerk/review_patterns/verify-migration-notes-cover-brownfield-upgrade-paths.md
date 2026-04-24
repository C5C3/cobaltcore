# Review Pattern: Verify migration notes cover brownfield upgrade paths

**Review-Area**: documentation
**Detection-Hint**: When a PR changes a path/key/identifier consumed by an external policy or config applied out-of-band (not reconciled by the operator/Flux), check whether the migration note distinguishes fresh deploys from existing clusters and names the manual re-apply step.
**Severity**: WARNING
**Occurrences**: 1

## What to check

For any change that alters a resource identifier (KV path, ACL subject, API route) consumed by a policy or configuration that lives as a filesystem artefact and is only applied by a bootstrap/setup script, confirm the migration/upgrade docs explicitly list the re-apply step for brownfield operators. Flag claims like 'no operator intervention required' when the change depends on out-of-band policy application.

## Why it matters

If the migration note implies the change is automatic, operators upgrading existing clusters will skip the policy re-apply and silently hit authorization failures (here: ESO 403 on every push, FernetKeysReady/CredentialKeysReady flipping to False) with no obvious link back to the missing step.

## Examples from external reviews

### CC-0093 — berendt
- **Feedback**: The migration note claims the new per-CR path is applied automatically on the next reconcile after upgrade with no operator intervention required. This only holds for fresh deployments via hack/deploy-infra.sh... Until the policy is re-applied, ESO will receive 403 on every push to the new per-CR paths, and FernetKeysReady / CredentialKeysReady will fail silently on the backup step.
- **What was missed**: For any change that alters a resource identifier (KV path, ACL subject, API route) consumed by a policy or configuration that lives as a filesystem artefact and is only applied by a bootstrap/setup script, confirm the migration/upgrade docs explicitly list the re-apply step for brownfield operators. Flag claims like 'no operator intervention required' when the change depends on out-of-band policy application.
- **Fix**: Rewrote the migration note to explicitly require re-applying the OpenBao ACL policy on brownfield upgrades (via hack/deploy-infra.sh / setup-policies.sh or `bao policy write`) and documented the 403 / condition-flip failure mode if skipped.
