---
title: Nova Reconciler Architecture
quadrant: operator
---

# Nova Reconciler Architecture

The Nova controller runs the shared table-driven pipeline
(`internal/common/reconcile`) with twelve sequential sub-reconcilers and a
parallel group of six. Every step is instrumented under the `nova_operator`
metrics prefix, and the first step to return a non-zero result or an error
short-circuits the chain. Conditions and the requeue are persisted on every exit
path through the shared status skeleton.

Two Prometheus vectors cover the pipeline
(`nova_operator_reconcile_duration_seconds`,
`nova_operator_reconcile_errors_total`, the latter labelled by `sub_reconciler`
and `condition_type`). Two per-CR collector pairs cover the Jobs:
`nova_operator_db_sync_total` and `nova_operator_db_sync_duration_seconds` for
the schema migrations and the three upgrade phases, and
`nova_operator_db_archive_*` for the recurring archive. Each pair is labelled by
`nova` and `namespace`, and the counters carry the terminal `result`. The
Grafana dashboard `operators/nova/dashboards/nova-operator.json` reads them
under the uid `nova-operator`.

Two controllers run in the binary. The pipeline below reconciles the `Nova`
kind; the [NovaCompute](#novacompute) controller runs the second kind, a node
pool of a compute cluster, on a pipeline of its own, and shares the
instrumenter and the metric vectors with it.

## Pipeline

```text
Secrets ──► DBConnectionSecrets ──► TransportURLSecret ──► Config ──► ComputeConfig ──►
Database ──► Conductor ──► Scheduler ──► Metadata ──► ConsoleProxy ──► Deployment ──► DBArchive ──► ┬─ HTTPRoute
                                                                                                    ├─ MetadataHTTPRoute
                                                                                                    ├─ ConsoleHTTPRoute
                                                                                                    ├─ HealthCheck
                                                                                                    ├─ HPA
                                                                                                    └─ NetworkPolicy  (parallel)
```

| Step | What it does | Condition |
| --- | --- | --- |
| Secrets | Gates on the selected secret store (`spec.secretStoreRef`, default `openbao-cluster-store`), then on the two database credential Secrets, the service-user password, the metadata shared secret, and, on a TLS bus, the broker CA bundle; reads the three values the later steps need and digests two of them. A metadata shared secret whose key exists but is empty waits like a missing one: nova would verify instance signatures against an empty key | `SecretsReady` |
| DBConnectionSecrets | Materializes both pymysql DSNs into `{name}-api-db-connection` and `{name}-db-connection` and digests each. The `nova_api` half runs first and returns on its own requeue, so a cell Secret is never derived while the `nova_api` credentials are missing | `SecretsReady` |
| TransportURLSecret | Materializes the `rabbit://` URL into `{name}-transport-url`, digests it, and resolves the broker port the NetworkPolicy member opens and the two bus workloads probe. A brownfield URL without an explicit port or with an IPv6 literal host sets `SecretsReady` False with the reason `TransportURLRejected` before the derived Secret is written, so a restarting pod keeps the last accepted URL: nova expands the cell mapping's `{hostname}:{port}` template from it | `SecretsReady` |
| Config | Renders `nova.conf` and the four role overlays (plus `logging.ini` under json logging) into an immutable content-addressed ConfigMap, and maintains the informational `ExtraConfigHealthy` condition. A section carrying a control character is not re-rendered: the step returns the artefacts the live API Deployment currently mounts, so the running pods keep their last-good config. Failures report through `SecretsReady` with the reason `ConfigError` | `SecretsReady` |
| ComputeConfig | Applies `{name}-compute-config`, the contract a compute node joins on, and stamps `status.computeConfigSecretRef`. It runs here because four of its keys are values the three steps above read. A fragment carrying a control character is not written: the published Secret stays as it was and the condition goes False | `ComputeConfigReady` |
| Database | Provisions both schemas and cell0, gates the requested release against the installed one, runs the migration Jobs, and promotes `installedRelease`; a release bump instead runs the expand-migrate-contract flow | `DatabaseReady` |
| Conductor | Ensures the `{name}-conductor` Deployment. No Service, no PodDisruptionBudget, no autoscaling: the conductor takes its work off the bus | `ConductorReady` |
| Scheduler | Ensures the `{name}-scheduler` Deployment, under the same contract | `SchedulerReady` |
| Metadata | Ensures the `{name}-metadata` Deployment and its Service on port 8775 | `MetadataReady` |
| ConsoleProxy | Ensures the `{name}-novncproxy` Deployment and its Service on port 6080, or deletes both when the proxy is switched off | `ConsoleProxyReady` |
| Deployment | Ensures the API Deployment, its Service (port 8774) and the PDB, and stamps `status.endpoint`. Mid-upgrade it flips the `RollingUpdate` phase to `Contracting` once every role has converged | `DeploymentReady` |
| DBArchive | Projects the `{name}-db-archive` CronJob and reports the newest terminal run it spawned. It runs before the parallel group, not inside it: all it needs is the rendered config the Config step produced | `DBArchiveReady` |
| HTTPRoute | Full `spec.gateway` lifecycle; reflects the Gateway's Accepted condition | `HTTPRouteReady` |
| MetadataHTTPRoute | The same for `spec.metadata.gateway` | `MetadataHTTPRouteReady` |
| ConsoleHTTPRoute | The same for `spec.consoleProxy.gateway`. A disabled proxy is handled as an absent block: the route is deleted in the same pass that deletes the proxy | `ConsoleHTTPRouteReady` |
| HealthCheck | HTTP GET of the cluster-local API root through the shared TTL probe cache. The compute API ships no `/healthcheck` route, so `/`, the version document it answers without a token, is what a 2xx is read off | `NovaAPIReady` |
| HPA | Creates and deletes the HorizontalPodAutoscaler of the API Deployment | `HPAReady` |
| NetworkPolicy | Creates and deletes the NetworkPolicy (auto-derived egress, including the broker port) and, while the console proxy is enabled, the `{name}-novncproxy` policy opening the proxy's VNC egress; refuses an empty ingress list (fail-closed) | `NetworkPolicyReady` |

`DBConnectionSecrets`, `TransportURLSecret` and `Config` reuse `SecretsReady`
rather than a dedicated `ConfigReady` condition: all three produce artefacts
that gate the same downstream graph, so a distinct `sub_reconciler` label on the
error counter disambiguates them during triage while the status contract stays
minimal.

The four workload steps behind the Database step run in pipeline order, and
that order is what a release upgrade rolls in: conductor, scheduler, metadata,
console proxy, then the API. Outside an upgrade none of the four short-circuits
on a workload that is still starting: each sets its condition and returns a zero
result, so a metadata API mid-rollout does not keep the API step from running,
and the `Owns(Deployment)` watch re-enqueues the CR as the rollout progresses.

The six members of the parallel group have no inter-dependency. Each operates on
its own copy of the CR and sets exactly one condition, and the group merges the
conditions back before the status write.

## Conditions

The aggregate `Ready` is True (reason `AllReady`) exactly when all fifteen
sub-conditions are True; otherwise False (`NotAllReady`). `ExtraConfigHealthy`
is kept outside the aggregate: it reports on an overlay the user owns and must
not depool a Nova whose API serves fine.

| Type | True reasons | False reasons |
| --- | --- | --- |
| `SecretsReady` | `SecretsAvailable` | `TargetClusterUnavailable`, `SecretStoreNotReady`, `WaitingForDBCredentials`, `WaitingForServiceUserCredentials`, `WaitingForMetadataSharedSecret`, `MetadataSharedSecretEmpty`, `WaitingForMessagingCA`, `WaitingForMessagingCredentials`, `TransportURLRejected`, `ConfigError` |
| `ComputeConfigReady` | `ComputeConfigPublished` | `ComputeConfigError` |
| `DatabaseReady` | `DatabaseSynced` | `ClusterNotReady`, `WaitingForDatabase`, `WaitingForConfig`, `ImageReleaseMismatch`, `DBSyncFailed`, `DBSyncInProgress`, `VersionParseError`, `DowngradeNotSupported`, `UpgradePathInvalid`, `UpgradeTargetChanged`, `ExpandInProgress`, `MigrateInProgress`, `UpgradeRollingUpdate`, `ContractInProgress`, `ExpandFailed`, `MigrateFailed`, `ContractFailed` |
| `ConductorReady` | `ConductorReady` | `WaitingForConductor` |
| `SchedulerReady` | `SchedulerReady` | `WaitingForScheduler` |
| `MetadataReady` | `MetadataReady` | `WaitingForMetadata` |
| `ConsoleProxyReady` | `ConsoleProxyReady`, `ConsoleProxyDisabled` | `WaitingForConsoleProxy` |
| `DeploymentReady` | `DeploymentReady` | `WaitingForDeployment` |
| `DBArchiveReady` | `DBArchiveScheduled`, `DBArchiveSuspended` | `DBArchiveJobFailed` |
| `NovaAPIReady` | `APIHealthy` | `APIUnhealthy`, `EndpointNotReady`, `HealthCheckTimeout`, `ConnectionFailed`, `HealthCheckFailed` |
| `HPAReady` | `HPAReady`, `HPANotRequired` | errors propagate |
| `NetworkPolicyReady` | `NetworkPolicyReady`, `NetworkPolicyNotRequired` | errors propagate |
| `HTTPRouteReady` | `HTTPRouteAccepted`, `HTTPRouteNotRequired` | `HTTPRouteNotAccepted`, `GatewayAPINotInstalled`, `CapabilityProbeFailed` |
| `MetadataHTTPRouteReady` | the same vocabulary | the same vocabulary |
| `ConsoleHTTPRouteReady` | the same vocabulary | the same vocabulary |
| `ExtraConfigHealthy` | `NoOwnedKeysOverridden` | `OwnedKeysOverridden` |

Two True reasons describe an absence.
`ConsoleProxyDisabled` says `spec.consoleProxy.enabled` is false, which is a
posture: the API serves and instances boot, they are only offered no console.
`DBArchiveSuspended` is the second: a paused archive is an operator's decision,
so the condition stays True while the metric simply stops incrementing. The
three route conditions add a third shape, `HTTPRouteNotRequired`, for a gateway
block that is not set.

`TargetClusterUnavailable` is set ahead of every sub-reconciler, when
`spec.targetClusterRef` names a target cluster that is not registered or no
longer resolves. The CR requeues after 15 seconds and acquires no finalizer, and
nothing is created on any cluster. See [Target Clusters](../target-clusters.md).

## Database

The Database step runs the shared provisioning flow twice, once per block: the
`nova_api` schema under the instance name `{name}-api`, then the cell schema
under the bare `{name}`. Each pass gates on its MariaDB cluster, ensures the
`Database`, `User` and `Grant` in managed mode, and sizes the SQL user's
`max_user_connections` for the CR's own topology, because the mariadb-operator
default of 10 is exceeded by the default topology before a single request is
served.

cell0 travels as an additional schema of the cell block, not as a block of its
own. `{database}_cell0` gets a `Database` CR and, in `Static` mode, a
second `Grant` on the same user, in the order primary Database, additional
Database, User with primary Grant, additional Grant, so a grant is never applied
before its schema and its user exist. One user for both is the point:
`nova-manage` maps cell0 with the credentials it already runs with, and
`nova-api` reads cell0 on every instance list, so a separate user would turn a
routine list into an error.

The step then waits for a rendered config, because the migration Jobs mount the
config ConfigMap as their whole config directory and an empty name would render
a volume the API server rejects (`DatabaseReady=False`, reason
`WaitingForConfig`).

Before either path advances release tracking, the step compares a tag-pinned
`spec.image` against `spec.openStackRelease`. The two fields are separate on
purpose, so that a digest-pinned image still resolves a schema, and nothing
else enforces that they agree: an image whose tag names a different release
would run the wrong `nova-manage` binary against a schema already at its own
head, and the phase Jobs would exit 0 as no-ops while `installedRelease` was
promoted to a release the pods do not run. A mismatch sets `DatabaseReady=False`
under `ImageReleaseMismatch` and requeues. A digest-pinned image and an
unparseable tag carry no comparable release and are left to the explicit
`spec.openStackRelease` declaration; a patch suffix such as `2026.1-p1` still
matches `2026.1`.

### The cells sequence

Steady state is a single `{name}-db-sync` Job. It migrates both schemas and maps
the cells in one pass, which is why there is no separate schema-check Job and
`SchemaDriftDetected` never fires for Nova. Eight commands run under
`/bin/sh -eu -c`, chained on success:

1. `nova-manage api_db sync` migrates the `nova_api` schema.
2. `nova-manage cell_v2 map_cell0 --database_connection '<template>'` maps the
   holding pen. It runs before the cell schema is migrated, because
   `nova-manage` reads the cell map to find the schema.
3. `nova-manage cell_v2 list_cells` lists what is mapped. The table is captured
   before it is read, so a failed listing stops the script instead of reading
   as an empty table, which would create the duplicate cell the guard exists to
   prevent.
4. `awk -F'|' …`, the guard. It succeeds when a row's name is `cell1` or its
   database connection names the cell schema, compared on the URL path with the
   query stripped. A second `create_cell` with the same templates neither fails
   nor updates the existing row: nova compares the stored, expanded URLs against
   the templates, never finds them equal, and maps a second cell, and from then
   on an instance boots into whichever of the two the scheduler was handed. The
   guard reads whole fields rather than grepping the table, because the expanded
   URLs carry the user, the host and the vhost, any of which may contain
   `cell1` as a word; and the schema match keeps a brownfield cell mapped under
   another name from being mapped a second time.
5. `nova-manage cell_v2 create_cell --name cell1 --transport-url '<template>'
   --database_connection '<template>'` runs only when the guard found no such
   cell.
6. `nova-manage db sync` migrates the cell schema, against a cell the API can
   already address.
7. `nova-manage cell_v2 list_cells` runs once more, captured the same way, and
   is piped into
8. `awk -F'|' …` which turns the ASCII table into one `<name>=<uuid>` line per
   cell and writes it to `/dev/termination-log`. A failed final listing fails
   the Job rather than completing it with an empty report.

Both the transport URL and the database connection travel as templates
(`{scheme}://{username}:{password}@{hostname}:{port}/…`) that nova expands from
the connection the Job already runs with, so the operator never assembles a URL
carrying a password. The cell schema is the only part that differs per cell, and
it is interpolated into a single-quoted shell word; neither name can carry a
quote, because the shared provisioning flow enforces `^[A-Za-z0-9_]{1,64}$` on
the primary schema and on cell0's derived name before anything is applied.

The termination log is what `status.cells` is built from. nova generates a
cell's UUID at map time, and a per-cell `nova-manage cell_v2` command addresses
the cell by UUID, so the report is the only place those identifiers can be read
back from. The message is read off the Job's pod that succeeded, because a Job
runs each retry as a pod of its own and a failed attempt wrote no report; a Job
without a succeeded pod is read off its newest one. The pods are listed through
the uncached API reader of the cluster that holds them: the operator reads one
pod's message once per Job and never watches pods, and a read through the cached
client would start an informer over every pod of the cluster that the
operator's RBAC does not allow to watch. Every line is matched against
`^([A-Za-z0-9_-]+)=([0-9a-f-]{36})$`,
so stray `nova-manage` output on the same stream is dropped instead of being
published as a cell. A report the operator cannot read leaves `status.cells` as
it is and emits a Normal `CellsReportUnavailable` event: the cells are mapped
either way, and dropping a published UUID would be worse than reporting a stale
one. `installedRelease` is promoted on Job success.

### Release upgrade

A `spec.openStackRelease` bump with the image in lockstep dispatches to the
shared expand-migrate-contract flow (`internal/common/database`), the same phase
machine Keystone and Cinder run. The step validates the path against
`installedRelease` and rejects a downgrade or a non-sequential jump
(`VersionParseError`, `DowngradeNotSupported`, `UpgradePathInvalid`), then walks
four phases. Nova splits the work differently from the phase names.

| Phase | What runs | Job |
| --- | --- | --- |
| `Expanding` | `nova-status upgrade check`, then `nova-manage api_db sync` and `nova-manage db sync`. Both migrations are additive, and the old release keeps running against the widened schema | `{name}-db-expand` |
| `Migrating` | `nova-manage cell_v2 list_cells`. Nova has no migrate verb of its own, so the phase is a read that proves the new code can address the `nova_api` schema the expand phase just migrated | `{name}-db-migrate` |
| `RollingUpdate` | No Job. The conductor, the scheduler, the metadata API, the console proxy and the API roll onto the new image, in that order, because that is the order the pipeline ensures them in | |
| `Contracting` | `nova-manage db online_data_migrations --max-count 1000` in a loop, the backfill the new schema needs, which the completed rollout makes safe; first for the cell schema, then for cell0 | `{name}-db-contract` |

Both phase scripts read an exit code as a severity rather than as a success
flag, the contract loop runs a second time against cell0, and the flip from
`RollingUpdate` to `Contracting` waits on every rendered role, not on the API
alone. After the contract phase, promoting `installedRelease` rolls the four
non-API roles once more through the `nova.c5c3.io/installed-release`
annotation. The operator-facing walkthrough of the flow, with the exit codes,
the cell0 pass, the flip gate, the second roll and the abort recipe, is
[Nova Upgrade Flow](./nova-upgrade-flow.md).

## DBArchive

The DBArchive step projects the `{name}-db-archive` CronJob on every pass with
the settings `effectiveDBArchive` resolves, and takes its run visibility from
the Jobs the CronJob spawned instead of from the CronJob object. It lists the
Jobs carrying this archive's labels, keeps the ones the CronJob controls, and
reports on the newest that reached a terminal state.

A run is a loop of batches. Each batch is `nova-manage db archive_deleted_rows
--all-cells --max_rows "$MAX_ROWS"`, with `--before <date> --task-log` appended
only while `RETENTION_DAYS` is set. The date is computed in the script with
`date -u -d "-${RETENTION_DAYS} days" +%Y-%m-%d`, and the argument is left
unquoted on purpose so an unset window adds no argument at all rather than an
empty one. `--task-log` travels with the date and only with it: `task_log` rows
are never soft-deleted, so without a date the flag would move the current audit
period out from under the `os-instance_usage_audit_log` API. The run passes
neither `--purge`, which would empty the shadow tables the archive promises to
keep, nor `--until-complete`.

`archive_deleted_rows` answers with a severity: 0 means nothing was archived, 1
means rows were, 2 means the row cap was invalid, 3 means no API database
connection was configured, and 4 means `--before` could not be parsed. The loop
starts another batch after a 1, pausing `SLEEP` seconds first, and stops on a 0,
on anything above 1, or once its 3000-second budget is spent; `test "$rc" -le 1`
then fails the Job only when the last batch failed. The loop is the script's
rather than `nova-manage`'s own `--until-complete`, which runs until the backlog
is empty however long that takes: on a neglected database that outlasts the
active deadline below and fails every run. The budget stops starting batches
ten minutes before the deadline, so a batch in flight is never killed half done,
and the next run continues where this one stopped.

The pod runs with `fsGroup` set to the openstack GID, like the long-lived
workloads. The database TLS files are projected at mode 0400, and without the
group owning them `nova-manage` could not read the client key it connects
with.

`ConcurrencyPolicy: Forbid` keeps a run that outlasts its interval from being
overtaken by the next firing, which is what a first pass over a long backlog
invites: two archives moving the same rows contend on the same tables, and on
Galera the later write-set fails certification. `activeDeadlineSeconds: 3600`
catches a run that wedges instead of failing, an unschedulable pod or a
`nova-manage` blocked on a database lock. Either reaches a terminal `Failed`
state within the hour and surfaces as `DBArchiveReady=False` under
`DBArchiveJobFailed` with a matching Warning event. A later successful run flips
it back to `True`.

The suspend arm outranks the failed arm. A suspended CronJob spawns no successor
to supersede a failed run, so the failed Job stays the newest terminal one for
good (the default `failedJobsHistoryLimit` retains it), and a `JobFailed` arm
that won here would pin `DBArchiveReady` False, and with it the aggregate
`Ready`, until someone deleted the Job by hand. The failure is still named in the
suspended condition's message; it is just not the state the CR is in.

See [DBArchiveSpec](./nova-crd.md#dbarchivespec) for the settings and the
defaults they resolve to.

## Requeue semantics

| Interval | Constant | Used by |
| --- | --- | --- |
| 1s | `RequeueNextPass` | After a finalizer add, and after the `RollingUpdate` to `Contracting` flip |
| 10s | `RequeueDeploymentPolling` | API Deployment readiness polling, the rollout wait of every workload step during an upgrade, and the wait for a Gateway to report Accepted |
| 15s | `RequeueSecretPolling` | The secret-store and credential gates, both derived DB-connection steps, the transport-URL step, and an unresolvable target cluster |
| 30s | `RequeueDatabaseWait` | The MariaDB gate of both blocks, the db-sync wait, the config wait, and the finalizer's cleanup wait |
| 30s | `RequeueUpgradeWait` | The expand, migrate and contract phase Jobs |
| 10s / 30s TTL | health-check retry, probe cache | A failed probe of the API root retries; a passing one is reused within the TTL |

## Watches and indexes

The Nova controller `Owns` its Deployment, Service, ConfigMap, Secret,
PodDisruptionBudget, HorizontalPodAutoscaler, NetworkPolicy, Job and CronJob.
The CronJob is in that list for its `status.active`: the list changes as each
spawned run starts and finishes, which is what wakes the reconcile that
refreshes `DBArchiveReady`. The HTTPRoute joins the set only when the Gateway
API CRD is installed on the management cluster; otherwise a gateway block
surfaces `HTTPRouteReady=False / GatewayAPINotInstalled` instead of crashing the
controller. The CR's own watch filters status-only updates, so a status write
does not re-wake the controller. A CR that names a target cluster has the same
children watched once more on the clusters it can project onto, by ownership
label rather than by owner reference, because an owner reference does not cross
a cluster boundary.

Beyond the owned set it watches:

- Secrets, mapped to the Nova CRs that reference them by name through the
  `spec.secretRefs.name` field index or own them through an owner reference. The
  index holds the deduplicated union of every Secret name a CR references: both
  database credentials, the service-user password, the metadata shared secret,
  the CA bundle and client certificate of each database block whose `tls` is
  enabled, the brownfield `spec.messaging.secretRef`, and the broker CA bundle.
  A disabled TLS block contributes nothing, so a name is indexed only while the
  connection actually reads it. The owner-reference leg is what reaches the four
  derived Secrets, which ESO does not own.
- MariaDB clusters, through one leg carrying both `clusterRef` fields: a Nova is
  enqueued when either block names the cluster the event came from, and once
  rather than twice when both name the same one.
- Both the cluster-scoped `ClusterSecretStore` and the namespaced `SecretStore`
  a Nova can select, so a store-backend outage reflects in `SecretsReady`.

The Secret index is registered on the local field indexer, never on the fleet: it indexes a CR kind, which exists on the management cluster alone, and
registering it on the fleet would fail the engagement of every target cluster.

## Rotation and deletion

Credential rotation happens at the OpenBao source. Both DSNs, the service-user
password, the transport URL and the metadata shared secret are all
environment-delivered, so a restart is required for a rotation to take effect.
The credential steps digest each value into a pod-template annotation
(`nova.c5c3.io/api-db-connection-hash`, `nova.c5c3.io/db-connection-hash`,
`nova.c5c3.io/authtoken-hash`, `nova.c5c3.io/transport-url-hash` and, on the
metadata pods, `nova.c5c3.io/metadata-secret-hash`), so a changed digest rolls
the workloads that carry it. Each is stamped only while it is non-empty, so a
pass that stopped at a credential gate leaves the annotation alone instead of
clearing it and rolling every pod.

Deletion runs the `nova.openstack.c5c3.io/finalizer`. It issues Delete on the
MariaDB `Database`/`User`/`Grant` CRs of both schemas before the owner-ref chain
disappears, under the names they were provisioned with: the `nova_api` block
under `{name}-api`, the cell block under `{name}`, with cell0 reached through the
cell key as an additional schema, so it has no key of its own. Whether anything
was still live is observed before the Delete is issued, because a Delete flips
the deletion timestamp and a post-Delete check would always report none-live and
release immediately. While either schema still had a live CR the finalizer is
held for one more pass and a `FinalizingDatabase` event is emitted; once neither
does, `DatabaseFinalized` is emitted, the per-CR metrics are dropped
(`metrics.DeleteForNova`), the health-probe cache entry is evicted, and the
finalizer is released.

A CR that names a target cluster carries a second finalizer. After the named
MariaDB cleanup, a label-selected sweep deletes everything the CR projected onto
that cluster: the Deployments, Services, ConfigMaps, Secrets, Jobs, the CronJob,
the PodDisruptionBudget, the HorizontalPodAutoscaler, the NetworkPolicy, the
HTTPRoutes and the three MariaDB kinds. The sweep runs after the named cleanup,
never before it, because that flow waits one pass on the CRs it deletes by name
and a sweep running first would delete them out from under it. A cluster that has
not resolved for the whole abandon window (five minutes) is the exception: its
children cannot be reached, a `RemoteChildrenAbandoned` Warning names what stays
behind, and the finalizer is released so the CR leaves etcd instead of hanging in
Terminating.

Every other owned resource is namespace-scoped with a controller owner
reference, so Kubernetes garbage collection reclaims it.

## NovaCompute

A [NovaCompute](./novacompute-crd.md) runs `nova-compute` on one node pool. Its
controller (`novacompute_controller.go`, recorder `novacompute-controller`)
runs six sequential steps under the same `nova_operator` instrumenter. Each
owns one condition, and the aggregate `Ready` is True only when all six are.
`ExtraConfigHealthy` stays out of it, as it does for the Nova.

### NovaCompute pipeline

```text
NovaRef ──► Nodes ──► PoolConfig ──► DaemonSet ──► Aggregates ──► Services
```

| Step | Function | Condition |
| --- | --- | --- |
| `NovaRef` | `reconcileNovaComputeNova` | `NovaReady` |
| `Nodes` | `reconcileNovaComputeNodes` | `NodesReady` |
| `PoolConfig` | `reconcileNovaComputeConfig` | `ConfigReady` |
| `DaemonSet` | `reconcileNovaComputeDaemonSet` | `DaemonSetReady` |
| `Aggregates` | `reconcileNovaComputeAggregates` | `AggregatesReady` |
| `Services` | `reconcileNovaComputeServices` | `ServicesReady` |

A step that sets its condition False for a reason that is not a wait on an input
returns a zero result, so the later steps still run: `NoMatchingNodes`,
`NodeConflict`, `DaemonSetProgressing`, `NodesWithoutZone` and
`AggregateZoneMismatch`. An empty selection therefore still drains the nodes the
pool held, and so does a rollout one NotReady node keeps from finishing. The `computeapi`
client the last two steps call Keystone and Nova through is built once per pass,
so a pass requests one token.

Every CR carries the finalizer `nova.openstack.c5c3.io/compute-drain`, and a CR
naming a target cluster carries the remote-children finalizer too. Both go on
before the pipeline runs.

### reconcileNovaComputeNova

Reads the Nova `spec.novaRef` names in the CR's namespace, on the management
cluster. In order it waits for the Nova (`NovaNotFound`), its installed release
(`WaitingForInstalledRelease`), its published contract
(`ComputeConfigNotPublished`), its cluster (`TargetClusterUnavailable`) and its
service-user password (`WaitingForServiceUserSecret`), each requeued after 15
seconds. The password is read on the cluster the Nova is placed on, and a placed
Nova is called through that cluster's service proxy. The step hands the later
ones the effective image, `spec.image` or
`ghcr.io/c5c3/nova-compute:<status.installedRelease>`, the contract Secret name,
and the Keystone and Nova URLs with the service user's credentials.

### reconcileNovaComputeNodes

Lists the selected Nodes through an uncached reader (the target cluster's own,
or the manager's API reader for a local pool), so no cluster-wide Node informer
starts on an install whose RBAC is namespace-scoped. It reads their metadata
only: a node's name and labels are all the rules use. A deleting pool lists
nothing. Held nodes the list no longer returns are read one by one, and a gone
node counts as leaving. The rivals are the other NovaComputes of the same Nova
on the same cluster, found through the `spec.novaRef.name` field index. The
phase rules are listed on the [CRD page](./novacompute-crd.md#node-phases). A
new conflict records a Warning `NodeConflict` event once. A 403 reports
`NodesForbidden` and requeues after 15 seconds; any other read error is
`NodeListError` and returns the error.

### reconcileNovaComputeConfig

Reads the contract Secret `<nova>-compute-config` in the CR's namespace on the
pool's cluster, waits for it (`WaitingForComputeConfig`) and for its three keys
(`ComputeConfigIncomplete`), renders `compute-pool.conf` with `spec.extraConfig`
merged over it into an immutable `{name}-config-<hash>` ConfigMap, prunes the
history to the three newest, and hashes the Secret's key/value pairs for the
pod template.

### reconcileNovaComputeDaemonSet

Builds the node affinity from the selector and the nodes the Nodes step
excluded and held, and applies `{name}-nova-compute`. With no affinity term (a
deleting pool holding no Draining node) it deletes the DaemonSet it owns
instead, which releases the last pod. It mirrors `desiredNumberScheduled` and
`numberReady`, reports `DaemonSetProgressing` while a rollout is in flight
without stopping the pipeline, and stamps `status.installedImage` once every
node runs a ready pod.

### reconcileNovaComputeAggregates

Lists the host aggregates, ensures one per zone of the pool's `Pending` and
`Active` nodes and, while the pool is not being deleted, `tenant_filter_tests`,
and marks what it creates with `c5c3.io:nova=<namespace>/<nova>`; an aggregate
it could not mark is deleted again. The set of aggregates still needed is the
union over every NovaCompute of the Nova that is not being deleted, on any
cluster; a pass that skipped the Nodes step lists those NovaComputes itself
(`NovaComputeListError` when that fails). A marked aggregate outside the set is
deleted once it holds no host, unless it carries metadata beyond the marker and
its availability zone, which keeps it and records a Warning `AggregateKept`. A
failed call reports `ComputeAPIError` and requeues after 30 seconds without an
error. Only a completed pass lets a deleting pool release its finalizer.

### reconcileNovaComputeServices

Lists the `nova-compute` services once, at microversion 2.69, and fails the
pass with `ComputeAPIError` when a cell did not answer, since its hosts would
otherwise read as serviceless and empty. It then walks the nodes: `Pending` and
`Active` follow the registration, `Draining` disables an enabled service once
and counts the servers on the host, and `Releasing` waits until no pod of the
pool runs on the node and then deletes the service, or drops the entry with no
call under a handover or when the service is already gone. The pods are listed
once per pass: on the node for a single `Releasing` node, and for the whole
pool when there are more. Its requeue is the
periodic poll of Nova: the shortest interval any node needs at entry or at exit.

### Requeue and teardown

| Interval | Constant | Used by |
| --- | --- | --- |
| 1s | `RequeueNextPass` | After a finalizer add |
| 10s | `RequeueComputeReleasePolling` | A `Releasing` node, and a node dropped in this pass |
| 10s | `RequeueDeploymentPolling` | An aggregate created concurrently |
| 15s | `RequeueSecretPolling` | The waits of `NovaReady`, `ConfigReady`, `NodesForbidden`, and an unresolvable target |
| 30s | `RequeueComputeDrainPolling` | A `Draining` or `Pending` node, and the retry after a failed Keystone or Nova call |
| 60s | `RequeueComputeServicePolling` | A pool whose nodes are all settled |

A deleting pool runs the pipeline with every held node leaving until a pass
that began with no node held completed the aggregates step. The pass that
releases the last node therefore keeps the finalizers: deleting the service
empties the aggregates the host sat in after its Aggregates step ran. Once the
pool holds no node, only the `NovaRef` and `Aggregates` steps run, so a pass
from a stale copy of the CR cannot recreate what the sweep removed. When the
Nova is gone, or the target cluster was abandoned, that part is skipped. Then
the remote children (the DaemonSet and the ConfigMaps) are swept, the
ControlPlane's contract mirror is reaped when no other pool of the Nova on the
cluster is live or still holds a node, and the finalizers are released.

### Watches

The NovaCompute controller watches its CRs (status-only updates filtered) and
`Owns` its DaemonSet and ConfigMaps, locally and, by ownership label, on the
target clusters. Beyond that:

- a Nova wakes its pools through the `spec.novaRef.name` index on a spec
  change and on a change to the two status fields a pool waits on, the
  installed release and the published contract;
- a NovaCompute wakes the other pools of the same Nova on a spec change, the
  start of its deletion, or a change in which nodes it holds, in which phase
  and zone, so a node one of them takes or releases re-evaluates the conflicts
  of the rest;
- the Secret `<nova>-compute-config` wakes the pools of `<nova>`, on the
  management cluster and on the target clusters;
- a Node label change on a target cluster wakes the pools placed there that
  select the node or hold it. There is no local Node leg: its informer cannot
  sync on a namespace-scoped install, and the pool's poll picks up a relabel.
