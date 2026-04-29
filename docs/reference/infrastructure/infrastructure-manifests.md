---
title: Infrastructure Manifests
quadrant: infrastructure
---

# Infrastructure Manifests

Reference documentation for the FluxCD infrastructure manifests. These manifests
define HelmRepository sources, HelmRelease operators, and infrastructure custom resources
that provision the shared platform services required by OpenStack operators. Deployment is
split into two phases: base resources (namespaces, sources, releases) and CRD-dependent
infrastructure resources (applied after operators install their CRDs).

## Directory Layout

```text
deploy/
└── flux-system/
    ├── kustomization.yaml                Base kustomize overlay (namespaces, FluxInstance, sources, releases)
    ├── namespaces.yaml                   Namespace resources for all components
    ├── fluxinstance.yaml                 FluxInstance CR driving the flux-operator
    ├── sources/                          FluxCD HelmRepository CRs
    │   ├── cert-manager.yaml             Jetstack Helm chart registry
    │   ├── mariadb-operator.yaml         MariaDB Operator Helm chart registry
    │   ├── external-secrets.yaml         External Secrets Operator Helm chart registry
    │   ├── openbao.yaml                  OpenBao Helm chart registry
    │   ├── c5c3-charts.yaml              C5C3 shared OCI chart registry
    │   ├── prometheus-community.yaml     Prometheus Community OCI chart registry
    │   └── chaos-mesh.yaml               Chaos Mesh Helm chart registry (kind-only addon — see "Kind Overlay Demo Addons")
    ├── releases/                         FluxCD HelmRelease CRs
    │   ├── cert-manager.yaml             cert-manager
    │   ├── prometheus-operator-crds.yaml Prometheus Operator CRDs
    │   ├── mariadb-operator-crds.yaml    MariaDB Operator CRDs
    │   ├── mariadb-operator.yaml         MariaDB Operator
    │   ├── external-secrets.yaml         External Secrets Operator
    │   ├── memcached-operator.yaml       Memcached Operator (from c5c3-charts)
    │   ├── openbao.yaml                  OpenBao HA Raft cluster
    │   ├── keystone-operator.yaml        Keystone Operator (from c5c3-charts)
    │   └── chaos-mesh.yaml               Chaos Mesh (kind-only addon — see "Kind Overlay Demo Addons")
    └── infrastructure/                   CRD-dependent infrastructure resources
        ├── kustomization.yaml            Infrastructure kustomize overlay
        ├── cluster-issuer.yaml           Self-signed ClusterIssuer (requires cert-manager CRDs)
        ├── mariadb.yaml                  MariaDB Galera cluster for OpenStack
        └── memcached.yaml                Memcached cluster for OpenStack
```

All YAML files carry the SPDX Apache-2.0 license header (3 lines: copyright, blank
comment, license identifier).

## Namespaces

Seven `Namespace` resources are defined in `namespaces.yaml` and included as the first
entry in the base kustomization. Kustomize applies `Namespace` resources before other
resource kinds, ensuring target namespaces exist before any namespaced resources are
created.

| Namespace | Purpose |
| --- | --- |
| `cert-manager` | cert-manager operator and its resources |
| `mariadb-system` | MariaDB Operator |
| `external-secrets` | External Secrets Operator |
| `monitoring-system` | Prometheus Operator CRDs |
| `memcached-system` | Memcached Operator |
| `openstack` | Infrastructure instance CRs (MariaDB cluster, Memcached cluster) |
| `openbao-system` | OpenBao HA Raft cluster |

