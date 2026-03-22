---
title: Infrastructure Manifests
quadrant: infrastructure
feature: CC-0008
---

# Infrastructure Manifests

Reference documentation for the FluxCD infrastructure manifests (CC-0008). These manifests
define HelmRepository sources, HelmRelease operators, and infrastructure custom resources
that provision the shared platform services required by OpenStack operators. Deployment is
split into two phases: base resources (namespaces, sources, releases) and CRD-dependent
infrastructure resources (applied after operators install their CRDs).

## Directory Layout

```text
deploy/
└── flux-system/
    ├── kustomization.yaml            Base kustomize overlay (namespaces, sources, releases)
    ├── namespaces.yaml               Namespace resources for all components
    ├── sources/                      FluxCD HelmRepository CRs
    │   ├── cert-manager.yaml         Jetstack Helm chart registry
    │   ├── mariadb-operator.yaml     MariaDB Operator Helm chart registry
    │   ├── external-secrets.yaml     External Secrets Operator Helm chart registry
    │   ├── openbao.yaml              OpenBao Helm chart registry
    │   └── c5c3-charts.yaml          C5C3 shared OCI chart registry
    ├── releases/                     FluxCD HelmRelease CRs
    │   ├── cert-manager.yaml         cert-manager
    │   ├── mariadb-operator.yaml     MariaDB Operator
    │   ├── external-secrets.yaml     External Secrets Operator
    │   └── memcached-operator.yaml   Memcached Operator (from c5c3-charts)
    └── infrastructure/               CRD-dependent infrastructure resources
        ├── kustomization.yaml        Infrastructure kustomize overlay
        ├── cluster-issuer.yaml       Self-signed ClusterIssuer (requires cert-manager CRDs)
        ├── mariadb.yaml              MariaDB Galera cluster for OpenStack
        └── memcached.yaml            Memcached cluster for OpenStack
```

All YAML files carry the SPDX Apache-2.0 license header (3 lines: copyright, blank
comment, license identifier).

## Namespaces

Five `Namespace` resources are defined in `namespaces.yaml` and included as the first
entry in the base kustomization. Kustomize applies `Namespace` resources before other
resource kinds, ensuring target namespaces exist before any namespaced resources are
created.

| Namespace | Purpose |
| --- | --- |
| `cert-manager` | cert-manager operator and its resources |
| `mariadb-system` | MariaDB Operator |
| `external-secrets` | External Secrets Operator |
| `memcached-system` | Memcached Operator |
| `openstack` | Infrastructure instance CRs (MariaDB cluster, Memcached cluster) |

**Note:** The `install.createNamespace: true` setting on HelmReleases instructs FluxCD's
helm-controller to create namespaces when installing charts. However, this does not help
when applying HelmRelease CRs via `kubectl apply -k` — the target namespace must already
exist for the API server to accept namespaced resources. The explicit `Namespace` resources
solve this chicken-and-egg problem.

## HelmRepository Sources

Five HelmRepository CRs define the Helm chart registries that FluxCD pulls from. All
use `apiVersion: source.toolkit.fluxcd.io/v1`, are deployed to the `flux-system`
namespace, and poll at `interval: 1h`.

| File | `metadata.name` | Registry URL | Type |
| --- | --- | --- | --- |
| `sources/cert-manager.yaml` | `cert-manager` | `https://charts.jetstack.io` | HTTPS |
| `sources/mariadb-operator.yaml` | `mariadb-operator` | `https://mariadb-operator.github.io/mariadb-operator` | HTTPS |
| `sources/external-secrets.yaml` | `external-secrets` | `https://charts.external-secrets.io` | HTTPS |
| `sources/openbao.yaml` | `openbao` | `https://openbao.github.io/openbao-helm` | HTTPS |
| `sources/c5c3-charts.yaml` | `c5c3-charts` | `oci://ghcr.io/c5c3/charts` | OCI |

The `c5c3-charts` repository is the only OCI-type source (`spec.type: oci`). It hosts
internally-built operator charts (e.g., memcached-operator) in the GitHub Container
Registry. All other repositories use standard HTTPS Helm registries.

## HelmRelease Operators

Four HelmRelease CRs deploy the infrastructure operators. All use
`apiVersion: helm.toolkit.fluxcd.io/v2` and share these common settings:

