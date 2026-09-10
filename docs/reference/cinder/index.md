---
title: Cinder Operator
quadrant: operator
---

# Cinder Operator

The Cinder operator runs the OpenStack block-storage service: the API under
uWSGI, the scheduler, one `cinder-volume` process per attached
[`CinderBackend`](./cinder-backend-crd.md), and `cinder-backup` while a
[`CinderBackupBackend`](./cinder-backup-backend-crd.md) is attached. All three
kinds live in `cinder.openstack.c5c3.io/v1alpha1`. `Cinder` describes the
service, and the two satellites describe storage: a volume backend and the one
backup driver. Phase 1 ships the NFS driver for both.

Storage attaches out-of-band, the inverted attachment
[`GlanceBackend`](../glance/glance-backend-crd.md) established: the backend
points at the `Cinder`, so adding or removing storage never edits the `Cinder`
CR.

## Processes

Four long-running processes carry the service. The rendered `[DEFAULT] host` is
the CR's name, and the three processes that hold a row in cinder's service
registry key that row on an identity derived from it.

| Process | Workload | Identity | What it does |
| --- | --- | --- | --- |
| uWSGI on `cinder.wsgi.api` | the `{name}` Deployment | `{name}` (registers no row) | The block-storage API on port 8776 |
| `cinder-scheduler` | the `{name}-scheduler` Deployment | `{name}-scheduler` | Picks a backend for every volume request the API accepts |
| `cinder-volume` | one `{name}-volume-{backend}` Deployment per attached `CinderBackend` | `{name}@{backend}` | Creates and serves the volumes of its one backend |
| `cinder-backup` | the `{name}-backup` Deployment | `{name}-backup` | Writes and restores volume backups |

