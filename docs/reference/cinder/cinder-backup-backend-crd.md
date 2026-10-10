---
title: CinderBackupBackend CRD API Reference
quadrant: operator
---

# CinderBackupBackend CRD API Reference

Reference documentation for the CinderBackupBackend Custom Resource Definition.
One CR attaches to a [Cinder](./cinder-crd.md) CR via `spec.cinderRef` and
describes the driver the backup service writes volume backups through: the
chunked NFS driver (`type: NFS`) or the Ceph backup driver (`type: RBD`).

It is a separate kind from [`CinderBackend`](./cinder-backend-crd.md) because
the two describe different things. A volume backend is one of several a Cinder
serves, and each gets its own `cinder-volume` Deployment; the backup driver is a
single property of the one `cinder-backup` Deployment. At most one may be
attached to a Cinder.

Backups are opt-in. A Cinder without a backup backend renders no backup
Deployment and reports `BackupBackendReady=True` under the reason
`NoBackupBackend`.

The CRD is generated from
`operators/cinder/api/v1alpha1/cinderbackupbackend_types.go`; the webhook lives
in `cinderbackupbackend_webhook.go` and the controllers in
`operators/cinder/internal/controller/cinderbackupbackend_controller.go` and
`reconcile_backup_backend.go`.

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `cinder.openstack.c5c3.io` |
| Version | `v1alpha1` |
| Kind | `CinderBackupBackend` |
| List Kind | `CinderBackupBackendList` |
| Scope | Namespaced |

`kubectl get cinderbackupbackends` shows Ready
(`.status.conditions[?(@.type=='Ready')].status`), Type (`.spec.type`), Cinder
(`.spec.cinderRef.name`), and Age.

## Example

```yaml
apiVersion: cinder.openstack.c5c3.io/v1alpha1
kind: CinderBackupBackend
metadata:
  name: nfs-backups
  namespace: openstack
spec:
  cinderRef:
    name: cinder
  type: NFS
  nfs:
    server: nfs-server.openstack.svc.cluster.local
    path: /backups
  fileSize: 52428800
  compression: zlib
```

An RBD target names the pool the backups are written to, the cephx user, the
monitors, the networks of the Ceph cluster and the Secret that holds the key:

```yaml
apiVersion: cinder.openstack.c5c3.io/v1alpha1
kind: CinderBackupBackend
metadata:
  name: rbd-backups
  namespace: openstack
spec:
  cinderRef:
    name: cinder
  type: RBD
  rbd:
    pool: backups
    user: cinder-backup
    monitors:
    - rook-ceph-mon-a.rook-ceph.svc.cluster.local
    networks:
    - 10.244.0.0/16
    - 10.96.0.0/12
    keySecretRef:
      name: ceph-client-cinder-backup
```

## Spec

### CinderBackupBackendSpec

