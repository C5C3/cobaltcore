# Review Pattern: Remove redundant namespace creation when namespace is already declared

**Review-Area**: architecture
**Detection-Hint**: When a HelmRelease sets `install.createNamespace: true`, check whether the target namespace is already declared as a standalone Namespace manifest (e.g., in namespaces.yaml or a kustomization). Also check how other HelmReleases in the same repo handle namespace creation for consistency.
**Severity**: WARNING
**Occurrences**: 1

## What to check

For every HelmRelease with `createNamespace: true`, verify there is no separate Namespace manifest for the same namespace. Compare against existing HelmReleases in the repo to confirm the pattern is consistent.

## Why it matters

Dual namespace ownership (Flux-managed manifest + Helm createNamespace) creates ambiguity about which controller owns the namespace lifecycle. Labels, annotations, or deletion policies applied via kustomize on the Namespace manifest may be silently overridden or ignored. It also breaks the established convention in the repo where all other releases rely solely on the declarative Namespace manifest.

## Examples from external reviews

### CC-0046 — sourcery-ai[bot]
- **Feedback**: Namespace is managed both via a standalone Namespace manifest and HelmRelease.createNamespace, which is redundant and can be simplified.
- **What was missed**: For every HelmRelease with `createNamespace: true`, verify there is no separate Namespace manifest for the same namespace. Compare against existing HelmReleases in the repo to confirm the pattern is consistent.
- **Fix**: Removed `createNamespace: true` from the HelmRelease install spec, relying on the existing `chaos-mesh` namespace declared in `deploy/flux-system/namespaces.yaml`.
