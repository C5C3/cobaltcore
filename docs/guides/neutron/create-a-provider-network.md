---
title: Create a Provider Network
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Create a Provider Network

A provider network puts instances on a segment that already exists outside
OpenStack. Two halves have to meet for that: every chassis node needs a local
Open vSwitch bridge for the physical network, and Neutron needs a network whose
type is `flat` and whose physical network names that mapping.

This guide adds the mapping to the `controlplane-chassis` chassis of the
ControlPlane devstack, creates the network and a port through the Neutron API,
and claims the port on the node the way a hypervisor would.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_OVN_KERNEL_MODULES=true make deploy-infra
```

Follow that tutorial through to the **Create a first network** check in its
Step 6, so the `ControlPlane` `controlplane` serves the network API and the
`OVNCentral` `controlplane-ovn` is Ready in the `openstack` namespace.
:::

1. A chassis on the node, as
   [Label a Node as a Compute or Network Node](../ovn/label-a-chassis-node.md)
   leaves it: the node carries `openstack.c5c3.io/chassis=true` and the
   `OVNChassis` `controlplane-chassis` reports `Ready` with one entry in
   `status.nodes`.
2. The `OS_*` variables the quick start exports in its Step 6, still in the
   shell you run the `openstack` commands from. Every call below carries
   `--insecure`, because the gateway listener presents a self-signed
   certificate.

## Steps

### 1. Map the physical network onto a local bridge

The label guide's manifest carries no `spec.bridgeMappings`, so a merge patch
writes the whole list:

```bash
kubectl patch ovnchassis controlplane-chassis -n openstack --type merge \
  -p '{"spec":{"bridgeMappings":[{"physicalNetwork":"physnet1","bridge":"br-ex"}]}}'
```

Nothing projects an `OVNChassis`. The ControlPlane references an `OVNCentral`
and stops there, so this CR is yours and the mapping stands until you change it.

The operator renders the list into the node's own Open vSwitch database and
creates each named bridge with `ovs-vsctl --may-exist add-br`. Read both back:

```bash
kubectl exec ds/controlplane-chassis-ovs -n openstack -c ovsdb-server -- \
  ovs-vsctl get open . external_ids:ovn-bridge-mappings
kubectl exec ds/controlplane-chassis-ovs -n openstack -c ovsdb-server -- \
  ovs-vsctl br-exists br-ex
```

The first prints `"physnet1:br-ex"`, quotes included. The second prints nothing
and exits 0. Check the bridge as well as the mapping: `ovn-controller` attaches
patch ports to a bridge but never creates one, so a mapping pointing at a
missing bridge drops every packet on that physical network while the CR and the
mapping still read correctly.

On a kind node `br-ex` has no uplink. Nothing leaves the host, which is why the
checks below stop at the port binding rather than at a ping.

### 2. Create the network and its subnet

```bash
openstack --insecure network create \
  --provider-network-type flat --provider-physical-network physnet1 provider
openstack --insecure subnet create --network provider \
  --subnet-range 198.51.100.0/24 --no-dhcp provider-subnet
```

`--provider-physical-network` takes the same name as the mapping's
`physicalNetwork`; a value with no mapping behind it yields a network no chassis
can reach. The range is the RFC 5737 documentation block, so it collides with
nothing on your host. `--no-dhcp` leaves address assignment on the physical
segment, where a provider network usually already has a DHCP server; Neutron
still allocates each port a fixed address out of the subnet.

### 3. Create a port and bind it to the node

```bash
NODE="$(kubectl get nodes -l openstack.c5c3.io/chassis=true -o jsonpath='{.items[0].metadata.name}')"
openstack --insecure port create --network provider --host "$NODE" smoke0
openstack --insecure port show smoke0 -c binding_vif_type -f value
```

Expect `ovs`. `--host` sets `binding:host_id`, and the ML2/OVN mechanism driver
binds the port to the chassis registered under that hostname. A port created
without a host stays unbound, and `binding_vif_type` reports `unbound`.

### 4. Claim the port on the integration bridge

An instance would be plugged in by its hypervisor. With no instance to start,
put an internal interface on `br-int` and tag it with the port's id, which is
the part of the plug that OVN reacts to:

```bash
PORT_ID=$(openstack --insecure port show smoke0 -f value -c id)
kubectl exec ds/controlplane-chassis-ovs -n openstack -c ovsdb-server -- \
  ovs-vsctl --may-exist add-port br-int smoke0 \
  -- set Interface smoke0 type=internal external_ids:iface-id=$PORT_ID
