---
title: OVN Dynamic Routing
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# OVN Dynamic Routing

This page describes how OVN 26.03.2 exchanges routes with the BGP fabric of the
[BGP Fabric Contract](bgp-fabric-contract.md) on a CobaltCore OpenStack node:
which side writes which setting, the router and port options of the Northbound
database, what ovn-controller does with them on a chassis, and how to observe
the result. The speaker on the node is frr-k8s, described on
[BGP Node Substrate](bgp-node-substrate.md), and the chassis side is the
`spec.fabric` block of the [OVNChassis CRD](../ovn/ovn-chassis-crd.md#ovnfabricspec).

## Who sets what

The ovn-operator prepares the chassis. For every node with a row in
`spec.fabric.nodes` it creates the fabric VRF with the fabric loopbacks, one OVS
peer bridge per uplink and the key `external_ids:dynamic-routing-port-mapping`
of the node's `Open_vSwitch` row, and for every such node that runs frr-k8s it
writes the node's `FRRConfiguration`.

The CMS writes every router and port option on this page. They are columns of
`Logical_Router` and `Logical_Router_Port` rows in the Northbound database, and
those rows belong to whoever builds the logical network, which in CobaltCore
OpenStack is Neutron. `OVNCentral` manages none of them, and it carries no field
for them: the `ovn-bgp` service plugin of Neutron sets them on its own routers
with fixed values (neutron 29.0.0, `neutron/services/bgp/commands.py`,
`ReconcileMainRouterCommand`), so a field on the operator's side would have no
reader.

## Router options

The options of a `Logical_Router` row, from `ovn-nb.xml` of OVN 26.03.2. Every
option but `dynamic-routing` takes effect only while `dynamic-routing` is
`true`.

| Option | Type | What it does |
| --- | --- | --- |
| `dynamic-routing` | boolean | The router takes part in dynamic routing: northd synchronizes the routes that are relevant for it into the Southbound `Advertised_Route` table, and ovn-controller learns routes for it |
| `dynamic-routing-redistribute` | comma-separated list | Which routes are advertised. `connected` advertises the networks of the router ports, `connected-as-host` advertises each used address of those networks as a host route instead and implies `connected`, `static` advertises the static routes, `nat` the external IP of every NAT rule and `lb` every load balancer VIP, both on this router and on its neighboring routers. A router port can override the list |
| `dynamic-routing-redistribute-local-only` | boolean | ovn-controller advertises an `Advertised_Route` row only on the chassis where its tracked port is bound. Default `false` |
| `dynamic-routing-vrf-id` | integer, 1 to 4294967295 | The table of the VRF ovn-controller advertises into and learns from, and the suffix of its default name `ovnvrf<id>`. Unset, the datapath id of the router is used |
| `dynamic-routing-vrf-name` | string | The name of that VRF instead of `ovnvrf<id>`; the table stays as above |
| `dynamic-routing-no-learning` | boolean | Disables route learning on the router and removes the routes it learned |
| `dynamic-routing-v4-prefix-nexthop` | IPv4 or IPv6 address | The next hop of advertised IPv4 routes, which are otherwise installed as blackholes |
| `dynamic-routing-v6-prefix-nexthop` | IPv6 address | The next hop of advertised IPv6 routes, which are otherwise installed as blackholes |

The `ovn-bgp` plugin sets `dynamic-routing=true`,
`dynamic-routing-redistribute=connected-as-host,nat` and
`dynamic-routing-redistribute-local-only=true` on its routers, with the VRF ids
42 and 10. The suite on this page uses the same two ids: 42 on the router whose
routes the node announces, 10 on the router that learns the leaf defaults. Those
are the ids `spec.fabric.advertiseVRFIDs` and `spec.fabric.learnVRFIDs` name.

## Port options

The options of a `Logical_Router_Port` row. The first four take effect only
while `dynamic-routing` is `true` on the port's router.

| Option | Type | What it does |
| --- | --- | --- |
| `dynamic-routing-maintain-vrf` | boolean | The ovn-controller of the chassis the port is bound to creates and maintains the router's VRF, `ovnvrf<id>` unless `dynamic-routing-vrf-name` names another. Unset or `false`, ovn-controller expects the VRF to exist already |
| `dynamic-routing-port-name` | string | Learns only the routes whose next hop is the interface of the named port. ovn-controller first looks the name up in `external_ids:dynamic-routing-port-mapping` of the local `Open_vSwitch` row, a comma-separated list of `port=interface` pairs, and otherwise uses the interface of the locally bound port of that name. It gives each uplink of a chassis its own router port |
| `dynamic-routing-redistribute` | comma-separated list | The router's list of the same name, for this port alone |
| `dynamic-routing-no-learning` | boolean | Disables route learning on this port and removes the routes it learned; it takes priority over the router's option |
| `routing-protocol-redirect` | string | The name of a logical switch port in the peer switch. Traffic to the router port's own addresses, its IPv6 link-local address included, and to the ports of the protocols in `routing-protocols` is redirected to that switch port, so a routing daemon bound there answers as if it were the router port |
| `routing-protocols` | comma-separated list | The protocols `routing-protocol-redirect` redirects: `BGP` (TCP port 179) and `BFD` (UDP port 3784) |

The peer-bridge model of `spec.fabric` sets neither redirect option. Its router
ports toward the leaves carry no address, and FRR peers over the interface of
the OVS peer bridge in the fabric VRF, not through a logical switch port. The
port mapping the operator writes names each bridge by its own name,
`br-fab1=br-fab1,br-fab2=br-fab2`, which is the port name the agent extension of
the `ovn-bgp` plugin writes into `dynamic-routing-port-name` (neutron 29.0.0,
`neutron/agent/ovn/extensions/bgp/events.py`, `LocalOVSEvent.run`).

## What ovn-controller does on a chassis

For a router whose `dynamic-routing-vrf-id` is `<id>`, ovn-controller uses the
VRF device `ovnvrf<id>` with routing table `<id>`. It maintains the VRF on a
chassis when three things hold: the router's datapath is local to the chassis,
as it is for a router with `options:chassis=<system-id>`, a port of the router with `dynamic-routing-maintain-vrf=true` has a peer
switch, and ovn-controller sees the option at a change of the port binding.

The third condition decides the order a CMS writes the rows in. On OVN 26.03.2 a
router, then a port created with `dynamic-routing-maintain-vrf=true`, then its
switch, each in its own `ovn-nbctl` call, left no VRF for more than 30 seconds,
until the option was toggled or `ovn-appctl inc-engine/recompute` ran on the
chassis. Setting the option last, after `ovn-nbctl --wait=hv sync`, created the
VRF within one second in three of three runs. FRR accepts a router for a VRF that
does not exist yet and announces its routes once ovn-controller creates it.

ovn-controller installs every advertised route of the router into the VRF as a
blackhole route, `blackhole <prefix> proto ovn metric 1000`, unless the router
sets a prefix next hop. frr-k8s redistributes those kernel routes and the fabric
router imports the VRF, which is how a route reaches the leaves. Routes the
speaker installs into the VRF of a learning router are read back into the
Southbound `Learned_Route` table, one row per route and router port.

`spec.fabric` addresses a VRF by its id: the generated `FRRConfiguration` names
`ovnvrf<id>`. A router that sets `dynamic-routing-vrf-name` is out of its reach.

## Worked example

The routers of the suite `tests/e2e-ovn-overlay/fabric-advertised-route/`,
written into the Northbound database the way a CMS writes them. `lr-adv`
advertises its static routes into VRF 42, two of them inside the chassis's
announceable ranges and two baits outside them. `lr-learn` learns the leaf
defaults into VRF 10 through one port per peer bridge.

<<< ../../../tests/e2e-ovn-overlay/fabric-advertised-route/routers.sh

## Observing

The two Southbound tables carry what OVN advertises and what it learned:

```bash
ovn-sbctl list Advertised_Route
ovn-sbctl list Learned_Route
```

On the chassis, the VRF of a router holds the routes ovn-controller installed for
it and the routes the speaker learned into it:

```bash
ip route show table 42
ip -6 route show table 42
ip route show table 10
```

The `RoutesAdvertised` condition of the `OVNChassis` reports the row counts of
both tables. The probe Job behind it runs once per 5-minute window, so the
condition is up to 5 minutes old, and the counts are those of the whole
Southbound database: the tables carry no chassis column. See
[OVNChassis CRD](../ovn/ovn-chassis-crd.md#conditions).
