---
title: Barbican Reconciler Architecture
quadrant: operator
---

# Barbican Reconciler Architecture

The Barbican controller runs the shared table-driven pipeline
(`internal/common/reconcile`) with eleven sub-reconcilers. Every step is
instrumented under the `barbican_operator` metrics prefix, and the first step to
return a non-zero result or an error short-circuits the chain. Conditions and
the requeue are persisted on every exit path through the shared status skeleton,
which also skips the write when a pass left status unchanged. A second
reconciler in the same manager owns the attached
[`BarbicanSecretStore`](./barbican-secret-store-crd.md) CRs.

Two Prometheus vectors cover the pipeline
(`barbican_operator_reconcile_duration_seconds`,
`barbican_operator_reconcile_errors_total`, the latter labelled by
`sub_reconciler` and `condition_type`). Five per-CR collectors cover the Jobs
and the store credentials: `barbican_operator_db_sync_total` and
`barbican_operator_db_clean_total`, each labelled by `barbican`, `namespace`,
and the terminal `result`; `barbican_operator_db_sync_duration_seconds` and
`barbican_operator_db_clean_duration_seconds`, labelled by `barbican` and
`namespace`; and `barbican_operator_secretstore_remints_total`, labelled by
`store`, `namespace`, and a `trigger` of `proactive` (the operator refreshed the
AppRole secret ID before its TTL lapsed) or `reactive` (the server had already
rejected it). The split is what tells a healthy rotation schedule apart from one
that keeps arriving too late. The Job series are dropped when a Barbican's
finalizer completes; the re-mint series is keyed by the store CR, so the store
controller drops it on the pass that observes that store's deletion.

## Pipeline

```text
Secrets ─► DBConnectionSecret ─► SecretStores ─► Config ─► DBClean ─► Database ─► Deployment ─► ┬─ HTTPRoute
                                                                                                ├─ HealthCheck
                                                                                                ├─ HPA
                                                                                                └─ NetworkPolicy  (parallel)
```

| Step | What it does | Condition |
| --- | --- | --- |
| Secrets | Gates on the selected External Secrets store (`spec.secretStoreRef`, default `openbao-cluster-store`), then the ESO-synced database and service-user credential Secrets; digests the service-user password for the rollout annotation | `SecretsReady` |
| DBConnectionSecret | Materializes the pymysql DSN into the derived `{name}-db-connection` Secret and digests it; **reports through `SecretsReady`** | `SecretsReady` |
| SecretStores | Aggregates the attached, credential-ready `BarbicanSecretStore`s into the secret-store sections of `barbican.conf`, records what it projected, and resolves the OpenBao egress hosts | `SecretStoresReady` |
| Config | Renders `barbican.conf` and `barbican-api-paste.ini` (plus `policy.yaml` when applicable) into an immutable content-hashed Secret, prunes the history down to three, and records the `ExtraConfigHealthy` guard. On an invalid projection it re-renders nothing and returns the Secret name the live Deployment mounts. **Reports through `SecretsReady`** (failure reason `ConfigError`) | `SecretsReady` |
| DBClean | Projects the `{name}-db-clean` CronJob against the rendered config and reports the newest terminal run it spawned | `DBCleanReady` |
| Database | Provisions the schema (MariaDB cluster gate, `Database`/`User`/`Grant` in managed mode), gates the requested release against the installed one, runs the `{name}-db-sync` Job, and promotes `installedRelease` | `DatabaseReady` |
| Deployment | Ensures the API Deployment, Service (port 9311), and PDB, narrows the Service selector to the API component once the rollout has converged, and stamps `status.endpoint`. It creates nothing while the projection is invalid | `DeploymentReady` |
| HTTPRoute | Full `spec.gateway` lifecycle; reflects the Gateway's Accepted condition. It renders no request timeout, since barbican answers short JSON requests and the gateway implementation's default is the right cap | `HTTPRouteReady` |
| HealthCheck | HTTP GET of the cluster-local `/healthcheck` app through the shared TTL probe cache. The paste composite routes that path outside the authtoken pipeline, so a 2xx means the WSGI app serves requests without a token or a database round trip | `BarbicanAPIReady` |
| HPA | Creates/deletes the HorizontalPodAutoscaler | `HPAReady` |
| NetworkPolicy | Creates/deletes the NetworkPolicy (auto-derived DNS, database, Keystone, cache, and OpenBao egress); refuses an empty ingress list (fail-closed) | `NetworkPolicyReady` |

