---
title: NovaCompute CRD
quadrant: operator
---

# NovaCompute CRD

`novacomputes.nova.openstack.c5c3.io/v1alpha1`, kind `NovaCompute`. The CRD is
generated from `operators/nova/api/v1alpha1/novacompute_types.go`; the
defaulting and validating webhooks live in
`operators/nova/api/v1alpha1/novacompute_webhook.go`.

One CR runs `nova-compute` on one node pool of a compute cluster. It names a
[Nova](./nova-crd.md) in its own namespace, selects the pool's nodes with a
required `spec.nodeSelector`, and projects a DaemonSet running
`ghcr.io/c5c3/nova-compute` onto them. Each node registers a compute service
under its Node name and joins the Nova's single cell. Everything below the
selector (the libvirt settings, the rollout pace, the overrides) applies to
every node the selector matches, so nodes with different settings get a CR of
their own.

The CR also looks after what the node's life in Nova needs. It keeps the host
aggregates openstack-hypervisor-operator onboards a hypervisor into, and a node
that leaves the pool is drained before its pod goes: its compute service is
disabled, the pod stays until Nova reports no instance on the host, and the
compute service is deleted after the pod is gone.

`kubectl get novacomputes` prints Ready
(`.status.conditions[?(@.type=='Ready')].status`), Desired
(`.status.desiredNumberScheduled`), Ready pods (`.status.numberReady`), and Age.

