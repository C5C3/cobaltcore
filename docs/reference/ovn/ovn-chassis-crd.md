---
title: OVNChassis CRD
quadrant: operator
---

# OVNChassis CRD

`ovnchassis.ovn.openstack.c5c3.io/v1alpha1`, kind `OVNChassis`. The CRD is
generated from `operators/ovn/api/v1alpha1/ovnchassis_types.go`; the defaulting
and validating webhooks live in
`operators/ovn/api/v1alpha1/ovnchassis_webhook.go`.

One CR describes one class of node. It puts the Open vSwitch and
`ovn-controller` DaemonSets onto the nodes `spec.nodeSelector` matches and
registers each of those nodes as a chassis with the `OVNCentral` it names.
Everything below the selector applies uniformly to every node matched: the
bridge mappings, the encapsulation, the rollout pace. A deployment whose gateway
nodes carry different wiring, or an availability zone with its own provider
networks, gets a second CR of its own; widening the selector would apply one
CR's wiring to nodes that do not have it.

The link to the control plane is one field. `spec.centralRef` names an
[OVNCentral](./ovn-central-crd.md) in the same namespace; the chassis wait for
its published Southbound address and for `status.clientSecretName`, and mount
that Secret. A central that projects onto another cluster than the chassis is
reached at the addresses it publishes on node ports, and the chassis mounts a
copy of its Secret, `{name}-ovn-client`, which the operator writes on the
chassis's own cluster. Both refs are immutable, because repointing a live chassis strands
its registration in the old Southbound database.

`kubectl get ovnchassis` prints Ready
(`.status.conditions[?(@.type=='Ready')].status`), Desired
(`.status.desiredNumberScheduled`), Ready pods (`.status.numberReady`), and Age.

