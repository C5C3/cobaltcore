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
that Secret. Both refs are immutable, because repointing a live chassis strands
its registration in the old Southbound database.

`kubectl get ovnchassis` prints Ready
(`.status.conditions[?(@.type=='Ready')].status`), Desired
(`.status.desiredNumberScheduled`), Ready pods (`.status.numberReady`), and Age.

## Spec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | no | operator-resolved `ghcr.io/c5c3/ovn:26.03.2` | The image that runs Open vSwitch and `ovn-controller`. Both ship in the one OVN image, so one reference covers them. The operator resolves it at reconcile time, so an unset field keeps tracking its tested version across upgrades (`image.go`) |
| `centralRef` | [`OVNCentralRef`](#ovncentralref) | yes | none | The `OVNCentral` whose Southbound database these chassis connect to and whose client Secret they mount. Immutable, enforced by a CEL transition rule and by the webhook |
| `nodeSelector` | `map[string]string` (MinProperties=1) | yes | none | The nodes both DaemonSets land on. At least one label is required: an empty selector matches every node in the cluster, which would start `ovn-controller` on the control-plane nodes and on whatever joins later |
| `tolerations` | `[]corev1.Toleration` | no | none | Lets the DaemonSet pods run on tainted nodes. Networking nodes are commonly tainted to keep ordinary workloads off them, and the chassis pods are what has to run there |
| `gateway` | [`*OVNGatewaySpec`](#ovngatewayspec) | no | `nil` | Marks the subset of the selected nodes that announce `enable-chassis-as-gw`, the flag that makes a chassis eligible to host a distributed router's gateway port. When nil no node in this CR is a gateway |
| `bridgeMappings` | [`[]OVNBridgeMapping`](#ovnbridgemapping) | no | none | Maps each OpenStack physical network onto the local OVS bridge that reaches it. List-map keyed by `physicalNetwork`. Every node this CR selects gets the same mapping |
| `encapType` | `string` (Enum `geneve`, `vxlan`) | no | `geneve` | The tunnel protocol between chassis. Geneve carries the variable-length option header OVN uses for its logical metadata; VXLAN has no room for it and so caps the logical topology. VXLAN exists for hardware that cannot terminate Geneve |
| `updateStrategy` | [`OVNChassisUpdateStrategy`](#ovnchassisupdatestrategy) | no | `{}` | Paces the DaemonSet rollout. Restarting `ovn-controller` interrupts the dataplane programming on that node, so the pace is a per-deployment tradeoff |
| `remoteProbeIntervalMs` | `int32` (Minimum=0) | no | `60000` | How long `ovn-controller` lets its Southbound connection sit idle before probing it. Zero disables the probe, which is what a chassis behind a connection-tracking middlebox needs when the probe is what tears the connection down |
| `ovs` | [`*OVNChassisContainerSpec`](#ovnchassiscontainerspec) | no | `nil` | Tunes the Open vSwitch containers |
| `controller` | [`*OVNChassisContainerSpec`](#ovnchassiscontainerspec) | no | `nil` | Tunes the `ovn-controller` container |
| `targetClusterRef` | [`*commonv1.TargetClusterRefSpec`](../target-clusters.md#the-field) | no | `nil` (the local cluster) | The registered target cluster the DaemonSets are created on. The CR itself, its status and its finalizer stay on the management cluster. Immutable, enforced by two CEL transition rules and by the webhook. It has to name the same cluster the `OVNCentral` names. See [Target Clusters](../target-clusters.md) |

### OVNCentralRef

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` (MinLength=1) | yes | none | The `OVNCentral`'s name. The reference is namespace-local: the chassis mount the client Secret it publishes, and a Secret cannot be mounted across namespaces |

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

### OVNChassisUpdateStrategy

A narrowed `DaemonSetUpdateStrategy`. There is no `maxSurge` counterpart, because
a second `ovn-controller` on the same node would contend with the first one over
the local Open vSwitch database.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `type` | `string` (Enum `RollingUpdate`, `OnDelete`) | no | `RollingUpdate` | The rollout mode. `OnDelete` hands the pace to whoever drains the nodes, which is what a deployment with an external maintenance workflow wants |
| `maxUnavailable` | `*intstr.IntOrString` | no | operator-resolved `1` | How many selected nodes may lose their dataplane programming at once. It applies to `RollingUpdate` only, and the webhook rejects it alongside `OnDelete` so it cannot read as effective |

### OVNChassisContainerSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `resources` | `*corev1.ResourceRequirements` | no | none | Requests and limits for the container. A block the CR leaves unset renders none, the same way the database container does: what a datapath needs depends on the traffic the node carries, and a default picked here would be wrong on most hardware |

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
`spec.updateStrategy.type`, `Minimum=0` on `spec.remoteProbeIntervalMs`, the two
patterns on a bridge mapping, and `MinLength=1` on `spec.centralRef.name`.
`spec.bridgeMappings` is a list-map keyed by `physicalNetwork`, so the API server
already refuses a repeated physical network.

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
name an `OVNCentral` that does not exist at admission time, so the check that
both CRs project onto the same cluster runs in the controller and reports
`CentralReady=False` with reason `CentralOnAnotherCluster`.

## Status

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | List-map keyed by `type`; see [Conditions](#conditions) |
| `observedGeneration` | `int64` | The `.metadata.generation` the controller last reconciled |
| `installedImage` | `string` | The image reference the running DaemonSets were projected from, recorded once the `ovn-controller` DaemonSet reports every node ready. It tells a rollout that has not reached the nodes from one that has |
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
| `leaving` | `bool` | The node is no longer selected, or no longer in the cluster, and its chassis registration has still to be deleted. Until that deletion lands the stale chassis keeps claiming the ports of workloads that have moved elsewhere |

### Conditions

Five sub-reconcilers each own one condition type. The aggregate `Ready` is `True`
only when all five are. For the pipeline that sets them see
[Reconciler Architecture](./ovn-reconciler.md).

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `CentralReady` | True | `CentralResolved` | The `OVNCentral` published its Southbound address and its client Secret, and both CRs project onto the same cluster. The message names the address the chassis dial |
| `CentralReady` | False | `CentralNotFound` | No `OVNCentral` of that name in the namespace. An `OVNChassis` applied before its `OVNCentral` is an ordinary ordering of two objects in one manifest, so this polls |
| `CentralReady` | False | `CentralReadError` | Reading the `OVNCentral` failed |
| `CentralReady` | False | `CentralOnAnotherCluster` | The two CRs name different target clusters. A chassis mounts the Secret the central publishes, and a Secret does not cross a cluster boundary. No requeue: both refs are immutable, so only deleting and reapplying one of the two can repair it |
| `CentralReady` | False | `CentralNotReady` | The `OVNCentral` has not published its Southbound address or its client Secret yet |
| `CentralReady` | False | `CentralUpgrading` | The central's `status.installedImage` differs from the image it resolves, so its own rollout is still in flight. OVN's supported upgrade order is central first, hypervisors second: `ovn-controller` reads the Southbound schema the central owns |
| `CentralReady` | False | `TargetClusterUnavailable` | `spec.targetClusterRef` names a cluster that does not resolve. This is the pipeline's first gate, so the failure lands on the condition the rest of the graph waits behind |
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
| `Ready` | True | `AllReady` | All five sub-conditions are True |
| `Ready` | False | `NotAllReady` | At least one is not |

## Sub-Resource Naming Convention

Every child takes the CR name plus a component suffix. For an `OVNChassis` named
`{name}`:

| Resource | Name | Notes |
| --- | --- | --- |
| DaemonSet | `{name}-ovs` | `ovsdb-server` and `ovs-vswitchd` on the selected nodes, in the node's network namespace |
| DaemonSet | `{name}-ovn-controller` | `ovn-controller`, connected to the Southbound address the central published |
| ConfigMap | `{name}-nodes` | One key per node this CR is responsible for, carrying that node's values. One object serves every node: the pod on a node reads the file named after it and ignores the rest |
| ConfigMap | `{name}-chassis-scripts` | The scripts both DaemonSets and the three Jobs run. The suffix names the kind, so a chassis and a central of the same name keep separate scripts ConfigMaps in one namespace |
| Job | `{name}-apply-<8 hex>` | Re-applies one node's values after they changed. Pinned to that node and in its network namespace |
| Job | `{name}-evacuate-<8 hex>` | Moves the gateway duties off one node, against the Northbound database |
| Job | `{name}-chassis-del-<8 hex>` | Deletes one node's Southbound `Chassis` row, against the Southbound database |

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

The one privileged container is the `host-prepare` init of the `{name}-ovs`
DaemonSet. It runs `modprobe openvswitch && modprobe geneve`, creates the two run
directories group-writable under uid and gid 42424, creates the local database
when the node carries none, and converts an existing one to the schema of the
image that is now running. Everything else runs unprivileged or with two named
capabilities: `NET_ADMIN` and `SYS_NICE` on `ovs-vswitchd`, `NET_ADMIN` on
`ovn-controller`. `SYS_NICE` is what lets `ovs-vswitchd` raise the priority of
its own polling threads; without it every start logs the failure and the daemon
runs at ordinary priority.

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
| `ovn-remote` | The Southbound relay when the central runs one, the Southbound database itself otherwise |
| `ovn-remote-probe-interval` | `spec.remoteProbeIntervalMs` |
| `ovn-bridge-mappings` | The `physnet:bridge` list rendered from `spec.bridgeMappings`, in spec order. Removed when there are no mappings, never set to the empty string: `ovs-vsctl` refuses a value-less assignment and would fail the init container under `set -eu` |
| `ovn-cms-options` | `enable-chassis-as-gw` on a gateway node, removed on every other one |

Every mapped bridge is then created with `ovs-vsctl --may-exist add-br`.
`ovn-controller` attaches patch ports to a bridge but never creates one, so a
mapping pointing at a bridge that does not exist would silently drop every packet
on that physical network.

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
