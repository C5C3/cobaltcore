# Review Pattern: Verify new resources are referenced by parent manifests

**Review-Area**: architecture
**Detection-Hint**: When a PR adds a new directory or file (e.g., a Kustomization, module, or config), search for references to it in parent/aggregating manifests. If no parent includes it, the resource is orphaned and will never be applied.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

For every new kustomization directory, Helm release, or deployable resource added in a PR, confirm it is referenced in the appropriate parent kustomization.yaml, flux Kustomization CR, or equivalent entry point.

## Why it matters

Orphaned resources silently fail to deploy. The PR appears complete, CI may pass, but the intended functionality is never actually applied to the cluster, creating a false sense of completion.

## Examples from external reviews

### CC-0009 — greptile-apps[bot]
- **Feedback**: `deploy/eso/` kustomization is orphaned — ESO resources will never be deployed. `deploy/eso/kustomization.yaml` is created in this PR but is not referenced by either `deploy/flux-system/kustomization.yaml` or `deploy/flux-system/infrastructure/kustomization.yaml`.
- **What was missed**: For every new kustomization directory, Helm release, or deployable resource added in a PR, confirm it is referenced in the appropriate parent kustomization.yaml, flux Kustomization CR, or equivalent entry point.
- **Fix**: Added `../../eso` to the resources list in `deploy/flux-system/infrastructure/kustomization.yaml`.