`DBConnectionSecret` and `Config` reuse `SecretsReady` instead of a dedicated
`ConfigReady` condition, the same mapping the sibling operators use: both
produce Secret artefacts that gate the same downstream graph, so a distinct
`sub_reconciler` label on the error counter disambiguates them during triage
while the status contract stays minimal.

Two orderings in that chain are decisions rather than data dependencies:

- **DBClean runs ahead of Database.** The clean-up's bulk `DELETE`s contend with
  the DDL locks a schema migration holds, so the CronJob has to be suspended for
  the length of a migration. The pipeline short-circuits at the Database step
  for as long as the sync Job runs, which means a step placed behind it is never
  reached during the one window that needs pausing. Its only input is the
  rendered config the CronJob mounts, which the Config step above already
  produced.
- **SecretStores never short-circuits.** Every waiting state of that step (no
  default, several defaults, several OpenBao stores, an unreadable credentials
  Secret) returns a zero result and no error, so a first install proceeds to the
  steps that report what is missing. A store's status flip re-enqueues the
  parent through the `BarbicanSecretStore` watch, which is a faster signal than
  any requeue interval.

The four members of the parallel group have no inter-dependency once the
Deployment and Service exist. Each works on its own copy of the CR, sets one
condition type, and always sets it, so a cluster without Gateway API or without
autoscaling still resolves the aggregate through the `NotRequired` reasons.

## Conditions

The aggregate `Ready` condition is `True` (reason `AllReady`) when all nine
sub-conditions are `True`, and `False` (`NotAllReady`) otherwise.

| Type | True reasons | False reasons |
| --- | --- | --- |
| `SecretsReady` | `SecretsAvailable` | `TargetClusterUnavailable`, `SecretStoreNotReady`, `WaitingForDBCredentials`, `WaitingForServiceUserCredentials`, `ConfigError` |
| `SecretStoresReady` | `AllStoresProjected` | `NoDefaultSecretStore`, `MultipleOpenBaoStores`, `WaitingForSecretStores` |
| `DBCleanReady` | `DBCleanScheduled`, `DBCleanSuspended` | `DBCleanJobFailed`, `DBCleanBlocked`, `WaitingForSecretStores` |
| `DatabaseReady` | `DatabaseSynced` | `WaitingForSecretStores`, `WaitingForDatabase`, `DBSyncInProgress`, `DBSyncFailed`, `VersionParseError`, `DowngradeNotSupported`, `UpgradePathInvalid`, `ImageReleaseMismatch`, plus the shared cluster-gate reason the provisioning flow sets while the referenced MariaDB is unavailable |
| `DeploymentReady` | `DeploymentReady` | `WaitingForSecretStores`, `WaitingForDeployment` |
| `BarbicanAPIReady` | `APIHealthy` | `APIUnhealthy`, `HealthCheckTimeout`, `ConnectionFailed`, `HealthCheckFailed`, plus the shared pre-endpoint wait the probe flow sets before `status.endpoint` is stamped |
| `HPAReady` | `HPAReady`, `HPANotRequired` | — (errors propagate) |
| `NetworkPolicyReady` | `NetworkPolicyReady`, `NetworkPolicyNotRequired` | — (errors propagate) |
| `HTTPRouteReady` | `HTTPRouteAccepted`, `HTTPRouteNotRequired` | `HTTPRouteNotAccepted`, `GatewayAPINotInstalled` |

