# Review Pattern: Enforce least-privilege on wildcard access paths

**Review-Area**: security
**Detection-Hint**: Look for wildcard glob patterns (e.g., `/*`) in policy files, IAM rules, or RBAC definitions. Compare the wildcard scope against the actual resources the consumer needs. If the wildcard covers more paths than the PR's stated purpose, flag it.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

When a policy grants access using a wildcard path, verify that the wildcard is scoped to the narrowest subtree that satisfies the declared need. Cross-reference the policy path with the actual ExternalSecrets, resource references, or consumers introduced in the same PR.

## Why it matters

An overly broad wildcard grants access to all current and future secrets under that path. If the token or credential is compromised, the blast radius extends far beyond what the feature requires, undermining the least-privilege model.

## Examples from external reviews

### CC-0009 — greptile-apps[bot]
- **Feedback**: `openstack/*` wildcard overly broad — violates least-privilege for this consumer. The wildcard grants read access to **all** current and future secrets under `openstack/` — including nova service passwords, neutron credentials, cinder, glance, and any others seeded by subsequent features. Only the specific path `kv-v2/data/openstack/keystone/db` (or at most `kv-v2/data/openstack/keystone/*`) is needed.
- **What was missed**: When a policy grants access using a wildcard path, verify that the wildcard is scoped to the narrowest subtree that satisfies the declared need. Cross-reference the policy path with the actual ExternalSecrets, resource references, or consumers introduced in the same PR.
- **Fix**: Narrowed the policy path from `kv-v2/data/openstack/*` to `kv-v2/data/openstack/keystone/*` and updated the DEVIATION comment to explain the least-privilege scoping rationale.
