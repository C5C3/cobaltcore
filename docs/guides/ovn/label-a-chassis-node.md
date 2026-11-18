---
title: Label a Node as a Compute or Network Node
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Label a Node as a Compute or Network Node

An `OVNChassis` puts the Open vSwitch and `ovn-controller` DaemonSets on the
nodes its selector matches and registers each of them as a chassis with the
`OVNCentral` it names. Two node labels decide which nodes those are:
`openstack.c5c3.io/chassis` makes a node run the datapath, and
`openstack.c5c3.io/gateway` promotes one of those nodes to a network node that
announces `enable-chassis-as-gw` and can host a distributed router's gateway
port.

This guide labels the node of the ControlPlane devstack, applies an `OVNChassis`
against the `controlplane-ovn` central that devstack creates, and reads the
registration back out of the node's own Open vSwitch database.

The figure shows what a labelled node runs once a metadata agent and a compute
pool select it as well. This guide builds the two pods at the top; `{chassis}`
is `controlplane-chassis` and `{ns}` is `openstack` here.

![One hypervisor node with the four pods three resources put on it. An OVNChassis creates two DaemonSets: the pod {chassis}-ovs with the init container host-prepare and the containers ovsdb-server and ovs-vswitchd, and the pod {chassis}-ovn-controller with the init container apply-node and the container ovn-controller. A NeutronMetadataAgent creates the pod {agent}-metadata-agent, a NovaCompute the pod {pool}-nova-compute. All four run in the network namespace of the node, beside a libvirtd that no operator runs. Three gates order the start. The operator creates the ovn-controller DaemonSet only once every OVS pod is Ready. The init container wait-for-chassis of nova-compute waits until apply-node has written a system-id into the local Open vSwitch database. The init container wait-for-chassis of the metadata agent waits until ovn-controller has registered the chassis in the Southbound database. The pods share host paths: /run/openvswitch is mounted by all four, /run/ovn by the two chassis pods, /run/netns by the metadata agent, /run/libvirt and /var/lib/nova by nova-compute and libvirtd. A gateway node has no further pod: it also matches spec.gateway.nodeSelector, and apply-node sets ovn-cms-options=enable-chassis-as-gw.](../../diagrams/compute-node-anatomy.svg)

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_OVN_KERNEL_MODULES=true make deploy-infra
```

Follow that tutorial through to the **Create a first network** check in its
Step 7, so the `OVNCentral` `controlplane-ovn` is Ready in the `openstack`
namespace and the `ControlPlane` `controlplane` serves the network API.
:::

The kind node's Linux kernel must provide the `openvswitch` and `geneve`
modules. `WITH_OVN_KERNEL_MODULES=true` asks `hack/deploy-infra.sh` to attempt
loading them on a Linux host. The host-side load needs root or passwordless
sudo; if the script cannot load a module, it logs a warning and continues. On a
non-Linux host, the script skips the host-side load because the kind node runs
in a Linux VM. In either case, the OVS DaemonSet's `host-prepare` init
container runs `modprobe openvswitch && modprobe geneve`. If either module is
unavailable to the node, the chassis pod stays in `Init:CrashLoopBackOff`.
The ControlPlane bring-up also deploys the `ovn-operator` in namespace
`ovn-system`.

## Steps

### 1. Label the node

The devstack cluster has one node, so read its name once and keep it:

```bash
NODE="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
test -n "$NODE"
kubectl label node "$NODE" openstack.c5c3.io/chassis=true --overwrite
```

On a larger cluster, repeat the label for every node that should carry the
datapath. The CRD fixes no label keys; `openstack.c5c3.io/chassis` and
`openstack.c5c3.io/gateway` are the keys the e2e suites and these guides use.
The operator never writes a Node: its RBAC on `nodes` is `get`, `list` and
`watch`, so the labels stay yours.

### 2. Apply the OVNChassis

```yaml
# controlplane-chassis.yaml
apiVersion: ovn.openstack.c5c3.io/v1alpha1
kind: OVNChassis
metadata:
  name: controlplane-chassis
  namespace: openstack
spec:
  centralRef:
    name: controlplane-ovn
  nodeSelector:
    openstack.c5c3.io/chassis: "true"
  gateway:
    nodeSelector:
      openstack.c5c3.io/gateway: "true"
```

```bash
kubectl apply -f controlplane-chassis.yaml
kubectl wait ovnchassis/controlplane-chassis -n openstack \
  --for=condition=Ready --timeout=10m
