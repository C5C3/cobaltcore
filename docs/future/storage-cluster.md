---
title: Storage Cluster
quadrant: infrastructure
---

# Storage Cluster

> **Status: sketch — not implemented.** Carried over in raw form from the
> original
> [C5C3 architecture document](https://c5c3.github.io/C5C3/03-components/04-storage).
> Nothing on this page exists in this repository yet; the implemented storage
> surface is the [Garage S3 object store](../reference/infrastructure/infrastructure-manifests.md#garage-object-store)
> backing Glance multi-store.

The original document plans a dedicated bare-metal Kubernetes cluster for
persistent storage, provisioned like the hypervisor cluster via IronCore and
Gardener. It serves RBD block devices to VM disks and volumes, and hands Ceph
client keys to the control-plane services through OpenBao and ESO.

## Sketched components

- **Rook Operator** managing the Ceph cluster: MON quorum, OSD provisioning,
  pools, and storage classes.
- **Ceph services**: MON, OSD, RBD for block storage, plus optional RadosGW
  (S3/Swift) and CephFS.
- **External Arbiter Operator** deploying Ceph Monitors into remote clusters
  for stretched-cluster quorum, with `RemoteCluster` and `RemoteArbiter` CRs
  (API group `ceph.c5c3.io`). An optional arbiter cluster at a third site
  hosts a MON without OSDs as the tiebreaker.
- **Prysm**, a storage observability platform running as a RadosGW sidecar:
  SMART disk health, S3 operation logs, quota tracking, and Prometheus
  metrics.

## Open questions

The consumers of this cluster (the Cinder operator, Nova ephemeral storage,
Glance RBD backends) are not onboarded, and how Ceph credentials would flow
through the implemented
[OpenBao/ESO round-trip](../reference/infrastructure/openbao-bootstrap.md) is
unexamined.

## Source

- [Storage components](https://c5c3.github.io/C5C3/03-components/04-storage)
- [Storage architecture](https://c5c3.github.io/C5C3/04-architecture/06-storage)
