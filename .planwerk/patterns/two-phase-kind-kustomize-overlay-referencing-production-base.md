# Pattern: Two-phase kind kustomize overlay referencing production base

**Component**: deploy/kind/base/, deploy/kind/infrastructure/
**Category**: configuration
**Applies-When**: Adding kind-specific resource sizing or configuration that differs from production manifests in deploy/flux-system/

## Description

Kind-specific differences are expressed as kustomize overlays in deploy/kind/ that reference the production manifests via relative paths (../../flux-system/ for base, ../../flux-system/infrastructure/ for CRD-dependent resources). The overlays use strategic merge patches to adjust replicas, storage classes, and HA settings without modifying the production manifests. This mirrors the two-phase kustomization pattern from deploy/flux-system/ (base for operators, infrastructure for CRD-dependent resources) and ensures kind testing validates the same manifests that ship to production with only resource sizing differences.

## Examples

### `deploy/kind/base/kustomization.yaml:14-15`

```
resources:
  - ../../flux-system/
```

### `deploy/kind/infrastructure/kustomization.yaml:16-17`

```
resources:
  - ../../flux-system/infrastructure/
```

