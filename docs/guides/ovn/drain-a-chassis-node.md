---
title: Drain a Chassis Node
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Drain a Chassis Node

Taking a node out of an `OVNChassis` is two label removals in a fixed order, and
the operator answers each with a per-node Job. Removing
`openstack.c5c3.io/gateway` moves the gateway bindings off the node; removing
`openstack.c5c3.io/chassis` afterwards deletes its registration from the
Southbound database and drops it out of `status.nodes`.

The order is what keeps the model consistent. An entry marked as leaving is only
ever handed to the chassis-deletion Job, so a node that loses the chassis label
first never runs an evacuation, and the gateway bindings stay pointed at a
chassis that is on its way out.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_OVN_KERNEL_MODULES=true make deploy-infra
```

Follow that tutorial through to the **Create a first network** check in its
Step 6, so the `OVNCentral` `controlplane-ovn` is Ready in the `openstack`
namespace and the `ControlPlane` `controlplane` serves the network API.
:::

1. A node labelled and registered as in
   [Label a Node as a Compute or Network Node](./label-a-chassis-node.md): the
   `OVNChassis` `controlplane-chassis` is `Ready`, the node carries both
   `openstack.c5c3.io/chassis=true` and `openstack.c5c3.io/gateway=true`, and
   `status.nodes[].gateway` reports `true` for it.
2. The same Linux host requirement that guide names: the chassis pods have to be
   running for the `apply` Job to reach the node.

## Steps

The maintenance Jobs are named after the node, hashed:
`{chassis}-<kind>-<8 hex>`, where the eight characters are the first four bytes
of the SHA-256 of the node name. A node name is a DNS subdomain of up to 253 characters and a Job name has
63, which is why the name is hashed. Compute the suffix once so the `kubectl
wait` calls below can address a Job by name:

```bash
NODE="$(kubectl get nodes -l openstack.c5c3.io/chassis=true -o jsonpath='{.items[0].metadata.name}')"
HASH="$(printf '%s' "$NODE" | sha256sum | cut -c1-8)"   # shasum -a 256 on macOS
echo "$NODE -> $HASH"
```

### 1. Take the gateway role away

```bash
kubectl label node "$NODE" openstack.c5c3.io/gateway-
```

Two Jobs run, in this order:

```bash
kubectl wait --for=condition=complete -n openstack \
  "job/controlplane-chassis-apply-${HASH}" --timeout=5m
kubectl wait --for=condition=complete -n openstack \
  "job/controlplane-chassis-evacuate-${HASH}" --timeout=5m
```

The `apply` Job re-applies the node's values, which now carry `GATEWAY=false`,
so the chassis stops announcing `enable-chassis-as-gw`. Only then does the
`evacuate` Job collect the `Gateway_Chassis` and `HA_Chassis` rows that name
this chassis and drop them out of the logical router ports and HA groups holding
them, in one `ovn-nbctl` call against the Northbound database. Running it the
other way round would have the model hand the bindings straight back to a node
that still claims the role. The Job then asks the Northbound what still names
the chassis and fails when a row survives, so `gatewayEvacuated` only ever
records a drain the database confirmed.

`kubectl wait` fails outright on a Job that has not been created yet, so poll for
its existence first if you run this immediately after the label change: the
operator renders the Job on the pass that observes the change.

The drain has landed when the evacuation's finding reaches the status:

```bash
kubectl get ovnchassis controlplane-chassis -n openstack \
  -o jsonpath='{.status.nodes[0].gatewayEvacuated} {.status.nodes[0].gateway}'
```

Expect `true` followed by an empty string. `status.nodes[].gateway` reports what
the node last applied, and it is held at `true` until the evacuation succeeds,
so the pair flipping together is what says the drain finished rather than that
the label vanished. Re-adding the label resets `gatewayEvacuated`, so a
re-promoted node is not treated as still drained.

### 2. Take the chassis role away

```bash
kubectl label node "$NODE" openstack.c5c3.io/chassis-
```

The node stops matching `spec.nodeSelector`, and its entry survives, re-rendered:

```bash
kubectl get configmap controlplane-chassis-nodes -n openstack \
  -o jsonpath="{.data['$NODE']}"
```

The entry now ends in `LEAVING=true`. The Southbound registration outlives the
selection, so the `SYSTEM_ID` that has to be deregistered has to outlive it too.
That is what the third Job addresses:

```bash
kubectl wait --for=condition=complete -n openstack \
  "job/controlplane-chassis-chassis-del-${HASH}" --timeout=5m
