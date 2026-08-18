---
title: Architecture
---

# Architecture

CobaltCore (C5C3) is a Kubernetes-native OpenStack distribution for operating
Hosted Control Planes. Its design originates in the
[C5C3 architecture document](https://c5c3.github.io/C5C3/), an early-concept
sketch of the full multi-cluster system. This page describes the architecture
as this repository implements it, and the implementation is authoritative
where the two differ. Building blocks from the original document that have no
implementation yet are collected as sketches in the [Future](../future/)
section.

The component-by-component catalog, with CRDs and API groups, is on
[Core Components](./core-components.md).

## Implemented topology

Everything runs on one management cluster. FluxCD applies the declarative
infrastructure stack, the operators come up in dependency order, and a single
`ControlPlane` resource then drives a complete OpenStack control plane.
Service workloads either stay on the management cluster or land on registered
target clusters.

```text
┌───────────────────────────────────────────────────────────────────────────┐
│                            MANAGEMENT CLUSTER                             │
│                                                                           │
│  GitOps                          Secrets & PKI                            │
│  ┌─────────────────────────┐     ┌─────────────────────────────────────┐  │
│  │ flux-operator           │     │ cert-manager                        │  │
│  │ └─ FluxInstance         │     │ OpenBao  (openbao-operator)         │  │
│  │    └─ deploy/           │     │ External Secrets Operator (ESO)     │  │
│  │       flux-system/      │     └─────────────────────────────────────┘  │
│  └─────────────────────────┘                                              │
│                                                                           │
│  Infrastructure operators        Infrastructure CRs                       │
│  ┌─────────────────────────┐     ┌─────────────────────────────────────┐  │
│  │ mariadb-operator        │────▶│ MariaDB (Galera)                    │  │
│  │ memcached-operator      │────▶│ Memcached                           │  │
│  │ garage-operator         │────▶│ Garage (S3 object store)            │  │
│  └─────────────────────────┘     └─────────────────────────────────────┘  │
│                                                                           │
│  Orchestration                   Service operators                        │
│  ┌─────────────────────────┐     ┌─────────────────────────────────────┐  │
│  │ c5c3-operator           │     │ keystone-operator                   │  │
│  │ └─ ControlPlane CR      │────▶│ glance-operator                     │  │
│  │    creates infra CRs,   │     │ placement-operator                  │  │
│  │    service CRs, and     │     │ horizon-operator                    │  │
│  │    K-ORC resources      │     │ barbican-operator                   │  │
│  └───────────┬─────────────┘     └───────────────┬─────────────────────┘  │
│              ▼                                   │ project Deployments,   │
│  ┌─────────────────────────┐                     │ Jobs, config, Secrets  │
│  │ K-ORC                   │                     ▼                        │
│  │ (declarative OpenStack  │     ┌─────────────────────────────────────┐  │
│  │  resource management)   │     │ OpenStack services                  │  │
│  └─────────────────────────┘     │ Keystone, Glance, Placement,        │  │
│                                  │ Horizon, Barbican                   │  │
│                                  │ (exposed via Gateway API)           │  │
│                                  └─────────────────────────────────────┘  │
└──────────────────────────────────────┬────────────────────────────────────┘
                                       │ kubeconfig Secrets
                                       ▼
                     ┌───────────────────────────────────────┐
                     │ TARGET CLUSTERS (optional)            │
                     │ receive projected service workloads   │
                     └───────────────────────────────────────┘
```

## Layering

The stack is built in three declarative layers.

**Infrastructure manifests** (`deploy/flux-system/`). A `FluxInstance` syncs
the repository, and HelmReleases install cert-manager, the External Secrets
Operator, OpenBao, and the infrastructure and service operators along an
explicit `dependsOn` graph; K-ORC is applied by a separate Flux
`Kustomization`. The full stack, its namespaces, and the dependency order are
documented in
[Infrastructure Manifests](../reference/infrastructure/infrastructure-manifests.md).

**Service operators** (`operators/`). One operator per OpenStack service, each
projecting the service's Deployments, Jobs, configuration, and Secrets from
its CR. The [Keystone operator](../reference/keystone/) is the reference
implementation that sets the patterns — CRD layout, sub-reconciler chain,
webhooks, finalizers, instrumentation — and Glance, Placement, Horizon, and
Barbican are onboarded on the same scaffolding
([Adding a New Operator](../contributing/adding-a-new-operator.md)).

**Orchestration** (`operators/c5c3/`). The c5c3-operator turns one
`ControlPlane` CR into a running control plane: it creates the MariaDB and
Memcached CRs, projects the service CRs, mints the admin application
credential through K-ORC, stewards the service-catalog entries, and aggregates
readiness into the `ControlPlane` status. See
[ControlPlane Reconciler Architecture](../reference/c5c3/controlplane-reconciler.md).

## Secret flow

OpenBao is the source of truth for credentials, and ESO moves them in both
directions: `ExternalSecret` resources deliver admin and database credentials
to the services, `PushSecret` resources write operator-generated secrets back
to OpenBao. Multi-tenant deployments get a per-ControlPlane tenant store
provisioned by the operator. The bootstrap sequence and the credential chain
are documented in
[OpenBao Bootstrap](../reference/infrastructure/openbao-bootstrap.md) and
[Infrastructure Manifests](../reference/infrastructure/infrastructure-manifests.md#admin-credential-chain).

## Service exposure

Service CRs opt into external exposure through the Gateway API: when
`spec.gateway` is set, the operator renders an `HTTPRoute` for the configured
public endpoint. The kind overlay installs Envoy Gateway as the demo
implementation; production overlays do not ship a Gateway controller, and
platform owners bring their own implementation.

## Multi-cluster placement

The implemented multi-cluster model is management cluster plus target
clusters. A target cluster is registered by a kubeconfig Secret, and the five
workload CRDs carry an optional `spec.targetClusterRef` that sends every
projected child there while the CR itself stays on the management cluster. The
`ControlPlane` carries one ref per service, so a single control plane can
spread its services across clusters. See
[Target Clusters](../reference/target-clusters.md) and the
[Deploy to a Target Cluster](../guides/deploy-to-a-target-cluster.md) guide.

## The original multi-cluster picture

The original document sketches four clusters, provisioned via Gardener and
IronCore. The table maps them to the state in this repository:

| Cluster (original) | Purpose | Status here |
| --- | --- | --- |
| Management | GitOps hub, OpenBao, ESO, observability UI (Greenhouse, Aurora) | Collapsed into the single management cluster above; a dedicated cluster is a [sketch](../future/management-cluster.md) |
| Control Plane | OpenStack control-plane services, K-ORC, infrastructure | Implemented as the management cluster, with optional [target clusters](../reference/target-clusters.md) for workload placement |
| Hypervisor | Compute virtualization on bare metal (LibVirt, OVN, node agents) | [Sketch](../future/hypervisor-cluster.md) |
| Storage | Ceph via Rook, storage observability | [Sketch](../future/storage-cluster.md) |

Beyond the clusters, the original document scopes services that are not
onboarded yet: the Nova, Neutron, and Cinder operators with the ovn-operator
SDN backend and their RabbitMQ and Valkey infrastructure, the optional Cortex
scheduler and Tempest operator, and consumer self-service via Crossplane.
Tempest exists in this repository as a container image driven by the
[e2e test infrastructure](../reference/testing/tempest-test-infrastructure.md),
not as an operator. New services follow the onboarding path in
[Adding a New Operator](../contributing/adding-a-new-operator.md).
