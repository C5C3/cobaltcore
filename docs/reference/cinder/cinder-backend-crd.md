---
title: CinderBackend CRD API Reference
quadrant: operator
---

# CinderBackend CRD API Reference

Reference documentation for the CinderBackend Custom Resource Definition. One CR
attaches to a [Cinder](./cinder-crd.md) CR via `spec.cinderRef` and describes one
volume backend. Phase 1 ships the NFS driver (`type: NFS`).

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

## Spec

### CinderBackendSpec

Three schema-level CEL rules hold even when the webhook is down: `cinderRef` and
`type` are immutable (UPDATE transition rules), and the `type`/`nfs` union rule
`(self.type == 'NFS') == has(self.nfs)` enforces exactly one backend block
matching `spec.type`.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `cinderRef` | `CinderRefSpec` | Yes | | Names the Cinder CR in the same namespace (`name`, `MinLength=1`). Immutable: re-pointing a backend would strand the volumes the old deployment created under a host identity nothing serves. The Cinder need not exist at admission time (GitOps ordering); a dangling reference surfaces as `Ready=False`. |
| `type` | `CinderBackendType` | Yes | | The volume driver; `NFS` only (enum). Immutable, because the volumes already on the backend were created by the driver that is being replaced. |
| `nfs` | `NFSBackendSpec` | When `type: NFS` | | The NFS export (union rule). |
| `imageVolumeCache` | `ImageVolumeCacheSpec` | No | | Turns on the per-backend image-volume cache. |
| `extraOptions` | `map[string]string` | No | | Free-form `[<name>]` section options not covered by the typed fields, keyed by bare option name. `MaxProperties=32`, and a CEL rule bounds each key at 256 and each value at 1024 characters. See the [denylist](#extraoptions-denylist). |

### NFSBackendSpec

| Field | Type | Required | Default | Rendered option | Description |
| --- | --- | --- | --- | --- | --- |
| `server` | `string` | Yes | | `nfs_shares_config` (first half) | The NFS server, a hostname or an IP address. It reaches the mount command verbatim, so the pattern `^[A-Za-z0-9.-]+$` admits only what a hostname or an IPv4 address carries; `MinLength=1` |
| `path` | `string` | Yes | | `nfs_shares_config` (second half) | The absolute export path on the server; pattern `^/[!-~]*$`, printable ASCII only. The exclusion is what keeps the shares file one export long and that export intact: the operator writes `server:path` into it verbatim, and the driver strips the line it reads back and cuts it at the first space. It is an allowlist because a schema pattern compiles with RE2, whose `\s` is the five ASCII characters, while the driver strips the full Unicode whitespace set — so a pasted U+00A0 would resolve to a different export there than the one mounted here |
| `mountOptions` | `string` | No | `nfsvers=4.1,soft,timeo=30,retrans=2` | `nfs_mount_options` | The comma-separated option string the export is mounted with; pattern `^[^\n\r]*$` for the same reason. Keep the mount soft: a hard mount blocks the `cinder-volume` process on an unreachable server instead of failing the request |

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

| Group | Rejected keys |
| --- | --- |
| Rendered from typed fields | `volume_driver`, `nfs_shares_config`, `nfs_mount_options`, `image_volume_cache_enabled`, `image_volume_cache_max_size_gb`, `image_volume_cache_max_count` |
| Operator-owned | `volume_backend_name`, `backend_host`, `nfs_mount_point_base`, `nas_secure_file_operations`, `nas_secure_file_permissions`, `nfs_snapshot_support`, `nfs_sparsed_volumes`, `nfs_qcow2_volumes` |

A key that survives both gates is merged into the rendered section without
overriding an operator key: the denylist normally makes the two disjoint, and
the merge is the fail-closed backstop for a CR written past admission.

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

Three caveats come with this driver:

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

## Detaching a backend

The controller holds a deleted `CinderBackend` with the finalizer
`cinder.openstack.c5c3.io/service-remove`, and the cinder-side step is what
releases it. A running `cinder-volume` reports itself into the service registry
every few seconds, so an entry removed while the process still runs comes back.
The order is therefore fixed: the volume Deployment is deleted, a later pass
finds it gone and runs the `<cinder>-<name>-service-remove` Job, the projected
Secrets are swept, and only then is the finalizer released. While that runs, the
backend reports `Ready=False` under the reason `Detaching`.

The Job runs `cinder-manage service remove cinder-volume <cinder>@<name>`.
Exit code 2 is "host not found", which is the state of a backend whose
`cinder-volume` never registered, so the script normalises it to success and
lets every other code fail the Job. A failed Job keeps the finalizer: the
registry entry is still there, and deleting the Job is what retries the removal
once the cause is understood. The parent then reports
`VolumeServicesReady=False` under `ServiceRemoveJobFailed`.

A backend whose parent Cinder is already gone is released unconditionally, with
a `ServiceRemoveSkipped` event: the registry lives in that Cinder's database, so
there is no row left to remove.

## Conditions

The dedicated `CinderBackendReconciler` is the single writer of this status. The
cinder-side sub-reconciler only reads `CredentialsReady`, which gates the
projection, and writes the aggregated `BackendsReady` condition onto the Cinder
CR instead.

| Type | Owner | Status | Reason | Meaning |
| --- | --- | --- | --- | --- |
| `CredentialsReady` | CinderBackend | True | `CredentialsNotRequired` | The NFS export is mounted with the pod's own identity, so there is no credential to resolve. The condition is still reported, because the cinder-side projection gates on it and an absent one reads as not ready. |
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

A backend whose `spec.nfs` block is absent, or whose rendered section carries a
control character, is skipped: the cinder-side step emits a
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

The `shares` key is mounted under the backend's own name, which is the path
`nfs_shares_config` points at, so the file is readable in a process that serves
one backend.

A backend that detached, was renamed or was skipped keeps no Deployment, so its
base name is swept whole rather than retained: nothing mounts those Secrets
anymore, and each of them names an export the Cinder no longer serves.

## Immutability and Validation Summary

Schema-layer rules (CEL and kubebuilder markers, enforced even when the webhook
is down): the `cinderRef` and `type` transition rules, the `type`/`nfs` union,
the `NFS` type enum, the `server` pattern and `MinLength`, the absolute-path and
printable-ASCII pattern on `path`, the newline pattern on `mountOptions`, the
`mountOptions` default, the two image-volume-cache minimums, and the
`extraOptions` `MaxProperties` and key/value length bounds.

Webhook rules (defense in depth plus the rules CEL cannot express): the union
re-check, the reserved-name collision guard and the `@`/`#` guard on
`metadata.name`, the combined 47-character name budget and the 35-character
bound on `metadata.name` alone, the printable-ASCII guard on `path` and the
newline guard on `mountOptions`, the `extraOptions` key charset, denylist and
INI-injection guard, and the image-volume-cache warning.
The `mountOptions` default is materialized by the defaulting webhook for callers
that bypass the schema.

## Chainsaw E2E Tests

The rejection corpus lives in `tests/e2e/cinder/invalid-cinderbackend-cr`: the
union rule, the reserved name, a relative export path, an export path carrying a
newline, both `extraOptions` guards, the `cinderRef` transition rule, and the two
name bounds.
