---
title: Core Components
---

# Core Components

The components of the CobaltCore stack, organized by layer. Each entry links
to the reference page that documents it in depth; the way the layers fit
together is described on the [Architecture](./index.md) page.

## Orchestration

The **c5c3-operator** (`operators/c5c3/`) owns the top of the stack. It
reconciles a `ControlPlane` CR into infrastructure CRs, service CRs, and K-ORC
resources, and aggregates their readiness.

| CRD | API group | Description |
| --- | --- | --- |
| `ControlPlane` | `c5c3.io/v1alpha1` | Top-level resource for a whole control plane: infrastructure, services, identity plane ([CRD reference](../reference/c5c3/controlplane-crd.md)) |
| `CredentialRotation` | `c5c3.io/v1alpha1` | Rotates the admin application credential ([reconciler reference](../reference/c5c3/controlplane-reconciler.md)) |
| `SecretAggregate` | `c5c3.io/v1alpha1` | Aggregates secrets from multiple sources into one consumer Secret |
| `KeystoneService` | `c5c3.io/v1alpha1` | Registers one service against a ControlPlane's identity plane: catalog entry and service account, from the service's own namespace ([CRD reference](../reference/c5c3/keystoneservice-crd.md)) |

## Service operators

One operator per OpenStack service (`operators/<service>/`), all built on the
scaffolding the Keystone operator establishes and all following the API-group
convention `<service>.openstack.c5c3.io`.

| Operator | CRDs | Service | Reference |
| --- | --- | --- | --- |
| keystone-operator | `Keystone`, `KeystoneIdentityBackend` | Identity | [Overview](../reference/keystone/) |
| glance-operator | `Glance`, `GlanceBackend` | Image | [Overview](../reference/glance/) |
| placement-operator | `Placement` | Resource tracking | [Overview](../reference/placement/) |
| horizon-operator | `Horizon` | Dashboard | [Overview](../reference/horizon/) |
| barbican-operator | `Barbican`, `BarbicanSecretStore` | Key management | [Overview](../reference/barbican/) |

## OpenStack resource management

**K-ORC**, the upstream
[OpenStack Resource Controller](https://k-orc.cloud/) (API group
`openstack.k-orc.cloud`), manages Keystone resources declaratively: domains,
projects, users, application credentials, catalog services, and endpoints. It
is applied by a Flux `Kustomization` rather than a HelmRelease, and the
c5c3-operator drives its CRs for the admin application credential, the
service accounts, and the catalog. See
[K-ORC in the infrastructure manifests](../reference/infrastructure/infrastructure-manifests.md#k-orc-openstack-resource-controller).

## Infrastructure stack

The HelmReleases under `deploy/flux-system/` install the backing services the
control plane depends on. Their configuration and the CRs built on top are
documented per component in
[Infrastructure Manifests](../reference/infrastructure/infrastructure-manifests.md):

- **cert-manager** — the base layer of the dependency graph; issues webhook
  and endpoint TLS certificates
  ([section](../reference/infrastructure/infrastructure-manifests.md#cert-manager)).
- **mariadb-operator** with a Galera `MariaDB` CR as the shared database
  backend
  ([section](../reference/infrastructure/infrastructure-manifests.md#mariadb-galera-cluster)).
- **memcached-operator** with a `Memcached` CR for token and content caching
  ([section](../reference/infrastructure/infrastructure-manifests.md#memcached-cluster)).
- **rabbitmq-cluster-operator** serving the `RabbitmqCluster` CRD behind a
  ControlPlane's opt-in shared message bus
  ([section](../reference/infrastructure/infrastructure-manifests.md#rabbitmq-cluster-operator)).
- **garage-operator** with a `Garage` CR providing the S3 object store that
  backs Glance multi-store
  ([section](../reference/infrastructure/infrastructure-manifests.md#garage-object-store)).
- **openbao-operator** and the OpenBao proving instance, the secret store for
  the whole stack
  ([section](../reference/infrastructure/infrastructure-manifests.md#openbao-proving-instance),
  [bootstrap](../reference/infrastructure/openbao-bootstrap.md)).
- **External Secrets Operator (ESO)** syncing secrets between OpenBao and the
  cluster in both directions
  ([section](../reference/infrastructure/infrastructure-manifests.md#external-secrets-operator)).
- **prometheus-operator CRDs** so operators can ship `ServiceMonitor`
  resources without a full monitoring stack
  ([section](../reference/infrastructure/infrastructure-manifests.md#prometheus-operator-crds)).

## GitOps

The **flux-operator** manages FluxCD through a `FluxInstance` CR that syncs
`deploy/flux-system/` from Git. HelmRelease ordering is expressed as
`dependsOn` edges with cert-manager at the base. See
[FluxInstance](../reference/infrastructure/infrastructure-manifests.md#fluxinstance)
and the
[dependency order](../reference/infrastructure/infrastructure-manifests.md#dependency-order).

## Observability and test infrastructure

Every operator exports Prometheus metrics
([Keystone Operator Metrics](../reference/keystone-operator-metrics.md) is the
reference set). The remaining pieces ship only with the kind overlay
(`deploy/kind/`) for development and CI:

- **kube-prometheus-stack** and **metrics-server** for local metrics, and
  **Chaos Mesh** driving the
  [chaos e2e suites](../reference/testing/chaos-e2e-tests.md) — all opt-in
  addons documented in
  [Infrastructure Manifests — Kind Overlay Demo Addons](../reference/infrastructure/infrastructure-manifests.md#kind-overlay-demo-addons).
- **dizzy**, the load/chaos stack around victoria-metrics, used by
  [dizzy chaos testing](../reference/testing/dizzy-chaos-testing.md).
- **Envoy Gateway** as the demo Gateway API implementation
  (`deploy/kind/base/`); production overlays bring their own Gateway
  controller.
- The **Tempest image** (`images/tempest/`) executed by the
  [Tempest test infrastructure](../reference/testing/tempest-test-infrastructure.md).

## Planned components

Components from the original
[C5C3 architecture document](https://c5c3.github.io/C5C3/) without an
implementation in this repository are sketched under [Future](../future/):
the [Hypervisor Cluster](../future/hypervisor-cluster.md), the
[Storage Cluster](../future/storage-cluster.md), and a dedicated
[Management Cluster](../future/management-cluster.md). The not-yet-onboarded
OpenStack services are listed on the
[Architecture](./index.md#the-original-multi-cluster-picture) page.
