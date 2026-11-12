---
title: BGP Fabric Contract
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# BGP Fabric Contract

This page is the interface between a CobaltCore OpenStack deployment and the
EVPN/VXLAN datacenter fabric it attaches to. It binds every chassis-bearing
node on one side and the leaf switches on the other, and it fixes the numbers
both sides configure: the peering, what a node may announce, how fast a
failure is detected, the two MTUs, the deployment VRF with its L3VNI, and which
addresses may ever reach the fabric. The network team builds the leaves
against it. The later phases of the BGP meta
[#899](https://github.com/C5C3/cobaltcore/issues/899) code against it: the
node substrate in [#1362](https://github.com/C5C3/cobaltcore/issues/1362), the
OVN dynamic-routing options in
[#1363](https://github.com/C5C3/cobaltcore/issues/1363), the Neutron `ovn-bgp`
plugin in [#1364](https://github.com/C5C3/cobaltcore/issues/1364), and the
EVPN evaluation in [#1365](https://github.com/C5C3/cobaltcore/issues/1365).

Nothing on this page depends on CobaltCore OpenStack code. A containerlab
topology proves it today, with FRR standing in for a node, two VTEP leaves, a
route reflector and an observer VTEP. The flat provider network the contract
carries is the one
[Create a Provider Network](../../guides/neutron/create-a-provider-network.md#toward-a-bgp-fabric)
builds, and the [Neutron Operator](../neutron/index.md) and
[OVN Operator](../ovn/index.md) pages point here for the fabric side.

![Two racks of chassis-bearing nodes. Each node keeps its platform-owned node network, which carries the InternalIP and the Geneve overlay, and has a fabric VRF with one loopback, 192.0.2.x/32. In each rack, node 1 and further nodes are dual-homed from the fabric VRF to the rack's leaf A and leaf B over MTU 9000 uplinks; every uplink carries an eBGP session over IPv6 link-local addresses with BFD at 300 ms and multiplier 3. A node announces only /32 and /128 host routes and accepts a default route; the leaf accepts only those, at most 8192, terminates the session in the deployment VRF, maps it to an L3VNI and exports the routes as Type-5 over both leaves toward the spines, which reflect EVPN routes. A node network bar runs under both racks, and a Geneve tunnel on UDP 6081 runs along it between node 1 of the first rack and node 1 of the second, never through a leaf. A dashed l2vpn evpn session from a node to a leaf is crossed out with the chip never: no EVPN to the node.](../../diagrams/compute-bgp-fabric.svg)

## Terms

| Term | Meaning | Source |
| --- | --- | --- |
| Chassis-bearing node | Any node an `OVNChassis` `spec.nodeSelector` matches. The guides label such a node `openstack.c5c3.io/chassis` | [OVNChassis CRD](../ovn/ovn-chassis-crd.md#spec), [Label a Node as a Compute or Network Node](../../guides/ovn/label-a-chassis-node.md) |
| Gateway node | The subset `spec.gateway.nodeSelector` picks. Its chassis announces `enable-chassis-as-gw` | [OVNGatewaySpec](../ovn/ovn-chassis-crd.md#ovngatewayspec), `operators/ovn/internal/controller/reconcile_nodes.go` |
| Node network | The network the platform delivers to the node. It carries the Kubernetes `InternalIP`, every Kubernetes flow and the Geneve overlay: the chassis init script writes the node's Kubernetes address into `external_ids:ovn-encap-ip`. CobaltCore OpenStack never touches it | `operators/ovn/internal/controller/reconcile_nodes.go` |
| Fabric VRF | The Linux VRF on the node that holds the two uplinks, the fabric loopback, the BGP sessions and every route the fabric sends | C10 below |
| Leaf pair | The two top-of-rack switches every node of a rack is dual-homed to | C1 below |
| Fabric | The leaves and spines together: an EVPN/VXLAN fabric whose VTEPs are the leaves, operated by the network team | C1 below |
| Deployment VRF | The VRF on each leaf that terminates the node sessions of one CobaltCore OpenStack deployment and is mapped to that deployment's L3VNI | C9 below |

The upstream BGP service plugin and its agent extension speak no BGP
themselves: "An external routing suite such as FRR must be deployed on every
node where BGP route advertisement is required (typically every compute and
network node)" (neutron 29.0.0, `doc/source/admin/ovn/bgp.rst`). The contract
therefore binds every chassis-bearing node, gateway nodes included.

## The contract

| Id | Item | Node side | Leaf side |
| --- | --- | --- | --- |
| C1 | Topology | The node network stays as delivered; two dedicated uplinks, one to each leaf of the rack's pair, as routed point-to-point links | Two node-facing ports per node; the leaves are the VTEPs of the EVPN/VXLAN fabric |
| C2 | Addressing | One IPv4 /32 and one IPv6 /128 fabric loopback on the fabric VRF device, apart from the `InternalIP`; IPv6 link-local addresses only on the uplinks | No global address on a node-facing port |
| C3 | Peering | One eBGP session per uplink, unnumbered, IPv4 and IPv6 unicast; a private 4-byte ASN per node | The session terminates in the deployment VRF with `remote-as external`; a remote VTEP holds a node's host route as two paths over two VTEP next hops |
| C4 | Announcements and filters | Announces `/32` and `/128` host routes from the announceable ranges only; accepts `0.0.0.0/0` and `::/0` only | Accepts the same host routes, at most 8192 per session; originates the default toward the node |
| C5 | Failover | BFD 300 ms × 3 on every session; `maximum-paths 2` | BFD 300 ms × 3 on every node session |
| C6 | MTU | MTU 9000 on both uplinks; `global_physnet_mtu` 9000; `[ml2] path_mtu` at the node network's MTU | MTU 9000 on node ports; at least 9050 on every fabric link that carries VXLAN (9070 over an IPv6 underlay) |
| C7 | Provider networks | One flat provider network per deployment, over OVS peer bridges that own the two uplinks | A plain unnumbered neighbour on each node port |
| C9 | Deployment VRF | Nothing: a node is no VTEP | One deployment VRF per deployment on every leaf, one L3VNI from 20000 to 29999, route target `<fabric ASN>:<L3VNI>`, a route distinguisher per leaf |
| C10 | Host isolation | Uplinks, loopback, sessions and the leaf defaults in the fabric VRF; no fabric route in the main table | The node sessions live in the deployment VRF, never in the leaf's default VRF |

C8 is the [address discipline](#address-discipline): it decides which ranges
the filters of C4 hold.

### Topology and addressing

C1: the node network stays as the platform delivers it, and CobaltCore
OpenStack never reconfigures a port of it. Every chassis-bearing node gains two
dedicated uplinks, one to each leaf of its rack's pair, and each uplink is a
routed point-to-point link. Those two links are the only L2 domains the fabric
shares with a node, and the fabric never learns a node-network, pod or service
prefix.

The fabric is an EVPN/VXLAN fabric. The leaves are the VTEPs; the spines carry
the underlay and the EVPN control plane. How they do that belongs to the
network team: the network-operator `Fabric` resource, for example, models an
OSPF or IS-IS underlay with iBGP EVPN and route reflectors
(`api/evpn/v1alpha1/fabric_types.go`). The contract prescribes nothing about
spine ASNs, AS paths or `multipath-relax` inside the fabric. It requires one
outcome: the fabric delivers a node's host routes to every other VTEP over both
leaves of the node's pair, as two paths with two VTEP next hops. The leaves
achieve that by exporting the routes as EVPN Type-5 routes (RFC 9136) under
distinct route distinguishers, which C9 fixes.

A node is not a VTEP. EVPN ends at the leaf.

C2: each node owns one IPv4 /32 and one IPv6 /128, its fabric loopback, on the
fabric VRF device, from ranges the network team owns. The IPv4 one is the
node's BGP router-id. The fabric loopback is not the Kubernetes `InternalIP`:
the chassis init script writes the node's Kubernetes address into
`external_ids:ovn-encap-ip`
(`operators/ovn/internal/controller/reconcile_nodes.go`), and `spec.encapType`
defaults to `geneve` (`operators/ovn/api/v1alpha1/ovnchassis_types.go`), so the
Geneve tunnels and UDP 6081 stay on the node network and never cross the
fabric. The uplinks carry no global IPv4 or IPv6 address, only IPv6 link-local
addresses.

Checks: A1 for the sessions over both uplinks, A2 for the two paths a remote
VTEP holds.

### Peering

C3: one eBGP session per uplink. The session is interface-based (unnumbered):
it runs between the IPv6 link-local addresses of the link, both address
families share it, and IPv4 reachability travels with an IPv6 next hop (RFC
8950; in FRR, `neighbor <ifname> interface`).

Each node has its own private 4-byte ASN from the RFC 6996 range
4200000000 to 4294967294. The leaf terminates the session in the deployment VRF
(`router bgp <fabric ASN> vrf <deployment VRF>` in FRR) and uses
`remote-as external`, so it accepts whatever node ASN the table assigns. The
contract uses no `allowas-in`, no `as-override` and no iBGP toward the node.

The network team owns the ASN table. This template carries the lab's values,
and a deployment adds one row per node:

| Device | Role | ASN |
| --- | --- | --- |
| `node1` | Chassis-bearing node | 4200000001 |
| `leaf1`, `leaf2`, `leaf3`, `spine1` | The lab fabric, as in the network-operator EVPN/VXLAN tutorial | 65000 |

Checks: A1 for the sessions in both families, A2 for the Type-5 routes over
both leaves.

### What a node announces and accepts

C4: a node announces host routes only. Today that is its fabric loopback /32
and /128. From #1363 on it also announces the /32 and /128 routes OVN
advertises for floating IPs, router gateway addresses and leaked tenant
addresses. The upstream `ovn-bgp` plugin sets `dynamic-routing-redistribute` to
`connected-as-host,nat` and `dynamic-routing-redistribute-local-only` to `true`
on its main router (neutron 29.0.0, `neutron/services/bgp/constants.py` and
`ReconcileMainRouterCommand` in `commands.py`). OVN 26.03.2 documents
`connected-as-host` as announcing the "individual IPs of a connected route ...
as host routes instead of announcing the subnet", and `nat` as one
`Advertised_Route` per NAT external IP (`ovn-nb.xml`, `Logical_Router`
options).

Outbound, the node applies a prefix-list that permits `/32` (`ge 32`) inside
the node loopback range and the fabric-announceable ranges of the
[address discipline](#address-discipline), `/128` (`ge 128`) likewise for
IPv6, and denies everything else, so a misconfigured or leaked prefix stays on
the node. Inbound, the node accepts `0.0.0.0/0` and `::/0` and nothing else,
into the fabric VRF and never into its main table. Each leaf originates the
default toward its nodes from the deployment VRF.

The leaf filters the same way. On a node session it accepts `/32` and `/128`
from the same ranges, and at most 8192 prefixes (`maximum-prefix 8192`). The
bound is far above the number of floating IPs one node holds and far below a
leaked full table. The fabric ECMPs a node's host routes across both leaves.

Checks: A4 for the prefixes the filters keep out in both directions, A6 for
the host-route-only announcements and the leaf's prefix bound.

### Failover

C5: every node session runs BFD with a transmit and receive interval of 300 ms
and a detect multiplier of 3, so a silent peer is declared down within 900 ms
of its last packet. The node runs `maximum-paths 2`: both uplinks carry
traffic before a failure, and the remaining one carries all of it afterwards.
The lab bounds the whole failover: BFD reports the peer down within 1000 ms of
the link going silent, and a remote VTEP has withdrawn the failed path within
2000 ms.

Check: A3.

### MTU

C6 fixes two MTUs, one per network.

The fabric MTU is 9000. Both uplinks of every node carry it, and Neutron's
`global_physnet_mtu` is 9000, the MTU of the flat provider network. Every
fabric link that carries VXLAN between VTEPs is at least 50 bytes larger than
the largest host-facing MTU over an IPv4 underlay (70 over IPv6): at least 9050
or 9070, so a 9000-byte frame from a node crosses the fabric unfragmented. The
lab runs 9216 on its fabric links, the value of the network-operator tutorial.

The tenant MTU follows the node network, which carries the Geneve overlay.
Neutron derives a Geneve tenant network's MTU from
`min(global_physnet_mtu, path_mtu)`, subtracts the IP header of
`[ml2] overlay_ip_version`, 20 bytes for IPv4 and 40 for IPv6 (`get_mtu` in
neutron 29.0.0 `neutron/plugins/ml2/drivers/type_tunnel.py`), then
`[ml2_type_geneve] max_header_size` (`type_geneve.py`). The Neutron operator
renders a `max_header_size` of 38
(`operators/neutron/internal/controller/reconcile_config.go`). The contract
binds `[ml2] path_mtu` to the node network's MTU.

The oslo default of `path_mtu` is 0, and with 0 `get_mtu` sizes tenant
networks from `global_physnet_mtu` alone. Over a 1500-byte node network that
hands tenants 8942 bytes the overlay cannot carry, so the default is the hazard
to avoid.

| `overlay_ip_version` | Node network MTU (`path_mtu`) | IP header | `max_header_size` | Tenant MTU |
| --- | --- | --- | --- | --- |
| 4 | 1500 | 20 | 38 | 1442 |
| 4 | 9000 | 20 | 38 | 8942 |
| 6 | 1500 | 40 | 38 | 1422 |
| 6 | 9000 | 40 | 38 | 8922 |

Tenant MTU = node network MTU − IP header − `max_header_size`. A tenant MTU of
1500 needs a node network MTU of at least 1558 over IPv4 or 1578 over IPv6.

The operator renders neither option today. Both are in the 2026.2 option
catalog (`operators/neutron/api/v1alpha1/catalogs/2026.2.json`), and
`spec.extraConfig` sets them ([Neutron CRD](../neutron/neutron-crd.md#spec)):
`DEFAULT: {global_physnet_mtu: "9000"}` and
`ml2: {path_mtu: "<node network MTU>"}`. Rendering them is #1364's. UDP 6081
and the overlay MTU are requirements on the node network, true of every
CobaltCore OpenStack deployment today, and none on the fabric.

Check: A5.

### Provider networks

C7: flat only, and one flat provider network per deployment. The upstream
plugin refuses anything else with a `BadRequest` (neutron 29.0.0,
`neutron/services/bgp/plugin.py`):

| Refused network | Message |
| --- | --- |
| A VLAN provider network | "VLAN provider networks are not supported when the BGP service plugin is enabled. Only flat provider networks are supported." |
| A second flat provider network | "Only a single flat provider network is supported when the BGP service plugin is enabled." |

The Neutron operator renders `type_drivers = geneve,flat`
(`reconcile_config.go`), so a VLAN network has no type driver to resolve
against even without the plugin.

The provider network's physical network maps to the OVS peer bridges that own
the two uplinks, one bridge per uplink. That is the shape the upstream agent
extension expects: it reads the bridges from
`external_ids:neutron-bgp-peer-bridges` on the `Open_vSwitch` row, each with
the NIC and a patch port to `br-int` (neutron 29.0.0,
`neutron/agent/ovn/extensions/bgp/events.py` and `bridge.py`). FRR peers over
the bridge's own interface inside the fabric VRF, as the upstream reference
`devstack/etc/frr_with_bfd/frr.conf` peers on `br-bgp-0` and `br-bgp-1`. The
per-chassis router's port toward each peer switch carries no address
(`ConnectChassisRouterToSwitchCommand` in `commands.py`), so the session is
IPv6 link-local only. The leaf sees a plain unnumbered neighbour on each port.

Check: no lab assertion, because the lab runs no Neutron. Once #1364 enables
the plugin, the two `BadRequest` responses are the check.

### The leaf side: VRF, L3VNI and route targets

C9: every leaf of the fabric carries one deployment VRF per CobaltCore
OpenStack deployment, mapped to one L3VNI. The VRF carries the fabric
loopbacks and the provider host routes of that deployment and nothing else.

- The L3VNI comes from the range 20000 to 29999, which the network team keeps
  for deployment VRFs.
- The route target is `<fabric ASN>:<L3VNI>`, imported and exported by every
  leaf of the deployment.
- The route distinguisher is per leaf, `<leaf loopback>:<n>` (`auto` in FRR,
  `Auto` in network-operator terms). Two leaves export the same host route
  under two route distinguishers, so a remote VTEP receives two distinct
  Type-5 routes and installs two paths.
- L3VNIs 10000 to 19999 stay reserved for tenant VRFs. The reservation becomes
  binding only with the decision #1365 records.

Neutron's `vni_ranges = 1:65536` (`reconcile_config.go`) are Geneve tunnel
keys inside OVN and share nothing with EVPN VNIs. The lab values are the VRF
`cobaltcore`, the L3VNI 20001 and the route target `65000:20001`.

Check: A2.

### Host isolation

C10: on the node, the two uplinks, the fabric loopback, the BGP sessions and
the default routes the leaves send live in a Linux VRF of their own, the
fabric VRF; the lab names it `fabric`. The node's main routing table holds no
fabric route, and the fabric VRF holds no node-network route, so a fabric
default can never steer Kubernetes traffic and the node network's default
never reaches the fabric.

How the fabric VRF connects to the `ovnvrf<id>` VRFs ovn-controller maintains
belongs to #1362 and #1363. The upstream reference leaks routes between its own
VRFs with `import vrf`.

On the leaf, the node sessions live in the deployment VRF, never in the leaf's
default VRF.

Check: A7.

## Address discipline

C8 decides what a node may ever announce. Neutron address scopes "build from
subnet pools" and prevent "the use of overlapping addresses in any two
subnets" of one scope; a subnet joins a scope by being allocated from a pool in
it (neutron 29.0.0, `doc/source/admin/config-address-scopes.rst`).

Rule 1. The fabric-announceable IPv4 and IPv6 space of a deployment is one
shared, admin-owned address scope per family, `fabric-v4` and `fabric-v6`.

Rule 2. The provider subnet, which holds the floating IPs and the router
gateway addresses, and every subnet that may be leaked are allocated from
subnet pools inside that scope. Neutron then refuses an overlap before the
plugin sees the subnet:

```shell
openstack address scope create --share --ip-version 4 fabric-v4
openstack address scope create --share --ip-version 6 fabric-v6
openstack subnet pool create --address-scope fabric-v4 \
  --pool-prefix <cidr> --default-prefix-length <n> <pool>
openstack subnet create --subnet-pool <pool> --prefix-length <n> \
  --network <network> <subnet>
```

The plugin adds its own conditions for a leaked subnet: it is on a Geneve
network, attached to a router with an external gateway, and overlaps no subnet
already leaked (`_validate_gateway_router_for_subnet` and
`_validate_no_cidr_overlap` in `plugin.py`).

Rule 3. Inside one deployment VRF, no announced CIDR overlaps another announced
CIDR, the node loopback range, the fabric's own loopback and underlay ranges,
the node network of any attached cluster, or its pod and service CIDRs. The
leaves' inbound prefix-lists and the nodes' outbound prefix-lists are generated
from the same table.

Rule 4. A subnet from an unscoped pool, or from no pool, is never leaked. A
leaked subnet sits on a network of its own: leaking exposes every address of
the network as a host route ("Known limitation" in `bgp.rst`).

| Range | Family | Purpose | Announced as | Owner |
| --- | --- | --- | --- | --- |
| `192.0.2.0/25` | IPv4 | Node fabric loopbacks | `/32` | Network team |
| `2001:db8::/64` | IPv6 | Node fabric loopbacks | `/128` | Network team |
| `198.51.100.0/24` | IPv4 | Provider subnet: floating IPs and router gateway addresses | `/32` | Cloud operator, pool in `fabric-v4` |
| `10.200.0.0/16` | IPv4 | Tenant leak pool (placeholder) | `/32` | Cloud operator, pool in `fabric-v4` |
| `192.0.2.128/25` | IPv4 | Fabric loopbacks and underlay links | never announced by a node | Network team |

The network team's copy of this table is the source of both filters, and the
lab's filters carry every announced row. The loopback, provider and fabric rows
are documentation ranges (RFC 5737, RFC 3849), so those lab values can never
collide with a real deployment. The leak pool is a placeholder each deployment
replaces, and no lab box announces from it.

## The leaf side in network-operator terms

The network team configures the switches later with the ironcore
[network-operator](https://ironcore.dev/network-operator/). Its API is
`v1alpha1`, and as of 2026-11-11 (`ironcore-dev/network-operator@3e70e998d`) it
has no SONiC provider: `internal/provider/` holds `cisco`, `nokia` and
`openconfig`. The table maps each leaf-side contract item to the kind and field
that carries it. It is prose a reader checks by hand, and the policy kinds'
own fields are the network team's to encode.

| Contract item | network-operator kind | Field | Value from the contract |
| --- | --- | --- | --- |
| C1 node-facing port | `Interface` (`api/core/v1alpha1/interface_types.go`) | `type`, `mtu`, `vrfRef`, `ipv6.useLinkLocalOnly` | `Physical`, 9000, the deployment VRF, `true` |
| C3 session | `BGPPeer` (`api/core/v1alpha1/bgppeer_types.go`) | `interfaceRef`, `asNumber`, `addressFamilies.ipv4Unicast.enabled`, `addressFamilies.ipv6Unicast.enabled` | The node-facing `Interface`, `external`, `true`, `true` |
| C4 inbound filter | `BGPPeer` (`api/core/v1alpha1/bgppeer_types.go`), `RoutingPolicy` (`api/core/v1alpha1/routingpolicy_types.go`), `PrefixSet` (`api/core/v1alpha1/prefixset_types.go`) | `inboundRoutingPolicyRef` under `addressFamilies.ipv4Unicast` and `addressFamilies.ipv6Unicast` | A `RoutingPolicy` over a `PrefixSet` that holds the address table's ranges, `/32` and `/128` only |
| C4 prefix limit | `BGPPeer` (`api/core/v1alpha1/bgppeer_types.go`) | None as of 2026-11-11 | 8192, set in the device configuration until the API carries a field |
| C5 BFD | `BGPPeer` (`api/core/v1alpha1/bgppeer_types.go`, `api/core/v1alpha1/bfd_types.go`) | `bfd.enabled`, `bfd.desiredMinimumTxInterval`, `bfd.requiredMinimumReceive`, `bfd.detectionMultiplier` | `true`, `300ms`, `300ms`, 3 |
| C9 VRF | `VRF` (`api/core/v1alpha1/vrf_types.go`) | `routeTargets`, `routeDistinguisher` | `65000:20001` import and export, `Auto` |
| C9 L3VNI | `EVPNInstance` (`api/core/v1alpha1/evpninstance_types.go`) | `type`, `vni`, `vrfRef` | `Routed`, 20001, the deployment VRF |
| C6 fabric links | `Interface` (`api/core/v1alpha1/interface_types.go`) | `mtu` | At least 9050; 9216 in the lab |

## Lab validation plan

containerlab is the gate. The topology under `tests/lab/bgp-fabric/` runs on
any Linux host with Docker, with FRR on every box, and `run.sh` checks the
seven assertions below against it. A run against the datacenter's real leaf
pair is the acceptance, and the network team performs it.

### What the lab proves

| Id | Contract item | Command | Bound |
| --- | --- | --- | --- |
| A1 | C1, C3 | `show bgp vrf cobaltcore summary json` on `leaf1` and `leaf2`; `show bgp summary json` on `spine1`; `show bfd peers json` on `leaf1` and `leaf2` | Within 60 s of `up`: the `eth1` peer `Established` in `ipv4Unicast` and `ipv6Unicast` on both leaves, `192.0.2.129`, `.130` and `.131` `Established` in `l2VpnEvpn` on `spine1`, and the node's BFD peer `status` `up` on both leaves |
| A2 | C2, C3, C9 | On `leaf3`: `show bgp vrf cobaltcore ipv4 unicast 192.0.2.1/32 json`, `show bgp vrf cobaltcore ipv6 unicast 2001:db8::1/128 json` and `ip -j route show vrf cobaltcore 192.0.2.1`; on `spine1`: `show bgp l2vpn evpn route type prefix json` | Exactly 2 paths in each family with the next hops `192.0.2.129` and `192.0.2.130`, one `bestpath` and the other `multipath`; 2 next hops in the kernel; the key `[5]:[0]:[32]:[192.0.2.1]` under two route distinguishers, `192.0.2.129:<n>` and `192.0.2.130:<n>`. The AS path is not asserted |
| A3 | C5 | On `leaf1`: `tc qdisc add dev eth1 root netem loss 0%`, `tc qdisc add dev eth1 ingress`, `tc filter add dev eth1 ingress matchall action drop` and `tc qdisc change dev eth1 root netem loss 100%`, then `show bfd peers json` every 20 ms; on `leaf3`: `show bgp vrf cobaltcore ipv4 unicast 192.0.2.1/32 json` every 100 ms | 2 paths on `leaf3` within 10 s before the silence; BFD `down` within 1000 ms and 1 path on `leaf3` within 2000 ms of the silence; 2 paths again within 60 s of the restore. `SKIP` with the kernel's text when `tc` reports "kind is unknown" (no `sch_netem`), "TC classifier not found" or "Failed to load TC action module" |
| A4 | C4 | On `node1`: `show bgp vrf fabric ipv4 unicast 203.0.113.0/24 json`, `show bgp vrf fabric ipv4 unicast neighbors eth1 advertised-routes json`, `show bgp vrf fabric ipv4 unicast 198.18.0.0/15 json`, `show bgp vrf fabric ipv4 unicast 0.0.0.0/0 json` and `show bgp vrf fabric ipv6 unicast ::/0 json`; on `leaf1`, `leaf2` and `leaf3`: `show bgp vrf cobaltcore ipv4 unicast 203.0.113.0/24 json`; on `leaf1`: `show bgp vrf cobaltcore ipv4 unicast 198.18.0.0/15 json` | Both baits present first; `203.0.113.0/24` held on `node1`, absent from its advertised routes and from all three leaves; `198.18.0.0/15` held on `leaf1`, absent on `node1`; 2 paths for `0.0.0.0/0` and for `::/0` on `node1` |
| A5 | C6 | `ip -j link show` on every box; on `node1`, after `ip -6 route flush cache`, `ping -6 -c 1 -s 8952 -I fabric 2001:db8:0:ff::3` and the same with `-s 8953`, reading `Ip6FragCreates` in `/proc/net/snmp6` around each | `mtu` 9000 on the four node-link ends (`eth1` and `eth2` on `node1`, `eth1` on `leaf1` and `leaf2`), 9216 on the six fabric-link ends; the 9000-byte probe answered with `Ip6FragCreates` unchanged; the 9001-byte probe raises it |
| A6 | C4 | `show bgp vrf fabric ipv4 unicast neighbors eth1 advertised-routes json` and its `ipv6` twin on `node1`; `show running-config` on `leaf1` | Every key of `advertisedRoutes` ends in `/32` (IPv4) or `/128` (IPv6); `neighbor nodes maximum-prefix 8192` present |
| A7 | C10 | On `node1`: `ip -j route show default`, `ip -j route show vrf fabric default`, `ip -j route show`, `ip -j -6 route show`, `ip -j route show vrf fabric` and its `-6` twin, and `ping -c 1 -I eth0 <gateway>`; on `leaf1`: `show bgp summary json` and `show bgp vrf cobaltcore summary json` | One main-table default, via `eth0`; one fabric-VRF default over `eth1` and `eth2`; no main-table entry via `eth1` or `eth2` and none for `198.51.100.0/24`, `198.18.0.0/15` or a default via a fabric interface; no fabric-VRF entry via `eth0`; the gateway of the main default answers over `eth0`; `leaf1` lists the `eth1` peer in `cobaltcore` and not in its default VRF |

Each box runs its FRR commands through `vtysh -c '<command> json'`, and
`run.sh` reads the answers with `jq`. An empty answer fails the assertion that
asked. A2 reads the lab for up to 30 s, because the sessions of A1 come up
before the Type-5 routes have reached `leaf3`.

Two prefixes are bait. `203.0.113.0/24` is a connected prefix in `node1`'s
fabric VRF that the outbound filter has to keep on the node. `198.18.0.0/15` is
a blackhole route of each leaf's deployment VRF, which the node's inbound
filter has to drop. A4 first checks that both are in place, because a missing
bait proves no filter.

A3 silences the link in both directions with its carrier up: the ingress
filter drops what `leaf1` receives from the node, and `netem` drops what it
sends. With the egress dropped alone, `leaf1` would keep receiving the node's
BFD packets, and the link would be silent in one direction only. The clock
of A3 starts once the ingress drop is in place, because from then on `leaf1`
receives nothing from the node. `netem` goes on at 0% loss before the ingress
drop and rises to 100% after it through `tc qdisc change`, so no `tc` call
that may load a kernel module runs on the clock. A host kernel without
`sch_netem`, or without the `sch_ingress`, `cls_matchall` and `act_gact`
modules the ingress drop needs, makes A3 report `SKIP` with the kernel's text,
and the run record shows it. Any other `tc` failure, a missing `tc` binary in
the image included, fails A3.

A5 probes over IPv6 because the image's BusyBox `ping` has no `-M do` and IPv6
routers never fragment in transit. `-I fabric` binds the socket to the VRF
device, so the lookup uses the fabric table and the source is the fabric
loopback; a source-address bind alone would look up the main table and leave
through `eth0`. The target sits in `leaf3`'s deployment VRF, so the probe
crosses a node link at 9000 and the VXLAN tunnel between `leaf1` or `leaf2` and
`leaf3` at 9216. Linux fragments the 9001-byte probe at the source, so the exit
code of that ping says nothing about the ceiling; A5 reads the fragment counter
instead. The cache flush keeps a path MTU learnt in an earlier run from making
the sender fragment the 9000-byte probe.

### Topology

`bgp-fabric.clab.yml` runs five containers of the pinned FRR image. `node1` is
dual-homed to `leaf1` and `leaf2` at MTU 9000; `leaf1`, `leaf2` and the
observer `leaf3` sit under `spine1` at 9216. The management network on `eth0`
belongs to the lab, with subnets Docker assigns, and `node1` keeps its `eth0`
with Docker's default routes as the stand-in for the node network. Every box
starts from its `setup.sh`, which waits for the links containerlab attaches,
builds the VRF and VXLAN devices FRR reads at start, and then starts FRR; the
leaves and the spine run FRR as well, and no SONiC image takes part.

<<< ../../../tests/lab/bgp-fabric/bgp-fabric.clab.yml

This is the node side of the contract in FRR syntax:

<<< ../../../tests/lab/bgp-fabric/node1/frr.conf

`leaf1/frr.conf` and `leaf2/frr.conf` carry the leaf side. The VRF
`cobaltcore` maps to `vni 20001` and holds the bait. OSPF area 0 and an iBGP
`l2vpn evpn` session to `spine1` with `advertise-all-vni` form the underlay
instance, which carries no IPv4 unicast. The deployment VRF instance
`router bgp 65000 vrf cobaltcore` holds the `nodes` peer-group with BFD, the
`NODES-IN-V4` and `NODES-IN-V6` prefix-lists, `maximum-prefix 8192`,
`default-originate` and `bgp bestpath as-path multipath-relax`. Two nodes with
different ASNs may announce the same provider host route after a floating IP
moves, and the leaf keeps both. The instance exports the VRF's routes as
Type-5 with the route target `65000:20001`. `leaf3/frr.conf` is the same leaf
without a node: it announces `2001:db8:0:ff::3/128` and runs
`maximum-paths ibgp 2`, because the imported Type-5 routes arrive over the
iBGP session with the reflector. `spine1/frr.conf` runs OSPF over its three
links and reflects `l2vpn evpn` to the three leaves; it terminates no VRF and
runs no VXLAN device.

### Running the lab

The lab needs a Linux host with Docker, a user in the `docker` group, `jq`,
and a kernel that ships the `vrf` and `vxlan` modules. It needs no root and no
installed containerlab: `run.sh` runs containerlab from the digest-pinned
`ghcr.io/srl-labs/clab` image. containerlab works in the host's network
namespace, so Docker Desktop on macOS cannot run the lab.

```shell
tests/lab/bgp-fabric/run.sh
```

| Subcommand | Effect |
| --- | --- |
| `up` | `containerlab deploy --reconfigure`, which replaces a lab of the same name |
| `check` | Runs A1 to A7 against the deployed lab, prints one `PASS`, `FAIL` or `SKIP` line per assertion with the measured value and then the run record, and exits 1 on a `FAIL` |
| `down` | `containerlab destroy --cleanup` |
| `all` | `up`, `check`, then `down`, which also runs when `up` or `check` fails or the run is interrupted; the default |

| Variable | Default | Effect |
| --- | --- | --- |
| `KEEP_LAB` | `false` | `true` makes `all` skip `down`, so a failed lab can be inspected |
| `CLAB_IMAGE` | the digest-pinned `ghcr.io/srl-labs/clab` image in `run.sh` | The containerlab image `run.sh` runs |
| `A1_WAIT_MS`, `A2_WAIT_MS`, `A3_READY_WAIT_MS`, `A3_RESTORE_WAIT_MS` | 60000, 30000, 10000, 60000 | How long `check` waits for a state before it fails; the bounds of A3 are fixed |

containerlab restarts a box whose entrypoint exits. When a `setup.sh` fails, a
missing kernel module for example, `run.sh` names the restarted container,
prints the last 20 lines of its log and ends the run.

::: warning What a run grants
The containerlab container runs `--privileged`, with the Docker socket and the
host's PID and network namespaces. For the duration of a run that is root on
the host. Run the lab on a workstation or a disposable VM, never on a shared
cluster node.
:::

No CI job runs the lab; that job belongs to #1362. Renovate watches the FRR and
the containerlab image without automerge, so a bump stays unverified until
someone runs `run.sh` and refreshes the record below.

### Leaf-pair acceptance

The network team owns this run. It configures the real leaf pair from the
contract and reads each assertion back with the vendor's own commands:

| Id | Configure | Read and expect |
| --- | --- | --- |
| A1 | An unnumbered node-facing peer in the deployment VRF over IPv6 link-local addresses, both address families, BFD 300 ms × 3 | Both node sessions established for IPv4 and IPv6, both BFD sessions up |
| A2 | The deployment VRF mapped to its L3VNI with the route target, the route distinguisher per leaf | On a third VTEP, the node's /32 and /128 as two paths over the two VTEP next hops |
| A3 | BFD on every node session, as for A1 | One node port silenced with its carrier up, for example by an ACL that drops every packet on the port: BFD down within 1000 ms, one path on the third VTEP within 2000 ms, two paths again after the restore |
| A4 | The inbound prefix-list from the address table, and default origination into the deployment VRF toward the node | A node prefix outside the table is refused; the node's fabric VRF holds the default from both leaves and no other fabric route |
| A5 | MTU 9000 on node ports, at least 9050 on fabric links | The vendor's interface command shows the MTU on each port; a 9000-byte IPv6 probe from a node's fabric loopback to a loopback in the deployment VRF of a third VTEP is answered without fragmentation |
| A6 | The prefix limit 8192 on every node session | The running configuration shows the limit; the routes received from a node are /32 and /128 only |
| A7 | The node port in the deployment VRF | The vendor's VRF membership command lists the node port in the deployment VRF and not in the default VRF |

The record below is filled in when the network team runs it.

### Run records

#### containerlab

Run on 2026-11-12 from commit 621f0ede5 (lab tree 56001fb7f) with containerlab
0.81.1 and `quay.io/frrouting/frr@sha256:e995beaa50fdc9edb35eadcfefa29b7f062cc06f2b812613789b68fa541554d2`.

| Assertion | Result | Measured |
| --- | --- | --- |
| A1 | PASS | leaf1 and leaf2 eth1 Established in ipv4Unicast and ipv6Unicast with BFD up, spine1 l2VpnEvpn Established to 192.0.2.129, .130 and .131, after 15 s |
| A2 | PASS | 2 paths for 192.0.2.1/32 and 2001:db8::1/128 on leaf3 over the VTEPs 192.0.2.129 and 192.0.2.130, one bestpath and one multipath each, 2 kernel next hops; spine1 holds [5]:[0]:[32]:[192.0.2.1] under 192.0.2.129:3 and 192.0.2.130:2 |
| A3 | PASS | BFD down after 670 ms, leaf3 at 1 path after 950 ms, 2 paths again 2 s after the restore |
| A4 | PASS | 203.0.113.0/24 held by node1, absent from its advertised routes on eth1 and from leaf1, leaf2 and leaf3; 198.18.0.0/15 held by leaf1, absent on node1; 2 paths for 0.0.0.0/0 and for ::/0 on node1 |
| A5 | PASS | 4 node link ends at MTU 9000, 6 fabric link ends at 9216; the 9000-byte probe to 2001:db8:0:ff::3 answered with 0 fragments; the 9001-byte probe cut into 2 fragments at node1 |
| A6 | PASS | node1 advertises only /32 (IPv4) and /128 (IPv6) keys on eth1; leaf1 runs neighbor nodes maximum-prefix 8192 |
| A7 | PASS | node1's main table: one default via eth0 (gateway 172.24.0.1 answers), no route via eth1 or eth2 or to a bait; fabric VRF: one default over eth1 and eth2, no route via eth0; leaf1 peers on eth1 in cobaltcore only |

#### Leaf pair

No acceptance run has been recorded yet. The network team records the first
one here.

## See also

- [Create a Provider Network](../../guides/neutron/create-a-provider-network.md):
  the flat provider network and the gateway label the contract builds on
- [Label a Node as a Compute or Network Node](../../guides/ovn/label-a-chassis-node.md):
  the labels that make a node chassis-bearing or a gateway
- [OVNChassis CRD](../ovn/ovn-chassis-crd.md): `nodeSelector`, `gateway` and
  `encapType`
- [Neutron CRD](../neutron/neutron-crd.md): `extraConfig`, the lever for
  `global_physnet_mtu` and `path_mtu`
- [Neutron Operator](../neutron/index.md) and [OVN Operator](../ovn/index.md)
- [Architecture Diagrams](../../contributing/architecture-diagrams.md): the
  conventions of the figure on this page
- [#899](https://github.com/C5C3/cobaltcore/issues/899), the BGP meta, and its
  phases [#1362](https://github.com/C5C3/cobaltcore/issues/1362),
  [#1363](https://github.com/C5C3/cobaltcore/issues/1363),
  [#1364](https://github.com/C5C3/cobaltcore/issues/1364) and
  [#1365](https://github.com/C5C3/cobaltcore/issues/1365)
