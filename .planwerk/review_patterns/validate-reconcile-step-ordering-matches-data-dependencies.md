# Review Pattern: Validate reconcile step ordering matches data dependencies

**Review-Area**: architecture
**Detection-Hint**: Trace the data produced and consumed by each reconcile step in order. For each step, check whether every resource it references (ConfigMaps, Secrets, Services) is guaranteed to exist because a prior step created it. Draw a dependency graph if the chain has more than 3 steps.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

In the controller's Reconcile method, verify the sequential ordering of reconcile sub-functions. If step B mounts or references a resource created by step A, step A must execute before step B. Specifically check that config generation runs before any Job that needs config.

## Why it matters

reconcileDatabase ran before reconcileConfig, so the ConfigMap did not yet exist when db_sync first attempted to run. Even if the volume mount had been present, the ConfigMap would be missing, causing the pod to fail to schedule or the mount to be empty. Incorrect ordering creates race conditions or guaranteed first-run failures.

## Examples from external reviews

### CC-0013 — greptile-apps[bot]
- **Feedback**: This is compounded by the reconcile ordering: `reconcileDatabase` runs before `reconcileConfig`, so the config ConfigMap does not yet exist when db_sync first runs.
- **What was missed**: In the controller's Reconcile method, verify the sequential ordering of reconcile sub-functions. If step B mounts or references a resource created by step A, step A must execute before step B. Specifically check that config generation runs before any Job that needs config.
- **Fix**: Reordered keystone_controller.go so that reconcileFernetKeys runs before reconcileConfig, and reconcileConfig runs before reconcileDatabase. Both reconcileDatabase and reconcileBootstrap now receive the configMapName returned by reconcileConfig.
