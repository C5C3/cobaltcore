---
title: Barbican CRD
quadrant: operator
---

# Barbican CRD

`barbicans.barbican.openstack.c5c3.io/v1alpha1`, kind `Barbican`. The CRD is
generated from `operators/barbican/api/v1alpha1/barbican_types.go`; the
validating and defaulting webhooks live in
`operators/barbican/api/v1alpha1/barbican_webhook.go`. The Helm chart ships a
synced copy (`make sync-crds` / `make verify-crd-sync`).

One Barbican CR describes the Key Manager API server: its OpenStack release,
container image, database and cache connections, the Keystone integration, and
the recurring database clean-up. The secret stores that hold the material
barbican protects are separate CRs
([`BarbicanSecretStore`](./barbican-secret-store-crd.md)), so the spec below
stays close to the plain API-server shape.

`kubectl get barbicans` prints Ready
(`.status.conditions[?(@.type=='Ready')].status`), Release
(`.status.installedRelease`), Endpoint (`.status.endpoint`), and Age.

## Spec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `openStackRelease` | `string` | yes | The OpenStack release the operator deploys and drives; pattern `^\d{4}\.[12]$` (the `YYYY.N` cadence, `N` ∈ {1,2}). It governs install and upgrade schema tracking: `status.installedRelease` is promoted to this value after a successful db-sync. Kept separate from the image tag so digest-pinned images still resolve a schema. It also selects the `barbican-api-paste.ini` layout: from `2026.1` the rendered file carries the oslo `request_id` filter in every pipeline and drops the `repoze.profile` pipeline and filter |
| `deployment` | `DeploymentSpec` | no | Shared pod-level knobs: `replicas` (default 3), `resources` (defaults: 256Mi request / 512Mi limit memory, 100m/500m CPU), `terminationGracePeriodSeconds`, `preStopSleepSeconds`, `strategy`, `topologySpreadConstraints`, `priorityClassName` |
| `image` | `ImageSpec` | yes | Container image. `tag` and `digest` are mutually exclusive and one of the two is required (shared CEL rule, re-checked by the webhook). The field carries no immutability rule |
| `database` | `DatabaseSpec` | yes | MariaDB connection, rendered into `[database]`. One of `clusterRef` (managed) or `host` (brownfield), never both; plus `database`, `secretRef`, and the optional `port`, `credentialsMode`, and `tls`. `credentialsMode` selects how the credential in `secretRef` is provisioned: `Static` (the default) keeps a long-lived password and has the operator manage the MariaDB `User`/`Grant` CRs, `Dynamic` takes short-lived engine-issued credentials and manages neither. `Dynamic` requires `clusterRef`. The mutual-exclusivity and Dynamic-requires-clusterRef rules are inherited from `commonv1.DatabaseSpec`, and so are `replicas` and `storageSize`: both sit in the schema, but only the c5c3 operator's managed-mode projection reads them, so the Barbican operator ignores whatever they say. With `tls` enabled the client keypair is projected into the API pods, the db-sync Job, and the clean-up CronJob alike |
| `cache` | `CacheSpec` | yes | Memcached backing the keystonemiddleware token cache, rendered as `[keystone_authtoken] memcached_servers`. One of `clusterRef` (managed) or `servers` (brownfield), never both. `backend` is webhook-defaulted to `dogpile.cache.pymemcache`. `replicas` is inherited from `commonv1.CacheSpec` on the same terms as the database counterparts: schema-visible, honoured by the c5c3 operator's managed-mode projection, ignored here |
| `keystoneEndpoint` | `string` | yes | The Keystone auth URL rendered as `[keystone_authtoken] auth_url`; must match `^https?://` and parse with a host. keystonemiddleware validates a token against it server-side on every authenticated request, so it must be reachable from inside the cluster: use the cluster-local Service URL, not an externally routable address |
| `keystonePublicEndpoint` | `string` | no | The browser-facing Keystone base URL rendered as `[keystone_authtoken] www_authenticate_uri`, the address a 401 points unauthenticated clients at; must match `^https?://` when set. When empty the operator falls back to `keystoneEndpoint` at render time (see `EffectiveKeystonePublicEndpoint`), which holds only when the internal and public Keystone URLs coincide |
| `serviceUser` | [`ServiceUserSpec`](#serviceuserspec) | yes | The Keystone service account Barbican authenticates as, and the Secret holding its password |
| `region` | `string` | no | The Keystone region (`[keystone_authtoken] region_name`); when empty the option is omitted and Barbican uses the catalog's default region |
| `apiServer` | [`*APIServerSpec`](#apiserverspec) | no | uWSGI process tuning. When nil the reconciler applies the same defaults the webhook writes into a present block |
| `dbClean` | [`*DBCleanSpec`](#dbcleanspec) | no | Tunes the recurring `barbican-manage db clean` CronJob. A nil block resolves like an empty one: the clean-up is scheduled either way, and the operator resolves the knobs at reconcile time, so the defaulting webhook leaves the block untouched |
| `gateway` | `*GatewaySpec` | no | External exposure via a Gateway API HTTPRoute forwarding to the `{name}` Service on port 9311; requires `hostname` and `parentRef.name`, and takes an optional `path` (default `/`) and an optional `annotations` map, copied verbatim onto the generated HTTPRoute's metadata for implementation-specific settings such as rate limits or CORS. Setting it also changes what barbican advertises: `[DEFAULT] host_href` and `status.endpoint` become `https://{hostname}`. Removing the block deletes the HTTPRoute |
| `networkPolicy` | `*NetworkPolicySpec` | no | Ingress restricted to TCP 9311 from the listed sources; egress auto-derived (DNS, the database, the Keystone endpoint's port, the cache, and the OpenBao servers of the projected stores), with `additionalEgress` appended after it. At least one ingress source is required (fail-closed) |
| `autoscaling` | `*AutoscalingSpec` | no | HPA bounds and CPU/memory utilization targets. At least one of `targetCPUUtilization` or `targetMemoryUtilization` is required, and an unset `minReplicas` resolves to `spec.deployment.replicas`. The bound also sizes the database user's `max_user_connections`, which the operator derives from `maxReplicas` when an HPA owns the replica count |
| `logging` | `*LoggingSpec` | no | oslo.log settings: `format` (`text`/`json`), `level`, `debug`, `perLoggerLevels`. The defaulting webhook materializes the block (`text`/`INFO`/`debug: false`) and the validating webhook checks the enums and the per-logger map, but the config renderer does not read the field yet, so no `[DEFAULT]` logging key is derived from it. Set the logging options through `spec.extraConfig` until it is wired |
| `secretStoreRef` | `*SecretStoreRefSpec` | no | Selects the External Secrets store the operator resolves `SecretsReady` against: `kind` (`ClusterSecretStore` \| `SecretStore`, default `ClusterSecretStore`) and a required `name`. When omitted the shared cluster-scoped `openbao-cluster-store` is used. A namespaced store is resolved in the Barbican's own namespace. Normally projected from the owning [ControlPlane](../c5c3/controlplane-crd.md). It selects the ESO store the credential Secrets arrive through and has nothing to do with the secret stores barbican itself writes to |
| `policyOverrides` | `*PolicySpec` | no | Custom oslo.policy rules. A CEL rule requires at least one of `rules` or `configMapRef`; when set, the operator renders a `policy.yaml` into the config Secret and wires `[oslo_policy] policy_file` at it. Inline `rules` win over `configMapRef` entries of the same name |
| `middleware` | `[]MiddlewareSpec` | no | WSGI filters inserted into the `barbican-api-keystone` pipeline, the one `composite:main` routes `/v1` to. Each entry carries `name`, `filterFactory`, `position` (`before` or `after` the base filters), and a `config` map rendered as the filter's own paste section. The other pipelines the image ships are rendered verbatim and are unreachable through `composite:main` |
| `plugins` | `[]PluginSpec` | no | Service plugins or drivers, each with a `name`, a `configSection`, and a `config` map. Modeled as a list-map keyed by `configSection`, so the API server rejects duplicate sections structurally and server-side apply merges entries by section instead of replacing the whole list. Plugin sections lose to the operator defaults on a key collision and to `extraConfig` on top of that |
| `extraConfig` | `map[string]map[string]string` | no | Free-form INI sections for configuration not covered by explicit fields, the escape hatch for options with no dedicated knob. The render-time merge is `plugins < operator defaults < extraConfig`, so user values win. Overrides of operator-owned keys are honored but surfaced through the `ExtraConfigHealthy` condition and an `ExtraConfigOwnedKeyOverride` Warning event, with three exceptions the validating webhook refuses outright (see [Defaulting and validation](#defaulting-and-validation)). Option names are checked at admission against a per-release option catalog embedded in the operator, selected by `spec.openStackRelease`; a release with no embedded catalog skips the check with an admission warning. The per-store `[secretstore:<name>]` sections are exempt from the catalog scan, since their names come from the attached store CRs and no release catalog can list them. A deprecated-but-accepted option is admitted with a warning naming its replacement. Values are checked for one thing: a newline or carriage return in a section name, key, or value is rejected, because the rendered INI writes each verbatim and a newline would inject config lines past the ownership and catalog gates |

### ServiceUserSpec

The name and domain fields are webhook-defaulted, so a minimal CR need only
supply the password Secret reference.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `username` | `string` | no | `barbican` | Keystone username (`[keystone_authtoken] username`) |
| `projectName` | `string` | no | `service` | The project the service user scopes to (`project_name`) |
| `userDomainName` | `string` | no | `Default` | The domain the service user lives in (`user_domain_name`) |
| `projectDomainName` | `string` | no | `Default` | The domain the service project lives in (`project_domain_name`) |
| `secretRef` | `SecretRefSpec` | yes | `key` → `password` | The Secret holding the service-user password; the password is injected as the `OS_KEYSTONE_AUTHTOKEN__PASSWORD` env var and never rendered into config |

Each of the four identity fields is rendered verbatim into
`[keystone_authtoken]`, so the validating webhook rejects a newline or carriage
return in any of them. `spec.region` lands in the same section through the same
renderer and goes through the same check.

### APIServerSpec

Tunes the API server process. It carries the uWSGI block alone: barbican ships
a WSGI application and no eventlet server, so there is no launch mode to select
and no worker count to configure outside uWSGI. The sibling Glance operator's
`workers` field has no counterpart here.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `uwsgi` | [`*UWSGISpec`](#uwsgispec) | no | uWSGI application-server parameters |

### UWSGISpec

The shared `commonv1.UWSGISpec`. A cross-field CEL rule mirrors the webhook:
`httpKeepAliveTimeout` may only be set when `httpKeepAlive` is true, since the
`--http-keepalive-timeout` flag is otherwise never emitted. A nil
`httpKeepAlive` counts as unset and resolves to the default `true`, so only an
explicit `false` conflicts with a set timeout.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `processes` | `int32` (Minimum=1) | no | `2` | uWSGI worker-process count (`--processes`) |
| `threads` | `int32` (Minimum=1) | no | `1` | Threads per worker (`--threads`) |
| `httpKeepAlive` | `*bool` | no | `true` | Enables `--http-keepalive`. A nil-preserving pointer: nil restores the default, an explicit `false` is honored verbatim and drops the flag |
| `harakiri` | `*int32` (Minimum=1) | no | — | Per-request worker lifetime cap (`--harakiri`, seconds). Omitted from the command when nil, with no hidden default. The webhook requires `harakiri < terminationGracePeriodSeconds − preStopSleepSeconds`, so the worst-case per-request kill fits inside the drain window |
| `httpKeepAliveTimeout` | `*int32` (Minimum=1) | no | — | Idle timeout of keep-alive connections (`--http-keepalive-timeout`, seconds). Omitted when nil; zero is rejected to avoid the unbounded interpretation |

Raising `processes` raises the pooled connection count: every worker holds at
least one connection once its app has loaded, because each one syncs the
secret-store table at start-up. The operator sizes the SQL user's
`max_user_connections` from the topology (`(pods + 1) × processes × threads + 2`),
so a raised `processes` needs no manual grant.

### DBCleanSpec

Barbican only soft-deletes, so the operator projects a CronJob running
`barbican-manage db clean` to hard-delete the rows that fell out of the
retention window. The block is resolved at reconcile time by `effectiveDBClean`
(`operators/barbican/internal/controller/reconcile_dbclean.go`) and is left
alone by the defaulting webhook (`barbican_webhook.go`), so a field left unset
keeps tracking the operator default across upgrades instead of freezing today's
value into the stored CR. A nil block resolves like an empty one.

| Field | Type | Required | Resolved default | Description |
| --- | --- | --- | --- | --- |
| `retentionDays` | `*int32` (Minimum=1) | no | `90` | How long a soft-deleted row survives before the clean-up hard-deletes it (`--min-days`). Barbican's own default. The floor of one day keeps the sweep from racing rows an in-flight request just wrote. Lowering it applies retroactively at the next firing |
| `schedule` | `string` | no | `"1 0 * * *"` | The cron expression the CronJob runs on. The validating webhook checks the grammar and the CRD carries no pattern for it: the accepted grammar includes descriptors such as `@daily`, which no regex expresses without also rejecting valid expressions |
| `softDeleteExpiredSecrets` | `*bool` | no | `true` | Adds the `--soft-delete-expired-secrets` pass, which soft-deletes secrets whose expiration has passed so the same run can purge them under the retention window. The `true` default is a documented deviation from barbican's CLI default of `false`: an expired secret is unusable, and without the pass its row is never soft-deleted and therefore never purged. Set it to `false` for the upstream behaviour |
| `cleanUnassociatedProjects` | `*bool` | no | `false` | Adds the `--clean-unassociated-projects` pass, which deletes project rows that no longer own a secret, container, or order, along with their quota records. Opt-in: the pass keys off barbican's own associations and not off Keystone, so a project whose secrets were all deleted loses its configured quotas too |
| `suspend` | `bool` | no | `false` | Pauses the CronJob without deleting it. `DBCleanReady` stays True while suspended, under its own reason, because a paused clean-up is an operator's posture. It is the staging escape hatch for a brownfield database whose backlog has never been cleaned |

The CronJob is also suspended for the length of a schema convergence, whatever
`suspend` says: `barbican-manage db upgrade` takes DDL locks the clean-up's bulk
`DELETE`s contend with. Each run carries an `activeDeadlineSeconds` of one hour
and a `Forbid` concurrency policy, so a wedged run fails instead of accumulating
one active Job per firing.

### Defaulting and validation

The mutating webhook applies the shared `DeploymentSpec` defaults (replicas 3,
256Mi/512Mi memory and 100m/500m CPU), materializes the
`dogpile.cache.pymemcache` cache backend and the `LoggingSpec` baseline, fills
the `ServiceUserSpec` identity defaults (`barbican` / `service` / `Default` /
`Default`, `secretRef.key` → `password`), and, only when `spec.apiServer.uwsgi`
is present, the uWSGI sub-field defaults (`processes` 2, `threads` 1,
`httpKeepAlive` true). `spec.dbClean` is left untouched, for the reason
[DBCleanSpec](#dbcleanspec) gives.

`metadata.name` is bounded at 43 characters. The db-clean CronJob is the child
object with the tightest name budget: Kubernetes caps a CronJob name at 52
characters, its controller appending an 11-character timestamp suffix to every
Job it spawns, and the `-db-clean` suffix spends that budget down. The two
constants say the same thing,
`MaxBarbicanNameLength = MaxCronJobNameLength − len("-db-clean")` = 52 − 9 = 43.
The rule runs on create only: the name is immutable, so on update it could only
fire against an object a pre-upgrade operator already admitted, including the
finalizer-removal update that completes a deletion, and rejecting that would
wedge the CR in `Terminating` with no field left to edit. A name the bound would
reject keeps reconciling: `dbCleanCronJobName` collapses it onto a content-stable
hash instead of building an object the API server refuses.

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
PriorityClass existence, topology-spread selectors (matching the `barbican` and
instance labels), and the policy-rule name/value guard.

Two checks are barbican's own. `spec.region`, the four `serviceUser` identity
fields, and `spec.gateway.hostname` are each rejected for a newline or carriage
return, because the renderer writes them verbatim and a newline would smuggle a
whole extra option past the ownership and catalog gates. The hostname is on that
list because it reaches the renderer as `[DEFAULT] host_href`, and `[DEFAULT]`
is rendered first, so an injected line lands in a section the operator never
writes again. `spec.dbClean` carries the second: `retentionDays` has a floor of
one, and `schedule` is parsed for cron grammar.

Two admission warnings describe what a change deletes, since neither is
recoverable and both are legitimate operational choices:

| Warning | When |
| --- | --- |
| Retention reduction | An update lowers the effective `retentionDays`. The next run hard-deletes every row that fell out of the window between the old and the new value |
| Brownfield first run | A CR is created against a brownfield database (`spec.database.host`) without `spec.dbClean.suspend`. The first firing applies the window retroactively to a backlog that has never been cleaned |

`spec.extraConfig` is a preserve-unknown-fields map CEL cannot constrain, so its
guards are webhook-only: an empty section name or option key, a newline or
carriage return in any section name, key, or value, and an override of a
`Rejected` owned key. The rejection list is driven by the `Rejected` flag in
`operators/barbican/api/v1alpha1/config_ownership.go` and holds three entries,
each a credential the operator delivers through an environment variable:

| Key | Owned by | Why the override is refused |
| --- | --- | --- |
| `[keystone_authtoken] password` | `spec.serviceUser.secretRef` | The middleware reads the password from `OS_KEYSTONE_AUTHTOKEN__PASSWORD`, so a file value is inert at runtime and only copies the service password into the config Secret every API pod mounts |
| `[vault_plugin] approle_secret_id` | the store's credentials Secret | Same shape: the secret ID arrives through `OS_VAULT_PLUGIN__APPROLE_SECRET_ID`, and rendering it would put the AppRole secret into that same Secret |
| `[vault_plugin] root_token_id` | operator-computed | The vault plugin prefers a root token over AppRole authentication, so a rendered one replaces the mount-scoped AppRole with an unscoped credential in plain text, and both are done the moment the pods load the file |

Every other owned key is honored and reported. The registry covers `[DEFAULT]`
(`db_auto_create`, `host_href`), `[database] connection`, the
`[keystone_authtoken]` keys `keystoneauth.Section` renders, the `[secretstore]`
registry pair, the `[vault_plugin]` options derived from the attached store,
`[queue] enable`, and `[oslo_policy] policy_file`. Conditionally rendered keys
are registered unconditionally: the registry records that a key is not the
user's to set, not that it is currently rendered.

The catalog check re-runs on update only when one of its inputs changed
(`spec.extraConfig` or `spec.openStackRelease`), so an unrelated edit such as a
replica change cannot retroactively reject a CR whose `extraConfig` a
regenerated catalog has since invalidated.

## Status

| Field | Description |
| --- | --- |
| `conditions` | List-map keyed by `type`; see the [reconciler reference](./barbican-reconciler.md#conditions) for the vocabulary |
| `observedGeneration` | The `.metadata.generation` last reconciled |
| `endpoint` | The Barbican API URL: `https://{gateway.hostname}` when a gateway is set, otherwise the cluster-local Service URL. It is the same value the rendered `[DEFAULT] host_href` carries, so what barbican advertises in its API links and what the CR reports are one value |
| `installedRelease` | The OpenStack release whose schema is currently installed, promoted to `spec.openStackRelease` after a successful db-sync |
| `installedImage` | The `spec.image` reference that migrated the schema `installedRelease` names. A digest carries no parseable release, so without this field a `spec.openStackRelease` bump that left `spec.image` untouched would run no migration and still promote `installedRelease`; the release gate compares against it to refuse that transition |
| `targetRelease` | The `spec.openStackRelease` a release bump is converging to. Stamped when the gate accepts the bump, cleared once `installedRelease` equals the spec release; empty in steady state |
| `projectedSecretStores` | The attached, credential-ready stores the last valid projection rendered into `barbican.conf`, in section order. A store dropping out of this set is what a detach is detected against |
| `projectedSecretStoreHosts` | The server URLs that same projection resolved. The NetworkPolicy egress set of an invalid projection is widened from this record and never re-derived from the live store specs, since `spec.openBao.server.url` is mutable and only scheme-checked |

The status carries no `upgradePhase`. Barbican runs no expand-migrate-contract
phase machine: its migrations apply in one `barbican-manage db upgrade` pass,
and the release rules the phased flow enforces on its way in (no downgrades, no
multi-release jumps) are checked by the release gate before any Job runs. See
[Database](./barbican-reconciler.md#database).

## Sub-Resource Naming Convention

Operator-managed sub-resources (Deployment, Service, PodDisruptionBudget,
HorizontalPodAutoscaler, NetworkPolicy, HTTPRoute, and the MariaDB
`Database`/`User`/`Grant`) use the bare CR name with no suffix, matching the
keystone convention. A Barbican CR named `barbican` in the `openstack` namespace
is therefore reachable in-cluster at
`http://barbican.openstack.svc.cluster.local:9311`, since the Service DNS name
is the CR name.

The content-addressed and derived resources are the exceptions:

| Resource | Name | Notes |
| --- | --- | --- |
| Config Secret | `{name}-config-<hash>` | Immutable, content-hashed `barbican.conf` and `barbican-api-paste.ini` (plus `policy.yaml` when applicable); 3 historical retained. A Secret rather than a ConfigMap, because the rendered `barbican.conf` carries the vault plugin's `approle_role_id` and barbican reads one configuration file |
| DB-connection Secret | `{name}-db-connection` | Derived pymysql DSN, stable name |
| DB-sync Job | `{name}-db-sync` | `barbican-manage db upgrade` |
| DB-clean CronJob | `{name}-db-clean` | `barbican-manage db clean`, projected on every Barbican. The plain form holds for every admissible name; an over-long inherited name collapses onto a hash |

The AppRole credentials Secret of a managed store is named after the store, not
after the Barbican: see
[Retained Artefacts](./barbican-secret-store-crd.md#retained-artefacts).

## Example

```yaml
apiVersion: barbican.openstack.c5c3.io/v1alpha1
kind: Barbican
metadata:
  name: barbican
  namespace: openstack
spec:
  openStackRelease: "2025.2"
  deployment:
    replicas: 3
  image:
    repository: ghcr.io/c5c3/barbican
    tag: "2025.2"
  database:
    clusterRef:
      name: openstack-mariadb
    database: barbican
    secretRef:
      name: barbican-db
  cache:
    clusterRef:
      name: openstack-memcached
  keystoneEndpoint: http://keystone.openstack.svc.cluster.local:5000/v3
  serviceUser:
    secretRef:
      name: barbican-keystone
```

The CR reaches `Ready` once a `BarbicanSecretStore` attached to it is
credential-ready and marked `isDefault`; until then the API Deployment is not
created at all, because barbican resolves its secret store at process start and
exits when none is configured.