```

`ovn-controller` looks a logical port up by the `iface-id` it finds on an
interface of `br-int`. Once it matches, it writes its own chassis into the
Southbound `Port_Binding` row, and Neutron reports the port `ACTIVE` as soon as
that row names a chassis:

```bash
for _ in $(seq 30); do
  [ "$(openstack --insecure port show smoke0 -c status -f value)" = ACTIVE ] && break
  sleep 2
done
openstack --insecure port show smoke0 -c status -f value
```

The interface is `type=internal` because nothing has to carry traffic through
it. The binding is the assertion.

### 5. Clean up

Remove the interface before the port, so `ovn-controller` never holds a claim on
a logical port that is gone:

```bash
kubectl exec ds/controlplane-chassis-ovs -n openstack -c ovsdb-server -- \
  ovs-vsctl --if-exists del-port br-int smoke0
openstack --insecure port delete smoke0
openstack --insecure subnet delete provider-subnet
openstack --insecure network delete provider
```

The bridge mapping outlives them. Drop it with
`kubectl patch ovnchassis controlplane-chassis -n openstack --type json -p '[{"op":"remove","path":"/spec/bridgeMappings"}]'`
to leave the node as the label guide left it.

## Toward a BGP fabric

Flat is the only provider type on offer here. The rendered `ml2_conf.ini`
carries `type_drivers = geneve,flat` with `flat_networks = *`, so a VLAN
provider network has no type driver to resolve against, and the native OVN
dynamic-routing path the fabric work builds on does not take VLAN provider
networks either.

Gateway nodes are picked out by the second label of the chassis contract,
`openstack.c5c3.io/gateway=true`, which the label guide sets and which makes a
chassis eligible to host a distributed router's gateway port. That selection is
what the route announcements will be anchored on. The page describing the fabric
contract arrives with that work.

## See also

- [OVNChassis CRD](../../reference/ovn/ovn-chassis-crd.md): `spec.bridgeMappings`
  and the [node contract](../../reference/ovn/ovn-chassis-crd.md#node-contract)
  the mapping is rendered into.
- [Label a Node as a Compute or Network Node](../ovn/label-a-chassis-node.md):
  the two labels and the registration this guide builds on.
- [Neutron CRD](../../reference/neutron/neutron-crd.md): the network service the
  `openstack` commands above address.

## Tested by

The flow above mirrors the following end-to-end suite, which labels the node,
applies a chassis carrying the mapping, writes the provider switch into the
Northbound database, announces a port on `br-int`, and reads the Southbound
binding back:

```bash
chainsaw test --test-dir tests/e2e/neutron/provider-network-smoke
```

The suite proves the bridge mapping on the node and the Southbound binding of
the port; it runs none of the `openstack` commands and asserts no `ACTIVE`
status, because its leg carries no Keystone and writes the logical model straight
into the Northbound database.

::: details The OVNChassis the suite applies
The suite shares the `openstack` namespace with every other neutron suite, so
its CRs are isolation-named (`neutron-provider-chassis` against the central
`neutron-provider-ovn`) where the walkthrough above uses the
`controlplane-chassis` and `controlplane-ovn` names the devstack produces. The
mapping is the same one Step 1 patches in.

<<< @/../tests/e2e/neutron/provider-network-smoke/02-ovnchassis-cr.yaml#ovnchassis-cr
:::
