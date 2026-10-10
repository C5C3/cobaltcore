---
title: CinderBackend CRD API Reference
quadrant: operator
---

# CinderBackend CRD API Reference

Reference documentation for the CinderBackend Custom Resource Definition. One CR
attaches to a [Cinder](./cinder-crd.md) CR via `spec.cinderRef` and describes one
volume backend: an NFS export (`type: NFS`) or a pool in a Ceph cluster
(`type: RBD`).

The attachment is inverted: the backend points at the Cinder, so backends are
added and removed without editing the Cinder CR. A dedicated controller owns the
backend's own conditions, while the cinder-side sub-reconciler renders every
attached, credential-ready backend into its own Secret and projects one
`cinder-volume` Deployment for it. For that controller topology see
[Cinder Reconciler Architecture](./cinder-reconciler.md).

There is no default backend to elect. A volume type selects its backend by name,
so the backends a Cinder serves are peers and an empty set is a valid state.

The CRD is generated from
`operators/cinder/api/v1alpha1/cinderbackend_types.go`; the webhook lives in
`cinderbackend_webhook.go` and the controllers in
`operators/cinder/internal/controller/cinderbackend_controller.go` and
`reconcile_backends.go`.

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `cinder.openstack.c5c3.io` |
| Version | `v1alpha1` |
| Kind | `CinderBackend` |
| List Kind | `CinderBackendList` |
| Scope | Namespaced |

`kubectl get cinderbackends` shows Ready
(`.status.conditions[?(@.type=='Ready')].status`), Type (`.spec.type`), Cinder
(`.spec.cinderRef.name`), and Age.

## Example

```yaml
apiVersion: cinder.openstack.c5c3.io/v1alpha1
kind: CinderBackend
metadata:
  name: nfs-a
  namespace: openstack
spec:
  cinderRef:
    name: cinder
  type: NFS
  nfs:
    server: nfs-server.openstack.svc.cluster.local
    path: /volumes
status:
  conditions:
  - type: CredentialsReady
    status: "True"
    reason: CredentialsNotRequired
  - type: ConfigProjected
    status: "True"
    reason: ConfigProjected
  - type: Ready
    status: "True"
    reason: AllReady
```

The `metadata.name` (`nfs-a`) becomes three things at once: the `[nfs-a]`
section of the rendered backend configuration, the `volume_backend_name` a
volume type schedules against, and the second half of the `cinder@nfs-a` host
identity every volume on this backend is keyed by.

An RBD backend names the Ceph pool, the cephx user, the monitors, the networks
the Ceph cluster answers on, and the Secret that holds the user's key:

```yaml
apiVersion: cinder.openstack.c5c3.io/v1alpha1
kind: CinderBackend
metadata:
  name: rbd-a
  namespace: openstack
spec:
  cinderRef:
    name: cinder
  type: RBD
  rbd:
    pool: volumes
    user: cinder
    monitors:
    - rook-ceph-mon-a.rook-ceph.svc.cluster.local
    networks:
    - 10.244.0.0/16
    - 10.96.0.0/12
    keySecretRef:
      name: ceph-client-cinder
status:
  conditions:
  - type: CredentialsReady
    status: "True"
    reason: CredentialsAvailable
  - type: ConfigProjected
    status: "True"
    reason: ConfigProjected
  - type: Ready
    status: "True"
    reason: AllReady
```

## Spec

### CinderBackendSpec

