---
title: OVN Reconciler Architecture
quadrant: operator
---

# OVN Reconciler Architecture

The ovn-operator runs two controllers over the shared table-driven pipeline
(`internal/common/reconcile`): `OVNCentralReconciler` with seven sub-reconcilers
and `OVNChassisReconciler` with five. In both, the first step to return a
non-zero result or an error short-circuits the chain, and every exit path
persists the conditions and the requeue through the shared status skeleton, which
skips the write when a pass left status unchanged.

Every step is instrumented under the `ovn_operator` metrics prefix. Two vectors
cover the pipelines: `ovn_operator_reconcile_duration_seconds`, labelled by
`sub_reconciler`, and `ovn_operator_reconcile_errors_total`, labelled by
`sub_reconciler` and `condition_type`. Two per-CR collectors cover the database
backup: `ovn_operator_backup_total`, labelled by `ovncentral`, `namespace` and
the terminal `result`, and `ovn_operator_backup_duration_seconds`, labelled by
`ovncentral` and `namespace` (`operators/ovn/internal/metrics/collectors.go`). Both backup
series are dropped for a CR name on the pass that observes the `OVNCentral` gone
from the API server, so a deleted CR leaks no time series. There is nothing to
drop for an `OVNChassis`: only an `OVNCentral` takes backups, and the two
pipeline vectors carry no CR name.

## Pipelines

### OVNCentral

```text
TLS ──► Northbound ──► Southbound ──► Endpoints ──► ┬─ Northd
                                                    ├─ Relay
                                                    └─ Backup   (parallel)
```

| Step | What it does | Condition |
| --- | --- | --- |
| TLS | Probes the target cluster for the `cert-manager.io/v1` Certificate kind, requests the two server keypairs, the client keypair and the relay keypair, and publishes `status.clientSecretName` once the client Secret carries `tls.crt`, `tls.key` and `ca.crt` | `TLSReady` |
| Northbound | Projects the Northbound Raft cluster: the shared scripts ConfigMap, the headless Service, one Service per member, and the StatefulSet | `NorthboundReady` |
| Southbound | The same code against the Southbound database, picking its condition type off the `raftDB` it was handed | `SouthboundReady` |
| Endpoints | Assembles both connection strings per database from the per-member Services and pods, and stamps `internalDbAddress` and `dbAddress` | `EndpointsReady` |
| Northd | Projects the `ovn-northd` Deployment and stamps `status.installedImage` once it is available | `NorthdReady` |
| Relay | Projects or removes the Southbound relay Deployment and Service, and stamps `status.relayAddress` | `RelayReady` |
| Backup | Projects the snapshot claim and the CronJob, and reports on the newest terminal backup Job | `BackupReady` |

TLS is the first gate: every OVN connection is authenticated with the
certificates it requests, so a database projected before them would come up with
nothing to present. The two databases follow, then the step that publishes their
addresses. The last three read those addresses and nothing of each other's
output, so they run as a parallel group. Each member works on its own copy of the
CR and always sets its one condition type, which is why a CR without
`spec.relay` still resolves the aggregate, through `RelayNotRequired`.

`RunParallelGroup` merges the conditions and the metadata off a member's copy and
nothing else, so the two status fields a member publishes are copied onto the
primary CR by hand in `parallelSteps`: `status.installedImage` from Northd and
`status.relayAddress` from Relay.

### OVNChassis

```text
Central ──► Nodes ──► OVS ──► Controller ──► Maintenance
```

| Step | What it does | Condition |
| --- | --- | --- |
| Central | Resolves the `OVNCentral` named by `spec.centralRef` into the Southbound remote, the two database addresses and the client Secret name | `CentralReady` |
| Nodes | Renders one entry per node into the `{name}-nodes` ConfigMap, applies the `{name}-chassis-scripts` ConfigMap, and rebuilds `status.nodes` | `NodesReady` |
| OVS | Projects the `{name}-ovs` DaemonSet | `OVSReady` |
| Controller | Projects the `{name}-ovn-controller` DaemonSet, mirrors its node counters into status, and stamps `status.installedImage` | `ControllerReady` |
| Maintenance | Runs the per-node `apply`, `evacuate` and `chassis-del` Jobs that are due | `MaintenanceReady` |

