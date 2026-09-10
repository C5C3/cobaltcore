---
title: Cinder CRD
quadrant: operator
---

# Cinder CRD

`cinders.cinder.openstack.c5c3.io/v1alpha1`, kind `Cinder`. The CRD is generated
from `operators/cinder/api/v1alpha1/cinder_types.go`; the validating and
defaulting webhooks live in `operators/cinder/api/v1alpha1/cinder_webhook.go`.
The Helm chart ships a synced copy (`make sync-crds` / `make verify-crd-sync`).

One `Cinder` CR describes the block-storage service: its OpenStack release,
container image, the database, cache and message-bus connections, the Keystone
integration, and the pod-level knobs of its four Deployments. Storage is not
part of this spec. Volume backends attach through
[`CinderBackend`](./cinder-backend-crd.md) CRs and the backup driver through a
[`CinderBackupBackend`](./cinder-backup-backend-crd.md).

`kubectl get cinders` shows Ready
(`.status.conditions[?(@.type=='Ready')].status`), Release
(`.status.installedRelease`), Endpoint (`.status.endpoint`) and Age.

## Spec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `openStackRelease` | `string` | yes | The OpenStack release the operator deploys and drives; pattern `^\d{4}\.[12]$` (the `YYYY.N` cadence, `N` in {1,2}). It governs install and upgrade tracking: `status.installedRelease` is promoted to it after a successful migration. Kept separate from the image tag so a digest-pinned image still resolves a schema |
| `image` | `ImageSpec` | yes | The container image every process runs: API, scheduler, volume, backup, and the migration, purge and service-remove Jobs. Exactly one of `tag` or `digest` (shared CEL rule, re-checked by the webhook) |
| `database` | `DatabaseSpec` | yes | MariaDB connection. Exactly one of `clusterRef` (managed) or `host` (brownfield); `credentialsMode` (`Static` \| `Dynamic`, where `Dynamic` requires `clusterRef`), `secretRef`, and optional `tls`. The rules are inherited from `commonv1.DatabaseSpec` |
| `cache` | `CacheSpec` | yes | Memcached. Exactly one of `clusterRef` (managed) or `servers` (brownfield). It backs both `[keystone_authtoken] memcached_servers` and the `[coordination]` lock backend |
| `messaging` | `MessagingSpec` | yes | The RabbitMQ connection. Required rather than optional: every volume request travels the bus, so a Cinder without a broker accepts requests nothing acts on. Exactly one of `clusterRef` (managed) or `secretRef` (brownfield), plus optional `tls` |
| `api` | [`CinderAPISpec`](#cinderapispec) | no | The API Deployment's pod-level block and its uWSGI parameters |
| `scheduler` | [`CinderSchedulerSpec`](#cinderschedulerspec) | no | The scheduler Deployment's pod-level block |
| `volume` | [`CinderVolumeSpec`](#cindervolumespec) | no | The pod-level block every `cinder-volume` Deployment runs with |
| `backup` | [`CinderBackupSpec`](#cinderbackupspec) | no | The backup Deployment's pod-level block |
| `keystoneEndpoint` | `string` | no | The Keystone auth URL rendered as `[keystone_authtoken] auth_url`; pattern `^https?://`, and the webhook also requires a parseable URL with a host. Cinder validates a token on every request server-side, so it must be reachable from inside the cluster. Required together with `serviceUser` (CEL rule); omitting both deploys the service without the Keystone integration, which renders `auth_strategy = noauth` |
| `keystonePublicEndpoint` | `string` | no | The browser-facing Keystone base URL rendered as `www_authenticate_uri`, the address a 401 points unauthenticated clients at. When empty the operator falls back to `keystoneEndpoint` at render time (`EffectiveKeystonePublicEndpoint`), correct only when the internal and public URLs coincide |
| `serviceUser` | [`ServiceUserSpec`](#serviceuserspec) | no | The Keystone service account and the Secret holding its password. Required exactly when `keystoneEndpoint` is set |
| `region` | `string` | no | The Keystone region (`region_name` in both identity sections); omitted when empty, and Cinder then uses the catalog's default region |
| `glanceEndpoint` | `string` | no | The image service `create volume from image` reads through (`[DEFAULT] glance_api_servers`); pattern `^https?://`. Omitted when empty, and Cinder resolves Glance from the Keystone catalog, which a Keystone-free deployment cannot do |
| `keyManager` | [`KeyManagerSpec`](#keymanagerspec) | no | The castellan key manager volume-encryption keys live in. Setting it requires `keystoneEndpoint` (CEL rule), because castellan authenticates with the same credentials |
| `internalTenant` | [`InternalTenantSpec`](#internaltenantspec) | no | The Keystone project and user Cinder owns its internal volumes as. The image-volume cache needs it: a cached volume belongs to the deployment rather than to the tenant whose request populated it |
| `dbPurge` | [`DBPurgeSpec`](#dbpurgespec) | no | The recurring purge of the rows Cinder only soft-deletes. A nil block resolves exactly like an empty one: 30 days of retention, daily at `1 0 * * *`, not suspended |
| `gateway` | `GatewaySpec` | no | External exposure through a Gateway API HTTPRoute on port 8776; requires `hostname` and `parentRef.name`. Setting it requires `keystoneEndpoint` (CEL rule): without it the API renders `auth_strategy = noauth`, so a Gateway would publish every volume operation unauthenticated |
| `networkPolicy` | `NetworkPolicySpec` | no | Ingress restricted to TCP 8776 from the listed sources; egress auto-derived (DNS, database, cache, Keystone, Glance, Barbican, the broker port and the NFS exports). At least one ingress source is required (fail-closed) |
| `autoscaling` | `AutoscalingSpec` | no | HPA bounds and CPU/memory utilization targets. It reaches the API Deployment alone |
| `logging` | `LoggingSpec` | no | oslo.log derivation: `format` (`text`/`json`), `level`, `debug`, `perLoggerLevels`. Materialized by the defaulting webhook to `text`/`INFO`/`debug: false` |
| `policyOverrides` | `PolicySpec` | no | Custom oslo.policy rules. A CEL rule requires at least one of `rules` or `configMapRef`; when set, the operator renders `policy.yaml` and wires `[oslo_policy] policy_file` |
| `extraConfig` | `map[string]map[string]string` | no | Free-form INI sections for options with no dedicated field. See [extraConfig](#extraconfig) |
| `secretStoreRef` | `SecretStoreRefSpec` | no | Selects the External Secrets store `SecretsReady` is resolved against: `kind` (`ClusterSecretStore` \| `SecretStore`, default `ClusterSecretStore`) and a required `name`. When omitted the shared cluster-scoped `openbao-cluster-store` is used |
| `targetClusterRef` | `TargetClusterRefSpec` | no | Names the registered target cluster that receives this Cinder's children: the four Deployments, the ConfigMaps, the Secrets, the Jobs and the database CRs. The CR itself does not move, and neither do its status, its finalizers or the webhooks that admit it. An attached satellite carries no ref of its own and follows this one. Immutable (two CEL transition rules, mirrored by the webhook): adding, removing or renaming it strands the children on the previously selected cluster; delete and recreate instead. See [Target Clusters](../target-clusters.md) |

### CinderAPISpec

The API is the only Deployment that scales horizontally.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `deployment` | `DeploymentSpec` | no | `replicas: 3` | Pod-level knobs: `replicas`, `resources` (256Mi/512Mi memory, 100m/500m CPU), `terminationGracePeriodSeconds` (30), `preStopSleepSeconds` (5), `strategy`, `topologySpreadConstraints`, `priorityClassName` |
| `uwsgi` | `UWSGISpec` | no | materialized | uWSGI parameters: `processes` (2), `threads` (1), `httpKeepAlive` (true), `harakiri` and `httpKeepAliveTimeout` (both omitted when unset). The defaulting webhook materializes the block, so the API always runs with the documented values |

### CinderSchedulerSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `deployment` | `DeploymentSpec` | no | `replicas: 1` | The same pod-level knobs. Schedulers are peers that hold no state between requests, so the count may be raised; every pod registers under the one `{name}-scheduler` identity |

The one-replica default is applied by the defaulting webhook and reaches the
absent block only. `commonv1.DeploymentSpec.Replicas` carries
`+kubebuilder:default=3`, which the API server materializes as soon as the CR
carries a `deployment` object at all, before any mutating webhook runs.

### CinderVolumeSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `deployment` | `DeploymentSpec` | no | `replicas: 1`, `strategy.type: Recreate` | The pod-level knobs every `cinder-volume` Deployment shares. The operator projects one Deployment per attached backend and applies this block to all of them, which is why `topologySpreadConstraints` is rejected here (see [Defaulting and validation](#defaulting-and-validation)) |

Two CEL rules on `CinderSpec` pin this block: `replicas` must be `1` and
`strategy.type` must be `Recreate`. A `cinder-volume` owns its backend through a
host identity rather than through a lock, so a second process under that
identity, or the surge pod of a rolling update overlapping the outgoing one, has
the same volume state open twice, which the NFS drivers refuse. Because the
schema default of 3 lands on any present block before the webhook runs, a CR
that spells out `spec.volume.deployment` at all has to spell out `replicas: 1`
beside whatever else it sets, or admission rejects it.

### CinderBackupSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `deployment` | `DeploymentSpec` | no | `replicas: 1`, `strategy.type: Recreate`, memory limit `2Gi` | The pod-level knobs of the backup Deployment |

The same two CEL rules apply, with the same consequence for a present block. The
memory limit is raised from the shared 512Mi because a backup reads the volume
in chunks of `spec.fileSize` bytes and compresses each chunk in memory: under
the shared limit the process is killed mid-backup and the restarted service
begins the volume again. An explicit `resources` block is left alone.

### ServiceUserSpec

The identity fields are webhook-defaulted, so a minimal block need only supply
the password Secret reference.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `username` | `string` | no | `cinder` | Keystone username (`username` in both identity sections) |
| `projectName` | `string` | no | `service` | The project the service user scopes to |
| `userDomainName` | `string` | no | `Default` | The domain the service user lives in |
| `projectDomainName` | `string` | no | `Default` | The domain the service project lives in |
| `secretRef` | `SecretRefSpec` | yes | `key` to `password` | The Secret holding the service-user password. The value is injected as `OS_KEYSTONE_AUTHTOKEN__PASSWORD` and `OS_SERVICE_USER__PASSWORD`, never rendered into config |

### KeyManagerSpec

A union rule enforces exactly one key-manager block matching `type`.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `type` | `KeyManagerType` | yes | `Barbican` when the block is present and the field empty | The castellan key manager; `Barbican` is the only value (enum) |
| `barbican.endpoint` | `string` | yes with `type: Barbican` | | The Barbican API endpoint (`[barbican] barbican_endpoint`); `MinLength=1`, pattern `^https?://`, and the webhook requires a parseable URL with a host |

### InternalTenantSpec

Both fields are IDs, not names. Cinder passes them to the volume API without
resolving them through Keystone, so a name reaches the backend as a project that
does not exist.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `projectID` | `string` | yes | The Keystone project ID internal volumes are created in (`MinLength=1`) |
| `userID` | `string` | yes | The Keystone user ID internal volumes are created as (`MinLength=1`) |

### DBPurgeSpec

Cinder never hard-deletes on its own: deleting a volume, a snapshot or a backup
flips its row to deleted, so the tables grow for the lifetime of the deployment.
The operator projects a CronJob running `cinder-manage db purge <days>` on every
Cinder, whether or not the block is set.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `retentionDays` | `*int32` (Minimum=1) | no | `30` | How long a soft-deleted row survives. Lowering it applies retroactively at the next firing, so the validating webhook warns on a reduction |
| `schedule` | `string` | no | `1 0 * * *` | The cron expression the CronJob runs on. Checked by the webhook against the cron grammar, the one rule with no CRD-schema counterpart: a regex that accepts a descriptor like `@daily` alongside 5-field expressions would also accept invalid ones |
| `suspend` | `bool` | no | `false` | Pauses the CronJob without deleting it. `DBPurgeReady` stays True under the reason `DBPurgeSuspended` |

The settings are resolved at reconcile time rather than written back into the
CR, so a field left unset keeps tracking the operator default across upgrades.

### extraConfig

`spec.extraConfig` is the escape hatch for options with no dedicated field. The
render-time merge is `operator defaults < extraConfig`, so a user value wins.
Overrides of operator-owned keys are honored and reported through the
`ExtraConfigHealthy` condition and an `ExtraConfigOwnedKeyOverride` Warning
event, with one group of exceptions: the keys below are rejected at admission,
because honoring them does the damage before any condition could report it.

| Section | Rejected keys | Why |
| --- | --- | --- |
| `[DEFAULT]` | `auth_strategy`, `api_paste_config` | Both select the WSGI pipeline, and `keystone` is the only pipeline that runs keystonemiddleware: an override can serve the whole API unauthenticated |
| `[DEFAULT]` | `transport_url` | Env-injected via `OS_DEFAULT__TRANSPORT_URL`, so a file value is inert at runtime and only copies the broker credentials into the rendered config |
| `[database]` | `connection` | Env-injected via `OS_DATABASE__CONNECTION`, same reasoning |
| `[keystone_authtoken]` | `password` | Env-injected via `OS_KEYSTONE_AUTHTOKEN__PASSWORD`, same reasoning |
| `[service_user]` | `password` | Env-injected via `OS_SERVICE_USER__PASSWORD`, same reasoning |
| `[cinder_sys_admin]`, `[privsep_osbrick]` | `helper_command`, `capabilities` | The helper command is what the service runs as root, and the capability set bounds what that helper may do |

Option names are validated at admission against a per-release option catalog
embedded in the operator (the release comes from `spec.openStackRelease`; a
release with no embedded catalog skips the check with an admission warning). An
unknown section or option is rejected with the section and key named, so an
arbitrary backend-named section is refused: backend options belong to the
[`CinderBackend`](./cinder-backend-crd.md) CR. A deprecated-but-accepted option
is admitted with a warning naming its replacement. Values are checked for one
thing, a newline or carriage return in a section name, key or value, because the
rendered INI writes each verbatim and a newline injects further config lines past
the ownership and catalog gates. The two privsep sections appear in no catalog,
since oslo.privsep registers a context's section at run time.

## Defaulting and validation

The defaulting webhook resolves the three non-API replica counts to one before
the shared `DeploymentSpec` defaults run, fills the backup container's resources
with the raised memory limit, materializes `spec.api.uwsgi` and `spec.logging`,
sets the `Recreate` strategy on the volume and backup blocks when they carry
none, fills the cache backend, and materializes the `ServiceUserSpec` identity
defaults. It leaves `spec.dbPurge` untouched, because those fields are resolved
at reconcile time.

The validating webhook accumulates every violation into one admission response.
It repeats the schema-layer rules as defense in depth (image tag/digest XOR,
the database, cache and messaging mutual-exclusivity rules, the
Dynamic-requires-clusterRef rule, the secret-store-ref shape, the replica floor,
the autoscaling bounds including the implicit `minReplicas` default from
`spec.api.deployment.replicas`, the network-policy ingress source, the gateway
hostname and parentRef, the four single-writer rules, the key-manager union and
the `internalTenant` IDs) and adds the rules CEL cannot express: the URL shape
of every endpoint field, the cron grammar of `spec.dbPurge.schedule`, the
logging enums including the per-logger-level map, the graceful-termination
arithmetic (`preStopSleepSeconds < terminationGracePeriodSeconds`, and
`harakiri` strictly inside the drain window), the request-versus-limit ordering,
the PriorityClass lookup, the topology-spread selector, and the `extraConfig`
guards.

Each block's topology-spread selector is measured against the pod selector of the
Deployment that block configures: `app.kubernetes.io/component` narrows the API,
scheduler and backup selectors, so a constraint on those blocks has to carry the
component too. `spec.volume.deployment` takes no constraint at all — one
Deployment is projected per attached backend, each pinned to a single replica and
selected by its own `volume-<backend>` component, so every selector matching all
of them matches the API, scheduler and backup pods too, and a constraint here
would spread each volume pod against pods it does not control. An empty list is
still accepted there, because it selects nothing and only switches the operator's
injected zone and hostname defaults off.

Two rules produce warnings rather than rejections: a reduced
`spec.dbPurge.retentionDays`, because the rows the next run removes do not come
back, and an `imageVolumeCache` enabled on a backend whose Cinder sets no
`spec.internalTenant`.

`metadata.name` is bounded at 43 characters on create. The `{name}-db-purge`
CronJob is the child with the tightest budget: Kubernetes caps a CronJob name at
52 characters, because its controller appends an 11-character timestamp suffix
to every Job it spawns. The bound applies on create only, since `metadata.name`
is immutable and an update rule could only reject a CR an earlier operator
version already admitted, including the finalizer-removal update that completes
its deletion. Such a grandfathered CR still reconciles: the operator collapses
the overflowing tail onto a content-stable hash and names the CronJob
`{truncated}-{hash}-db-purge`.

## Rendered configuration

`cinder.conf` is byte-identical across the supported releases, so a release bump
never rotates the ConfigMap for a configuration change that is not there. This
is the full document for a Cinder with the Keystone integration, an untouched
`spec.logging`, and no key manager:

```ini
[DEFAULT]
api_paste_config = /var/lib/openstack/etc/cinder/api-paste.ini
auth_strategy = keystone
debug = false
host = cinder
image_conversion_dir = /var/lib/cinder/conversion
resource_query_filters_file = /var/lib/openstack/etc/cinder/resource_filters.json
state_path = /var/lib/cinder
use_stderr = true

[cinder_sys_admin]
capabilities =
helper_command = privsep-helper --config-dir /etc/cinder/cinder.conf.d

[coordination]
backend_url = file:///var/lib/cinder/coordination

[database]
connection = mysql+pymysql://placeholder

[keystone_authtoken]
auth_type = password
auth_url = http://keystone.openstack.svc:5000
memcached_servers = mc:11211
project_domain_name = Default
project_name = service
region_name = RegionOne
user_domain_name = Default
username = cinder
www_authenticate_uri = http://keystone.openstack.svc:5000

[oslo_concurrency]
lock_path = /var/lib/cinder/tmp

[oslo_messaging_notifications]
driver = noop

[oslo_messaging_rabbit]
rabbit_quorum_queue = true
rabbit_transient_quorum_queue = true
use_queue_manager = true

[privsep_osbrick]
capabilities =
helper_command = privsep-helper --config-dir /etc/cinder/cinder.conf.d

[service_user]
auth_type = password
auth_url = http://keystone.openstack.svc:5000
project_domain_name = Default
project_name = service
region_name = RegionOne
send_service_user_token = true
user_domain_name = Default
username = cinder
```

The two `capabilities` keys are rendered with an empty value, which oslo reads
as the empty set. Rendering them rather than omitting them is what makes them
the operator's to own, and a golden test pins both lines byte for byte. A Cinder
without `spec.keystoneEndpoint` renders the same document minus
`[keystone_authtoken]` and `[service_user]`, with `auth_strategy = noauth`. That
pipeline takes the project from the request URL and validates no token, which is
why a CEL rule refuses to pair it with `spec.gateway`: it is a shape for
in-cluster suites, not one to publish. The privsep sections stay: the volume and
backup services need their helper whether or not an identity service exists.

Neither password is in the file. `[keystone_authtoken] password` and
`[service_user] password` are never emitted, and `[database] connection` carries
the placeholder `mysql+pymysql://placeholder` so oslo.config parses the file
before the environment override applies. `transport_url` is absent for the same
reason.

The conditional sections and keys:

| Rendered | When |
| --- | --- |
| `[DEFAULT] glance_api_servers` | `spec.glanceEndpoint` is set |
| `[DEFAULT] cinder_internal_tenant_project_id` / `cinder_internal_tenant_user_id` | `spec.internalTenant` is set |
| `[DEFAULT] default_log_levels` | `spec.logging.perLoggerLevels` is non-empty |
| `[DEFAULT] log_config_append` | `spec.logging.format` is `json`; the file `logging.ini` is rendered beside `cinder.conf` |
| `[keystone_authtoken] region_name`, `[service_user] region_name` | `spec.region` is set |
| `[key_manager] backend`, `[barbican] barbican_endpoint` / `barbican_endpoint_type` / `auth_endpoint` | `spec.keyManager` is set |
| `[oslo_messaging_rabbit] ssl` / `ssl_ca_file` | `spec.messaging.tls` is set |
| `[oslo_policy] policy_file` | `spec.policyOverrides` renders a non-empty `policy.yaml` |

`[DEFAULT] enabled_backends` is deliberately absent from `cinder.conf`. Each
`cinder-volume` gets a per-backend overlay naming the single backend it serves,
so the key never belongs in the document the API and the scheduler read.

`scheduler.conf` is the whole scheduler overlay:

```ini
[DEFAULT]
host = cinder-scheduler
```

Every scheduler pod renders the same identity however many replicas run.
Schedulers are peers that hold no state, so a per-pod identity would fill the
service registry with entries no volume is keyed by.

## Config directories and mounts

oslo.config reads every file in a `--config-dir`, so each process is given the
shared directory plus the overlays it alone may see.

| Path | Content | Read by |
| --- | --- | --- |
| `/etc/cinder/cinder.conf.d` | `cinder.conf`, plus `logging.ini` and `policy.yaml` when rendered | All four processes and every Job |
| `/etc/cinder/scheduler.conf.d` | `scheduler.conf` | The scheduler pods |
| `/etc/cinder/backends.conf.d` | `backend.conf` and `{backend}.shares` | The backend's `cinder-volume` |
| `/etc/cinder/volume.conf.d` | `volume.conf`, the `enabled_backends` overlay | The backend's `cinder-volume` |
| `/etc/cinder/backup.conf.d` | `backup.conf` | The backup pod |
| `/var/lib/cinder` | `[DEFAULT] state_path`, an `emptyDir`; `conversion` and `tmp` live below it | All four processes |
| `/var/lib/cinder/mnt/<md5>` | One volume backend's NFS export | That backend's `cinder-volume`, and the backup pod |
| `/var/lib/cinder/backup_mount/<md5>` | The backup target's NFS export | The backup pod |
| `/tmp` | Writable scratch beside the read-only root filesystem | All four processes and every Job |
| `/etc/cinder-db-tls/` | `ca.crt`, `tls.crt`, `tls.key` | Only while `spec.database.tls` is enabled |
| `/etc/rabbitmq-ca` | `ca.crt`, the file `ssl_ca_file` names | Only while `spec.messaging.tls` is set |

The migration, purge and service-remove Jobs mount the whole config ConfigMap at
`/etc/cinder/cinder.conf.d`. The one extra file they see is `scheduler.conf`,
whose host identity `cinder-manage` never consults. The four workloads mount
every key except that one, so no pod registers under the scheduler's identity.

The launch commands:

| Process | Command |
| --- | --- |
| API | `uwsgi --http :8776 … --module cinder.wsgi.api:application … --pyargv "--config-dir /etc/cinder/cinder.conf.d"` |
| Scheduler | `cinder-scheduler --config-dir /etc/cinder/cinder.conf.d --config-dir /etc/cinder/scheduler.conf.d` |
| Volume | `cinder-volume --config-dir /etc/cinder/cinder.conf.d --config-dir /etc/cinder/backends.conf.d --config-dir /etc/cinder/volume.conf.d` |
| Backup | `cinder-backup --config-dir /etc/cinder/cinder.conf.d --config-dir /etc/cinder/backup.conf.d` |

The API is launched with `--module` because the image ships no WSGI entry
script, and cinder's entry point reads `CONF(sys.argv[1:])`, which is why the
config directory travels in `--pyargv`.

## Environment overrides

Every credential reaches the processes as an oslo.config `OS_<GROUP>__<OPTION>`
environment override sourced from a Secret, so none of them is written into the
ConfigMap the pods mount.

| Variable | Source | Overrides |
| --- | --- | --- |
| `OS_DATABASE__CONNECTION` | `{name}-db-connection` | `[database] connection` |
| `OS_DEFAULT__TRANSPORT_URL` | `{name}-transport-url` | `[DEFAULT] transport_url` |
| `OS_KEYSTONE_AUTHTOKEN__PASSWORD` | `spec.serviceUser.secretRef` | `[keystone_authtoken] password` |
| `OS_SERVICE_USER__PASSWORD` | `spec.serviceUser.secretRef` | `[service_user] password` |

The migration Jobs carry the same four. They read neither the bus nor Keystone,
but an override is inert without the section that consumes it, and one
environment for every process is what keeps a Job from migrating a different
database than the API serves. The three bus processes also carry
`CINDER_AMQP_PORT`, the broker port their readiness probe checks.

Each value is digested into a pod-template annotation, so a rotation at the
OpenBao source rolls the pods that consume it:
`cinder.c5c3.io/db-connection-hash`, `cinder.c5c3.io/authtoken-hash` and
`cinder.c5c3.io/transport-url-hash`. The scheduler, volume and backup pods carry
one more, `cinder.c5c3.io/installed-release`.

## Status

| Field | Description |
| --- | --- |
| `conditions` | List-map keyed by `type`; see the [reconciler reference](./cinder-reconciler.md#conditions) for the vocabulary |
| `observedGeneration` | The `.metadata.generation` last reconciled |
| `endpoint` | The Cinder API URL: `https://{gateway.hostname}/` when a gateway is set, otherwise the cluster-local Service URL |
| `installedRelease` | The OpenStack release whose schema is currently installed, promoted to `spec.openStackRelease` after the upgrade completes (or after the first `db sync` on a fresh install) |
| `targetRelease` | The release being upgraded to during an active transition. Set when the upgrade initiates, cleared on completion or abort |
| `upgradePhase` | The current phase during an active release upgrade (`Expanding`, `Migrating`, `RollingUpdate`, `Contracting`); empty when no upgrade is in flight |
| `volumeServices` | One entry per attached backend: `backend` and the `host` identity its `cinder-volume` registers under. It reports what the operator configured, not what Cinder reports back, so an entry appears as soon as the Deployment is projected |

## Sub-Resource Naming Convention

Operator-managed sub-resources of the API (Deployment, Service,
PodDisruptionBudget, HorizontalPodAutoscaler, NetworkPolicy, HTTPRoute) use the
bare CR name with no suffix, matching the keystone convention. A Cinder named
`cinder` in the `openstack` namespace is therefore reachable in-cluster at
`cinder.openstack.svc.cluster.local:8776`, the port every OpenStack client and
catalog entry assumes.

The other children carry a suffix, and the ones a backend contributes carry its
name:

| Resource | Name | Notes |
| --- | --- | --- |
| Config ConfigMap | `{name}-config-<hash>` | Immutable, content-hashed `cinder.conf` and `scheduler.conf`; 3 historical retained |
| Backend Secret | `{name}-backend-{backend}-<hash>` | Immutable, content-hashed `backend.conf`, `shares`, `volume.conf`; 3 historical retained |
| Backup Secret | `{name}-backup-{backupBackend}-<hash>` | Immutable, content-hashed `backup.conf`; 3 historical retained |
| DB-connection Secret | `{name}-db-connection` | Derived pymysql DSN, stable name |
| Transport-URL Secret | `{name}-transport-url` | Derived `rabbit://` URL, stable name |
| DB-sync Job | `{name}-db-sync` | `cinder-manage db sync` |
| Upgrade Jobs | `{name}-db-expand`, `{name}-db-migrate`, `{name}-db-contract` | The three release-upgrade phases |
| DB-purge CronJob | `{name}-db-purge` | `cinder-manage db purge <days>` |
| Scheduler Deployment | `{name}-scheduler` | Also the `[DEFAULT] host` of every scheduler pod |
| Volume Deployment | `{name}-volume-{backend}` | One per attached `CinderBackend` |
| Backup Deployment | `{name}-backup` | Only while a backup backend is projected |
| Service-remove Job | `{name}-{backend}-service-remove` | Runs once while a backend detaches |

Two of those names are contracts with cinder rather than with Kubernetes. The
volume service of a backend registers as `{name}@{backend}`, which is the
identity every volume it creates is keyed by, and the operator reports it in
`status.volumeServices`. Every scheduler pod registers as `{name}-scheduler`,
one registry row for the whole Deployment.

The mount path is a contract with os-brick. A backend's export is mounted at
`/var/lib/cinder/mnt/<md5 of "server:path">`, the hex MD5 the remotefs driver
derives, because cinder resolves the provider location of every volume to that
path and a mount anywhere else leaves the volumes it holds unreachable. For the
export `nfs-server.openstack.svc.cluster.local:/volumes` the directory is:

```text
/var/lib/cinder/mnt/6f3cb55ed3b423dbb7791aaf3783754f
```

The backup target is mounted the same way below
`/var/lib/cinder/backup_mount/`.

`metadata.name` is capped at 43 characters, which is what the 52-character
CronJob cap leaves once `-db-purge` is appended. The satellite names have their
own budget: `metadata.name` plus `spec.cinderRef.name` of a `CinderBackend` may
not exceed 47 characters, because the service-remove Job's name is copied into a
label value Kubernetes caps at 63.

## Example

```yaml
apiVersion: cinder.openstack.c5c3.io/v1alpha1
kind: Cinder
metadata:
  name: cinder
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  image:
    repository: ghcr.io/c5c3/cinder
    tag: "2025.2"
  api:
    deployment:
      replicas: 3
  database:
    clusterRef:
      name: openstack-mariadb
    database: cinder
    secretRef:
      name: cinder-db
  cache:
    clusterRef:
      name: openstack-memcached
  messaging:
    clusterRef:
      name: openstack-rabbitmq
  keystoneEndpoint: http://keystone.openstack.svc.cluster.local:5000/v3
  serviceUser:
    secretRef:
      name: cinder-keystone
  glanceEndpoint: http://glance.openstack.svc.cluster.local:9292
```
