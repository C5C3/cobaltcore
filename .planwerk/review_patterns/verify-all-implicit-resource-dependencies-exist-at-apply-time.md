# Review Pattern: Verify all implicit resource dependencies exist at apply time

**Review-Area**: architecture
**Detection-Hint**: When reviewing Kubernetes manifests bundled in a single kustomization, check whether any resource references a namespace that is not declared as a resource in the same kustomization, or uses a CRD apiVersion/kind that is only installed asynchronously by another resource in the same apply. Mentally walk through `kubectl apply` on a completely empty cluster.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

For every resource in a kustomization: (1) Is its target namespace either `default`, a system namespace, or explicitly declared as a Namespace resource in the same kustomization? (2) Is its apiVersion/kind a built-in Kubernetes type, or does it depend on a CRD that gets installed asynchronously (e.g., via a HelmRelease)? If a CRD-dependent resource (like ClusterIssuer) is in the same apply as the CRD installer (like a cert-manager HelmRelease), the apply will fail on a fresh cluster.

## Why it matters

On a freshly bootstrapped cluster, `kubectl apply` sends all resources simultaneously. Kubernetes rejects resources whose namespace doesn't exist or whose CRD hasn't been registered yet. This causes first-apply failures, noisy error logs, and delayed convergence — undermining the reliability of the documented deployment procedure.

## Examples from external reviews

### CC-0008 — greptile-apps[bot]
- **Feedback**: The `ClusterIssuer` is bundled in the same file as the `HelmRelease`, so when `kubectl apply -k deploy/flux-system/` is run (the documented deploy command), both documents are applied simultaneously. At that moment, the `cert-manager.io/v1` CRD doesn't exist yet... The kustomization deploys resources into five namespaces (`cert-manager`, `mariadb-system`, `external-secrets`, `memcached-system`, and `openstack`) that do not exist in a freshly-bootstrapped cluster.
- **What was missed**: For every resource in a kustomization: (1) Is its target namespace either `default`, a system namespace, or explicitly declared as a Namespace resource in the same kustomization? (2) Is its apiVersion/kind a built-in Kubernetes type, or does it depend on a CRD that gets installed asynchronously (e.g., via a HelmRelease)? If a CRD-dependent resource (like ClusterIssuer) is in the same apply as the CRD installer (like a cert-manager HelmRelease), the apply will fail on a fresh cluster.
- **Fix**: Restructured into a two-phase kustomization: a base kustomization (with a new namespaces.yaml declaring all 5 namespaces, plus HelmRepository sources and HelmRelease operators) and a separate infrastructure kustomization for CRD-dependent resources (ClusterIssuer, MariaDB/Memcached instance CRs) that depends on the base phase.
