# Review Pattern: Document intentional environment divergence in overlays

**Review-Area**: documentation
**Detection-Hint**: When a kustomize overlay patch changes multiple default behaviors (disables features, overrides resource limits, changes runtime settings), check whether the base configuration's defaults are intentional for non-overlayed environments and whether this divergence is documented.
**Severity**: WARNING
**Occurrences**: 1

## What to check

When an overlay significantly alters the base HelmRelease values (e.g., disabling dashboard, changing container runtime, reducing resources), verify that (1) the base defaults are intentional for production/non-kind environments and (2) the divergence is documented so operators understand what differs between environments.

## Why it matters

Undocumented divergence between environments leads to surprises during incidents or environment promotion. For example, the dashboard being enabled in production but disabled in kind may be intentional, but without documentation a future contributor may assume the kind overlay represents the desired state everywhere, or may not realize production runs with higher resource usage.

## Examples from external reviews

### CC-0046 — sourcery-ai[bot]
- **Feedback**: The kind overlay patches Chaos Mesh to use containerd, disable the dashboard, and reduce resources, but the base HelmRelease uses upstream defaults; double-check that the divergence between kind and non-kind environments is intentional and documented where needed.
- **What was missed**: When an overlay significantly alters the base HelmRelease values (e.g., disabling dashboard, changing container runtime, reducing resources), verify that (1) the base defaults are intentional for production/non-kind environments and (2) the divergence is documented so operators understand what differs between environments.
- **Fix**: Added comments in the base HelmRelease and/or overlay documenting the intentional differences between kind (dev) and non-kind (production) configurations.