```

Once it succeeds the operator forgets the node in both places at once, the
ConfigMap key and the status entry:

```bash
kubectl get ovnchassis controlplane-chassis -n openstack \
  -o jsonpath='{.status.nodes} {.status.desiredNumberScheduled}'
kubectl get ovnchassis controlplane-chassis -n openstack \
  -o jsonpath='{.status.conditions[?(@.type=="MaintenanceReady")].reason}'
```

`status.nodes` comes back empty, the counter reads `0`, and `MaintenanceReady`
settles on `MaintenanceIdle`. A Job still in flight reports `MaintenanceRunning`,
which is also `True`: a rolling drain is not an outage, so the aggregate `Ready`
is not held down for it.

### What keeps forwarding meanwhile

Dropping the chassis label takes the node out of both DaemonSets, so its pods
are terminated while the workloads on it are still sending packets. The preStop
hooks are written for that moment. `ovsdb-server` and `ovs-vswitchd` run
`ovs-appctl -t /run/openvswitch/ovsdb-server.ctl exit` and
`ovs-appctl -t /run/openvswitch/ovs-vswitchd.ctl exit`, neither with
`--cleanup`: that flag tears the kernel datapath down, and the node would stop
forwarding until a replacement pod started. `ovn-controller` runs
`ovn-appctl -t /run/ovn/ovn-controller.ctl exit --restart`, which leaves the
datapath flows in place and keeps the Southbound `Chassis` row for a successor
pod to adopt, with no re-registration. The traffic on the node therefore keeps
flowing on the kernel datapath the departed pods left behind, until the
`chassis-del` Job removes the row.

### When a maintenance Job fails

A Job that exhausts its budget (each runs with `backoffLimit: 0`, a 300-second
active deadline) is reported on the CR and raised as an event:

```bash
kubectl get events -n openstack --field-selector reason=ChassisMaintenanceJobFailed
kubectl logs -n openstack "job/controlplane-chassis-evacuate-${HASH}"
```

`MaintenanceReady` goes `False` with reason `MaintenanceJobFailed` and a message
naming the kind, the Job and the node. No error is returned to the workqueue: a
rerun under an unchanged key produces the same failure, so the operator stops
and waits for you. Fix the cause the pod logs name, usually an unreachable
database address or a Northbound row the evacuation could not remove, then
delete the failed Job:

```bash
kubectl delete job -n openstack "controlplane-chassis-evacuate-${HASH}"
```

The controller watches the Jobs it owns, so the delete wakes it, the maintenance
step finds no Job under that name, and it creates a fresh one.

## Deleting the CR is not a drain

`kubectl delete ovnchassis controlplane-chassis` removes the configuration of a
node, not the node itself. The DaemonSet pods go, and with them `ovn-controller`,
while the Southbound `Chassis` rows they registered stay behind, the way
deleting a Deployment leaves the rows its pods wrote. A stale row keeps claiming
the ports of workloads that have moved elsewhere. Drop the nodes out of
`spec.nodeSelector` and let the `chassis-del` Jobs run, as above, before
deleting the CR.

## See also

- [OVNChassis CRD](../../reference/ovn/ovn-chassis-crd.md): `status.nodes`, the
  Job naming convention, and the per-node `external_ids`.
- [OVN Controller Events](../../reference/ovn/ovn-events.md): the events both
  controllers emit and the field selectors to watch them with.
- [OVN Reconciler Architecture](../../reference/ovn/ovn-reconciler.md): the
  three triggers the maintenance step compares, and its condition contract.

## Tested by

The flow above mirrors the following end-to-end suite, which brings a gateway
chassis up on the one kind node, removes each label in turn, and waits on the
Jobs by their computed names. It seeds a logical router port bound to the
chassis before the drain and reads both databases back afterwards, because
`gatewayEvacuated` and the emptied `status.nodes` follow the Jobs' exit codes
and both Jobs exit 0 on a database that holds nothing for the chassis:

```bash
chainsaw test --test-dir tests/e2e/ovn/chassis-drain
```

::: details The OVNChassis the suite applies
The suite shares the `openstack` namespace with every other OVN suite, so its
CRs are isolation-named (`ovn-drain` against the central `ovn-drain-central`)
where the walkthrough above uses the `controlplane-chassis` and
`controlplane-ovn` names the devstack produces. Both selectors are the ones this
guide removes labels for.

<<< @/../tests/e2e/ovn/chassis-drain/01-ovnchassis-cr.yaml#ovnchassis-cr
:::
