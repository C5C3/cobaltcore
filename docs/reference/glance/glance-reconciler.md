---
title: Glance Reconciler Architecture
quadrant: operator
---

# Glance Reconciler Architecture

The Glance controller runs the shared table-driven pipeline
(`internal/common/reconcile`) with eight sub-reconcilers. Every step is
instrumented under the `glance_operator` metrics prefix, and the first step to
return a non-zero result or an error short-circuits the chain — conditions and
the requeue are persisted on every exit path through the shared status skeleton.

Two Prometheus vectors cover the pipeline
(`glance_operator_reconcile_duration_seconds`,
`glance_operator_reconcile_errors_total`, the latter labelled by
`sub_reconciler` and `condition_type`), and two per-CR db-sync collectors cover
the schema migration (`glance_operator_db_sync_total`,
`glance_operator_db_sync_duration_seconds`).

## Pipeline

```text
Secrets ──► DBConnectionSecret ──► Backends ──► Config ──► Database ──► Deployment ──► ┬─ HTTPRoute
                                                                                       ├─ HealthCheck
                                                                                       ├─ HPA
                                                                                       └─ NetworkPolicy  (parallel)
```

| Step | What it does | Condition |
| --- | --- | --- |
| Secrets | Gates on the selected secret store (`spec.secretStoreRef`, default `openbao-cluster-store`), then the ESO-synced database and service-user credential Secrets; digests the service-user password for the rollout annotation | `SecretsReady` |
| DBConnectionSecret | Materializes the pymysql DSN into the derived `{name}-db-connection` Secret and digests it; **reports through `SecretsReady`** | `SecretsReady` |
| Backends | Aggregates the attached, credential-ready `GlanceBackend`s into the content-hashed backends Secret. Waiting states never short-circuit the pipeline (first-install proceeds; a backend status flip re-enqueues via the watch) | `BackendsReady` |
| Config | Renders `glance-api.conf` / `glance-api-paste.ini` (plus `policy.yaml` / `logging.conf` when applicable) into an immutable content-addressed ConfigMap. An invalid projection keeps the live Deployment's last-good names instead of re-rendering. **Reports through `SecretsReady`** (failure reason `ConfigError`) | `SecretsReady` |
| Database | Provisions/migrates the schema (MariaDB gate, `Database`/`User`/`Grant`, one `glance-manage db sync` Job); a release bump instead runs the shared expand-migrate-contract flow; promotes `installedRelease` | `DatabaseReady` |
| Deployment | Ensures the API Deployment (both launch modes), Service (port 9292), and PDB; stamps `status.endpoint`. Mid-upgrade it flips the `RollingUpdate` phase to `Contracting` once the rolled-out Deployment reports ready | `DeploymentReady` |
| HTTPRoute | Full `spec.gateway` lifecycle; reflects the Gateway's Accepted condition | `HTTPRouteReady` |
| HealthCheck | HTTP GET of the cluster-local `/healthcheck` through the shared TTL probe cache | `GlanceAPIReady` |
| HPA | Creates/deletes the HorizontalPodAutoscaler | `HPAReady` |
| NetworkPolicy | Creates/deletes the NetworkPolicy (auto-derived DB/cache/S3 egress); refuses an empty ingress list (fail-closed) | `NetworkPolicyReady` |

`DBConnectionSecret` and `Config` deliberately reuse `SecretsReady` rather than
a dedicated `ConfigReady` condition: both produce Secret/ConfigMap artefacts
that gate the same downstream graph, so a distinct `sub_reconciler` label on the
error counter disambiguates them during triage while the status contract stays
minimal.

## Conditions

The aggregate `Ready` condition is `True` (reason `AllReady`) exactly when all
eight sub-conditions are `True`; otherwise `False` (`NotAllReady`).

