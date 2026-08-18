---
title: Hypervisor Cluster
quadrant: infrastructure
---

# Hypervisor Cluster

> **Status: sketch — not implemented.** Carried over in raw form from the
> original
> [C5C3 architecture document](https://c5c3.github.io/C5C3/03-components/02-hypervisor).
> Nothing on this page exists in this repository yet.

The original document plans a dedicated bare-metal Kubernetes cluster for
compute virtualization: IronCore provisions the servers and installs
GardenLinux, Gardener manages the resulting cluster, and the control plane
reaches it through the Nova and Neutron APIs and the OVN southbound database.

## Sketched components

- **Hypervisor Operator** — watches Kubernetes Nodes and manages `Hypervisor`
  CRs (API group `hypervisor.c5c3.io`), with controllers for onboarding,
  maintenance mode, eviction and evacuation, decommissioning, and OpenStack
  aggregate and trait synchronization. Companion CRDs: `Eviction`,
  `Migration`.
- **Node agents** as DaemonSets on every hypervisor node:
  - *Hypervisor Node Agent*: LibVirt introspection that updates `Hypervisor`
    status (versions, capabilities, running instances) and includes the HA
    agent subscribing to LibVirt domain events.
  - *OVS Agent*: Open vSwitch introspection into an `OVSNode` CR
    (API group `ovs.c5c3.io`) covering bridges, bonds, flow statistics, and
    health conditions.
  - *ovn-controller*: programs OVS flows from the OVN southbound database.
  - *Nova Compute Agent*: VM lifecycle and resource reporting to Nova.
- **Virtualization layer**: LibVirt with a QEMU/KVM or Cloud Hypervisor
  backend, either provided by the GardenLinux image or deployed as a
  containerized DaemonSet.

## Open questions

Whether the implemented management/target-cluster mechanics
([Target Clusters](../reference/target-clusters.md)) extend to a hypervisor
cluster is unexamined, and the Nova and Neutron operators this cluster
presumes are not onboarded (see the service list on the
[Architecture](../architecture/index.md#the-original-multi-cluster-picture)
page).

## Source

- [Hypervisor components](https://c5c3.github.io/C5C3/03-components/02-hypervisor)
- [Hypervisor lifecycle](https://c5c3.github.io/C5C3/04-architecture/03-hypervisor-lifecycle)
- [High availability](https://c5c3.github.io/C5C3/04-architecture/04-high-availability)
