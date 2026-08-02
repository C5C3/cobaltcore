---
title: Placement CRD
quadrant: operator
---

# Placement CRD

`placements.placement.openstack.c5c3.io/v1alpha1`, kind `Placement`. The CRD is
generated from `operators/placement/api/v1alpha1/placement_types.go`; the
validating and defaulting webhooks live in
`operators/placement/api/v1alpha1/placement_webhook.go`. The Helm chart ships a
synced copy (`make sync-crds` / `make verify-crd-sync`).

One Placement CR describes the Placement API server: its OpenStack release,
container image, database and cache connections, and the Keystone integration.
Placement tracks resource-provider inventories and allocations for the compute
services and owns no backing store of its own, so the spec stays close to the
plain API-server shape.

## Spec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `openStackRelease` | `string` | yes | The OpenStack release the operator deploys and drives; pattern `^\d{4}\.[12]$` (the `YYYY.N` cadence, `N` ∈ {1,2}). It governs install and upgrade schema tracking: `status.installedRelease` is promoted to this value after a successful db-sync. Kept separate from the image tag so digest-pinned images still resolve a schema. It selects no launch mode: placement runs the same uWSGI command and renders the same option names on every supported release |
| `deployment` | `DeploymentSpec` | no | Shared pod-level knobs: `replicas` (default 3), `resources` (defaults: 512Mi request / 1Gi limit memory, 100m/500m CPU), `terminationGracePeriodSeconds`, `preStopSleepSeconds`, `strategy`, `topologySpreadConstraints`, `priorityClassName` |
| `image` | `ImageSpec` | yes | Container image. `tag` and `digest` are mutually exclusive and one of the two is required (shared CEL rule, re-checked by the webhook) |
| `database` | `DatabaseSpec` | yes | MariaDB connection, rendered into `[placement_database]`. One of `clusterRef` (managed) or `host` (brownfield), never both; plus `database`, `secretRef`, and the optional `port`, `credentialsMode`, and `tls`. `credentialsMode` selects how the credential in `secretRef` is provisioned: `Static` (the default) keeps a long-lived password and has the operator manage the MariaDB `User`/`Grant` CRs, `Dynamic` takes short-lived engine-issued credentials and manages neither. `Dynamic` requires `clusterRef`. The mutual-exclusivity and Dynamic-requires-clusterRef rules are inherited from `commonv1.DatabaseSpec`, and so are `replicas` (default 3, minimum 1) and `storageSize` (default `100Gi`): both sit in the schema, but only the c5c3 operator's managed-mode projection reads them, so the Placement operator ignores whatever they say |
| `cache` | `CacheSpec` | yes | Memcached backing the keystonemiddleware token cache, rendered as `[keystone_authtoken] memcached_servers`. One of `clusterRef` (managed) or `servers` (brownfield), never both. `backend` is webhook-defaulted to `dogpile.cache.pymemcache`. `replicas` (default 3) is inherited from `commonv1.CacheSpec` on the same terms as the database counterparts: schema-visible, honoured by the c5c3 operator's managed-mode projection, ignored here (`cache.ResolveServers` addresses the cluster by its `clusterRef` name alone) |
| `keystoneEndpoint` | `string` | yes | The Keystone auth URL rendered as `[keystone_authtoken] auth_url`; must match `^https?://` and parse with a host. keystonemiddleware validates a token against it server-side on every authenticated request, so it must be reachable from inside the cluster: use the cluster-local Service URL, not an externally routable address |
| `keystonePublicEndpoint` | `string` | no | The browser-facing Keystone base URL rendered as `[keystone_authtoken] www_authenticate_uri`, the address a 401 points unauthenticated clients at; must match `^https?://` when set. When empty the operator falls back to `keystoneEndpoint` at render time (see `EffectiveKeystonePublicEndpoint`), which holds only when the internal and public Keystone URLs coincide |
| `serviceUser` | [`ServiceUserSpec`](#serviceuserspec) | yes | The Keystone service account Placement authenticates as, and the Secret holding its password |
| `region` | `string` | no | The Keystone region (`[keystone_authtoken] region_name`); when empty the option is omitted and Placement uses the catalog's default region |
| `apiServer` | [`*APIServerSpec`](#apiserverspec) | no | uWSGI process tuning. When nil the reconciler applies the same defaults the webhook writes into a present block |
| `gateway` | `*GatewaySpec` | no | External exposure via a Gateway API HTTPRoute forwarding to the `{name}` Service on port 8778; requires `hostname` and `parentRef.name`, and takes an optional `path` (default `/`) and an optional `annotations` map, copied verbatim onto the generated HTTPRoute's metadata for implementation-specific settings such as rate limits or CORS (the route timeout stays operator-managed). Removing the block deletes the HTTPRoute |
| `networkPolicy` | `*NetworkPolicySpec` | no | Ingress restricted to TCP 8778 from the listed sources; egress auto-derived (DNS, the database, the Keystone endpoint's port, and the cache), with `additionalEgress` appended after it. At least one ingress source is required (fail-closed) |
| `autoscaling` | `*AutoscalingSpec` | no | HPA bounds and CPU/memory utilization targets. At least one of `targetCPUUtilization` or `targetMemoryUtilization` is required, and an unset `minReplicas` resolves to `spec.deployment.replicas` |
| `logging` | `*LoggingSpec` | no | oslo.log derivation: `format` (`text`/`json`), `level`, `debug`, `perLoggerLevels`. Materialized by the defaulting webhook to `text`/`INFO`/`debug: false`. The `json` format ships a `logging.conf` in the config ConfigMap and points `[DEFAULT] log_config_append` at it |
| `secretStoreRef` | `*SecretStoreRefSpec` | no | Selects the External Secrets store the operator resolves `SecretsReady` against: `kind` (`ClusterSecretStore` \| `SecretStore`, default `ClusterSecretStore`) and a required `name`. When omitted the shared cluster-scoped `openbao-cluster-store` is used. A namespaced store is resolved in the Placement's own namespace. Normally projected from the owning [ControlPlane](../c5c3/controlplane-crd.md) |
| `policyOverrides` | `*PolicySpec` | no | Custom oslo.policy rules. A CEL rule requires at least one of `rules` or `configMapRef`; when set, the operator renders a `policy.yaml` into the config ConfigMap and wires `[oslo_policy] policy_file` at it. Inline `rules` win over `configMapRef` entries of the same name |
| `extraConfig` | `map[string]map[string]string` | no | Free-form INI sections for configuration not covered by explicit fields, the escape hatch for options with no dedicated knob. The render-time merge is `operator defaults < extraConfig`, so user values win. Overrides of operator-owned keys are honored but surfaced through the `ExtraConfigHealthy` condition and an `ExtraConfigOwnedKeyOverride` Warning event, with three exceptions the validating webhook refuses outright (see [Defaulting and validation](#defaulting-and-validation)). Option names are checked at admission against a per-release option catalog embedded in the operator, selected by `spec.openStackRelease`; a release with no embedded catalog skips the check with an admission warning. Placement exempts no section whole, so an unknown section or option is rejected with the section and key named. A deprecated-but-accepted option is admitted with a warning naming its replacement. Values are checked for one thing: a newline or carriage return in a section name, key, or value is rejected, because the rendered INI writes each verbatim and a newline would inject config lines past the ownership and catalog gates. Every rule here is webhook-only with no CEL backstop |

### ServiceUserSpec

The name and domain fields are webhook-defaulted, so a minimal CR need only
supply the password Secret reference.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `username` | `string` | no | `placement` | Keystone username (`[keystone_authtoken] username`) |
| `projectName` | `string` | no | `service` | The project the service user scopes to (`project_name`) |
| `userDomainName` | `string` | no | `Default` | The domain the service user lives in (`user_domain_name`) |
| `projectDomainName` | `string` | no | `Default` | The domain the service project lives in (`project_domain_name`) |
| `secretRef` | `SecretRefSpec` | yes | `key` → `password` | The Secret holding the service-user password; the password is injected as the `OS_KEYSTONE_AUTHTOKEN__PASSWORD` env var and never rendered into config |

Each of the four identity fields is rendered verbatim into
`[keystone_authtoken]`, so the validating webhook rejects a newline or carriage
return in any of them. `spec.region` lands in the same section through the same
renderer and goes through the same check.

### APIServerSpec

Tunes the API server process. It carries the uWSGI block alone: placement has
only ever shipped a WSGI application and no eventlet server, so there is no
launch mode to select and no worker count to configure outside uWSGI. The
sibling Glance operator's `workers` field has no counterpart here.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `uwsgi` | [`*UWSGISpec`](#uwsgispec) | no | uWSGI application-server parameters |

### UWSGISpec

A cross-field CEL rule mirrors the webhook: `httpKeepAliveTimeout` may only be
set when `httpKeepAlive` is true, since the `--http-keepalive-timeout` flag is
otherwise never emitted. A nil `httpKeepAlive` counts as unset and resolves to
the default `true`, so only an explicit `false` conflicts with a set timeout.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `processes` | `int32` (Minimum=1) | no | `2` | uWSGI worker-process count (`--processes`) |
| `threads` | `int32` (Minimum=1) | no | `1` | Threads per worker (`--threads`) |
| `httpKeepAlive` | `*bool` | no | `true` | Enables `--http-keepalive`. A nil-preserving pointer: nil restores the default, an explicit `false` is honored verbatim and drops the flag |
| `harakiri` | `*int32` (Minimum=1) | no | — | Per-request worker lifetime cap (`--harakiri`, seconds). Omitted from the command when nil, with no hidden default. The webhook requires `harakiri < terminationGracePeriodSeconds − preStopSleepSeconds`, so the worst-case per-request kill fits inside the drain window |
| `httpKeepAliveTimeout` | `*int32` (Minimum=1) | no | — | Idle timeout of keep-alive connections (`--http-keepalive-timeout`, seconds). Omitted when nil; zero is rejected to avoid the unbounded interpretation |

The three hardcoded defaults live in `placement_webhook.go`
(`DefaultUWSGIProcesses`, `DefaultUWSGIThreads`, `DefaultUWSGIHTTPKeepAlive`) and
are what the reconciler falls back to when `spec.apiServer` or
`spec.apiServer.uwsgi` is nil, so a CR that bypassed the defaulting webhook
launches with the documented command.

### Defaulting and validation

The mutating webhook fills `spec.deployment.resources` with 512Mi memory request
/ 1Gi memory limit (CPU keeps the shared 100m/500m) before the shared
`DeploymentSpec` defaults run, because the API container forks
`DefaultUWSGIProcesses` workers and each one carries its own interpreter,
SQLAlchemy session pool, and oslo stack. It then applies the shared
`DeploymentSpec` defaults, materializes the `dogpile.cache.pymemcache` cache
backend and the `LoggingSpec` baseline, fills the `ServiceUserSpec` identity
defaults (`placement` / `service` / `Default` / `Default`, `secretRef.key` →
`password`), and, only when `spec.apiServer.uwsgi` is present, the uWSGI
sub-field defaults (`processes` 2, `threads` 1, `httpKeepAlive` true).

There is no `metadata.name` length bound. Glance carries one because Kubernetes
caps a CronJob name at 52 characters, its controller appending an 11-character
timestamp suffix to every Job it spawns, and the `-db-purge` suffix spends that
budget down to 43 for the CR name. Placement projects no CronJob, so every child
object name fits inside the 63 characters Kubernetes already allows a CR name,
and any admissible name stays admissible.

The validating webhook accumulates every violation into one admission response,
reusing the shared validators: replicas floor, image tag/digest exclusivity, the
database mutual-exclusivity and Dynamic-requires-clusterRef rules, cache
mutual-exclusivity and its control-character guard, secret-store-ref shape, both
Keystone endpoint URL shapes, logging enums (including the per-logger-level map,
which has no schema-layer counterpart), the graceful-termination arithmetic
(`preStopSleepSeconds < terminationGracePeriodSeconds`, and `harakiri` strictly
inside the drain window), the `httpKeepAliveTimeout` pairing, the
`Recreate`-vs-`rollingUpdate` sanity check, autoscaling bounds (including the
implicit `minReplicas` default from `deployment.replicas`), network-policy
ingress, gateway hostname and `parentRef.name`, resource requests-vs-limits,
PriorityClass existence, and topology-spread selectors (matching the `placement`
and instance labels).

One check is placement's own. `spec.region` and the four `serviceUser` identity
fields are each rejected for a newline or carriage return, because the renderer
writes them into `[keystone_authtoken]` verbatim and a newline would smuggle a
whole extra option past the ownership and catalog gates. The two Keystone
endpoint fields need no entry: `url.Parse` already refuses control bytes for
them.

`spec.extraConfig` is a preserve-unknown-fields map CEL cannot constrain, so its
guards are webhook-only: an empty section name or option key, a newline or
carriage return in any section name, key, or value, and an override of a
`Rejected` owned key. The rejection list is driven by the `Rejected` flag in
`operators/placement/api/v1alpha1/config_ownership.go` and holds three entries:

| Key | Owned by | Why the override is refused |
| --- | --- | --- |
| `[api] auth_strategy` | operator-computed | `extraConfig` has the last word in the merge, so `noauth2` would put the API on the no-auth middleware: every request unauthenticated, with project and role taken from the `x-auth-token` header, and reachable from outside the cluster while `spec.gateway` is set |
| `[DEFAULT] auth_strategy` | operator-computed | Placement's deprecated alias for the same option, still honored by oslo.config. Both guards key on the `(section, key)` pair, so registering the `[api]` entry alone would leave this spelling open. The operator never renders it |
| `[keystone_authtoken] password` | `spec.serviceUser.secretRef` | The middleware reads the password from `OS_KEYSTONE_AUTHTOKEN__PASSWORD`, so a file value is inert at runtime and only leaks the service password into a namespace-readable ConfigMap |

Every other owned key is honored and reported. The full registry covers
`[DEFAULT]` (`use_stderr`, `debug`, `default_log_levels`, `log_config_append`),
`[placement_database]` (`connection`, `max_retries`,
`connection_recycle_time`), the `[keystone_authtoken]` keys
`keystoneauth.Section` renders, and `[oslo_policy] policy_file`. Conditionally
rendered keys are registered unconditionally: the registry records that a key is
not the user's to set, not that it is currently rendered.

The catalog check re-runs on update only when one of its inputs changed
(`spec.extraConfig` or `spec.openStackRelease`), so an unrelated edit such as a
replica change cannot retroactively reject a CR whose `extraConfig` a
regenerated catalog has since invalidated.

## Status

| Field | Description |
| --- | --- |
| `conditions` | List-map keyed by `type`; see the [reconciler reference](./placement-reconciler.md#conditions) for the vocabulary |
| `observedGeneration` | The `.metadata.generation` last reconciled |
| `endpoint` | The Placement API URL: `https://{gateway.hostname}/` when a gateway is set, otherwise the cluster-local Service URL |
| `installedRelease` | The OpenStack release whose schema is currently installed, promoted to `spec.openStackRelease` after a successful db-sync |
| `installedImage` | The `spec.image` reference that migrated the schema `installedRelease` names. A digest carries no parseable release, so without this field a `spec.openStackRelease` bump that left `spec.image` untouched would run no migration and still promote `installedRelease`; the release gate compares against it to refuse that transition |
| `targetRelease` | The `spec.openStackRelease` a release bump is converging to. Stamped when the gate accepts the bump, cleared once `installedRelease` equals the spec release; empty in steady state |

The status carries no `upgradePhase`. Placement runs no expand-migrate-contract
phase machine: its migrations apply in one `placement-manage db sync` pass, and
the release rules the phased flow enforces on its way in (no downgrades, no
multi-release jumps) are checked by the release gate before any Job runs. See
[Database](./placement-reconciler.md#database).

## Sub-Resource Naming Convention

Operator-managed sub-resources (Deployment, Service, PodDisruptionBudget,
HorizontalPodAutoscaler, NetworkPolicy, HTTPRoute, and the MariaDB
`Database`/`User`/`Grant`) use the bare CR name with no suffix, matching the
keystone convention. A Placement CR named `placement` in the `openstack`
namespace is therefore reachable in-cluster at
`placement.openstack.svc.cluster.local:8778`, since the Service DNS name is the
CR name.

The content-addressed and derived resources are the exceptions:

| Resource | Name | Notes |
| --- | --- | --- |
| Config ConfigMap | `{name}-config-<hash>` | Immutable, content-hashed `placement.conf` (plus `policy.yaml` / `logging.conf` when applicable); 3 historical retained |
| DB-connection Secret | `{name}-db-connection` | Derived pymysql DSN, stable name |
| DB-sync Job | `{name}-db-sync` | `placement-manage db sync`, `db online_data_migrations`, and `placement-status upgrade check` |

Three rows is the whole list. There is no backends Secret, because placement
attaches no stores, and no purge CronJob, because `placement-manage` has no
purge command and the schema keeps no soft-deleted rows to reclaim.

## Example

```yaml
apiVersion: placement.openstack.c5c3.io/v1alpha1
kind: Placement
metadata:
  name: placement
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  deployment:
    replicas: 3
  image:
    repository: ghcr.io/c5c3/placement
    tag: "2025.2"
  database:
    clusterRef:
      name: openstack-mariadb
    database: placement
    secretRef:
      name: placement-db
  cache:
    clusterRef:
      name: openstack-memcached
  keystoneEndpoint: http://keystone.openstack.svc.cluster.local:5000/v3
  serviceUser:
    secretRef:
      name: placement-keystone
```