`TargetClusterUnavailable` is set ahead of every sub-reconciler, when
`spec.targetClusterRef` names a target cluster that is not registered or no
longer resolves. The message carries the resolver's error, the CR requeues after
15 seconds and acquires no finalizer, and nothing is created on any cluster. The
attached `BarbicanSecretStore` reports the same reason on `CredentialsReady`.
See [Target Clusters](../target-clusters.md).

`WaitingForSecretStores` appears on four condition types. On
`SecretStoresReady` it reports a credentials Secret this pass could not read. On
`DBCleanReady`, `DatabaseReady`, and `DeploymentReady` it reports the cascade a
missing default store causes: the CronJob and the migration Job have no config
Secret to mount, and the API pods have no store to resolve secrets through.

`DBCleanSuspended` is a True reason twice over, once for
`spec.dbClean.suspend` and once for the pause a schema convergence imposes. The
migration pause reports `DBCleanBlocked` and `False` instead when the release
gate rejected the transition it is waiting for: nothing is running, the
convergence is not coming, and the backlog grows until `spec.openStackRelease`
and `spec.image` are corrected.

One further condition is set and never aggregated. `ExtraConfigHealthy` reports
whether `spec.extraConfig` overrides an operator-owned key: `True` with reason
`NoOwnedKeysOverridden`, `False` with reason `OwnedKeysOverridden` and a message
naming each overridden key. It stays out of the sub-condition list so an
override the operator honors reports itself without holding the aggregate down.
The three credential overrides that would already have done their damage by the
time a condition could report them are refused at admission; see
[Defaulting and validation](./barbican-crd.md#defaulting-and-validation).

## Secret-store aggregation

The SecretStores step lists the attached stores through the
`spec.barbicanRef.name` field index, sorts them by name so the rendered sections
and the config Secret's content hash are stable across passes, and keeps the
ones whose `CredentialsReady` condition is True. It never consults the store's
aggregate `Ready`, which also requires `ConfigProjected` and would deadlock,
since `ConfigProjected` only turns True after this step projects the store.

What it renders into the one content-hashed config Secret:

- `[secretstore]`, the process-global registry, carrying
  `enable_multiple_secret_stores = true` and a `stores_lookup_suffix` list of
  the ready store names.
- One `[secretstore:<name>]` per ready store, each with
  `secret_store_plugin = vault_plugin`. The default store's section also carries
  `global_default = True`; the option is written on that store alone, since a
  second global default is a configuration error barbican refuses to start with.
- `[vault_plugin]`, derived from the default store alone. It is process-global,
  and the single-OpenBao-store rule guarantees the default is the only store
  that can contribute to it.

`approle_secret_id` is never rendered. The AppRole secret ID reaches the pods as
the `OS_VAULT_PLUGIN__APPROLE_SECRET_ID` environment variable, sourced from the
Secret the store itself owns, and its SHA-256 is stamped into a pod-template
annotation so a re-mint rolls the Deployment. The rendered document is a Secret,
but every API pod mounts it and the store controller reads it back to observe
`ConfigProjected`, so the secret ID's only copy stays where the store keeps it.
The role ID does travel in the file, which is why the config artefact is a
Secret and not a ConfigMap.

Three historical config Secrets are retained beside the current one, so a
rollback has something to roll back to. An invalid projection re-renders nothing
at all: the Config step returns the Secret name the live Deployment mounts, and
the running pods keep the configuration they started with. Egress follows that
same last-good record, not the projection this pass could build. The
NetworkPolicy step widens its OpenBao host set from
`status.projectedSecretStoreHosts`, because a dropped host means no OpenBao rule
at all, and with `Egress` in `policyTypes` no rule is a deny. The record is
written on the valid path alone: re-deriving the URL from the live store spec
would let anyone able to write a store point `spec.openBao.server.url` at a
server of their choosing, let the projection go invalid, and have that port land
in a destination-unrestricted egress rule on API pods they cannot edit.

A store that drops out of `status.projectedSecretStores` raises the
`SecretStoreDetached` Warning event on the pass that de-projects it, naming what
the change costs: barbican resolves each stored secret through the
`secret_stores` row that names the store it was written to, so the material
written through a de-projected store stops resolving.

## Database

The Database step runs the shared provisioning flow first: a MariaDB cluster
gate and `Database`/`User`/`Grant` in managed mode, a no-op in brownfield, and no
`User`/`Grant` under `credentialsMode: Dynamic`, where an external engine issues
the credential. The SQL user's `max_user_connections` is sized from the CR's own
topology, `(pods + 1) × processes × threads + 2`, with `pods` taken from
`spec.autoscaling.maxReplicas` when an HPA owns the replica count. Left unsized,
the mariadb-operator default of 10 applies, which a raised
`spec.apiServer.uwsgi.processes` exceeds before a single request is served.

The migration is a single `{name}-db-sync` Job running `barbican-manage db
upgrade`, one alembic upgrade to head that applies every pending revision in one
pass. The Job passes no `--config-file`: `barbican-manage` builds its
oslo.config from the project name and resolves `barbican.conf` through the fixed
search path, which the mounted config directory is on, so the Job and the API
pods read the identical file. Because that one pass is idempotent, the Job set
carries no schema-check command, and `SchemaDriftDetected` never fires for
Barbican.

A release transition is a `spec.openStackRelease` bump with the image in
lockstep. There is no phase machine: the release gate validates the requested
release against `status.installedRelease` before any Job runs, and rejects an
unparseable release on either side (`VersionParseError`), a downgrade
(`DowngradeNotSupported`), and a jump of more than one release
(`UpgradePathInvalid`). Each rejection sets `DatabaseReady=False` with the shared
reason, raises a Warning event, and returns an error so the controller backs off.
An accepted bump stamps `status.targetRelease`, the db-sync Job runs on the new
image, the Deployment rolls onto it, and the sync flow promotes
`status.installedRelease` on Job success, at which point `targetRelease` is
cleared and `status.installedImage` records the image that ran the migration.

Two guards keep the release marker honest, one per pinning style. A tag-pinned
image whose tag names a different release than `spec.openStackRelease` sets
`DatabaseReady=False / ImageReleaseMismatch` and requeues, since the Job would
run the wrong `barbican-manage` binary. A release bump that leaves `spec.image`
untouched is refused under the same reason, because the Job's pod template would
be identical, the shared flow would short-circuit on the already-completed Job,
and `installedRelease` would advance off a run of the previous release's binary.
The second guard compares `status.installedImage`, so it covers a digest-pinned
image the first one cannot read.

The single pass has one consequence the phased flow does not. This step runs
ahead of the Deployment step and requeues while the Job runs, so the schema
reaches release N before a single pod of release N exists: from Job completion
until the last old pod terminates, release N-1 code serves traffic against a
release N schema. Whether that window is safe is a property of the alembic
revisions the target release carries. A release with a non-additive revision (a
dropped or renamed column) needs the API drained before the upgrade;
`DatabaseReady` goes True the moment the Job completes and reports nothing about
the rollout that follows.

## Requeue semantics

| Interval | Used by |
| --- | --- |
| 10s | Deployment readiness polling, HTTPRoute acceptance, health-check retry |
| 15s | ESO secret-gate polling (Secrets, DBConnectionSecret) |
| 30s | MariaDB and db-sync database wait, the finalizer hold while the MariaDB CRs tear down, and every waiting or failure state of the store controller |
| 30s TTL | Health-probe cache (a passing `/healthcheck` probe is reused within the TTL) |
| 15m | Store credential revalidation: every pass for a brownfield store, and for a managed store whose secret ID carries no TTL and has no re-mint timer to ride on |

A managed store with a TTL requeues at its own proactive re-mint threshold, two
thirds of the recorded lifetime, floored at the 30s retry cadence. That leaves a
full third of the TTL as the margin in which a failing re-mint can be retried
while the credential in the pods is still valid.

## Rotation and deletion

- **Credential rotation** happens at the source. The service-user password
  (`OS_KEYSTONE_AUTHTOKEN__PASSWORD`), the assembled DSN
  (`OS_DATABASE__CONNECTION`), and the AppRole secret ID
  (`OS_VAULT_PLUGIN__APPROLE_SECRET_ID`) are all environment-variable-delivered,
  so a restart is required for a rotation to take effect. Each value is digested
  into a pod-template annotation, `barbican.c5c3.io/authtoken-hash`,
  `barbican.c5c3.io/db-connection-hash`, and
  `barbican.c5c3.io/secret-store-credentials-hash`, so a changed digest rolls the
  Deployment. Each annotation is omitted while its digest is empty, which is what
  the requeue and error paths upstream produce, so a short-circuited pass causes
  no spurious rollout.
- **Deletion** runs the `barbican.openstack.c5c3.io/finalizer`. It issues Delete
  on the MariaDB `Database`/`User`/`Grant` CRs before the owner-ref chain
  disappears, emitting `FinalizingDatabase` while cleanup remains and
  `DatabaseFinalized` when the finalizer is released. Whether cleanup remains is
  observed before the Delete, since a Delete flips `DeletionTimestamp` and a
  post-Delete check would always report nothing live and release immediately.
  Every other owned resource is namespace-scoped with a controller owner
  reference, so Kubernetes garbage collection reclaims it. Releasing the
  finalizer also drops the CR's metric series and evicts its health-probe cache
  entry. A CR whose target cluster was deregistered in the meantime cannot
  reach any of it: the finalizer is released against no cleanup at all, a
  `RemoteChildrenAbandoned` Warning names what stays behind, and the CR
  leaves etcd instead of hanging in Terminating. See
  [Target Clusters](../target-clusters.md).
  A `BarbicanSecretStore` carries no finalizer at all; see
  [Retained Artefacts](./barbican-secret-store-crd.md#retained-artefacts).

## Watches

The Barbican controller `Owns` its Deployment, Service, Secret,
PodDisruptionBudget, HorizontalPodAutoscaler, NetworkPolicy, Job, and CronJob.
The HTTPRoute is added to the `Owns` set only when the Gateway API CRD is
installed, probed at setup through the RESTMapper; without it `spec.gateway`
surfaces `HTTPRouteReady=False / GatewayAPINotInstalled` instead of failing the
controller at start with an unknown kind. The CR's own status-only updates are
filtered out, so a status write does not re-wake the controller.

Beyond the owned set it watches:

- **Secrets**, mapped to the Barbican CRs that reference them. One field index
  holds the deduplicated union of `spec.serviceUser.secretRef.name` and
  `spec.database.secretRef.name`; a second holds the AppRole credentials and CA
  bundle Secrets a brownfield store references, so a rotated store credential
  re-renders the config through the store's parent. A managed store indexes under
  nothing there, since it references no Secret by name. ESO-managed Secrets are
  owned by the ExternalSecret controller, so an owner-reference watch alone would
  never match them.
- **BarbicanSecretStores**, mapped to their parent, without a generation
  predicate: the status transitions are the signal. `CredentialsReady` turning
  True triggers projection and a `DeletionTimestamp` triggers de-projection.
- **MariaDB** clusters referenced by `spec.database.clusterRef`, so an upstream
  database outage reflects in `DatabaseReady` without waiting for the periodic
  requeue.
- Both the cluster-scoped `ClusterSecretStore` and the namespaced `SecretStore` a
  Barbican can select, so a backend outage reflects in `SecretsReady` as soon as
  ESO flips the store's Ready condition. A Barbican that omits
  `spec.secretStoreRef` resolves to the shared cluster store, so the default
  fan-out is preserved while a Barbican pinned to a namespaced store is woken
  only by its own.

The store controller watches two objects of its own, both without a generation
predicate for the same reason: the parent `Barbican`, whose status flips carry
the projection landing in the Deployment, and the `OpenBaoCluster` a managed
store names, whose Available condition is what unblocks a store waiting on it.
Both resolve through field indexes registered by the Barbican controller's
`SetupWithManager`, which is the single registration site and therefore runs
first.
