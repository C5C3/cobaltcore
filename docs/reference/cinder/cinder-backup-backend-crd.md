---
title: CinderBackupBackend CRD API Reference
quadrant: operator
---

# CinderBackupBackend CRD API Reference

Reference documentation for the CinderBackupBackend Custom Resource Definition.
One CR attaches to a [Cinder](./cinder-crd.md) CR via `spec.cinderRef` and
describes the driver the backup service writes volume backups through. Phase 1
ships the NFS driver (`type: NFS`).

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

## Spec

### CinderBackupBackendSpec

Three schema-level CEL rules hold even when the webhook is down: `cinderRef` and
`type` are immutable (UPDATE transition rules), and the `type`/`nfs` union rule
`(self.type == 'NFS') == has(self.nfs)` enforces exactly one backup backend
block matching `spec.type`.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `cinderRef` | `CinderRefSpec` | Yes | | Names the Cinder CR in the same namespace (`name`, `MinLength=1`). Immutable: re-pointing the driver at a different Cinder would leave the backups it already wrote recorded against a deployment that cannot read them. The Cinder need not exist at admission time; a dangling reference surfaces as `Ready=False`. |
| `type` | `CinderBackupBackendType` | Yes | | The backup driver; `NFS` only (enum). Immutable, because a restore reads a backup with the driver that wrote it. |
| `nfs` | `NFSBackupBackendSpec` | When `type: NFS` | | The NFS export the chunks are written to (union rule). Its three fields (`server`, `path`, `mountOptions`) carry the same types, patterns and default as [NFSBackendSpec](./cinder-backend-crd.md#nfsbackendspec). |
| `fileSize` | `*int64` | No | `52428800` | The size in bytes of one backup chunk (`backup_file_size`). Cinder splits a volume into objects of this size and writes them one at a time, so it bounds both the memory a backup holds and the work a failed chunk costs. `Minimum=1048576` and `MultipleOf=32768`: cinder hashes each chunk in blocks of `backup_sha_block_size_bytes` (32 KiB) and refuses a chunk size that block size does not divide. |
| `compression` | `string` | No | `zlib` | The algorithm each chunk is compressed with (`backup_compression_algorithm`); one of `none`, `zlib`, `bz2`, `zstd` (enum). `none` trades backup capacity for CPU on the backup pod. |
| `extraOptions` | `map[string]string` | No | | Free-form backup-section options not covered by the typed fields, keyed by bare option name. `MaxProperties=32`, and a CEL rule bounds each key at 256 and each value at 1024 characters. See the [denylist](#extraoptions-denylist). |

## extraOptions Denylist

The validating webhook rejects the option names the projection owns. Every key
must first match `^[A-Za-z0-9_]+$`, and a value carrying a newline or carriage
return is rejected as an INI-injection guard.

| Group | Rejected keys |
| --- | --- |
| Rendered from typed fields | `backup_driver`, `backup_share`, `backup_mount_options`, `backup_file_size`, `backup_compression_algorithm` |
| Operator-owned | `host`, `backup_mount_point_base`, `backup_use_same_host` |

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
`--config-dir` beside the shared one, so the keys land on the backup pod and
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
backup backend keeps the existing backups restorable. `backup_use_same_host` is
`false` because the backup service owns its target through that identity and a
backup is never handed to another host.

The backup pod mounts two kinds of export: its own target under
`/var/lib/cinder/backup_mount/<md5 of "server:path">`, and every volume
backend's export under `/var/lib/cinder/mnt/<md5>`. A backup reads the volume
itself through os-brick rather than a copy the volume service hands over, so it
needs the source at the same path cinder resolved the volume's provider location
to.

A restore writes into an existing volume: it is a request against a volume that
is already there, not a way to recreate one that is gone (decision D6 of issue
[#979](https://github.com/C5C3/cobaltcore/issues/979)). Plan a recovery
accordingly, by creating the target volume first.

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

## Conditions

The dedicated `CinderBackupBackendReconciler` is the single writer of this
status, and it carries the `CinderBackend` vocabulary: both satellites answer
the same two questions, so an operator reads one status shape whichever kind is
in front of them. The cinder-side sub-reconciler only reads `CredentialsReady`
and writes the aggregated `BackupBackendReady` condition onto the Cinder CR.

| Type | Owner | Status | Reason | Meaning |
| --- | --- | --- | --- | --- |
| `CredentialsReady` | CinderBackupBackend | True | `CredentialsNotRequired` | The NFS export is mounted with the pod's own identity, so there is no credential to resolve. |
| `CredentialsReady` | CinderBackupBackend | False | `WaitingForParent` | No Cinder of the name in `spec.cinderRef` exists, so which cluster the backup service runs on is unknown. The CR requeues after 15 seconds. |
| `CredentialsReady` | CinderBackupBackend | False | `TargetClusterUnavailable` | The parent Cinder's `spec.targetClusterRef` names a target cluster that is not registered or no longer resolves. See [Target Clusters](../target-clusters.md). |
| `ConfigProjected` | CinderBackupBackend | True | `ConfigProjected` | The `cinder-backup` Deployment mounts a `backup.conf` carrying this backend's rendered section. |
| `ConfigProjected` | CinderBackupBackend | False | `WaitingForProjection` | The projection has not landed in the Deployment yet, or the parent Cinder no longer exists and the standing claim is stale. |
| `Ready` | CinderBackupBackend | True | `AllReady` | Both sub-conditions are True. |
| `Ready` | CinderBackupBackend | False | `NotAllReady` | At least one sub-condition is not True. |
| `BackupBackendReady` | Cinder | True | `BackupBackendProjected` | The single attached, credential-ready backup backend is rendered. |
| `BackupBackendReady` | Cinder | True | `NoBackupBackend` | No CinderBackupBackend is attached. Backups are opt-in, and a Cinder without them still serves volumes. |
| `BackupBackendReady` | Cinder | False | `WaitingForBackupBackend` | The attached backup backend is not yet credential-ready, or was skipped for a fault; the message names it. |
| `BackupBackendReady` | Cinder | False | `MultipleBackupBackends` | More than one attached backup backend is credential-ready; nothing is rendered. |

A backup backend whose `spec.nfs` block is absent, or whose rendered section
carries a control character, is skipped: the cinder-side step emits a
`CinderBackupBackendSkipped` Warning event on the Cinder CR and leaves
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

The base name carries this CR's name, so a replacement backup backend renders
under its own base name. A backend that detached, was replaced or was skipped
keeps no Deployment, and its base name is then swept whole: nothing mounts those
Secrets anymore, and each of them names the export a restore would read.

## Immutability and Validation Summary

Schema-layer rules (CEL and kubebuilder markers): the `cinderRef` and `type`
transition rules, the `type`/`nfs` union, the `NFS` type enum, the `server`
pattern and `MinLength`, the absolute-path and printable-ASCII pattern on
`path`, the newline pattern on `mountOptions`, the `fileSize` minimum, multiple
and default, the `compression` enum and default, and the `extraOptions`
`MaxProperties` and key/value length bounds.

Webhook rules (defense in depth plus the rules CEL cannot express): the union
re-check, the `fileSize` bounds, the `compression` enum, the printable-ASCII
guard on `path` and the newline guard on `mountOptions`, the `extraOptions` key
charset, denylist and INI-injection guard, and the single-attachment check
against the namespace siblings. The defaulting webhook materializes `fileSize`,
`compression` and `nfs.mountOptions` for callers that bypass the schema.

## Chainsaw E2E Tests

The rejection corpus lives in `tests/e2e/cinder/invalid-cinderbackupbackend-cr`.
