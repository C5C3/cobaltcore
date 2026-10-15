---
title: BGP Fabric Contract
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# BGP Fabric Contract

This page is the interface between a CobaltCore deployment and the datacenter
fabric it attaches to over BGP. It binds every chassis-bearing node on one side
and the leaf and spine switches on the other, and it fixes the numbers both
sides configure: the peering, what a node may announce, how fast a failure is
detected, the MTU, and which addresses may ever reach the fabric. The network
team builds the leaf pair against it. The later phases of
[#899](https://github.com/C5C3/cobaltcore/issues/899) code against it: the
frr-k8s substrate in [#1362](https://github.com/C5C3/cobaltcore/issues/1362),
the OVN dynamic-routing options in
[#1363](https://github.com/C5C3/cobaltcore/issues/1363) and the Neutron
`ovn-bgp` plugin in [#1364](https://github.com/C5C3/cobaltcore/issues/1364).

Nothing on this page depends on CobaltCore code. A containerlab topology proves
the node side today, with FRR standing in for a node. The flat provider network
the contract carries is the one
[Create a Provider Network](../../guides/neutron/create-a-provider-network.md#toward-a-bgp-fabric)
builds, and the [Neutron Operator](../neutron/index.md) and
[OVN Operator](../ovn/index.md) pages track BGP itself in #899.

![Two racks of chassis-bearing nodes. In each rack, node 1 and further nodes are dual-homed to the rack's leaf A and leaf B over MTU 9000 uplinks; every uplink carries an eBGP session over IPv6 link-local addresses with BFD at 300 ms and multiplier 3. Both leaves connect to two spines. Each node has one loopback, 192.0.2.x/32, and announces only /32 and /128 host routes; the leaf accepts only those, at most 8192, and sends the node a default route. A Geneve tunnel on UDP 6081 runs loopback to loopback between node 1 of the first rack and node 1 of the second. A dashed L2 link between the racks is crossed out with the chip never: no stretched L2.](../../diagrams/compute-bgp-fabric.svg)

## Terms

- A *chassis-bearing node* is any node an `OVNChassis` `spec.nodeSelector`
  matches ([OVNChassis CRD](../ovn/ovn-chassis-crd.md#spec)). The guides label
  such a node `openstack.c5c3.io/chassis`
  ([Label a Node as a Compute or Network Node](../../guides/ovn/label-a-chassis-node.md)).
- A *gateway node* is the subset `spec.gateway.nodeSelector` picks
  ([OVNGatewaySpec](../ovn/ovn-chassis-crd.md#ovngatewayspec)). Its chassis
  announces `enable-chassis-as-gw`
  (`operators/ovn/internal/controller/reconcile_nodes.go`).
- A *leaf pair* is the two top-of-rack switches every node of a rack is
  dual-homed to.
- The *fabric* is the leaves and the spines together, operated by the network
  team.

Neither the upstream BGP service plugin nor its agent extension speaks BGP. An
external routing suite such as FRR must run on every node that advertises
routes, typically every compute and network node (neutron 29.0.0,
`doc/source/admin/ovn/bgp.rst`). The contract therefore binds every
chassis-bearing node, gateway nodes included.

## The contract

| Id | Item | Node side | Fabric side |
| --- | --- | --- | --- |
| C1 | Topology | Two routed point-to-point uplinks, one to each leaf of its rack's pair | Leaf and spine with ECMP between them; no VLAN or L2 segment spans racks |
| C2 | Addressing | One IPv4 /32 and one IPv6 /128 on the loopback, the IPv4 one is the Kubernetes `InternalIP`; IPv6 link-local addresses only on the uplinks | Routes the loopbacks; no global address on a node-facing port |
| C3 | Peering | One eBGP session per uplink, unnumbered, both address families; a private 4-byte ASN of its own | One eBGP session per node port; an ASN per leaf and per spine; `bgp bestpath as-path multipath-relax` on leaves and spines |
| C4 | Announcements and filters | Announces /32 and /128 host routes from the announceable ranges only; accepts default routes only | Accepts /32 and /128 from the same ranges, at most 8192 prefixes per session; originates the default toward the node; ECMPs a node's host routes over both leaves |
| C5 | Failover | BFD 300 ms × 3 on every session; `maximum-paths 2` | BFD 300 ms × 3 on every node-facing session |
| C6 | MTU | MTU 9000 on both uplinks; Neutron `global_physnet_mtu` 9000 | MTU 9000 on every port toward nodes and spines; UDP 6081 between node loopbacks unfiltered |
| C7 | Provider networks | One flat provider network per deployment, over the two uplinks | Sees its addresses as host routes under C4 and nothing else |
| C9 | EVPN reservation | L3VNIs 10000 to 19999 for tenant VRFs, binding with #1365 | Keeps 10000 to 19999 free of other VXLAN or EVPN use |

C8 is the [address discipline](#address-discipline): it decides which ranges
the filters of C4 hold.

### Topology and addressing

C1: every chassis-bearing node has two uplinks, one to each leaf of its rack's
pair, and each uplink is a routed point-to-point link. No VLAN and no other L2
segment spans racks; the only L2 domain a node is on is each of its two links.
Leaves and spines form the fabric, with ECMP between the two layers.

C2: each node owns one IPv4 /32 and one IPv6 /128 on its loopback, and the
IPv4 loopback is the node's Kubernetes `InternalIP`. That address is the tunnel
endpoint. The chassis init script writes the node's Kubernetes address into
`external_ids:ovn-encap-ip`
(`operators/ovn/internal/controller/reconcile_nodes.go`), and
`spec.encapType` defaults to `geneve`
(`operators/ovn/api/v1alpha1/ovnchassis_types.go`). Geneve tunnels therefore
run loopback to loopback and keep flowing over the second uplink when the first
one fails. The uplinks carry no global IPv4 or IPv6 address, only IPv6
link-local addresses.

Checks: A1 for the sessions over both uplinks, A2 for the loopback route over
both leaves.

### Peering

C3: one eBGP session per uplink. The session is interface-based (unnumbered):
it runs between the IPv6 link-local addresses of the link, and both address
families share it, IPv4 reachability travelling with an IPv6 next hop (RFC
8950; in FRR, `neighbor <ifname> interface`).

Each node has its own private 4-byte ASN from the RFC 6996 range 4200000000 to
4294967294, and each leaf and each spine has its own ASN too, the numbering RFC
7938 describes for large datacenters. Leaves and spines run
`bgp bestpath as-path multipath-relax`. A node's host route reaches a spine
over two leaves, with two AS paths of equal length that differ in the leaf
ASN, and without the option the spine installs only one of them. The contract
uses no `allowas-in`, no `as-override` and no iBGP.

The network team owns the ASN table. This template carries the lab's values,
and a deployment adds one row per node and per switch:

| Device | Role | ASN |
| --- | --- | --- |
| `node1` | Chassis-bearing node | 4200000001 |
| `leaf1` | Leaf A of the node's rack | 4200001001 |
| `leaf2` | Leaf B of the node's rack | 4200001002 |
| `spine1` | Spine | 4200000100 |

Checks: A1 for the sessions in both families, A2 for the two AS paths the
spine keeps.

### What a node announces and accepts

C4: a node announces host routes only. Today that is its loopback /32 and
/128. From #1363 on it also announces the /32 and /128 routes OVN advertises
for floating IPs, router gateway addresses and leaked tenant addresses. The
upstream `ovn-bgp` plugin sets `dynamic-routing-redistribute` to
`connected-as-host,nat` and `dynamic-routing-redistribute-local-only` to
`true` on its main router (neutron 29.0.0, `neutron/services/bgp/constants.py`
and `commands.py`). OVN 26.03.2 documents `connected-as-host` as announcing the
individual addresses of a connected network "as host routes instead of
announcing the subnet", and `nat` as one advertised route per NAT external IP
(`ovn-nb.xml`). Every address a node advertises is a host route, bound to the
chassis that holds it.

Outbound, the node applies a prefix-list that permits /32 (`ge 32`) inside the
node loopback range and the fabric-announceable ranges of the
[address discipline](#address-discipline), /128 (`ge 128`) likewise for IPv6,
and denies everything else, so a misconfigured or leaked prefix stays on the
node. Inbound, the node accepts default routes only: `0.0.0.0/0` and `::/0`.
Each leaf originates the default toward its nodes.

The fabric filters the same way. On a node session each leaf accepts /32 and
/128 from the same ranges, and at most 8192 prefixes (`maximum-prefix 8192`).
The bound is far above the number of floating IPs one node holds and far below
a leaked full table. The fabric ECMPs a node's host routes across both leaves.

Checks: A4 for the prefixes the filters keep out in both directions, A6 for
the host-route-only announcements and the leaf's prefix bound.

### Failover

C5: every session runs BFD with a transmit and receive interval of 300 ms and a
detect multiplier of 3, so a silent peer is declared down within 900 ms of its
last packet. The node runs `maximum-paths 2`: both uplinks carry traffic
before a failure, and the remaining one carries all of it afterwards. The lab
bounds the whole failover: BFD reports the peer down within 1000 ms of the link
going silent, and the spine has withdrawn the failed path within 2000 ms.

Check: A3.

### MTU

C6: every fabric link and both uplinks of every node carry an MTU of 9000
bytes. The loopback addresses sit on `lo`, whose own MTU plays no part: a
packet from a loopback address leaves through an uplink. Neutron's
`global_physnet_mtu` is 9000 as well. The operator renders no
`global_physnet_mtu`, so the oslo default of 1500 applies today, and
`spec.extraConfig` sets the option under `[DEFAULT]`
([Neutron CRD](../neutron/neutron-crd.md#spec)):

```yaml
spec:
  extraConfig:
    DEFAULT:
      global_physnet_mtu: "9000"
```

Changing the operator's default is #1364's call.

Neutron derives a Geneve tenant network's MTU from
`min(global_physnet_mtu, path_mtu)`. It subtracts the IP header of
`[ml2] overlay_ip_version`, 20 bytes for IPv4 and 40 for IPv6 (`get_mtu` in
`neutron/plugins/ml2/drivers/type_tunnel.py`), then
`[ml2_type_geneve] max_header_size` (`get_mtu` in `type_geneve.py`). The
Neutron operator renders a `max_header_size` of 38
(`operators/neutron/internal/controller/reconcile_config.go`), the floor OVN
accepts (`OVN_MIN_GENEVE_MAX_HEADER_SIZE = 38` in neutron 29.0.0
`mech_driver.py`). Over an IPv4 underlay the overhead is 38 + 20 = 58 bytes:

| Underlay | Node MTU | IP header | `max_header_size` | Tenant MTU |
| --- | --- | --- | --- | --- |
| IPv4 | 9000 | 20 | 38 | 8942 |
| IPv6 | 9000 | 40 | 38 | 8922 |

`overlay_ip_version` defaults to 4 whatever the underlay carries
(`neutron/conf/plugins/ml2/config.py`), and the operator renders none, so the
IPv6 row holds only with `ml2: {overlay_ip_version: "6"}` in
`spec.extraConfig`. C2 makes the tunnel endpoints IPv4, so the IPv4 row is the
binding one.

The same arithmetic gives the binding minimum: a node MTU of at least 1558
bytes over an IPv4 underlay, or 1578 over IPv6, keeps a tenant MTU of 1500.
UDP 6081 between node loopbacks passes the fabric unfiltered.

Check: A5.

### Provider networks

C7: flat only, and one flat provider network per deployment. The upstream
plugin refuses anything else with a `BadRequest` (neutron 29.0.0,
`neutron/services/bgp/plugin.py`):

| Refused network | Message |
| --- | --- |
| A VLAN provider network | "VLAN provider networks are not supported when the BGP service plugin is enabled. Only flat provider networks are supported." |
| A second flat provider network | "Only a single flat provider network is supported when the BGP service plugin is enabled." |

The Neutron operator renders `type_drivers = geneve,flat` (`reconcile_config.go`),
so a VLAN network has no type driver to resolve against even without the
plugin. The provider network's physical network maps to a bridge whose uplinks
are the node's two fabric links. Whether the host or an OVS bridge owns the
uplinks is settled by #1363 and #1364, and the fabric sees no difference.

Check: no lab assertion, because the lab runs no Neutron. Once #1364 enables
the plugin, the two `BadRequest` responses are the check.

### Reserved for EVPN

C9: L3VNIs 10000 to 19999 are reserved for tenant VRFs, one VNI per VRF. A
route target takes the form `<fabric ASN>:<VNI>`, a route distinguisher
`<node loopback>:<VNI>`. Neutron's `vni_ranges = 1:65536`
(`reconcile_config.go`) are Geneve tunnel keys inside OVN and share nothing
with EVPN VNIs. The fabric keeps 10000 to 19999 free of other VXLAN or EVPN
use. The reservation becomes binding only with the decision #1365 records.

Check: none until #1365.

## Address discipline

C8 decides what a node may ever announce. Neutron address scopes "build from
subnet pools" and prevent "the use of overlapping addresses in any two
subnets" of one scope; a subnet joins a scope by being allocated from a pool in
it (neutron 29.0.0, `doc/source/admin/config-address-scopes.rst`).

Rule 1. The fabric-announceable IPv4 and IPv6 space of a deployment is one
shared, admin-owned address scope per family, `fabric-v4` and `fabric-v6`:

```shell
openstack address scope create --share --ip-version 4 fabric-v4
openstack address scope create --share --ip-version 6 fabric-v6
```

Rule 2. The provider subnet, which holds the floating IPs and the router
gateway addresses, and every subnet that may be leaked are allocated from
subnet pools inside that scope. Neutron then refuses an overlap before the
plugin sees the subnet:

```shell
openstack subnet pool create --address-scope fabric-v4 \
  --pool-prefix <cidr> --default-prefix-length <n> <pool>
openstack subnet create --subnet-pool <pool> --prefix-length <n> \
  --network <network> <subnet>
```

The plugin adds its own conditions for a leaked subnet: it is on a Geneve
network, attached to a router with an external gateway, and overlaps no subnet
already leaked (`_validate_gateway_router_for_subnet` and
`_validate_no_cidr_overlap` in `plugin.py`).

Rule 3. Outside EVPN VRFs, no announced CIDR overlaps another announced CIDR, a
node loopback range, a leaf or spine infrastructure range, or the Kubernetes
pod and service CIDRs of any attached cluster. The fabric's prefix-lists and
the node's outbound prefix-list are generated from the same table.

Rule 4. A subnet from an unscoped pool, or from no pool, is never leaked. A
leaked subnet sits on a network of its own: leaking connects the plugin's
router to the whole network, and every address on it becomes a host route
("Known limitation" in `bgp.rst`).

| Range | Family | Purpose | Announced as | Owner |
| --- | --- | --- | --- | --- |
| `192.0.2.0/24` | IPv4 | Node loopbacks | `/32` | Network team |
| `2001:db8::/64` | IPv6 | Node loopbacks | `/128` | Network team |
| `198.51.100.0/24` | IPv4 | Provider subnet: floating IPs and router gateway addresses | `/32` | Cloud operator, pool in `fabric-v4` |
| `10.200.0.0/16` | IPv4 | Tenant leak pool (placeholder) | `/32` | Cloud operator, pool in `fabric-v4` |

The network team's copy of this table is the source of both filters, the
leaves' inbound lists and the nodes' outbound lists. The loopback and provider
rows are documentation ranges (RFC 5737, RFC 3849), so the lab's values can
never collide with a real deployment, and the lab's filters carry those three
rows.

## Lab validation plan

containerlab is the gate. The topology under `tests/lab/bgp-fabric/` puts one
node, dual-homed to a leaf pair under one spine, on any Linux host with Docker,
with FRR on every box, and `run.sh` checks the six assertions below against
it. A run against the datacenter's real leaf pair is the acceptance, and the
network team performs it.

### What the lab proves

| Id | Contract item | Command | Bound |
| --- | --- | --- | --- |
| A1 | C1, C3 | `show bgp summary json` on `leaf1` and `leaf2` (peer `eth1`) and on `spine1` (peers `eth1` and `eth2`), in `ipv4Unicast` and `ipv6Unicast`; `show bfd peers json` on `leaf1` and `leaf2` | Every peer `Established` and the node's BFD peer `status` `up` within 60 s of `up` |
| A2 | C2, C3, C4 | `show bgp ipv4 unicast 192.0.2.1/32 json` and `show bgp ipv6 unicast 2001:db8::1/128 json` on `spine1`; `ip -j route show 192.0.2.1` on `spine1` | 2 paths in each family with the AS paths `4200001001 4200000001` and `4200001002 4200000001`, one `bestpath` and the other `multipath`; 2 next hops in the kernel |
| A3 | C5 | `tc qdisc add dev eth1 root netem loss 100%` plus an ingress drop on `leaf1`; `show bfd peers json` on `leaf1` sampled every 20 ms; `show bgp ipv4 unicast 192.0.2.1/32 json` on `spine1` sampled every 100 ms | BFD `down` within 1000 ms and 1 path on `spine1` within 2000 ms of the silence; 2 paths again within 60 s of the restore. `SKIP` with the `tc` error on a host kernel without `sch_netem` |
| A4 | C4 | `show bgp ipv4 unicast 203.0.113.0/24 json` on all four boxes; `show bgp ipv4 unicast neighbors eth1 advertised-routes json` on `node1` and `leaf1`; `show bgp ipv4 unicast 198.18.0.0/15 json`, `show bgp ipv4 unicast 0.0.0.0/0 json` and `show bgp ipv6 unicast ::/0 json` on `node1` | `203.0.113.0/24` held on `node1`, neither advertised by it nor held on `leaf1`, `leaf2` or `spine1`; `198.18.0.0/15` offered by `leaf1`, absent on `node1`; 2 paths for each default |
| A5 | C6 | `ip -j link show` on all four boxes; on `node1`, after `ip -6 route flush cache`, `ping -6 -c 1 -s 8952 -I 2001:db8::1 2001:db8::201` and the same with `-s 8953`, reading `Ip6FragCreates` in `/proc/net/snmp6` around each | MTU 9000 on all eight link ends; the 9000-byte probe answered with no fragment created; the 9001-byte probe fragmented at the source |
| A6 | C4 | `show bgp ipv4 unicast neighbors eth1 advertised-routes json` and its `ipv6` twin on `node1`; `show running-config` on `leaf1` | Every advertised key ends in `/32` (IPv4) or `/128` (IPv6); `neighbor nodes maximum-prefix 8192` present |

Each box runs its FRR commands through `vtysh -c '<command> json'`, and
`run.sh` reads the answers with `jq`. Two prefixes are bait. `203.0.113.0/24`
is a connected prefix on `node1`'s loopback that the outbound filter has to
keep on the node. `198.18.0.0/15` is a blackhole route each leaf redistributes
toward the node, which the node's inbound filter has to drop. A4 first checks
that both are in place, because a missing bait proves no filter.

A3 silences the link in both directions with its carrier up: `netem` drops
what `leaf1` sends to the node, and a `tc` ingress filter drops what it
receives. With the egress dropped alone, `leaf1` kept receiving the node's
packets and reported the peer down after about 1750 ms, 900 ms after `node1`
had stopped sending. A host kernel without `sch_netem`, or without the
`sch_ingress`, `cls_matchall` and `act_gact` modules the ingress drop needs,
makes A3 report `SKIP` with the `tc` error, and the run record shows it. A3 is
never passed in that case. Any other `tc` failure, a missing `tc` binary in the
image included, fails A3.

A5 probes over IPv6 because the image's BusyBox `ping` has no `-M do` and IPv6
routers never fragment in transit. The 9000-byte packet (a 40-byte IPv6
header, an 8-byte ICMPv6 header and 8952 bytes of payload) either crosses every
link at MTU 9000 or is answered with Packet Too Big. The 9001-byte probe still
gets its reply, because Linux fragments it at the source, so A5 reads the
fragment counter where a ping exit code would mislead. The cache flush keeps a
path MTU learnt in an earlier run from making the sender fragment the
9000-byte probe.

### Topology

`bgp-fabric.clab.yml` runs four containers of the pinned FRR image: `node1`
dual-homed to `leaf1` and `leaf2`, both leaves under `spine1`, every link at
MTU 9000. The management network on `eth0` belongs to the lab, with subnets
Docker assigns, and `node1` drops Docker's default routes after start, so its
only defaults are the ones the leaves send.

<<< @/../tests/lab/bgp-fabric/bgp-fabric.clab.yml

This is the node side of the contract in FRR syntax:

<<< @/../tests/lab/bgp-fabric/node1/frr.conf

`leaf1/frr.conf` and `leaf2/frr.conf` carry the fabric side: the `nodes`
peer-group with BFD, the `NODES-IN-V4` and `NODES-IN-V6` prefix-lists,
`maximum-prefix 8192` and `default-originate`. They announce their loopbacks
with `network` statements, so the management network is never announced.

### Running the lab

The lab needs a Linux host with Docker, a user in the `docker` group, and
`jq`. It needs no root and no installed containerlab: `run.sh` runs
containerlab from the digest-pinned `ghcr.io/srl-labs/clab` image. containerlab
works in the host's network namespace, so Docker Desktop on macOS cannot run
the lab.

```shell
tests/lab/bgp-fabric/run.sh
```

| Subcommand | Effect |
| --- | --- |
| `up` | `containerlab deploy --reconfigure`, which replaces a lab of the same name |
| `check` | Runs A1 to A6 against the deployed lab, prints one `PASS`, `FAIL` or `SKIP` line per assertion with the measured value and then the run record, and exits 1 on a `FAIL` |
| `down` | `containerlab destroy --cleanup` |
| `all` | `up`, `check`, then `down`, which also runs when `up` or `check` fails or the run is interrupted; the default |

| Variable | Default | Effect |
| --- | --- | --- |
| `KEEP_LAB` | `false` | `true` makes `all` skip `down`, so a failed lab can be inspected |
| `CLAB_IMAGE` | the digest-pinned `ghcr.io/srl-labs/clab` image in `run.sh` | The containerlab image `run.sh` runs |

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
| A1 | An unnumbered node-facing peer-group over IPv6 link-local addresses, both address families, BFD 300 ms × 3 | Both node sessions Established for IPv4 and IPv6, both BFD sessions up |
| A2 | `multipath-relax`, or the vendor's equivalent, on leaves and spines | On a spine, the node's loopback /32 and /128 over both leaves, with two next hops installed |
| A3 | BFD on every node-facing session, as for A1 | One node uplink silenced with its carrier up, for example by an ACL that drops every packet on the port: BFD down within 1000 ms, one path on the spine within 2000 ms, two paths again after the restore |
| A4 | The node-facing inbound prefix-list from the address table, and default origination toward the node | A node prefix outside the table is refused; the node's routing table holds the default from both leaves and no other fabric route |
| A5 | MTU 9000 on every port toward nodes and spines | The vendor's interface command shows 9000 on each port; a 9000-byte IPv6 probe from a node loopback to a spine loopback is answered without fragmentation |
| A6 | `maximum-prefix 8192` on every node session | The running configuration shows the limit; the routes received from a node are /32 and /128 only |

The record below is filled in when the network team runs it.

### Run records

#### containerlab

Run on 2026-10-15 from commit 9396d2e7d with containerlab 0.81.1 and
`quay.io/frrouting/frr@sha256:e995beaa50fdc9edb35eadcfefa29b7f062cc06f2b812613789b68fa541554d2`.

| Assertion | Result | Measured |
| --- | --- | --- |
| A1 | PASS | leaf1, leaf2 and spine1 sessions Established in both families, both node BFD sessions up, after 4 s |
| A2 | PASS | 2 paths for 192.0.2.1/32 and 2001:db8::1/128 on spine1, AS paths 4200001001 4200000001 and 4200001002 4200000001, one bestpath and one multipath each, 2 kernel next hops |
| A3 | PASS | BFD down after 700 ms, spine1 at 1 path after 860 ms, 2 paths again 3 s after the restore |
| A4 | PASS | 203.0.113.0/24 held by node1, not advertised and absent on leaf1, leaf2 and spine1; 198.18.0.0/15 offered by leaf1, absent on node1; 2 paths for 0.0.0.0/0 and for ::/0 on node1 |
| A5 | PASS | 8 link ends at MTU 9000; the 9000-byte probe answered with 0 fragments; the 9001-byte probe cut into 2 fragments at node1 |
| A6 | PASS | node1 advertises only /32 (IPv4) and /128 (IPv6) keys on eth1; leaf1 runs neighbor nodes maximum-prefix 8192 |

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
  `global_physnet_mtu`
- [Neutron Operator](../neutron/index.md) and [OVN Operator](../ovn/index.md)
- [#899](https://github.com/C5C3/cobaltcore/issues/899), the BGP meta, and its
  phases [#1362](https://github.com/C5C3/cobaltcore/issues/1362),
  [#1363](https://github.com/C5C3/cobaltcore/issues/1363),
  [#1364](https://github.com/C5C3/cobaltcore/issues/1364) and
  [#1365](https://github.com/C5C3/cobaltcore/issues/1365)