The `chaos-mesh` namespace is **not** part of the production base. It is created
inline by the kind-only opt-in overlay at `deploy/kind/chaos-mesh/` when
`WITH_CHAOS_MESH=true make deploy-infra` is used. See
[Chaos Mesh (kind-only opt-in)](#chaos-mesh-kind-only-opt-in) below.

**Note:** The `install.createNamespace: true` setting on HelmReleases instructs FluxCD's
helm-controller to create namespaces when installing charts. However, this does not help
when applying HelmRelease CRs via `kubectl apply -k` — the target namespace must already
exist for the API server to accept namespaced resources. The explicit `Namespace` resources
solve this chicken-and-egg problem.

## FluxInstance

**File:** `deploy/flux-system/fluxinstance.yaml`

A single `FluxInstance` CR drives the
[flux-operator](https://github.com/controlplaneio-fluxcd/flux-operator), which replaces
the imperative `flux install` / `flux bootstrap` path with a declarative,
operator-managed Flux lifecycle. The flux-operator reconciles the Flux
controller Deployments from this spec and publishes a `FluxReport/flux` summarizing the
installation state.

| Property | Value |
| --- | --- |
| API version | `fluxcd.controlplane.io/v1` |
| Kind | `FluxInstance` |
| Name | `flux` |
| Namespace | `flux-system` |

**Spec fields:**

| Field | Value | Purpose |
| --- | --- | --- |
| `distribution.version` | `"2.x"` | Minor-version track pinned by the operator; picks the latest Flux 2.x release |
| `distribution.registry` | `ghcr.io/fluxcd` | Controller image registry |
| `components` | `source-controller`, `kustomize-controller`, `helm-controller`, `notification-controller` | Four Flux controllers installed — image-automation and image-reflector controllers are omitted (not used in this project) |
| `cluster.type` | `kubernetes` | Generic Kubernetes distribution (not OpenShift/EKS-specific) |
| `cluster.size` | `small` | Small resource profile suitable for single-node kind and low-traffic management clusters |
| `cluster.multitenant` | `false` | Cross-namespace references allowed — simplifies the single-tenant management cluster model |
| `cluster.networkPolicy` | `false` | No NetworkPolicies applied to flux-system (kind overlay assumes a permissive default; production overlays opt in) |

**No `spec.sync` block.** The kind Quick Start applies Helm sources and releases
directly via `kubectl apply -k deploy/kind/base/`, so the `FluxInstance` here does not
carry a `GitRepository` sync. Production overlays that want continuous reconciliation
from Git add a `spec.sync` block on top of this base.

**Kustomize ordering.** Kustomize applies `Namespace` resources first by default, so
`flux-system` exists before the `FluxInstance` is created. The flux-operator itself is
installed out-of-band by `hack/deploy-infra.sh` (pinned `FLUX_OPERATOR_VERSION`,
applied via `kubectl apply -f install.yaml`) before this kustomization is applied.

## HelmRepository Sources

Six HelmRepository CRs define the Helm chart registries that FluxCD pulls from. All
use `apiVersion: source.toolkit.fluxcd.io/v1`, are deployed to the `flux-system`
namespace, and poll at `interval: 1h`.

| File | `metadata.name` | Registry URL | Type |
| --- | --- | --- | --- |
| `sources/cert-manager.yaml` | `cert-manager` | `https://charts.jetstack.io` | HTTPS |
| `sources/mariadb-operator.yaml` | `mariadb-operator` | `https://mariadb-operator.github.io/mariadb-operator` | HTTPS |
| `sources/external-secrets.yaml` | `external-secrets` | `https://charts.external-secrets.io` | HTTPS |
| `sources/openbao.yaml` | `openbao` | `https://openbao.github.io/openbao-helm` | HTTPS |
| `sources/c5c3-charts.yaml` | `c5c3-charts` | `oci://ghcr.io/c5c3/charts` | OCI |
| `sources/prometheus-community.yaml` | `prometheus-community` | `oci://ghcr.io/prometheus-community/charts` | OCI |

The `chaos-mesh` HelmRepository ships in the kind-only opt-in overlay at
`deploy/kind/chaos-mesh/source.yaml` — it is intentionally absent
from `deploy/flux-system/{sources,kustomization.yaml}`. See
[Chaos Mesh (kind-only opt-in)](#chaos-mesh-kind-only-opt-in).

The `c5c3-charts` and `prometheus-community` repositories are OCI-type sources
(`spec.type: oci`). `c5c3-charts` hosts internally-built operator charts (e.g.,
memcached-operator) in the GitHub Container Registry. `prometheus-community` hosts
Prometheus community charts (e.g., prometheus-operator-crds). All other repositories
use standard HTTPS Helm registries.

## HelmRelease Operators

Eight HelmRelease CRs deploy the infrastructure operators and CRD charts. All use
`apiVersion: helm.toolkit.fluxcd.io/v2` and share these common settings:

| Setting | Value | Purpose |
| --- | --- | --- |
| `spec.interval` | `30m` | Reconciliation interval |
| `spec.install.crds` | `CreateReplace` | Install CRDs if missing, replace if outdated |
| `spec.install.createNamespace` | `true` | Auto-create target namespace |
| `spec.upgrade.crds` | `CreateReplace` | Update CRDs on chart upgrade |
| `spec.upgrade.remediation.retries` | `3` | Retry failed upgrades up to 3 times |

### Dependency Order

cert-manager is the base layer (no `dependsOn`). The CRD-only charts
(prometheus-operator-crds, mariadb-operator-crds) also have no dependencies. All other
operators depend on cert-manager because they require TLS certificates for webhook
servers. Some operators have additional dependencies on CRD charts or other operators:

```text
cert-manager              (base — no dependencies)
prometheus-operator-crds  (no dependencies)
mariadb-operator-crds     (no dependencies)
├── mariadb-operator      dependsOn: cert-manager, mariadb-operator-crds
├── external-secrets      dependsOn: cert-manager
├── memcached-operator    dependsOn: cert-manager, prometheus-operator-crds
├── openbao               dependsOn: cert-manager
└── keystone-operator     dependsOn: cert-manager, mariadb-operator, memcached-operator, external-secrets
```

FluxCD resolves this dependency graph and installs operators in the correct order.
If cert-manager is not ready, dependent operators are held in a pending state.

The kind-only `chaos-mesh` HelmRelease (`deploy/kind/chaos-mesh/`) also
declares `dependsOn: cert-manager` but is only installed when
`WITH_CHAOS_MESH=true make deploy-infra` is used. Production overlays do not
install it. See [Chaos Mesh (kind-only opt-in)](#chaos-mesh-kind-only-opt-in).

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
| `crds.enabled` | `true` | Install CRDs via the Helm chart |
| `prometheus.enabled` | `false` | Prometheus metrics disabled |
| `startupapicheck.enabled` | `false` | Disable startup API check job |

### Prometheus Operator CRDs

**File:** `deploy/flux-system/releases/prometheus-operator-crds.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `monitoring-system` |
| Chart | `prometheus-operator-crds` |
| Version constraint | `>=17.0.0 <20.0.0` |
| Source | `prometheus-community` HelmRepository |
| Dependencies | None |

The Prometheus Operator CRDs chart installs ServiceMonitor, PodMonitor, PrometheusRule,
and related monitoring.coreos.com CRDs. These are required by the memcached-operator
controller, which unconditionally watches ServiceMonitor resources via Owns().

### MariaDB Operator CRDs

**File:** `deploy/flux-system/releases/mariadb-operator-crds.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `mariadb-system` |
| Chart | `mariadb-operator-crds` |
| Version constraint | `>=0.30.0 <1.0.0` |
| Source | `mariadb-operator` HelmRepository |
| Dependencies | None |

A separate CRD chart is required since mariadb-operator v0.35.0. Must be installed before
mariadb-operator so CRDs are available for the operator and for infrastructure CRs
(e.g., MariaDB Galera cluster).

### MariaDB Operator

**File:** `deploy/flux-system/releases/mariadb-operator.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `mariadb-system` |
| Chart | `mariadb-operator` |
| Version constraint | `>=0.30.0 <1.0.0` |
| Source | `mariadb-operator` HelmRepository |
| Dependencies | `cert-manager` in `cert-manager` namespace, `mariadb-operator-crds` in `mariadb-system` namespace |

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `metrics.enabled` | `false` | Prometheus metrics disabled |
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
| `installCRDs` | `true` | Install CRDs via the Helm chart |
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
| Dependencies | `cert-manager` in `cert-manager` namespace, `prometheus-operator-crds` in `monitoring-system` namespace |

**Source reference:** The Memcached Operator chart is published to the shared `c5c3-charts`
OCI registry (`oci://ghcr.io/c5c3/charts`), not a dedicated HelmRepository. The
`sourceRef.name` is `c5c3-charts`, matching the OCI HelmRepository in `sources/`.

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `metrics.enabled` | `true` | Expose Prometheus metrics |
| `webhook.enabled` | `true` | Enable admission webhooks for Memcached CRDs |

### OpenBao

**File:** `deploy/flux-system/releases/openbao.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `openbao-system` |
| Chart | `openbao` |
| Version constraint | `>=0.5.0 <1.0.0` |
| Source | `openbao` HelmRepository |
| Dependencies | `cert-manager` in `cert-manager` namespace |

OpenBao is deployed as a 3-replica HA Raft cluster with TLS enabled. The injector is
disabled. TLS certificates are sourced from a cert-manager-provisioned Secret
(`openbao-tls`). See `architecture/docs/09-implementation/09-openbao-deployment.md` for
design rationale.

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `global.tlsDisable` | `false` | Enable TLS globally |
| `server.authDelegator.enabled` | `true` | Enable ClusterRoleBinding for TokenReview API (ESO auth) |
| `server.ha.enabled` | `true` | Enable HA mode |
| `server.ha.replicas` | `3` | 3-node Raft cluster |
| `server.ha.raft.enabled` | `true` | Use Raft storage backend |
| `server.dataStorage.size` | `10Gi` | Persistent volume size |
| `injector.enabled` | `false` | Disable the Vault/Bao agent injector |

### Keystone Operator

**File:** `deploy/flux-system/releases/keystone-operator.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `openstack` |
| Chart | `keystone-operator` |
| Version constraint | `>=0.1.0 <1.0.0` |
| Source | `c5c3-charts` HelmRepository (shared OCI registry) |
| Dependencies | `cert-manager`, `mariadb-operator`, `memcached-operator`, `external-secrets` |

The Keystone Operator manages OpenStack Keystone identity service instances. It depends
on four upstream operators: cert-manager for TLS, mariadb-operator for database
provisioning, memcached-operator for caching, and external-secrets for secret management.

**Helm values:**

| Key | Value | Purpose |
| --- | --- | --- |
| `replicas` | `2` | Run 2 controller replicas for HA |
| `leaderElection.enabled` | `true` | Enable leader election for HA |
| `image.tag` | `latest` | Use latest image until a versioned release publishes a semver tag |

## HelmRelease–HelmRepository Cross-Reference

Each HelmRelease `sourceRef.name` must match a HelmRepository `metadata.name` in
`sources/`. This table shows the mapping:

| HelmRelease | `sourceRef.name` | HelmRepository file |
| --- | --- | --- |
| `cert-manager` | `cert-manager` | `sources/cert-manager.yaml` |
| `prometheus-operator-crds` | `prometheus-community` | `sources/prometheus-community.yaml` |
| `mariadb-operator-crds` | `mariadb-operator` | `sources/mariadb-operator.yaml` |
| `mariadb-operator` | `mariadb-operator` | `sources/mariadb-operator.yaml` |
| `external-secrets` | `external-secrets` | `sources/external-secrets.yaml` |
| `memcached-operator` | `c5c3-charts` | `sources/c5c3-charts.yaml` |
| `openbao` | `openbao` | `sources/openbao.yaml` |
| `keystone-operator` | `c5c3-charts` | `sources/c5c3-charts.yaml` |

The kind-only `chaos-mesh` HelmRelease ships in the opt-in overlay at
`deploy/kind/chaos-mesh/release.yaml`, with its own local
`source.yaml`. It is intentionally absent from this always-on table because
production overlays do not install it.

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
`password`) — secret provisioning is handled by the External Secrets Operator integration.

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
namespaces, the FluxInstance CR, HelmRepository sources, and HelmRelease operators.
These resources do not depend on any custom CRDs.

**Resource count:** 16 files producing 22 Kubernetes resources.

| Category | Count | Resources |
| --- | --- | --- |
| Namespace | 7 | cert-manager, mariadb-system, external-secrets, monitoring-system, memcached-system, openstack, openbao-system |
| FluxInstance | 1 | flux (drives the flux-operator) |
| HelmRepository | 6 | cert-manager, mariadb-operator, external-secrets, openbao, c5c3-charts, prometheus-community |
| HelmRelease | 8 | cert-manager, prometheus-operator-crds, mariadb-operator-crds, mariadb-operator, external-secrets, memcached-operator, openbao, keystone-operator |
| **Total** | **22** | |

The `chaos-mesh` HelmRepository, HelmRelease, and Namespace ship in the
kind-only opt-in overlay at `deploy/kind/chaos-mesh/` and are not
counted here.

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

This applies 22 resources: 7 namespaces, 1 FluxInstance, 6 HelmRepository
sources, and 8 HelmRelease operators. FluxCD resolves the dependency graph between
HelmReleases and installs operators in the correct order. Wait for all operators to
finish installing before proceeding to step 2.

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
> the External Secrets Operator integration. Until that Secret exists, the
> mariadb-operator will enter a failed reconciliation loop with
> `Secret "mariadb-root-password" not found` errors. This is expected and resolves
> automatically once OpenBao bootstrap is applied.

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
(e.g., OpenBao) requires four steps:

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
Secret management is handled by the External Secrets Operator integration,
which provisions secrets from an external vault into the cluster.

### Memcached Operator source

The Memcached Operator chart is sourced from the shared `c5c3-charts` OCI registry
rather than a dedicated HelmRepository. This follows the project convention of publishing
internally-built charts to `oci://ghcr.io/c5c3/charts` (see
`architecture/docs/09-implementation/07-ci-cd-and-packaging.md`).

## Kind Overlay Demo Addons

The kind overlay (`deploy/kind/base/kustomization.yaml`) layers a small set of
kind-only demo manifests on top of the production base. These files live under
`deploy/kind/base/` and are **not** referenced from `deploy/flux-system/kustomization.yaml`,
so they never reach production clusters. The section below catalogues these addons;
earlier kind-only manifests (Headlamp, OpenBao UI patch) are documented in the Quick Start.
Chaos Mesh ships as a
separate **opt-in** kind overlay at `deploy/kind/chaos-mesh/` — applied only when
`WITH_CHAOS_MESH=true` is set on `make deploy-infra`; see
[Chaos Mesh (kind-only opt-in)](#chaos-mesh-kind-only-opt-in) below.

### Flux Web UI ResourceSet

**File:** `deploy/kind/base/flux-web.yaml`

A single `ResourceSet` CR drives the flux-operator's bundled
[Flux Web UI](https://fluxoperator.dev/web-ui/) as a demo surface for the kind
Quick Start (Step 4a). The `ResourceSet` renders two sibling resources — an
`OCIRepository` pointing at the official flux-operator Helm chart and a
`HelmRelease` that installs that chart with only the Web UI sub-chart enabled.

| Property | Value |
| --- | --- |
| API version | `fluxcd.controlplane.io/v1` |
| Kind | `ResourceSet` |
| Name | `flux-web` |
| Namespace | `flux-system` |
| Chart URL | `oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator` |
| Version pin (input) | `0.47.x` — SemVer range locked to the minor track of `FLUX_OPERATOR_VERSION` in `hack/deploy-infra.sh` |

**Helm values on the nested `HelmRelease`:**

| Key | Value | Purpose |
| --- | --- | --- |
| `web.serverOnly` | `true` | Render only the Web UI Deployment + Service; skip the operator Deployment, CRDs, and RBAC that the original `install.yaml` bootstrap already owns |
| `installCRDs` | `false` | The flux-operator CRDs (`FluxInstance`, `ResourceSet`, `ResourceSetInputProvider`, …) are already installed by the out-of-band `install.yaml` apply in `hack/deploy-infra.sh` — re-applying them here would fight the bootstrap on every reconcile |
| `fullnameOverride` | `flux-web` | Give the Web UI Deployment / Service / ServiceAccount a distinct identity so they do not collide with the operator's own `flux-operator-*` workload names |

**Version tracking.** The `spec.inputs[0].version` SemVer range is updated
automatically by a Renovate `customManager` entry in `renovate.json` that
targets `deploy/kind/base/flux-web.yaml` and pulls release metadata from
`controlplaneio-fluxcd/flux-operator` GitHub releases. The customManager shares
the same `packageRules` as `hack/deploy-infra.sh` — major upgrades are
disabled, minor/patch upgrades auto-merge after a three-day `minimumReleaseAge`
cooldown.

**Production opt-out.** `deploy/flux-system/kustomization.yaml` deliberately
does **not** list `deploy/kind/base/flux-web.yaml`. The flux-operator Web UI
ships without token authentication, without TLS termination, and without an
Ingress story — it is safe as a localhost port-forward demo on a single-node
kind cluster, not as a shared-cluster surface. Production overlays can opt
back in explicitly once upstream adds those prerequisites.

**Access (kind Quick Start, Step 4a):**

```bash
kubectl port-forward svc/flux-web -n flux-system 9080:9080
```

Browse <http://localhost:9080> — no login required. The Web UI complements
Headlamp by rendering the three flux-operator-specific CRDs (`ResourceSet`,
`ResourceSetInputProvider`, `FluxReport`) that the generic Headlamp Flux
plugin does not know about.

### Chaos Mesh (kind-only opt-in)

**File:** `deploy/kind/chaos-mesh/kustomization.yaml`

[Chaos Mesh](https://chaos-mesh.org/) ships as a separate **opt-in** kind
overlay. The default `make deploy-infra` flow does **not** install
it — first-run deployments skip the privileged `chaos-daemon` DaemonSet, the
`chaos-mesh` namespace, and the upstream HelmRepository / HelmRelease pair so
that developers who never run chaos E2E suites pay zero install cost. The
production `deploy/flux-system/` overlay also does not install Chaos Mesh.

The overlay is self-contained: the `HelmRepository` lives in
`deploy/kind/chaos-mesh/source.yaml` and the `HelmRelease` in
`deploy/kind/chaos-mesh/release.yaml` (both relocated from the former
`deploy/flux-system/{sources,releases}/chaos-mesh.yaml` locations). The
overlay bundles them with:

| Property | Value |
| --- | --- |
| Target namespace | `chaos-mesh` (created inline with the privileged PodSecurity label required by `chaos-daemon`'s host PID/network access) |
| Chart | `chaos-mesh` |
| Version constraint | `>=2.6.0 <3.0.0` |
| Source | `chaos-mesh` HelmRepository (`deploy/kind/chaos-mesh/source.yaml`) |
| Dependencies | `cert-manager` in `cert-manager` namespace |

**Kind-tuning patch** (relocated here from
`deploy/kind/base/kustomization.yaml` because kustomize requires the patch
target to live in the same overlay):

| Helm value | Override | Purpose |
| --- | --- | --- |
| `chaosDaemon.runtime` | `containerd` | Match the kind node's container runtime |
| `chaosDaemon.socketPath` | `/run/containerd/containerd.sock` | Mount the kind containerd socket so chaos-daemon can attack pods |
| `chaosDaemon.resources` | `25m / 64Mi` requests | Reduce footprint on single-node kind |
| `dashboard.create` | `false` | Dashboard is unnecessary in CI |
| `controllerManager.resources` | `25m / 64Mi` requests | Reduce footprint on single-node kind |

These overrides diverge intentionally from the upstream chart defaults
(dashboard enabled, larger resource requests, auto-detected runtime), which
target multi-node production clusters. Because the patch and the
HelmRelease both live in the kind-only overlay, production environments that
opt into Chaos Mesh start from the upstream defaults instead of inheriting
the kind-tuning values.

**No load-restrictor flag required.** The overlay has no parent-directory
`../../` references — every resource (`namespace.yaml`, `source.yaml`,
`release.yaml`) lives under `deploy/kind/chaos-mesh/`. Kustomize's default
`LoadRestrictionsRootOnly` security check is therefore satisfied without
`--load-restrictor=LoadRestrictionsNone`, which matters because kubectl's
embedded kustomize does not expose that flag (kubernetes/kubectl#948) and
`hack/deploy-infra.sh` invokes the apply via `kubectl apply -k`.

**Opt-in usage:**

```bash
WITH_CHAOS_MESH=true make deploy-infra
```

This is the prerequisite for `make e2e-chaos`. See
[Chaos E2E Tests](../testing/chaos-e2e-tests.md) for the full workflow.
