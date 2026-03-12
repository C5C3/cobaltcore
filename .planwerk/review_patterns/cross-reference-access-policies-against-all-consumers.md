# Review Pattern: Cross-reference access policies against all consumers

**Review-Area**: validation
**Detection-Hint**: When reviewing an access policy (e.g., HCL, RBAC), search the codebase for all resources that reference the same auth role or store, and verify every referenced path is covered by the policy.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

For each policy file, enumerate all paths it grants access to. Then find every consumer (ExternalSecrets, service accounts, etc.) that uses the role bound to that policy, and confirm every path they reference is included. A simple grep for the secret store name across ExternalSecret manifests reveals the full set of paths that must be authorized.

## Why it matters

A missing path in an access policy causes silent 403 failures at runtime. The resource appears correctly configured but will never reconcile, producing hard-to-diagnose errors that only surface in a live cluster.

## Examples from external reviews

### CC-0009 — greptile-apps[bot]
- **Feedback**: The `eso-management` policy only grants read access to `kv-v2/data/bootstrap/*` and `kv-v2/data/infrastructure/*`. However, the `keystone-db` ExternalSecret reads from `openstack/keystone/db`, which resolves to the KV v2 path `kv-v2/data/openstack/keystone/db`.
- **What was missed**: For each policy file, enumerate all paths it grants access to. Then find every consumer (ExternalSecrets, service accounts, etc.) that uses the role bound to that policy, and confirm every path they reference is included. A simple grep for the secret store name across ExternalSecret manifests reveals the full set of paths that must be authorized.
- **Fix**: Added `path "kv-v2/data/openstack/keystone/*" { capabilities = ["read"] }` and corresponding metadata path to the eso-management.hcl policy (scoped to `keystone/*` rather than `openstack/*` to enforce least-privilege).
