---
title: Placement Reconciler Architecture
quadrant: operator
---

# Placement Reconciler Architecture

The Placement controller runs the shared table-driven pipeline
(`internal/common/reconcile`) with nine sub-reconcilers. Every step is
instrumented under the `placement_operator` metrics prefix, and the first step to
return a non-zero result or an error short-circuits the chain. Conditions and the
requeue are persisted on every exit path through the shared status skeleton,
which also skips the write when a pass left status unchanged.

Two Prometheus vectors cover the pipeline
(`placement_operator_reconcile_duration_seconds`,
`placement_operator_reconcile_errors_total`, the latter labelled by
`sub_reconciler` and `condition_type`), and two per-CR collectors cover the
schema migration (`placement_operator_db_sync_total`, labelled by `placement`,
`namespace`, and the terminal `result`, and
`placement_operator_db_sync_duration_seconds`, labelled by `placement` and
`namespace`). There are no purge collectors: placement projects no recurring
maintenance Job. Every series carrying a CR's name and namespace is dropped when
that CR's finalizer completes.

## Pipeline

```text
Secrets ──► DBConnectionSecret ──► Config ──► Database ──► Deployment ──► ┬─ HTTPRoute
                                                                          ├─ HealthCheck
                                                                          ├─ HPA
                                                                          └─ NetworkPolicy   (parallel)
```

| Step | What it does | Condition |
| --- | --- | --- |
| Secrets | Gates on the selected secret store (`spec.secretStoreRef`, default `openbao-cluster-store`), then the ESO-synced database and service-user credential Secrets; digests the service-user password for the rollout annotation | `SecretsReady` |
| DBConnectionSecret | Materializes the pymysql DSN into the derived `{name}-db-connection` Secret and digests it; **reports through `SecretsReady`** | `SecretsReady` |
| Config | Renders `placement.conf` (plus `policy.yaml` / `logging.conf` when applicable) into an immutable content-addressed ConfigMap and prunes the history down to three, and records the `ExtraConfigHealthy` guard. **Reports through `SecretsReady`** (failure reason `ConfigError`) | `SecretsReady` |
| Database | Provisions the schema (MariaDB cluster gate, `Database`/`User`/`Grant` in managed mode), gates the requested release against the installed one, runs the `{name}-db-sync` Job, and promotes `installedRelease` | `DatabaseReady` |
| Deployment | Ensures the API Deployment, Service (port 8778), and PDB, narrows the Service selector to the API component once the rollout has converged, and stamps `status.endpoint` | `DeploymentReady` |
| HTTPRoute | Full `spec.gateway` lifecycle; reflects the Gateway's Accepted condition. It renders no request timeout, since placement answers short JSON requests and the gateway implementation's default is the right cap | `HTTPRouteReady` |
| HealthCheck | HTTP GET of the cluster-local `/` version document through the shared TTL probe cache | `PlacementAPIReady` |
| HPA | Creates/deletes the HorizontalPodAutoscaler | `HPAReady` |
| NetworkPolicy | Creates/deletes the NetworkPolicy (auto-derived DNS/database/Keystone/cache egress); refuses an empty ingress list (fail-closed) | `NetworkPolicyReady` |

`DBConnectionSecret` and `Config` reuse `SecretsReady` instead of a dedicated
`ConfigReady` condition, the same mapping the sibling operators use: both produce
Secret and ConfigMap artefacts that gate the same downstream graph, so a distinct
`sub_reconciler` label on the error counter disambiguates them during triage
while the status contract stays minimal.

The four members of the parallel group have no inter-dependency once the
Deployment and Service exist. Each works on its own copy of the CR, sets one
condition type, and always sets it, so a cluster without Gateway API or without
autoscaling still resolves the aggregate through the `NotRequired` reasons.

## Conditions

The aggregate `Ready` condition is `True` (reason `AllReady`) when all seven
sub-conditions are `True`, and `False` (`NotAllReady`) otherwise.