Three schema-level CEL rules hold even when the webhook is down: `cinderRef` and
`type` are immutable (UPDATE transition rules), and the union rule
`(self.type == 'NFS') == has(self.nfs) && (self.type == 'RBD') == has(self.rbd)`
enforces exactly one backup backend block matching `spec.type`.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `cinderRef` | `CinderRefSpec` | Yes | | Names the Cinder CR in the same namespace (`name`, `MinLength=1`). Immutable: re-pointing the driver at a different Cinder would leave the backups it already wrote recorded against a deployment that cannot read them. The Cinder need not exist at admission time; a dangling reference surfaces as `Ready=False`. |
| `type` | `CinderBackupBackendType` | Yes | | The backup driver: `NFS` or `RBD` (enum). Immutable, because a restore reads a backup with the driver that wrote it. |
| `nfs` | `NFSBackupBackendSpec` | When `type: NFS` | | The NFS export the chunks are written to (union rule). Its three fields (`server`, `path`, `mountOptions`) carry the same types, patterns and default as [NFSBackendSpec](./cinder-backend-crd.md#nfsbackendspec). |
| `rbd` | `RBDBackupBackendSpec` | When `type: RBD` | | The Ceph pool the backups are written to, the user and the key Secret (union rule). See [RBDBackupBackendSpec](#rbdbackupbackendspec). |
| `fileSize` | `*int64` | No | `52428800` | The size in bytes of one backup chunk (`backup_file_size`). Cinder splits a volume into objects of this size and writes them one at a time, so it bounds both the memory a backup holds and the work a failed chunk costs. `Minimum=1048576` and `MultipleOf=32768`: cinder hashes each chunk in blocks of `backup_sha_block_size_bytes` (32 KiB) and refuses a chunk size that block size does not divide. The option belongs to the chunked NFS driver; an RBD target neither renders nor reads it, and the Ceph driver splits a full copy by `backup_ceph_chunk_size` instead. |
| `compression` | `string` | No | `zlib` | The algorithm each chunk is compressed with (`backup_compression_algorithm`); one of `none`, `zlib`, `bz2`, `zstd` (enum). `none` trades backup capacity for CPU on the backup pod. The option belongs to the chunked NFS driver; an RBD target neither renders nor reads it, because the Ceph driver compresses nothing. |
| `extraOptions` | `map[string]string` | No | | Free-form backup-section options not covered by the typed fields, keyed by bare option name. `MaxProperties=32`, and a CEL rule bounds each key at 256 and each value at 1024 characters. See the [denylist](#extraoptions-denylist). |

### RBDBackupBackendSpec

The operator creates neither the pool nor the user: both exist on the Ceph
cluster before the backup target is applied. Every value below reaches a file, a
command line or a NetworkPolicy verbatim, so each pattern is an allowlist. The
markers are the ones of [RBDBackendSpec](./cinder-backend-crd.md#rbdbackendspec),
and the validating webhook repeats each rule and adds the two a pattern cannot
express (see the [validation summary](#immutability-and-validation-summary)).
There is no `secretUUID`: no hypervisor attaches a backup image, so no libvirt
secret looks its key up.

| Field | Type | Required | Default | Rendered option | Description |
| --- | --- | --- | --- | --- | --- |
| `pool` | `string` | Yes | | `backup_ceph_pool` | The pool the backups are written to as RBD images. `MinLength=1`, `MaxLength=64`, pattern `^[A-Za-z0-9._-]+$` |
| `user` | `string` | Yes | | `backup_ceph_user` | The cephx user without its `client.` prefix, such as `cinder-backup`; a CEL rule rejects `client.cinder-backup`. The value is also the `--id` the Ceph tools receive, the `[client.<user>]` section of the keyring and part of the keyring's file name. `MinLength=1`, `MaxLength=64`, pattern `^[A-Za-z0-9._-]+$` |
| `monitors` | `[]string` | Yes | | `mon_host` in `ceph.conf` | The monitor addresses, each a hostname or an IPv4 address with an optional `:port`, joined with commas in list order. A bare address is tried on the msgr2 port 3300 and then on the msgr1 port 6789. IPv6 literals are not admitted. 1 to 16 items of 1 to 253 characters matching `^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`; the webhook also requires a port between 1 and 65535 |
| `networks` | `[]string` | Yes | | The `ipBlock` peers of the Ceph egress rule | The IPv4 CIDRs the monitors and the OSDs answer on. They are unioned with the networks of the Cinder's RBD volume backends into the one Ceph egress rule. The field is required so a target admitted on a Cinder without `spec.networkPolicy` keeps its egress when the policy is enabled later. 1 to 16 items matching `^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$`; the webhook also requires the canonical form with the host bits zero |
| `clusterName` | `string` | No | `ceph` | The file names `/etc/ceph/<cluster>.conf` (which `backup_ceph_conf` names) and `/etc/ceph/<cluster>.client.<user>.keyring` | The Ceph cluster name. The Ceph backup driver takes no cluster argument, so the name decides the two file names alone. `MaxLength=63`, pattern `^[A-Za-z0-9_-]+$` |
| `keySecretRef` | `SecretNameRefSpec` | Yes | | The keyring | Names the Secret that holds the key (`name`, a DNS-1123 subdomain of at most 253 characters). See [The key Secret](#the-key-secret) |

### The key Secret

The key Secret follows the contract of the [RBD volume
backend](./cinder-backend-crd.md#the-key-secret). It lives in the backup
target's namespace on the cluster the parent Cinder's `spec.targetClusterRef`
names, which is where the backup pod runs; without a target cluster that is the
management cluster. It carries the raw cephx key, as
`ceph auth get-key client.<user>` prints it, under one data key: `userKey`.
Rook's `CephClient` writes that key into the Secret `rook-ceph-client-<name>` it
produces, so such a Secret works unchanged once it is in the namespace. A
hand-made one is a single command:

```bash
ceph auth get-key client.cinder-backup |
  kubectl -n openstack create secret generic ceph-client-cinder-backup --from-file=userKey=/dev/stdin
```

The pipe keeps the key off the `kubectl` command line, which other users of the
host can read.

`CredentialsReady` stays `False` while the Secret, its `userKey` or a usable
value is missing; the messages are listed under [Conditions](#conditions).

The key is copied into the target's content-hashed projection Secret. Replacing
it changes that Secret's name, and the backup Deployment rolls with the
`Recreate` strategy. A backup or restore in flight during the roll fails and is
retried by its caller. The previous key stays in up to three retained projection
Secrets, which is harmless because Ceph no longer accepts it.

Rotate with `kubectl patch` on the Secret, or let Rook rewrite its own Secret in
place. While the Secret is gone, `CredentialsReady` turns `False` at the next
15-second poll of the backup backend controller, and the Cinder deletes the
backup Deployment and sweeps the target's projection Secrets at its next pass.
Both come back once the Secret is there again.

## extraOptions Denylist

The validating webhook rejects the option names the projection owns. Every key
must first match `^[A-Za-z0-9_]+$`, and a value carrying a newline or carriage
return is rejected as an INI-injection guard. The denylist is selected by
`spec.type`: the shared rows apply to both types, each driver's rows to its own.

| Group | Rejected keys |
| --- | --- |
| Shared, rendered from typed fields | `backup_driver` |
| Shared, operator-owned | `host`, `backup_use_same_host` |
| NFS, rendered from typed fields | `backup_share`, `backup_mount_options`, `backup_file_size`, `backup_compression_algorithm` |
| NFS, operator-owned | `backup_mount_point_base` |
| RBD, rendered from typed fields | `backup_ceph_user`, `backup_ceph_pool` |
| RBD, operator-owned | `backup_ceph_conf` |

`fileSize` and `compression` are options of the NFS driver: an RBD target
carries their schema defaults after admission and ignores them. The other Ceph
driver options stay reachable through `extraOptions` with the upstream
defaults: `backup_ceph_chunk_size` (128 MiB), `backup_ceph_stripe_unit`,
`backup_ceph_stripe_count`, `backup_ceph_image_journals`,
`backup_ceph_max_snapshots` and `restore_discard_excess_bytes`. Past the
webhook, the renderer drops every key the denylist of the target's type names.

## One attachment per Cinder

Two gates enforce it. At admission the webhook lists the namespace siblings with
an uncached read, filters to the same `spec.cinderRef.name`, skips itself and
Terminating siblings, and rejects the CR with `already has CinderBackupBackend`.
The rule runs on update as well, because it is a rule about the namespace rather
than about this object.

Should a second CR slip past admission, the cinder-side step refuses to guess:
with more than one credential-ready candidate it renders nothing and sets
`BackupBackendReady=False` under the reason `MultipleBackupBackends`, naming the
candidates. Nothing being rendered means nothing to run, so the same pass
deletes the `<cinder>-backup` Deployment. Backups resume when one of the two CRs
is removed.

## Rendered backup section

The projection is a `[DEFAULT]` section of its own, mounted as a second
`--config-dir` beside the shared one, so the keys reach the backup pod and
nowhere else. For `nfs-backups` on a Cinder named `cinder`:

```ini
[DEFAULT]
backup_compression_algorithm = zlib
backup_driver = cinder.backup.drivers.nfs.NFSBackupDriver
backup_file_size = 52428800
backup_mount_options = nfsvers=4.1,soft,timeo=30,retrans=2
backup_mount_point_base = /var/lib/cinder/backup_mount
backup_share = nfs-server.openstack.svc.cluster.local:/backups
backup_use_same_host = false
host = cinder-backup
```

`host` is derived from the Cinder rather than from this CR, so replacing the
backup backend with one pointing at the same export keeps the existing backups
restorable. `backup_use_same_host` is `false` because the backup service owns
its target through that identity and a backup is never handed to another host.
It decides which backup service may serve a restore, and nothing more: the
transition rules above freeze `cinderRef` and `type` alone, so a `spec.nfs`
repointed at another export is admitted silently and strands every backup
already written, with both conditions staying `True`.

The backup pod of an NFS target mounts two kinds of export: its own target under
`/var/lib/cinder/backup_mount/<md5 of "server:path">`, and every NFS volume
backend's export under `/var/lib/cinder/mnt/<md5>`. An RBD target mounts no
export, and the volume backends' exports stay. A backup reads the volume
itself through os-brick rather than a copy the volume service hands over, so it
needs the source at the same path cinder resolved the volume's provider location
to.

A restore writes into an existing volume: it is a request against a volume that
is already there, not a way to recreate one that is gone (decision D6 of issue
[#979](https://github.com/C5C3/cobaltcore/issues/979)). Plan a recovery
accordingly, by creating the target volume first.

The file the driver writes into is what makes the difference. Restoring into a
volume the request creates opens that file with truncation, and the chunked
driver writes only the non-zero blocks it stored, so the file ends up as large
as the data written and no larger. `qemu-img info` then reports a virtual size
below the volume's, and the driver refuses to attach it. Restoring into a volume
that already exists seeks to each block's offset and writes every byte, and the
volume comes back byte-identical to the source.

On an NFS target the backup process holds one object and its compressed form in
memory at a time, so its footprint follows `spec.fileSize`. The rendered default is `52428800`
bytes, and the reconciler renders `2Gi` as the backup Deployment's memory
request and limit to match (see
[CinderBackupSpec](./cinder-crd.md#cinderbackupspec)), with `MALLOC_ARENA_MAX=2`
on the container so the buffers the thread pool frees are returned rather than
kept in per-thread arenas.

For `rbd-backups` on the same Cinder, the section names the Ceph driver, the
pool, the user and the configuration file the driver connects with:

```ini
[DEFAULT]
backup_ceph_conf = /etc/ceph/ceph.conf
backup_ceph_pool = backups
backup_ceph_user = cinder-backup
backup_driver = cinder.backup.drivers.ceph.CephBackupDriver
backup_use_same_host = false
host = cinder-backup
```

The `extraOptions` the denylist admits follow. The same Secret carries the Ceph
client configuration, `ceph.conf`:

```ini
[global]
mon_host = rook-ceph-mon-a.rook-ceph.svc.cluster.local
keyring = /etc/ceph/ceph.client.cinder-backup.keyring
log_file = /dev/null
admin_socket = /tmp/$cluster-$name.$pid.$cctid.asok
```

and the keyring, whose `key` line carries the trimmed `userKey` of the key
Secret:

```ini
[client.cinder-backup]
	key = AQBzm0VoQ2x9JBAAeT1n6hV4cRk8yL2oWq5dXg==
```

### Ceph files in the backup pod

The backup pod carries one projected volume at `/etc/ceph`, read-only. When the
target is RBD it holds the target's `<cluster>.conf` and keyring. Whatever the
target's type, it also holds the keyring of every projected RBD volume backend,
under `/etc/ceph/<cluster>.client.<user>.keyring`: a backup of an RBD volume
reads the volume through os-brick with the volume backend's own user, and
os-brick opens that keyring file and nothing else of the backend. It builds the
configuration it connects with from the monitors the connection names, so the
volume backend's `ceph.conf` is not projected. A rotated volume key therefore
rolls the backup pod as well. The projected volume hides the `/etc/ceph/rbdmap`
file `ceph-common` ships, which nothing in the pod reads.

The files come from several Secrets, and two of them can name one file: two
volume backends on the same cluster and user, or a volume backend that shares
the target's user. The API server refuses a projected volume with two items on
one path, so the first writer keeps the path: the target first, then the volume
backends in name order. A later source with the same key is dropped without a
word. One with a different key is dropped as well, and the Cinder emits a
`CephKeyringConflict` Warning event naming both sides: os-brick then reads that
backend's volumes with the first writer's key, and a backup of them fails once
Ceph refuses it.

Three caveats come with the Ceph backup driver:

- The driver connects to the cluster once, when the backup service starts. When
  the monitors do not answer, the service logs
  `Fixed interval looping call 'cinder.backup.manager.BackupManager._setup_backup_driver' failed`
  with librados' `error connecting to the cluster`, still connects to the
  broker and passes its readiness probe, and reports `backend_state` false. A
  backup request then fails with
  `Create backup aborted due to backup service is down.` The service does not
  repeat the setup on its own: delete its pod once the monitors, the networks
  and the key are right.
- A backup of an RBD volume is an `rbd export-diff` from a snapshot the driver
  keeps on the source image, so an incremental backup carries the blocks that
  changed since the last one. A backup of an NFS volume is a full copy, written
  in `backup_ceph_chunk_size` pieces of 128 MiB. The backup container keeps its
  `2Gi` memory figure and `MALLOC_ARENA_MAX=2`, which hold one such piece at a
  time.
- A target switched between the two drivers cannot restore or delete the
  backups the other driver wrote. cinder refuses the restore with
  `Restore backup aborted, the backup service currently configured [...] is not the backup service that was used to create this backup`,
  and the delete with the same sentence starting `Delete backup aborted`. Delete
  the backups before the switch, or switch back to reach them.

## Deleting a backup backend

Deletion is a plain delete. The backup service registers as `<cinder>-backup`
and re-registers under that identity whenever it starts, so no per-target row in
the service registry has to be unregistered and this kind carries no finalizer.
The parent Cinder sees the detach through its watch, deletes the
`<cinder>-backup` Deployment on its next pass, and sweeps the Secrets this CR
was rendered into. `BackupServiceReady` then returns to True under the reason
`BackupNotConfigured`.

That is the one behavioural difference from a `CinderBackend`, which is held by
the `cinder.openstack.c5c3.io/service-remove` finalizer until a Job has removed
its per-backend registry row.

## Rolling the operator back

The `cinder-operator` HelmRelease installs and upgrades its CRDs with
`crds: CreateReplace`. Moving it back to a chart without the `RBD` type
therefore also puts back a `CinderBackupBackend` schema without `spec.rbd`, and
the API server drops `spec.rbd` from every `type: RBD` target when it reads the
object. The older operator writes the target's status, which persists the object
without its `rbd` block, and the older Cinder pass skips the target for lacking
an `nfs` block and deletes the backup Deployment. After the next upgrade the
target reports `spec.rbd is not set` until its full manifest is applied again.
Finished backups in the Ceph pool and their records in the Cinder database are
not touched. A backup, restore or delete that is running when the Deployment
goes away does not finish. When the backup service starts again, it sets a
`creating` backup to `error` and the volume of a running restore to
`error_restoring`. A `deleting` backup keeps that state, and its image in the
pool stays partly removed, until the backup service starts again and resumes
the delete. A backup of an encrypted volume goes to `error_deleting` instead,
and a service started under an NFS target refuses the resumed delete with the
`Delete backup aborted` error above, sets the backup to `error` and leaves the
image in the pool.
Before the rollback, wait until `openstack volume backup list --all-projects`
shows no `creating`, `restoring` or `deleting` backup. Then delete the RBD
backup targets, or keep their manifests and re-apply them after upgrading again.

## Conditions

The dedicated `CinderBackupBackendReconciler` is the single writer of this
status, and it carries the `CinderBackend` vocabulary: both satellites answer
the same two questions, so an operator reads one status shape whichever kind is
in front of them. The cinder-side sub-reconciler only reads `CredentialsReady`
and writes the aggregated `BackupBackendReady` condition onto the Cinder CR.

The figure shows the order the two conditions turn `True` in. An NFS
`CinderBackupBackend` passes step 1 without a credential, an RBD one once its
key Secret carries a usable `userKey`, and step 5 compares the name of the
Secret the `backup` volume references.
[The handshake](../backend/kubernetes-packages.md#satellite-handshake)
lists the five steps and what differs per kind.

![The handshake between a satellite resource and the service it attaches to, in five numbered steps across two controllers. 1: the satellite controller checks the credentials and sets CredentialsReady on the satellite. 2: the aggregation step of the service controller reads only that condition. 3: it renders one section per satellite that passed into a Secret whose name carries a hash of its content. 4: the pod template of the service's Deployment mounts that Secret, and a new name rolls the pods. 5: the satellite controller finds its section in the mounted Secret and sets ConfigProjected. Ready turns True once both conditions are. An arrow marked never runs from Ready to the aggregation step: reading Ready there would deadlock, because Ready needs ConfigProjected, which needs that step. On a KeystoneIdentityBackend the gate is DomainReady, and ConfigProjected also waits until the rollout has finished.](../../diagrams/service-satellite-handshake.svg)

| Type | Owner | Status | Reason | Meaning |
| --- | --- | --- | --- | --- |
| `CredentialsReady` | CinderBackupBackend | True | `CredentialsNotRequired` | An NFS target: the export is mounted with the pod's own identity, so there is no credential to resolve. |
| `CredentialsReady` | CinderBackupBackend | True | `CredentialsAvailable` | An RBD target whose key Secret carries a cephx key under `userKey`: `RBD key Secret "<name>" carries the userKey data key`. |
| `CredentialsReady` | CinderBackupBackend | False | `WaitingForCredentials` | An RBD target whose key is not usable yet. The message names the cause: `RBD key ExternalSecret <ns>/<name> not found yet` (neither the Secret nor an ExternalSecret of that name exists), `waiting for the RBD key Secret to carry the userKey data key` (an ExternalSecret has not synced yet), `RBD key Secret exists but is missing expected keys` (the Secret has no `userKey`), `Secret "<name>" carries an empty userKey`, or `Secret "<name>" carries a userKey that is not a cephx key (base64 expected)`. A CR written past admission without `spec.rbd` reads `spec.rbd is not set; no RBD key to resolve`. The target requeues after 15 seconds, and `ConfigProjected` is not observed again until the gate is `True`. |
| `CredentialsReady` | CinderBackupBackend | False | `WaitingForParent` | No Cinder of the name in `spec.cinderRef` exists, so which cluster the backup service runs on is unknown. The CR requeues after 15 seconds. |
| `CredentialsReady` | CinderBackupBackend | False | `TargetClusterUnavailable` | The parent Cinder's `spec.targetClusterRef` names a target cluster that is not registered or no longer resolves. See [Target Clusters](../target-clusters.md). |
| `ConfigProjected` | CinderBackupBackend | True | `ConfigProjected` | The `backup` volume of the `cinder-backup` Deployment references the Secret rendered for this backend. The controller compares the name of that Secret and does not read it. |
| `ConfigProjected` | CinderBackupBackend | False | `WaitingForProjection` | The projection has not landed in the Deployment yet, or the parent Cinder no longer exists and the standing claim is stale. |
| `Ready` | CinderBackupBackend | True | `AllReady` | Both sub-conditions are True. |
| `Ready` | CinderBackupBackend | False | `NotAllReady` | At least one sub-condition is not True. |
| `BackupBackendReady` | Cinder | True | `BackupBackendProjected` | The single attached, credential-ready backup backend is rendered. |
| `BackupBackendReady` | Cinder | True | `NoBackupBackend` | No CinderBackupBackend is attached. Backups are opt-in, and a Cinder without them still serves volumes. |
| `BackupBackendReady` | Cinder | False | `WaitingForBackupBackend` | The attached backup backend is not yet credential-ready, or was skipped for a fault; the message names it. |
| `BackupBackendReady` | Cinder | False | `MultipleBackupBackends` | More than one attached backup backend is credential-ready; nothing is rendered. |

`status.observedGeneration` carries the `metadata.generation` of the last spec
the controller observed (`cinderbackupbackend_types.go`), so a status reported
against an older spec is distinguishable from a current one.

A backup backend that carries no `spec.nfs` or `spec.rbd` block matching its
type, whose rendered files carry a control character, or whose RBD key Secret
vanished or stopped carrying a cephx key between its gate and the render is
skipped: the cinder-side step emits a `CinderBackupBackendSkipped` Warning event
on the Cinder CR, naming the Secret and the data key but never a key, and leaves
`BackupBackendReady` in its waiting state. The `CinderBackupBackend` controller
emits no events of its own.

## Retained Artefacts

The rendered driver section lives in a content-hashed
`<cinder>-backup-<name>-<hash>` Secret owned by the parent Cinder. Up to 3
historical copies are retained for fast rollback, and all of them are collected
with the Cinder CR.

| Data key | Content | Mounted at |
| --- | --- | --- |
| `backup.conf` | The `[DEFAULT]` section above | `/etc/cinder/backup.conf.d/backup.conf` |
| `ceph.conf` | The Ceph client configuration above | `/etc/ceph/<cluster>.conf` |
| `keyring` | The keyring of `client.<user>` | `/etc/ceph/<cluster>.client.<user>.keyring` |

An NFS target's Secret carries `backup.conf` alone; `ceph.conf` and `keyring`
exist on RBD targets only.

The base name carries this CR's name, so a replacement backup backend renders
under its own base name. A backend that detached, was replaced or was skipped
keeps no Deployment, and its base name is then swept whole: nothing mounts those
Secrets anymore, and each of them names the target a restore would read (an NFS
export, or an RBD pool together with the cephx key that opens it).

## Immutability and Validation Summary

Schema-layer rules (CEL and kubebuilder markers): the `cinderRef` and `type`
transition rules, the two-sided union of `type` with `nfs` and `rbd`, the
`NFS`/`RBD` type enum, the `server` pattern and `MinLength`, the absolute-path
and printable-ASCII pattern on `path`, the newline pattern on `mountOptions`,
the patterns and bounds of `pool`, `user`, `monitors`, `networks`,
`clusterName` and `keySecretRef.name`, the CEL rule against the `client.` prefix
on `user`, the `clusterName` default, the `fileSize` minimum, multiple and
default, the `compression` enum and default, and the `extraOptions`
`MaxProperties` and key/value length bounds.

Webhook rules (defense in depth plus the rules CEL cannot express): the union
re-check of both blocks, the `fileSize` bounds, the `compression` enum, the
printable-ASCII guard on `path` and the newline guard on `mountOptions`, the
`client.` prefix twin on `user`, the newline guard on `pool`, `user`,
`clusterName` and every monitor, a monitor port between 1 and 65535, a
canonical IPv4 CIDR per network, the DNS-1123 check on `keySecretRef.name`, the
`extraOptions` key charset, the per-type denylist and the INI-injection guard,
and the single-attachment check against the namespace siblings. The defaulting
webhook fills `fileSize`, `compression`, `nfs.mountOptions` and
`rbd.clusterName` for callers that bypass the schema.

## Chainsaw E2E Tests

The rejection corpus lives in `tests/e2e/cinder/invalid-cinderbackupbackend-cr`:
the union rule, the chunk-size floor, the compression enum, the `extraOptions`
denylist, the `cinderRef` transition rule, the single-attachment rule, and an
export path carrying a newline. The RBD entries add both halves of the union
with the `rbd` block, the `client.` prefix, an empty monitor list, a network
that is no CIDR, a key Secret name that is no DNS-1123 subdomain,
`backup_ceph_pool` in `extraOptions` on an RBD target, and the `type` transition
rule as an update from `NFS` to `RBD`.

The [`backup-rbd` suite](../testing/cinder-e2e-tests.md#backup-rbd) proves the
gate, the projected driver section and Ceph files, the volume backend's keyring
in the backup pod, the Ceph egress union and the roll on a replaced backup key
and on a replaced volume key, on a kind cluster that runs no Ceph.
