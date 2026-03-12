# Review Pattern: Hardcoded sub-resource names prevent multi-instance support

**Review-Area**: architecture
**Detection-Hint**: When a reconciler creates sub-resources (Deployments, Jobs, Secrets, etc.), check whether the resource names are hardcoded strings or derived from the parent CR's name. Search for static Name fields in object builders.
**Severity**: BLOCKING
**Occurrences**: 2

## What to check

In every buildX/createX function that constructs a child resource's ObjectMeta, verify that Name incorporates the parent CR's .Name (e.g., fmt.Sprintf("%s-db-sync", cr.Name)) rather than using a hardcoded literal like "keystone-db-sync". Check all sub-reconciler files consistently.

## Why it matters

If two instances of the CR exist in the same namespace, hardcoded names cause both reconcilers to race over the same child resources. One will fail with AlreadyExists or silently adopt the other's resources via conflicting ownerReferences, leading to data corruption or reconciliation loops.

## Examples from external reviews

### CC-0013 — greptile-apps[bot]
- **Feedback**: If a second `Keystone` CR is created in the same namespace (e.g., for a staging vs. production environment, or a canary deployment), both reconcilers will race to own resources with the same name. The first reconciler's owner reference will win; the second will get an `AlreadyExists` error or silently operate against the wrong resources.
- **What was missed**: In every buildX/createX function that constructs a child resource's ObjectMeta, verify that Name incorporates the parent CR's .Name (e.g., fmt.Sprintf("%s-db-sync", cr.Name)) rather than using a hardcoded literal like "keystone-db-sync". Check all sub-reconciler files consistently.
- **Fix**: All hardcoded resource names across all four sub-reconciler files were changed to incorporate keystone.Name (e.g., fmt.Sprintf("%s-db-sync", keystone.Name)).

### CC-0013 — greptile-apps[bot]
- **Feedback**: `CreateImmutableConfigMap` is called with the hardcoded base name `"keystone-config"` rather than `fmt.Sprintf("%s-config", keystone.Name)`. When two `Keystone` CRs in the same namespace produce a ConfigMap with the same content (identical configs, same hash suffix), the second CR's reconciler will hit the owner-UID check inside `CreateImmutableConfigMap` and return: `existing ConfigMap <ns>/keystone-config-<hash> is not owned by <ns>/<second-cr-name>`
- **What was missed**: Every child resource (ConfigMap, Secret, Job, CronJob, etc.) created by a reconciler must include the parent CR name in its own name. Look for string constants like `"keystone-config"` passed to resource-creation helpers instead of `fmt.Sprintf("%s-config", cr.Name)`.
- **Fix**: Changed `"keystone-config"` to `fmt.Sprintf("%s-config", keystone.Name)` to scope the ConfigMap name to the owning CR instance.
