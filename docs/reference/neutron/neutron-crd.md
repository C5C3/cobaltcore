---
title: Neutron CRD
quadrant: operator
---

# Neutron CRD

`neutrons.neutron.openstack.c5c3.io/v1alpha1`, kind `Neutron`. The CRD is
generated from `operators/neutron/api/v1alpha1/neutron_types.go`; the defaulting
and validating webhooks live in
`operators/neutron/api/v1alpha1/neutron_webhook.go`.

One CR describes the Neutron network service as this operator runs it: the API
server behind uWSGI, the two worker Deployments that serve no HTTP, the database
and cache connections, the Keystone integration, the message bus, and the
recurring comparison of the Neutron database against the OVN Northbound
database. The mechanism driver is ML2/OVN and nothing else. The rendered
`ml2_conf.ini` sets `mechanism_drivers = ovn`, and the `[ovn]` connection
strings come from the `OVNCentral` named by `spec.ovn.centralRef`. The CR
carries one such reference, because the logical network model lives in one
Northbound database.

The bus posture follows from that. Nothing in this deployment consumes RPC: OVN
answers DHCP and metadata out of the logical model, no agent registers with the
API, and no `neutron-rpc-server` is projected. The rendered configuration sets
both RPC worker counts to zero and drops versioned notifications at the source.
`spec.messaging` stays required, because every neutron process and every
migration Job is started with `OS_DEFAULT__TRANSPORT_URL` sourced from the
derived transport-URL Secret. See [Rendered defaults](#rendered-defaults).

The OVN metadata agent is a separate kind,
[`NeutronMetadataAgent`](./neutron-metadata-agent-crd.md). It runs on the compute
and network nodes, in the privileged namespace the `OVNChassis` DaemonSets live
in, which is not a namespace the API Deployment belongs in.

`kubectl get neutrons` prints Ready
(`.status.conditions[?(@.type=='Ready')].status`), Release
(`.status.installedRelease`), Endpoint (`.status.endpoint`), and Age.

Most deployments get their `Neutron` from a ControlPlane projection. See
[`ServiceNeutronSpec`](../c5c3/controlplane-crd.md#serviceneutronspec) for which
of the fields below the plane fills and which it leaves to this CR.

## Spec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `openStackRelease` | `string` (Pattern `^\d{4}\.[12]$`) | yes | none | The OpenStack release the operator deploys and drives. It governs install and upgrade release tracking: `status.installedRelease` is promoted to this value after a successful db-sync. The pattern admits the `YYYY.N` cadence with `N` in {1, 2}, the same class the validating webhook and `release.ParseRelease` accept, so a non-cadence minor is rejected at every layer. Kept separate from the image tag so a digest-pinned image still names a schema |
| `deployment` | [`commonv1.DeploymentSpec`](../keystone/keystone-crd.md#deploymentspec) | no | `{}` | Pod-level knobs of the API Deployment: `replicas` (default 3), `resources` (resolved per resource when the pod is rendered: 100m CPU request, no CPU limit, and 512Mi memory request and limit at the default `spec.apiServer.uwsgi` counts, see the [resource defaults](../keystone/keystone-crd.md#resource-defaults)), `terminationGracePeriodSeconds`, `preStopSleepSeconds`, `strategy`, `topologySpreadConstraints`, `priorityClassName`, and the node placement `nodeSelector`, `tolerations` and `affinity` (see [NodePlacementSpec](../keystone/keystone-crd.md#nodeplacementspec)). It is also the fallback of `spec.jobs` |
| `jobs` | [`*commonv1.JobSpec`](../keystone/keystone-crd.md#jobspec) | no | `nil` | Sizes, prioritizes and places the pods of the db-sync Job, the db-expand, db-migrate and db-contract upgrade phases, and the ovn-db-sync CronJob. A field left unset falls back to `spec.deployment`. Unset resources default to a `100m` CPU request and `368Mi` memory as request and limit; ovn-db-sync gets the request floor instead (see [OVNDBSyncSpec](#ovndbsyncspec)) |
| `image` | [`commonv1.ImageSpec`](../keystone/keystone-crd.md#imagespec) | yes | none | The Neutron container image, run by the API pods, both worker Deployments, the migration Jobs, and the ovn-db-sync CronJob. `tag` and `digest` are mutually exclusive and one of the two is required. The field carries no immutability rule |
| `database` | [`commonv1.DatabaseSpec`](../keystone/keystone-crd.md#databasespec) | yes | none | The MariaDB connection, rendered into the plain `[database]` section. One of `clusterRef` (managed) or `host` (brownfield), never both, plus `database`, `secretRef`, and the optional `port`, `credentialsMode` and `tls`. `credentialsMode: Dynamic` requires `clusterRef`. `replicas` and `storageSize` sit in the schema and are read by the ControlPlane's managed-mode projection alone, so this operator ignores them |
| `cache` | [`commonv1.CacheSpec`](../keystone/keystone-crd.md#cachespec) | yes | `backend: dogpile.cache.pymemcache` | The Memcached instance backing the keystonemiddleware token cache, rendered as `[keystone_authtoken] memcached_servers`. One of `clusterRef` (managed) or `servers` (brownfield), never both. Managed mode resolves to `<clusterRef.name>:11211`; `replicas` is honoured by the ControlPlane projection alone |
| `keystoneEndpoint` | `string` (MinLength=1, Pattern `^https?://`) | yes | none | The Keystone auth URL rendered as `[keystone_authtoken] auth_url`. keystonemiddleware validates every token against it server-side, so it has to be reachable from the Neutron pods: use the cluster-local Service URL, and not an address that only resolves outside the cluster |
| `keystonePublicEndpoint` | `string` (Pattern `^https?://`) | no | falls back to `keystoneEndpoint` | The browser-facing Keystone base URL rendered as `[keystone_authtoken] www_authenticate_uri`, the address a 401 points unauthenticated clients at. Resolved at render time by `EffectiveKeystonePublicEndpoint`, so a later edit of `keystoneEndpoint` keeps being tracked by the fallback |
| `serviceUser` | [`ServiceUserSpec`](#serviceuserspec) | yes | see below | The Keystone service account Neutron authenticates as, and the Secret holding its password |
| `region` | `string` | no | `""` | The Keystone region (`[keystone_authtoken] region_name`). When empty the option is omitted and Neutron uses the catalog's default region |
| `apiServer` | [`*APIServerSpec`](#apiserverspec) | no | `nil` | uWSGI process tuning. When nil the reconciler falls back to the same constants the webhook writes into a present block |
| `workers` | [`WorkersSpec`](#workersspec) | no | `{}` | Pod-level knobs of the two worker Deployments. Both are sized by the one block |
| `messaging` | [`commonv1.MessagingSpec`](../c5c3/controlplane-crd.md#messagingspec) | yes | `replicas: 3` | The RabbitMQ connection. One of `clusterRef` (managed) or `secretRef` (brownfield), never both, plus the optional `tls` block naming a CA bundle Secret. `replicas` is read by the ControlPlane's managed-mode projection alone. A placed Neutron whose bus runs on the management cluster has to use `secretRef`: the transport-URL helper reads and writes in the CR's own namespace through the CR's own children client, which for a placed CR is the target cluster's |
| `ovn` | [`OVNSpec`](#ovnspec) | yes | none | The OVN control plane this Neutron programs |
| `nova` | [`*NovaSpec`](#novaspec) | no | `nil` (notifications off) | The compute service this Neutron notifies when a port changes state, and the Keystone account it notifies as. The presence of the block is the switch: while it is set the operator turns both `notify_nova_on_port_*` flags on and renders the `[nova]` credentials, and while it is absent the flags stay off and the section stays empty, so a port never waits for a vif-plugged event nothing sends. There is no `enabled` field, and no endpoint override: neutron's `[nova]` option group carries `region_name` and `endpoint_type` alone, so the compute endpoint is resolved from the Keystone catalog |
| `ovnDBSync` | [`*OVNDBSyncSpec`](#ovndbsyncspec) | no | `nil` (no CronJob) | The recurring `neutron-ovn-db-sync-util` run. A nil block means no CronJob at all. See [ovnDBSync](#ovndbsync) |
| `gateway` | [`*commonv1.GatewaySpec`](../keystone/keystone-crd.md#gatewayspec) | no | `nil` | External exposure through a Gateway API HTTPRoute forwarding to the `{name}` Service on port 9696. Requires `hostname` and `parentRef.name`, and takes an optional `path` and `annotations` map. Removing the block deletes the HTTPRoute. It does not change `status.endpoint`, which stays the cluster-local Service URL |
| `networkPolicy` | [`*commonv1.NetworkPolicySpec`](../keystone/keystone-crd.md#networkpolicyspec) | no | `nil` | Ingress restricted to TCP 9696 from the listed sources; egress derived for DNS, the database, the Keystone endpoint, the cache, the two OVN databases, and the broker port, with `additionalEgress` appended after it. At least one ingress source is required (fail-closed) |
| `autoscaling` | [`*commonv1.AutoscalingSpec`](../keystone/keystone-crd.md#autoscalingspec) | no | `nil` | HPA bounds and CPU/memory utilization targets for the API Deployment. At least one of `targetCPUUtilization` or `targetMemoryUtilization` is required, and an unset `minReplicas` resolves to `spec.deployment.replicas`. The two worker Deployments are never autoscaled |
| `logging` | [`*commonv1.LoggingSpec`](../keystone/keystone-crd.md#loggingspec) | no | `text` / `INFO` / `debug: false` | oslo.log derivation: `format` (`text` or `json`), `level`, `debug`, `perLoggerLevels`. Materialized by the defaulting webhook. The `json` format ships a `logging.conf` in the config ConfigMap and points `[DEFAULT] log_config_append` at it |
| `secretStoreRef` | [`*commonv1.SecretStoreRefSpec`](../keystone/keystone-crd.md#secretstorerefspec) | no | `nil` (`openbao-cluster-store`) | Selects the External Secrets store the operator resolves `SecretsReady` against: `kind` (`ClusterSecretStore` \| `SecretStore`, default `ClusterSecretStore`) and a required `name`. A namespaced store is resolved in this Neutron's own namespace. The ControlPlane projects this field onto the Neutron it owns |
| `targetClusterRef` | [`*commonv1.TargetClusterRefSpec`](../target-clusters.md#the-field) | no | `nil` (the local cluster) | Names the registered target cluster that receives this Neutron's children: the three Deployments, the ConfigMaps, the Secrets, and the database CRs. The CR itself stays on the management cluster, and so do its status and its finalizers. Immutable, enforced by two CEL transition rules and by the webhook. See [Target Clusters](../target-clusters.md) |
| `extraConfig` | `map[string]map[string]string` | no | `nil` | Free-form INI sections for options with no dedicated field. The render-time merge is `operator defaults < extraConfig`, so a user value wins, and each section is routed to the file its consumer reads. An override of an operator-owned key is honored and reported through the `ExtraConfigHealthy` condition and an `ExtraConfigOwnedKeyOverride` Warning event, except for the fifteen keys the webhook refuses outright. Option names are checked at admission against the per-release catalog embedded in the operator. See [Defaulting and validation](#defaulting-and-validation) |

`DeploymentSpec`, `AutoscalingSpec`, `NetworkPolicySpec`,
`NetworkPolicyIngressSource`, `LoggingSpec`, `GatewaySpec`,
`GatewayParentRefSpec` and `UWSGISpec` are Go aliases of the shared `commonv1`
definitions, which carry the per-field godoc and the validation markers.

### ServiceUserSpec

The name and domain fields are webhook-defaulted, so a minimal CR need only
supply the password Secret reference.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `username` | `string` | no | `neutron` | Keystone username (`[keystone_authtoken] username`) |
| `projectName` | `string` | no | `service` | The project the service user scopes to (`project_name`) |
| `userDomainName` | `string` | no | `Default` | The domain the service user lives in (`user_domain_name`) |
| `projectDomainName` | `string` | no | `Default` | The domain the service project lives in (`project_domain_name`) |
| `secretRef` | [`commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | yes | `key` → `password` | The Secret holding the service-user password. The password is injected as the `OS_KEYSTONE_AUTHTOKEN__PASSWORD` env var and never rendered into the config |

Each of the four identity fields is written verbatim into
`[keystone_authtoken]`, so the validating webhook rejects a newline or carriage
return in any of them. `spec.region` reaches the same section through the same
renderer and goes through the same check.

### NovaSpec

The notifier is a REST client of the compute API's `os-server-external-events`
resource, so it needs a Keystone account of its own and a region to resolve the
endpoint in.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `region` | `string` | no | `""` | The Keystone region the compute endpoint is resolved in (`[nova] region_name`). When empty the option is omitted and the notifier uses the catalog's default region |
| `serviceUser` | [`NovaNotifierUserSpec`](#novanotifieruserspec) | yes | see below | The Keystone account the notifier authenticates as, and the Secret holding its password |

### NovaNotifierUserSpec

The field set of [`ServiceUserSpec`](#serviceuserspec) with defaults of its own.
The account is separate from `spec.serviceUser` because the notifier calls the
compute API as a user holding the `admin` role, which the token-validation
account does not need: nova resolves the instance behind a notified port with the
caller's own context, unelevated, so a notifier holding `service` alone has every
event answered 404 and leaves the port in `BUILD`.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `username` | `string` | no | `neutron-nova` | Keystone username (`[nova] username`) |
| `projectName` | `string` | no | `service` | The project the notifier account scopes to (`project_name`) |
| `userDomainName` | `string` | no | `Default` | The domain the notifier account lives in (`user_domain_name`) |
| `projectDomainName` | `string` | no | `Default` | The domain the notifier project lives in (`project_domain_name`) |
| `secretRef` | [`commonv1.SecretRefSpec`](../keystone/keystone-crd.md#secretrefspec) | yes | `key` → `password` | The Secret holding the notifier password. It is injected as `OS_NOVA__PASSWORD` into the API Deployment, both worker Deployments and the ovn-db-sync CronJob, and never rendered into the config. The webhook requires a non-empty `name`: `serviceUser.secretRef.name must be set when spec.nova is configured: it carries the password Neutron notifies Nova with` |

The region and the four identity fields are written verbatim into `[nova]`, so
the validating webhook rejects a newline or carriage return in any of the five,
the way it does for `spec.serviceUser`.

A CR written before `spec.nova` existed configures the notifier through
`spec.extraConfig`: the two `[DEFAULT] notify_nova_on_port_*` switches and the
`[nova]` section. Those keys are now operator-owned, so after the upgrade such a
CR reports `ExtraConfigHealthy=False` with an `ExtraConfigOwnedKeyOverride`
Warning event, and its next write raises an admission warning, although nothing
it renders changes: `spec.extraConfig` still wins the merge. A carried-over
`[nova] password` is kept with a warning (see
[Rejected owned keys](#rejected-owned-keys)). Move the account into `spec.nova`,
with its password in the Secret `serviceUser.secretRef` names, and drop the
entries from `spec.extraConfig`.

### APIServerSpec

Tunes the API server process. It carries the uWSGI block alone: the API pods
start uWSGI on the `neutron.wsgi.api` module, so there is no launch mode to
select. The RPC worker counts live in `[DEFAULT]` and are fixed at zero.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `uwsgi` | [`*commonv1.UWSGISpec`](../keystone/keystone-crd.md#uwsgispec) | no | `nil` | uWSGI application-server parameters: `processes` (default 2), `threads` (default 1), `httpKeepAlive` (default true), `harakiri` and `httpKeepAliveTimeout` (both omitted when nil). A CEL rule and the webhook agree that `httpKeepAliveTimeout` may only be set while `httpKeepAlive` is true |

The concurrency this block declares also sizes the database. The SQL user's
`max_user_connections` cap is `(pods + 1) x processes x (5 + threads - 1)`, plus
`2 x (worker replicas + 1) x 5`, plus two transient Job connections; `pods` is
the autoscaling ceiling when an HPA owns the replica count, and the `+ 1` on
each term is the surge pod a rollout adds. The 5 is oslo.db's `max_pool_size`
default, which the rendered `[database]` section does not override: a neutron
process keeps up to that many pooled connections once load has touched it. A
single-threaded API process measured 4 to 5 under plain port CRUD, and every
uWSGI thread beyond the first adds one. Below that cap the last processes to
start fail their pool with MySQL error 1226 and crash-loop their pod, and under
load the process that opens the connection past it answers HTTP 500.

### WorkersSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `deployment` | [`commonv1.DeploymentSpec`](../keystone/keystone-crd.md#deploymentspec) | no | `{}` | Pod-level knobs of the worker Deployments. `replicas` (default 3) sizes both, so the default is three periodic-worker pods and three OVN maintenance-worker pods. Each worker runs one single-threaded process, so a `resources` block that names neither CPU nor memory renders a 100m CPU request, no CPU limit, and 368Mi as memory request and limit (see the [resource defaults](../keystone/keystone-crd.md#resource-defaults)). The worker Deployments render its `nodeSelector`, `tolerations` and `affinity` too; the webhook does not validate this block yet ([#1100](https://github.com/C5C3/cobaltcore/issues/1100)) |

Neither worker Deployment gets a Service, an HPA or a PodDisruptionBudget: no
client dials them, their load is the maintenance queue, and an eviction costs a
delayed maintenance pass.

### OVNSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `centralRef` | [`OVNCentralRef`](#ovncentralref) | yes | none | The [`OVNCentral`](../ovn/ovn-central-crd.md) whose Northbound and Southbound databases the ML2/OVN mechanism driver connects to |

### OVNCentralRef

Unlike the reference on `OVNChassis`, this one carries a namespace: the OVN
control plane commonly lives in the privileged networking namespace while the
Neutron API lives with the rest of the control plane. The operator reads the
connection details out of the central's status and mirrors its client Secret;
the source is never mounted into the Neutron pods.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` (MinLength=1) | yes | none | The `OVNCentral`'s name |
| `namespace` | `string` | no | this CR's namespace | The namespace the `OVNCentral` lives in. The defaulting webhook fills an empty value, so the CR records which namespace was meant when it was created and a later move of the Neutron cannot re-point it |

Both fields are resolved into the `[ovn]` connection strings, so both are
checked for control characters at admission.

### OVNDBSyncSpec

Neutron and the OVN Northbound database each hold their own copy of the logical
network model, and a write that fails halfway through leaves them apart. The
utility walks both and reports, or repairs, the difference.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `schedule` | `string` | no | `0 * * * *`, resolved at reconcile time | The standard cron expression the CronJob runs on. Checked by the validating webhook, with no CRD pattern behind it: the accepted grammar includes descriptors such as `@daily`, which no regex expresses without also rejecting valid expressions |
| `syncMode` | `string` (Enum `log`, `repair`) | no | `log` | What the utility does with the difference it finds |
| `suspend` | `bool` | no | `false` | Pauses the CronJob without deleting it. The escape hatch for a maintenance window in which a repair-mode run would fight an operator editing the Northbound database by hand |

The CronJob pod takes its priority class and node placement from
[`spec.jobs`](../keystone/keystone-crd.md#jobspec), falling back to
`spec.deployment`. Its resources resolve through the request floor rather than
the Job defaults: a `100m` CPU and a `256Mi` memory request, and no limit. The
utility loads both logical models, so its working set grows with them, and a
default memory limit would OOM-kill the run once they outgrew it.

## Rendered defaults

`reconcile_config.go` renders two files into one immutable ConfigMap,
`neutron.conf` first and `ml2_conf.ini` second. The API Deployment names them in
`OS_NEUTRON_CONFIG_DIR` and `OS_NEUTRON_CONFIG_FILES`, since uWSGI imports
`neutron.wsgi.api` and there is no argv to carry a flag; the worker Deployments,
the migration Jobs and the ovn-db-sync CronJob pass the same two files as
`--config-file` arguments. The order is the same on both paths, so an `ml2`
option set in both files resolves to the `ml2_conf.ini` value. Section names
decide the file. `ml2`, the five `ml2_type_*` sections, `ovn`, `ovn_nb_global`,
`ovs`, `ovs_driver`, `securitygroup` and `sriov_driver` go to `ml2_conf.ini`;
everything else goes to `neutron.conf`. The routing covers `spec.extraConfig`
too, so a user override lands in the file its consumer reads.

Nothing here depends on `spec.openStackRelease`: two CRs differing only in their
release render byte-identical files.

What a CR that says nothing beyond the required fields gets in `neutron.conf`:

| Section | Key | Value |
| --- | --- | --- |
| `[DEFAULT]` | `core_plugin` | `ml2` |
| `[DEFAULT]` | `service_plugins` | `ovn-router` |
| `[DEFAULT]` | `auth_strategy` | `keystone` |
| `[DEFAULT]` | `api_paste_config` | `/var/lib/openstack/etc/neutron/api-paste.ini` |
| `[DEFAULT]` | `state_path` | `/var/lib/neutron` |
| `[DEFAULT]` | `rpc_workers` | `0` |
| `[DEFAULT]` | `rpc_state_report_workers` | `0` |
| `[DEFAULT]` | `dhcp_agent_notification` | `false` |
| `[DEFAULT]` | `dns_domain` | `cobaltcore.local.` |
| `[DEFAULT]` | `notify_nova_on_port_status_changes` | `true` while `spec.nova` is set, `false` otherwise |
| `[DEFAULT]` | `notify_nova_on_port_data_changes` | `true` while `spec.nova` is set, `false` otherwise |
| `[DEFAULT]` | `use_stderr` | `true` |
| `[DEFAULT]` | `debug` | from `spec.logging.debug`, which oslo.log reads independently of the root logger level |
| `[DEFAULT]` | `default_log_levels` | the sorted `spec.logging.perLoggerLevels` CSV; omitted when the map is empty |
| `[DEFAULT]` | `log_config_append` | `/etc/neutron/logging.conf`; written only for `spec.logging.format: json` |
| `[database]` | `connection` | `mysql+pymysql://placeholder` |
| `[keystone_authtoken]` | `auth_type` | `password` |
| `[keystone_authtoken]` | `auth_url`, `www_authenticate_uri`, `username`, `project_name`, `user_domain_name`, `project_domain_name` | from `spec.keystoneEndpoint`, the effective public endpoint, and `spec.serviceUser` |
| `[keystone_authtoken]` | `region_name`, `memcached_servers` | from `spec.region` and `spec.cache`; each omitted when its source is empty |
| `[oslo_messaging_notifications]` | `driver` | `noop` |
| `[oslo_messaging_rabbit]` | `rabbit_quorum_queue`, `rabbit_transient_quorum_queue`, `use_queue_manager` | `true` |
| `[oslo_messaging_rabbit]` | `ssl`, `ssl_ca_file` | `true` and `/etc/rabbitmq-ca/ca.crt`; written only while `spec.messaging.tls` is set |
| `[oslo_concurrency]` | `lock_path` | `/var/lib/neutron/lock` |
| `[nova]` | `auth_type`, `auth_url`, `username`, `project_name`, `user_domain_name`, `project_domain_name` | `password`, `spec.keystoneEndpoint`, and the four `spec.nova.serviceUser` identity fields; written only while `spec.nova` is set |
| `[nova]` | `region_name` | from `spec.nova.region`; omitted when empty |
| `[nova]` | `endpoint_type` | `internal`, the catalog entry a co-located control plane can reach. The public one routes the notifications out of the cluster and back |
| `[nova]` | none | Without `spec.nova` the section is rendered empty. The header is what a Nova option added through `spec.extraConfig` fills in |

And in `ml2_conf.ini`:

| Section | Key | Value |
| --- | --- | --- |
| `[ml2]` | `mechanism_drivers` | `ovn` |
| `[ml2]` | `type_drivers` | `geneve,flat` |
| `[ml2]` | `tenant_network_types` | `geneve` |
| `[ml2]` | `extension_drivers` | `port_security,dns_domain_ports,tag_ports_during_bulk_creation` |
| `[ml2_type_geneve]` | `vni_ranges` | `1:65536` |
| `[ml2_type_geneve]` | `max_header_size` | `38`, the room the chassis encapsulation reserves. A larger header than the tunnel accounts for exceeds the path MTU |
| `[ml2_type_flat]` | `flat_networks` | `*` |
| `[securitygroup]` | `enable_security_group` | `true` |
| `[ovn]` | `ovn_nb_connection`, `ovn_sb_connection` | the two addresses resolved off the referenced `OVNCentral` |
| `[ovn]` | `ovn_nb_private_key`, `ovn_sb_private_key` | `/etc/ovn/tls/tls.key` |
| `[ovn]` | `ovn_nb_certificate`, `ovn_sb_certificate` | `/etc/ovn/tls/tls.crt` |
| `[ovn]` | `ovn_nb_ca_cert`, `ovn_sb_ca_cert` | `/etc/ovn/tls/ca.crt` |
| `[ovn]` | `ovn_l3_scheduler` | `leastloaded`, which spreads the gateway routers over the chassis carrying the gateway role |
| `[ovn]` | `ovn_metadata_enabled` | `true`, which is what makes OVN serve 169.254.169.254 to the instances the [`NeutronMetadataAgent`](./neutron-metadata-agent-crd.md) answers for |

Five keys carry the bus posture. `rpc_workers` and
`rpc_state_report_workers` are both zero, because nothing consumes RPC here and
no `neutron-rpc-server` is projected. `[oslo_messaging_notifications] driver` is
`noop`, so versioned notifications are dropped at the source; nothing
accumulates in a queue nobody drains. The two `notify_nova_on_port_*` options
follow `spec.nova`: the port events are REST calls to
`os-server-external-events` rather than oslo notifications, so the `noop` driver
does not reach them. A valid
`transport_url` is still needed: `spec.messaging` is required, and the URL
reaches every neutron process and every migration Job through the
`OS_DEFAULT__TRANSPORT_URL` environment override sourced from the derived
transport-URL Secret, never through the rendered file.

Two options a reader may look for are rendered nowhere. Bridge mappings are a
property of the node, so they live on the
[`OVNChassis`](../ovn/ovn-chassis-crd.md) as `spec.bridgeMappings` and reach the
local OVS as `ovn-bridge-mappings`. `enable_distributed_floating_ip` is absent
from the defaults; the option catalog knows it, so `spec.extraConfig` can set it.

Four values never enter either file: the database password, the broker password,
the service-user password, and, while `spec.nova` is set, the notifier password.
Each arrives as an oslo.config environment override (`OS_DATABASE__CONNECTION`,
`OS_DEFAULT__TRANSPORT_URL`, `OS_KEYSTONE_AUTHTOKEN__PASSWORD`,
`OS_NOVA__PASSWORD`), which is why the `[database] connection`
value in the rendered file is a placeholder URL: oslo.config parses the file
before the override is applied, so the placeholder has to be syntactically
valid.

## Defaulting and validation

The mutating webhook applies the shared `DeploymentSpec` default to both
`spec.deployment` and `spec.workers.deployment` (replicas 3) and writes no
`resources`, which the reconciler resolves when it renders each Deployment. It
materializes the cache backend `dogpile.cache.pymemcache`, materializes
`spec.logging` and its baseline (`text` / `INFO` / `debug: false`) so no
reconciler dereferences a nil pointer, and fills the `ServiceUserSpec` identity
defaults (`neutron` / `service` / `Default` / `Default`, `secretRef.key` →
`password`). Inside a **present** `spec.nova` it fills the notifier identity the
same way (`neutron-nova` / `service` / `Default` / `Default`, `secretRef.key` →
`password`); an absent block stays absent, because that is what keeps the port
notifications off. It fills an empty `spec.ovn.centralRef.namespace` with the
CR's own namespace. For the halves a CR carries it fills
`spec.messaging.secretRef.key` with `transport_url` and
`spec.messaging.tls.caBundleSecretRef.key` with `ca.crt`. When `spec.apiServer`
is present it applies the shared uWSGI leaf defaults.

Two defaults are resolved at reconcile time and never written into the stored
CR, so an unset field keeps tracking the operator default across upgrades:
`DefaultOVNDBSyncSchedule` (`0 * * * *`) and `DefaultOVNDBSyncMode` (`log`).

`metadata.name` is bounded at 40 characters, `MaxNeutronNameLength`, computed as
`MaxCronJobNameLength` (52) minus `len("-ovn-db-sync")`. The bound is enforced on
create alone: `metadata.name` is immutable, so on update the rule could only
fire against an object a pre-upgrade operator already admitted, and it would
refuse every update to that CR with no field left to edit.

```text
name must be at most %d characters: the ovn-db-sync CronJob appends %q and Kubernetes caps CronJob names at %d characters
```

The three arguments are the bound (40), the suffix (`-ovn-db-sync`), and
`MaxCronJobNameLength` (52).

An update to a CR that is being deleted and leaves the spec unchanged is
admitted without validation. That is the finalizer removal the reconciler
issues, and an unchanged spec admitted earlier can fail today's rules: a
PriorityClass deleted since, or a topology-spread constraint that still names
only the name and instance labels. Rejecting the removal would hold the CR in
`Terminating`. An update that changes the spec of a deleting CR is still
validated.

### Schema-layer rules

These hold even when the webhook is down.

| Message | Where it comes from |
| --- | --- |
| `targetClusterRef is immutable` | Two CEL transition rules on `NeutronSpec`, one for adding or removing the ref and one for renaming it |
| `exactly one of image.tag or image.digest must be set` | Inherited from `commonv1.ImageSpec` on `spec.image` |
| `exactly one of clusterRef or host must be set` | Inherited from `commonv1.DatabaseSpec` |
| `credentialsMode Dynamic requires clusterRef (managed mode)` | Inherited from `commonv1.DatabaseSpec` |
| `exactly one of clusterRef or servers must be set` | Inherited from `commonv1.CacheSpec` |
| `clusterRef.name must not contain a newline or carriage return` | Inherited from `commonv1.CacheSpec`, alongside the `^[^\n\r]*$` items pattern on `servers` |
| `exactly one of clusterRef or secretRef must be set` | Inherited from `commonv1.MessagingSpec` |
| `preStopSleepSeconds must be strictly less than terminationGracePeriodSeconds` | Inherited from `commonv1.DeploymentSpec` on both deployment blocks; the rule substitutes the effective defaults 5 and 30 for an unset pointer |
| `httpKeepAliveTimeout may only be set when httpKeepAlive is true` | Inherited from `commonv1.UWSGISpec` |
| `at least one of targetCPUUtilization or targetMemoryUtilization must be set` | Inherited from `commonv1.AutoscalingSpec` |
| `minReplicas must not exceed maxReplicas` | Inherited from `commonv1.AutoscalingSpec` |
| `at least one ingress source must be specified` | Inherited from `commonv1.NetworkPolicySpec` |
| `logger name must not be empty` | Inherited from `commonv1.LoggingSpec` on `perLoggerLevels` |
| `per-logger level must be one of DEBUG, INFO, WARNING, ERROR, CRITICAL` | Inherited from `commonv1.LoggingSpec` on `perLoggerLevels` |

The `Enum` marker on `spec.ovnDBSync.syncMode` and the `Pattern` markers on
`spec.openStackRelease`, `spec.keystoneEndpoint` and
`spec.keystonePublicEndpoint` belong here too.

### Webhook rules

The validating webhook accumulates every violation into one admission response.
Most rules repeat a schema-layer check so the invariant survives an object that
reached etcd through a bypass; the `extraConfig` rules and the cron grammar have
no schema counterpart at all.

Messaging and the OVN reference:

| Message | Trigger |
| --- | --- |
| `exactly one of clusterRef or secretRef must be set` | `spec.messaging` names both modes or neither |
| `caBundleSecretRef.name must be set when spec.messaging.tls is configured` | A TLS block with no CA bundle name has nothing to verify the broker against |
| `centralRef.name must be set (the OVNCentral this Neutron programs)` | `spec.ovn.centralRef.name` is empty. A Neutron without a control plane has nothing to program |
| `serviceUser.secretRef.name must be set when spec.nova is configured: it carries the password Neutron notifies Nova with` | `spec.nova` is present with an empty `serviceUser.secretRef.name`. A notifier with no password to authenticate with has every event refused |

Keystone endpoints and URL shapes, applied to `keystoneEndpoint` when non-empty
and to `keystonePublicEndpoint` only when set:

| Message | Trigger |
| --- | --- |
| `keystoneEndpoint must be set (the Keystone auth_url the Neutron pods reach)` | The field is empty |
| `must be a valid URL: %v` | `url.Parse` refuses the value; the argument is the parser's own error |
| `scheme must be http or https` | The parsed scheme is anything else |
| `URL must include a host` | The parsed URL carries no host |

Control characters. Each of these values is written into the rendered INI as
`%s = %s`, so a newline would inject a whole config line past the
`(section, key)`-keyed ownership and catalog gates:

| Message | Applies to |
| --- | --- |
| `value must not contain a newline or carriage return: it is rendered verbatim into neutron.conf, so a newline injects arbitrary config lines` | `spec.region`, the four `spec.serviceUser` identity fields, `spec.nova.region` and the four `spec.nova.serviceUser` identity fields (read through a zero-valued `NovaSpec` when the block is absent, so the check costs nothing there), `spec.ovn.centralRef.name`, `spec.ovn.centralRef.namespace`, and `spec.gateway.hostname` |
| `value must not contain a newline or carriage return: it is rendered verbatim into the service configuration file, so a newline injects arbitrary config lines` | `spec.cache.clusterRef.name` and each entry of `spec.cache.servers`, through the shared cache validator |

`spec.gateway.hostname` is on the first list although the config renderer never
writes it: in this operator the hostname reaches the generated HTTPRoute and
nothing else. The guard therefore sits ahead of the Gateway API's own hostname
validation, which runs a pipeline step after the config has been written and
mounted. The two Keystone endpoints need no entry at all: `url.Parse` already
refuses control bytes.

Image, replicas and graceful termination:

| Message | Trigger |
| --- | --- |
| `repository must be set` | `spec.image.repository` is empty |
| `exactly one of image.tag or image.digest must be set` | Both or neither are set |
| `replicas must be at least 1` | `spec.deployment.replicas` below 1 |
| `terminationGracePeriodSeconds must be at least 10` | `spec.deployment.terminationGracePeriodSeconds` is set below 10 |
| `preStopSleepSeconds must be non-negative` | `spec.deployment.preStopSleepSeconds` is set below 0 |
| `preStopSleepSeconds (%d) must be strictly less than terminationGracePeriodSeconds (%d)` | The two resolved values leave no drain window. Nil pointers resolve to 5 and 30, so the rule holds when one or both are omitted |

uWSGI and the rollout strategy:

| Message | Trigger |
| --- | --- |
| `harakiri (%d) must be strictly less than terminationGracePeriodSeconds - preStopSleepSeconds (%d)` | The worst-case per-request kill would fall outside the drain window. The arguments are the harakiri value and the resolved window |
| `httpKeepAliveTimeout may only be set when httpKeepAlive is true` | A timeout set beside an explicit `httpKeepAlive: false`. A nil pointer counts as unset and resolves to `true`, so only the explicit `false` conflicts |
| `rollingUpdate must not be set when strategy.type is Recreate` | The Deployment controller would reject the object at apply time |

Autoscaling:

| Message | Trigger |
| --- | --- |
| `maxReplicas must be at least 1` | `spec.autoscaling.maxReplicas` below 1 |
| `minReplicas must be at least 1` | `spec.autoscaling.minReplicas` is set below 1 |
| `minReplicas must not exceed maxReplicas` | The two bounds cross |
| `maxReplicas must be >= spec.deployment.replicas (%d) when minReplicas is not set, because minReplicas defaults to spec.deployment.replicas` | An unset `minReplicas` would resolve above `maxReplicas` and produce an HPA the API server rejects |
| `targetCPUUtilization must be between 1 and 100` | The target is outside the range |
| `targetMemoryUtilization must be between 1 and 100` | The target is outside the range |
| `at least one of targetCPUUtilization or targetMemoryUtilization must be set` | An autoscaling block with no target |
| `%s request must be greater than zero while %s is set: the HorizontalPodAutoscaler divides the pods' usage by the sum of their containers' requests` | A zero or negative request in `spec.deployment.resources` for the resource a set target measures, reported at `requests.<cpu\|memory>`. The arguments are the resource and the target field |
| `%s limit must be greater than zero while %s is set: without a request the API server copies the limit into the request, and the HorizontalPodAutoscaler divides the pods' usage by the sum of their containers' requests` | `spec.deployment.resources` names no request for the resource a set target measures, and its limit is zero or negative, reported at `limits.<cpu\|memory>`. The arguments are the resource and the target field |

Network policy, gateway, resources and scheduling:

| Message | Trigger |
| --- | --- |
| `at least one ingress source must be specified` | `spec.networkPolicy.ingress` is empty. A policy with no source would allow every source |
| `hostname must be set when spec.gateway is configured` | `spec.gateway.hostname` is empty |
| `parentRef.name must be set when spec.gateway is configured` | `spec.gateway.parentRef.name` is empty |
| `%s request must not exceed limit (%s)` | A request in `spec.deployment.resources` above its own limit. The arguments are the resource name and the limit |
| `labelSelector is required on each TopologySpreadConstraint` | A constraint in `spec.deployment.topologySpreadConstraints` carries none |
| `labelSelector.matchLabels must equal the Deployment selector labels %v` | The selector does not equal the API Deployment's pod selector: the `neutron` name label, the instance label, and `app.kubernetes.io/component: api`. A selector without the component label also matches the worker and ovn-db-sync pods and is rejected |
| `matchExpressions are not allowed; labelSelector must use matchLabels only` | A constraint selects with expressions |
| `field.NotFound` on `spec.deployment.priorityClassName` | The named PriorityClass does not exist. The check is skipped when no lookup client is injected |
| `failed to look up PriorityClass: %w` | The lookup itself failed |
| `field.Invalid` on `spec.deployment.nodeSelector` | A key is not a qualified label name, or a value (reported at `nodeSelector[<key>]`) is not a valid label value |
| `field.Invalid` / `field.NotSupported` on `spec.deployment.tolerations[i]` | A toleration the API server would refuse: an invalid key, an empty key without `Exists`, `Exists` with a value, an `Lt` or `Gt` value that is not an integer, `tolerationSeconds` without `NoExecute`, or an unknown operator or effect |
| `%s request must not exceed limit (%s)` on `spec.jobs.resources` | A request in `spec.jobs.resources` above its own limit |
| `field.NotFound` on `spec.jobs.priorityClassName` | The named PriorityClass does not exist. `""` opts out of the fallback and is not looked up |
| `field.Invalid` on `spec.jobs.nodeSelector` / `spec.jobs.tolerations[i]` | The node selector and toleration rules above, applied to `spec.jobs` |

Secret store, target cluster and logging:

| Message | Trigger |
| --- | --- |
| `store name must be set` | `spec.secretStoreRef` is present with an empty `name` |
| `field.NotSupported` on `spec.secretStoreRef.kind` | A kind outside `ClusterSecretStore` and `SecretStore`. An empty kind is accepted and defaults to `ClusterSecretStore` |
| `target cluster name must be set` | `spec.targetClusterRef` is present with an empty `name` |
| `targetClusterRef is immutable (adding or removing it after creation is not permitted)` | An update adds or drops the ref. Both strand the children already created on the previously selected cluster |
| `targetClusterRef is immutable (the children already exist on the previously named cluster)` | An update renames the ref |
| `field.NotSupported` on `spec.logging.format` | A format outside `text` and `json` |
| `field.NotSupported` on `spec.logging.level` | A level outside `DEBUG`, `INFO`, `WARNING`, `ERROR`, `CRITICAL` |
| `logger name must not be empty` | A `perLoggerLevels` entry keyed on the empty string |
| `logger name must not contain a newline or carriage return: it is rendered verbatim into neutron.conf, so a newline injects arbitrary config lines` | A logger name with a control character. The name renders into the `[DEFAULT] default_log_levels` CSV |
| `level must be one of DEBUG, INFO, WARNING, ERROR, CRITICAL` | A `perLoggerLevels` value outside the set |

`spec.ovnDBSync`, whose cron grammar has no schema counterpart:

| Message | Trigger |
| --- | --- |
| `invalid cron expression: %v` | `spec.ovnDBSync.schedule` is non-empty and `cron.ParseStandard` refuses it. The argument is the parser's own error. An empty schedule is not parsed, since it resolves the operator default |
| `field.NotSupported` on `spec.ovnDBSync.syncMode`, listing `log` and `repair` | A mode outside the enum, re-checked here for an object that bypassed the schema |

`spec.extraConfig` is a preserve-unknown-fields map CEL cannot constrain, so
every guard on it is the webhook's alone:

| Message | Trigger |
| --- | --- |
| `extraConfig section name must not be empty` | A section keyed on the empty string, which would render a nameless `[]` header |
| `extraConfig section name must not contain a newline or carriage return` | A control character in a section name |
| `extraConfig key must not be empty` | An option keyed on the empty string, which would render a bare `= value` line |
| `extraConfig key and value must not contain a newline or carriage return: the rendered INI writes them verbatim, so a newline injects arbitrary config lines` | A control character in an option key or value |
| `%s is managed via %s and must not be set in extraConfig (%s)` | An override of a `Rejected` owned key. The arguments are the key, the field or computation that owns it, and its impact; the parenthesis is dropped for an entry with no impact text |
| `no such option in the neutron %s option catalog` | An option name the release catalog does not know. The argument is the catalog's release |
| `no such section in the neutron %s option catalog` | A section name the release catalog does not know |

The catalog is the flat union of `neutron.conf`, `ml2_conf.ini` and
`neutron_ovn_metadata_agent.ini`, with no per-file provenance, so a section
belonging to the metadata agent passes here and oslo.config ignores it at
runtime. Owned keys are exempt from the scan, so an operator-owned key never
doubles as an unknown option. The check re-runs on update only when
`spec.extraConfig` or `spec.openStackRelease` changed, so an unrelated edit such
as a replica change cannot retroactively reject a CR whose `extraConfig` a
regenerated catalog has since invalidated.

Four admission warnings come out of the same check:

```text
spec.extraConfig was not validated against an option catalog: spec.openStackRelease does not name an OpenStack release
spec.extraConfig was not validated against an option catalog: no catalog for release %q is embedded in this operator build
spec.extraConfig [%s] %s: deprecated option in neutron %s, replaced by %s
spec.extraConfig [%s] %s: deprecated option in neutron %s with no replacement
```

The first two are the fail-open paths: an unresolvable release skips the option
check with one warning rather than blocking admission. The last two name a
deprecated-but-accepted option and, where upstream declares one, its
replacement.

### Rejected owned keys

`config_ownership.go` records every key the operator computes and renders, one
registry per kind. An entry is honored-and-reported unless honoring the override
would already have done the damage by the time `ExtraConfigHealthy` could
surface it, which is the case for a credential the rendering copies into the
config Secret every pod mounts, a path or connection string that points a
process somewhere the operator did not provision, and a switch that selects a
security control. Those fifteen are refused at admission for the `Neutron`
kind:

| Key | Owned by | Why the override is refused |
| --- | --- | --- |
| `[DEFAULT] auth_strategy` | operator-computed | It names the WSGI pipeline `api-paste.ini` serves the API through, and `keystone` is the only one that runs keystonemiddleware. Anything else serves every request unauthenticated, reachable from outside the cluster while `spec.gateway` is set |
| `[DEFAULT] api_paste_config` | operator-computed | It names the pipeline definition. A path the pod does not carry fails the API on start; one it does carry can drop the auth filter |
| `[DEFAULT] transport_url` | `spec.messaging` | The runtime value arrives through `OS_DEFAULT__TRANSPORT_URL`, so a file value is inert and only copies the broker credentials into the rendered config Secret |
| `[database] connection` | `spec.database` | The runtime value arrives through `OS_DATABASE__CONNECTION`, so a file value is inert and only copies the database password into the rendered config Secret |
| `[keystone_authtoken] password` | `spec.serviceUser.secretRef` | The middleware reads the password from `OS_KEYSTONE_AUTHTOKEN__PASSWORD`, so a file value is inert and only copies the service password into the rendered config Secret |
| `[nova] password` | `spec.nova.serviceUser.secretRef` | The notifier reads the password from `OS_NOVA__PASSWORD`, so a file value is inert and only copies the notifier password into the rendered config Secret |
| `[securitygroup] enable_security_group` | operator-computed | It is what makes the ML2/OVN mechanism driver program the ACLs a port's security groups describe. Disabling it leaves every instance port reachable from every other |
| `[ovn] ovn_nb_connection` | `spec.ovn.centralRef` | The connection string is resolved from the referenced `OVNCentral`; another address points the mechanism driver at a logical model it does not own |
| `[ovn] ovn_sb_connection` | `spec.ovn.centralRef` | The Southbound half of the same rule |
| `[ovn] ovn_nb_private_key` | operator-computed | The operator mounts the client keypair the `OVNCentral` issuer signed; another path names a file the pod does not carry, and the connection falls back to no client identity |
| `[ovn] ovn_nb_certificate` | operator-computed | The certificate half of the same keypair |
| `[ovn] ovn_nb_ca_cert` | operator-computed | The CA bundle verifies the database endpoint; another path either fails the handshake or trusts a server the operator did not provision |
| `[ovn] ovn_sb_private_key` | operator-computed | The Southbound keypair, on the same terms as the Northbound one |
| `[ovn] ovn_sb_certificate` | operator-computed | The Southbound certificate |
| `[ovn] ovn_sb_ca_cert` | operator-computed | The Southbound CA bundle |

An update that carries a refused key over from the stored object with the same
value is admitted with a warning instead of refused. `[nova] password` joined the
list when the operator started owning the notifier account, and before that
`spec.extraConfig` was the documented way to configure the notifier, so refusing
it on every update, the finalizer removal on delete included, would leave such a
CR with no update to remove it through. A new or changed value is still refused.

Every other entry in the registry is honored and reported: the remaining
`[DEFAULT]` keys, including the two `notify_nova_on_port_*` switches
`spec.nova` owns; the `[keystone_authtoken]` keys `keystoneauth.Section` renders
and the eight `[nova]` keys it renders beside them, owned by `spec.nova`,
`spec.keystoneEndpoint` and the operator; the
`[oslo_messaging_notifications]`, `[oslo_messaging_rabbit]` and
`[oslo_concurrency]` keys; the `[ml2]` and per-type-driver keys; and the two
remaining `[ovn]` keys `ovn_l3_scheduler` and `ovn_metadata_enabled`. A
conditionally rendered key is registered unconditionally: the registry records
that a key is not the user's to set, not that it is currently rendered.

## Status

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | List-map keyed by `type`; see [Conditions](#conditions) |
| `observedGeneration` | `int64` | The `.metadata.generation` the controller last reconciled |
| `endpoint` | `string` | The Neutron API URL, `http://{name}.{namespace}.svc.cluster.local:9696`. It is the cluster-local Service URL whether or not `spec.gateway` is set, and it is stamped once the API Deployment is available |
| `installedRelease` | `string` | The OpenStack release whose schema is currently installed, promoted to `spec.openStackRelease` after a successful db-sync |
| `installedImage` | `string` | The `spec.image` reference that migrated the schema `installedRelease` names. A digest carries no parseable release, so without this field a `spec.openStackRelease` bump that left `spec.image` untouched would run no migration and still promote `installedRelease`; the release gate compares against it to refuse that transition |
| `targetRelease` | `string` | The `spec.openStackRelease` an active upgrade is converging to. Empty in steady state |
| `upgradePhase` | `commonv1.UpgradePhase` | The current expand-migrate-contract phase (`Expanding`, `Migrating`, `RollingUpdate`, `Contracting`); empty when no upgrade is in flight |

Neutron's alembic tree splits into an expand and a contract branch with no
data-migration command in between. The shared flow runs a Job for every phase,
so the `Migrating` Job runs `neutron-db-manage current`: it prints the revision
each branch sits at and exits 0, which keeps the phase readable in the Job log.

### Conditions

Fourteen sub-reconcilers report under ten condition types: four of them share
`SecretsReady` and two share `OVNEndpointsReady`. The aggregate `Ready` is
`True` only when all ten are. For the order they run in and what each one does,
see the [reconciler reference](./neutron-reconciler.md).

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `SecretsReady` | True | `SecretsAvailable` | The selected store is ready and both credential Secrets carry their keys |
| `SecretsReady` | False | `SecretStoreNotReady` | The External Secrets store this Neutron selected is not ready. Checked ahead of the individual Secrets, so an upstream outage surfaces even while per-ExternalSecret caches still report their last successful sync |
| `SecretsReady` | False | `WaitingForDBCredentials` | The database credentials Secret is missing or carries no `username`/`password`. Also set by the step that derives the connection Secret |
| `SecretsReady` | False | `WaitingForServiceUserCredentials` | The service-user Secret is missing or carries no password under the configured key |
| `SecretsReady` | False | `WaitingForMessagingCredentials` | The managed `RabbitmqCluster` has published no default-user Secret yet, or the brownfield Secret carries no transport URL |
| `SecretsReady` | False | `ConfigError` | Rendering or pruning the config ConfigMap failed. Config artefacts gate the same downstream graph as the credential Secrets, so they share that condition and there is no separate one for them |
| `SecretsReady` | False | `TargetClusterUnavailable` | `spec.targetClusterRef` names a cluster that does not resolve. Reported on the pipeline's first condition, and on the same condition while a deleting CR waits for its target cluster to come back |
| `OVNEndpointsReady` | True | `OVNEndpointsResolved` | Both database addresses are resolved and the message names them |
| `OVNEndpointsReady` | False | `OVNCentralNotFound` | The `OVNCentral` named by `spec.ovn.centralRef` does not exist. A Neutron applied before its central is an ordinary ordering of two objects in one manifest, so this polls |
| `OVNEndpointsReady` | False | `OVNCentralReadError` | Reading the central failed |
| `OVNEndpointsReady` | False | `OVNEndpointsPending` | The central publishes no Northbound or Southbound address yet, or no client Secret name. Across a cluster boundary the message names the two `externallyReachable` fields to set, since only the node ports are routable there |
| `OVNEndpointsReady` | False | `OVNClientSecretPending` | The central published a client Secret name before cert-manager wrote the Secret |
| `OVNEndpointsReady` | False | `OVNClientSecretIncomplete` | The source Secret carries no `tls.crt`, `tls.key` or `ca.crt` yet; the message names the missing key |
| `OVNEndpointsReady` | False | `OVNClientSecretReadError` | Reading the source Secret failed |
| `OVNEndpointsReady` | False | `OVNClientSecretMirrorFailed` | Writing the mirrored `{name}-ovn-client` Secret failed |
| `OVNEndpointsReady` | False | `TargetClusterUnavailable` | The central's own target cluster does not resolve, so its client Secret cannot be read |
| `DatabaseReady` | True | `DatabaseSynced` | The schema is at the head revision for the installed release |
| `DatabaseReady` | False | `ClusterNotReady` | The referenced MariaDB cluster is not ready |
| `DatabaseReady` | False | `WaitingForDatabase` | The MariaDB `Database`, `User` or `Grant` CR is not ready |
| `DatabaseReady` | False | `WaitingForConfig` | No rendered config exists yet, which means the OVN endpoints are still unresolved. The migration Jobs mount that ConfigMap as their whole `/etc/neutron`, so an empty name would render a volume the API server rejects |
| `DatabaseReady` | False | `DBSyncInProgress` | The db-sync Job is running |
| `DatabaseReady` | False | `DBSyncFailed` | The db-sync Job failed |
| `DatabaseReady` | False | `VersionParseError` | The installed or the requested release does not parse |
| `DatabaseReady` | False | `DowngradeNotSupported` | The requested release is older than the installed one |
| `DatabaseReady` | False | `UpgradePathInvalid` | The jump skips a release; upgrade one release at a time |
| `DatabaseReady` | False | `ImageReleaseMismatch` | A tag-pinned `spec.image` names a different release than `spec.openStackRelease`, or a release bump left `spec.image` untouched so no migration would run. The message names both readings of the second case, because the recovery differs |
| `DatabaseReady` | False | `UpgradeTargetChanged` | `spec.openStackRelease` changed while an upgrade was in flight |
| `DatabaseReady` | False | `ExpandInProgress`, `MigrateInProgress`, `ContractInProgress` | The phase Job of that name is running |
| `DatabaseReady` | False | `ExpandFailed`, `MigrateFailed`, `ContractFailed` | The phase Job of that name failed |
| `DatabaseReady` | False | `UpgradeRollingUpdate` | The upgrade waits for the Deployment to roll onto the target-release image before contracting |
| `OVNDBSyncReady` | True | `OVNDBSyncNotRequired` | `spec.ovnDBSync` is unset, so no CronJob exists. The absence is the configured state |
| `OVNDBSyncReady` | True | `OVNDBSyncScheduled` | The CronJob is in place and the most recent terminal run did not fail. The message names the resolved schedule and mode |
| `OVNDBSyncReady` | True | `OVNDBSyncSuspended` | `spec.ovnDBSync.suspend` is set. It outranks a failed run, because a suspended CronJob spawns no successor to supersede it; the failure is still named in the message |
| `OVNDBSyncReady` | False | `OVNDBSyncJobFailed` | The newest terminal run failed, so the two databases are no longer being compared |
| `DeploymentReady` | True | `DeploymentReady` | The API Deployment is available |
| `DeploymentReady` | False | `WaitingForDeployment` | The Deployment is rolling out, or the upgrade's rolling-update phase waits for every replica to be updated, ready and counted before the contract Jobs drop what the old pods still read |
| `WorkersReady` | True | `WorkersReady` | Both worker Deployments are available |
| `WorkersReady` | False | `WaitingForWorkers` | At least one of them is not |
| `NeutronAPIReady` | True | `APIHealthy` | An HTTP GET against the cluster-local API root answered 2xx. `/` is the version document, which the API pods answer without a token and without touching the database |
| `NeutronAPIReady` | False | `APIUnhealthy` | The probe answered a non-2xx status |
| `NeutronAPIReady` | False | `EndpointNotReady` | `status.endpoint` is not stamped yet, or the endpoint does not resolve |
| `NeutronAPIReady` | False | `HealthCheckTimeout` | The probe exceeded its deadline |
| `NeutronAPIReady` | False | `ConnectionFailed` | The connection was refused |
| `NeutronAPIReady` | False | `HealthCheckFailed` | The probe failed for another reason; the message carries it |
| `HTTPRouteReady` | True | `HTTPRouteAccepted` | The Gateway controller reports `Accepted=True` on the route's parent |
| `HTTPRouteReady` | True | `HTTPRouteNotRequired` | `spec.gateway` is unset, so any previous route was deleted |
| `HTTPRouteReady` | False | `HTTPRouteNotAccepted` | The parent Gateway has not accepted the route |
| `HTTPRouteReady` | False | `GatewayAPINotInstalled` | The cluster the children land on does not serve the HTTPRoute kind while `spec.gateway` is set |
| `HTTPRouteReady` | False | `CapabilityProbeFailed` | The probe for that kind failed against the target cluster's RESTMapper |
| `HPAReady` | True | `HPAReady` | The HorizontalPodAutoscaler matches the desired state |
| `HPAReady` | True | `HPANotRequired` | `spec.autoscaling` is unset, so any previous HPA was deleted |
| `NetworkPolicyReady` | True | `NetworkPolicyReady` | The NetworkPolicy matches the desired state |
| `NetworkPolicyReady` | True | `NetworkPolicyNotRequired` | `spec.networkPolicy` is unset, so any previous policy was deleted and traffic flows unrestricted |
| `Ready` | True | `AllReady` | All ten sub-conditions are True |
| `Ready` | False | `NotAllReady` | At least one is not |

`ExtraConfigHealthy` is set beside these on every pass that renders a config,
reporting the honored overrides of operator-owned keys. It is informational and
stays out of the `Ready` aggregation.

`NeutronAPIReady`, `HTTPRouteReady`, `HPAReady` and `NetworkPolicyReady` are the
members of the post-deployment parallel group. Each of them always resolves,
configured-ready or not-required, so a cluster without a gateway, an HPA or a
network policy still reaches `Ready`.

## Sub-Resource Naming Convention

The API Deployment, its Service, the PodDisruptionBudget, the
HorizontalPodAutoscaler, the NetworkPolicy, the HTTPRoute and the MariaDB
`Database`/`User`/`Grant` take the bare CR name with no suffix, matching the
keystone convention. A `Neutron` named `neutron` in the `openstack` namespace is
therefore reachable in-cluster at `neutron.openstack.svc.cluster.local:9696`,
since the Service DNS name is the CR name and 9696 is the port the
uWSGI-served API listens on.

The derived and content-addressed children carry a suffix:

| Resource | Name | Notes |
| --- | --- | --- |
| Config ConfigMap | `{name}-config-<hash>` | Immutable, content-hashed `neutron.conf` and `ml2_conf.ini`, plus `uwsgi.ini` and, for json logging, `logging.conf`; 3 historical retained |
| DB-connection Secret | `{name}-db-connection` | The derived pymysql DSN, stable name, consumed through `OS_DATABASE__CONNECTION` |
| Transport-URL Secret | `{name}-transport-url` | The derived `rabbit://` URL, consumed through `OS_DEFAULT__TRANSPORT_URL` |
| OVN client Secret | `{name}-ovn-client` | The mirror of the client identity the `OVNCentral` publishes, carrying `tls.crt`, `tls.key` and `ca.crt` and nothing else. The source lives in the central's namespace, on a placed Neutron even on another cluster, so the pods mount the copy |
| DB-sync Job | `{name}-db-sync` | `neutron-db-manage upgrade head`. There is no schema-check Job: the alembic upgrade is idempotent, so a second read-only run would assert nothing |
| Expand Job | `{name}-db-expand` | `neutron-db-manage upgrade --expand`, release upgrades only |
| Migrate Job | `{name}-db-migrate` | `neutron-db-manage current`, release upgrades only |
| Contract Job | `{name}-db-contract` | `neutron-db-manage upgrade --contract`, release upgrades only |
| OVN sync CronJob | `{name}-ovn-db-sync` | `neutron-ovn-db-sync-util`, only while `spec.ovnDBSync` is set. The suffix is what bounds `metadata.name` to 40 characters |
| Worker Deployment | `{name}-periodic-workers` | `neutron-periodic-workers`, the recurring maintenance tasks of the ML2 plugin |
| Worker Deployment | `{name}-ovn-maintenance-worker` | `neutron-ovn-maintenance-worker`, which reconciles the Northbound model against the Neutron database |

One `Neutron` owns three Deployments, so each of them selects on its own
component label. A selector without the component key would have the three count
and adopt each other's pods, and the Service would route API traffic to a worker
that serves no HTTP. The PodDisruptionBudget selects the API component and
excludes Job pods, which carry no readiness probe and would otherwise be Ready
from their first moment and raise `disruptionsAllowed`.

## ovnDBSync

A nil `spec.ovnDBSync` means no CronJob. Any CronJob a previous spec created is
deleted and `OVNDBSyncReady` reports `OVNDBSyncNotRequired`: the sync reads, and
in repair mode rewrites, the entire logical model, so scheduling it is a choice
the CR has to make.

With the block present, `schedule` and `syncMode` are resolved at reconcile time
when empty, to `0 * * * *` and `log`. Resolving them at reconcile time keeps an
unset field tracking the operator default across upgrades; a defaulting webhook
would freeze today's value into the stored CR.

The two modes differ in what they leave behind. `neutron-ovn-db-sync-util
--ovn-neutron_sync_mode log` reports the drift and exits 0 whether or not the
two databases agree, so a run that found the Northbound out of step is
indistinguishable from a clean one here: the condition reports Job outcomes, and
the report lives in the run's log. `repair` deletes every Northbound object the
Neutron database cannot account for and creates the ones it is missing, so
switch to it for a run you intend and switch back afterwards, or use `suspend`
for the window in which someone is editing the Northbound database by hand.

Each firing gets one Job with `concurrencyPolicy: Forbid`, no retries, and a
one-hour active deadline. Two runs would walk the same two databases and, in
repair mode, write the second one, so the later run would undo what the earlier
is halfway through. A retry would walk the same unreachable Northbound and
produce the same failure minutes later; the next firing is the retry. The
deadline is what keeps a wedged run observable, since a pod stuck in
`ImagePullBackOff` or a utility blocked on an unresponsive Northbound reaches no
terminal state on its own.

When a run fails, the condition message and the Warning event (see
[Controller events](./neutron-events.md)) both name the line to look for in the
pod log:

```text
Could not retrieve schema from <address>
```

It means the Northbound at that address was unreachable, and not that the two
databases disagree.

For the procedure around a repair-mode run, see
[Repair OVN drift with db-sync](../../guides/neutron/repair-ovn-drift-with-db-sync.md).

## Example

```yaml
apiVersion: neutron.openstack.c5c3.io/v1alpha1
kind: Neutron
metadata:
  name: neutron-basic
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  deployment:
    replicas: 2
  workers:
    deployment:
      replicas: 1
  image:
    repository: ghcr.io/c5c3/neutron
    tag: "2025.2"
  database:
    clusterRef:
      name: openstack-db
    database: neutron_basic
    secretRef:
      name: neutron-db
  cache:
    clusterRef:
      name: openstack-memcached
  messaging:
    secretRef:
      name: neutron-basic-messaging
  ovn:
    centralRef:
      name: neutron-basic-ovn
  keystoneEndpoint: http://keystone.openstack.svc.cluster.local:5000/v3
  serviceUser:
    username: admin
    projectName: admin
    userDomainName: Default
    projectDomainName: Default
    secretRef:
      name: keystone-admin
      key: password
```

This is the fixture the `basic-deployment` suite applies
(`tests/e2e/neutron/basic-deployment/02-neutron-cr.yaml`). Its
`keystoneEndpoint` is the cluster-local convention URL every colocated service
uses, and the leg deploys no Keystone: nothing resolves that address there, so
`spec.serviceUser` names the pre-seeded `keystone-admin` Secret and the suite
asserts on the operator's own conditions, with no authenticated API call in it. The API runs two replicas because the PodDisruptionBudget builder renders
`minAvailable: 1` only above one replica, which is what makes the suite's PDB
assertion meaningful; both workers stay at one, since neither serves a request.