The `OVNCentral` is the first gate: its Southbound address and its client Secret
parameterise every later step, so a chassis whose central has published neither
projects nothing at all. The per-node values come next, because both DaemonSets
mount the ConfigMap holding them.

There is no parallel group here. The two DaemonSets look independent, but Open
vSwitch owns the local database `ovn-controller` writes its chassis record into,
so a node that runs the second without the first has nothing to register against.
Maintenance runs last, since it acts on nodes the node step has already marked as
leaving or as giving up the gateway role. `central` and `nodes` are threaded from
the steps that resolve them to the steps that consume them through a closure, so
the hand-off stays inside one pass. A field on the reconciler would be shared by
every CR reconciled concurrently.

## Conditions

Each aggregate `Ready` is `True` with reason `AllReady` when every sub-condition
of that kind is `True`, and `False` with `NotAllReady` otherwise. An `OVNCentral`
aggregates seven, an `OVNChassis` five.

| Type | Kind | True reasons | False reasons |
| --- | --- | --- | --- |
| `TLSReady` | `OVNCentral` | `CertificatesIssued` | `CertificatePending`, `CertificateError`, `CertManagerUnavailable`, `CapabilityProbeFailed`, `TargetClusterUnavailable` |
| `NorthboundReady` | `OVNCentral` | `StatefulSetReady` | `StatefulSetProgressing`, `StatefulSetError` |
| `SouthboundReady` | `OVNCentral` | `StatefulSetReady` | `StatefulSetProgressing`, `StatefulSetError` |
| `EndpointsReady` | `OVNCentral` | `EndpointsPublished` | `EndpointsPending` |
| `NorthdReady` | `OVNCentral` | `DeploymentReady` | `DeploymentProgressing`, `DeploymentError`, `WaitingForEndpoints` |
| `RelayReady` | `OVNCentral` | `DeploymentReady`, `RelayNotRequired` | `DeploymentProgressing`, `DeploymentError`, `ServicePending`, `WaitingForEndpoints` |
| `BackupReady` | `OVNCentral` | `BackupScheduled`, `BackupSuspended` | `BackupJobFailed`, `BackupPVCInvalid`, `BackupError`, `WaitingForEndpoints` |
| `CentralReady` | `OVNChassis` | `CentralResolved` | `CentralNotFound`, `CentralReadError`, `CentralOnAnotherCluster`, `CentralNotReady`, `CentralUpgrading`, `TargetClusterUnavailable` |
| `NodesReady` | `OVNChassis` | `NodesRendered` | `NoMatchingNodes`, `NodeListError`, `NodesError` |
| `OVSReady` | `OVNChassis` | `DaemonSetReady` | `DaemonSetProgressing`, `DaemonSetError` |
| `ControllerReady` | `OVNChassis` | `DaemonSetReady` | `DaemonSetProgressing`, `DaemonSetError` |
| `MaintenanceReady` | `OVNChassis` | `MaintenanceIdle`, `MaintenanceRunning`, `MaintenanceDeferred` | `MaintenanceJobFailed`, `MaintenanceError` |

`TargetClusterUnavailable` is set ahead of every sub-reconciler, when
`spec.targetClusterRef` names a cluster that is not registered or no longer
resolves. It lands on the first condition of that CR's pipeline, `TLSReady` on an
`OVNCentral` and `CentralReady` on an `OVNChassis`, so it holds down the
condition the rest of the graph waits behind. The message carries the resolver's
error, the CR requeues after 15 seconds and acquires no finalizer, and nothing is
created on any cluster. See [Target Clusters](../target-clusters.md).

A running maintenance Job leaves `MaintenanceReady` at `True`. A Job that runs
finishes on its own, and holding the aggregate `Ready` down for a rolling drain
would report ordinary progress as an outage. Only a terminal Job failure and an
API error flip the condition.

One map attributes an error to its condition type for the `condition_type` label.
It serves both pipelines, because a `sub_reconciler` name is unique across the
two (`instrumentation.go`):