| Type | True reasons | False reasons |
| --- | --- | --- |
| `SecretsReady` | `SecretsAvailable` | `SecretStoreNotReady`, `WaitingForDBCredentials`, `WaitingForServiceUserCredentials`, `ConfigError` |
| `DatabaseReady` | `DatabaseSynced` | `ClusterNotReady`, `WaitingForDatabase`, `DBSyncInProgress`, `DBSyncFailed`, `VersionParseError`, `DowngradeNotSupported`, `UpgradePathInvalid`, `ImageReleaseMismatch` |
| `DeploymentReady` | `DeploymentReady` | `WaitingForDeployment` |
| `PlacementAPIReady` | `APIHealthy` | `APIUnhealthy`, `EndpointNotReady`, `HealthCheckTimeout`, `ConnectionFailed`, `HealthCheckFailed` |
| `HPAReady` | `HPAReady`, `HPANotRequired` | — (errors propagate) |
| `NetworkPolicyReady` | `NetworkPolicyReady`, `NetworkPolicyNotRequired` | — (errors propagate) |
| `HTTPRouteReady` | `HTTPRouteAccepted`, `HTTPRouteNotRequired` | `HTTPRouteNotAccepted`, `GatewayAPINotInstalled` |

One further condition is set and never aggregated. `ExtraConfigHealthy` reports
whether `spec.extraConfig` overrides an operator-owned key: `True` with reason
`NoOwnedKeysOverridden`, `False` with reason `OwnedKeysOverridden` and a message
naming each overridden key. It stays out of the sub-condition list on purpose, so
an override that the operator honors reports itself without holding `Ready` down.
The three overrides that would already have done their damage by the time a
condition could report them are refused at admission instead; see
[Defaulting and validation](./placement-crd.md#defaulting-and-validation).

## Launch mode

There is one launch mode. Placement never shipped an eventlet server, so
`spec.openStackRelease` selects nothing here and two CRs differing only in their
release render byte-identical config files.

The container command is `uwsgi --http :8778`, followed by `--http-keepalive`
(and `--http-keepalive-timeout <n>` when `httpKeepAliveTimeout` is set),
`--log-master`, `--log-format` with the request-line format the sibling operators
share, `--wsgi-file /var/lib/openstack/bin/placement-api`, `--master`,
`--lazy-apps`, `--need-app`, `--processes`, `--threads`, and `--harakiri <n>`
when `harakiri` is set. Keep-alive is on by default and an explicit
`httpKeepAlive: false` drops both flags; `--harakiri` appears only when the field
is set.

The WSGI entry file is written by the operator's Placement image, because 2025.2
declares `placement-api` as a PBR `wsgi_scripts` entry the install mode does not
materialize and 2026.1 declares no WSGI script at all. The config location
travels in the `OS_PLACEMENT_CONFIG_DIR=/etc/placement` environment variable: the
entry calls `init_application()` with no arguments and placement's
`_get_config_files` loads `$OS_PLACEMENT_CONFIG_DIR/placement.conf`, one file,
with no directory scan and no `sys.argv` parsing. That is why the command carries
no `--pyargv`, and why the config ConfigMap is mounted as the whole
`/etc/placement` directory: its other data keys (`policy.yaml`, `logging.conf`)
are invisible to the config loader and are reached through the options that name
them. The `placement-manage` and `placement-status` CLIs read neither the
environment variable nor a config directory, so the db-sync Job passes
`--config-file /etc/placement/placement.conf` on each of its three commands.

## Database

The Database step runs the shared provisioning flow first: a MariaDB cluster gate
and `Database`/`User`/`Grant` in managed mode, a no-op in brownfield, and no
`User`/`Grant` under `credentialsMode: Dynamic`, where an external engine issues
the credential.

The migration is a single `{name}-db-sync` Job running three commands under
`/bin/sh -eu -c`:

```sh
placement-manage --config-file /etc/placement/placement.conf db sync && \
placement-manage --config-file /etc/placement/placement.conf db online_data_migrations && \
{ placement-status --config-file /etc/placement/placement.conf upgrade check || [ $? -eq 1 ]; }
```

`db sync` applies every pending schema migration, `db online_data_migrations`
moves the rows the new schema expects, and `placement-status upgrade check`
validates the result. The brace group scopes the exit-1 tolerance to the check
alone: `placement-status` exits 1 for warnings, an outcome the Job must not fail
on, and 2 for errors, which fails the group and, under `sh -e`, the Job. The two
`placement-manage` commands run outside the group, so their failures stay fatal.

Because `db sync` applies all pending migrations in one idempotent pass and the
upgrade check inside the script already validates the migrated schema, the Job
set carries no schema-check command (`SchemaCheckCommand: nil`). Unlike Keystone,
**`SchemaDriftDetected` never fires for Placement**.

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
run the wrong `placement-manage` binary. A release bump that leaves `spec.image`
untouched is refused under the same reason, because the Job's pod template would
be identical, the shared flow would short-circuit on the already-completed Job,
and `installedRelease` would advance off a run of the previous release's binary.
The second guard compares `status.installedImage`, so it covers a digest-pinned
image the first one cannot read.

## Requeue semantics

| Interval | Used by |
| --- | --- |
| 10s | Deployment readiness polling, HTTPRoute acceptance, health-check retry |
| 15s | ESO secret-gate polling (Secrets, DBConnectionSecret) |
| 30s | MariaDB and db-sync database wait, and the finalizer hold while the MariaDB CRs tear down |
| 30s TTL | Health-probe cache (a passing `/` probe is reused within the TTL) |

## Rotation and deletion

- **Credential rotation** happens at the OpenBao source. The service-user
  password (consumed via `OS_KEYSTONE_AUTHTOKEN__PASSWORD`) and the assembled DSN
  (consumed via `OS_PLACEMENT_DATABASE__CONNECTION`) are both env-var-delivered,
  so a restart is required for a rotation to take effect. The Secrets and
  DBConnectionSecret steps digest each value into a pod-template annotation,
  `placement.c5c3.io/authtoken-hash` and `placement.c5c3.io/db-connection-hash`,
  so a changed digest rolls the Deployment. Each annotation is omitted while its
  digest is empty, which is what the requeue and error paths upstream produce, so
  a short-circuited pass causes no spurious rollout.
- **Deletion** runs the `placement.openstack.c5c3.io/finalizer`. It issues Delete
  on the MariaDB `Database`/`User`/`Grant` CRs before the owner-ref chain
  disappears, emitting `FinalizingDatabase` while cleanup remains and
  `DatabaseFinalized` when the finalizer is released. Whether cleanup remains is
  observed before the Delete, since a Delete flips `DeletionTimestamp` and a
  post-Delete check would always report nothing live and release immediately.
  Every other owned resource is namespace-scoped with a controller owner
  reference, so Kubernetes garbage collection reclaims it. Releasing the
  finalizer also drops the CR's metric series and evicts its health-probe cache
  entry.

## Watches

The Placement controller `Owns` its Deployment, Service, ConfigMap, Secret,
PodDisruptionBudget, HorizontalPodAutoscaler, NetworkPolicy, and, for the db-sync
migration, Job. The HTTPRoute is added to the `Owns` set only when the Gateway
API CRD is installed, probed at setup through the RESTMapper; without it
`spec.gateway` surfaces `HTTPRouteReady=False / GatewayAPINotInstalled` instead
of failing the controller at start with an unknown kind. The CR's own
status-only updates are filtered out, so a status write does not re-wake the
controller.

Beyond the owned set it watches:

- **Secrets**, mapped to the Placement CRs that reference them. The
  `spec.secretRefs.name` field index holds the deduplicated union of
  `spec.serviceUser.secretRef.name` and `spec.database.secretRef.name`, so a
  Secret event resolves in one lookup instead of listing every Placement in the
  namespace. ESO-managed Secrets are owned by the ExternalSecret controller, so
  an owner-reference watch alone would never match them.
- **MariaDB** clusters referenced by `spec.database.clusterRef`, so an upstream
  database outage reflects in `DatabaseReady` without waiting for the periodic
  requeue.
- Both the cluster-scoped `ClusterSecretStore` and the namespaced `SecretStore` a
  Placement can select, so a store-backend outage reflects in `SecretsReady` as
  soon as ESO flips the store's Ready condition. A Placement that omits
  `spec.secretStoreRef` resolves to the shared cluster store, so the default
  fan-out is preserved while a Placement pinned to a namespaced store is woken
  only by its own.