| Setting | Value | Purpose |
| --- | --- | --- |
| `spec.interval` | `30m` | Reconciliation interval |
| `spec.install.crds` | `CreateReplace` | Install CRDs if missing, replace if outdated |
| `spec.install.createNamespace` | `true` | Auto-create target namespace |
| `spec.upgrade.crds` | `CreateReplace` | Update CRDs on chart upgrade |
| `spec.upgrade.remediation.retries` | `3` | Retry failed upgrades up to 3 times |

### Dependency Order

cert-manager is the base layer (no `dependsOn`). All other operators depend on
cert-manager because they require TLS certificates for webhook servers:

```text
cert-manager  (base — no dependencies)
├── mariadb-operator     dependsOn: cert-manager/cert-manager
├── external-secrets     dependsOn: cert-manager/cert-manager
└── memcached-operator   dependsOn: cert-manager/cert-manager
```

FluxCD resolves this dependency graph and installs operators in the correct order.
If cert-manager is not ready, dependent operators are held in a pending state.

### cert-manager

**File:** `deploy/flux-system/releases/cert-manager.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `cert-manager` |
| Chart | `cert-manager` |
| Version constraint | `>=1.16.0 <2.0.0` |
| Source | `cert-manager` HelmRepository |
| Dependencies | None (base layer) |

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `prometheus.enabled` | `true` | Expose Prometheus metrics endpoint |

### MariaDB Operator

**File:** `deploy/flux-system/releases/mariadb-operator.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `mariadb-system` |
| Chart | `mariadb-operator` |
| Version constraint | `>=0.30.0 <1.0.0` |
| Source | `mariadb-operator` HelmRepository |
| Dependencies | `cert-manager` in `cert-manager` namespace |

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `metrics.enabled` | `true` | Expose Prometheus metrics |
| `webhook.enabled` | `true` | Enable admission webhooks for MariaDB CRDs |

### External Secrets Operator

**File:** `deploy/flux-system/releases/external-secrets.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `external-secrets` |
| Chart | `external-secrets` |
| Version constraint | `>=0.10.0 <1.0.0` |
| Source | `external-secrets` HelmRepository |
| Dependencies | `cert-manager` in `cert-manager` namespace |

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `webhook.port` | `9443` | Webhook server listen port |
| `certController.enabled` | `true` | Manage webhook TLS certificates |

### Memcached Operator

**File:** `deploy/flux-system/releases/memcached-operator.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `memcached-system` |
| Chart | `memcached-operator` |
| Version constraint | `>=0.1.0 <1.0.0` |
| Source | `c5c3-charts` HelmRepository (shared OCI registry) |
| Dependencies | `cert-manager` in `cert-manager` namespace |

**Source reference:** The Memcached Operator chart is published to the shared `c5c3-charts`
OCI registry (`oci://ghcr.io/c5c3/charts`), not a dedicated HelmRepository. The
`sourceRef.name` is `c5c3-charts`, matching the OCI HelmRepository in `sources/`.

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `metrics.enabled` | `true` | Expose Prometheus metrics |
| `webhook.enabled` | `true` | Enable admission webhooks for Memcached CRDs |

## HelmRelease–HelmRepository Cross-Reference

Each HelmRelease `sourceRef.name` must match a HelmRepository `metadata.name` in
`sources/`. This table shows the mapping:

| HelmRelease | `sourceRef.name` | HelmRepository file |
| --- | --- | --- |
| `cert-manager` | `cert-manager` | `sources/cert-manager.yaml` |
| `mariadb-operator` | `mariadb-operator` | `sources/mariadb-operator.yaml` |
| `external-secrets` | `external-secrets` | `sources/external-secrets.yaml` |
| `memcached-operator` | `c5c3-charts` | `sources/c5c3-charts.yaml` |

The `openbao` HelmRepository (`sources/openbao.yaml`) has no corresponding HelmRelease
yet — it is provisioned for future use (CC-0009).

## Infrastructure Custom Resources

Infrastructure CRs are instance-level resources managed by the operators installed via
HelmReleases above. They are separated into their own kustomization
(`infrastructure/kustomization.yaml`) because they depend on CRDs that are only available
after the corresponding operator HelmReleases install their Helm charts.