```go
var subReconcilerConditionTypes = map[string]string{
	"TLS":        conditionTypeTLSReady,
	"Northbound": conditionTypeNorthboundReady,
	"Southbound": conditionTypeSouthboundReady,
	"Endpoints":  conditionTypeEndpointsReady,
	"Northd":     conditionTypeNorthdReady,
	"Relay":      conditionTypeRelayReady,
	"Backup":     conditionTypeBackupReady,

	"Central":     conditionTypeCentralReady,
	"Nodes":       conditionTypeNodesReady,
	"OVS":         conditionTypeOVSReady,
	"Controller":  conditionTypeControllerReady,
	"Maintenance": conditionTypeMaintenanceReady,
}
```

A `sub_reconciler` name that reaches the instrumenter without a key here resolves
to `UNKNOWN`. An empty label would collapse two series into one; `UNKNOWN` makes
the drift visible in alerts.

## Sub-reconcilers

### reconcileTLS

**File:** `operators/ovn/internal/controller/reconcile_tls.go`

**Purpose:** Request every certificate an OVN connection is authenticated with:
one server keypair per database, one for the relay tier when the CR runs one, and
one client keypair shared by everything that dials them. All of them are ensured
before the first pending one is reported, so a cluster starting from nothing
requests every certificate on its first pass. Reporting the first pending one and
returning would cost a polling interval per certificate. The step ends at the
issued client Secret, one layer past the Certificates: the Secret is what the
workloads mount and what an `OVNChassis` is pointed at through
`status.clientSecretName`.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `CapabilityProbeFailed` | The RESTMapper probe for the Certificate kind failed against the target cluster | none (error returned) |
| `False` | `CertManagerUnavailable` | "cert-manager is not installed on the cluster the children land on; spec.tls requires it" | `RequeueSecretPolling` |
| `False` | `CertificatePending` | "Waiting for cert-manager to issue \<names\>", or "Waiting for cert-manager to write Secret \<name\>" | `RequeueSecretPolling` |
| `False` | `CertificateError` | The wrapped apply or read error, or the defect of the issued Secret: "issued Secret \<name\> lacks tls.crt", or "issuer \<kind\>/\<name\> issued no ca.crt; spec.tls.issuerRef must name a CA issuer" | none |
| `True` | `CertificatesIssued` | "Both server certificates and the client certificate in Secret \<name\> are issued" | none |

**Error handling:** A failed capability probe, a failed `EnsureCertificate` and a
failed Secret read set the condition and return the error, so the controller
backs off. A Secret that is missing a key is reported and left alone, with no
requeue and no error: only an edit to `spec.tls.issuerRef` can leave that state,
and a retry would change neither the Secret nor the message the CR carries. A
cluster without cert-manager is a wait, not an error: `spec.tls` is required and
the fix happens outside the CR.

### reconcileNorthbound

**File:** `operators/ovn/internal/controller/reconcile_database.go`

**Purpose:** Project the Northbound Raft cluster. It is a two-line wrapper that
hands `reconcileRaftDatabase` the `northboundDB(cr)` descriptor, which carries
the `nb` suffix, the spec block, the ports and the condition type.

