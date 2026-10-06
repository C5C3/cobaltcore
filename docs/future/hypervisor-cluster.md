---
title: Hypervisor Cluster
quadrant: infrastructure
---

# Hypervisor Cluster

> **Status: sketch — the dedicated cluster is not implemented.** Carried over
> in raw form from the original
> [C5C3 architecture document](https://c5c3.github.io/C5C3/03-components/02-hypervisor).
> What exists is the node layer: this repository has `NovaCompute`,
> `OVNChassis` and `NeutronMetadataAgent`, and the
> [Quick Start (metal-stack)](../quick-start-metal-stack.md) runs them on two
> workers of the control-plane cluster, beside a containerized libvirt
> DaemonSet and the upstream openstack-hypervisor-operator and kvm-node-agent.
> Their `Hypervisor` CRs belong to the API group `kvm.cloud.sap`, not to the
> sketched `hypervisor.c5c3.io`.

The original document plans a dedicated bare-metal Kubernetes cluster for
compute virtualization: IronCore provisions the servers and installs
GardenLinux, Gardener manages the resulting cluster, and the control plane
reaches it through the Nova and Neutron APIs and the OVN southbound database.

In the
[multi-cluster target picture](../architecture/index.md#the-multi-cluster-target-picture)
this cluster is the one labelled OpenStack Compute.

![Five Kubernetes clusters on Garden Linux nodes: CobaltCore Operation and Monitoring, OpenStack Control Plane, Ceph Storage, OpenStack Compute, and OpenStack Network. Gardener manages the clusters, IronCore provisions the bare-metal nodes and manages them out of band. API users call the OpenStack API on the control plane; end users reach the VMs through the network cluster.](../diagrams/cobaltcore-architecture.svg)

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

A hypervisor cluster is a registered target cluster
([Target Clusters](../reference/target-clusters.md)). `nova-compute` runs there
as a `NovaCompute` pool, beside openstack-hypervisor-operator and
kvm-node-agent, which CobaltCore adopts from cobaltcore-dev as they are. The
[Nova](../reference/nova/index.md) and [Neutron](../reference/neutron/index.md)
control planes this cluster presumes are onboarded, and the
[OVN operator](../reference/ovn/index.md) projects `ovs` and `ovn-controller`
onto labelled nodes. What such a cluster reads from the control plane, and what
it has to provide in return, is written down in
[Connect a Compute Cluster](../guides/nova/connect-a-compute-cluster.md). The
dedicated bare-metal cluster of the original document, provisioned by IronCore
and managed by Gardener, stays a sketch.

The figure shows the node layer as the Quick Start (metal-stack) builds it
today: on the workers of the control-plane cluster, without a dedicated compute
cluster.

![The metal-stack lab after both parts of the quick start. One Gardener shoot holds everything. Cluster-wide, built by Part 1: the Envoy proxy of the Gateway openstack-gw, the ControlPlane controlplane with its eight OpenStack services, the OVNCentral controlplane-ovn, the backing services, an NFS server for Cinder, and the hypervisor operator. Built by Part 2: the resources OVNChassis lab-chassis, NeutronMetadataAgent lab-metadata-agent and NovaCompute lab, which put one pod of each of their DaemonSets on every labelled worker. Every worker is a Kubernetes node, a KVM hypervisor and an OVN chassis at once: it runs Open vSwitch, ovn-controller, the metadata agent, nova-compute, libvirt with QEMU, kvm-node-agent and the reservation of the migration ports, and it hosts servers. The figure draws worker 1 and worker N and a box for more. Between any two workers run Geneve tunnels on UDP 6081, libvirt with TLS on TCP 16514, and QEMU migrations with TLS on TCP 49152 to 49215. Each worker reaches the bus, the Southbound database, the metadata API and the NFS server inside the cluster. The only way in from the workstation is a port-forward of local port 8443 to the Envoy proxy.](../diagrams/compute-metal-stack-lab.svg)

## Source

- [Hypervisor components](https://c5c3.github.io/C5C3/03-components/02-hypervisor)
- [Hypervisor lifecycle](https://c5c3.github.io/C5C3/04-architecture/03-hypervisor-lifecycle)
- [High availability](https://c5c3.github.io/C5C3/04-architecture/04-high-availability)