### Self-Signed ClusterIssuer

**File:** `deploy/flux-system/infrastructure/cluster-issuer.yaml`

| Property | Value |
| --- | --- |
| API version | `cert-manager.io/v1` |
| Kind | `ClusterIssuer` |
| Name | `selfsigned-cluster-issuer` |
| Scope | Cluster-scoped (no namespace) |

The self-signed ClusterIssuer provides a default certificate issuer for development
environments. It requires cert-manager CRDs (`cert-manager.io/v1`) which are installed
by the cert-manager HelmRelease.

### MariaDB Galera Cluster

**File:** `deploy/flux-system/infrastructure/mariadb.yaml`

| Property | Value |
| --- | --- |
| API version | `k8s.mariadb.com/v1alpha1` |
| Kind | `MariaDB` |
| Name | `openstack-db` |
| Namespace | `openstack` |
| Replicas | `3` |
| Galera | Enabled (`spec.galera.enabled: true`) |
| MaxScale | Enabled, 2 replicas (`spec.maxScale.enabled: true`, `spec.maxScale.replicas: 2`) |
| Storage | `100Gi`, storage class `ceph-rbd` |

The MariaDB CR provisions a 3-node Galera cluster with synchronous replication managed
by the mariadb-operator. MaxScale is enabled with 2 replicas to provide intelligent query
routing and read/write splitting across the Galera nodes.

The root password is sourced from a Kubernetes Secret (`mariadb-root-password`, key
`password`) — secret provisioning is handled by the External Secrets Operator integration
(CC-0009).

**Services:**

| Service | Type | Purpose |
| --- | --- | --- |
| Primary | `ClusterIP` | Read-write endpoint for application connections |
| Secondary | `ClusterIP` | Read-only endpoint for read replicas |

**Monitoring:** Prometheus metrics are enabled (`spec.metrics.enabled: true`).

### Memcached Cluster

**File:** `deploy/flux-system/infrastructure/memcached.yaml`

| Property | Value |
| --- | --- |
| API version | `memcached.c5c3.io/v1beta1` |
| Kind | `Memcached` |
| Name | `openstack-memcached` |
| Namespace | `openstack` |
| Replicas | `3` |
| Image | `memcached:1.6` |

The Memcached CR provisions a 3-replica Memcached cluster for OpenStack session and
token caching. The memcached-operator manages pod lifecycle and provides stable DNS-based
service discovery for operator consumers.