A volume backend's identity is what the volumes it holds are keyed by, so it is
derived from the CR and never from a pod: a Deployment roll, a rescheduled pod
and a replaced node all keep it. The three non-API processes serve no HTTP port.
Their readiness probe is `cinder-amqp-ready`, which reports whether a process of
the container holds a socket established to the broker port (see
[Container Images](../ci-cd/container-images.md#cinder)).

## Design decisions

The decision record lives in meta issue
[#979](https://github.com/C5C3/cobaltcore/issues/979). These are the entries
this operator implements, together with the choices the CRD carries.

- The message bus is required. `spec.messaging` has no optional form: the API
  hands every volume request to the scheduler over the bus, the scheduler hands
  it to a `cinder-volume`, and backup jobs travel the same way. The URL reaches
  all four processes and every migration Job as the `OS_DEFAULT__TRANSPORT_URL`
  environment override sourced from the derived `{name}-transport-url` Secret,
  so the broker password stays out of the rendered configuration. Volume
  lifecycle notifications are dropped at the source
  (`[oslo_messaging_notifications] driver = noop`), because nothing in this
  deployment consumes them.
- No default backend. A volume type selects its backend by name, so the
  backends a `Cinder` serves are peers, and an empty set is a valid state rather
  than a violated invariant: `BackendsReady` is then True under the reason
  `NoBackends`. That is the one place the satellite shape departs from
  `GlanceBackend`, which elects exactly one default store.
- One immutable Secret per backend. Each backend renders into its own
  content-hashed `{name}-backend-{backend}-<hash>` Secret, which only that
  backend's `cinder-volume` Deployment mounts, so attaching a second backend
  leaves the first one's pods untouched.
- Inline CSI volumes for every share. A backend's export is mounted through an
  inline `nfs.csi.k8s.io` volume at `/var/lib/cinder/mnt/<md5 of server:path>`,
  the path os-brick's remotefs driver derives and cinder resolves every volume's
  provider location to. No PersistentVolume is bound and no cluster-scoped RBAC
  is needed, and a detaching backend leaves nothing to reclaim. The kind stack
  has to enable the driver's inline mode, which the CI wiring of
  [#988](https://github.com/C5C3/cobaltcore/issues/988) owns.
- Detaching a backend runs a Job. A deleted `CinderBackend` is held by the
  `cinder.openstack.c5c3.io/service-remove` finalizer until its volume service
  is stopped and a `{name}-{backend}-service-remove` Job has run
  `cinder-manage service remove cinder-volume {name}@{backend}` against the
  database. Exit code 2 ("host not found") counts as success: a backend whose
  `cinder-volume` never registered has no row to remove and must still detach.
- The privsep posture is unprivileged (decision D3). Both privsep contexts,
  `[cinder_sys_admin]` and `[privsep_osbrick]`, are rendered with an empty
  `capabilities` value, because the pod's security context grants none, and
  `nas_secure_file_operations = true` is written explicitly so the driver runs
  its file operations as the service user instead of through a root helper the
  image has no sudoers entry for.
- Configuration reaches the processes as directories. Every process is launched
  with `--config-dir /etc/cinder/cinder.conf.d`, which holds a file named
  `cinder.conf`, plus the directories that carry its own overlay: the scheduler
  reads `scheduler.conf.d`, a volume service reads `backends.conf.d` and
  `volume.conf.d`, the backup service reads `backup.conf.d`. oslo.config reads
  every file in a directory, so the split is what keeps one process from
  registering under another's identity.
- A release bump rolls the processes in pipeline order: scheduler, volume,
  backup, then API. After the contract phase the three non-API Deployments roll
  once more, because the `cinder.c5c3.io/installed-release` pod annotation
  changes with `status.installedRelease`. Each of them caches the RPC version
  its peers announced at startup, and that cache pins the wire format for the
  life of the process.

## Owned resources

For a `Cinder` named `{name}`, with `{backend}` and `{backupBackend}` the names
of the attached satellites:

| Resource | Name | Purpose |
| --- | --- | --- |
| Deployment | `{name}` | The API pods, uWSGI on port 8776 |
| Service | `{name}` | ClusterIP in front of the API pods on port 8776 |
| PodDisruptionBudget | `{name}` | `minAvailable: 1` above one replica, `maxUnavailable: 1` at one; selects the API component and excludes Job pods |
| HorizontalPodAutoscaler | `{name}` | Only while `spec.autoscaling` is set; the API is the only autoscaled Deployment |
| NetworkPolicy | `{name}` | Only while `spec.networkPolicy` is set; one policy covers all four workloads and the Job pods |
| HTTPRoute | `{name}` | Only while `spec.gateway` is set |
| Deployment | `{name}-scheduler` | `cinder-scheduler` |
| Deployment | `{name}-volume-{backend}` | The `cinder-volume` of one attached backend |
| Deployment | `{name}-backup` | `cinder-backup`, only while a backup backend is projected |
| ConfigMap | `{name}-config-<hash>` | Immutable, content-addressed `cinder.conf` and `scheduler.conf`, plus `logging.ini` and `policy.yaml` when applicable; 3 historical retained |
| Secret | `{name}-backend-{backend}-<hash>` | Immutable, content-addressed `backend.conf`, `shares` and `volume.conf` of one backend; 3 historical retained |
| Secret | `{name}-backup-{backupBackend}-<hash>` | Immutable, content-addressed `backup.conf`; 3 historical retained |
| Secret | `{name}-db-connection` | The derived pymysql DSN, consumed through `OS_DATABASE__CONNECTION` |
| Secret | `{name}-transport-url` | The derived `rabbit://` URL, consumed through `OS_DEFAULT__TRANSPORT_URL` |
| Job | `{name}-db-sync` | `cinder-manage db sync` |
| Job | `{name}-db-expand`, `{name}-db-migrate`, `{name}-db-contract` | The three release-upgrade phases |
| CronJob | `{name}-db-purge` | The recurring `cinder-manage db purge` |
| Job | `{name}-{backend}-service-remove` | Unregisters one detaching backend's volume service |
| MariaDB `Database` / `User` / `Grant` | `{name}` | Managed mode only (`spec.database.clusterRef`); a brownfield database is left alone |

## Reference pages

- [Cinder CRD](./cinder-crd.md): the `spec`/`status` contract, the rendered
  defaults, and the validation rules
- [CinderBackend CRD](./cinder-backend-crd.md): one volume backend, its
  conditions, and the detach contract
- [CinderBackupBackend CRD](./cinder-backup-backend-crd.md): the single backup
  driver and its conditions
- [Controller Events](./cinder-events.md): the Kubernetes events the three
  controllers emit
- [Reconciler Architecture](./cinder-reconciler.md): the sub-reconciler
  pipeline, conditions, and requeue semantics
