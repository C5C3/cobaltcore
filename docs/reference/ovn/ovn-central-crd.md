---
title: OVNCentral CRD
quadrant: operator
---

# OVNCentral CRD

`ovncentrals.ovn.openstack.c5c3.io/v1alpha1`, kind `OVNCentral`. The CRD is
generated from `operators/ovn/api/v1alpha1/ovncentral_types.go`; the defaulting
and validating webhooks live in
`operators/ovn/api/v1alpha1/ovncentral_webhook.go`.

One CR describes the whole OVN control plane: the Northbound and Southbound Raft
databases, the northd daemon that translates between them, an optional
Southbound relay tier, the certificates every connection is authenticated with,
and the recurring database backup. The two databases share one CR because northd
only works against both of them and their Raft clusters have to be sized
together.

`kubectl get ovncentrals` prints Ready
(`.status.conditions[?(@.type=='Ready')].status`), NB
(`.status.northbound.readyReplicas`), SB (`.status.southbound.readyReplicas`),
and Age.

## Spec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | no | operator-resolved `ghcr.io/c5c3/ovn:26.03.2` | The image that runs `ovsdb-server`, northd, and the relay. All three ship in the one OVN image, so one reference covers them. The operator resolves it at reconcile time, so an unset field keeps tracking its tested version across upgrades (`image.go`) |
| `northbound` | [`OVNDatabaseSpec`](#ovndatabasespec) | no | `{}` | The Northbound database, the one the CMS writes the logical network model into |
| `southbound` | [`OVNDatabaseSpec`](#ovndatabasespec) | no | `{}` | The Southbound database, the one northd writes translated flows into and every chassis reads from. It is the busier of the two, which is why it can be fronted by a relay |
| `northd` | [`OVNNorthdSpec`](#ovnnorthdspec) | no | `{}` | The `ovn-northd` daemon that compiles the Northbound model into Southbound flows |
| `relay` | [`*OVNRelaySpec`](#ovnrelayspec) | no | `nil` | Fronts the Southbound database with `ovsdb-server` relays. Every chassis holds an open Southbound connection, so past a few hundred nodes the read load is what limits the cluster. When nil the chassis connect to the database directly |
| `tls` | [`OVNTLSSpec`](#ovntlsspec) | yes | — | The cert-manager issuer every OVN certificate is requested from. Required: the databases carry the entire logical network model, so an unauthenticated listener would let any pod that reaches the port rewrite the network |
| `backup` | [`*OVNBackupSpec`](#ovnbackupspec) | no | `nil` | Tunes the recurring database backup. A nil block still gets a CronJob; it only means every setting resolves to the operator default. See [Backup](#backup) |
| `targetClusterRef` | [`*commonv1.TargetClusterRefSpec`](../target-clusters.md#the-field) | no | `nil` (the local cluster) | The registered target cluster the children are created on. The CR itself, its status and its finalizer stay on the management cluster. Immutable, enforced by two CEL transition rules and by the webhook. See [Target Clusters](../target-clusters.md) |

### OVNDatabaseSpec

Configures one of the two Raft databases. Three fields are frozen after creation
by CEL transition rules, because they are baked into the cluster when its first
member creates the database file. See
[Defaulting and validation](#defaulting-and-validation).

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `replicas` | `int32` (Minimum=1, Maximum=5) | no | `3` | The number of Raft members. It must be odd: an even cluster tolerates no more failures than the odd one below it and has two ways to split the vote. Immutable |
| `storage` | [`OVNStorageSpec`](#ovnstoragespec) | no | `{}` | Sizes the per-member claim holding the database file and its Raft log. Immutable, because a StatefulSet volume claim template cannot change |
| `externallyReachable` | `bool` | no | `false` | Publishes every member on a node port, making the database reachable on the IP of every node. The client this exists for is an `OVNChassis` on a node without cluster networking, which dials the Southbound database alone |
| `nodePortBase` | `*int32` (Minimum=30000, Maximum=32767) | no | `30641` for `northbound`, `30651` for `southbound` | The first node port of this database's range. Member `i` is published on `nodePortBase + i`, because a Raft client has to address the individual members |
| `electionTimerMs` | `int32` (Minimum=1000, Maximum=180000) | no | `1000` | How long a follower waits without hearing from the leader before it starts an election. Written into the database when it is created, so it is immutable through this field |
| `inactivityProbeMs` | `int32` (Minimum=0) | no | `60000` | How long `ovsdb-server` lets a client connection sit idle before probing it. Zero disables the probe, which is what a client behind a connection-tracking middlebox needs when the probe is what tears the connection down |
| `resources` | `*corev1.ResourceRequirements` | no | none | Requests and limits for the `ovsdb` container. When nil the container is built with empty requirements, so the pod lands in the BestEffort QoS class; the shared 100m/500m CPU and 256Mi/512Mi memory defaults apply to the northd and relay Deployments, not here |

### OVNStorageSpec

Sizes a PersistentVolumeClaim. Shared by the two database members and by the
backup volume, which have the same two knobs.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `size` | `string` (Pattern `^[0-9]+(Mi\|Gi\|Ti)$`) | no | `1Gi` | The requested volume size. The pattern admits binary units only: a decimal `1G` differs from `1Gi` by enough to matter on a volume this small |
| `storageClassName` | `*string` | no | `nil` | Selects the StorageClass. When nil the cluster's default class is used |

### OVNNorthdSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `deployment` | [`commonv1.DeploymentSpec`](../keystone/keystone-crd.md#deploymentspec) | no | `{}` | The pod-level knobs of the northd Deployment: `replicas` (default 3), `resources` (100m/500m CPU, 256Mi/512Mi memory), `terminationGracePeriodSeconds`, `preStopSleepSeconds`, `strategy`, `topologySpreadConstraints`, `priorityClassName`. Three northd pods are one active instance and two standbys, so the count sizes failover |
| `threads` | `int32` (Minimum=1, Maximum=16) | no | `1` | Parallel logical-flow computation threads. Past a handful the lock contention inside northd eats the gain, so the ceiling stays low |

### OVNRelaySpec

Relays are stateless caches, so they scale independently of the Raft cluster
behind them. The block has no `deployment` field: there is nothing to drain and
no rollout ordering to respect, so the operator applies the shared deployment
defaults to the two knobs below.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `replicas` | `int32` (Minimum=1) | yes | — | The number of relay pods. Unlike the database replicas this is a plain scaling knob with no odd-count or immutability constraint |
| `resources` | `*corev1.ResourceRequirements` | no | 100m/500m CPU, 256Mi/512Mi memory | Requests and limits for the relay container |

### OVNTLSSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `issuerRef` | [`OVNIssuerRef`](#ovnissuerref) | yes | — | The issuer. It must be CA-capable: OVN authenticates peers against the issuing CA certificate, so an issuer that cannot expose one (ACME, for instance) produces certificates the databases reject |

The operator sets no `role=` on the OVSDB connection rows, so every certificate
this issuer signs has unrestricted read and write on both databases. Do not
share the issuer with workloads outside this control plane.

### OVNIssuerRef

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` (MinLength=1) | yes | — | The cert-manager issuer's name |
| `kind` | `string` (Enum `Issuer`, `ClusterIssuer`) | no | `ClusterIssuer` | The issuer scope. `ClusterIssuer` is the default because the OVN CA is normally shared with the chassis namespaces, which a namespaced `Issuer` cannot reach |

### OVNBackupSpec

The snapshot volume is a child of the `OVNCentral` like any other, so deleting
the CR deletes it along with the database volumes. Configure `s3` for a copy
that outlives the CR.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `schedule` | `string` | no | `0 2 * * *` | The cron expression the backup runs on, resolved at reconcile time when empty. A stated expression is parsed at admission with the library the CronJob controller uses |
| `retentionDays` | `*int32` (Minimum=1) | no | `14` | How long a snapshot is kept before a later run deletes it. Shortening the window is echoed back as an admission warning |
| `suspend` | `bool` | no | `false` | Stops the CronJob from firing without deleting it or the snapshots already taken. The switch to reach for during a maintenance window that would otherwise snapshot a half-migrated database |
| `storage` | [`OVNStorageSpec`](#ovnstoragespec) | no | `{}` | Sizes the claim the snapshots are written to |
| `s3` | [`*OVNBackupS3Spec`](#ovnbackups3spec) | no | `nil` | Copies each snapshot off-cluster. Without it the snapshots share the fate of the cluster that holds them |

### OVNBackupS3Spec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `bucket` | `string` (MinLength=1) | yes | — | The target bucket name |
| `prefix` | `string` | no | `""` | The key prefix each snapshot is written under, so one bucket can hold the backups of several deployments |
| `endpoint` | `string` (Pattern `^https://`) | yes | — | The S3 service URL. HTTPS only: the upload carries the access key beside a full snapshot of both databases, and SigV4 authenticates a request without encrypting it |
| `region` | `string` | no | `""` | The S3 region. Optional because most S3-compatible implementations ignore it |
| `credentialsSecretRef` | [`commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | yes | — | The Secret holding the access key under the keys `access-key-id` and `secret-access-key`. It lives in the `OVNCentral`'s own namespace |
| `image` | [`*commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | no | operator-resolved `ghcr.io/c5c3/backup-shifter:latest` | The image the upload step runs. See the warning in [Backup](#backup) |

## Defaulting and validation

The mutating webhook leaves the object untouched. Every default is either a
`+kubebuilder:default` the API server applies from the CRD schema, or a value
the operator resolves at reconcile time: the image, the two node-port bases, and
the backup schedule and retention. Resolving those four late keeps an unset
field tracking the operator default across upgrades instead of freezing today's
value into the stored CR. The webhook stays registered so a default that has to
be materialized later can be added without changing the deployed webhook
configuration.

`ovncentral_webhook.go` holds the constants those resolutions read:
`DefaultBackupSchedule` is `0 2 * * *`, `DefaultBackupRetentionDays` is 14,
`DefaultNorthboundNodePortBase` is 30641 and `DefaultSouthboundNodePortBase` is
30651. The two bases carry their database's OVSDB port in the last two digits,
and sit ten apart so both ranges reach the five-replica ceiling without
colliding.

### Schema-layer rules

These hold even when the webhook is down.

| Message | Where it comes from |
| --- | --- |
| `targetClusterRef is immutable` | Two CEL transition rules on `OVNCentralSpec`, one for adding or removing the ref and one for renaming it |
| `replicas is immutable: Raft membership changes are not supported` | Transition rule on `OVNDatabaseSpec`. A membership change needs an `ovsdb-tool`/`ovs-appctl` procedure against the running cluster that the operator does not perform |
| `electionTimerMs is immutable: it is applied when the clustered database is created` | Transition rule on `OVNDatabaseSpec` |
| `storage is immutable: volumeClaimTemplates cannot change` | Transition rule on `OVNDatabaseSpec`. The API server refuses the StatefulSet update anyway; rejecting it here reports the constraint at admission |
| `replicas must be odd` | Field rule on `OVNDatabaseSpec.replicas` |
| `exactly one of image.tag or image.digest must be set` | Inherited from `commonv1.ImageSpec`, on `spec.image` and on `spec.backup.s3.image` |
| `preStopSleepSeconds must be strictly less than terminationGracePeriodSeconds` | Inherited from `commonv1.DeploymentSpec` on `spec.northd.deployment`; the rule substitutes the effective defaults 5 and 30 for an unset pointer |

Each of the three `OVNDatabaseSpec` transition rules is guarded by a `has()`
check on both sides. The API server evaluates every rule of the type against the
empty-object default the two referencing fields carry, and that object has none
of the three keys, so an unguarded rule makes the whole CRD unapplicable. The
guard changes nothing for a stored CR: all three keys carry a schema default, so
admission has filled them long before an update can reach these rules.

### Webhook rules

The validating webhook accumulates every violation into one admission response.

| Message | Trigger |
| --- | --- |
| `name must be at most %d characters: the backup CronJob appends %q and Kubernetes caps CronJob names at %d characters` | `metadata.name` longer than `MaxOVNCentralNameLength`. The three arguments are the bound (45), the suffix (`-backup`), and `MaxCronJobNameLength` (52) |
| `issuerRef.name must be set` | `spec.tls.issuerRef.name` is empty |
| `repository must be set` | `spec.image.repository` or `spec.backup.s3.image.repository` is empty |
| `exactly one of image.tag or image.digest must be set` | Both or neither are set on either image reference, re-checked outside the schema |
| `nodePortBase %d leaves no room for %d replicas below %d` | A base whose range runs past 32767. The arguments are the effective base, the effective replica count, and the ceiling. The last member's Service would be rejected by the API server, leaving that member unreachable from outside the cluster |
| `northbound and southbound nodePort ranges overlap` | The two ranges intersect. Each runs over as many consecutive ports as there are members, so two bases that look far apart still collide once both databases are scaled up |
| `retentionDays must be at least 1` | `spec.backup.retentionDays` below 1, alongside the `Minimum=1` marker. Zero would delete every snapshot the run just took |
| `invalid cron expression: %v` | `spec.backup.schedule` is non-empty and `cron.ParseStandard` refuses it. The argument is the parser's own error. An empty schedule is not parsed, since it resolves the operator default |
| `credentialsSecretRef.name must be set` | `spec.backup.s3` is set with no credentials Secret named |
| `target cluster name must be set` | `spec.targetClusterRef` is present with an empty `name` |
| `targetClusterRef is immutable (adding or removing it after creation is not permitted)` | An update adds or drops the ref. Both strand the children already created on the previously selected cluster |
| `targetClusterRef is immutable (the children already exist on the previously named cluster)` | An update renames the ref |

The name bound is `MaxOVNCentralNameLength`, computed as `MaxCronJobNameLength`
minus `len("-backup")`, which is 45 characters. It is enforced on create only.
`metadata.name` is immutable, so on update the rule could only fire against an
object a pre-upgrade operator already admitted, and the validating webhook also
sees the finalizer-removal update that completes a deletion: rejecting that one
would wedge the CR in `Terminating` with no field left to edit.

One update raises a warning instead of a rejection:

```text
spec.backup.retentionDays reduced %d → %d: the next backup run deletes the snapshots taken between %d and %d days ago, which cannot be undone
```

The effect is immediate and irreversible, and a typo (3 for 30) is
indistinguishable from an intended change at admission time. Shortening the
window is a legitimate operational choice, so it stays a warning. The comparison
runs against the resolved retention on both sides, so dropping `spec.backup`
entirely does not read as a reduction to nothing.

The shared deployment validators the service operators run (priority-class
existence, topology-spread selectors, requests against limits) have no
counterpart here. `spec.northd.deployment` is checked by its inherited CEL rule
alone.

## Status

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | List-map keyed by `type`; see [Conditions](#conditions) |
| `observedGeneration` | `int64` | The `.metadata.generation` the controller last reconciled |
| `northbound` | [`OVNDatabaseStatus`](#ovndatabasestatus) | The observed state of the Northbound database |
| `southbound` | [`OVNDatabaseStatus`](#ovndatabasestatus) | The observed state of the Southbound database |
| `relayAddress` | `string` | The Southbound relay Service, `ssl:<clusterIP>:6642`. Set while `spec.relay` is set and cleared when the relay is removed |
| `clientSecretName` | `string` | The Secret holding the client certificate every OVN client authenticates with (`tls.crt`, `tls.key`, `ca.crt`). An `OVNChassis` mounts it, so this is the field that connects the two kinds |
| `installedImage` | `string` | The image reference the running control plane was projected from, recorded once northd runs on it. It tells a rollout that has not reached the pods from one that has |

### OVNDatabaseStatus

| Field | Type | Description |
| --- | --- | --- |
| `internalDbAddress` | `string` | The connection string for clients inside the cluster, one entry per member |
| `dbAddress` | `string` | The connection string for clients outside the cluster. Empty unless `externallyReachable` is set |
| `readyReplicas` | `int32` | The number of Raft members that are ready. A cluster keeps serving writes while a minority is down, so this is a health signal |

### Address computation

`reconcile_endpoints.go` assembles both strings from the per-member Services and
pods, read through the target cluster's uncached reader.

The internal address lists `ssl:<clusterIP>:<port>` per member in ordinal order,
joined with commas, at port 6641 for the Northbound and 6642 for the Southbound
database. The external address is assembled only under
`spec.<db>.externallyReachable`, as `ssl:<hostIP>:<nodePortBase + ordinal>` per
member, from the `HostIP` of the member's pod.

Both are IP literals, never DNS names. `ovsdb-server` resolves a remote once at
startup and never again, so a name whose address changes leaves the client
wedged against the old one.

A member whose pod is gone is skipped in the node-facing list: a rescheduling
member has no node to name, while the members beside it are still reachable. A
member whose Service is missing or carries no cluster IP stops the whole
database, and so does a published database on which no member has a node address
at all. While either database is in that state every address field is cleared,
on both databases, and `EndpointsReady` reports `EndpointsPending`. A client is
handed the member list as one string, so a list missing the member that happens
to be the leader reads as a cluster that cannot serve writes rather than as one
whose address is still being assembled.

### Conditions

Seven sub-reconcilers each own one condition type. The aggregate `Ready` is
`True` only when all seven are.

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `TLSReady` | True | `CertificatesIssued` | Both server certificates and the client certificate are issued, and the client Secret carries `tls.crt`, `tls.key`, and `ca.crt` |
| `TLSReady` | False | `CertificatePending` | cert-manager has not issued one of the Certificates yet, or has not written the client Secret |
| `TLSReady` | False | `CertificateError` | A Certificate could not be applied, the client Secret could not be read, or it is missing a key. A missing `ca.crt` names its cause: an issuer that is not CA-backed |
| `TLSReady` | False | `CertManagerUnavailable` | The cluster the children land on does not serve the `cert-manager.io/v1` Certificate kind. A wait, not an error: `spec.tls` is required, so there is nothing to fall back to |
| `TLSReady` | False | `CapabilityProbeFailed` | The probe for that kind failed against the target cluster's RESTMapper |
| `TLSReady` | False | `TargetClusterUnavailable` | `spec.targetClusterRef` names a cluster that does not resolve. TLS is the pipeline's first gate, so the failure lands on the condition the rest of the graph waits behind |
| `NorthboundReady`, `SouthboundReady` | True | `StatefulSetReady` | Every Raft member of that database is ready |
| `NorthboundReady`, `SouthboundReady` | False | `StatefulSetProgressing` | Fewer members are ready than `replicas`, or the StatefulSet's counters still describe the template before the last apply. The message counts the ready members |
| `NorthboundReady`, `SouthboundReady` | False | `StatefulSetError` | A child of that database could not be applied or read; the message carries the wrapped error. A target cluster that grants the operator no `statefulsets` verb lands here |
| `EndpointsReady` | True | `EndpointsPublished` | Both databases are reachable at the published addresses |
| `EndpointsReady` | False | `EndpointsPending` | A member Service has no cluster IP yet, no member of a published database runs on a node, or a read failed |
| `NorthdReady` | True | `DeploymentReady` | The northd Deployment is available |
| `NorthdReady` | False | `DeploymentProgressing` | The Deployment is rolling out |
| `NorthdReady` | False | `DeploymentError` | The Deployment could not be applied |
| `NorthdReady` | False | `WaitingForEndpoints` | One of the two database addresses is not published yet. northd is configured with both, so applying a Deployment with an empty `--ovnnb-db` would crash-loop the pods |
| `RelayReady` | True | `RelayNotRequired` | `spec.relay` is not set; clients connect to the Southbound database directly |
| `RelayReady` | True | `DeploymentReady` | The relay Deployment is available at `status.relayAddress` |
| `RelayReady` | False | `DeploymentProgressing` | The relay Deployment is rolling out |
| `RelayReady` | False | `DeploymentError` | The relay Deployment, its Service, or its removal failed |
| `RelayReady` | False | `ServicePending` | The relay Service has no cluster IP yet. Reported ahead of the Deployment's own state: without an address the relays are unreachable however many are running |
| `RelayReady` | False | `WaitingForEndpoints` | The Southbound address is not published yet |
| `BackupReady` | True | `BackupScheduled` | The CronJob is in place and the most recent terminal run did not fail |
| `BackupReady` | True | `BackupSuspended` | `spec.backup.suspend` is set. It outranks a failed run, because a suspended CronJob spawns no successor to supersede it; the failure is still named in the message |
| `BackupReady` | False | `BackupJobFailed` | The newest terminal backup Job failed, so a restore would lose everything written since the last successful run |
| `BackupReady` | False | `BackupPVCInvalid` | The API server rejected the snapshot claim, which is what lowering `spec.backup.storage.size` produces. No error is returned: only a spec edit can undo it |
| `BackupReady` | False | `BackupError` | The claim, the CronJob, or the Job listing failed |
| `BackupReady` | False | `WaitingForEndpoints` | Both database addresses reach the run as environment variables, and one is not published yet |
| `Ready` | True | `AllReady` | All seven sub-conditions are True |
| `Ready` | False | `NotAllReady` | At least one is not |

northd, the relay, and the backup read the published addresses and nothing of
each other's output, so they run as a parallel group after the endpoint step.

## Sub-Resource Naming Convention

Every child takes the CR name plus a component suffix. For an `OVNCentral` named
`{name}`:

| Resource | Name | Notes |
| --- | --- | --- |
| StatefulSet | `{name}-nb`, `{name}-sb` | One per database. Each shares its name with the headless Service that gives its members their stable DNS names, because a StatefulSet derives the per-pod names from its `serviceName` |
| Pod | `{name}-nb-0`, `{name}-nb-1`, … and `{name}-sb-0` upward | One per Raft member, in ordinal order |
| Service (per member) | `{name}-nb-0` upward, matching the pod | ClusterIP on 6641 (NB) or 6642 (SB); `NodePort` at `nodePortBase + ordinal` under `externallyReachable`. A Raft client addresses the members individually, so a single load-balanced Service would send half the writes to a follower |
| PersistentVolumeClaim | `db-{name}-nb-0` upward | From the `db` volume claim template |
| Secret | `{name}-nb-server`, `{name}-sb-server` | The server keypair each database's members listen with, written by cert-manager under the Certificate's name |
| ConfigMap | `{name}-central-scripts` | The run and set-connection scripts of both databases plus the backup script. The backup shares it so the snapshot script cannot drift from the run scripts |
| Deployment | `{name}-northd` | northd. It has no Service: the daemon connects out to both databases and nothing connects to it |
| Deployment, Service | `{name}-sb-relay` | Only while `spec.relay` is set. The relay's own keypair is issued under the same name |
| Secret | `{name}-client` | The keypair every OVN client authenticates with, published as `status.clientSecretName` |
| PersistentVolumeClaim, CronJob | `{name}-backup` | The snapshot volume and the CronJob that writes to it, under one name |

The Raft ports are fixed: the Northbound database serves clients on 6641 and
replicates on 6643, the Southbound serves on 6642 and replicates on 6644.

An `OVNCentral` name longer than 45 characters is refused on create, so the
plain `{name}-backup` form always fits. A CR that a pre-upgrade operator
admitted above the bound keeps working: the CronJob name then collapses onto a
content-stable hash, so the apply keeps succeeding.

## Backup

The CronJob snapshots both databases with `ovsdb-client` and keeps the snapshots
on the `{name}-backup` claim. A Raft cluster survives the loss of a minority of
its members but not an operator error applied to all of them, so these snapshots
are the only path back from a corrupted logical model.

The schedule and the retention resolve at reconcile time (`reconcile_backup.go`,
constants in `ovncentral_webhook.go`): `0 2 * * *` and 14 days when
`spec.backup` leaves them unset. A nil block behaves like an empty one, so every
`OVNCentral` gets a backup.

Each run writes `nb-<timestamp>.backup` and `sb-<timestamp>.backup` under
`/backup`, where the timestamp is `date -u +%Y%m%dT%H%M%SZ`, for example
`nb-20260906T020000Z.backup`. The script writes to a `.tmp` file first and
renames it only after `ovsdb-client` exits zero with a non-empty result, so a
run that dies mid-copy leaves nothing that looks like a snapshot. A volume that
filled up lets `ovsdb-client` exit zero with nothing written, and a zero-byte
file restores no database, which is why an empty result fails the run. Retention
prunes `*.backup` files older than `retentionDays`, sweeps `*.backup.tmp`
leftovers older than a day, and deletes any zero-byte file it still finds.

With `spec.backup.s3` set, the snapshot moves into an init container and a
shifter container copies `/backup` to the bucket with `rclone` once it has
finished. The shifter mounts the volume read-only: the retention window is the
snapshot container's to enforce, and an upload that could delete would make a
misconfigured prefix destructive. rclone is configured entirely through
environment variables, with the access key read from `credentialsSecretRef`.

::: warning
`spec.backup.s3.image` defaults to `ghcr.io/c5c3/backup-shifter:latest`
(`image.go`). The tag is mutable and kubelet defaults `imagePullPolicy` to
`Always` for `:latest`, so every firing runs whatever the last merge pushed,
with the S3 credentials in its environment and both database snapshots on its
volume. Pin the field to a digest to opt out.
:::

Restoring a snapshot is a manual procedure: see
[Restore an OVN database snapshot](../../guides/ovn/restore-an-ovn-database-snapshot.md).

## Example

```yaml
apiVersion: ovn.openstack.c5c3.io/v1alpha1
kind: OVNCentral
metadata:
  name: ovn-basic
  namespace: openstack
spec:
  tls:
    issuerRef:
      name: openstack-ovn-ca-issuer
  northbound:
    externallyReachable: true
  southbound:
    externallyReachable: true
  northd:
    deployment:
      replicas: 1
```

This is the fixture the `central-basic-deployment` suite applies
(`tests/e2e/ovn/central-basic-deployment/00-ovncentral-cr.yaml`). Both databases
keep the default three Raft members, and `spec.image` is left unset so the
operator resolves its own OVN image.