| Type | True reasons | False reasons |
| --- | --- | --- |
| `SecretsReady` | `SecretsAvailable` | `SecretStoreNotReady`, `WaitingForDBCredentials`, `WaitingForServiceUserCredentials`, `ConfigError` |
| `BackendsReady` | `AllBackendsProjected` | `WaitingForBackends`, `NoDefaultBackend` |
| `DatabaseReady` | `DatabaseSynced` | `ClusterNotReady`, `WaitingForDatabase`, `DBSyncFailed`, `DBSyncInProgress`, `WaitingForBackends`, `ImageReleaseMismatch`, `VersionParseError`, `DowngradeNotSupported`, `UpgradePathInvalid`, `UpgradeTargetChanged`, `ExpandInProgress`, `MigrateInProgress`, `UpgradeRollingUpdate`, `ContractInProgress`, `ExpandFailed`, `MigrateFailed`, `ContractFailed` |
| `DeploymentReady` | `DeploymentReady` | `WaitingForDeployment`, `WaitingForBackends` |
| `GlanceAPIReady` | `APIHealthy` | `APIUnhealthy`, `EndpointNotReady`, `HealthCheckTimeout`, `ConnectionFailed`, `HealthCheckFailed` |
| `HPAReady` | `HPAReady`, `HPANotRequired` | — (errors propagate) |
| `NetworkPolicyReady` | `NetworkPolicyReady`, `NetworkPolicyNotRequired` | — (errors propagate) |
| `HTTPRouteReady` | `HTTPRouteAccepted`, `HTTPRouteNotRequired` | `HTTPRouteNotAccepted`, `GatewayAPINotInstalled` |

Both `DatabaseReady` and `DeploymentReady` carry a `WaitingForBackends` reason:
the schema cannot be `db sync`-ed and the Deployment cannot be created until a
ready default backend has produced a rendered config to run against.

## Launch modes

`spec.openStackRelease` selects the launch mode at the `2026.1` boundary: a
release `>= 2026.1` launches under uWSGI, anything below runs the eventlet
`glance-api` server. An unparseable release (a CR that bypassed the CRD pattern)
falls back to the eventlet mode. Both modes load the **same two** oslo.config
`--config-dir` roots — the immutable config ConfigMap
(`/etc/glance/glance-api.conf.d/`) and the backends Secret
(`/etc/glance/backends.conf.d/`).

- **uWSGI (`>= 2026.1`).** The command is `uwsgi --http :9292
  --http-auto-chunked --http-chunked-input …` — the two chunked flags are always
  on because Glance streams image bodies with chunked transfer encoding. Keep-alive
  (`--http-keepalive`, and `--http-keepalive-timeout` when set) and `--harakiri`
  are emitted from the `spec.apiServer.uwsgi` knobs. uWSGI loads the WSGI app
  through `--wsgi-file /var/lib/openstack/bin/glance-wsgi-api` (the image-shipped
  shim), passing both `--config-dir` roots via `--pyargv`, because glance's stock
  WSGI module ignores `sys.argv`.
- **eventlet (`< 2026.1`).** The command is `glance-api --config-dir
  /etc/glance/glance-api.conf.d/ --config-dir /etc/glance/backends.conf.d/`; the
  worker count comes from `[DEFAULT] workers` in the config (from
  `spec.apiServer.workers`), not the CLI.

Setting the wrong knob for the active mode is legal but inert; the validating
webhook returns an admission warning rather than rejecting.

## Backends aggregation

The Backends step lists the attached `GlanceBackend`s (via the
`spec.glanceRef.name` field index), sorts them by name for deterministic output,
and renders one `[<name>]` store section per **credential-ready** backend into a
content-hashed backends Secret (rolled on any content change, 3 historical
retained). It gates each backend on `CredentialsReady==True` — never the
aggregate `Ready`, which also requires `ConfigProjected` and would deadlock,
since `ConfigProjected` only turns True after this step projects the backend.

- **Exactly-one-Ready-default rule.** Zero or more than one credential-ready
  default is an invalid projection (`BackendsReady=False / NoDefaultBackend`):
  nothing is re-rendered and the config step retains last-good.
- **Per-backend fault isolation.** A backend whose credentials Secret is
  missing/unreadable, or whose rendered values carry a control character, is
  skipped with a `GlanceBackendSkipped` Warning event on the Glance CR while the
  healthy siblings keep projecting.

## Database

The Database step runs the shared provisioning + sync flow: a MariaDB cluster
gate and `Database`/`User`/`Grant` in managed mode (a no-op in brownfield),
then a single `{name}-db-sync` Job running `glance-manage db sync` followed by
`db load_metadefs`. Because Glance's migrations apply in one idempotent pass,
there is **no schema-check Job**, so, unlike Keystone, `SchemaDriftDetected`
never fires for Glance. On fresh installs and patch bumps `installedRelease` is
promoted to `spec.openStackRelease` on Job success.