```

Nothing projects an `OVNChassis`: the ControlPlane references an `OVNCentral`
and stops there, so this CR is yours to edit and no reconcile takes your
changes back. `spec.centralRef` and `spec.nodeSelector` name the central and the
nodes; `spec.gateway.nodeSelector` narrows that same set, and in this devstack
it matches no node until Step 3 puts the second label on one. The manifest
carries no `spec.bridgeMappings`, because a kind node has no provider network
to map.

`Ready` aggregates six sub-conditions. Read them when the wait times out:

```bash
kubectl get ovnchassis controlplane-chassis -n openstack \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
```

### 3. Promote the node to a network node

A node with the chassis label alone is a compute node: it forwards the traffic
of the workloads on it and announces no gateway role. Add the second label to
make it a network node:

```bash
kubectl label node "$NODE" openstack.c5c3.io/gateway=true --overwrite
```

On a multi-node cluster, set `NODE` to the chassis node you want to promote
before running the label command.

Three things follow. The node's entry in the `controlplane-chassis-nodes`
ConfigMap is re-rendered with `GATEWAY=true`, so its hash no longer matches
`status.nodes[].configHash` and the operator runs a
`controlplane-chassis-apply-<8 hex>` Job pinned to that node. The Job re-runs
the same `apply-node.sh` the pod's init container runs, which sets
`ovn-cms-options=enable-chassis-as-gw` in the local `Open_vSwitch` table.
`status.nodes[].gateway` follows the selector on promotion and can turn `true`
before the Job completes. On demotion, it stays `true` until the evacuation Job
succeeds. `MaintenanceReady` also stays `True` while a Job runs, so neither it
nor the aggregate `Ready` condition proves that the node has applied the
gateway configuration. After the operator creates the apply Job, check its
name and wait for it to complete:

```bash
kubectl get jobs -n openstack
kubectl wait 'job/controlplane-chassis-apply-<8-hex-suffix>' -n openstack \
  --for=condition=Complete --timeout=5m
```

If the wait fails, inspect the Job's pod logs:

```bash
kubectl logs 'job/controlplane-chassis-apply-<8-hex-suffix>' -n openstack
```

The local database check in Verification confirms the applied gateway option.

Taking the label off again is an evacuation, with a Job of its own:
[Drain a Chassis Node](./drain-a-chassis-node.md) walks it.

## Verification

The per-node registration record carries the chassis identity:

```bash
kubectl get ovnchassis controlplane-chassis -n openstack \
  -o jsonpath='{.status.nodes}'
```

Expect one entry per labelled node, with `systemID` holding a UUID and
`gateway: true` on the promoted one. A compute node has no `gateway` key
because the field uses `omitempty` for false values. The `systemID` is what the
chassis-deletion Job addresses later, so a node without one leaves the Job
unable to identify the registration to remove.

Both DaemonSets run on the selected nodes:

```bash
kubectl get ds -n openstack \
  controlplane-chassis-ovs controlplane-chassis-ovn-controller
```

`DESIRED` and `READY` should equal the number of labelled nodes on both. The
counters on the CR (`status.desiredNumberScheduled`, `status.numberReady`) are
mirrored from the `ovn-controller` DaemonSet alone: that daemon is what makes a
node a chassis, and a node running Open vSwitch without it carries no logical
flows.

Finally, read the values the node applied to its own database:

```bash
kubectl exec ds/controlplane-chassis-ovs -n openstack -c ovsdb-server -- \
  ovs-vsctl get open . external_ids
```

The devstack has one labelled node, so the command reads its database. On a
multi-node cluster, list the OVS DaemonSet pods:

```bash
kubectl get pods -n openstack \
  -l app.kubernetes.io/instance=controlplane-chassis,app.kubernetes.io/component=ovs
```

Replace `ds/controlplane-chassis-ovs` with each pod name in the `kubectl exec`
command and run it once per pod to check every node.

The output should show `system-id` matching `status.nodes[].systemID`,
`hostname` with the node name, `ovn-encap-type=geneve` with the node address in
`ovn-encap-ip`, and `ovn-remote` pointing at the Southbound address published
by `controlplane-ovn`. A gateway node also has
`ovn-cms-options=enable-chassis-as-gw`; a compute node has no
`ovn-cms-options` key.

## See also

- [OVNChassis CRD](../../reference/ovn/ovn-chassis-crd.md): the node contract,
  the full `external_ids` table, and the three maintenance Jobs.
- [OVN Reconciler Architecture](../../reference/ovn/ovn-reconciler.md): which
  sub-reconciler owns which condition, and what each Job is triggered by.
- [Target Clusters](../../reference/target-clusters.md): running the chassis
  layer on a cluster other than the management one.

## Tested by

The flow above mirrors the following end-to-end suite, which labels the node,
applies a chassis, and probes the Southbound database for the registration and
its Geneve tunnel endpoint:

```bash
chainsaw test --test-dir tests/e2e/ovn/chassis-single-node
```

::: details The OVNChassis the suite applies
The suite shares the `openstack` namespace with every other OVN suite, so its
CRs are isolation-named (`ovn-chassis` against the central `ovn-chassis-central`)
where the walkthrough above uses the `controlplane-chassis` and
`controlplane-ovn` names the devstack produces. It also declares no
`spec.gateway`: a kind node has no uplink, and a gateway chassis without one
would announce connectivity it does not have.

<<< ../../../tests/e2e/ovn/chassis-single-node/02-ovnchassis-cr.yaml#ovnchassis-cr
:::
