---
title: Management Cluster
quadrant: infrastructure
---

# Management Cluster

> **Status: sketch — not implemented.** Carried over in raw form from the
> original
> [C5C3 architecture document](https://c5c3.github.io/C5C3/03-components/03-management).
> Today every component named below that exists runs on the single
> [management cluster](../architecture/index.md#implemented-topology) next to
> the control plane; the dedicated cluster and the observability UI do not
> exist in this repository.

The original document separates a management cluster from the control-plane
cluster: GitOps, secrets, and cross-cluster observability live on their own
cluster, which then deploys workloads into the control-plane, hypervisor, and
storage clusters via kubeconfig Secrets and syncs credentials everywhere
through ESO.

In the
[multi-cluster target picture](../architecture/index.md#the-multi-cluster-target-picture)
this cluster is the one labelled CobaltCore Operation and Monitoring.

![Five Kubernetes clusters on Garden Linux nodes: CobaltCore Operation and Monitoring, OpenStack Control Plane, Ceph Storage, OpenStack Compute, and OpenStack Network. Gardener manages the clusters, IronCore provisions the bare-metal nodes and manages them out of band. API users call the OpenStack API on the control plane; end users reach the VMs through the network cluster.](../diagrams/cobaltcore-architecture.svg)

## Sketched components

- **Flux Operator + FluxCD** as the GitOps hub for all clusters, deploying
  remotely via kubeconfig Secrets. Implemented today as the in-cluster
  [FluxInstance](../reference/infrastructure/infrastructure-manifests.md#fluxinstance).
- **OpenBao** as the central secret store (HA, 3x Raft) for every cluster.
  Implemented today as the single-cluster
  [proving instance](../reference/infrastructure/infrastructure-manifests.md#openbao-proving-instance).
- **External Secrets Operator** running in all clusters, each with a
  `ClusterSecretStore` pointing back at the management-cluster OpenBao.
- **Greenhouse** for centralized monitoring and alerting across clusters.
- **Aurora Dashboard** as the unified management UI over servers, networks,
  and volumes.

## Open questions

Splitting the implemented single-cluster stack would need a story for where
the operators run, which cluster owns the `ControlPlane` CR, and how the
existing [target-cluster placement](../reference/target-clusters.md) relates
to a remote control-plane cluster.

## Source

- [Management components](https://c5c3.github.io/C5C3/03-components/03-management)
- [Architecture overview](https://c5c3.github.io/C5C3/02-architecture-overview)
