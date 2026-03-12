# Review Pattern: Flag hardcoded topology assumptions about referenced resources

**Review-Area**: architecture
**Detection-Hint**: Look for magic numbers or hardcoded lists when generating endpoints, hostnames, or addresses for external/referenced resources (e.g. StatefulSet pod ordinals, port numbers, replica counts). The values should derive from the referenced resource's actual configuration.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When code generates endpoint lists or connection strings for resources referenced via a `clusterRef` or similar pointer, verify it reads the actual replica count or topology from the referenced resource rather than hardcoding assumptions like a fixed number of pod ordinals.

## Why it matters

Hardcoded replica counts (e.g. always generating 3 Memcached endpoints) produce unreachable endpoints when fewer replicas exist and silently exclude capacity when more exist. This creates subtle runtime failures that are hard to diagnose in non-default configurations.

## Examples from external reviews

### CC-0013 — greptile-apps[bot]
- **Feedback**: The managed-mode cache helper hard-codes pod ordinals `0`, `1`, `2` regardless of `spec.cache.clusterRef`'s actual replica count... If the referenced Memcached StatefulSet is deployed with 1 or 2 replicas, Keystone's oslo.cache driver will include unreachable endpoints in the pymemcache pool.
- **What was missed**: When code generates endpoint lists or connection strings for resources referenced via a `clusterRef` or similar pointer, verify it reads the actual replica count or topology from the referenced resource rather than hardcoding assumptions like a fixed number of pod ordinals.
- **Fix**: Derived the endpoint list from the actual replica count of the referenced Memcached cluster instead of hardcoding three ordinals.