## Spec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | no | operator-resolved `ghcr.io/c5c3/ovn:26.03.2` | The image that runs Open vSwitch and `ovn-controller`. Both ship in the one OVN image, so one reference covers them. The operator resolves it at reconcile time, so an unset field keeps tracking its tested version across upgrades (`image.go`). An unset image names no `pullPolicy` of its own, so its containers take the operator default (`--default-image-pull-policy`) or the rule: `Always` for the tag (see [pull policy resolution](../keystone/keystone-crd.md#pull-policy-resolution)) |
| `centralRef` | [`OVNCentralRef`](#ovncentralref) | yes | none | The `OVNCentral` whose Southbound database these chassis connect to and whose client Secret they mount. Immutable, enforced by a CEL transition rule and by the webhook |
| `nodeSelector` | `map[string]string` (MinProperties=1) | yes | none | The nodes both DaemonSets land on. At least one label is required: an empty selector matches every node in the cluster, which would start `ovn-controller` on the control-plane nodes and on whatever joins later |
| `tolerations` | `[]corev1.Toleration` | no | none | Lets the DaemonSet pods run on tainted nodes. Networking nodes are commonly tainted to keep ordinary workloads off them, and the chassis pods are what has to run there |
| `jobs` | [`*commonv1.JobBaseSpec`](../keystone/keystone-crd.md#jobspec) | no | `nil` | Sizes and prioritizes the maintenance Jobs: `apply`, `evacuate` and `chassis-del`, and on a chassis with `fabric` the route probe. It carries `resources` and `priorityClassName` only. Unset resources default to a `70m` CPU request and `368Mi` memory as request and limit, and an unset priority class renders none; there is no fallback Deployment. The Jobs take `spec.tolerations`. The block has no placement fields: the apply Job is pinned to its node, and a node selector that node does not match fails the pinned pod's kubelet admission |
| `gateway` | [`*OVNGatewaySpec`](#ovngatewayspec) | no | `nil` | Marks the subset of the selected nodes that announce `enable-chassis-as-gw`, the flag that makes a chassis eligible to host a distributed router's gateway port. When nil no node in this CR is a gateway |
| `bridgeMappings` | [`[]OVNBridgeMapping`](#ovnbridgemapping) | no | none | Maps each OpenStack physical network onto the local OVS bridge that reaches it. List-map keyed by `physicalNetwork`. Every node this CR selects gets the same mapping |
| `fabric` | [`*OVNFabricSpec`](#ovnfabricspec) | no | `nil` | Attaches the selected nodes to the BGP fabric of the [BGP Fabric Contract](../networking/bgp-fabric-contract.md). Every node with a row in `fabric.nodes` gets the fabric VRF, the fabric loopbacks, one OVS peer bridge per uplink and the port mapping `ovn-controller` reads; every such node labelled `openstack.c5c3.io/frr-k8s=true` also gets an `FRRConfiguration`. The router and port options that make OVN advertise or learn routes are the CMS's to write (see [OVN Dynamic Routing](../networking/ovn-dynamic-routing.md)). When nil no node is attached |
| `encapType` | `string` (Enum `geneve`, `vxlan`) | no | `geneve` | The tunnel protocol between chassis. Geneve carries the variable-length option header OVN uses for its logical metadata; VXLAN has no room for it and so caps the logical topology. VXLAN exists for hardware that cannot terminate Geneve |
| `updateStrategy` | [`OVNChassisUpdateStrategy`](#ovnchassisupdatestrategy) | no | `{}` | Paces the DaemonSet rollout. Restarting `ovn-controller` interrupts the dataplane programming on that node, so the pace is a per-deployment tradeoff |
| `remoteProbeIntervalMs` | `int32` (Minimum=0) | no | `60000` | How long `ovn-controller` lets its Southbound connection sit idle before probing it. Zero disables the probe, which is what a chassis behind a connection-tracking middlebox needs when the probe is what tears the connection down |
| `ovs` | [`*OVNChassisOVSSpec`](#ovnchassisovsspec) | no | `nil` | Tunes the `ovs-vswitchd` container: its resources and its revalidator thread count. When nil the operator renders no requests or limits for the container and pins 2 revalidator threads. The local `ovsdb-server` container beside it takes no resources from any field |
| `controller` | [`*OVNChassisContainerSpec`](#ovnchassiscontainerspec) | no | `nil` | Tunes the `ovn-controller` container |
| `verticalAutoscaling` | [`*VerticalAutoscalingSpec`](../keystone/keystone-crd.md#verticalautoscalingspec) | no | `nil` | Opts both chassis DaemonSets, `{name}-ovs` and `{name}-ovn-controller`, one VPA each, into a VerticalPodAutoscaler that controls the requests of its containers; see [VerticalAutoscalingSpec](../keystone/keystone-crd.md#verticalautoscalingspec). On a cluster without the VPA, `VPAReady` turns False with reason `VPANotInstalled`. |
| `targetClusterRef` | [`*commonv1.TargetClusterRefSpec`](../target-clusters.md#the-field) | no | `nil` (the local cluster) | The registered target cluster the DaemonSets are created on. The CR itself, its status and its finalizer stay on the management cluster. Immutable, enforced by two CEL transition rules and by the webhook. It may name another cluster than the `OVNCentral`'s, which then has to publish both databases outside its cluster (see [Conditions](#conditions)). See [Target Clusters](../target-clusters.md) |

### OVNCentralRef

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` (MinLength=1) | yes | none | The `OVNCentral`'s name. The reference is namespace-local: the chassis mount the client Secret it publishes, and a Secret cannot be mounted across namespaces. Across clusters the chassis mounts the copy in the same namespace on its own cluster |

### OVNGatewaySpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `nodeSelector` | `map[string]string` (MinProperties=1) | yes | none | Picks the gateway nodes out of the set `spec.nodeSelector` already matched, so it can only narrow that set. At least one label is required: an empty selector would promote every selected node to a gateway, spreading external connectivity across nodes with no uplink to carry it |

### OVNBridgeMapping

Ties one OpenStack physical network to one local OVS bridge.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `physicalNetwork` | `string` (Pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, MaxLength=63) | yes | none | The provider-network name Neutron knows the segment by. The grammar is the DNS-1123 label the Neutron network name is bounded to |
| `bridge` | `string` (Pattern `^[a-zA-Z0-9_.-]{1,15}$`) | yes | none | The local OVS bridge name, bounded to 15 characters: the bridge appears as a Linux interface and the kernel's `IFNAMSIZ` leaves 15 usable bytes |

### OVNFabricSpec

Attaches a class of chassis nodes to the BGP fabric. The node network stays
untouched: BGP runs unnumbered over dedicated uplinks inside a VRF of its own,
and the uplinks belong to OVS peer bridges. The clause numbers refer to the
[BGP Fabric Contract](../networking/bgp-fabric-contract.md).

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `vrf` | [`OVNFabricVRFSpec`](#ovnfabricvrfspec) | no | `{}` | The Linux VRF that holds the uplinks, the loopbacks and the BGP sessions (C10), which keeps them out of the main table |
| `uplinks` | [`[]OVNFabricUplink`](#ovnfabricuplink) (MinItems=1, MaxItems=4) | yes | none | The dedicated uplinks, the same on every selected node. List-map keyed by `interface` |
| `mtu` | `int32` (Minimum=1280, Maximum=9216) | no | `9000` | The MTU of every uplink and peer bridge (C6). The leaves set the same MTU on their node ports |
| `nodes` | [`[]OVNFabricNode`](#ovnfabricnode) (MaxItems=1000) | no | none | The per-node table the network team owns: the ASN and the loopbacks of each node. List-map keyed by `name`. A selected node without a row stays unattached, and `FabricReady` names it |
| `announceableRanges` | `[]string` (MaxItems=64, items Format `cidr`) | no | none | The ranges a node may announce host routes from, besides its loopbacks (C4, C8). A route OVN installs outside them never leaves the node, because the node's outbound prefix lists permit nothing else. List-set |
| `advertiseVRFIDs` | `[]int64` (MaxItems=8, items 1 to 4294967295) | no | none | The `dynamic-routing-vrf-id` of every router whose routes the node announces. The fabric router imports each `ovnvrf<id>`. List-set |
| `learnVRFIDs` | `[]int64` (MaxItems=8, items 1 to 4294967295) | no | none | The `dynamic-routing-vrf-id` of every router that learns the leaf defaults. Each `ovnvrf<id>` imports the fabric VRF. List-set |
| `bfd` | [`OVNFabricBFDSpec`](#ovnfabricbfdspec) | no | `{}` | The BFD timers of every fabric session (C5) |

An `OVNChassis` author decides which ranges its nodes may announce. The guard on
the fabric side is the inbound prefix list of the leaf.

### OVNFabricVRFSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` (Pattern `^[a-zA-Z0-9_.-]{1,15}$`) | no | `fabric` | The VRF device, bounded to 15 characters like every interface name |
| `table` | `int64` (Minimum=1, Maximum=4294967295) | no | `100` | The VRF's routing table. Neither one of the kernel's tables nor a listed VRF id, which `ovn-controller` uses as the table of `ovnvrf<id>` |

### OVNFabricUplink

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `interface` | `string` (Pattern `^[a-zA-Z0-9_.-]{1,15}$`) | yes | none | The NIC on the node. It carries link-local addresses only (C2); the node preparation refuses one with a global address |
| `bridge` | `string` (Pattern `^[a-zA-Z0-9_.-]{1,15}$`) | yes | none | The OVS peer bridge that takes the NIC. FRR peers over the bridge's own interface in the fabric VRF, and the bridge name is the port name a router port names in `dynamic-routing-port-name` |

### OVNFabricNode

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` (DNS subdomain, MaxLength=253) | yes | none | The Node's name |
| `asn` | `int64` (Minimum=4200000000, Maximum=4294967294) | yes | none | The node's private 4-byte ASN (C3). The operator uses it in the generated `FRRConfiguration`; on a node with a host FRR it records the value the host stanza carries |
| `loopbackIPv4` | `string` (Format `ipv4`) | yes | none | The /32 fabric loopback, which is also the BGP router id (C2) |
| `loopbackIPv6` | `string` (Format `ipv6`) | yes | none | The /128 fabric loopback |

### OVNFabricBFDSpec

The bounds are frr-k8s's own.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `receiveIntervalMs` | `int32` (Minimum=10, Maximum=60000) | no | `300` | The minimum interval at which the node accepts BFD control packets |
| `transmitIntervalMs` | `int32` (Minimum=10, Maximum=60000) | no | `300` | The minimum interval at which the node sends them |
| `detectMultiplier` | `int32` (Minimum=2, Maximum=255) | no | `3` | How many packets may be lost before the session is declared down |

### OVNChassisUpdateStrategy

A narrowed `DaemonSetUpdateStrategy`. There is no `maxSurge` counterpart, because
a second `ovn-controller` on the same node would contend with the first one over
the local Open vSwitch database.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `type` | `string` (Enum `RollingUpdate`, `OnDelete`) | no | `RollingUpdate` | The rollout mode. `OnDelete` hands the pace to whoever drains the nodes, which is what a deployment with an external maintenance workflow wants |
| `maxUnavailable` | `*intstr.IntOrString` | no | operator-resolved `1` | How many selected nodes may lose their dataplane programming at once. It applies to `RollingUpdate` only, and the webhook rejects it alongside `OnDelete` so it cannot read as effective |

### OVNChassisOVSSpec

The `ovs` block carries the fields of
[`OVNChassisContainerSpec`](#ovnchassiscontainerspec) inline, so
`spec.ovs.resources` keeps its place, and adds one field.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `resources` | `*corev1.ResourceRequirements` | no | none | As in [`OVNChassisContainerSpec`](#ovnchassiscontainerspec), for `ovs-vswitchd` and `host-prepare` |
| `revalidatorThreads` | `*int32` (Minimum=1) | no | operator-resolved `2` | The number of revalidator threads `ovs-vswitchd` runs, the threads that dump the datapath flows and expire or revalidate them. The operator resolves the default at render time and never writes it into the CR |

The count reaches the `ovs-vswitchd` container as the environment variable
`OVS_REVALIDATOR_THREADS`, and `run-vswitchd.sh` writes it as
`other_config:n-revalidator-threads` into the node's `Open_vSwitch` table before
it starts the daemon. A pod created before the operator upgrade has no such
variable, but the kubelet still refreshes the script inside it. When that pod's
`ovs-vswitchd` container restarts, the script skips the write and starts the
daemon with the OVS default count, which lasts until the DaemonSet replaces the
pod.

Unpinned, OVS 3.7 starts the handler count divided by four, plus one,
revalidators. The handler count follows the CPU cores in the process's CPU
affinity, so the default would be 2 revalidators on a 4-CPU node and 5 on a
16-CPU node, and a chassis pod's footprint would depend on the node it lands on.
The handler threads cannot be pinned the same way. Under the kernel datapath's
per-CPU upcall dispatch, which Linux 5.14 and later provide, OVS starts one
handler per core in its CPU affinity and overrides
`other_config:n-handler-threads`. A CPU limit does not change that affinity;
only a cpuset bounds the handlers.

`ovs-vswitchd` logs the applied counts at INFO when it starts its threads:

```text
Overriding n-handler-threads to <n>, setting n-revalidator-threads to <m>
```

`kubectl logs <pod> -c ovs-vswitchd` shows the line, and
`kubectl exec <pod> -c ovs-vswitchd -- ovs-vsctl get open . other_config:n-revalidator-threads`
prints the pinned value. The value lives in the node's `conf.db`, which outlives
the pod. Every start of `ovs-vswitchd` writes it again, so changing
`revalidatorThreads` takes effect when the DaemonSet rolls the pod. An operator
downgrade to a release without the field leaves the last value in place; remove
it with `ovs-vsctl remove open . other_config n-revalidator-threads`.

### OVNChassisContainerSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `resources` | `*corev1.ResourceRequirements` | no | none | Requests and limits for the container and for the init container that prepares it: `host-prepare` in the OVS DaemonSet for `spec.ovs`, `apply-node` in the controller DaemonSet for `spec.controller`. An init container never runs beside the main containers, so this adds nothing to the pod's footprint. When nil the operator renders none on either, unless a LimitRange in the namespace fills them in: what a datapath needs depends on the traffic the node carries, so no default fits most hardware |

## Defaulting and validation

The mutating webhook leaves the object untouched. Every default is either a
`+kubebuilder:default` the API server applies from the CRD schema, or a value the
operator resolves at reconcile time: the image and the `maxUnavailable` of 1.
Resolving those two late keeps an unset field tracking the operator default
across upgrades instead of freezing today's value into the stored CR. The webhook
stays registered so a default that has to be materialized later can be added
without changing the deployed webhook configuration.

### Schema-layer rules

These hold even when the webhook is down.

| Message | Where it comes from |
| --- | --- |
| `targetClusterRef is immutable` | Two CEL transition rules on `OVNChassisSpec`, one for adding or removing the ref and one for renaming it |
| `centralRef is immutable` | A third transition rule on `OVNChassisSpec`. Repointing a live chassis leaves its registration behind in the old Southbound database, where it keeps claiming the ports of workloads that have moved on |
| `exactly one of image.tag or image.digest must be set` | Inherited from `commonv1.ImageSpec` on `spec.image` |

The three transition rules are evaluated on UPDATE only. Beside them the schema
carries the ordinary field markers: `MinProperties=1` on both selectors, the
`geneve`/`vxlan` enum on `spec.encapType`, the `RollingUpdate`/`OnDelete` enum on
`spec.updateStrategy.type`, `Minimum=0` on `spec.remoteProbeIntervalMs`,
`Minimum=1` on `spec.ovs.revalidatorThreads`, the two
patterns on a bridge mapping, and `MinLength=1` on `spec.centralRef.name`.
`spec.bridgeMappings` is a list-map keyed by `physicalNetwork`, so the API server
already refuses a repeated physical network. Under `spec.fabric` the schema
carries the bounds of the type tables above: the uplink count, the name
patterns, the table, MTU, ASN, VRF id and BFD ranges, the `ipv4`, `ipv6` and
`cidr` formats, and the list-map keys `interface` on the uplinks and `name` on
the node table. These markers have no webhook twin: the API server enforces them
whether or not the webhook runs.

### Webhook rules

The validating webhook accumulates every violation into one admission response.

| Message | Trigger |
| --- | --- |
| `name must be at most %d characters: the per-node Jobs append up to 21 characters and Kubernetes caps object names at 63` | `metadata.name` longer than `MaxOVNChassisNameLength`. The argument is the bound, 42 |
| `centralRef.name must be set` | `spec.centralRef.name` is empty |
| `centralRef is immutable` | An update renames `spec.centralRef.name`, the webhook-layer twin of the CEL rule |
| `nodeSelector must carry at least one label` | `spec.nodeSelector` is empty or absent, mirroring the `MinProperties=1` marker |
| `gateway.nodeSelector must carry at least one label` | `spec.gateway` is set with an empty selector, mirroring the second `MinProperties=1` marker |
| `maxUnavailable applies to RollingUpdate only` | `spec.updateStrategy.maxUnavailable` is set alongside `type: OnDelete`, where nothing reads it |
| `maxUnavailable must be an integer or a percentage such as "25%"` | The value is neither, so `intstr.GetScaledValueFromIntOrPercent` refuses it |
| `maxUnavailable must resolve to at least 1 for RollingUpdate` | The value scales to zero, which is a rollout that can never move a node |
| `Duplicate value` on `spec.bridgeMappings[i].bridge` | Two mappings name the same local bridge. The rendered `ovn-bridge-mappings` string would have its second entry shadow the first, and the list-map key covers the physical-network side alone |
| `Duplicate value` on `spec.bridgeMappings[i].physicalNetwork` | Two mappings name the same physical network. The `+listMapKey` already refuses this at the schema layer; the webhook repeats it so the invariant holds where schema validation is bypassed |
| `repository must be set` | `spec.image.repository` is empty |
| `exactly one of image.tag or image.digest must be set` | Both or neither are set on `spec.image`, re-checked outside the schema |
| `target cluster name must be set` | `spec.targetClusterRef` is present with an empty `name` |
| `targetClusterRef is immutable (adding or removing it after creation is not permitted)` | An update adds or drops the ref. Both strand the children already created on the previously selected cluster |
| `targetClusterRef is immutable (the children already exist on the previously named cluster)` | An update renames the ref |
| `%s request must not exceed limit (%s)` | A request in `spec.jobs.resources` above its own limit |
| `field.NotFound` on `spec.jobs.priorityClassName` | The named PriorityClass does not exist. Skipped when no lookup client is injected, and for `""` |
| `revalidatorThreads must be at least 1` | `spec.ovs.revalidatorThreads` is set below 1, mirroring the `Minimum=1` marker |
| `Duplicate value` on `spec.fabric.uplinks[i].bridge` | Two uplinks name one peer bridge. The list-map key covers the interface side alone |
| `a peer bridge cannot carry the name of an uplink interface` | A bridge carries the name of an uplink interface, which the bridge would take in the kernel |
| `br-int is the integration bridge of ovn-controller` | A bridge is named `br-int` |
| `the name is taken by the fabric VRF device` | A bridge or an interface carries `spec.fabric.vrf.name`; the error names that field |
| `the prefix ovnvrf is reserved for the VRFs ovn-controller maintains` | `spec.fabric.vrf.name` starts with `ovnvrf` |
| `tables 253, 254 and 255 are the kernel's default, main and local tables` | `spec.fabric.vrf.table` is one of them |
| `ovn-controller uses a VRF id as the table of ovnvrf<id>` | `spec.fabric.vrf.table` is a listed VRF id |
| `a VRF id is either advertised from or learned into, not both` | An id is in both `advertiseVRFIDs` and `learnVRFIDs`; the error is on the `learnVRFIDs` entry. Importing in both directions between one `ovnvrf<id>` and the fabric VRF is untested |
| `Duplicate value` on `spec.fabric.nodes[i].asn` | Two rows share an ASN. A BGP speaker drops a route whose AS path carries its own ASN, so the two nodes would drop each other's routes |
| `Duplicate value` on `spec.fabric.nodes[i].loopbackIPv4` or `loopbackIPv6` | Two rows share a loopback, compared as parsed addresses, so `2001:db8::1` and `2001:DB8:0::1` are one |
| `must be a network address; did you mean <masked prefix>` | An announceable range has host bits set. The `FRRConfiguration` prints every range masked |
| `advertiseVRFIDs needs at least one announceable range` | `advertiseVRFIDs` is set and `announceableRanges` is empty, so the outbound lists would let no OVN route through |

The fabric rules parse loopbacks with `netip.ParseAddr` and ranges with
`netip.ParsePrefix`, and skip a value that does not parse: the schema format has
rejected it already.

The percentage in `maxUnavailable` is scaled against 100, not against the node
count, and rounded up the way the DaemonSet controller rounds it. The nodes a
DaemonSet lands on are unknown at admission time, and a percentage that rounds to
zero on a smaller cluster wedges the rollout the same way. `1%` is therefore
admitted and behaves as one node.

`MaxOVNChassisNameLength` is 42. The child with the tightest name budget is the
per-node chassis-deletion Job: `{name}-chassis-del-{8 hex}` adds 21 characters,
and Kubernetes caps an object name at 63. The bound is enforced on create only.
`metadata.name` is immutable, so on update the rule could only fire against an
object a pre-upgrade operator already admitted, and the validating webhook also
sees the finalizer-removal update that completes a deletion: rejecting that one
would wedge the CR in `Terminating` with no field left to edit.

One cross-CR constraint is outside the webhook's reach. `spec.centralRef` may
name an `OVNCentral` that does not exist at admission time, so the controller
checks it. When the two CRs project onto different clusters, the central has to
set `spec.northbound.externallyReachable` and `spec.southbound.externallyReachable`:
the chassis dials the Southbound database, and the gateway-evacuation Job writes
the Northbound one. A central that publishes either one alone leaves the chassis
at `CentralReady=False` with reason `CentralNotExternallyReachable`, and the
message names both fields.

## Status

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | List-map keyed by `type`; see [Conditions](#conditions) |
| `observedGeneration` | `int64` | The `.metadata.generation` the controller last reconciled |
| `installedImage` | `string` | The image reference the running DaemonSets were projected from, recorded once the `ovn-controller` DaemonSet reports every node ready. It tells a rollout that has not reached the nodes from one that has |
| `clientSecretName` | `string` | The Secret in this CR's namespace, on the cluster this CR projects onto, that the chassis pods mount: the `OVNCentral`'s `status.clientSecretName` when both CRs project onto the same cluster, the copy `{name}-ovn-client` when they do not. A `NeutronMetadataAgent` attached to this chassis mounts the same Secret |
| `desiredNumberScheduled` | `int32` | How many nodes the DaemonSets should run on, mirrored from the `ovn-controller` DaemonSet |
| `numberReady` | `int32` | How many of those nodes have a ready `ovn-controller` pod |
| `nodes` | [`[]OVNChassisNodeStatus`](#ovnchassisnodestatus) | The per-node registration state, list-map keyed by `name` |

The two counters are read from the `ovn-controller` DaemonSet, not from the Open
vSwitch one: `ovn-controller` is what makes a node a chassis, and a node running
Open vSwitch alone carries no logical flows.

### OVNChassisNodeStatus

`status.nodes` is what the controller works from; it re-derives nothing from the
Southbound database on a normal pass. A node that stops being selected disappears
from the node list while its chassis registration survives, so the record of it
has to outlive the selection.

| Field | Type | Description |
| --- | --- | --- |
| `name` | `string` | The node's name. Also the key of that node's entry in the `{name}-nodes` ConfigMap and the file the pod on it reads |
| `systemID` | `string` | The chassis identity `ovn-controller` registered in the Southbound database, a UUID. A chassis-deletion Job addresses it, so it has to be recorded before the node goes away |
| `gateway` | `bool` | Whether the node currently announces `enable-chassis-as-gw`. It reports what the node last applied, so the flip to `false` is held back until the evacuation has landed |
| `configHash` | `string` | Eight hex characters of the SHA-256 of the entry the node last applied. Comparing it against the rendered entry tells a configuration change from a node that has not reported back yet |
| `gatewayEvacuated` | `bool` | The evacuation Job moved the gateway ports off this node and succeeded. It resets when the node takes the gateway role back, so a re-promoted node is not treated as still drained |
| `fabric` | `bool` | The node may carry the devices of a fabric assignment. Set from the first pass that renders an assignment for the node, because its `fabric-node` init container can apply the entry before any `apply` Job runs, and cleared only by the `apply` Job that tore the devices down. While it is set and the entry carries no assignment, the node gets that Job |
| `leaving` | `bool` | The node is no longer selected, or no longer in the cluster, and its chassis registration has still to be deleted. Until that deletion lands the stale chassis keeps claiming the ports of workloads that have moved elsewhere |

### Conditions

Eight sub-reconcilers set eight condition types: the client-Secret step reports
under `CentralReady`, the condition of the central step before it, and the
Fabric step also sets `RoutesAdvertised`, which is no sub-condition. The
aggregate `Ready` is `True` only when the other seven are. For the pipeline that
sets them see [Reconciler Architecture](./ovn-reconciler.md).

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `CentralReady` | True | `CentralResolved` | The `OVNCentral` published the addresses that apply and its client Secret, and the Secret the pods mount is in place. The message names the address the chassis dial: the relay or the Southbound database inside one cluster, the relay's or the Southbound database's node address across clusters |
| `CentralReady` | False | `CentralNotFound` | No `OVNCentral` of that name in the namespace. An `OVNChassis` applied before its `OVNCentral` is an ordinary ordering of two objects in one manifest, so this polls |
| `CentralReady` | False | `CentralReadError` | Reading the `OVNCentral` failed |
| `CentralReady` | False | `CentralNotExternallyReachable` | The two CRs project onto different clusters and the central does not set both `spec.northbound.externallyReachable` and `spec.southbound.externallyReachable`. The message names both clusters and both fields. No requeue: the fix is an edit to the central, which the central watch delivers |
| `CentralReady` | False | `CentralNotReady` | The `OVNCentral` has not published the Southbound address that applies (outside its cluster, across a boundary) or its client Secret yet, or it publishes its relay outside its cluster and has no relay node address yet |
| `CentralReady` | False | `ClientSecretPending` | The central's client Secret does not exist yet on the central's cluster, so there is nothing to copy. Polls |
| `CentralReady` | False | `ClientSecretIncomplete` | The central's client Secret lacks `tls.crt`, `tls.key` or `ca.crt`, or carries one empty. The message names the key. Polls |
| `CentralReady` | False | `ClientSecretReadError` | Reading the central's client Secret failed |
| `CentralReady` | False | `ClientSecretCopyFailed` | Writing the copy `{name}-ovn-client` failed, or a Secret of that name exists that this chassis does not own, which the operator refuses to overwrite |
| `CentralReady` | False | `CentralUpgrading` | The central's `status.installedImage` differs from the image it resolves, so its own rollout is still in flight. OVN's supported upgrade order is central first, hypervisors second: `ovn-controller` reads the Southbound schema the central owns |
| `CentralReady` | False | `TargetClusterUnavailable` | `spec.targetClusterRef` names a cluster that does not resolve, or the cluster the central projects onto does not resolve when the client Secret has to be copied from it. This is the pipeline's first gate, so the failure lands on the condition the rest of the graph waits behind |
| `NodesReady` | True | `NodesRendered` | Every selected node has an entry in the `{name}-nodes` ConfigMap. The message counts the entries and how many of them are leaving |
| `NodesReady` | False | `NoMatchingNodes` | No node carries `spec.nodeSelector`. The message repeats the selector. Both ConfigMaps are still applied, because a pod whose ConfigMap volume does not exist never starts |
| `NodesReady` | False | `NodeListError` | Listing the nodes of the target cluster failed |
| `NodesReady` | False | `NodesError` | Reading the live nodes ConfigMap or applying one of the two ConfigMaps failed |
| `OVSReady`, `ControllerReady` | True | `DaemonSetReady` | That DaemonSet runs a ready pod on every node it selects. A DaemonSet selecting no node is ready on zero nodes: its rollout has nothing left to do |
| `OVSReady`, `ControllerReady` | False | `DaemonSetProgressing` | Fewer nodes run a ready pod than the DaemonSet schedules. The message counts both |
| `OVSReady`, `ControllerReady` | False | `DaemonSetError` | The DaemonSet could not be applied or read |
| `MaintenanceReady` | True | `MaintenanceIdle` | No node has maintenance outstanding |
| `MaintenanceReady` | True | `MaintenanceRunning` | Per-node Jobs are in flight. The message names the nodes. It stays True: a Job that runs finishes on its own, and holding the aggregate `Ready` down for it would report a rolling drain as an outage |
| `MaintenanceReady` | True | `MaintenanceDeferred` | A node needs an address the `OVNCentral` has not published. It outranks `MaintenanceRunning` in the message, because this one waits on another CR |
| `MaintenanceReady` | False | `MaintenanceJobFailed` | A per-node Job reached a terminal failure. The message names the kind, the Job and the node. No error is returned: a rerun under an unchanged key produces the same failure |
| `MaintenanceReady` | False | `MaintenanceError` | Running a Job, or dropping a deregistered node from the ConfigMap, failed |
| `FabricReady` | True | `FabricConfigured` | Every selected node has a fabric assignment, and every one of them labelled `openstack.c5c3.io/frr-k8s=true` carries its `FRRConfiguration`. The message counts both |
| `FabricReady` | True | `FabricNotConfigured` | `spec.fabric` is not set. Any `FRRConfiguration` the CR wrote before is deleted and the fabric finalizer released |
| `FabricReady` | False | `NodeWithoutFabricEntry` | `spec.fabric` is set and a selected node has no usable row in `spec.fabric.nodes`. The message names the nodes, sorted |
| `FabricReady` | False | `FRRK8sNotInstalled` | A fabric node carries the frr-k8s label, but its cluster does not serve `frrk8s.metallb.io/v1beta1` `FRRConfiguration`. The message names the nodes. Nothing is written |
| `FabricReady` | False | `CapabilityProbeFailed` | The cluster could not be probed for the kind; the error is returned |
| `FabricReady` | False | `FabricError` | Listing, applying or deleting an `FRRConfiguration`, or writing the fabric finalizer, failed; the error is returned. Also an `FRRConfiguration` of a wanted name that this CR does not own, which is left untouched and named |
| `RoutesAdvertised` | True | `RoutesAdvertised` | The route probe reports advertised routes. Message: "The Southbound database of OVNCentral \<name\> holds \<a\> advertised and \<l\> learned routes" |
| `RoutesAdvertised` | False | `NoAdvertisedRoutes` | The route probe reports none; same message |
| `RoutesAdvertised` | False | `RouteProbeFailed` | The probe Job failed, or running it or reading its report failed; the message carries the error |
| `RoutesAdvertised` | Unknown | `RouteProbeUnreadable` | The Job's termination message is not `advertised=<n> learned=<n>` |
| `RoutesAdvertised` | Unknown | `RouteProbeDeferred` | The `OVNCentral` has not published its Southbound address; no Job runs |
| `RoutesAdvertised` | Unknown | `RouteProbePending` | No probe Job has finished yet |
| `Ready` | True | `AllReady` | All seven sub-conditions are True |
| `Ready` | False | `NotAllReady` | At least one is not |
| `VPAReady` | True | `VPAReady` | The VerticalPodAutoscaler of every opted-in workload is applied; the message names them. See [VerticalAutoscalingSpec](../keystone/keystone-crd.md#vpaready-condition) |
| `VPAReady` | True | `VPANotRequired` | No workload opts in (`spec.verticalAutoscaling` unset); a VPA the CR created before is deleted |
| `VPAReady` | False | `VPANotInstalled` | A workload opts in, but the cluster the children land on does not serve `autoscaling.k8s.io/v1` `VerticalPodAutoscaler`. Nothing is created, and the other conditions still converge |
| `VPAReady` | False | `CapabilityProbeFailed` | The target cluster the CR names could not be probed for the kind |
| `VPAReady` | False | `VPAError` | Listing, applying or deleting a VPA failed; the message carries the error |

`RoutesAdvertised` is set only while `spec.fabric` is set and is no
sub-condition of `Ready`: a fabric without a floating IP advertises nothing and
is still ready. The probe Job behind it runs once per 5-minute window, so the
condition is up to 5 minutes old, and the counts are those of the whole
Southbound database, which carries no chassis column. While a Job runs, the
condition keeps its previous value.

## Sub-Resource Naming Convention

Every child takes the CR name plus a component suffix. For an `OVNChassis` named
`{name}`:

| Resource | Name | Notes |
| --- | --- | --- |
| DaemonSet | `{name}-ovs` | `ovsdb-server` and `ovs-vswitchd` on the selected nodes, in the node's network namespace |
| DaemonSet | `{name}-ovn-controller` | `ovn-controller`, connected to the Southbound address the central published |
| ConfigMap | `{name}-nodes` | One key per node this CR is responsible for, carrying that node's values. One object serves every node: the pod on a node reads the file named after it and ignores the rest |
| ConfigMap | `{name}-chassis-scripts` | The scripts both DaemonSets and the four Jobs run. The suffix names the kind, so a chassis and a central of the same name keep separate scripts ConfigMaps in one namespace |
| Secret | `{name}-ovn-client` | Only while the central projects onto another cluster. The copy of the central's client identity (`tls.crt`, `tls.key`, `ca.crt`) the pods mount, kept equal to the source across renewals. Swept with the DaemonSets on a placed chassis, garbage-collected through its controller reference on a local one |
| Job | `{name}-apply-<8 hex>` | Re-applies one node's values after they changed. Pinned to that node and in its network namespace |
| Job | `{name}-evacuate-<8 hex>` | Moves the gateway duties off one node, against the Northbound database |
| Job | `{name}-chassis-del-<8 hex>` | Deletes one node's Southbound `Chassis` row, against the Southbound database |
| Job | `{name}-route-probe` | Only while `spec.fabric` is set. Counts the rows of the Southbound `Advertised_Route` and `Learned_Route` tables once per 5-minute window and reports them in its termination message for `RoutesAdvertised` |
| FRRConfiguration | `{name}-fabric-<8 hex>` | Only for a fabric node labelled `openstack.c5c3.io/frr-k8s=true`. Lives in `frr-k8s-system`, the namespace frr-k8s reads, so it carries the ownership labels instead of an owner reference, and the finalizer `ovn.openstack.c5c3.io/fabric-frrconfigurations` holds the CR until it is deleted. The annotation `ovn.openstack.c5c3.io/node` names its node. See [The FRRConfiguration](#the-frrconfiguration) |

The eight hex characters are the first four bytes of the SHA-256 of the node
name. A node name is a DNS subdomain of up to 253 characters and a Job name has
63, so the name is hashed into the suffix; four bytes tell any two node names of
one cluster apart. Each Job runs with `backoffLimit: 0`, an active
deadline of 300 seconds and a TTL of 86400 seconds after it finishes.

The longest of the three suffixes is what bounds `metadata.name` at 42
characters. See [Webhook rules](#webhook-rules).

## Node contract

`spec.nodeSelector` is required and is copied onto both DaemonSets, so the pods
land where the CR says and nowhere else. `spec.gateway.nodeSelector` is evaluated
on top of it: a node is a gateway when it matches both. A nil `spec.gateway`
matches no node.

The figure shows the two chassis pods on a node, the pods of a metadata agent
and of a compute pool that wait for them, and what a gateway node adds.

![One hypervisor node with the four pods three resources put on it. An OVNChassis creates two DaemonSets: the pod {chassis}-ovs with the init container host-prepare and the containers ovsdb-server and ovs-vswitchd, and the pod {chassis}-ovn-controller with the init container apply-node and the container ovn-controller. A NeutronMetadataAgent creates the pod {agent}-metadata-agent, a NovaCompute the pod {pool}-nova-compute. All four run in the network namespace of the node, beside a libvirtd that no operator runs. Three gates order the start. The operator creates the ovn-controller DaemonSet only once every OVS pod is Ready. The init container wait-for-chassis of nova-compute waits until apply-node has written a system-id into the local Open vSwitch database. The init container wait-for-chassis of the metadata agent waits until ovn-controller has registered the chassis in the Southbound database. The pods share host paths: /run/openvswitch is mounted by all four, /run/ovn by the two chassis pods, /run/netns by the metadata agent, /run/libvirt and /var/lib/nova by nova-compute and libvirtd. A gateway node has no further pod: it also matches spec.gateway.nodeSelector, and apply-node sets ovn-cms-options=enable-chassis-as-gw.](../../diagrams/compute-node-anatomy.svg)

The operator never writes a Node. Its RBAC on `nodes` is `get`, `list` and
`watch`, and the labels a node carries are an input. Whoever provisions the
cluster owns them.

Both DaemonSets run with `hostNetwork: true`. The tunnels between chassis
terminate on the node's own address, and the bridges the datapath attaches to are
the node's interfaces. Three host paths are mounted:

| Path | Mounted by | Why it is a host path |
| --- | --- | --- |
| `/run/openvswitch` | both DaemonSets, the `apply` Job | The local Open vSwitch database, the sockets the containers reach each other through, and the pid files. The kernel datapath outlives the pod: a restarted pod that found an empty directory would build a second database and leave the flows of the first one behind |
| `/run/ovn` | both DaemonSets | `ovn-controller`'s socket and pid file, for the same reason |
| `/lib/modules` | the `ovs` DaemonSet, read-only | The node's kernel module tree, which the init container loads the datapath modules from. It is mounted as it is, with no `DirectoryOrCreate`, so a node that has no module tree fails on the mount instead of on a `modprobe` |

Without `spec.fabric`, the one privileged container is the `host-prepare` init
of the `{name}-ovs` DaemonSet. It runs `modprobe openvswitch && modprobe geneve`, creates the two run
directories group-writable under uid and gid 42424, creates the local database
when the node carries none, and converts an existing one to the schema of the
image that is now running. Everything else runs unprivileged or with two named
capabilities: `NET_ADMIN` and `SYS_NICE` on `ovs-vswitchd`, `NET_ADMIN` on
`ovn-controller`. `SYS_NICE` is what lets `ovs-vswitchd` raise the priority of
its own polling threads; without it every start logs the failure and the daemon
runs at ordinary priority. A chassis with `spec.fabric` adds the `fabric-node`
container described below, privileged and as uid 0.

The `apply-node` init container of the `{name}-ovn-controller` pod is what writes
the node's identity into the local database, and the `apply` Job runs the same
script. It waits for its own key in the `{name}-nodes` ConfigMap, then sets these
`external_ids` on the `Open_vSwitch` table:

| Key | Value |
| --- | --- |
| `system-id` | The UUID recorded as `status.nodes[].systemID` |
| `hostname` | The node name, from the downward API |
| `ovn-encap-type` | `spec.encapType` |
| `ovn-encap-ip` | The node's own address, from the downward API |
| `ovn-remote` | The Southbound relay when the central runs one the chassis can reach: on another cluster, only a relay published with `spec.relay.externallyReachable`. The Southbound database itself otherwise |
| `ovn-remote-probe-interval` | `spec.remoteProbeIntervalMs` |
| `ovn-bridge-mappings` | The `physnet:bridge` list rendered from `spec.bridgeMappings`, in spec order. Removed when there are no mappings, never set to the empty string: `ovs-vsctl` refuses a value-less assignment and would fail the init container under `set -eu` |
| `ovn-cms-options` | `enable-chassis-as-gw` on a gateway node, removed on every other one |

Every mapped bridge is then created with `ovs-vsctl --may-exist add-br`.
`ovn-controller` attaches patch ports to a bridge but never creates one, so a
mapping pointing at a bridge that does not exist would silently drop every packet
on that physical network.

The `ovs-vswitchd` container writes one `other_config` key on the same table
itself, before its daemon starts: `n-revalidator-threads`, from
[`spec.ovs.revalidatorThreads`](#ovnchassisovsspec).

### Fabric devices

A chassis with `spec.fabric` runs a second init container, `fabric-node`, after
`apply-node` in the `{name}-ovn-controller` pod, privileged and as uid 0 with the
resources of `spec.controller`. Its script, `fabric-node.sh`, reads the six
`FABRIC_` keys of the node's entry, which only a node with a usable row in
`spec.fabric.nodes` carries. The `apply` Job of such a node runs `apply-node.sh`
in an init container and `fabric-node.sh` in its privileged main container. A
chassis without the block renders its DaemonSet and its Jobs as before, so an
operator upgrade restarts no `ovn-controller`.

Before its first change the script refuses two uplinks and exits 1: one that does
not exist (`ERROR: fabric uplink <nic> does not exist on <node>`), and one that
carries a global address (`ERROR: fabric uplink <nic> carries a global address on
<node>; a fabric uplink carries link-local addresses only`). The second guard
keeps a mistyped interface name from pulling the node network's port into a
bridge. It then creates or keeps:

| Device or key | What the script sets |
| --- | --- |
| The VRF `spec.fabric.vrf.name` | Table `spec.fabric.vrf.table`, up, the two loopbacks as /32 and /128 and no other global address. A VRF on another table is re-created. A kernel without the `vrf` module fails the run with `ERROR: cannot create the VRF device <name> on <node>; the node kernel has no vrf module` |
| One OVS bridge per uplink | Marked `external_ids:c5c3-fabric=true`, with the NIC as its port, marked the same way, and `mtu_request` set to `spec.fabric.mtu`; its interface enslaved to the VRF and up. The script waits up to 30 seconds for the bridge device and fails with `ERROR: peer bridge <bridge> did not appear on <node>` after that |
| The NIC | `accept_ra` 0, IPv6 off (`disable_ipv6` 1), MTU `spec.fabric.mtu`, up. As an OVS port it carries no address, so no link-local address or `fe80::/64` route of its own lands in the node's main table. The bridge gets `accept_ra` 0 too and keeps IPv6 for the session, both before either comes up |
| `external_ids:c5c3-fabric-vrf` | The VRF the script created, on the `Open_vSwitch` row |
| `external_ids:dynamic-routing-port-mapping` | `<bridge>=<bridge>` per uplink, the mapping `ovn-controller` reads for `dynamic-routing-port-name` |

The markers are how a later run removes its own devices and nothing else:
marked bridges the entry no longer names, marked ports the entry no longer
pairs with their bridge, so an uplink can move to another NIC or two bridges can
swap their NICs, and the recorded VRF when the entry names another one or none.
An entry without `FABRIC_` keys is the teardown: the marked bridges, the
recorded VRF and the port-mapping key go, the NICs stay. The init container and
an `apply` Job can run the script on one node at the same moment, so every step
either sets a state or accepts that the other run was first.

Removing a node's row, or the whole block, changes the node's entry, and the
`apply` Job that follows runs the teardown, because `status.nodes[].fabric`
records that the node may carry fabric devices. The flag goes up with the
first entry that carries an assignment, so a row removed before its `apply` Job
succeeded, whose entry hashes as the recorded one again, still gets the
teardown Job. A node that leaves the
selector, and every node of a deleted CR, keeps its devices until it reboots: no
pod of the chassis runs there any more. Remove the node's row or the block first,
wait for `status.nodes[].fabric` to turn false, and then deselect the node or
delete the CR.

### The FRRConfiguration

For a fabric node labelled `openstack.c5c3.io/frr-k8s=true`, the Fabric step
writes `{name}-fabric-<8 hex>` into `frr-k8s-system`, selected to the node by
its `kubernetes.io/hostname` label. A node with a host FRR never carries the
label and gets no object; its stanza is written by hand as
[BGP Node Substrate](../networking/bgp-node-substrate.md) describes. For a node
with ASN 4200000001, the loopbacks `192.0.2.1` and `2001:db8::1`, the uplinks of
the example below and its VRF ids and ranges, the spec is:

```yaml
spec:
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: node-a
  bgp:
    bfdProfiles:
    - name: fabric
      receiveInterval: 300
      transmitInterval: 300
      detectMultiplier: 3
    routers:
    - asn: 4200000001
      id: 192.0.2.1
      vrf: fabric
      prefixes: [192.0.2.1/32, 2001:db8::1/128]
      imports:
      - vrf: ovnvrf42
      neighbors:
      - interface: br-fab1
        dynamicASN: external
        bfdProfile: fabric
        toAdvertise:
          allowed:
            mode: all
        toReceive:
          allowed:
            mode: filtered
            prefixes:
            - prefix: 0.0.0.0/0
            - prefix: ::/0
      # the same neighbor for br-fab2
    - asn: 4200000001
      vrf: ovnvrf42
    - asn: 4200000001
      vrf: ovnvrf10
      imports:
      - vrf: fabric
  raw:
    priority: 10
    rawConfig: |
      router bgp 4200000001 vrf ovnvrf42
       address-family ipv4 unicast
        redistribute kernel
       exit-address-family
       address-family ipv6 unicast
        redistribute kernel
       exit-address-family
      exit
      ip prefix-list br-fab1-fabric-allowed-ipv4 seq 100 permit 198.51.100.0/24 ge 32
      ip prefix-list br-fab2-fabric-allowed-ipv4 seq 100 permit 198.51.100.0/24 ge 32
      ipv6 prefix-list br-fab1-fabric-allowed-ipv6 seq 100 permit 2001:db8::/64 ge 128
      ipv6 prefix-list br-fab2-fabric-allowed-ipv6 seq 100 permit 2001:db8::/64 ge 128
```

There is one import, one router and one `redistribute kernel` block per
advertised VRF id, and one router importing the fabric VRF per learned id. The
n-th range of a family takes `seq 100+n-1` in every list of that family, under
the list names of frr-k8s v0.0.27. Without an advertised id the object has no
`raw` block. Every value of the raw block is an integer, a pattern-bound name or
a prefix the operator parsed and printed again.

The operator applies the object only when the live one does not already contain
it, so a steady pass costs one list, and a field edited by hand is written back
on the next pass. Turning the frr-k8s label off a node deletes its object. On a
target cluster the operator's account needs the opt-in grant of the
target-cluster access chart (see
[Target Clusters](../target-clusters.md)).

### Label conventions

The CRD fixes no label keys. The suites and the guides use
`openstack.c5c3.io/chassis=true` for the nodes an `OVNChassis` selects and
`openstack.c5c3.io/gateway=true` for the gateway subset, and following that
convention keeps a deployment readable next to the e2e fixtures.

## Example

```yaml
apiVersion: ovn.openstack.c5c3.io/v1alpha1
kind: OVNChassis
metadata:
  name: ovn-chassis
  namespace: openstack
spec:
  centralRef:
    name: ovn-chassis-central
  nodeSelector:
    openstack.c5c3.io/chassis: "true"
```

This is the fixture the `chassis-single-node` suite applies
(`tests/e2e/ovn/chassis-single-node/02-ovnchassis-cr.yaml`). It carries no bridge
mappings and no gateway: a kind node has no provider network to map, and a
gateway chassis without an uplink would announce connectivity it does not have.
`spec.image` is left unset so the operator resolves the same OVN image the
control plane runs.

A chassis attached to the BGP fabric adds the `fabric` block. This is the
fixture of the `fabric-advertised-route` suite
(`tests/e2e-ovn-overlay/fabric-advertised-route/02-ovnchassis-cr.yaml`), with
the node name the suite fills in:

```yaml
apiVersion: ovn.openstack.c5c3.io/v1alpha1
kind: OVNChassis
metadata:
  name: ovn-fabric
  namespace: openstack
spec:
  centralRef:
    name: ovn-fabric-central
  nodeSelector:
    e2e.c5c3.io/ovn-fabric: "true"
  fabric:
    uplinks:
    - interface: fab1
      bridge: br-fab1
    - interface: fab2
      bridge: br-fab2
    nodes:
    - name: node-a
      asn: 4200000001
      loopbackIPv4: 192.0.2.1
      loopbackIPv6: "2001:db8::1"
    announceableRanges:
    - 198.51.100.0/24
    - 2001:db8::/64
    advertiseVRFIDs: [42]
    learnVRFIDs: [10]
```

The VRF, the MTU and the BFD timers take their defaults: `fabric` on table 100,
9000, and 300 ms, 300 ms, 3.