**API group:** The API group is `memcached.c5c3.io`, matching the CRD definition
shipped by the [memcached-operator](https://github.com/C5C3/memcached-operator) Helm chart.

## Kustomization

Deployment is split into two kustomize overlays to separate base resources from
CRD-dependent infrastructure resources:

### Base Kustomization

**File:** `deploy/flux-system/kustomization.yaml`

The base kustomization uses `apiVersion: kustomize.config.k8s.io/v1beta1` and includes
namespaces, HelmRepository sources, and HelmRelease operators. These resources do not
depend on any custom CRDs.

**Resource count:** 10 files producing 14 Kubernetes resources.

| Category | Count | Resources |
| --- | --- | --- |
| Namespace | 5 | cert-manager, mariadb-system, external-secrets, memcached-system, openstack |
| HelmRepository | 5 | cert-manager, mariadb-operator, external-secrets, openbao, c5c3-charts |
| HelmRelease | 4 | cert-manager, mariadb-operator, external-secrets, memcached-operator |
| **Total** | **14** | |

### Infrastructure Kustomization

**File:** `deploy/flux-system/infrastructure/kustomization.yaml`

The infrastructure kustomization includes CRD-dependent resources that require their
operator CRDs to be installed first. This kustomization must be applied after the base
kustomization and after operators have finished installing their CRDs.

**Resource count:** 3 files producing 3 Kubernetes resources.

| Category | Count | Resources |
| --- | --- | --- |
| ClusterIssuer | 1 | selfsigned-cluster-issuer (requires cert-manager CRDs) |
| MariaDB | 1 | openstack-db (requires mariadb-operator CRDs) |
| Memcached | 1 | openstack-memcached (requires memcached-operator CRDs) |
| **Total** | **3** | |

## Deployment

### Step 1: Apply base resources

```bash
kubectl apply -k deploy/flux-system/
```

This applies 14 resources: 5 namespaces, 5 HelmRepository sources, and 4 HelmRelease
operators. FluxCD resolves the dependency graph between HelmReleases and installs
operators in the correct order. Wait for all operators to finish installing before
proceeding to step 2.

### Step 2: Apply infrastructure resources

```bash
kubectl apply -k deploy/flux-system/infrastructure/
```

This applies 3 CRD-dependent resources: the ClusterIssuer, MariaDB cluster, and
Memcached cluster. These resources require CRDs that are installed by the operator
HelmReleases in step 1. If CRDs are not yet available, the apply will fail — wait
for the operators to finish installing and retry.

> **Expected transient failure:** The MariaDB cluster references a
> `rootPasswordSecretKeyRef` Secret (`mariadb-root-password`) that is provisioned by
> the External Secrets Operator integration (CC-0009). Until that Secret exists, the
> mariadb-operator will enter a failed reconciliation loop with
> `Secret "mariadb-root-password" not found` errors. This is expected and resolves
> automatically once CC-0009 is applied.

### Validate manifests locally

```bash
kustomize build deploy/flux-system/
kustomize build deploy/flux-system/infrastructure/
```

These commands render the manifest output without applying it. Use them to verify YAML
syntax and resource inclusion before deployment.

### Prerequisites

- A Kubernetes cluster with FluxCD installed (source-controller and helm-controller)
- `kubectl` configured with cluster access
- For local validation only: `kustomize` CLI

## Extensibility

The manifest structure is designed for straightforward extension. Adding a new operator
(e.g., OpenBao for CC-0009) requires four steps:

1. **Add a source file** in `sources/` (e.g., `sources/openbao.yaml`) — or reuse an
   existing HelmRepository if the chart is in a shared registry
2. **Add a release file** in `releases/` (e.g., `releases/openbao.yaml`) with the
   HelmRelease CR, `dependsOn` for cert-manager, and the standard install/upgrade settings
3. **Add both paths** to the `resources` list in `kustomization.yaml`
4. **Add the operator namespace** to `namespaces.yaml` (e.g., `openbao-system`) so the
   namespace exists before `kubectl apply -k` creates the namespaced HelmRelease CR

Infrastructure instance CRs (e.g., a new database or cache cluster) follow the same
pattern: add a file in `infrastructure/` and list it in
`infrastructure/kustomization.yaml`.

## Design Decisions

### Two-phase kustomization

Resources are split into a base kustomization (namespaces, sources, releases) and an
infrastructure kustomization (CRD-dependent resources). This separation ensures that
`kubectl apply -k` does not attempt to create CRD-dependent resources before the
corresponding CRDs exist. The base kustomization can be applied independently, and the
infrastructure kustomization is applied after operators have installed their CRDs.

In FluxCD-managed clusters, this pattern maps to two FluxCD Kustomization CRs where the
infrastructure Kustomization depends on the base Kustomization (using `spec.dependsOn`),
eliminating noisy first-apply failures.

### Explicit namespace resources

All target namespaces are defined as explicit `Namespace` resources in `namespaces.yaml`.
While HelmReleases set `install.createNamespace: true` for FluxCD's helm-controller, the
explicit namespace resources ensure namespaces exist before `kubectl apply -k` attempts
to create namespaced resources (HelmRelease CRs specify a target namespace in their
metadata).

### Namespace auto-creation

All HelmReleases set `install.createNamespace: true` as a safety net for FluxCD
deployments. This is complementary to the explicit `Namespace` resources — the explicit
resources handle the `kubectl apply -k` path, while `createNamespace` handles edge cases
in FluxCD reconciliation.

### No secret configuration

The manifests intentionally contain no password, credential, or secret configuration.
Secret management is handled by the External Secrets Operator integration (CC-0009),
which provisions secrets from an external vault into the cluster.

### Memcached Operator source

The Memcached Operator chart is sourced from the shared `c5c3-charts` OCI registry
rather than a dedicated HelmRepository. This follows the project convention of publishing
internally-built charts to `oci://ghcr.io/c5c3/charts` (see
`architecture/docs/09-implementation/07-ci-cd-and-packaging.md`).
