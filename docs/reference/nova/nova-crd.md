---
title: Nova CRD
quadrant: operator
---

# Nova CRD

`novas.nova.openstack.c5c3.io/v1alpha1`, kind `Nova`. The CRD is generated from
`operators/nova/api/v1alpha1/nova_types.go`; the validating and defaulting
webhooks live in `operators/nova/api/v1alpha1/nova_webhook.go`. The Helm chart
ships a synced copy (`make sync-crds` / `make verify-crd-sync`).

One `Nova` CR describes the compute control plane: its OpenStack release, the
container image, the two database schemas, the cache and message-bus
connections, the Keystone integration, the services Nova calls as a client, and
the pod-level knobs of its five Deployments. The compute nodes are not in this
spec. They read the contract Secret `status.computeConfigSecretRef` names.

`kubectl get novas` shows Ready
(`.status.conditions[?(@.type=='Ready')].status`), Release
(`.status.installedRelease`), Endpoint (`.status.endpoint`) and Age.

## Spec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `openStackRelease` | `string` | yes | The OpenStack release the operator deploys and drives; pattern `^\d{4}\.[12]$` (the `YYYY.N` cadence, `N` in {1,2}). It governs install and upgrade tracking: `status.installedRelease` is promoted to it after a successful migration. Kept separate from the image tag so a digest-pinned image still resolves a schema |
| `image` | `ImageSpec` | yes | The container image every process runs: the five Deployments, the migration Jobs and the archive CronJob. Exactly one of `tag` or `digest` (shared CEL rule, re-checked by the webhook) |
| `apiDatabase` | `DatabaseSpec` | yes | The MariaDB connection of the `nova_api` schema: the cell map, the flavors, the instance-to-cell mappings and the API-level quotas. The operator names its managed instance `{name}-api`. Exactly one of `clusterRef` (managed) or `host` (brownfield); `credentialsMode` (`Static` \| `Dynamic`, where `Dynamic` requires `clusterRef`), `secretRef`, and optional `tls`. The rules are inherited from `commonv1.DatabaseSpec`. The schema name and the managed/brownfield mode are immutable (two CEL transition rules, mirrored by the webhook): the schema holds the cell map and every instance mapping |
| `database` | `DatabaseSpec` | yes | The MariaDB connection of the cell schema: the instances, their migrations, their block-device mappings. The operator names its managed instance `{name}` and grants the same user on `{database}_cell0` as well, so the name is bounded at 58 characters. It must name a different schema than `apiDatabase`, `apiDatabase` must not name its cell0, and both use the same `credentialsMode` (three CEL rules on the spec). The schema name and the managed/brownfield mode are immutable (two CEL transition rules, mirrored by the webhook): the cell mappings store the schema name |
| `cache` | `CacheSpec` | yes | Memcached. Exactly one of `clusterRef` (managed) or `servers` (brownfield). It backs both `[keystone_authtoken] memcached_servers` and the `[cache]` block Nova keeps its resolved cell mappings in |
| `messaging` | `MessagingSpec` | yes | The RabbitMQ connection, with no optional form: the API hands every instance request to the conductor over the bus, the conductor asks the scheduler for a host over the bus, and every `nova-compute` joins the same way. Exactly one of `clusterRef` (managed) or `secretRef` (brownfield), plus optional `tls`. A brownfield URL must name an explicit port and must not use an IPv6 literal host: nova expands the cell mapping's `{hostname}:{port}` template from it |
| `api` | [`NovaAPISpec`](#novaapispec) | no | The API Deployment's pod-level block and its uWSGI parameters |
| `metadata` | [`NovaMetadataSpec`](#novametadataspec) | yes | The metadata Deployment, its uWSGI parameters, the shared secret it verifies proxied requests with, and its own gateway block. Required because the shared secret has no default |
| `scheduler` | [`NovaSchedulerSpec`](#novaschedulerspec) | no | The scheduler Deployment and its worker count |
| `conductor` | [`NovaConductorSpec`](#novaconductorspec) | no | The conductor Deployment and its worker count |
| `consoleProxy` | [`NovaConsoleProxySpec`](#novaconsoleproxyspec) | no | The console proxy: the switch that projects it, its Deployment, and its own gateway block |
| `keystoneEndpoint` | `string` | yes | The Keystone auth URL rendered as `[keystone_authtoken] auth_url` and as the `auth_url` of every client section; `MinLength=1`, pattern `^https?://`, and the webhook also requires a parseable URL with a host. Nova reaches it server-side on every request and before every outgoing call, so it must resolve from inside the cluster. Nova has no Keystone-free posture: an instance boot needs a Placement allocation, a Neutron port and a Glance image, and all three are authenticated calls |
| `keystonePublicEndpoint` | `string` | no | The browser-facing Keystone base URL rendered as `www_authenticate_uri`, the address a 401 points unauthenticated clients at. When empty the operator falls back to `keystoneEndpoint` at render time (`EffectiveKeystonePublicEndpoint`), correct only when the internal and public URLs coincide |
| `serviceUser` | [`ServiceUserSpec`](#serviceuserspec) | yes | The Keystone service account and the Secret holding its password. The same account validates tokens and makes every outgoing call |
| `region` | `string` | no | The Keystone region (`region_name` in both identity sections and in every client section); omitted when empty, and Nova then uses the catalog's default region |
| `endpoints` | [`NovaEndpointsSpec`](#novaendpointsspec) | no | Pins the services Nova calls as a client and switches the two optional ones on |
| `dbArchive` | [`DBArchiveSpec`](#dbarchivespec) | no | The recurring archive that moves Nova's soft-deleted rows into the shadow tables. A nil block resolves like an empty one: `@daily`, 1000 rows per table per batch, one second between batches, no retention window, not suspended |
| `gateway` | `GatewaySpec` | no | External exposure of the API through a Gateway API HTTPRoute on port 8774; requires `hostname` and `parentRef.name` |
| `networkPolicy` | `NetworkPolicySpec` | no | Ingress restricted to TCP 8774, 8775 and 6080 from the listed sources; egress auto-derived. At least one ingress source is required (fail-closed). See [Network policy](#network-policy) |
| `autoscaling` | `AutoscalingSpec` | no | HPA bounds and CPU/memory utilization targets. It reaches the API Deployment alone |
| `logging` | `LoggingSpec` | no | oslo.log derivation: `format` (`text`/`json`), `level`, `debug`, `perLoggerLevels`. Materialized by the defaulting webhook to `text`/`INFO`/`debug: false` |
| `extraConfig` | `map[string]map[string]string` | no | Free-form INI sections for options with no dedicated field. See [extraConfig](#extraconfig) |
| `secretStoreRef` | `SecretStoreRefSpec` | no | Selects the External Secrets store `SecretsReady` is resolved against: `kind` (`ClusterSecretStore` \| `SecretStore`, default `ClusterSecretStore`) and a required `name`. When omitted the shared cluster-scoped `openbao-cluster-store` is used |
| `targetClusterRef` | `TargetClusterRefSpec` | no | Names the registered target cluster that receives this Nova's children: the five Deployments, the Services, the ConfigMaps, the Secrets, the Jobs, the CronJob and the database CRs. The CR itself does not move, and neither do its status, its finalizers or the webhooks that admit it. Immutable (two CEL transition rules, mirrored by the webhook): adding, removing or renaming it strands the children on the previously selected cluster; delete and recreate instead. See [Target Clusters](../target-clusters.md) |

### NovaAPISpec

The API is the only Deployment that scales horizontally.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `deployment` | `DeploymentSpec` | no | `replicas: 3` | Pod-level knobs: `replicas`, `resources` (resolved per resource when the pod is rendered: 100m CPU request, no CPU limit, and memory sized from `spec.api.uwsgi`, 512Mi as request and limit at its defaults, see the [resource defaults](../keystone/keystone-crd.md#resource-defaults)), `terminationGracePeriodSeconds` (30), `preStopSleepSeconds` (5), `strategy`, `topologySpreadConstraints`, `priorityClassName` |
| `uwsgi` | `UWSGISpec` | no | materialized | uWSGI parameters: `processes` (2), `threads` (1), `httpKeepAlive` (true), `harakiri` and `httpKeepAliveTimeout` (both omitted when unset). The defaulting webhook materializes the block, so the API always runs with the documented values |

### NovaMetadataSpec

The metadata API answers an instance's call to 169.254.169.254. The request
does not arrive from the instance: the Neutron metadata agent proxies it and
signs it with a shared secret, which is what tells this API which instance
asked.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `deployment` | `DeploymentSpec` | no | `replicas: 1` | The same pod-level knobs, with memory sized from `spec.metadata.uwsgi` (512Mi at its defaults). The front end holds no state between requests, so the count may be raised |
| `uwsgi` | `UWSGISpec` | no | materialized | The same uWSGI parameters as the API's |
| `sharedSecretRef` | `SecretRefSpec` | yes | `key` to `shared_secret` | The Secret holding the value the Neutron metadata agent signs proxied requests with. The operator reads it rather than generating one: the same value has to reach the `NeutronMetadataAgent`, and a value only this side knows leaves every metadata request rejected |
| `gateway` | `GatewaySpec` | no | | External exposure of the metadata API on a hostname of its own. Rarely wanted: the metadata agent dials the Service from inside the cluster |

### NovaSchedulerSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `deployment` | `DeploymentSpec` | no | `replicas: 1`, `terminationGracePeriodSeconds: 200` | The same pod-level knobs, with memory sized from `workers`, one single-threaded process each: 512Mi at the default two, 656Mi at three. Schedulers are peers that read the same host state out of Placement and hold nothing between requests, so the count may be raised |
| `workers` | `*int32` (Minimum=1) | no | `2` | `nova-scheduler` worker processes per pod, rendered into the scheduler's own overlay. Raising it multiplies the database and bus connections the pod holds |

### NovaConductorSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `deployment` | `DeploymentSpec` | no | `replicas: 1`, `terminationGracePeriodSeconds: 200` | The same pod-level knobs, with memory sized from `workers` like the scheduler's (512Mi at the default two) |
| `workers` | `*int32` (Minimum=1) | no | `2` | `nova-conductor` worker processes per pod, rendered into the conductor's own overlay |

The raised grace period is the one place these two blocks depart from the
shared defaults. Both shut down by draining their RPC server, which nova
33.0.0 completes in 165 to 167 seconds, so the shared 30-second window would
end every rollout in a SIGKILL with requests still in flight. An explicit value
is left alone.

The one-replica defaults of the metadata, scheduler and conductor blocks are
applied by the defaulting webhook and reach the absent block only.
`commonv1.DeploymentSpec.Replicas` carries `+kubebuilder:default=3`, which the
API server materializes as soon as the CR carries a `deployment` object at all,
before any mutating webhook runs.

### NovaConsoleProxySpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `enabled` | `*bool` | no | `true` | Projects the proxy. Setting it to `false` deletes the Deployment, the Service, the HTTPRoute and the proxy's NetworkPolicy, renders `[vnc] enabled = false` in `nova.conf` and in the compute contract, and the defaulting webhook removes `consoleProxy.deployment` in the same request |
| `deployment` | `*DeploymentSpec` | no | `replicas: 1` while enabled | The pod-level knobs. The field is a pointer so an absent block stays absent: a value block would serialize as `deployment: {}` on every CR, the API server would fill it with three replicas, and the console rule below would fire on a block nobody wrote. For the same reason the defaulting webhook removes the block it materialized once the proxy is switched off, so a patch that sets only `enabled: false` is admitted. The proxy runs one single-threaded process, so a block that names neither CPU nor memory, or no block at all, renders a 100m CPU request, no CPU limit, and 368Mi as memory request and limit |
| `gateway` | `GatewaySpec` | no | | External exposure of the proxy on a hostname of its own. The console URL is `https://<hostname>/vnc_lite.html?path=%3Ftoken%3D<token>`, and the WebSocket that follows the page opens on `/`, so the two cannot be split off the API's hostname by path. For the same reason `path` must be empty or `/` (webhook): a prefix route would match neither |

### ServiceUserSpec

The identity fields are webhook-defaulted, so a minimal block need only supply
the password Secret reference.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `username` | `string` | no | `nova` | Keystone username (`username` in both identity sections and in the credential-carrying client sections) |
| `projectName` | `string` | no | `service` | The project the service user scopes to |
| `userDomainName` | `string` | no | `Default` | The domain the service user lives in |
| `projectDomainName` | `string` | no | `Default` | The domain the service project lives in |
| `secretRef` | `SecretRefSpec` | yes | `key` to `password` | The Secret holding the service-user password. The value is injected as `OS_KEYSTONE_AUTHTOKEN__PASSWORD`, `OS_SERVICE_USER__PASSWORD`, `OS_PLACEMENT__PASSWORD`, `OS_NEUTRON__PASSWORD` and, with Cinder enabled, `OS_CINDER__PASSWORD`; it is never rendered into config |

### NovaEndpointsSpec

Each block configures one client section. An empty override leaves Nova
resolving the service from the Keystone catalog with `valid_interfaces =
internal`, which is the right answer for a colocated control plane. An override
is for the deployment whose catalog carries an address the Nova pods cannot
reach, or none at all.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `placement.override` | `string` | no | | `[placement] endpoint_override`; pattern `^https?://`. Nova claims every instance's resources here before it boots, so the service is mandatory and the block only decides how it is addressed |
| `neutron.override` | `string` | no | | `[neutron] endpoint_override`; same pattern. Nova creates and binds a port for every instance |
| `glance.override` | `string` | no | | `[glance] endpoint_override`; same pattern. Nova reads the image of every instance it boots |
| `cinder.enabled` | `bool` | no | `false` | Renders `[cinder]` at all. A standalone Nova without block storage has no volume service to name |
| `cinder.override` | `string` | no | | `[cinder] endpoint_template`, read only while `cinder.enabled` is true. The section addresses the volume service by catalog tuple (`catalog_info = block-storage:cinder:internalURL`), so its addressing keys differ from the siblings above |
| `barbican.enabled` | `bool` | no | `false` | Renders `[key_manager]` and `[barbican]`, which is what lets Nova read the key of an encrypted volume |
| `barbican.override` | `string` | no | | `[barbican] barbican_endpoint`, read only while `barbican.enabled` is true |

### DBArchiveSpec

Nova never hard-deletes on its own: deleting an instance flips its row to
deleted, so the tables grow for the lifetime of the deployment. The operator
projects a CronJob running `nova-manage db archive_deleted_rows` on every Nova,
whether or not the block is set. The archive is a move: the rows survive in the
shadow tables and stay available for accounting, and reclaiming that space is a
separate `nova-manage db purge` the operator does not schedule. A run repeats
bounded batches until one finds nothing left to move or its 50-minute budget is
spent; the next run continues where it stopped.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `schedule` | `string` | no | `@daily` | The cron expression the CronJob runs on. Checked by the webhook against the cron grammar, the one rule with no CRD-schema counterpart: a regex that accepts a descriptor like `@daily` alongside 5-field expressions would also accept invalid ones |
| `maxRows` | `*int32` (Minimum=1) | no | `1000` | The `--max_rows` bound, how many rows one batch moves per table. It keeps a batch on a long-neglected database from holding table locks for the length of the backlog |
| `sleep` | `*int32` (Minimum=0) | no | `1` | The seconds the run pauses between batches. Zero runs the batches back to back, which finishes sooner at the cost of the database serving the API at the same time |
| `retentionDays` | `*int32` (Minimum=1) | no | unset | When set, the run passes `--before` with today's date minus this many days, so a row soft-deleted inside the window stays in the live table, and `--task-log` with it. Unset passes neither, so every soft-deleted row is eligible and `task_log` is left alone: its rows are never soft-deleted, and archiving them without a date would move the audit period the `os-instance_usage_audit_log` API still reports on. Lowering it, or dropping it after it was set, widens what the next run moves, so the webhook warns |
| `suspend` | `bool` | no | `false` | Pauses the CronJob without deleting it. `DBArchiveReady` stays True under the reason `DBArchiveSuspended` |

The settings are resolved at reconcile time and never written back into the
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
| `[DEFAULT]` | `transport_url` | Env-injected via `OS_DEFAULT__TRANSPORT_URL`, so a file value is inert at runtime and only copies the broker credentials into the rendered config |
| `[DEFAULT]` | `web` | The directory the console proxy serves the noVNC client from. The operator mounts the client the image ships at this path; another path either is empty or is not that client, and the browser gets a blank page with no error from the proxy |
| `[database]`, `[api_database]` | `connection` | Env-injected via `OS_DATABASE__CONNECTION` and `OS_API_DATABASE__CONNECTION`, same reasoning as `transport_url` |
| `[keystone_authtoken]`, `[service_user]`, `[placement]`, `[neutron]`, `[cinder]` | `password` | Each is env-injected through its own `OS_<SECTION>__PASSWORD` override |
| `[neutron]` | `metadata_proxy_shared_secret` | Env-injected; a file value would copy the value a proxied request's signature is verified with into the config Secret every pod mounts |
| `[oslo_messaging_rabbit]` | `ssl`, `ssl_ca_file` | The first selects whether the bus that carries every RPC call is encrypted; the second points the verification at a file the operator mounts |
| `[vnc]` | `novncproxy_host`, `novncproxy_port` | The proxy's Service routes to this address and port. Another one leaves the Service with a port nothing listens on while the pod stays Ready |

Option names are validated at admission against a per-release option catalog
embedded in the operator (the release comes from `spec.openStackRelease`; a
release with no embedded catalog skips the check with an admission warning). An
unknown section or option is rejected with the section and key named, and a
deprecated-but-accepted option is admitted with a warning naming its
replacement. The owned keys are exempt from the catalog scan, which is what
keeps the keystoneauth credentials out of it: keystonemiddleware and the auth
plugins register them at run time and no catalog enumerates them. Values are
checked for one thing, a newline or carriage return in a section name, key or
value, because the rendered INI writes each verbatim and a newline injects
further config lines past the ownership and catalog gates.

`[api] auth_strategy` is absent from the registry on purpose. Neither embedded
catalog registers such an option, so the operator neither renders it nor claims
it; keystonemiddleware is wired through the api-paste pipeline instead.

## Defaulting and validation

The defaulting webhook resolves the metadata, scheduler and conductor replica
counts to one before the shared `DeploymentSpec` defaults run, materializes the
console-proxy block at one replica while the proxy is enabled and removes it
while it is disabled, gives the scheduler and the conductor the
200-second grace period and two workers each, materializes both `uwsgi` blocks
and `spec.logging`, fills the cache backend, and materializes the
`ServiceUserSpec` identity defaults together with the two Secret keys
(`password` and `shared_secret`). It leaves `spec.dbArchive` untouched, because
those fields are resolved at reconcile time.

Six CEL rules sit on `NovaSpec` itself:

| Rule | Message |
| --- | --- |
| `has(self.targetClusterRef) == has(oldSelf.targetClusterRef)` | `targetClusterRef is immutable` |
| `self.targetClusterRef.name == oldSelf.targetClusterRef.name` | `targetClusterRef is immutable` |
| `self.apiDatabase.database != self.database.database` | `apiDatabase and database must name different schemas` |
| `self.apiDatabase.database != self.database.database + '_cell0'` | `apiDatabase must not name the cell0 schema derived from database` |
| the two `credentialsMode` values, each resolved to `Static` when empty | `apiDatabase and database must use the same credentialsMode` |
| `!has(self.consoleProxy.deployment)` while the proxy is disabled | `consoleProxy.deployment must not be set when consoleProxy.enabled is false` |

Five more sit on the two database blocks:

| Field | Rule | Message |
| --- | --- | --- |
| `apiDatabase` | `self.database == oldSelf.database` | `apiDatabase.database is immutable: the schema holds the cell map and every instance mapping` |
| `apiDatabase` | `has(self.clusterRef) == has(oldSelf.clusterRef)` | `apiDatabase mode (managed clusterRef vs brownfield host) is immutable` |
| `database` | `self.database == oldSelf.database` | `database.database is immutable: the cell mappings store the schema name` |
| `database` | `has(self.clusterRef) == has(oldSelf.clusterRef)` | `database mode (managed clusterRef vs brownfield host) is immutable` |
| `database` | `size(self.database) <= 58` | `database.database must be at most 58 characters: cell0 is provisioned as <database>_cell0 under the 64-character schema limit` |

The transition rules are evaluated on UPDATE only, so the ref, the schema names
and the connection modes are frozen at the schema layer even while the
validating webhook is down. A renamed cell schema would be migrated and read by
the conductor while `nova-api` kept reading the old one through the unchanged
cell mapping, and a renamed `nova_api` schema would orphan every instance and
host mapping. The database rules on the spec keep the pair of schemas apart:
both run their own `db-sync` against a different set of migrations, so pointing
them at one schema, or the API block at cell0, installs both migration histories
into it, and they share a credential path, so a deployment cannot issue one of
them dynamic credentials and the other a static password.

The console rule only ever meets a block written past the defaulting webhook,
which removes the block while the proxy is disabled. The invalid-cr corpus
therefore carries no fixture for it; the CRD-only envtest pins it.

The validating webhook accumulates every violation into one admission response.
It repeats the schema-layer rules as defense in depth (the image tag/digest
XOR, both databases' mutual-exclusivity and Dynamic-requires-clusterRef rules,
the database rules and transition rules above, the console rule, the cache and messaging rules,
the secret-store-ref shape, the replica floor of every block, the worker floors,
the autoscaling bounds including the implicit `minReplicas` default from
`spec.api.deployment.replicas`, the network-policy ingress source, and the
hostname and `parentRef.name` of all three gateway blocks) and adds the rules
CEL cannot express: the URL shape of every endpoint field, the cron grammar of
`spec.dbArchive.schedule`, the logging enums including the per-logger-level map,
the graceful-termination arithmetic (`preStopSleepSeconds <
terminationGracePeriodSeconds`, and each `harakiri` strictly inside its own
drain window), the request-versus-limit ordering, the PriorityClass lookup, the
topology-spread selector, the `extraConfig` guards, a newline or carriage return
in any typed field rendered verbatim into `nova.conf` and the compute fragment
(`spec.region`, the four `spec.serviceUser` names, and
`spec.consoleProxy.gateway.hostname`), and the console gateway's `path`.

An update to a CR that is being deleted and leaves the spec unchanged is
admitted without validation. That is the finalizer removal the reconciler
issues, and an unchanged spec admitted earlier can fail today's rules, a
PriorityClass deleted since or an owned key a later operator rejects; rejecting
the removal would hold the CR in Terminating with nothing left to edit.

Each block's topology-spread selector is measured against the pod selector of
the Deployment that block configures. The API's selector is the shared one
narrowed by `app.kubernetes.io/component=api`; the other four are narrowed by
`metadata`, `scheduler`, `conductor` and `novncproxy`, so a constraint on one of
those blocks has to carry its component too. The console block is measured only
while the proxy is enabled and the block exists: a disabled proxy's block is
already rejected by the console rule, and running it through the pod-level rules
as well would report a replica floor on a Deployment the operator never creates.

One rule produces a warning rather than a rejection: an update that reduces
`spec.dbArchive.retentionDays`, or drops it after it was set, because the next
run then also moves the rows soft-deleted inside the old window. The rows land
in the shadow tables and do not disappear, so this stays a warning, but a
typo (3 for 30) is indistinguishable from an intended edit at admission time.
Setting a window where there was none, or raising one, is silent.

`metadata.name` is bounded at 41 characters on create. The `{name}-db-archive`
CronJob is the child with the tightest budget: Kubernetes caps a CronJob name at
52 characters, because its controller appends an 11-character timestamp suffix
to every Job it spawns. The bound applies on create only, since `metadata.name`
is immutable and an update rule could only reject a CR an earlier operator
version already admitted, including the finalizer-removal update that completes
its deletion. Such a grandfathered CR still reconciles: the operator collapses
the overflowing tail onto a content-stable hash and names the CronJob
`{truncated}-{hash}-db-archive`.

`metadata.name` also must not end in `-api`, `-metadata`, `-scheduler`,
`-conductor`, `-novncproxy` or `-console`, on create only for the same reason. A
Nova named `nova` names children under each of those suffixes, and a Nova named
`nova-api` would name its cell schema's MariaDB objects and connection Secret
exactly like the first one's `nova_api` objects, so the two CRs would write, and
on deletion delete, each other's. It must not end in `-cell0` either: a Nova
named `nova` with `spec.database.database: nova` names its cell0 `Database` and
`Grant` `nova-nova-cell0`, which a Nova named `nova-nova-cell0` would name its
cell schema's objects.

## Rendered configuration

`nova.conf` is byte-identical across the supported releases, so a release bump
never rotates the ConfigMap for a configuration change that is not there. A
golden test pins the full document of two fixtures at both releases and
compares the two renders against each other.

Every process reads the same `nova.conf`. These are its sections:

| Section | What it carries |
| --- | --- |
| `[DEFAULT]` | `use_stderr = true`, `debug` from `spec.logging.debug`, and `state_path = /var/lib/nova`. `default_log_levels` joins them while `spec.logging.perLoggerLevels` is non-empty, and `log_config_append` while the format is `json` |
| `[api]` | `local_metadata_per_cell = false`: one global metadata front end instead of one per cell, so it resolves an instance through the API database |
| `[api_database]`, `[database]` | `connection = mysql+pymysql://placeholder` in both. The real URLs arrive as environment overrides, and the placeholder is there so oslo.config parses the file before they apply |
| `[keystone_authtoken]` | The token middleware: `auth_type`, `auth_url`, `www_authenticate_uri`, the four identity fields, `memcached_servers`, and `region_name` while `spec.region` is set. No password |
| `[service_user]` | The same identity plus `send_service_user_token = true`, which is what lets a call outlive the user token that started the request |
| `[placement]`, `[neutron]` | The client credentials plus `valid_interfaces = internal`, and `endpoint_override` when the block pins one |
| `[glance]` | `valid_interfaces = internal`, plus `region_name` and `endpoint_override` when set. No credentials: Nova reads an image with the token of the request it is serving |
| `[cinder]` | Only while `spec.endpoints.cinder.enabled`: the client credentials, `catalog_info = block-storage:cinder:internalURL`, `os_region_name` and `endpoint_template` when set |
| `[key_manager]`, `[barbican]` | Only while `spec.endpoints.barbican.enabled`: `backend = barbican`, then `auth_endpoint`, `barbican_endpoint_type = internal`, `send_service_user_token = true`, plus `barbican_region_name` and `barbican_endpoint` when set |
| `[oslo_messaging_rabbit]` | `rabbit_quorum_queue`, `rabbit_transient_quorum_queue` and `use_queue_manager`, plus `ssl` and `ssl_ca_file` while `spec.messaging.tls` is set |
| `[oslo_messaging_notifications]` | `driver = noop` |
| `[oslo_concurrency]` | `lock_path = /var/lib/nova/tmp` |
| `[upgrade_levels]` | `compute = auto`, which caps the compute RPC version at the oldest `nova-compute` still registered, so a control plane upgraded ahead of its computes keeps talking to them |
| `[cache]` | `enabled = true`, the backend from `spec.cache.backend`, and `memcache_servers` derived from the cache block |
| `[scheduler]` | `discover_hosts_in_cells_interval = 300`, the periodic that maps a newly registered compute node into the cell. The worker count is not here; it is the scheduler's own overlay |
| `[vnc]` | `enabled` from `spec.consoleProxy.enabled`, and `novncproxy_base_url` while the proxy runs: the gateway hostname when one is set, the cluster-local Service URL otherwise |

`[DEFAULT] host` is absent, so every process registers under its own pod name.
No password is in the file: the identity sections are rendered without one and
each arrives as an environment override.

Four overlays ship beside `nova.conf` in the same ConfigMap, one per role that
owns options the others must not read. They are assembled by string formatting
instead of by the INI renderer, and a test pins them byte for byte.

`metadata.conf`, the key that makes the metadata API trust the instance
identity in the headers of a proxied request:

```ini
[neutron]
service_metadata_proxy = true
```

`scheduler.conf` and `conductor.conf`, each carrying the worker count of its
own process (`spec.scheduler.workers` and `spec.conductor.workers`, two when
unset):

```ini
[scheduler]
workers = 2
```

```ini
[conductor]
workers = 2
```

`novncproxy.conf`, the client directory the proxy serves and the address pair
its Service routes to. The empty `[api_database]` connection blanks the
placeholder of the shared document: the proxy gets no nova_api credentials, and
nova reads a non-empty value as a reachable API database:

```ini
[DEFAULT]
web = /usr/share/novnc

[api_database]
connection =

[vnc]
novncproxy_host = 0.0.0.0
novncproxy_port = 6080
```

All four ship on every render, including the console proxy's while the proxy is
disabled. The ConfigMap is one object for the whole CR, and a key no pod mounts
costs nothing, while a key that came and went with a switch would rotate the
ConfigMap and roll the four workloads that do not read it.

## Config directories and mounts

oslo.config reads every file in a `--config-dir`, so each process is given the
shared directory and, where it owns one, an overlay directory of its own.

| Path | Content | Read by |
| --- | --- | --- |
| `/etc/nova/nova.conf.d` | `nova.conf`, plus `logging.ini` when the render produced one, plus `metadata.conf` for the metadata API alone | All five processes, the migration Jobs and the archive CronJob |
| `/etc/nova/scheduler.conf.d` | `scheduler.conf` | The scheduler pods |
| `/etc/nova/conductor.conf.d` | `conductor.conf` | The conductor pods |
| `/etc/nova/novncproxy.conf.d` | `novncproxy.conf` | The console proxy pods |
| `/var/lib/nova` | `[DEFAULT] state_path`, an `emptyDir`; the `[oslo_concurrency] lock_path` directory `tmp` lives below it | Every process |
| `/tmp` | Writable scratch beside the read-only root filesystem | Every process and every Job |
| `/etc/nova-db-tls/api/`, `/etc/nova-db-tls/cell/` | `ca.crt`, `tls.crt`, `tls.key` of one schema each | Only while that block's `tls` is enabled |
| `/etc/rabbitmq-ca` | `ca.crt`, the file `ssl_ca_file` names | Only while `spec.messaging.tls` is set |

The two db-tls directories sit outside `/etc/nova` rather than under it: the
config mount occupies `/etc/nova/nova.conf.d` read-only, and a mount nested
inside a read-only tmpfs has no mountpoint directory the runtime could create.

The three console-script roles read their overlay from a directory and not from
a file name, because they are launched with two `--config-dir` arguments:

```text
nova-scheduler --config-dir /etc/nova/nova.conf.d --config-dir /etc/nova/scheduler.conf.d
```

The two HTTP front ends have no such argument list. They are WSGI applications
launched with `uwsgi --module`, and they name the documents they load through
two environment variables instead:

| Variable | Value |
| --- | --- |
| `OS_NOVA_CONFIG_DIR` | `/etc/nova/nova.conf.d` |
| `OS_NOVA_CONFIG_FILES` (API) | `/var/lib/openstack/etc/nova/api-paste.ini;nova.conf` |
| `OS_NOVA_CONFIG_FILES` (metadata) | `/var/lib/openstack/etc/nova/api-paste.ini;nova.conf;metadata.conf` |

The first entry is absolute, and its position is fixed. `wsgi_app.py` joins
every entry to `OS_NOVA_CONFIG_DIR` with `os.path.join`, which returns an
absolute entry unchanged, and `deploy.loadapp` reads the first entry of the list
as the paste configuration. An `api-paste.ini` anywhere else in the list is a
document nova loads as service configuration.

The migration Jobs and the archive CronJob mount the whole ConfigMap at
`/etc/nova/nova.conf.d`. They need no file selection, and the role overlays they
see beside `nova.conf` set worker counts and listen addresses `nova-manage` does
not read.

## Environment overrides

Every credential reaches the processes as an oslo.config `OS_<GROUP>__<OPTION>`
environment override sourced from a Secret, so none of them is written into the
ConfigMap the pods mount. The order is fixed, so a rebuilt pod template is
byte-identical to the live one and no workload rolls on a reordered slice.

| Variable | Source | Overrides | Carried by |
| --- | --- | --- | --- |
| `OS_DEFAULT__TRANSPORT_URL` | `{name}-transport-url` | `[DEFAULT] transport_url` | Every role |
| `OS_DATABASE__CONNECTION` | `{name}-db-connection` | `[database] connection` | Every role |
| `OS_API_DATABASE__CONNECTION` | `{name}-api-db-connection` | `[api_database] connection` | Every role but the console proxy |
| `OS_KEYSTONE_AUTHTOKEN__PASSWORD` | `spec.serviceUser.secretRef` | `[keystone_authtoken] password` | Every role |
| `OS_SERVICE_USER__PASSWORD` | `spec.serviceUser.secretRef` | `[service_user] password` | Every role |
| `OS_PLACEMENT__PASSWORD` | `spec.serviceUser.secretRef` | `[placement] password` | Every role |
| `OS_NEUTRON__PASSWORD` | `spec.serviceUser.secretRef` | `[neutron] password` | Every role |
| `OS_CINDER__PASSWORD` | `spec.serviceUser.secretRef` | `[cinder] password` | Every role, only while `spec.endpoints.cinder.enabled` |
| `OS_NEUTRON__METADATA_PROXY_SHARED_SECRET` | `spec.metadata.sharedSecretRef` | `[neutron] metadata_proxy_shared_secret` | The metadata API alone |
| `NOVA_AMQP_PORT` | the resolved transport URL | nothing; the readiness probe reads it | The scheduler and the conductor |

"Every role" includes the migration Jobs and the archive CronJob. They read
neither the bus nor Keystone, but an override is inert without the section that
consumes it, and one environment for every process is what keeps a Job from
migrating a different database than the API serves. The console proxy is the one
exception: it reads console tokens out of the cell schema and opens no
`nova_api` connection, so an override for a section it does not configure would
only be an unused Secret reference.

`NOVA_AMQP_PORT` is a plain shell variable rather than an oslo.config override.
The `nova-amqp-ready` probe reads it, and the port travels in the environment so
the value stays out of the Unhealthy event the kubelet copies a probe's output
into. The archive CronJob takes two more of the same kind, `MAX_ROWS` and
`SLEEP`, plus `RETENTION_DAYS` while a window is configured. Its script turns
`MAX_ROWS` and `RETENTION_DAYS` into `nova-manage` arguments and pauses `SLEEP`
seconds between batches.

Each credential is digested into a pod-template annotation, so a rotation at the
OpenBao source rolls the pods that consume it:

| Annotation | Digest of | Carried by |
| --- | --- | --- |
| `nova.c5c3.io/api-db-connection-hash` | The `nova_api` DSN | Every workload |
| `nova.c5c3.io/db-connection-hash` | The cell DSN | Every workload |
| `nova.c5c3.io/authtoken-hash` | The service-user password | Every workload |
| `nova.c5c3.io/transport-url-hash` | The transport URL | Every workload |
| `nova.c5c3.io/metadata-secret-hash` | The metadata shared secret | The metadata API alone |
| `nova.c5c3.io/installed-release` | `status.installedRelease` | Every role but the API |

The release annotation is what rolls the four non-API roles once more after the
contract phase of an upgrade. The scheduler and the conductor cache the RPC
version their peers announced at startup, and that cache pins the wire format
for the life of the process; the metadata API and the console proxy read rows
the contract phase rewrites. The API carries no such stamp, because it rolled
during the `RollingUpdate` phase and came up holding the new minimum. Each
digest annotation is stamped only while its value is non-empty, so a pass that
stopped at a credential gate leaves the annotation alone instead of clearing it
and rolling every pod.

## Status

| Field | Description |
| --- | --- |
| `conditions` | List-map keyed by `type`; see the [reconciler reference](./nova-reconciler.md#conditions) for the vocabulary |
| `observedGeneration` | The `.metadata.generation` last reconciled |
| `endpoint` | The Nova API URL: `https://{gateway.hostname}/` when a gateway is set, otherwise the cluster-local Service URL |
| `installedRelease` | The OpenStack release whose schema is currently installed, promoted to `spec.openStackRelease` after the upgrade completes (or after the first `db-sync` on a fresh install) |
| `targetRelease` | The release being upgraded to during an active transition. Set when the upgrade initiates, cleared on completion or abort |
| `upgradePhase` | The current phase during an active release upgrade (`Expanding`, `Migrating`, `RollingUpdate`, `Contracting`); empty when no upgrade is in flight |
| `cells` | One entry per mapped cell, `name` and `uuid`, cell0 first. The UUIDs are read back off the `db-sync` Job's termination log, because nova generates them at map time and a per-cell `nova-manage cell_v2` command addresses a cell by UUID |
| `computeConfigSecretRef` | Names the compute-contract Secret, `{name}-compute-config` |

## Compute contract

`{name}-compute-config` is the whole handover to the compute nodes. It keeps
one name for the lifetime of the CR and is updated in place: a consumer mounts
it by name, so a content-hashed name of the kind the config ConfigMap carries
would break that mount on every rotation.

| Key | Content |
| --- | --- |
| `nova-compute.conf` | The `nova.conf` fragment a `nova-compute` reads |
| `transport_url` | The same `rabbit://` URL the control-plane pods use |
| `password` | The service-user password |
| `metadata_proxy_shared_secret` | The value the Neutron metadata agent signs proxied requests with |
| `cell_name` | `cell1`, the single real cell the `db-sync` Job maps |
| `ca.crt` | The broker's CA bundle, only while `spec.messaging.tls` is set |

The fragment carries `[DEFAULT]` (`use_stderr`, `debug`),
`[keystone_authtoken]`, `[service_user]`, `[placement]`, `[neutron]`,
`[glance]`, `[oslo_messaging_rabbit]`, `[oslo_messaging_notifications]`,
`[upgrade_levels]` and `[vnc]`, plus `[cinder]`, `[key_manager]` and
`[barbican]` under the same switches the shared document uses. Every section
comes from the helper the control plane's own `nova.conf` is rendered with, so
the two documents name one service the same way or neither does, and switch the
console the same way: `[vnc] enabled` defaults to true on a compute, so a
disabled proxy is rendered as `enabled = false` rather than left out.

The fragment is rendered from typed spec fields, so it is checked for a newline
or carriage return before the Secret is written. A value carrying one sets
`ComputeConfigReady=False` under `ComputeConfigError` and leaves the published
Secret as it was, the way the Config step keeps its last-good ConfigMap.

What the fragment leaves out is the more interesting half:

- No `[database]`, `[api_database]` or `[api]`. A compute node reaches no schema
  directly; the conductor reads the cell database on its behalf, which is the
  whole point of the split.
- No `[cache]`, and no `memcached_servers` in `[keystone_authtoken]`. The
  memcached this deployment runs is a management-cluster Service a compute
  cluster does not resolve. `www_authenticate_uri` stays: it is an address a 401
  points a client at, not one this process dials.
- No `[neutron] service_metadata_proxy` and no
  `metadata_proxy_shared_secret`. Both belong to the metadata API that verifies
  a proxied request, not to the compute. The shared secret travels as its own
  Secret key instead.
- No `[scheduler]`, `[conductor]` or `[oslo_concurrency]`, which configure
  processes a compute node does not run, and no `state_path` or
  `log_config_append`, which name paths inside a pod this operator builds.
- No `spec.extraConfig`. The overrides configure the control plane the CR
  author runs, while the fragment is loaded by hypervisors in another trust
  domain, where a section of the author's choosing would reach options no typed
  field exposes: the privsep helpers, libvirt, the instance paths. An override a
  compute needs as well belongs to the compute deployment.

The mount path is part of the contract. `[oslo_messaging_rabbit] ssl_ca_file`
resolves to `/etc/nova/compute-config/ca.crt`, so a compute that projects the
Secret anywhere else finds no CA bundle where its own config says one is.

## Network policy

While `spec.networkPolicy` is set, one NetworkPolicy covers every pod of the CR.
Its selector carries no component key, so the five Deployments and the Job pods
are all subject to it: they read the same two schemas, publish on the same
broker and call the same siblings, so splitting the egress set per component
would duplicate every rule and let the copies drift.

Ingress is a single rule over TCP 8774, 8775 and 6080. Its peers are the
declared `spec.networkPolicy.ingress` sources, then one peer per namespace a
Gateway fronts one of the three front ends from (deduplicated, since one Gateway
commonly carries all three routes, and without the console gateway while the
proxy is disabled, since no route fronts it then), then the operator's own
namespace so its health check can reach the API.

Egress is auto-derived in a fixed order:

1. DNS.
2. The `spec.apiDatabase` port, then the `spec.database` port. Each block
   carries its own connection parameters and may sit on a different MariaDB, so
   each contributes a rule.
3. The cache port, when the cache block yields one.
4. The `spec.keystoneEndpoint` port. Port-only, with the destination
   unrestricted, like the database and cache rules.
5. One rule holding the ports of the siblings Nova calls as a client.
6. The broker port of the resolved transport URL. It is omitted while the
   messaging step has not materialised a URL yet.
7. Everything in `spec.networkPolicy.additionalEgress`, appended last.

The sibling rule reads only the port off each endpoint. A block that pins an
override contributes that URL's port; a block that pins none contributes the
conventional port of that service, because Nova then resolves the address from
the Keystone catalog and the operator has no way to read it.

| Sibling | Port used without an override |
| --- | --- |
| Placement | 8778 |
| Neutron | 9696 |
| Glance | 9292 |
| Cinder | 8776, only while `spec.endpoints.cinder.enabled` |
| Barbican | 9311, only while `spec.endpoints.barbican.enabled` |

The console proxy dials something none of the other pods do: the VNC server of
the hypervisor an instance runs on, on a port libvirt picked from 5900 to 65535.
While the proxy is enabled, a second NetworkPolicy, `{name}-novncproxy`, selects
the proxy pods alone and opens egress to TCP 5900-65535, port-only because the
hypervisors are addressed by whatever the compute registered. It carries no
ingress policy type, so the proxy's ingress stays governed by the main policy,
and it lives apart from it because policies are additive per pod: on the main
policy the range would open for every Nova pod.

## Sub-Resource Naming Convention

Operator-managed sub-resources of the API (Deployment, Service,
PodDisruptionBudget, HorizontalPodAutoscaler, NetworkPolicy, HTTPRoute) use the
bare CR name with no suffix, matching the keystone convention. A Nova named
`nova` in the `openstack` namespace is therefore reachable in-cluster at
`nova.openstack.svc.cluster.local:8774`, the port every OpenStack client and
catalog entry assumes.

The other children carry a suffix:

| Resource | Name | Notes |
| --- | --- | --- |
| Metadata Deployment / Service | `{name}-metadata` | Port 8775; the Neutron metadata agent of every compute node dials this Service, so the name is part of the deployment's contract with Neutron |
| Scheduler Deployment | `{name}-scheduler` | No Service: the process takes its work off the bus |
| Conductor Deployment | `{name}-conductor` | No Service, same reason |
| Console proxy Deployment / Service | `{name}-novncproxy` | Port 6080, named after the process the image ships |
| Console proxy NetworkPolicy | `{name}-novncproxy` | The proxy's VNC egress, only while `spec.networkPolicy` is set and the proxy is enabled |
| Console HTTPRoute | `{name}-console` | Named after what it exposes, not after the process behind it |
| Metadata HTTPRoute | `{name}-metadata` | Only while `spec.metadata.gateway` is set |
| Config ConfigMap | `{name}-config-<hash>` | Immutable, content-hashed `nova.conf` and the four role overlays; 3 historical retained |
| API DB-connection Secret | `{name}-api-db-connection` | Derived pymysql DSN of the `nova_api` schema, stable name |
| Cell DB-connection Secret | `{name}-db-connection` | Derived pymysql DSN of the cell schema, stable name |
| Transport-URL Secret | `{name}-transport-url` | Derived `rabbit://` URL, stable name |
| Compute-contract Secret | `{name}-compute-config` | Stable name, updated in place |
| DB-sync Job | `{name}-db-sync` | Both schema migrations and the cell mapping |
| Upgrade Jobs | `{name}-db-expand`, `{name}-db-migrate`, `{name}-db-contract` | The three release-upgrade phases |
| DB-archive CronJob | `{name}-db-archive` | `nova-manage db archive_deleted_rows` |
| MariaDB `Database` / `User` / `Grant` | `{name}-api` | The `nova_api` schema, managed mode only |
| MariaDB `Database` / `User` / `Grant` | `{name}` | The cell schema, managed mode only |
| MariaDB `Database` / `Grant` | `{name}-{database}-cell0` | cell0, an additional schema on the cell block's user, so it gets no `User` of its own. The object name is the SQL name `{database}_cell0` lowercased with `_` mapped to `-`, so a Nova named `nova` with `spec.database.database: nova` derives `nova-nova-cell0` |

Two names are contracts with nova rather than with Kubernetes. The single real
cell is mapped as `cell1`, which the compute contract publishes under
`cell_name` and `status.cells` reports beside `cell0`. The holding pen's schema
is `{database}_cell0`, derived rather than configured: `nova-manage` maps it by
convention, and both names are provisioned on the same block's user, because
`nova-api` reads cell0 on every instance list and a user without it turns a
routine list into an error.

`metadata.name` is capped at 41 characters, which is what the 52-character
CronJob cap leaves once `-db-archive` is appended. It must not end in `-api`,
`-metadata`, `-scheduler`, `-conductor`, `-novncproxy` or `-console`: the Nova
named by the rest of the name builds children under exactly that name. Nor may
it end in `-cell0`, the suffix of every cell0 `Database` and `Grant`.

The pod-template annotations follow the same convention under the
`nova.c5c3.io/` prefix: `api-db-connection-hash`, `db-connection-hash`,
`authtoken-hash` and `transport-url-hash` on every workload,
`metadata-secret-hash` on the metadata pods, and `installed-release` on every
role but the API. See [Environment overrides](#environment-overrides) for what
each digests.

## Example

```yaml
apiVersion: nova.openstack.c5c3.io/v1alpha1
kind: Nova
metadata:
  name: nova
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  image:
    repository: ghcr.io/c5c3/nova
    tag: "2025.2"
  apiDatabase:
    clusterRef:
      name: openstack-mariadb
    database: nova_api
    secretRef:
      name: nova-api-db
  database:
    clusterRef:
      name: openstack-mariadb
    database: nova
    secretRef:
      name: nova-db
  cache:
    clusterRef:
      name: openstack-memcached
  messaging:
    clusterRef:
      name: openstack-rabbitmq
  keystoneEndpoint: http://keystone.openstack.svc.cluster.local:5000/v3
  serviceUser:
    secretRef:
      name: nova-keystone
  metadata:
    sharedSecretRef:
      name: nova-metadata-secret
  consoleProxy:
    gateway:
      hostname: console.example.com
      parentRef:
        name: openstack-gw
```

## Chainsaw E2E Tests

The rejection corpus lives in `tests/e2e/nova/invalid-cr`, generated by its
`_generate.py`: the release pattern, the image and database and cache and
messaging union rules, the messaging TLS rule, the archive and scheduler bounds,
the two cross-database rules, the cell0 name rules, both `extraConfig` guards,
the two `metadata.name` rules, the two empty-`name` reference rules
(`spec.metadata.sharedSecretRef`, `spec.targetClusterRef`), the region
control-character rule, the URL fields and the console gateway path.
The functional suites that reconcile a Nova to Ready are described in
[Nova E2E Test Suites](../testing/nova-e2e-tests.md).
