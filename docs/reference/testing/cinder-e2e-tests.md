---
title: Cinder E2E Test Suites
quadrant: operator
---

# Cinder E2E Test Suites

Reference documentation for the Cinder Chainsaw E2E test suites. These tests
validate the CinderReconciler's end-to-end behavior in a real Kubernetes cluster
with the infrastructure dependencies deployed (MariaDB, Memcached, ESO, OpenBao,
the kind NFS server and the kind RabbitMQ broker).

For the CRD validation E2E tests, see
[Cinder CRD](../cinder/cinder-crd.md),
[CinderBackend CRD API Reference](../cinder/cinder-backend-crd.md#chainsaw-e2e-tests)
and
[CinderBackupBackend CRD API Reference](../cinder/cinder-backup-backend-crd.md#chainsaw-e2e-tests).
For the reconciler architecture and sub-reconciler contracts, see
[Cinder Reconciler Architecture](../cinder/cinder-reconciler.md). For
infrastructure deployment automation, see
[Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md).

## Overview

The suites cover the reconciler lifecycle from a first deployment through backend
attach and detach, a backup round trip, scaling, a cross-release upgrade and
deletion. Each suite creates its own `Cinder` CR in the shared `openstack`
namespace. Two suites sit outside it: `pod-security-restricted` owns a
`cinder-pss` namespace labelled with the restricted profile, and
`cinder-operator/metrics` installs a second cinder-operator Helm release in
`cinder-system`.

Satellite objects carry a per-suite name prefix (`basic-nfs1`, `nfsbe-nfs1`,
`multi-nfs-a`). `CinderBackend.spec.cinderRef` is immutable, so a backend named
after its driver alone would be one object several suites apply, delete and
re-point at each other's `Cinder`. The CI leg narrows Chainsaw to
`--parallel 2` instead of the shared config's four: a cinder suite is three or
four Deployments, a db-sync Job and a probe pod, and four of those at once do
not fit a 4-vCPU kind node.

### One RabbitMQ vhost per suite

`tests/e2e/cinder/broker-vhost.sh` gives each suite a private queue namespace on
the kind-only `shared-rabbitmq` broker. The `create` form reads the broker's
default-user Secret name from `status.defaultUser.secretReference.name`, runs
`rabbitmqctl add_vhost` and `rabbitmqctl set_permissions` inside the broker pod,
and applies a Secret whose `transport_url` key holds
`rabbit://<user>:<password>@shared-rabbitmq.openstack.svc.cluster.local:5672/<vhost>`.
Every suite names its CR as the vhost and `<cr>-messaging` as the Secret, which
the `Cinder` CR then references through `spec.messaging.secretRef`. The managed
mode (`messaging.clusterRef`) does not work here: `BuildTransportURL` renders its
path as `/`, which would put every suite on the default vhost.

The vhost is what keeps the suites apart. Cinder's RPC topics are named after the
deployment: every scheduler consumes `cinder-scheduler` and every volume service
consumes `cinder-volume.<host>@<backend>`. Two suites on one vhost would share
those queues, so one suite's scheduler could answer the other's API. The first
step of a suite calls the `create` form; that step's `cleanup` block calls
`broker-vhost.sh delete <vhost> || true` at the end of the test. Every call site
carries that guard, and the script's header states it as the contract: a cleanup
block runs after its suite has reached a verdict, so a broker that is restarting
under the load of two parallel suites would otherwise redden a run whose
assertions all passed. A leaked vhost prints one `WARNING:` line and dies with
the kind cluster.

### No identity service

No workload suite sets `spec.keystoneEndpoint` or `spec.serviceUser`. The two are
set together or not at all, and leaving both out renders `auth_strategy = noauth`
with no `[keystone_authtoken]` section, which is how the storage path is
exercised on a cluster that runs no Keystone. HTTP probes therefore send
`X-Auth-Token: admin:admin`, the noauth pipeline's `<user>:<project>` pair, and
address project-less paths under
`http://<cr>.openstack.svc.cluster.local:8776`. A project-scoped route takes a
hex project id, so `basic-deployment` asserts that `/v3/admin/volumes` is a 404.

The probe itself runs in a pod of its own. The script step starts it with
`kubectl run <name> --restart=Never --image=ghcr.io/c5c3/cinder:2025.2` against
`/var/lib/openstack/bin/python -c`, polls `.status.phase` until `Succeeded` or
`Failed`, reads the output back with `kubectl logs`, and greps for the sentinel
the Python script prints as its last line. `kubectl run -i` is not used: it
streams from the moment the attach lands, so a probe that finishes first would
lose its sentinel. Running the request from a pod also makes it cross kube-proxy
the way any other client's request does.

### The exports the suites mount

`WITH_NFS=true` installs the kind NFS server and csi-driver-nfs. The server
pre-creates `/volumes` and `/backups` as `42424:42424` mode `0770`, and `fsid=0`
makes `/exports` the NFSv4 pseudo-root, so a backend addresses
`nfs-server.openstack.svc.cluster.local:/volumes`. `multi-backend` creates a
third export, `/volumes-b`, in its first step and removes it in that step's
cleanup. The operator mounts an export as an inline CSI volume at
`/var/lib/cinder/mnt/<md5>`, the path os-brick's remotefs driver resolves every
volume of that share to, and the file assertions reach it with `kubectl exec`
into the `cinder-volume` pod that holds the mount.

## Prerequisites

| Prerequisite | Details |
| --- | --- |
| Infrastructure stack | `WITH_NFS=true WITH_MESSAGING=true make deploy-infra` (see [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md)) |
| Cinder operator | cinder-operator chart installed with the three CRDs registered and the webhooks active |
| ESO ExternalSecret | `cinder-db` synced in the `openstack` namespace |
| MariaDB instance | `openstack-db` MariaDB CR Ready in `openstack` |
| Memcached instance | `openstack-memcached` Memcached CR Ready in `openstack` |
| Message broker | `shared-rabbitmq` RabbitmqCluster in `openstack` (`WITH_MESSAGING=true`) |
| NFS export | `nfs-server` Deployment and csi-driver-nfs in `openstack` (`WITH_NFS=true`) |
| Service images | `ghcr.io/c5c3/cinder:2025.2` for every suite, plus `ghcr.io/c5c3/cinder:2026.1` for `basic-deployment-2026-1` and the target half of `release-upgrade` |
| Chainsaw | v0.2.14 |

## Running the Tests

```bash
# Run the Cinder suites the way the e2e-operator CI leg does
chainsaw test --config tests/e2e/chainsaw-config.yaml --parallel 2 \
  tests/e2e/cinder/ tests/e2e/cinder-operator/

# Run a single suite
chainsaw test --config tests/e2e/chainsaw-config.yaml \
  --test-dir tests/e2e/cinder/basic-deployment
```

## Chainsaw Configuration

All tests use the shared configuration at `tests/e2e/chainsaw-config.yaml`:

| Setting | Value | Purpose |
| --- | --- | --- |
| `timeouts.apply` | 30s | Resource application timeout |
| `timeouts.assert` | 120s | Default assertion timeout (every Cinder suite overrides it) |
| `timeouts.cleanup` | 3m | Post-test resource cleanup |
| `timeouts.delete` | 30s | Resource deletion timeout |
| `timeouts.error` | 30s | Error assertion timeout |
| `timeouts.exec` | 30s | Script execution timeout |
| `execution.parallel` | 4 | Maximum concurrent test suites (the CI leg narrows this to 2) |
| `execution.failFast` | true | Stop on first failure |
| `report.format` | JUNIT-TEST | JUnit XML output for CI |
| `report.path` | `_output/reports` | Report directory |

Every suite raises `timeouts.assert` above the shared 120 s, because the schema
migration, the API cold start and the NFS mount of the volume service all precede
the first assertion. Most settle on 5 minutes. `release-upgrade` takes 10 minutes
for the three phase Jobs and the three-Deployment rollout that follow the patch,
and `gateway-quick-start-smoke` takes 12. `deletion-cleanup` also raises
`timeouts.error` to 3 minutes for the MariaDB `Database`, `User` and `Grant`
deletions.

## Test Suite Inventory

| Suite | CR Name | Reconciler Behavior Validated |
| --- | --- | --- |
| [basic-deployment](#basic-deployment) | `cinder-basic` | Happy path on 2025.2: thirteen sub-conditions, the three Deployments and their owned children, the rendered `cinder.conf` and backend Secret, the API over HTTP |
| [basic-deployment-2026-1](#basic-deployment-2026-1) | `cinder-basic-2026-1` | The same assertions against the 2026.1 image, so a difference between the two releases fails here |
| [nfs-backend](#nfs-backend) | `cinder-nfs` | Volume data path on one export: create, extend, clone and delete, each read back through the mount |
| [multi-backend](#multi-backend) | `cinder-multi` | One `cinder-volume` Deployment per backend, a per-pod `enabled_backends` overlay, volume-type placement on the second export |
| [backend-detach](#backend-detach) | `cinder-detach` | `CinderBackend` deletion: volume Deployment removed first, service-remove Job run, finalizer released, service registry and `status.volumeServices` follow |
| [backup-nfs](#backup-nfs) | `cinder-backup` | Backup round trip on an NFS target: the two backup conditions, the `cinder-backup` Deployment, backup, restore, delete, detach |
| [scale](#scale) | `cinder-scale` | `spec.api.deployment.replicas` 3 → 5 → 1 with the PodDisruptionBudget policy flipping, scheduler and volume service left at one replica |
| [healthcheck](#healthcheck) | `cinder-health` | `CinderAPIReady=True/APIHealthy` and the cluster-local `status.endpoint` |
| [httproute](#httproute) | `cinder-route` | `spec.gateway` lifecycle: HTTPRoute created, `HTTPRouteNotAccepted` then `HTTPRouteAccepted`, deleted with the spec block |
| [network-policy](#network-policy) | `cinder-netpol` | Rendered NetworkPolicy: ingress on 8776, auto-derived DNS, database, cache, messaging and export egress, update and delete |
| [deletion-cleanup](#deletion-cleanup) | `cinder-cleanup` | Finalizer cleanup of every owned child and the MariaDB CRs; both satellites survive the parent and release on `ServiceRemoveSkipped` |
| [pod-security-restricted](#pod-security-restricted) | `cinder-pss` | Every Pod the reconciler projects admits under `pod-security.kubernetes.io/enforce=restricted`, with zero `FailedCreate` violations |
| [release-upgrade](#release-upgrade) | `cinder-upgrade` | Cross-release upgrade 2025.2 to 2026.1: phase progression, the three phase Jobs, the three Deployments, the API on the new release |
| [maintenance-endpoint-isolation](#maintenance-endpoint-isolation) | `cinder-isolation` | db-purge and service-remove pods never become API Service backends, and the Service is never left without any |
| [gateway-quick-start-smoke](#gateway-quick-start-smoke) | `cinder-smoke` | `curl -k https://cinder.127-0-0-1.nip.io/` answers HTTP 300 with the Cinder version document |
| [metrics](#metrics) | — (operator-level) | cinder-operator chart renders and removes the ServiceMonitor |
| [invalid-cr](../cinder/cinder-crd.md) | (rejected at admission) | `Cinder` rejection corpus: the release pattern, the image and database and cache and messaging union rules, the messaging TLS rule, the db-purge bounds, the Keystone-pairing rules, the volume replica cap, the backup strategy, both `extraConfig` guards, the name bound and the gateway rule |
| [invalid-cinderbackend-cr](../cinder/cinder-backend-crd.md#chainsaw-e2e-tests) | (rejected at admission) | `CinderBackend` rejection corpus: the union rule, the reserved name, a relative export path, an export path carrying a newline, both `extraOptions` guards, the `cinderRef` transition rule, and the two name bounds |
| [invalid-cinderbackupbackend-cr](../cinder/cinder-backup-backend-crd.md#chainsaw-e2e-tests) | (rejected at admission) | `CinderBackupBackend` rejection corpus: the union rule, the chunk-size minimum, the compression enum, the `extraOptions` denylist, the `cinderRef` transition rule, the second-backup rule, and an export path carrying a newline |

---

## Test Suite Details

### basic-deployment

**File:** `tests/e2e/cinder/basic-deployment/chainsaw-test.yaml`

**Purpose:** Validates the full reconciliation cycle of a noauth Cinder on the
2025.2 release with one NFS backend attached. All thirteen sub-conditions reach
True, the API Deployment runs the uWSGI command the image supports, the volume
Deployment is the single-writer shape the NFS drivers require, and the API
answers over HTTP.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` | `broker-vhost.sh create cinder-basic cinder-basic-messaging openstack`, then `00-cinder-cr.yaml` (`cinder-basic`) and `01-cinderbackend-cr.yaml` (`basic-nfs1`). The step cleanup deletes the vhost at the end of the test |
| 2 | Assert every sub-condition, Ready and the volume services | `assert` (5m) | Thirteen sub-conditions True with their reasons (`SecretsAvailable`, `AllBackendsProjected`, `NoBackupBackend`, `DatabaseSynced`, `SchedulerReady`, `AllVolumeServicesReady`, `BackupNotConfigured`, `DeploymentReady`, `APIHealthy`, `HPANotRequired`, `NetworkPolicyNotRequired`, `HTTPRouteNotRequired`, `DBPurgeScheduled`), `Ready=True/AllReady`, and `status.volumeServices` carrying host `cinder-basic@basic-nfs1` |
| 3 | Assert the three Deployments, the Service, PDB and CronJob | `assert` (5m) | API Deployment: one container running `--module cinder.wsgi.api:application` with `--pyargv --config-dir /etc/cinder/cinder.conf.d`, `OS_DATABASE__CONNECTION` and `OS_DEFAULT__TRANSPORT_URL` sourced from Secrets, no `OS_KEYSTONE_AUTHTOKEN__PASSWORD`, `availableReplicas: 2`. Scheduler Deployment: readiness off `cinder-amqp-ready` and a `scheduler-config` overlay mount. Volume Deployment: `replicas: 1`, `strategy.type: Recreate`, the export as an `nfs.csi.k8s.io` inline volume with mount options `nfsvers=4.1,soft,timeo=30,retrans=2`, mounted at `/var/lib/cinder/mnt/6f3cb55ed3b423dbb7791aaf3783754f`. Service on port 8776, PDB `minAvailable: 1` selecting the API component, CronJob `cinder-basic-db-purge` on `1 0 * * *` running `cinder-manage db purge 30`. An `error` assertion pins the absence of `cinder-basic-backup` |
| 4 | Assert the rendered cinder.conf | `script` | Resolves the content-hashed ConfigMap through the API Deployment's `config` volume, then matches `auth_strategy = noauth`, `host = cinder-basic`, `api_paste_config`, `resource_query_filters_file` and `driver = noop`, and fails on a `[keystone_authtoken]` section or an `enabled_backends` key. A second script cuts `[privsep_osbrick]` out and matches the helper command and an empty capabilities list |
| 5 | Assert the rendered backend Secret | `script` | Resolves the content-hashed Secret through the volume Deployment's `backend` volume, then matches `[basic-nfs1]`, `volume_driver = cinder.volume.drivers.nfs.NfsDriver`, `backend_host = cinder-basic`, `nfs_shares_config`, `nas_secure_file_operations = true` and `nfs_snapshot_support = false`. The `shares` key and the `volume.conf` overlay are diffed in full against one export line and `enabled_backends = basic-nfs1` |
| 6 | Assert the Cinder API answers over HTTP | `script` (6m) | Probe pod: version document 300 with v3.0 `CURRENT`, `/healthcheck` 200 `OK`, `/v3/resource_filters` at microversion `volume 3.33`, `/v3/os-services` with `cinder-scheduler` and `cinder-basic@basic-nfs1` both `up`, an empty `/v3/volumes`, and 404 for `/v3/admin/volumes`. Sentinel `BASIC-PROBE-OK` |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`

---

### basic-deployment-2026-1

**File:** `tests/e2e/cinder/basic-deployment-2026-1/chainsaw-test.yaml`

**Purpose:** The per-release twin of `basic-deployment`. Nothing in the config
step reads `spec.openStackRelease`, so the operator renders the same files for
both releases and this suite asserts the same uWSGI command, the same backend
wiring and the same `cinder.conf` shape against the 2026.1 image. A difference
between the two suites is the failure it exists to catch. The CR pins
`image.tag: "2026.1"`, so a release bump that forgot the tag surfaces here rather
than in a passing 2025.2 run.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` | `broker-vhost.sh create cinder-basic-2026-1 …`, then `00-cinder-cr.yaml` (`cinder-basic-2026-1`) and `01-cinderbackend-cr.yaml` (`basic-2026-1-nfs1`) |
| 2 | Assert every sub-condition, Ready and the volume services | `assert` (5m) | The thirteen sub-conditions, `Ready=True/AllReady`, and `status.volumeServices` carrying `cinder-basic-2026-1@basic-2026-1-nfs1` |
| 3 | Assert the three Deployments, the Service, PDB and CronJob | `assert` (5m) | The same workload shape as `basic-deployment`, against the `:2026.1` image |
| 4 | Assert the rendered cinder.conf | `script` | The noauth pipeline, the privsep contexts, and no `enabled_backends` |
| 5 | Assert the rendered backend Secret | `script` | Driver wiring, the export line and the `enabled_backends` overlay of `basic-2026-1-nfs1` |
| 6 | Assert the Cinder API answers over HTTP | `script` (6m) | The same probe against the 2026.1 API |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`

---

### nfs-backend

**File:** `tests/e2e/cinder/nfs-backend/chainsaw-test.yaml`

**Purpose:** Drives one volume through its whole life and reads the export back
after every stage. `basic-deployment` stops at the empty volume list; this suite
creates, extends, clones and deletes, and each step derives the volume it works
on by name from `GET /v3/volumes`, because Chainsaw hands no values between
steps.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` | `broker-vhost.sh create cinder-nfs …`, then `00-cinder-cr.yaml` (`cinder-nfs`) and `01-cinderbackend-cr.yaml` (`nfsbe-nfs1`) |
| 2 | Assert Ready and the mounted export | `assert` (5m) | `Ready=True/AllReady`, and Deployment `cinder-nfs-volume-nfsbe-nfs1` mounting the export at `/var/lib/cinder/mnt/6f3cb55ed3b423dbb7791aaf3783754f` |
| 3 | Create a volume and read the file it became | `script` (8m) | A 1 GiB create reaches `available`, and `kubectl exec` into the volume pod stats the file as `42424:42424` mode 660 at its full apparent size |
| 4 | Extend the volume and read the file back | `script` (8m) | `os-extend` grows the same file to 2 GiB in place |
| 5 | Clone the volume and read both files | `script` (10m) | The clone lands beside its source, world-readable from `_set_rw_permissions_for_all`, with the source's `.info` snapshot file written alongside |
| 6 | Delete both volumes and assert the share is clean | `script` (8m) | Both volumes leave the API and neither file is left on the export |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`

**Design note:** `nas_secure_file_operations` and `nas_secure_file_permissions`
are both true, so the driver runs its file operations as the service user
instead of through a root helper. The ownership and mode in steps 3 to 5 are
that posture read off the share.

---

### multi-backend

**File:** `tests/e2e/cinder/multi-backend/chainsaw-test.yaml`

**Purpose:** One Cinder serving two `CinderBackend` objects on two different
exports. The operator renders one `cinder-volume` Deployment per backend, each
pod mounts its own export and no other, and a volume created through a volume
type whose `volume_backend_name` selects the second backend lands on the second
export alone.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Create the second export and the vhost, then apply the CRs | `script` (2m) + `apply` | Creates `/volumes-b` on the NFS server as `42424:42424` mode `0770`, takes the `cinder-multi` vhost, then applies `00-cinder-cr.yaml`, `01-cinderbackend-a-cr.yaml` (`multi-nfs-a`) and `02-cinderbackend-b-cr.yaml` (`multi-nfs-b`). The step cleanup removes the export and the vhost |
| 2 | Assert Ready and one Deployment per backend | `assert` (5m) | `BackendsReady=True/AllBackendsProjected`, `Ready=True/AllReady`, and the two volume Deployments mounting `/var/lib/cinder/mnt/6f3cb55ed3b423dbb7791aaf3783754f` and `/var/lib/cinder/mnt/e0c7531cb098b3f9cdce2c2a14ed80fd` |
| 3 | Assert each pod's overlay names its own backend alone | `script` | The `volume.conf.d` overlay of each pod lists one backend, so the two processes drive one backend each |
| 4 | Place a volume on the second backend through a volume type | `script` (10m) | A volume type with a `volume_backend_name` extra spec places the volume on `multi-nfs-b`; the file is stat'ed under the second mount and the first export is listed to show it is not there |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-a-cr.yaml`, `02-cinderbackend-b-cr.yaml`

---

### backend-detach

**File:** `tests/e2e/cinder/backend-detach/chainsaw-test.yaml`

**Purpose:** Two backends come up, one is deleted, and the suite follows the work
the operator has to do before it may release the `CinderBackend`: stop the volume
Deployment, run the service-remove Job, release the finalizer, and let the
service registry and `status.volumeServices` follow.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` | `broker-vhost.sh create cinder-detach …`, then `00-cinder-cr.yaml` (`cinder-detach`), `01-cinderbackend-a-cr.yaml` (`detach-nfs-a`) and `02-cinderbackend-b-cr.yaml` (`detach-nfs-b`) |
| 2 | Assert both backends are up before anything is deleted | `assert` (5m) | `Ready=True/AllReady` and both volume Deployments available |
| 3 | Delete one backend and assert the detach completed | `delete` (6m) + `error` + `assert` + `script` (3m) | `detach-nfs-b` is deleted and gone, Deployment `cinder-detach-volume-detach-nfs-b` is gone, Job `cinder-detach-detach-nfs-b-service-remove` succeeded, `VolumeServicesReady=True/AllVolumeServicesReady` and `Ready=True/AllReady` hold with the surviving backend alone. The script reads the Job UID out of the parent's dedupe annotation and finds the `ServiceRemoved` event on `cinder-detach` |
| 4 | Assert the API's own service registry agrees | `script` (8m) | `GET /v3/os-services` no longer lists the detached host, while the surviving one keeps reporting `up` |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-a-cr.yaml`, `02-cinderbackend-b-cr.yaml`

**Design note:** The volume Deployment is removed before the Job runs. A running
`cinder-volume` reports itself back into the service registry every few seconds,
so an entry removed under it would come back.

---

### backup-nfs

**File:** `tests/e2e/cinder/backup-nfs/chainsaw-test.yaml`

**Purpose:** One Cinder with a volume backend and a `CinderBackupBackend`
attached, driven through a full backup round trip: a marked volume is backed up,
the chunks are read off the target export, the backup is restored into a second
volume, and the backup backend is detached again.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` | `broker-vhost.sh create cinder-backup …`, then `00-cinder-cr.yaml` (`cinder-backup`), `01-cinderbackend-cr.yaml` (`backup-nfs1`) and `02-cinderbackupbackend-cr.yaml` (`backup-nfsbk`) |
| 2 | Assert the backup conditions and the volume service | `assert` (5m) | `BackupBackendReady=True/BackupBackendProjected`, `BackupServiceReady=True/BackupServiceReady`, `Ready=True/AllReady`, and the `CinderBackupBackend` itself `Ready=True/AllReady` |
| 3 | Assert the cinder-backup Deployment | `assert` (5m) | The single-writer shape with both exports mounted: its own target under `backup_mount`, and the volume backend's export at the path the volume service holds it at, because a backup reads the source volume itself |
| 4 | Assert the rendered backup section | `script` | The rendered `[DEFAULT]` section carries the NFS backup driver, the share, the chunk size, the compression algorithm and the host identity backups are recorded against |
| 5 | Create the source volume and mark its file | `script` (10m) | A volume reaches `available` and a byte marker is written into its file through the volume pod |
| 6 | Back the volume up and read the chunks off the target | `script` (12m) | The backup reaches `available` and its chunks land on the target export as `42424:42424` mode 660 in a directory named after the backup |
| 7 | Restore into a second volume and read it back | `script` (14m) | The restore into a second, existing volume comes back at its full 1 GiB with the marker at offset zero |
| 8 | Delete the backup and both volumes | `script` (12m) | The backup and both volumes leave the deployment |
| 9 | Detach the backup backend | `delete` (2m) + `error` + `assert` | The `CinderBackupBackend` is deleted, Deployment `cinder-backup-backup` is gone, and both backup conditions go True again under `NoBackupBackend` and `BackupNotConfigured` with `Ready=True/AllReady` |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`, `02-cinderbackupbackend-cr.yaml`

**Design note:** Step order is fixed. The backup and both volumes are deleted
through the API before the backup backend is detached, because deleting a backup
is work the `cinder-backup` service does and the detach is what takes that
service away.

---

### scale

**File:** `tests/e2e/cinder/scale/chainsaw-test.yaml`

**Purpose:** Patching `spec.api.deployment.replicas` propagates to the API
Deployment and its PodDisruptionBudget. Every step also asserts that the
scheduler and the volume service still run one available replica: both are
separate Deployments fed by separate spec fields, so a builder that read the API
replica count for every workload would scale them along.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` | `broker-vhost.sh create cinder-scale …`, then `00-cinder-cr.yaml` (`cinder-scale`, `replicas: 3`) and `01-cinderbackend-cr.yaml` (`scale-nfs1`) |
| 2 | Assert Ready and the initial replica count | `assert` (5m) | `Ready=True/AllReady`, the API Deployment at 3, PDB `minAvailable: 1`, scheduler and volume Deployment at one available replica each |
| 3 | Scale up to 5 and assert the rollout completed | `patch` + `assert` (5m) | `02-patch-scale-up.yaml` takes the API to 5, with `updatedReplicas` proving new pods started; `Ready=True/AllReady` holds and the other two Deployments are untouched |
| 4 | Scale down to 1 and assert the PDB flips | `patch` + `assert` (5m) | `03-patch-scale-to-one.yaml` takes the API to 1 and the PDB to `maxUnavailable: 1`; `Ready=True/AllReady` holds |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`, `02-patch-scale-up.yaml`, `03-patch-scale-to-one.yaml`

**Design note:** The suite sets `spec.concurrent: false`. Step 3 holds 700m of
CPU requests on the single node `hack/kind-config.yaml` declares: five API pods,
the scheduler and the volume service. Run alone, before the concurrent suites
start, that peak fits.

---

### healthcheck

**File:** `tests/e2e/cinder/healthcheck/chainsaw-test.yaml`

**Purpose:** The operator's HTTP probe of the Cinder API drives `CinderAPIReady`
and `status.endpoint`. The probe GETs `/healthcheck`, which the oslo healthcheck
middleware answers without a token and without touching the database or the
message bus.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` | `broker-vhost.sh create cinder-health …`, then `00-cinder-cr.yaml` (`cinder-health`) and `01-cinderbackend-cr.yaml` (`health-nfs1`) |
| 2 | Assert CinderAPIReady and the endpoint | `assert` (5m) | `CinderAPIReady=True/APIHealthy`, `Ready=True/AllReady`, and `status.endpoint` carrying the cluster-local Service URL |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`

**Design note:** Cinder writes the internal URL into `status.endpoint` whenever
`spec.gateway` is unset, and this CR sets none, so the probe target and the
advertised endpoint coincide.

---

### httproute

**File:** `tests/e2e/cinder/httproute/chainsaw-test.yaml`

**Purpose:** The full lifecycle of `spec.gateway` driven by the Cinder CR:
HTTPRoute created with the expected ParentRef, Hostname and BackendRef on port
8776, `HTTPRouteReady=False/HTTPRouteNotAccepted` until a Gateway controller
accepts the route, `HTTPRouteReady=True/HTTPRouteAccepted` after it, and the
route deleted when the spec block goes away.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Install the HTTPRoute CRD | `apply` | `00-httproute-crd.yaml`, the minimal CRD, so the operator can create HTTPRoute objects on a cluster with no Gateway controller |
| 2 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` | `broker-vhost.sh create cinder-route …`, then `01-cinder-cr.yaml` (`cinder-route`) and `02-cinderbackend-cr.yaml` (`route-nfs1`) |
| 3 | Assert the HTTPRoute is created with the expected spec | `assert` (5m) | HTTPRoute `cinder-route` with the ParentRef, Hostname and BackendRef the CR asks for |
| 4 | Assert HTTPRouteReady=False/HTTPRouteNotAccepted | `assert` (5m) | The condition before any controller reports `Accepted` |
| 5 | Simulate a Gateway controller accepting the route | `script` | `kubectl patch httproute cinder-route --subresource=status` writes an `Accepted=True` parent status under a fake controller name |
| 6 | Assert HTTPRouteReady=True/HTTPRouteAccepted | `assert` (5m) | The condition flips on the patched parent status |
| 7 | Unset spec.gateway | `patch` | `03-patch-remove-gateway.yaml` |
| 8 | Assert the HTTPRoute is deleted and NotRequired | `assert` (5m) + `script` | `HTTPRouteReady=True/HTTPRouteNotRequired`, and the HTTPRoute is gone |

**Fixtures:** `00-httproute-crd.yaml`, `01-cinder-cr.yaml`, `02-cinderbackend-cr.yaml`, `03-patch-remove-gateway.yaml`

**Scope:** The suite covers the operator's HTTPRoute contract alone. It patches
the `Accepted` condition itself; no Gateway controller runs, so nothing here
proves traffic reaches the Cinder API. That is what
[gateway-quick-start-smoke](#gateway-quick-start-smoke) does.

---

### network-policy

**File:** `tests/e2e/cinder/network-policy/chainsaw-test.yaml`

**Purpose:** The NetworkPolicy sub-reconciler renders ingress on the Cinder API
port 8776 and a deterministic auto-derived egress set, and follows
`spec.networkPolicy` through an update and a removal.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` | `broker-vhost.sh create cinder-netpol …`, then `00-cinder-cr.yaml` (`cinder-netpol`) and `01-cinderbackend-cr.yaml` (`netpol-nfs1`) |
| 2 | Assert the NetworkPolicy carries the expected rules | `assert` (5m) | `NetworkPolicyReady=True/NetworkPolicyReady` and NetworkPolicy `cinder-netpol` with ingress on 8776 and egress DNS (UDP and TCP 53), database (TCP 3306), cache (TCP 11211), messaging (TCP 5672) and export (TCP 2049), in that order |
| 3 | Add a second ingress source and assert it lands | `patch` + `assert` (5m) | `02-patch-update-ingress.yaml` adds a source and the rendered policy follows |
| 4 | Disable networkPolicy and assert deletion | `patch` + `assert` (5m) + `script` | `03-patch-disable-networkpolicy.yaml` leaves `NetworkPolicyReady=True/NetworkPolicyNotRequired` and no NetworkPolicy |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`, `02-patch-update-ingress.yaml`, `03-patch-disable-networkpolicy.yaml`

**Scope:** The default CI CNI (kindnet) does not enforce NetworkPolicy, so the
suite validates the rendered policy object and its lifecycle. Traffic enforcement
is out of its reach. The
export rule is what separates Cinder from the sibling suites: every attached
backend contributes TCP 2049, and a Cinder with no backend opens nothing. The
absent rule carries as much, since this CR sets no `spec.keystoneEndpoint` and
gets no Keystone rule.

---

### deletion-cleanup

**File:** `tests/e2e/cinder/deletion-cleanup/chainsaw-test.yaml`

**Purpose:** Covers the finalizer-driven cleanup on Cinder CR deletion, and what
the two satellite kinds do when the parent they name is gone.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` | `broker-vhost.sh create cinder-cleanup …`, then `00-cinder-cr.yaml` (`cinder-cleanup`), `01-cinderbackend-cr.yaml` (`cleanup-nfs1`) and `02-cinderbackupbackend-cr.yaml` (`cleanup-nfsbk`) |
| 2 | Wait for Ready=True and assert the cleanup finalizer | `assert` (5m) | `Ready=True/AllReady` with the cleanup finalizer on the CR, and both satellites `Ready=True/AllReady` under `ConfigProjected` |
| 3 | Assert every owned child exists before the deletion | `assert` (5m) | The four Deployments, the Service, the PDB, Secrets `cinder-cleanup-db-connection` and `cinder-cleanup-transport-url`, Job `cinder-cleanup-db-sync` and CronJob `cinder-cleanup-db-purge` |
| 4 | Delete the Cinder CR | `delete` (2m) | Deletes `cinder-cleanup` from `openstack` |
| 5 | Assert the owned children and the MariaDB CRs are gone | `error` (3m) | Thirteen absence assertions: the ten children of step 3 plus the MariaDB `Database`, `User` and `Grant` named `cinder-cleanup` |
| 6 | Assert the content-hashed objects are gone | `script` (3m) | No `cinder-cleanup` config ConfigMap and no projection Secret remains |
| 7 | Assert both satellites survive the parent | `assert` (5m) | Both are user-owned, so nothing reaps them: `CredentialsReady=False/WaitingForParent`, a demoted `ConfigProjected` under `WaitingForProjection`, and `Ready=False/NotAllReady` |
| 8 | Delete both satellites and assert the skipped unregistration | `delete` (2m) + `error` + `script` (3m) | Both satellites leave immediately, and a `ServiceRemoveSkipped` event sits on `cleanup-nfs1` |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`, `02-cinderbackupbackend-cr.yaml`

**Design notes:**

- The `error` timeout is raised to 3 minutes for step 5. The finalizer only
  issues the Delete of the MariaDB CRs; the mariadb-operator drops the schema and
  the user before it releases them, and those deletions serialize past the
  shared config's 30 seconds under parallel suites.
- Step 8 is the `ServiceRemoveSkipped` path. The service registry lives in the
  database that went with the parent, so no service-remove Job can ever run.
  `CinderBackend` releases its `cinder.openstack.c5c3.io/service-remove`
  finalizer unconditionally in that state, because holding it would wedge the
  object in `Terminating`.

---

### pod-security-restricted

**File:** `tests/e2e/cinder/pod-security-restricted/chainsaw-test.yaml`

**Purpose:** A Cinder reconciles to Ready inside a namespace labelled
`pod-security.kubernetes.io/enforce=restricted`, and every workload kind the
operator projects admits under that profile: the API, the scheduler, the volume
service, the backup service, the db-sync Job, the db-purge CronJob and the
service-remove Job.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply the labelled namespace, seed OpenBao, take a vhost | `apply` + `script` (5m) + `assert` | `00-namespace.yaml` creates `cinder-pss` with the restricted labels; the script copies the shared standalone values into the namespace-scoped OpenBao path and provisions the per-tenant store with `setup-eso-tenant.sh`; the `cinder-db` ExternalSecret is asserted synced, and the suite takes its vhost |
| 2 | Pre-create brownfield MariaDB Database/User/Grant | `apply` + `assert` (5m) | `00-brownfield-db-setup.yaml` creates `cinder-pss-restricted-db`, `-user` and `-grant` in `openstack` |
| 3 | Apply the CRs and assert Ready under the restricted profile | `apply` + `assert` (5m) + `error` + `script` | `01-cinderbackend-cr.yaml` (`pss-nfs1`), `02-cinderbackupbackend-cr.yaml` (`pss-nfsbk`) and `03-cinder-cr.yaml` (`cinder-pss`) reach `Ready=True/AllReady` with all four Deployments available; `error` assertions pin that no `Database`, `User` or `Grant` named `cinder-pss` was created, which is what brownfield mode means |
| 4 | Run the db-purge CronJob by hand under the profile | `script` (3m) + `assert` (5m) | `kubectl create job cinder-pss-purge-now --from=cronjob/cinder-pss-db-purge` copies the rendered jobTemplate, and the Job succeeds |
| 5 | Detach the volume backend and let the service-remove Job run | `delete` (6m) + `assert` (5m) | Deleting `pss-nfs1` runs Job `cinder-pss-pss-nfs1-service-remove` to success and leaves `VolumeServicesReady=False/NoBackends` |
| 6 | Assert zero PSS-violation FailedCreate events | `script` | Scans the `FailedCreate` events of `cinder-pss` for the literal `violates PodSecurity "restricted:latest"` and requires a zero count |

**Fixtures:** `00-namespace.yaml`, `00-brownfield-db-setup.yaml`, `01-cinderbackend-cr.yaml`, `02-cinderbackupbackend-cr.yaml`, `03-cinder-cr.yaml`

**Design notes:**

- The suite sets `spec.namespace: ""` to opt out of Chainsaw's per-test
  namespace, which carries no PSS labels. Chainsaw then creates no namespace and
  the test applies its own; Chainsaw's applied-resource cleanup deletes it again.
- The CR runs in brownfield mode (`database.host` plus `cache.servers`, no
  `clusterRef`). The shared provisioning flow resolves `ClusterRef` in the CR's
  own namespace, so a CR in `cinder-pss` cannot reach `openstack/openstack-db`
  that way.
- Steps 4 and 5 trigger the two Job kinds by hand. The purge fires daily at
  00:01 and a detach only happens when a user deletes a backend, so neither
  would fall inside a test run on its own.

---

### release-upgrade

**File:** `tests/e2e/cinder/release-upgrade/chainsaw-test.yaml`

**Purpose:** The one Cinder suite that enters the four-phase upgrade machine of
`operators/cinder/internal/controller/reconcile_database.go`. Every other suite
installs a release into a fresh, empty schema and so only ever runs the db-sync
command. This one upgrades 2025.2 to 2026.1 against a schema the 2025.2 db-sync
already populated.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` | `broker-vhost.sh create cinder-upgrade …`, then `00-cinder-cr.yaml` (`cinder-upgrade`, `:2025.2`) and `01-cinderbackend-cr.yaml` (`upgrade-nfs1`) |
| 2 | Assert the pre-upgrade steady state on :2025.2 | `assert` (10m) + `error` | `Ready=True/AllReady` with `installedRelease` 2025.2, and `error` assertions pinning that none of the three phase Jobs exists yet |
| 3 | Patch release + image tag to trigger the upgrade | `patch` | `03-patch-upgrade.yaml` sets `openStackRelease` and `image.tag` to 2026.1 |
| 4 | Follow status.upgradePhase to the settled end state | `script` (10m) + `assert` (10m) | The observed phases must be an ordered subsequence of `Expanding`, `Migrating`, `RollingUpdate`, `Contracting` that contains `RollingUpdate`; then `status.upgradePhase` clears, `installedRelease` is 2026.1, and `Ready=True/AllReady` |
| 5 | Assert the three phase Jobs ran on the new release | `assert` (10m) + `script` (2m) | Jobs `cinder-upgrade-db-expand`, `-db-migrate` and `-db-contract` each succeeded, and the migrate Job's pod carries the `cinder-status` verdict in its termination message |
| 6 | Assert all three Deployments rolled onto :2026.1 | `assert` (10m) + `script` (5m) | The API, scheduler and volume Deployments are on `:2026.1` with a converged rollout, and the steady-state db-sync Job re-ran on the new image |
| 7 | Assert the upgraded API answers and its registry is healthy | `script` (8m) | The probe pod reaches the API on the new release and every process in `/v3/os-services` reports state `up` |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-cr.yaml`, `03-patch-upgrade.yaml`

---

### maintenance-endpoint-isolation

**File:** `tests/e2e/cinder/maintenance-endpoint-isolation/chainsaw-test.yaml`

**Purpose:** Maintenance pods never become backends of the API Service. Cinder
carries more components under one instance label than any sibling operator, so
its API Service selector adds `app.kubernetes.io/component=api`, and this suite
checks that claim against the live EndpointSlices.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Give the suite its vhost, then apply the CRs | `script` (2m) + `apply` | `broker-vhost.sh create cinder-isolation …`, then `00-cinder-cr.yaml` (`cinder-isolation`), `01-cinderbackend-a-cr.yaml` (`isolation-nfs1`) and `02-cinderbackend-b-cr.yaml` (`isolation-nfs-b`) |
| 2 | Wait for Ready=True with both volume services up | `assert` (5m) | `Ready=True/AllReady` and the API Deployment available |
| 3 | Sample the API Service endpoints across both maintenance runs | `script` (180s) | Starts a db-purge Job from the CronJob and deletes `isolation-nfs-b` without waiting, then samples the non-terminating EndpointSlice addresses once a second for 90 seconds. The count never exceeds `spec.replicas` of the API Deployment, reaches it at least once, and neither the live db-purge pod IP nor the live service-remove pod IP ever appears among the addresses |

**Fixtures:** `00-cinder-cr.yaml`, `01-cinderbackend-a-cr.yaml`, `02-cinderbackend-b-cr.yaml`

**Design notes:**

- Terminating addresses are excluded from the count, so an ordinary API-pod
  replacement is not misreported as a maintenance-pod leak. The CR is never
  patched after step 1, because a spec change would roll the API Deployment and
  put a surge pod into the slices.
- Two flags turn the window from an expectation into an observation: the run
  fails unless a live db-purge pod and a live service-remove pod were each seen
  holding a pod IP. Without them the endpoint counts prove nothing.
- The replica bound is read off the live Deployment. A value copied from the
  fixture could silently stop matching the CR.

---

### gateway-quick-start-smoke

**File:** `tests/e2e/cinder/gateway-quick-start-smoke/chainsaw-test.yaml`

**Purpose:** The external URL the quick start tells a user to open works.
`curl -k https://cinder.127-0-0-1.nip.io/` answers HTTP 300 with the Cinder
version document, over the chain kind host `:443` to kind node `:31443` to the
Envoy Gateway proxy to the `https-cinder` listener on `openstack/openstack-gw`
to the operator-managed HTTPRoute to the Service to the API pod.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply namespace | `apply` | `00-namespace.yaml` alone. Neither CR is applied here: their CRDs are registered by the cinder-operator, which is suspended in some overlays, so the applies live behind the step-2 presence guard |
| 2 | Smoke-check Cinder API reachability | `script` (12m) | Exits 0 with a `SKIP:` line when `GatewayClass/envoy`, `Gateway/openstack-gw` or either Cinder CRD is absent. Otherwise it waits for `Gateway/openstack-gw` to be `Programmed`, applies `01-cinderbackend-cr.yaml` (`smoke-nfs1`) and `02-smoke-cinder-cr.yaml` (`cinder-smoke`), waits for `HTTPRouteReady=True` and `CinderAPIReady=True`, and curls the external URL until it answers 300 with a body carrying a `versions` key |

**Fixtures:** `00-namespace.yaml`, `01-cinderbackend-cr.yaml`, `02-smoke-cinder-cr.yaml`

**Design notes:**

- curl runs from the Chainsaw host, not from an in-cluster pod. The
  nip.io-to-127.0.0.1 mapping is an external DNS concern, and
  `hack/kind-config.yaml` already bridges host `127.0.0.1:443` to the proxy
  NodePort 31443.
- Only HTTP 300 is accepted. cinder's api-paste routes `/` through
  `root_app_factory` into the unauthenticated `apiversions` application, which
  serves the version document with 300 Multiple Choices, where the neutron and
  placement roots answer 200. A 404 would prove the route attached but the
  backend did not serve.
- The suite lives under `tests/e2e/cinder/` so the `e2e-operator` matrix job
  runs it. That job is the only place where the Cinder CRDs are registered.

---

### metrics

**File:** `tests/e2e/cinder-operator/metrics/chainsaw-test.yaml`

**Purpose:** The cinder-operator chart renders, and later removes, a
`monitoring.coreos.com/v1` ServiceMonitor pointing at the operator's `/metrics`
endpoint when `monitoring.serviceMonitor.enabled` is toggled.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Install cinder-operator chart with monitoring enabled | `script` (120s) | `helm install cinder-operator-metrics operators/cinder/helm/cinder-operator/ -n cinder-system` with `webhook.enabled=false`, `replicas=1`, `monitoring.serviceMonitor.enabled=true` and `interval=30s` |
| 2 | Assert the ServiceMonitor exists with the expected spec | `assert` (5m) | ServiceMonitor `cinder-operator-metrics` in `cinder-system` with the chart's name, instance and managed-by labels, the matching selector, and one endpoint on port `metrics`, path `/metrics`, interval `30s` |
| 3 | Uninstall the test release (cleanup) | `script` (120s) | `helm uninstall cinder-operator-metrics` |
| 4 | Assert the ServiceMonitor is removed | `script` | Polls until `kubectl get servicemonitor cinder-operator-metrics` fails |

**Fixtures:** none. The suite drives the Helm chart directly.

**Design notes:**

- The suite installs its own release rather than patching the cluster's
  operator. The `cinder-system/cinder-operator` HelmRelease is suspended in the
  kind CI overlay, and the operator that actually runs there is installed by
  `hack/ci-deploy-operator.sh`, so patching the suspended HelmRelease has no
  observable effect.
- `webhook.enabled=false` keeps the second release from registering a
  cluster-scoped webhook pair that would send Cinder, CinderBackend and
  CinderBackupBackend admission at a Service this release tears down again.
- Prometheus target-Up polling is out of scope. The E2E cluster installs
  prometheus-operator-crds but runs no Prometheus, so there is no
  `/api/v1/targets` endpoint to poll.

---

## Assertion Patterns

### Resource assertion (`assert`)

Declarative YAML matching against a Kubernetes resource, with JMESPath filter
syntax for conditions and for fields a plain subtree cannot reach. Used for
condition checks, replica counts, rendered container commands and resource
existence. Most suites raise the assert timeout to 5 minutes at the spec level.

```yaml
- try:
    - assert:
        resource:
          apiVersion: cinder.openstack.c5c3.io/v1alpha1
          kind: Cinder
          metadata:
            name: cinder-basic
            namespace: openstack
          status:
            volumeServices:
            - backend: basic-nfs1
              host: cinder-basic@basic-nfs1
            (conditions[?type == 'Ready']):
            - status: "True"
              reason: AllReady
```

`error` assertions carry the opposite claim and appear wherever something must
be absent: the backup Deployment in `basic-deployment`, every owned child in
`deletion-cleanup`, the MariaDB CRs in `pod-security-restricted`, and the three
phase Jobs before the upgrade in `release-upgrade`.

### Probe pod with a sentinel (`script`)

Anything the API has to answer is driven from a pod off the service image, so
the request crosses kube-proxy like any other client's. The script embeds a
Python program in a quoted heredoc, starts it detached, waits for a terminal
pod phase, reads the log back and greps for the sentinel the program prints
last. A missing sentinel fails the step even when the pod succeeded.

```yaml
- script:
    timeout: 6m
    shell: bash
    content: |
      kubectl run "$POD" -n "$NAMESPACE" --image=ghcr.io/c5c3/cinder:2025.2 \
        --restart=Never --command -- /var/lib/openstack/bin/python -c "$PROBE_SRC" \
        http://cinder-basic.openstack.svc.cluster.local:8776 \
        cinder-basic-scheduler cinder-basic@basic-nfs1
      # …poll .status.phase, then:
      echo "${OUT}" | grep -q 'BASIC-PROBE-OK'
```

The sentinels are per suite: `BASIC-PROBE-OK` in the two `basic-deployment`
suites, and one per stage in the data-path suites. The Python side retries only
connection-level failures, because kube-proxy's endpoint programming can trail
the CR's Ready flip by a second or two. Every HTTP status the API answers with
is a verdict and fails hard with its code.

### File inspection through the mount (`script` with `kubectl exec`)

A volume is a file on the export, so the data-path suites read it where the
driver wrote it. The `cinder-volume` pod is the one process holding the mount,
and the mount path is the md5 os-brick's remotefs driver derives from the share:

```bash
MNT=/var/lib/cinder/mnt/6f3cb55ed3b423dbb7791aaf3783754f
kubectl exec -n "$NAMESPACE" deploy/cinder-nfs-volume-nfsbe-nfs1 -- \
  stat -c '%u %g %a %s' "${MNT}/volume-${VID}"
```

`nfs-backend` reads owner, group, mode and apparent size after every stage,
`multi-backend` stats the file under the second mount and lists the first export
to show it is not there, and `backup-nfs` reads the chunk directory off the
backup target.

## File Layout

```text
tests/e2e/cinder/
├── broker-vhost.sh                     One RabbitMQ vhost per suite, plus its transport-URL Secret
├── backend-detach/
│   ├── chainsaw-test.yaml              Detach one CinderBackend from a running Cinder
│   ├── 00-cinder-cr.yaml               Cinder CR cinder-detach
│   ├── 01-cinderbackend-a-cr.yaml      The surviving backend detach-nfs-a
│   └── 02-cinderbackend-b-cr.yaml      The backend that is deleted, detach-nfs-b
├── backup-nfs/
│   ├── chainsaw-test.yaml              Backup round trip on an NFS target
│   ├── 00-cinder-cr.yaml               Cinder CR cinder-backup
│   ├── 01-cinderbackend-cr.yaml        Volume backend backup-nfs1
│   └── 02-cinderbackupbackend-cr.yaml  Backup backend backup-nfsbk
├── basic-deployment/
│   ├── chainsaw-test.yaml              Happy path on 2025.2
│   ├── 00-cinder-cr.yaml               Cinder CR cinder-basic
│   └── 01-cinderbackend-cr.yaml        Backend basic-nfs1
├── basic-deployment-2026-1/
│   ├── chainsaw-test.yaml              Happy path on 2026.1
│   ├── 00-cinder-cr.yaml               Cinder CR cinder-basic-2026-1
│   └── 01-cinderbackend-cr.yaml        Backend basic-2026-1-nfs1
├── deletion-cleanup/
│   ├── chainsaw-test.yaml              Finalizer cleanup and the orphaned satellites
│   ├── 00-cinder-cr.yaml               Cinder CR cinder-cleanup
│   ├── 01-cinderbackend-cr.yaml        Volume backend cleanup-nfs1
│   └── 02-cinderbackupbackend-cr.yaml  Backup backend cleanup-nfsbk
├── gateway-quick-start-smoke/
│   ├── chainsaw-test.yaml              External URL smoke check, SKIP-gated
│   ├── 00-namespace.yaml               The namespace the smoke CRs land in
│   ├── 01-cinderbackend-cr.yaml        Backend smoke-nfs1
│   └── 02-smoke-cinder-cr.yaml         Cinder CR cinder-smoke with spec.gateway
├── healthcheck/
│   ├── chainsaw-test.yaml              CinderAPIReady and status.endpoint
│   ├── 00-cinder-cr.yaml               Cinder CR cinder-health
│   └── 01-cinderbackend-cr.yaml        Backend health-nfs1
├── httproute/
│   ├── chainsaw-test.yaml              spec.gateway lifecycle
│   ├── 00-httproute-crd.yaml           Minimal HTTPRoute CRD for clusters with no Gateway controller
│   ├── 01-cinder-cr.yaml               Cinder CR cinder-route with spec.gateway
│   ├── 02-cinderbackend-cr.yaml        Backend route-nfs1
│   └── 03-patch-remove-gateway.yaml    Patch unsetting spec.gateway
├── invalid-cinderbackend-cr/
│   ├── chainsaw-test.yaml              CinderBackend rejection corpus
│   ├── _generate.py                    Generator for the fixtures below
│   └── 00-…-09-….yaml                  Ten rejection fixtures
├── invalid-cinderbackupbackend-cr/
│   ├── chainsaw-test.yaml              CinderBackupBackend rejection corpus
│   ├── _generate.py                    Generator for the fixtures below
│   └── 00-…-08-….yaml                  Nine rejection fixtures
├── invalid-cr/
│   ├── chainsaw-test.yaml              Cinder rejection corpus
│   ├── _generate.py                    Generator for the fixtures below
│   └── 00-…-18-….yaml                  Nineteen rejection fixtures
├── maintenance-endpoint-isolation/
│   ├── chainsaw-test.yaml              Maintenance pods stay out of the API EndpointSlices
│   ├── 00-cinder-cr.yaml               Cinder CR cinder-isolation
│   ├── 01-cinderbackend-a-cr.yaml      Backend isolation-nfs1
│   └── 02-cinderbackend-b-cr.yaml      Backend isolation-nfs-b, detached inside the window
├── multi-backend/
│   ├── chainsaw-test.yaml              Two backends on two exports
│   ├── 00-cinder-cr.yaml               Cinder CR cinder-multi
│   ├── 01-cinderbackend-a-cr.yaml      Backend multi-nfs-a on /volumes
│   └── 02-cinderbackend-b-cr.yaml      Backend multi-nfs-b on /volumes-b
├── network-policy/
│   ├── chainsaw-test.yaml              NetworkPolicy create, update and delete
│   ├── 00-cinder-cr.yaml               Cinder CR cinder-netpol
│   ├── 01-cinderbackend-cr.yaml        Backend netpol-nfs1
│   ├── 02-patch-update-ingress.yaml    Patch adding a second ingress source
│   └── 03-patch-disable-networkpolicy.yaml Patch removing spec.networkPolicy
├── nfs-backend/
│   ├── chainsaw-test.yaml              Volume data path: create, extend, clone, delete
│   ├── 00-cinder-cr.yaml               Cinder CR cinder-nfs
│   └── 01-cinderbackend-cr.yaml        Backend nfsbe-nfs1
├── pod-security-restricted/
│   ├── chainsaw-test.yaml              Reconciliation under the restricted PSS profile
│   ├── 00-namespace.yaml               Labelled namespace cinder-pss and its cinder-db ExternalSecret
│   ├── 00-brownfield-db-setup.yaml     Pre-created MariaDB Database/User/Grant
│   ├── 01-cinderbackend-cr.yaml        Backend pss-nfs1
│   ├── 02-cinderbackupbackend-cr.yaml  Backup backend pss-nfsbk
│   └── 03-cinder-cr.yaml               Cinder CR cinder-pss in brownfield mode
├── release-upgrade/
│   ├── chainsaw-test.yaml              Cross-release upgrade 2025.2 to 2026.1
│   ├── 00-cinder-cr.yaml               Cinder CR cinder-upgrade on 2025.2
│   ├── 01-cinderbackend-cr.yaml        Backend upgrade-nfs1
│   └── 03-patch-upgrade.yaml           Patch to release and image tag 2026.1
└── scale/
    ├── chainsaw-test.yaml              API replica scaling and PDB policy
    ├── 00-cinder-cr.yaml               Cinder CR cinder-scale with replicas 3
    ├── 01-cinderbackend-cr.yaml        Backend scale-nfs1
    ├── 02-patch-scale-up.yaml          Patch to 5 replicas
    └── 03-patch-scale-to-one.yaml      Patch to 1 replica

tests/e2e/cinder-operator/
└── metrics/
    └── chainsaw-test.yaml              cinder-operator chart ServiceMonitor
```

## Related Resources

- [Cinder Operator](../cinder/index.md) — the four processes and the design decisions behind them
- [Cinder CRD](../cinder/cinder-crd.md) — CRD types, webhooks and the rendered configuration
- [CinderBackend CRD API Reference](../cinder/cinder-backend-crd.md) — volume backends and their rejection corpus
- [CinderBackupBackend CRD API Reference](../cinder/cinder-backup-backend-crd.md) — backup targets and their rejection corpus
- [Cinder Reconciler Architecture](../cinder/cinder-reconciler.md) — sub-reconciler contracts and unit tests
- [Chaos E2E Test Suites](./chaos-e2e-tests.md) — the three Cinder outage suites on the chaos `network` leg
- [Tempest Test Infrastructure](./tempest-test-infrastructure.md) — the upstream API tests the cinder legs run
- [ControlPlane E2E Test Suites](./controlplane-e2e-tests.md) — Cinder as a placed service under a ControlPlane
- [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md) — infrastructure stack deployment, `WITH_NFS` and `WITH_MESSAGING`
- `tests/e2e/chainsaw-config.yaml` — shared Chainsaw configuration
- `.github/workflows/ci.yaml` — the `e2e-operator` matrix leg that runs these suites