A release transition (a `spec.openStackRelease` bump with the image in lockstep)
instead dispatches to the shared expand-migrate-contract flow
(`internal/common/database`), the same phase machine Keystone runs. The step
validates the path against `installedRelease`, rejecting a downgrade or a
non-sequential jump with `VersionParseError`, `DowngradeNotSupported`, or
`UpgradePathInvalid`, then walks `Expanding → Migrating → RollingUpdate →
Contracting` with `glance-manage db expand|migrate|contract` phase Jobs on the
new image. The outgoing pods keep serving the expanded schema across the
rollout, so the API stays available, and `installedRelease` is promoted only
after contract completes. The Deployment step owns the `RollingUpdate →
Contracting` flip: once `reconcileDeployment` sees the rolled-out Deployment
ready, it advances the phase to `Contracting` and requeues so the contract Job
runs. See the [Glance Upgrade Flow](./glance-upgrade-flow.md) for the phase
table, condition reasons, events, and abort semantics.

## Requeue semantics

| Interval | Used by |
| --- | --- |
| 10s | Deployment readiness polling, HTTPRoute acceptance, health-check retry |
| 15s | ESO secret-gate polling (Secrets, DBConnectionSecret; the GlanceBackend controller's credential/projection backstop) |
| 30s | MariaDB / db-sync / ready-default-backend database wait |
| 30s TTL | Health-probe cache (a passing `/healthcheck` probe is reused within the TTL) |

## Rotation and deletion

- **Credential rotation** happens at the OpenBao source. The service-user
  password (consumed via `OS_KEYSTONE_AUTHTOKEN__PASSWORD`) and the assembled
  DSN (consumed via `OS_DATABASE__CONNECTION`) are both env-var-delivered, so a
  restart is required for a rotation to take effect. The Secrets and
  DBConnectionSecret steps digest each value into a pod-template annotation —
  `glance.c5c3.io/authtoken-hash` and `glance.c5c3.io/db-connection-hash` — so a
  changed digest rolls the Deployment.
- **Deletion** runs the `glance.openstack.c5c3.io/finalizer`: it issues Delete
  on the MariaDB `Database`/`User`/`Grant` CRs before the owner-ref chain
  disappears, emitting `FinalizingDatabase` while cleanup remains and
  `DatabaseFinalized` when the finalizer is released. Every other owned resource
  is namespace-scoped with a controller owner reference, so Kubernetes garbage
  collection reclaims it. The `GlanceBackend` controller has no finalizer: the
  operator provisions no S3 resources, so there is nothing to clean up remotely.
  Deletion still **de-registers the store** — the bucket and its objects are
  external state, and image location rows referencing that store id are left
  dangling, so those images become undownloadable until a backend with the same
  name is re-attached.

## Watches

The Glance controller `Owns` its Deployment, Service, ConfigMap, Secret,
PodDisruptionBudget, HorizontalPodAutoscaler, NetworkPolicy, and — for the
db-sync migration — Job; the HTTPRoute is added to the `Owns` set only when the
Gateway API CRD is installed (otherwise `spec.gateway` surfaces
`HTTPRouteReady=False / GatewayAPINotInstalled` instead of crashing the
controller). Beyond the owned set it watches:

- **Secrets**, mapped to the Glance CRs that reference them directly (via the
  `spec.serviceUser.secretRef.name` / `spec.database.secretRef.name` field
  index) or through an attached backend's S3 credentials Secret (via the backend
  secret-name index).
- **GlanceBackends**, mapped to their parent Glance with no generation predicate
  — a backend's `CredentialsReady` flip is exactly what wakes the aggregation.
- **MariaDB** clusters referenced by `spec.database.clusterRef`, so an upstream
  database outage reflects in `DatabaseReady` without waiting for a periodic
  requeue.
- Both the cluster-scoped `ClusterSecretStore` and the namespaced `SecretStore`
  a Glance can select, so a store-backend outage reflects in `SecretsReady`.

The Glance reconciler registers **both** controllers' field indexes at setup
(`spec.glanceRef.name` and the secret-name indexes), so it must be set up before
the `GlanceBackend` controller. The `GlanceBackend` controller in turn watches
its parent Glance (no generation predicate: the store config landing in the
Deployment is the wake signal its `ConfigProjected` gate waits on) and is the
**single writer** of `GlanceBackend` status.