## Spec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `novaRef` | [`NovaRef`](#novaref) | yes | none | The Nova this pool joins. The pods mount its compute contract and its API registers their services. Immutable, enforced by a CEL transition rule and by the webhook |
| `nodeSelector` | `map[string]string` (MinProperties=1) | yes | none | The nodes of the pool. At least one label is required: an empty selector matches every node in the cluster. It may change, and a node that stops matching is drained (see [The drain](#the-drain)) |
| `tolerations` | `[]corev1.Toleration` | no | none | Lets the pods run on tainted nodes. A toleration of `kvm.cloud.sap/offboarding:NoExecute` without `tolerationSeconds` is rejected (see [Webhook rules](#webhook-rules)) |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | no | `ghcr.io/c5c3/nova-compute:<Nova status.installedRelease>` | The nova-compute image. When nil the tag is the release the referenced Nova has installed, not the one it is moving to: the control plane upgrades first, and the pool follows once the schemas have moved |
| `libvirt` | [`NovaComputeLibvirtSpec`](#novacomputelibvirtspec) | no | `{}` | The `[libvirt]` options the pool renders |
| `updateStrategy` | [`NovaComputeUpdateStrategy`](#novacomputeupdatestrategy) | no | `{}` | Paces the DaemonSet rollout |
| `resources` | `*corev1.ResourceRequirements` | no | none | Requests and limits of the `nova-compute` container, applied to the `wait-for-chassis` init container too. Nil renders none |
| `extraConfig` | `map[string]map[string]string` | no | none | INI sections merged over the rendered `compute-pool.conf`. It is the per-pool override surface. The keys the pod takes from its environment or its mounts are rejected at admission (see [NovaComputeOwnedConfigKeys](#owned-keys)) |
| `targetClusterRef` | [`*commonv1.TargetClusterRefSpec`](../target-clusters.md#the-field) | no | `nil` (the local cluster) | The registered target cluster the DaemonSet and its ConfigMaps are created on. The CR, its status and its finalizers stay on the management cluster. Immutable. See [Target Clusters](../target-clusters.md) |

### NovaRef

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` (MinLength=1) | yes | none | The Nova's name. The reference is namespace-local: the pods mount the compute contract Secret in the CR's namespace |

### NovaComputeLibvirtSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `virtType` | `string` (Enum `kvm`, `qemu`) | no | `kvm` | `[libvirt] virt_type`. `qemu` runs guests without hardware acceleration |
| `cpuMode` | `string` (Enum `host-model`, `host-passthrough`, `custom`, `none`) | no | none | `[libvirt] cpu_mode`. When empty the key is not rendered and Nova's default applies |
| `cpuModels` | `[]string` (items `^[A-Za-z0-9_.-]+$`) | no | none | `[libvirt] cpu_models`, rendered comma-joined. Required with `cpuMode: custom` and refused otherwise |
| `imagesType` | `string` (Enum `default`, `qcow2`, `raw`, `flat`) | no | none | `[libvirt] images_type`. When empty the key is not rendered. The `rbd`, `lvm` and `ploop` backends are not offered: the image carries neither a Ceph client nor `lvm2` |

### NovaComputeUpdateStrategy

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `type` | `string` (Enum `RollingUpdate`, `OnDelete`) | no | `RollingUpdate` | The rollout mode. `OnDelete` hands the pace to whoever deletes the pods |
| `maxUnavailable` | `*intstr.IntOrString` | no | `1` (operator-resolved) | How many nodes may restart `nova-compute` at once. RollingUpdate only |

The set of nodes a pool holds is part of the pod template (see
[The pod](#the-pod)), so every change of it rolls the pool's pods under
`RollingUpdate`: a drain that starts, a node that is released, a conflict that
appears. Running instances survive a restart, because their domains live in the
node's libvirt, but a live migration in flight through a restarting
`nova-compute` fails. A pool that must not restart `nova-compute` on its own
takes `type: OnDelete`.

## Defaulting and validation

The mutating webhook leaves the object untouched. Every default is a
`+kubebuilder:default` the API server applies from the schema, or a value the
operator resolves at reconcile time: the image and the `maxUnavailable` of 1.
The webhook stays registered so a default that has to be materialized later can
be added without changing the deployed webhook configuration.

### Schema-layer rules

These hold even when the webhook is down.

| Message | Where it comes from |
| --- | --- |
| `targetClusterRef is immutable` | Two CEL transition rules on `NovaComputeSpec`, one for adding or removing the ref and one for renaming it |
| `novaRef is immutable` | A third transition rule. A pool moved to another Nova leaves its compute services registered with the first one |
| `cpuModels is required when cpuMode is custom and must be empty otherwise` | A CEL rule on `NovaComputeLibvirtSpec` |
| `exactly one of image.tag or image.digest must be set` | Inherited from `commonv1.ImageSpec` on `spec.image` |

Beside them the schema carries `MinLength=1` on `spec.novaRef.name`,
`MinProperties=1` on `spec.nodeSelector`, the three `spec.libvirt` enums, the
item pattern on `spec.libvirt.cpuModels`, and the `RollingUpdate`/`OnDelete`
enum on `spec.updateStrategy.type`.

### Webhook rules

The validating webhook accumulates every violation into one admission response.

| Message | Trigger |
| --- | --- |
| `name must be at most 63 characters: it is the app.kubernetes.io/instance label value of every child` | `metadata.name` longer than `MaxNovaComputeNameLength`. Create only: the name is immutable, and the webhook also sees the finalizer removal that completes a deletion |
| `novaRef.name must be set (the Nova this node pool joins)` | `spec.novaRef.name` is empty |
| `novaRef is immutable` | An update renames `spec.novaRef.name` |
| `nodeSelector must carry at least one label` | `spec.nodeSelector` is empty or absent |
| the messages of `IsQualifiedName` and `IsValidLabelValue` | A selector key or value the API server would refuse in a label selector, which would fail the Node list on every pass |
| `tolerates kvm.cloud.sap/offboarding:NoExecute; openstack-hypervisor-operator deletes the compute service only after every agent pod on the node is gone, and a nova-compute that stays re-registers it` | A toleration without `tolerationSeconds` that tolerates the taint, matched the way openstack-hypervisor-operator matches it. `operator: Exists` with an empty key tolerates every taint and is rejected too. A toleration with `tolerationSeconds` is admitted: the pod leaves on its own, and the operator counts it as evictable |
| `maxUnavailable applies to RollingUpdate only` | `maxUnavailable` together with `type: OnDelete` |
| `maxUnavailable must be an integer or a percentage such as "25%"` | A malformed value |
| `maxUnavailable must resolve to at least 1 for RollingUpdate` | A value that scales to zero against 100 |
| `cpuModels is required when cpuMode is custom and must be empty otherwise` | The webhook twin of the CEL rule |
| `<key> is managed via <source> and must not be set in extraConfig (<impact>)` | A rejected key of the [owned-key registry](#owned-keys) in `spec.extraConfig` |
| `extraConfig ... must not contain a newline or carriage return` | A control character in a section, key or value of `spec.extraConfig` |
| `no such option in the nova <release> option catalog` | An option the catalog of the referenced Nova's `spec.openStackRelease` does not carry |
| `repository must be set`, `exactly one of image.tag or image.digest must be set` | `spec.image` is set and incomplete |
| `target cluster name must be set` | `spec.targetClusterRef` is present with an empty `name` |
| `targetClusterRef is immutable (...)` | An update adds, drops or renames the ref |

The option-catalog check needs the release of the Nova, so the webhook reads the
Nova `spec.novaRef` names. A Nova that does not exist yet (a pool is commonly
applied beside it) skips the check with the single warning
`extraConfig catalog check skipped: Nova <ns>/<name> not found`. An empty
`spec.extraConfig` reads nothing and warns nothing. On update the check runs
again only when `spec.extraConfig` changed.

### Owned keys

`NovaComputeOwnedConfigKeys` in `operators/nova/api/v1alpha1/config_ownership.go`
lists the keys of `compute-pool.conf` the operator owns.

| Section | Key | Treatment | Source |
| --- | --- | --- | --- |
| `DEFAULT` | `host` | rejected | the node name, `OS_DEFAULT__HOST` from `spec.nodeName` |
| `DEFAULT` | `my_ip` | rejected | `OS_DEFAULT__MY_IP` from `status.hostIP` |
| `DEFAULT` | `transport_url` | rejected | `OS_DEFAULT__TRANSPORT_URL` from the compute contract |
| `DEFAULT` | `state_path` | rejected | the `/var/lib/nova` host mount |
| `oslo_concurrency` | `lock_path` | rejected | the `/var/lib/nova` host mount |
| `libvirt` | `connection_uri` | rejected | the `/run/libvirt` host mount |
| `os_vif_ovs` | `ovsdb_connection` | rejected | the `/run/openvswitch` host mount |
| `vnc` | `server_proxyclient_address` | rejected | `OS_VNC__SERVER_PROXYCLIENT_ADDRESS` from `status.hostIP` |
| `keystone_authtoken`, `service_user`, `placement`, `neutron`, `cinder` | `password` | rejected | `OS_<SECTION>__PASSWORD` from the compute contract |
| `libvirt` | `virt_type`, `cpu_mode`, `cpu_models`, `images_type` | reported | `spec.libvirt.*` |

A reported key is honored and surfaced through the `ExtraConfigHealthy`
condition and a Warning event. `[DEFAULT] compute_driver` and `[vnc]
server_listen` are rendered as defaults and are not owned: the e2e suite runs
the fake driver through `spec.extraConfig`, and the listen address is a
per-pool choice. The default `server_listen = $my_ip` binds every instance's
console to the node address the console proxy dials. The pod shares the node's
network and Nova sets no console password, so `0.0.0.0` would open every
console on every interface of the node.

## Status

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | List-map keyed by `type`; see [Conditions](#conditions) |
| `observedGeneration` | `int64` | The `.metadata.generation` the controller last reconciled |
| `installedImage` | `string` | The image the running DaemonSet was projected from, recorded once every node runs a ready pod |
| `desiredNumberScheduled` | `int32` | How many nodes the DaemonSet should run on, mirrored from it |
| `numberReady` | `int32` | How many of those nodes run a ready pod |
| `nodes` | [`[]NovaComputeNodeStatus`](#novacomputenodestatus) | The per-node ledger of the pool, list-map keyed by `name` |

### NovaComputeNodeStatus

A node that stops being selected keeps its entry until its compute service is
deleted, so the record of a drain outlives the selection.

| Field | Type | Description |
| --- | --- | --- |
| `name` | `string` | The node's name, which is also the host of its compute service |
| `phase` | `string` | One of the [node phases](#node-phases) |
| `zone` | `string` | The node's `topology.kubernetes.io/zone` label, the availability zone of its aggregate |
| `serviceID` | `string` | The UUID of the `nova-compute` service registered under the node name |
| `serviceStatus` | `string` | `enabled` or `disabled`, as Nova reports it |
| `serviceState` | `string` | `up` or `down`, as Nova reports it |
| `disabledReason` | `string` | The reason Nova records for a disabled service |
| `instances` | `*int32` | How many servers Nova counts on a Draining node |
| `conflictsWith` | `string` | The NovaCompute that holds a node in `Conflict` |

### Node phases

| Phase | Meaning |
| --- | --- |
| `Pending` | Selected; no compute service is registered under the node name yet |
| `Active` | Selected; the service is registered |
| `Draining` | The node left the pool. Its pod stays, its service is disabled, and Nova still counts instances on it |
| `Releasing` | No instance is left, or another pool took the node over. The pod is released, and the service is deleted once the pod is gone |
| `Conflict` | Selected, but another NovaCompute of the same Nova on the same cluster holds the node. No pod of this CR runs there |

A node is held by a CR when it appears in that CR's `status.nodes` in any phase
but `Conflict`. The rules, in order:

1. A node the selector matches and this CR holds stays; a `Draining` or
   `Releasing` entry goes back to `Active` (or `Pending`), and its service stays
   disabled. Only when an older pool that is not being deleted holds it
   `Pending` or `Active` too, which a pass that read the other pool's status
   before its write landed can leave behind, does this CR yield: `Conflict`,
   with the service left to the older pool. Held by another pool: `Conflict`.
   Neither holds it but an older pool selects it (by `creationTimestamp`, then
   name): `Conflict`. Otherwise this CR takes it as `Pending`.
2. A held node that is no longer selected, is gone from the cluster, or belongs
   to a CR being deleted: `Releasing` without a disable when a pool that is not
   being deleted selects it (a handover), `Draining` otherwise.
3. A `Conflict` entry that is no longer selected is dropped.

Pools of different Novas, or on different clusters, never conflict.

### Conditions

Six steps each own one condition type. The aggregate `Ready` is `True` only when
all six are. `ExtraConfigHealthy` reports on the overlay the owner writes and
stays out of the aggregate. For the pipeline see
[Reconciler Architecture](./nova-reconciler.md#novacompute).

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `NovaReady` | True | `NovaResolved` | The Nova exists, has installed a release, has published its contract, and its service-user password is readable |
| `NovaReady` | False | `NovaNotFound` | No Nova of that name in the namespace |
| `NovaReady` | False | `WaitingForInstalledRelease` | The Nova has not installed a release yet |
| `NovaReady` | False | `ComputeConfigNotPublished` | The Nova has not published `status.computeConfigSecretRef` yet |
| `NovaReady` | False | `TargetClusterUnavailable` | The pool's target cluster, or the Nova's, does not resolve |
| `NovaReady` | False | `WaitingForServiceUserSecret` | The Nova's service-user Secret, or its key, is absent or empty |
| `NovaReady` | False | `NovaError` | Reading the Nova or its Secret failed |
| `NodesReady` | True | `NodesResolved` | The message counts the selected nodes and the ones leaving |
| `NodesReady` | False | `NoMatchingNodes` | Nothing is selected and nothing is held. Not a wait: the later steps still run |
| `NodesReady` | False | `NodeConflict` | A selected node is held by another pool; the message lists `node (held by <pool>)`. A Warning event `NodeConflict` records each new conflict once |
| `NodesReady` | False | `NodesForbidden` | The Node list answered 403, which is the nova chart installed with `rbac.namespaceScoped=true` and a pool on the local cluster |
| `NodesReady` | False | `NodeListError` | Listing or reading Nodes failed otherwise |
| `ConfigReady` | True | `ConfigRendered` | `compute-pool.conf` is rendered into its ConfigMap |
| `ConfigReady` | False | `WaitingForComputeConfig` | The contract Secret is not in the CR's namespace on the pool's cluster. The ControlPlane mirrors it for a ControlPlane-managed Nova; otherwise copy it ([Connect a compute cluster](../../guides/nova/connect-a-compute-cluster.md)) |
| `ConfigReady` | False | `ComputeConfigIncomplete` | `nova-compute.conf`, `transport_url` or `password` is missing or empty; the message names them |
| `ConfigReady` | False | `ConfigError` | Reading the Secret, or writing or pruning the ConfigMaps, failed |
| `DaemonSetReady` | True | `DaemonSetReady` | Every node the DaemonSet selects runs a ready pod, or the pool holds no node and the DaemonSet was removed |
| `DaemonSetReady` | False | `DaemonSetProgressing` | A rollout is in flight. Not a wait: the aggregates and the drain still run |
| `DaemonSetReady` | False | `DaemonSetError` | The DaemonSet could not be applied, read or deleted |
| `AggregatesReady` | True | `AggregatesEnsured` | The aggregates of the pool's zones and `tenant_filter_tests` are in place |
| `AggregatesReady` | False | `NodesWithoutZone` | A selected node carries no `topology.kubernetes.io/zone`; openstack-hypervisor-operator cannot onboard it. The other zones are still ensured |
| `AggregatesReady` | False | `AggregateZoneMismatch` | An aggregate of that name exists with another availability zone; the message names both |
| `AggregatesReady` | False | `WaitingForAggregate` | Another creator made the aggregate between the list and the create; the next pass reads it |
| `AggregatesReady` | False | `ComputeAPIError` | A Keystone or Nova call failed; the message names the call and the HTTP status. Retried after 30 seconds |
| `AggregatesReady` | False | `NovaComputeListError` | Listing the other NovaComputes of the Nova failed, on a pass that did not run the Nodes step |
| `ServicesReady` | True | `ServicesUp` | Every node's compute service is registered and up |
| `ServicesReady` | True | `Draining` | Nodes are Draining or Releasing; the message counts the instances per node |
| `ServicesReady` | False | `WaitingForServices` | A selected node has no compute service yet |
| `ServicesReady` | False | `ServicesDown` | Nova reports an Active node's service down |
| `ServicesReady` | False | `ComputeAPIError` | A Keystone or Nova call failed, or a cell did not answer the service list. Retried after 30 seconds |
| `ServicesReady` | False | `PodListError` | Listing the pods on a Releasing node failed |
| `Ready` | True | `AllReady` | All six sub-conditions are True |
| `Ready` | False | `NotAllReady` | At least one is not |

## Sub-Resource Naming Convention

| Resource | Name | Notes |
| --- | --- | --- |
| DaemonSet | `{name}-nova-compute` | Labels `app.kubernetes.io/name: novacompute`, `app.kubernetes.io/instance: {name}`, `app.kubernetes.io/component: nova-compute` |
| ConfigMap | `{name}-config-<hash>` | The immutable `compute-pool.conf`; the three newest before the mounted one are kept |

The compute contract Secret is not a child: the Nova publishes it, or the
ControlPlane mirrors it. On a target cluster the children carry the ownership
labels instead of an owner reference, and the teardown sweeps them.

## Node contract

### The pod

The DaemonSet has no node selector. The node set is a required node affinity:
one term selects the pool (a `key In [value]` per selector pair, minus a
`metadata.name NotIn` per node in `Conflict`), and one term
`metadata.name In [node]` per Draining node keeps its pod while its instances
leave. A deleting pool that holds no Draining node has no term, and its
DaemonSet is deleted.

The pod runs with `hostNetwork: true` and the seccomp profile `RuntimeDefault`.
`nova-compute` runs privileged as uid 0 with
`nova-compute --config-file /etc/nova/compute-config/nova-compute.conf
--config-dir /etc/nova/compute-pool.conf.d`. Root is what a stock host needs:
the libvirt socket is `root:libvirt` 0660 with a group ID that differs from
host to host, and a `hostPath` directory comes up `root:root`. There is no
probe; the service state Nova reports is the health signal.

| Volume | Mount path | Details |
| --- | --- | --- |
| the contract Secret | `/etc/nova/compute-config` | read-only; the fragment's `ssl_ca_file` names `ca.crt` here |
| the pool ConfigMap | `/etc/nova/compute-pool.conf.d` | read-only |
| hostPath `/run/libvirt` | same | `DirectoryOrCreate`; the libvirt socket |
| hostPath `/var/lib/nova` | same | `DirectoryOrCreate`, `mountPropagation: Bidirectional`, so the NFS volumes os-brick mounts below it reach the host's QEMU. The node's `compute_id` lives here, so a restarted pod keeps its identity |
| hostPath `/run/openvswitch` | same | `DirectoryOrCreate`; the node's Open vSwitch database |
| hostPath `/dev` | same | |
| hostPath `/sys/fs/cgroup` | same | read-only |
| hostPath `/lib/modules` | same | read-only, for os-brick's `modprobe` |
| hostPath `/etc/iscsi`, `/etc/nvme`, `/etc/multipath` | same | `DirectoryOrCreate` |
| hostPath `/etc/multipath.conf` | same | `FileOrCreate` |
| emptyDir | `/tmp` | |

The `wait-for-chassis` init container runs the image's `python3` under the
restricted profile and waits until the node's OVN chassis has written
`external_ids:system-id` into the local Open vSwitch database, the gate the
[metadata agent](../neutron/index.md) runs. It speaks the OVSDB JSON-RPC
protocol itself, because the image ships no `ovsdb-client`, so a pool's nodes
need an [OVNChassis](../ovn/ovn-chassis-crd.md).

### The namespace

A pod carrying the privileged profile needs a namespace that admits it. On a
compute cluster that is a namespace in the target-cluster-access chart's
`privilegedNamespaces`, the list the OVN chassis uses as well (see
[Target Clusters](../target-clusters.md)). A local pool runs in its namespace
on the management cluster. Label that namespace with
`pod-security.kubernetes.io/enforce` deliberately: one without the label admits
the privileged pod, so whoever may create a NovaCompute there gets root on the
nodes it selects (see
[Who may name a cluster](../target-clusters.md#who-may-name-a-cluster)).
openstack-hypervisor-operator waits for the agent pods of its
`--agent-namespaces` to leave a node it offboards, so the pool's namespace
belongs in that list too, and no pod of the pool may tolerate
`kvm.cloud.sap/offboarding:NoExecute` indefinitely.

Every node needs the `topology.kubernetes.io/zone` label. The pool creates the
aggregate named after the zone, and openstack-hypervisor-operator adds the host
to it on onboarding.

### Reaching Nova

The operator calls Keystone and the Nova API itself: it lists and disables the
compute services, counts the servers on a host and keeps the aggregates. It
authenticates as the Nova's `spec.serviceUser` at microversion 2.53, and lists
the services at 2.69: below it Nova leaves out a cell that does not answer, and
the servers list skips that cell as well, so its draining hosts would read as
empty. A service Nova reports `UNKNOWN` fails the pass with `ComputeAPIError`
instead. The disable, the delete, the aggregates and the `host` filter of the
server count are admin-only under Nova's default policies, so that user needs
the `admin` role. The ControlPlane grants it; a standalone Nova whose service
user lacks it reports `ComputeAPIError` with HTTP 403. The operator reaches the
local Nova at its Service URL and a placed one through the target cluster's
service proxy, so a NetworkPolicy in front of Keystone or Nova has to admit the
nova-operator.

### The aggregates

For each zone of its `Pending` and `Active` nodes the pool ensures an aggregate
named after the zone, carrying it as its availability zone, and, while it is
not being deleted, the zone-less `tenant_filter_tests`. openstack-hypervisor-operator
puts every host it onboards into both and fails the onboarding when either is
missing. An aggregate the pool creates carries the metadata
`c5c3.io:nova=<namespace>/<nova>`; one it created but could not mark is deleted
again and recreated on the next pass. One that exists already is used as it is.
A marked aggregate that no pool of the Nova needs is deleted once no host is
left in it, so the last pool of a Nova removes `tenant_filter_tests` on its way
out. One that carries metadata beyond the marker and its availability zone,
such as the `filter_tenant_id` of a tenant isolation, is kept instead, with a
Warning event `AggregateKept` naming the keys: deleting it would drop them, and
the aggregate the zone gets back would come without them. An unmarked aggregate
is never deleted.

### The drain

Leaving the pool is the drain. It starts when a node stops matching the
selector, when its Node is deleted, or for every node when the CR is deleted:

1. The node goes `Draining`. The pool disables its compute service once, with
   the reason `c5c3.io: leaving NovaCompute <namespace>/<name>`, and keeps its
   pod through the held affinity term.
2. The pool never migrates an instance. On a compute cluster,
   openstack-hypervisor-operator's Eviction, started through
   `Hypervisor.spec.maintenance`, empties the host; without it the owner does.
3. When Nova counts no server on the host, the node goes `Releasing` and its pod
   is released.
4. Once the pod is gone the pool deletes the compute service. Nova removes the
   host from every aggregate, deletes its resource providers and destroys its
   host mapping. A delete Nova refuses because instances came back returns the
   node to `Draining`.

The pool never enables a service. A node selected again mid-drain goes back to
`Active` with its service still disabled, and so does a node another pool takes
over (`Releasing` without a disable): the new pool finds the service as the old
one left it. `openstack compute service set --enable <host> nova-compute`
re-enables it by hand. A node deleted while it still holds instances stays
`Draining` until the owner acts.

The CR carries the finalizer `nova.openstack.c5c3.io/compute-drain` until every
node is released and the aggregates are cleaned up, so an unreachable Nova API
holds a deletion. The escape is removing the finalizer by hand, which leaves the
compute services and the marked aggregates in Nova:

```bash
kubectl patch novacompute <name> -n <namespace> --type=merge \
  -p '{"metadata":{"finalizers":null}}'
```

When the Nova is gone, or the target cluster was abandoned, the teardown skips
the Nova side. The last pool of a Nova on a cluster also deletes two Secrets in
its namespace, each only when it carries the label
`nova.openstack.c5c3.io/compute-config-mirror: "true"`, the mark of the
ControlPlane's mirror: the compute contract `<nova>-compute-config`, and
`<nova>-hypervisor-operator-auth`, the credentials the ControlPlane copies there
for openstack-hypervisor-operator. Each deleted Secret gets a
`ComputeConfigMirrorReaped` event of its own, and an absent one is skipped. A
pool being deleted that still holds a node counts as one left: its draining pod
mounts the contract.

## Example

```yaml
apiVersion: nova.openstack.c5c3.io/v1alpha1
kind: NovaCompute
metadata:
  name: pool-a
  namespace: openstack
spec:
  novaRef:
    name: nova-pool
  nodeSelector:
    openstack.c5c3.io/nova-compute-pool: a
  resources:
    requests:
      cpu: 50m
      memory: 128Mi
    limits:
      memory: 768Mi
  extraConfig:
    DEFAULT:
      compute_driver: fake.FakeDriverWithoutFakeNodes
      vif_plugging_is_fatal: "false"
      vif_plugging_timeout: "0"
```

This is the pool the `compute-node-pool` suite applies
(`tests/e2e/nova/compute-node-pool/12-novacompute-pool-a.yaml`). The
`extraConfig` switches to the fake driver, because kind has no KVM; a pool on
real hypervisors leaves `compute_driver` at its default and sets
`spec.libvirt` instead.
