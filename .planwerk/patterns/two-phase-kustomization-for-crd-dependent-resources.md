# Pattern: Two-phase kustomization for CRD-dependent resources

**Component**: deploy/flux-system/
**Category**: configuration
**Applies-When**: Adding Kubernetes resources to the deploy/ manifests that depend on CRDs installed by operator HelmReleases (e.g., ClusterIssuer requires cert-manager CRDs, MariaDB CR requires mariadb-operator CRDs); Adding a HelmRelease or namespaced resource to the base kustomization that targets a non-default namespace

## Description

CRD-dependent resources (ClusterIssuer, operator instance CRs like MariaDB/Memcached) are placed in a separate infrastructure kustomization (deploy/flux-system/infrastructure/kustomization.yaml) rather than the base kustomization. The base kustomization contains only built-in Kubernetes types (Namespaces, HelmRepository, HelmRelease). This prevents kubectl apply failures on fresh clusters where CRDs do not exist yet. Deployment follows a two-step process: step 1 applies base resources, step 2 applies infrastructure resources after operators have installed their CRDs.

All target namespaces used by HelmRelease CRs and infrastructure resources are declared as explicit Namespace resources in namespaces.yaml, listed as the first resource in the base kustomization. This ensures kubectl apply -k works on fresh clusters where namespaces don't exist yet. When adding a new operator HelmRelease targeting a new namespace, the namespace must also be added to namespaces.yaml. The install.createNamespace: true setting on HelmReleases is retained as a complementary safety net for FluxCD reconciliation.

## Examples

### `deploy/flux-system/kustomization.yaml:19-34`

```
resources:
  # Namespace resources (applied first by kustomize resource ordering)
  - namespaces.yaml

  # HelmRepository sources (FluxCD chart registries)
  - sources/cert-manager.yaml
  - sources/mariadb-operator.yaml
  - sources/external-secrets.yaml
  - sources/openbao.yaml
  - sources/c5c3-charts.yaml

  # HelmRelease operators (deployed via FluxCD)
  - releases/cert-manager.yaml
  - releases/mariadb-operator.yaml
  - releases/external-secrets.yaml
  - releases/memcached-operator.yaml
```

### `deploy/flux-system/infrastructure/kustomization.yaml:15-18`

```
resources:
  - cluster-issuer.yaml
  - mariadb.yaml
  - memcached.yaml
```
### `deploy/flux-system/namespaces.yaml:8-32`

```
---
apiVersion: v1
kind: Namespace
metadata:
  name: cert-manager
---
apiVersion: v1
kind: Namespace
metadata:
  name: mariadb-system
---
apiVersion: v1
kind: Namespace
metadata:
  name: external-secrets
---
apiVersion: v1
kind: Namespace
metadata:
  name: memcached-system
---
apiVersion: v1
kind: Namespace
metadata:
  name: openstack
```

### `deploy/flux-system/kustomization.yaml:19-21`

```
resources:
  # Namespace resources (applied first by kustomize resource ordering)
  - namespaces.yaml
```