Three schema-level CEL rules hold even when the webhook is down: `cinderRef` and
`type` are immutable (UPDATE transition rules), and the union rule
`(self.type == 'NFS') == has(self.nfs) && (self.type == 'RBD') == has(self.rbd)`
enforces exactly one backend block matching `spec.type`.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `cinderRef` | `CinderRefSpec` | Yes | | Names the Cinder CR in the same namespace (`name`, `MinLength=1`). Immutable: re-pointing a backend would strand the volumes the old deployment created under a host identity nothing serves. The Cinder need not exist at admission time (GitOps ordering); a dangling reference surfaces as `Ready=False`. |
| `type` | `CinderBackendType` | Yes | | The volume driver, `NFS` or `RBD` (enum). Immutable, because the volumes already on the backend were created by the driver that is being replaced. |
| `nfs` | `NFSBackendSpec` | When `type: NFS` | | The NFS export (union rule). |
| `rbd` | `RBDBackendSpec` | When `type: RBD` | | The Ceph pool, the cephx user, the monitors, the networks and the key Secret (union rule). |
| `imageVolumeCache` | `ImageVolumeCacheSpec` | No | | Turns on the per-backend image-volume cache. |
| `extraOptions` | `map[string]string` | No | | Free-form `[<name>]` section options not covered by the typed fields, keyed by bare option name. `MaxProperties=32`, and a CEL rule bounds each key at 256 and each value at 1024 characters. See the [denylist](#extraoptions-denylist). |

### NFSBackendSpec

| Field | Type | Required | Default | Rendered option | Description |
| --- | --- | --- | --- | --- | --- |
| `server` | `string` | Yes | | `nfs_shares_config` (first half) | The NFS server, a hostname or an IP address. It reaches the mount command verbatim, so the pattern `^[A-Za-z0-9.-]+$` admits only what a hostname or an IPv4 address carries; `MinLength=1` |
| `path` | `string` | Yes | | `nfs_shares_config` (second half) | The absolute export path on the server; pattern `^/[!-~]*$`, printable ASCII only. The exclusion is what keeps the shares file one export long and that export intact: the operator writes `server:path` into it verbatim, and the driver strips the line it reads back and cuts it at the first space. It is an allowlist because a schema pattern compiles with RE2, whose `\s` is the five ASCII characters, while the driver strips the full Unicode whitespace set — so a pasted U+00A0 would resolve to a different export there than the one mounted here |
| `mountOptions` | `string` | No | `nfsvers=4.1,soft,timeo=30,retrans=2` | `nfs_mount_options` | The comma-separated option string the export is mounted with; pattern `^[^\n\r]*$` for the same reason. Keep the mount soft: a hard mount blocks the `cinder-volume` process on an unreachable server instead of failing the request |

### RBDBackendSpec

The operator creates neither the pool nor the user: both exist on the Ceph
cluster before the backend is applied. Every value below reaches a file, a
command line or a NetworkPolicy verbatim, so each pattern is an allowlist. The
validating webhook repeats each rule and adds the two a pattern cannot express
(see the [validation summary](#immutability-and-validation-summary)).

| Field | Type | Required | Default | Rendered option | Description |
| --- | --- | --- | --- | --- | --- |
| `pool` | `string` | Yes | | `rbd_pool` | The pool that keeps the volumes as RBD images. `MinLength=1`, `MaxLength=64`, pattern `^[A-Za-z0-9._-]+$` |
| `user` | `string` | Yes | | `rbd_user` | The cephx user without its `client.` prefix, such as `cinder`; a CEL rule rejects `client.cinder`. The value is also the `--id` the Ceph tools receive, the `[client.<user>]` section of the keyring and part of the keyring's file name. `MinLength=1`, `MaxLength=64`, pattern `^[A-Za-z0-9._-]+$` |
| `monitors` | `[]string` | Yes | | `mon_host` in `ceph.conf` | The monitor addresses, each a hostname or an IPv4 address with an optional `:port`, joined with commas in list order. A bare address is tried on the msgr2 port 3300 and then on the msgr1 port 6789. IPv6 literals are not admitted. 1 to 16 items of 1 to 253 characters matching `^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`; the webhook also requires a port between 1 and 65535 |
| `networks` | `[]string` | Yes | | The `ipBlock` peers of the Ceph egress rule | The IPv4 CIDRs the monitors and the OSDs answer on. For a Rook cluster in the same Kubernetes cluster that is the pod network and, because Rook advertises each monitor through a Service, the service network. The field is required so a backend admitted on a Cinder without `spec.networkPolicy` keeps its egress when the policy is enabled later. 1 to 16 items matching `^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$`; the webhook also requires the canonical form with the host bits zero, such as `10.128.0.0/22` for `10.128.0.1/22`, because the API server refuses any other form in an `ipBlock` |
| `clusterName` | `string` | No | `ceph` | `rbd_cluster_name` | The Ceph cluster name. It is the `--cluster` argument of the Ceph tools and names the projected files `/etc/ceph/<cluster>.conf` and `/etc/ceph/<cluster>.client.<user>.keyring`. `MaxLength=63`, pattern `^[A-Za-z0-9_-]+$` |
| `keySecretRef` | `SecretNameRefSpec` | Yes | | The keyring | Names the Secret that holds the key (`name`, a DNS-1123 subdomain of at most 253 characters). See [The key Secret](#the-key-secret) |
| `secretUUID` | `string` | No | | `rbd_secret_uuid`, only when set | The UUID of the libvirt secret the hypervisors look the key up by, in lowercase `8-4-4-4-12` form. Left unset, the driver uses the FSID it reads from the cluster at start. The libvirt secret on the hypervisors has to carry the same UUID either way |

### The key Secret

The key Secret lives in the backend's namespace on the cluster the parent
Cinder's `spec.targetClusterRef` names, which is where the volume pods run.
Without a target cluster that is the management cluster. The Secret carries the
raw cephx key, as `ceph auth get-key client.<user>` prints it, under one data
key: `userKey`. Rook's `CephClient` writes that key into the Secret
`rook-ceph-client-<name>` it produces, so such a Secret works unchanged once it
is in the namespace. A hand-made one is a single command:

```bash
ceph auth get-key client.cinder |
  kubectl -n openstack create secret generic ceph-client-cinder --from-file=userKey=/dev/stdin
```

The pipe keeps the key off the `kubectl` command line, which other users of the
host can read.

The operator never learns where the key came from, so a Rook-produced Secret, an
External Secrets Operator sync and the command above all satisfy the contract.
`CredentialsReady` stays `False` while the Secret, its `userKey` or a usable
value is missing; the messages are listed under [Conditions](#conditions).

The key is copied into the backend's content-hashed projection Secret.
Replacing it changes that Secret's name, and the backend's `cinder-volume` rolls
with the `Recreate` strategy. Volume operations in flight during the roll fail
and are retried by their callers. The previous key stays in up to three retained
projection Secrets, which is harmless because Ceph no longer accepts it.

Rotate with `kubectl patch` on the Secret, or let Rook rewrite its own Secret in
place. Never delete and recreate the Secret to rotate it. While the Secret is
gone, `CredentialsReady` turns `False` within the 15-second poll of the backend
controller, the Cinder stops the backend's volume service, and its projection
Secrets are swept. The volume service and its projection come back once the
Secret is there again. A patch that leaves `userKey` empty or not a cephx key
has the same effect: the Cinder skips the backend rather than project a keyring
Ceph would refuse.

### ImageVolumeCacheSpec

The cache trades backend capacity for `create volume from image` latency: the
first volume created from an image is kept, and later requests clone it on the
backend instead of pulling the image through Glance again.

| Field | Type | Required | Default | Rendered option | Description |
| --- | --- | --- | --- | --- | --- |
| `enabled` | `bool` | Yes | `false` | `image_volume_cache_enabled` | Turns the cache on. It is a field rather than the presence of the block, so the bounds can be configured in a CR that keeps the cache off |
| `maxSizeGB` | `*int32` (Minimum=1) | No | | `image_volume_cache_max_size_gb` | Caps the total size of the cached image volumes; unset means no size bound |
| `maxCount` | `*int32` (Minimum=1) | No | | `image_volume_cache_max_count` | Caps the number of cached image volumes; unset means no count bound |

Whichever bound is reached first evicts the least recently used entry. Leaving
both unset caches without a bound, which fits a backend with capacity to spare.

The cached volumes belong to the deployment rather than to the tenant whose
request populated the cache, so cinder creates them as the project and user
`spec.internalTenant` names on the parent Cinder. Enabling the cache on a
backend whose Cinder sets no `internalTenant` is admitted with an admission
warning rather than rejected: the two CRs are applied independently, so GitOps
ordering may present the backend first. Until the block is added, the cache
stays inactive.

## extraOptions Denylist

The validating webhook rejects `extraOptions` keys the projection owns, so the
escape hatch cannot silently contradict the typed spec. Every key must first
match `^[A-Za-z0-9_]+$`; the pattern runs before the denylist, so an embedded
newline or a denylist-evading trailing space cannot slip through. Values
carrying a newline or carriage return are rejected as an INI-injection guard.

The denylist is selected by `spec.type`: the keys every backend renders apply
to both types, and each driver's own keys only to its type, so
`nfs_mount_options` is free on an RBD backend.

| Group | Rejected keys |
| --- | --- |
| Every type, rendered from typed fields | `volume_driver`, `image_volume_cache_enabled`, `image_volume_cache_max_size_gb`, `image_volume_cache_max_count` |
| Every type, operator-owned | `volume_backend_name`, `backend_host` |
| NFS, rendered from typed fields | `nfs_shares_config`, `nfs_mount_options` |
| NFS, operator-owned | `nfs_mount_point_base`, `nas_secure_file_operations`, `nas_secure_file_permissions`, `nfs_snapshot_support`, `nfs_sparsed_volumes`, `nfs_qcow2_volumes` |
| RBD, rendered from typed fields | `rbd_pool`, `rbd_user`, `rbd_cluster_name`, `rbd_secret_uuid` |
| RBD, operator-owned | `rbd_ceph_conf`, `rbd_keyring_conf` |

`rbd_keyring_conf` is no option cinder registers since OSSN-0085, but the driver
still reads it for cinderlib; the operator projects the keyring into
`/etc/ceph` instead.

A key that passes both gates is merged into the rendered section without
overriding an operator key, and the renderer drops every key the type's
denylist names. The denylist normally keeps the two disjoint; the merge is the
fail-closed backstop for a CR written past admission, and it keeps
`rbd_keyring_conf` out of a section that renders no key of that name.

## Name rules

`metadata.name` becomes a cinder.conf section, so the webhook rejects a name
cinder or the operator already uses:

- `default` (compared case-insensitively) names the `[DEFAULT]` section, which
  would make the backend's options service-wide ones.
- Any section name one of the embedded option catalogs enumerates, such as
  `database` or `keystone_authtoken`, collides with a section the rendered
  configuration already carries.
- A name containing `@` or `#` is refused: cinder reads them as the separators
  of a service identity (`<cinder>@<backend>`) and of a pool within it
  (`<host>#<pool>`).

`metadata.name` plus `spec.cinderRef.name` may not exceed 47 characters
together. Detaching the backend runs a `<cinder>-<name>-service-remove` Job
whose name is copied into a label value, and Kubernetes caps a label value at 63
characters. The budget is shared rather than per-name because either name may be
the long one. It is enforced on create only: both names are immutable, so an
update rule could only reject a CR an earlier operator version already admitted,
including the finalizer-removal update that completes its deletion.

`metadata.name` may not exceed 35 characters on its own either. The same detach
records the Job's terminal state under the annotation key
`cobaltcore.c5c3.io/last-service-remove-<name>-job-uid` on the Cinder, and
Kubernetes caps an annotation key's name part at 63 characters. The two bounds
measure different things — a short Cinder name buys room under the shared budget
but none here — and this one is enforced on create only for the same reason.

## Rendered backend section

Every credential-ready backend renders into a `[<name>]` section of its own
`backend.conf`. The keys the operator writes for `nfs-a` on a Cinder named
`cinder`:

```ini
[nfs-a]
backend_host = cinder
nas_secure_file_operations = true
nas_secure_file_permissions = true
nfs_mount_options = nfsvers=4.1,soft,timeo=30,retrans=2
nfs_mount_point_base = /var/lib/cinder/mnt
nfs_qcow2_volumes = false
nfs_shares_config = /etc/cinder/backends.conf.d/nfs-a.shares
nfs_snapshot_support = false
nfs_sparsed_volumes = true
volume_backend_name = nfs-a
volume_driver = cinder.volume.drivers.nfs.NfsDriver
```

`backend_host` is the Cinder's name, not the backend's: cinder appends
`@<backend>` itself, so the rendered value is the half the operator owns. The
three `image_volume_cache_*` keys follow when the cache is enabled.

Five caveats come with the NFS driver:

- Snapshots are off. `nfs_snapshot_support = false` is rendered on every
  backend, so a volume on an NFS backend cannot be snapshotted, and neither can
  the operations built on snapshots.
- The export's permissions do not separate projects. Both `nas_secure_*` keys
  are `true`, which is what lets the driver run its file operations as the
  service user rather than through a root helper the image has no sudoers entry
  for (decision D3 of issue [#979](https://github.com/C5C3/cobaltcore/issues/979)).
  Every volume file on the export is then owned by that one identity, so a
  cloned volume is as readable as its source within the export's permission
  model. Project isolation is the API's, not the filesystem's.
- Active/active is refused. A `cinder-volume` owns its backend through its host
  identity, and the NFS drivers refuse two processes holding the same volume
  state, so `spec.volume.deployment` is pinned at one replica on the `Recreate`
  strategy (see [CinderVolumeSpec](./cinder-crd.md#cindervolumespec)).
- A clone is world-readable. Cloning a volume runs through the driver's internal
  temporary snapshot and ends in `_set_rw_permissions_for_all`, so the cloned
  file carries mode `666` where a created volume's carries `660`. The export
  itself is mode `0770`, which keeps the difference inside the service group.
- Uploading a volume that was itself created from an image fails. Glance rejects
  the request at both 27.0.0 and 28.0.0 with
  `400 Unable to set 'properties' to {}. Reason: {} is not of type 'string'`.
  A volume that was not created from an image uploads.

An RBD backend `rbd-a` on the same Cinder renders:

```ini
[rbd-a]
backend_host = cinder
rbd_ceph_conf = /etc/ceph/ceph.conf
rbd_cluster_name = ceph
rbd_pool = volumes
rbd_user = cinder
volume_backend_name = rbd-a
volume_driver = cinder.volume.drivers.rbd.RBDDriver
```

`rbd_secret_uuid` follows when `spec.rbd.secretUUID` is set, and the cache keys
and the `extraOptions` follow as for NFS. The same Secret carries the Ceph
client configuration, `ceph.conf`:

```ini
[global]
mon_host = rook-ceph-mon-a.rook-ceph.svc.cluster.local
keyring = /etc/ceph/ceph.client.cinder.keyring
log_file = /dev/null
admin_socket = /tmp/$cluster-$name.$pid.$cctid.asok
```

and the keyring, whose `key` line carries the trimmed `userKey` of the key
Secret:

```ini
[client.cinder]
	key = AQDHlkVoYx3QKRAAk9m6j4QzG8w2yK0hfr9c6g==
```

`log_file` and `admin_socket` replace two defaults the service user cannot
honour, because it can write neither `/var/log/ceph` nor `/var/run/ceph`. The
dollar signs are Ceph metavariables, which librados expands per process.

Four caveats come with the RBD driver:

- The driver connects to the cluster during its setup. When the monitors do not
  answer, the volume service logs `Error connecting to ceph cluster.` and
  `Failed to initialize driver.`, still connects to the broker and passes its
  readiness probe, and serves no volume. It does not repeat the setup on its
  own: delete its pod once the monitors answer, and the Deployment starts a new
  one.
- `rbd_secret_uuid` defaults to the FSID the driver reads at start. The libvirt
  secret on the hypervisors has to carry the same UUID, whether it is set here
  or left to that default. The lab's libvirt DaemonSet defines
  `090e4a3c-6c20-4e74-82dc-1a70382babe8`
  ([Lab hypervisors](../infrastructure/infrastructure-manifests.md#lab-hypervisors)),
  so a lab backend sets `secretUUID` to it.
- `rbd_exclusive_cinder_pool` stays at its default, `true`: the driver reports
  the capacity it provisioned from cinder's own volume records and does not list
  the pool. Two backends on one pool, or a pool shared with other users, each
  count only their own volumes, so the over-subscription check sees less
  provisioned capacity than the pool holds. Set
  `rbd_exclusive_cinder_pool: "false"` in `extraOptions` for a shared pool.
- `cinder-backup` cannot read a volume on an RBD backend yet. The backup pod
  carries no Ceph client configuration and no keyring
  ([#1342](https://github.com/C5C3/cobaltcore/issues/1342)), so a backup of
  such a volume fails even while a `CinderBackupBackend` is attached.

## Detaching a backend

The controller holds a deleted `CinderBackend` with the finalizer
`cinder.openstack.c5c3.io/service-remove`, and the cinder-side step is what
releases it. A running `cinder-volume` reports itself into the service registry
every few seconds, so an entry removed while the process still runs comes back.
The order is therefore fixed: the volume Deployment is deleted, a later pass
finds it gone and runs the `<cinder>-<name>-service-remove` Job, the projected
Secrets are swept, and only then is the finalizer released. The Deployment is
deleted with foreground propagation, so the API server keeps it until its pods
have exited, and the Job never starts beside a terminating `cinder-volume`.
While that runs, the backend reports `Ready=False` under the reason `Detaching`.

The Job runs `cinder-manage service remove cinder-volume <cinder>@<name>`.
Exit code 2 is "host not found", which is the state of a backend whose
`cinder-volume` never registered, so the script normalises it to success and
lets every other code fail the Job. The removed `services` row is only
soft-deleted: `GET /v3/os-services` stops listing it as soon as the Job
succeeds, but the row stays in the table until the next
[db purge](./cinder-reconciler.md#dbpurge) sweeps it, and a backend that is
attached again registers under a new row id. A failed Job keeps the finalizer:
the registry entry is still there, and deleting the Job is what retries the
removal once the cause is understood. The parent then reports
`VolumeServicesReady=False` under `ServiceRemoveJobFailed`.

A backend whose parent Cinder is already gone is released unconditionally, with
a `ServiceRemoveSkipped` event: the registry lives in that Cinder's database, so
there is no row left to remove.

## Conditions

The dedicated `CinderBackendReconciler` is the single writer of this status. The
cinder-side sub-reconciler only reads `CredentialsReady`, which gates the
projection, and writes the aggregated `BackendsReady` condition onto the Cinder
CR instead.

The figure shows the order the two conditions turn `True` in. An NFS
`CinderBackend` passes step 1 without a credential, an RBD one once its key
Secret carries a usable `userKey`, and step 5 looks at the volume Deployment of
this backend.
[The handshake](../backend/kubernetes-packages.md#satellite-handshake)
lists the five steps and what differs per kind.

![The handshake between a satellite resource and the service it attaches to, in five numbered steps across two controllers. 1: the satellite controller checks the credentials and sets CredentialsReady on the satellite. 2: the aggregation step of the service controller reads only that condition. 3: it renders one section per satellite that passed into a Secret whose name carries a hash of its content. 4: the pod template of the service's Deployment mounts that Secret, and a new name rolls the pods. 5: the satellite controller finds its section in the mounted Secret and sets ConfigProjected. Ready turns True once both conditions are. An arrow marked never runs from Ready to the aggregation step: reading Ready there would deadlock, because Ready needs ConfigProjected, which needs that step. On a KeystoneIdentityBackend the gate is DomainReady, and ConfigProjected also waits until the rollout has finished.](../../diagrams/service-satellite-handshake.svg)

| Type | Owner | Status | Reason | Meaning |
| --- | --- | --- | --- | --- |
| `CredentialsReady` | CinderBackend | True | `CredentialsNotRequired` | An NFS backend: the export is mounted with the pod's own identity, so there is no credential to resolve. The condition is still reported, because the cinder-side projection gates on it and an absent one reads as not ready. |
| `CredentialsReady` | CinderBackend | True | `CredentialsAvailable` | An RBD backend whose key Secret carries a cephx key under `userKey`: `RBD key Secret "<name>" carries the userKey data key`. |
| `CredentialsReady` | CinderBackend | False | `WaitingForCredentials` | An RBD backend whose key is not usable yet. The message names the cause: `RBD key ExternalSecret <ns>/<name> not found yet` (neither the Secret nor an ExternalSecret of that name exists), `waiting for the RBD key Secret to carry the userKey data key` (an ExternalSecret has not synced yet), `RBD key Secret exists but is missing expected keys` (the Secret has no `userKey`), `Secret "<name>" carries an empty userKey`, or `Secret "<name>" carries a userKey that is not a cephx key (base64 expected)`. A CR written past admission without `spec.rbd` reads `spec.rbd is not set; no RBD key to resolve`. The backend requeues after 15 seconds, and `ConfigProjected` is not observed again until the gate is `True`, so `Ready` follows `CredentialsReady`. |
| `CredentialsReady` | CinderBackend | False | `WaitingForParent` | No Cinder of the name in `spec.cinderRef` exists, so which cluster this backend's volume service runs on is unknown. The backend requeues after 15 seconds. |
| `CredentialsReady` | CinderBackend | False | `TargetClusterUnavailable` | The parent Cinder's `spec.targetClusterRef` names a target cluster that is not registered or no longer resolves. The message carries the resolver's error, the backend requeues after 15 seconds, and nothing is created on any cluster. See [Target Clusters](../target-clusters.md). |
| `ConfigProjected` | CinderBackend | True | `ConfigProjected` | This backend's `cinder-volume` Deployment mounts a `backend.conf` carrying its `[<name>]` section. |
| `ConfigProjected` | CinderBackend | False | `WaitingForProjection` | The projection has not landed in the Deployment yet, or the parent Cinder no longer exists and the standing claim is stale. |
| `Ready` | CinderBackend | True | `AllReady` | Both sub-conditions are True. |
| `Ready` | CinderBackend | False | `NotAllReady` | At least one sub-condition is not True. |
| `Ready` | CinderBackend | False | `Detaching` | The CR is being deleted and waits for the parent to run the service-remove Job. |
| `BackendsReady` | Cinder | True | `AllBackendsProjected` | Every attached backend is credential-ready and projected. |
| `BackendsReady` | Cinder | True | `NoBackends` | No CinderBackend is attached. It is True rather than False: a Cinder without volume backends serves its API and its scheduler, and attaching storage is a separate act. |
| `BackendsReady` | Cinder | False | `WaitingForBackends` | At least one attached backend is pending, either not yet credential-ready or skipped for a per-backend fault. The ready subset is still projected, and the message names what is missing. |

`status.observedGeneration` carries the `metadata.generation` of the last spec
the controller observed (`cinderbackend_types.go`), so a status reported against
an older spec is distinguishable from a current one.

A backend that carries no `spec.nfs` or `spec.rbd` block matching its type,
whose rendered files carry a control character, or whose RBD key Secret vanished
between its gate and the render is skipped: the cinder-side step emits a
`CinderBackendSkipped` Warning event on the Cinder CR and keeps projecting the
healthy siblings, so one bad backend never fails the whole aggregation. The
`CinderBackend` controller itself emits one event, `ServiceRemoveSkipped`; the
rest of its contract is carried by the conditions above.

## Retained Artefacts

One backend's projection lives in a content-hashed
`<cinder>-backend-<name>-<hash>` Secret owned by the parent Cinder. Up to 3
historical copies are retained for fast rollback, and all of them are collected
with the Cinder CR. Each backend gets its own base name rather than sharing one
Secret with its siblings, so a change to one backend does not roll the pods of
the others.

| Data key | Content | Mounted at |
| --- | --- | --- |
| `backend.conf` | The `[<name>]` section above | `/etc/cinder/backends.conf.d/backend.conf` |
| `shares` | One line, `server:path`, the export list the driver reads | `/etc/cinder/backends.conf.d/<name>.shares` |
| `volume.conf` | `[DEFAULT] enabled_backends = <name>`, the overlay naming the single backend this process serves | `/etc/cinder/volume.conf.d/volume.conf` |
| `ceph.conf` | The Ceph client configuration above | `/etc/ceph/<cluster>.conf` |
| `keyring` | The keyring of `client.<user>` | `/etc/ceph/<cluster>.client.<user>.keyring` |

An NFS backend's Secret carries `backend.conf`, `shares` and `volume.conf`; an
RBD backend's carries `backend.conf`, `volume.conf`, `ceph.conf` and `keyring`.
The `shares` key is mounted under the backend's own name, which is the path
`nfs_shares_config` points at, so the file is readable in a process that serves
one backend. The two Ceph files are mounted read-only at `/etc/ceph` under the
names librados and the Ceph tools look for.

A backend that detached, was renamed or was skipped keeps no Deployment, so its
base name is swept whole rather than retained: nothing mounts those Secrets
anymore, and each of them names an export the Cinder no longer serves.

## Immutability and Validation Summary

Schema-layer rules (CEL and kubebuilder markers, enforced even when the webhook
is down): the `cinderRef` and `type` transition rules, the two-sided union of
`type` with `nfs` and `rbd`, the `NFS`/`RBD` type enum, the `server` pattern and
`MinLength`, the absolute-path and printable-ASCII pattern on `path`, the
newline pattern on `mountOptions`, the `mountOptions` default, the patterns and
bounds of `pool`, `user`, `monitors`, `networks`, `clusterName`,
`keySecretRef.name` and `secretUUID`, the CEL rule against the `client.` prefix
on `user`, the `clusterName` default, the two image-volume-cache minimums, and
the `extraOptions` `MaxProperties` and key/value length bounds.

Webhook rules (defense in depth plus the rules CEL cannot express): the union
re-check of both blocks, the reserved-name collision guard and the `@`/`#` guard
on `metadata.name`, the combined 47-character name budget and the 35-character
bound on `metadata.name` alone, the printable-ASCII guard on `path` and the
newline guard on `mountOptions`, the `client.` prefix twin on `user`, the
newline guard on `pool`, `user`, `clusterName` and every monitor, a monitor port
between 1 and 65535, a canonical IPv4 CIDR per network, the DNS-1123 check on
`keySecretRef.name`, the `extraOptions` key charset, the per-type denylist and
the INI-injection guard, and the image-volume-cache warning.
The defaulting webhook fills the `mountOptions` and `clusterName` defaults for
callers that bypass the schema.

## Chainsaw E2E Tests

The rejection corpus lives in `tests/e2e/cinder/invalid-cinderbackend-cr`: the
union rule, the reserved name, a relative export path, an export path carrying a
newline, both `extraOptions` guards, the `cinderRef` transition rule, and the two
name bounds. The RBD entries add both halves of the union with the `rbd` block,
the `client.` prefix, an empty monitor list, a network that is no CIDR, a key
Secret name that is no DNS-1123 subdomain, `rbd_keyring_conf` in `extraOptions`
on an RBD backend, and the `type` transition rule as an update from `NFS` to
`RBD`.

The [`rbd-backend` suite](../testing/cinder-e2e-tests.md#rbd-backend) proves the
gate, the projection, the mounts, the Ceph egress rule and the roll on a replaced
key on a kind cluster that runs no Ceph.