**Condition Contract:** as [reconcileSouthbound](#reconcilesouthbound), under
`NorthboundReady`.

**Error handling:** as `reconcileSouthbound`.

### reconcileSouthbound

**File:** `operators/ovn/internal/controller/reconcile_database.go`

**Purpose:** The same `reconcileRaftDatabase` body against the Southbound
database. It applies the shared scripts ConfigMap, the headless Service, one
Service per member before the StatefulSet, and the StatefulSet itself, then reads
the StatefulSet back and mirrors `readyReplicas` into
`status.<db>.readyReplicas`. The per-member Services go first: a member that
comes up before its Service has no address to publish, and the endpoint step
would hold the whole CR unready until the next pass created it.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `StatefulSetError` | The wrapped error of whichever child failed to apply or read. A target cluster that grants the operator no `statefulsets` verb lands here | none (error returned) |
| `False` | `StatefulSetProgressing` | "\<n\> of \<m\> sb Raft members are ready" | `RequeueRaftWait` |
| `True` | `StatefulSetReady` | "All \<m\> sb Raft members are ready" | none |

**Error handling:** Every failed apply reports `StatefulSetError` whichever of the
four objects failed. The step is one unit of work, and a reason per object would
put whichever happened to fail first into a field consumers match on. Readiness
is judged on a `Get` after the apply: the counters live on the status
subresource the apply strips out, and the
`observedGeneration` comparison is what tells a converged cluster from one whose
counters still describe the template before the last apply.

### reconcileEndpoints

**File:** `operators/ovn/internal/controller/reconcile_endpoints.go`

**Purpose:** Publish the addresses clients connect to the two databases on. The
internal string lists `ssl:<clusterIP>:<port>` per member in ordinal order; the
external one is assembled only under `spec.<db>.externallyReachable`, as
`ssl:<hostIP>:<nodePortBase + ordinal>`. Both are IP literals, because
`ovsdb-server` resolves a remote once at startup and never again. Services and
pods are read through the target cluster's uncached reader, so an address the
cache has not caught up with is not published as absent.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `EndpointsPending` | "Waiting for every member Service to be assigned a cluster IP and for at least one member of each database to run on a node" | `RequeueRaftWait` |
| `True` | `EndpointsPublished` | "Both databases are reachable at the published addresses" | none |

**Error handling:** A failed read of a Service or a pod sets `EndpointsPending`
and returns the error. Nothing is published while either database is incomplete,
and every address field is cleared on both databases: a client is handed the
member list as one string, so a list missing the member that happens to be the
leader reads to that client as a cluster that cannot serve writes. A member whose
pod is gone is skipped in the node-facing list instead: a rescheduling member has
no node to name, while the members beside it stay reachable.

### reconcileNorthd

**File:** `operators/ovn/internal/controller/reconcile_northd.go`

**Purpose:** Project `ovn-northd`, the daemon that compiles the Northbound
logical model into the Southbound flow table. Every replica connects to both
databases and they coordinate through a lock in the Southbound, so the replica
count buys failover. It buys no throughput. The step stamps
`status.installedImage` once the Deployment is available: northd is the one
process of the three that fails on a Southbound schema it cannot compile against,
so it is what the image is judged by.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `WaitingForEndpoints` | "Waiting for both database addresses to be published" | `RequeueRaftWait` |
| `False` | `DeploymentError` | "ensuring northd Deployment: \<error\>" | none (error returned) |
| `False` | `DeploymentProgressing` | "Waiting for the northd Deployment to become available" | `RequeueDeploymentPolling` |
| `True` | `DeploymentReady` | "The northd Deployment is available" | none |

**Error handling:** A failed apply sets `DeploymentError` and returns the error.
The endpoint gate comes first and returns no error: applying a Deployment with an
empty `--ovnnb-db` would crash-loop the pods until the next pass rewrote the
template.

### reconcileRelay

**File:** `operators/ovn/internal/controller/reconcile_relay.go`

**Purpose:** Project or remove the Southbound relay tier. Relays are stateless
read-through caches in front of the Southbound database, which every chassis
holds an open connection to. With `spec.relay` unset the step deletes the
Deployment and the Service, clears `status.relayAddress` and reports `True`: a
chassis handed a cluster IP whose Service no longer exists waits out its own
timeout instead of falling back to the database it can still reach.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `True` | `RelayNotRequired` | "spec.relay is not set; clients connect to the Southbound database directly" | none |
| `False` | `WaitingForEndpoints` | "Waiting for the Southbound database address to be published" | `RequeueRaftWait` |
| `False` | `DeploymentError` | The wrapped error of the Deployment, the Service, the live read, or the removal | none (error returned) |
| `False` | `ServicePending` | "Waiting for Service \<name\> to be assigned a cluster IP" | `RequeueRaftWait` |
| `False` | `DeploymentProgressing` | "Waiting for the sb-relay Deployment to become available" | `RequeueDeploymentPolling` |
| `True` | `DeploymentReady` | The relay Deployment is available at `status.relayAddress` | none |

**Error handling:** All four failing objects report `DeploymentError` and return
the error, for the reason the database step gives. `ServicePending` is reported
ahead of the Deployment's own state: without an address the relays are
unreachable however many of them are running.

### reconcileBackup

**File:** `operators/ovn/internal/controller/reconcile_backup.go`

**Purpose:** Project the snapshot claim and the backup CronJob, and report on the
newest backup Job that reached a terminal state. Run visibility is derived rather
than watched: the CronJob controller spawns one Job per firing and prunes them by
history limit, so the step lists the Jobs carrying the backup component labels,
keeps the ones this CronJob controls, and reads the newest terminal one. See
[Backup](./ovn-central-crd.md#backup).

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `WaitingForEndpoints` | "Waiting for both database addresses to be published" | `RequeueRaftWait` |
| `False` | `BackupPVCInvalid` | "The API server rejected PersistentVolumeClaim \<name\>: \<error\>" | `RequeueSecretPolling` |
| `False` | `BackupError` | The wrapped error of the claim, the CronJob, or the Job listing | none (error returned) |
| `True` | `BackupSuspended` | The schedule and retention that apply when resumed, plus the last failed run when there was one | none |
| `False` | `BackupJobFailed` | "Backup Job \<name\> failed; the snapshots are no longer current and a restore would lose everything written since the last successful run" | none |
| `True` | `BackupScheduled` | The schedule and the retention window | none |

**Error handling:** An `Invalid` rejection of the snapshot claim, which is what
lowering `spec.backup.storage.size` produces, surfaces the API server's own
message on the condition and returns no error: a retry cannot fix a spec edit
only a human can undo. Suspension outranks a failed run, because a suspended
CronJob spawns no successor to supersede it, so a `BackupJobFailed` arm winning
there would pin the aggregate `Ready` down until someone deleted the Job by hand.
A terminal run feeds the two backup collectors and, on failure, a
`BackupJobFailed` Warning, both deduped on the Job UID through an annotation on
the CR. See [Controller Events](./ovn-events.md).

### reconcileCentral

**File:** `operators/ovn/internal/controller/reconcile_central.go`

**Purpose:** Resolve the `OVNCentral` this chassis attaches to into the four
values the later steps are parameterised by: the Southbound remote
`ovn-controller` dials (the relay when the central runs one, the database
otherwise), the Northbound address the evacuation Job edits the logical model
through, the Southbound address the chassis-deletion Job writes to, and the name
of the client Secret every chassis container mounts. The central CR is read
through the management-cluster client, because both CRs are written by whoever
deploys the control plane and live beside each other whatever cluster their
children land on.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `CentralNotFound` | "OVNCentral \<name\> does not exist in namespace \<ns\>; the chassis stay unconfigured until it does" | `RequeueSecretPolling` |
| `False` | `CentralReadError` | "reading OVNCentral \<ns\>/\<name\>: \<error\>" | none (error returned) |
| `False` | `CentralOnAnotherCluster` | Names the cluster each of the two CRs projects onto | none |
| `False` | `CentralNotReady` | "Waiting for OVNCentral \<name\> to publish its Southbound address and its client Secret" | `RequeueRaftWait` |
| `False` | `CentralUpgrading` | "Waiting for OVNCentral \<name\> to finish rolling out \<image\>; the chassis follow the central, because ovn-controller reads the Southbound schema the central owns" | `RequeueRaftWait` |
| `True` | `CentralResolved` | "The chassis connect to OVNCentral \<name\> at \<address\>" | none |

**Error handling:** A missing `OVNCentral` polls and leaves the pass successful:
an `OVNChassis` applied before its `OVNCentral` is an ordinary ordering of two
objects in one manifest. A cluster mismatch sets the condition and returns
neither an error nor a requeue, because both refs are immutable and only deleting
and reapplying one of the two CRs can repair it. The upgrade gate compares the
central's `status.installedImage` against the image it resolves, so it holds only
while the central's own rollout is in flight. The chassis image is not compared
against it: a chassis pinned to an older image than the central is the direction
OVN supports, and a gate demanding equality would wedge it.

### reconcileNodes

**File:** `operators/ovn/internal/controller/reconcile_nodes.go`

**Purpose:** Render one entry per node into the `{name}-nodes` ConfigMap, apply
the `{name}-chassis-scripts` ConfigMap, and rebuild `status.nodes`. The chassis
pod has no API client of its own, so the rendered entry is the whole channel
between the operator and a node: `SYSTEM_ID`, `GATEWAY`, `BRIDGE_MAPPINGS`,
`ENCAP_TYPE`, and `LEAVING` when the node is on its way out. Nodes are listed
with `spec.nodeSelector` applied by the API server, through the target cluster's
uncached reader.

A node's system-id is read back from the live ConfigMap first and from
`status.nodes` second, and a fresh UUID is minted only when neither holds one
that matches the UUID grammar. An identity that changes leaves the previous
registration behind in the Southbound database, where it keeps claiming the ports
of the workloads running on that very node. A value that is not a UUID is
discarded, never rendered back out: the node sources that file under bash, where
a `$(...)` in a value runs as a command.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `NodeListError` | "listing nodes: \<error\>" | none (error returned) |
| `False` | `NodesError` | "reading the \<name\>-nodes ConfigMap: \<error\>", or "ensuring \<name\> ConfigMap: \<error\>" | none (error returned) |
| `False` | `NoMatchingNodes` | "No node matches spec.nodeSelector \<selector\>" | none |
| `True` | `NodesRendered` | "Rendered \<n\> node entries (\<m\> leaving)" | none |

**Error handling:** Both failure reasons set the condition and return the error,
so the controller backs off. An empty selection is reported without a requeue:
the Node watch wakes the CR when a node is labelled, and polling for one would
add nothing. Both ConfigMaps are applied even when nothing is selected, because a
pod whose ConfigMap volume does not exist never starts.

**Drain semantics.** A node that stops matching `spec.nodeSelector`, or that
leaves the cluster, keeps its entry with `LEAVING=true`. The mappings and the
encapsulation are re-rendered from the spec, never carried over from the live
ConfigMap, so everything reaching the file a node sources comes from a source
admission validated. The entry survives until the chassis-deletion Job has
succeeded, at which point the maintenance step drops the ConfigMap key and the
status entry together.

### reconcileOVS

**File:** `operators/ovn/internal/controller/reconcile_ovs.go`

**Purpose:** Project the `{name}-ovs` DaemonSet: `ovsdb-server` holding the node's
configuration database, `ovs-vswitchd` driving the datapath, and the privileged
`host-prepare` init container that loads the kernel modules and prepares the run
directories. It is the step `ovn-controller` depends on, because the local
database is where a node's chassis configuration is written. Neither container
has a liveness probe: `ovs-vswitchd` owns the kernel datapath every workload on
the node forwards through, so restarting it for a fault the readiness probe
already reports would turn a degraded node into a disconnected one.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `DaemonSetError` | "ensuring ovs DaemonSet: \<error\>" | none (error returned) |
| `False` | `DaemonSetProgressing` | "Waiting for the ovs DaemonSet: \<n\> of \<m\> nodes run a ready pod" | `RequeueDeploymentPolling` |
| `True` | `DaemonSetReady` | "The ovs DaemonSet runs a ready pod on \<m\> nodes" | none |

**Error handling:** A failed apply or read sets `DaemonSetError` and returns the
error. A DaemonSet that selects no node is ready on zero nodes: its rollout has
nothing left to do, and reporting the CR unready until somebody labels a node
would make an empty selection look like a stuck one.

**preStop:** `ovsdb-server` runs `ovs-appctl -t /run/openvswitch/ovsdb-server.ctl
exit` and `ovs-vswitchd` runs `ovs-appctl -t
/run/openvswitch/ovs-vswitchd.ctl exit`. Neither carries `--cleanup`: that flag
tears the kernel datapath down, and the node would stop forwarding for the whole
time it takes the replacement pod to start. Leaving the datapath in place keeps
the flows valid across the restart, and the new `ovs-vswitchd` adopts it.

### reconcileController

**File:** `operators/ovn/internal/controller/reconcile_controller.go`

**Purpose:** Project the `{name}-ovn-controller` DaemonSet: the `apply-node` init
container that writes this node's values into the local Open vSwitch database,
and the daemon that turns the Southbound logical flows into datapath flows. The
step mirrors `desiredNumberScheduled` and `numberReady` from this DaemonSet
and not from the Open vSwitch one: `ovn-controller` is what makes a node a
chassis. `status.installedImage` is stamped on the ready arm alone, so a
rollout that has reached no node yet leaves the previous value in place.

The Southbound address is not passed on the command line. `ovn-controller` reads
it from the local database, where the init container wrote it, so a changed
remote reaches a running chassis without a pod restart. Readiness is the
Southbound connection: the probe greps `ovn-appctl connection-status` for
`connected`, so a live process with a dead connection stays unready. A chassis
that cannot reach the database serves stale flows and must not count as a node
the rollout may move on from.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `DaemonSetError` | "ensuring ovn-controller DaemonSet: \<error\>" | none (error returned) |
| `False` | `DaemonSetProgressing` | "Waiting for the ovn-controller DaemonSet: \<n\> of \<m\> nodes run a ready pod" | `RequeueDeploymentPolling` |
| `True` | `DaemonSetReady` | "The ovn-controller DaemonSet runs a ready pod on \<m\> nodes" | none |

**Error handling:** as `reconcileOVS`. The counters are mirrored before the
readiness branch, so a progressing DaemonSet still reports what it has.

**preStop:** `ovn-appctl -t /run/ovn/ovn-controller.ctl exit --restart`. The
restart flag leaves the datapath flows in place and keeps the Southbound
`Chassis` row, so the successor pod takes both over instead of re-registering the
node and waiting for the flows to be recomputed.

### reconcileMaintenance

**File:** `operators/ovn/internal/controller/reconcile_maintenance.go`

**Purpose:** Move every node through whatever step of its lifecycle is due. It
compares the entry the node step rendered against the entry the pass read out of
status before that rewrite: the rendered one says what a node should be running,
the recorded one what it was last known to run. Nodes are walked in name order,
and a node whose Job is still running does not hold the others up. A failure does
stop the walk, because it needs an operator and not another Job.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `True` | `MaintenanceDeferred` | "Waiting for OVNCentral \<name\> to publish the address the maintenance of \<nodes\> needs" | `RequeueRaftWait` |
| `True` | `MaintenanceRunning` | "Maintenance Jobs are running for \<n\> nodes: \<nodes\>" | `RequeueRaftWait` |
| `True` | `MaintenanceIdle` | "No node has maintenance outstanding" | none |
| `False` | `MaintenanceJobFailed` | "The \<kind\> Job \<name\> for node \<node\> failed; inspect its pod logs" | none |
| `False` | `MaintenanceError` | The wrapped error of a Job run or of the ConfigMap rewrite | none (error returned) |

**Error handling:** A permanently failed Job is reported on the condition and
never returned as an error. A rerun under an unchanged key produces the same
failure, so a returned error would only put the CR on the workqueue's backoff and
hot-loop a pass that cannot help.
The Warning that dates the failure rides the shared terminal-state helper, which
dedupes on the Job UID. A deferred node outranks a running one in the message,
because a Job that runs finishes on its own while a node waiting for an address
needs the other CR to move; both poll at the same interval.

**Drain semantics.** Three Jobs cover the three transitions, each named
`{name}-<kind>-<8 hex>` after the SHA-256 of the node name, each with
`backoffLimit: 0`:

| Trigger | Job | What it does |
| --- | --- | --- |
| The rendered entry hashes differently than `status.nodes[].configHash`, and that hash is not empty | `apply` | Reruns `apply-node.sh` on the node, pinned to it and in its network namespace. A node seen for the first time runs no Job: its own init container applies the values |
| The node lost the gateway role (`prev.gateway` and not `entry.gateway`) and `gatewayEvacuated` is not set | `evacuate` | Runs `lrp-del-gateway-chassis` for every logical router port and `ha-chassis-group-remove-chassis` for every HA group, against the Northbound database. Each loop collects its rows first and submits every removal as one `ovn-nbctl` call |
| The entry is marked `leaving` | `chassis-del` | Runs `chassis-del` against the Southbound database, then the step drops the node's ConfigMap key and its `status.nodes` entry together |

The apply runs before the evacuation: the chassis has to stop announcing itself
as a gateway before the bindings are taken off it, or the model would hand them
back while the node still claims the role. `status.nodes[].gateway` is held at
`true` until the evacuation lands, so the trigger survives the pass that started
it. `gatewayEvacuated` is set when the Job succeeds and reset as soon as the node
takes the gateway role back, so a re-promoted node is not treated as still
drained. A node whose Job needs an address the `OVNCentral` has not published is
deferred until it is.

Deleting an `OVNChassis` runs none of this. The teardown removes the
configuration of a node, not the node itself: the DaemonSet pods go, and
with them `ovn-controller`, while the Southbound `Chassis` rows they registered
stay behind, the same way deleting a Deployment leaves the rows its pods wrote
(`ovnchassis_controller.go`). Drop the nodes out of `spec.nodeSelector` and let
the `chassis-del` Jobs run before deleting the CR.

## Requeue semantics

| Constant | Value | Used by |
| --- | --- | --- |
| `RequeueRaftWait` (`requeue_intervals.go`) | 15s | Every OVN wait state: a Raft cluster that has not elected a leader, a member endpoint without an address, a central that has not published one, and a maintenance Job that is still running |
| `RequeueSecretPolling` (`internal/common/reconcile/intervals.go`) | 15s | cert-manager polling in the TLS step, the missing-`OVNCentral` poll, the rejected snapshot claim, and the target-cluster hold on both controllers |
| `RequeueDeploymentPolling` (`internal/common/reconcile/intervals.go`) | 10s | Deployment and DaemonSet readiness polling: northd, the relay, and the two chassis DaemonSets |
| `RequeueNextPass` (`internal/common/reconcile/intervals.go`) | 1s | The single pass after the remote-children finalizer was added, so the next reconcile observes the persisted finalizer and not the in-memory copy |

None of the wait states reaches the workqueue as an error, so these intervals are
what paces the retries. A returned error goes on the controller's own backoff
instead, capped at 30 seconds by the shared rate limiter
(`bootstrap.TypedControllerOptions`).

## Watches

Both controllers filter their CR's status-only updates through
`watch.CRUpdatePredicate`, so a status write does not re-wake the controller, and
both turn the multicluster builder's cluster-not-found wrapper off: an
unresolvable target cluster is surfaced as `TargetClusterUnavailable` and
requeued, where the wrapper would swallow it as a successful reconcile.

The `OVNCentral` controller `Owns` its StatefulSet, Deployment, Service,
ConfigMap, PersistentVolumeClaim, CronJob and Job. The cert-manager Certificate
joins that set only when the kind is present on the management cluster, probed at
setup through the RESTMapper. An unconditional `Owns(Certificate)` would fail at
start with "no matches for kind Certificate", which takes down every controller
in the binary. `reconcileTLS` reports the missing kind on the CR as
`CertManagerUnavailable`.

The `OVNChassis` controller `Owns` its DaemonSet, ConfigMap and Job, and adds two
watches:

- **OVNCentral**, mapped to every `OVNChassis` attached to it through the
  `spec.centralRef` field index, scoped to the central's namespace. The leg
  carries no generation predicate: what the chassis wait on are the central's
  status flips, and those leave the generation untouched. The index is registered
  by the `OVNCentral` controller's setup, which `main.go` runs first.
- **Node**, under `predicate.LabelChangedPredicate`, mapped to every `OVNChassis`
  the cache holds across all namespaces. A node's labels are what every CR's
  selector is evaluated against, and the CR that has to re-render is as often the
  one that just lost the node as the one that gained it, so there is no index to
  narrow the fan-out through. The predicate narrows the other side instead, which
  is what keeps the kubelet's status heartbeat off the leg.

Both controllers watch their children a second time on the clusters a CR can
project onto (`AddRemoteChildWatches`). An owner reference does not cross a
cluster boundary, so the ownership labels are what map a remote child back to its
CR. The Node watch is registered on both sides too, through `AddInputWatch`: the
management cluster's nodes for a chassis that keeps its children local, and the
target clusters' for a placed one. Legs on a target cluster are engaged on all of
them, so they drop the events belonging to a CR that projects somewhere else. The
field index stays on the local field indexer, because it is an index on a CR kind
and no target cluster holds one.
