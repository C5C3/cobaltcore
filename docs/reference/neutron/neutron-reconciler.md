---
title: Neutron Reconciler Architecture
quadrant: operator
---

# Neutron Reconciler Architecture

The neutron-operator runs two controllers over the shared table-driven pipeline
(`internal/common/reconcile`): `NeutronReconciler` with fourteen sub-reconcilers
and `NeutronMetadataAgentReconciler` with four. In both, the first step to return
a non-zero result or an error short-circuits the chain, and every exit path
persists the conditions and the requeue through the shared status skeleton, which
skips the write when a pass left status unchanged.

Every step is instrumented under the `neutron_operator` metrics prefix. Two
vectors cover both pipelines, because one instrumenter serves both kinds:
`neutron_operator_reconcile_duration_seconds`, labelled by `sub_reconciler`, and
`neutron_operator_reconcile_errors_total`, labelled by `sub_reconciler` and
`condition_type`. Four per-CR collectors cover the two Job families
(`operators/neutron/internal/metrics/collectors.go`):
`neutron_operator_db_sync_total`, labelled by `neutron`, `namespace` and the
terminal `result`, and `neutron_operator_db_sync_duration_seconds`, labelled by
`neutron` and `namespace`, for the migration Jobs;
`neutron_operator_ovn_db_sync_total` and
`neutron_operator_ovn_db_sync_duration_seconds`, with the same two label sets,
for the runs of the ovn-db-sync CronJob. The db-sync pair carries no phase label,
so the steady-state `db-sync` Job and the three upgrade-phase Jobs feed the same
series; each phase keeps its own dedupe annotation, so a run is counted once.

All four series are dropped for a CR's name and namespace on the pass that
releases the `Neutron` finalizer, which also evicts that CR's health-probe cache
entry, so a deleted CR leaks neither a time series nor a cached probe. A
`NeutronMetadataAgent` has nothing to drop: only a `Neutron` owns a schema and a
sync CronJob, and the two pipeline vectors carry no CR name.

## Pipelines

### Neutron

```text
Secrets ──► DBConnectionSecret ──► TransportURLSecret ──► OVNEndpoints ──► OVNClientSecret
  ──► Config ──► OVNDBSync ──► Database ──► Deployment ──► Workers ──► ┬─ HTTPRoute
                                                                       ├─ HealthCheck
                                                                       ├─ HPA
                                                                       └─ NetworkPolicy   (parallel)
```

| Step | What it does | Condition |
| --- | --- | --- |
| Secrets | Gates on the External Secrets store the CR selects, then the database and service-user credential Secrets, and digests the service-user password | `SecretsReady` |
| DBConnectionSecret | Materialises the pymysql DSN into the derived `{name}-db-connection` Secret and digests it. Reports through `SecretsReady` | `SecretsReady` |
| TransportURLSecret | Materialises the `rabbit://` URL into `{name}-transport-url`, digests it, and returns the broker port the network policy opens. Reports through `SecretsReady` | `SecretsReady` |
| OVNEndpoints | Resolves the Northbound and Southbound addresses off the `OVNCentral` named by `spec.ovn.centralRef` | `OVNEndpointsReady` |
| OVNClientSecret | Mirrors the client identity that central published into `{name}-ovn-client` and digests it. Reports through `OVNEndpointsReady` | `OVNEndpointsReady` |
| Config | Renders `neutron.conf`, `ml2_conf.ini` and `uwsgi.ini` into an immutable content-addressed ConfigMap, prunes the history to three, and records the `ExtraConfigHealthy` guard. Reports failures through `SecretsReady` | `SecretsReady` |
| OVNDBSync | Projects or removes the `{name}-ovn-db-sync` CronJob and reports on the newest terminal run | `OVNDBSyncReady` |
| Database | Provisions the schema, gates the requested release against the installed one, and runs the migration Jobs against the rendered config | `DatabaseReady` |
| Deployment | Ensures the API Deployment, its Service and the PodDisruptionBudget, and stamps `status.endpoint` | `DeploymentReady` |
| Workers | Ensures the `{name}-periodic-workers` and `{name}-ovn-maintenance-worker` Deployments | `WorkersReady` |
| HTTPRoute | Full `spec.gateway` lifecycle; reflects the Gateway's `Accepted` condition | `HTTPRouteReady` |
| HealthCheck | HTTP GET of the cluster-local API root through the shared TTL probe cache | `NeutronAPIReady` |
| HPA | Creates or removes the HorizontalPodAutoscaler | `HPAReady` |
| NetworkPolicy | Creates or removes the NetworkPolicy, with the auto-derived egress set; refuses an empty ingress list | `NetworkPolicyReady` |

The three Secret steps come first, because everything behind them mounts what
they write: the derived DSN and transport URL are what the pods and the migration
Jobs source their credentials from, and the rendered config carries a placeholder
in their place. The two OVN steps follow, and the config step waits behind both:
the `[ovn]` section is parameterised by the two resolved addresses, and it names
the three files of the mirrored client identity.

OVNDBSync runs ahead of Database rather than in the parallel group behind it. The
pipeline short-circuits at the Database step for as long as a migration Job runs,
so a step placed behind it would not be reached to report on the schedule during
the one window that changes it. Its only input is the rendered config the CronJob
mounts. Database itself comes before Deployment, so the API pods start once the
schema they query exists, and Workers after Deployment, sharing its four digests.

Once the Deployment and the Service are in place, the last four steps read none
of each other's output and run as a parallel group. Each member works on its own
copy of the CR and always sets its one condition type, so a cluster without a
gateway, an autoscaler or a network policy still resolves the aggregate through
the `NotRequired` reasons. `RunParallelGroup` merges the conditions and the
metadata off a member's copy and nothing else; no member of this group writes any
other status field, so there is nothing to copy back by hand.

### NeutronMetadataAgent

```text
Chassis ──► Secrets ──► Config ──► DaemonSet
```

| Step | What it does | Condition |
| --- | --- | --- |
| Chassis | Resolves the `OVNChassis` into its node selector and tolerations, and that chassis's `OVNCentral` into the Southbound address and the client Secret name | `ChassisReady` |
| Secrets | Gates on the Nova metadata shared secret and, under `spec.messaging`, materialises `{name}-transport-url` and digests it | `SecretsReady` |
| Config | Renders `neutron_ovn_metadata_agent.ini` into an immutable content-addressed ConfigMap and prunes the history to three. Reports failures through `SecretsReady` | `SecretsReady` |
| DaemonSet | Projects the `{name}-metadata-agent` DaemonSet onto the chassis's nodes, mirrors its node counters into status, and stamps `status.installedImage` | `DaemonSetReady` |

The chassis is the first gate: the nodes it selects, its central's Southbound
address and that central's client Secret parameterise every later step, so an
agent whose chassis has not resolved projects nothing at all. The credentials
come next, because the rendered file is a function of the messaging block and the
DaemonSet mounts the Secrets, and the config before the DaemonSet that mounts it.
The resolved chassis, the transport digest and the ConfigMap name are threaded
from the steps that produce them to the steps that consume them through a
closure, so the hand-off stays inside one pass and not on the reconciler, where
it would be shared by every CR reconciled concurrently.

## Conditions

Each aggregate `Ready` is `True` with reason `AllReady` when every sub-condition
of that kind is `True`, and `False` with `NotAllReady` otherwise. A `Neutron`
aggregates ten, a `NeutronMetadataAgent` three.

| Type | Kind | True reasons | False reasons |
| --- | --- | --- | --- |
| `SecretsReady` | `Neutron` | `SecretsAvailable` | `SecretStoreNotReady`, `WaitingForDBCredentials`, `WaitingForServiceUserCredentials`, `WaitingForMessagingCredentials`, `ConfigError`, `TargetClusterUnavailable` |
| `OVNEndpointsReady` | `Neutron` | `OVNEndpointsResolved` | `OVNCentralNotFound`, `OVNCentralReadError`, `OVNEndpointsPending`, `OVNClientSecretPending`, `OVNClientSecretIncomplete`, `OVNClientSecretReadError`, `OVNClientSecretMirrorFailed`, `TargetClusterUnavailable` |
| `DatabaseReady` | `Neutron` | `DatabaseSynced` | `ClusterNotReady`, `WaitingForDatabase`, `WaitingForConfig`, `DBSyncInProgress`, `DBSyncFailed`, `VersionParseError`, `DowngradeNotSupported`, `UpgradePathInvalid`, `ImageReleaseMismatch`, `UpgradeTargetChanged`, `ExpandInProgress`, `ExpandFailed`, `MigrateInProgress`, `MigrateFailed`, `ContractInProgress`, `ContractFailed`, `UpgradeRollingUpdate` |
| `OVNDBSyncReady` | `Neutron` | `OVNDBSyncNotRequired`, `OVNDBSyncScheduled`, `OVNDBSyncSuspended` | `OVNDBSyncJobFailed` |
| `DeploymentReady` | `Neutron` | `DeploymentReady` | `WaitingForDeployment` |
| `WorkersReady` | `Neutron` | `WorkersReady` | `WaitingForWorkers` |
| `HTTPRouteReady` | `Neutron` | `HTTPRouteAccepted`, `HTTPRouteNotRequired` | `HTTPRouteNotAccepted`, `GatewayAPINotInstalled`, `CapabilityProbeFailed` |
| `NeutronAPIReady` | `Neutron` | `APIHealthy` | `APIUnhealthy`, `EndpointNotReady`, `HealthCheckTimeout`, `ConnectionFailed`, `HealthCheckFailed` |
| `HPAReady` | `Neutron` | `HPAReady`, `HPANotRequired` | none (errors propagate) |
| `NetworkPolicyReady` | `Neutron` | `NetworkPolicyReady`, `NetworkPolicyNotRequired` | none (errors propagate) |
| `ChassisReady` | `NeutronMetadataAgent` | `ChassisResolved` | `ChassisNotFound`, `ChassisReadError`, `ChassisOnAnotherCluster`, `CentralNotFound`, `CentralReadError`, `CentralNotReady`, `TargetClusterUnavailable` |
| `SecretsReady` | `NeutronMetadataAgent` | `SecretsAvailable` | `WaitingForNovaSharedSecret`, `WaitingForMessagingCredentials`, `ConfigError` |
| `DaemonSetReady` | `NeutronMetadataAgent` | `DaemonSetReady` | `DaemonSetProgressing`, `DaemonSetError` |

`TargetClusterUnavailable` is set ahead of every sub-reconciler, when
`spec.targetClusterRef` names a cluster that is not registered or no longer
resolves. It lands on the first condition of that CR's pipeline, `SecretsReady`
on a `Neutron` and `ChassisReady` on a `NeutronMetadataAgent`, so it holds down
the condition the rest of the graph waits behind. The message carries the
resolver's error, the CR requeues after 15 seconds and acquires no finalizer, and
nothing is created on any cluster. A deleting CR waiting for its target cluster
to come back reports the same reason on the same condition. See
[Target Clusters](../target-clusters.md).

`ExtraConfigHealthy` is set beside these on every pass that renders a config, on
both kinds, and reports the honored overrides of operator-owned keys. It stays
out of the `Ready` aggregation, so an override the operator honors reports itself
without holding the CR unready.

One map attributes an error to its condition type for the `condition_type` label.
It serves both pipelines, because both reconcilers run in one binary under the
same metrics prefix (`instrumentation.go`):

```go
var subReconcilerConditionTypes = map[string]string{
	"Secrets":            "SecretsReady",
	"DBConnectionSecret": "SecretsReady",
	"TransportURLSecret": "SecretsReady",
	"Config":             "SecretsReady",
	"OVNEndpoints":       "OVNEndpointsReady",
	"OVNClientSecret":    "OVNEndpointsReady",
	"Database":           "DatabaseReady",
	"OVNDBSync":          "OVNDBSyncReady",
	"Deployment":         "DeploymentReady",
	"Workers":            "WorkersReady",
	"HTTPRoute":          "HTTPRouteReady",
	"HealthCheck":        "NeutronAPIReady",
	"HPA":                "HPAReady",
	"NetworkPolicy":      "NetworkPolicyReady",

	"Chassis":   "ChassisReady",
	"DaemonSet": "DaemonSetReady",
}
```

Sixteen keys cover eighteen sub-reconcilers. `Secrets` and `Config` appear once,
because both pipelines run a step of that name and both drive `SecretsReady`.
Four steps share `SecretsReady` and two share `OVNEndpointsReady`: all of them
produce artefacts the same downstream graph mounts, so collapsing them under one
condition keeps the status contract small while the `sub_reconciler` label on the
error counter still separates them during triage.

A `sub_reconciler` name that reaches the instrumenter without a key here resolves
to `UNKNOWN`. An empty label would collapse two series into one; `UNKNOWN` makes
the drift visible in alerts.

## Sub-reconcilers

### reconcileSecrets

**File:** `operators/neutron/internal/controller/reconcile_secrets.go`

**Purpose:** Gate on the credentials the Neutron pods consume: the External
Secrets store the CR selects through `spec.secretStoreRef`, then the database
credentials Secret and the service-user password Secret. The store is checked
first, so an outage of the secret backend surfaces even while the
per-ExternalSecret caches still report their last successful sync. The step ends
by digesting the service-user password for the pod-template annotation the
deployment and worker steps stamp: the password reaches the process as
`OS_KEYSTONE_AUTHTOKEN__PASSWORD`, so a rotation at the source takes effect on a
pod restart and on nothing else.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `SecretStoreNotReady` | "\<kind\> \"\<name\>\" is not ready; upstream secret backend unreachable", naming the selected `ClusterSecretStore` or `SecretStore` | `RequeueSecretPolling` |
| `False` | `WaitingForDBCredentials` | The gate's attribution of the miss: "Database credentials ExternalSecret \<ns\>/\<name\> not found yet", "Waiting for ESO to sync database credentials from OpenBao", or "Database credentials Secret exists but is missing expected keys" | `RequeueSecretPolling` |
| `False` | `WaitingForServiceUserCredentials` | The same three attributions against the service-user Secret and its configured key | `RequeueSecretPolling` |
| `True` | `SecretsAvailable` | none | none |

**Error handling:** A backend read error is returned without a condition, so the
controller falls back on its own rate limiter. One class is recorded first and
then returned: a namespace the target cluster's cache refuses, which every pass
fails on identically until the registration or the CR moves, and which would
otherwise live only in the operator's log.

### reconcileDBConnectionSecret

**File:** `operators/neutron/internal/controller/reconcile_dbconnection_secret.go`

**Purpose:** Derive the pymysql DSN from the upstream credentials Secret and
write it to `{name}-db-connection`, through the shared
`database.ReconcileConnectionSecret`. Every neutron process and every migration
Job reads it as `OS_DATABASE__CONNECTION`, which is why `[database] connection`
in the rendered file is a placeholder URL. With `spec.database.tls` enabled the
DSN carries `ssl_ca`, `ssl_cert` and `ssl_key` paths under
`/etc/neutron-db-tls/`, the directory the workloads and the Jobs project the
keypair at, so the key bytes never pass through the operator process. The step
returns the DSN's digest for the rollout annotation.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `WaitingForDBCredentials` | "Upstream database credentials Secret \<ns\>/\<name\> not found", or "… missing key \<key\>" | `RequeueSecretPolling` |

**Error handling:** A failed read of the upstream Secret is returned. No derived
Secret is written with empty credentials. The step sets no `True` arm:
`reconcileSecrets` ahead of it already reported `SecretsAvailable` on the same
condition, and the digest is empty on every path that wrote nothing, which keeps
the annotation off the pod template on a short-circuited pass.

### reconcileTransportURLSecret

**File:** `operators/neutron/internal/controller/reconcile_messaging.go`

**Purpose:** Materialise the `rabbit://` transport URL into
`{name}-transport-url` and return the two values later steps read off it: the
URL's digest for the rollout annotation, and the broker's TCP port, which the
network-policy step opens as an egress peer. Managed mode reads the
`RabbitmqCluster`'s default-user Secret, brownfield mode the Secret
`spec.messaging.secretRef` names. Nothing in this deployment dials the broker,
and the URL is required all the same: `config.init` calls `n_rpc.init` in every
neutron process, so the value has to parse.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `WaitingForMessagingCredentials` | The managed `RabbitmqCluster` has published no default-user Secret yet, or the brownfield Secret carries no transport URL under its key | `RequeueSecretPolling` |

**Error handling:** A failed read or write is returned, and the port and the
digest are both zero on the waiting path, where the network-policy step then
omits the messaging rule. The port falls back to 5672 for a URL that names none,
so the derived rule always carries one port. The shared helper reads and writes
in the CR's own namespace through the CR's own children client, which is why a
placed `Neutron` whose bus runs on the management cluster has to use
`spec.messaging.secretRef`.

### reconcileOVNEndpoints

**File:** `operators/neutron/internal/controller/reconcile_ovn.go`

**Purpose:** Resolve the Northbound and Southbound addresses of the `OVNCentral`
named by `spec.ovn.centralRef`. The central is read through the
management-cluster client, because both CRs live there whatever cluster their
children land on, and the ref carries a namespace: the OVN control plane commonly
lives in the privileged networking namespace while the Neutron API lives with the
rest of the control plane. Which pair of published addresses applies follows from
where the two CRs project their children. Inside one cluster each database is
reached at its `internalDbAddress`; across a cluster boundary only the
`dbAddress` published on node ports is routable. Both fields are written by the
central's own endpoint step, documented under
[OVN Reconciler Architecture](../ovn/ovn-reconciler.md#reconcileendpoints).

**OVN coupling.** Both steps of this file write `OVNEndpointsReady`, and these
are all of their arms:

| Step | Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- | --- |
| OVNEndpoints | `False` | `OVNCentralNotFound` | "OVNCentral \<ns\>/\<name\> does not exist; the ML2/OVN mechanism driver stays unconfigured until it does" | 15s |
| OVNEndpoints | `False` | `OVNCentralReadError` | "reading OVNCentral \<ns\>/\<name\>: \<error\>" | none (error returned) |
| OVNEndpoints | `False` | `OVNEndpointsPending` | "Waiting for OVNCentral \<ns\>/\<name\> to publish its Northbound and Southbound addresses", when both CRs project onto the same cluster | 15s |
| OVNEndpoints | `False` | `OVNEndpointsPending` | Names the cluster each of the two CRs projects onto and asks for `spec.northbound.externallyReachable` and `spec.southbound.externallyReachable` on the central, when they project onto different ones | 15s |
| OVNEndpoints | `False` | `OVNEndpointsPending` | "Waiting for OVNCentral \<ns\>/\<name\> to publish its client Secret; the databases accept no connection without a client certificate" | 15s |
| OVNClientSecret | `False` | `TargetClusterUnavailable` | The resolver's error for the central's own target cluster | 15s |
| OVNClientSecret | `False` | `OVNClientSecretPending` | "Waiting for the OVN client Secret \<ns\>/\<name\> the OVNCentral publishes" | 15s |
| OVNClientSecret | `False` | `OVNClientSecretReadError` | "reading OVN client Secret \<ns\>/\<name\>: \<error\>" | none (error returned) |
| OVNClientSecret | `False` | `OVNClientSecretIncomplete` | "OVN client Secret \<ns\>/\<name\> carries no \<key\> yet", naming `tls.crt`, `tls.key` or `ca.crt` | 15s |
| OVNClientSecret | `False` | `OVNClientSecretMirrorFailed` | The wrapped error of claiming, creating, reading or updating `{name}-ovn-client` | none (error returned) |
| OVNEndpoints | `True` | `OVNEndpointsResolved` | "The ML2/OVN mechanism driver connects to OVNCentral \<ns\>/\<name\> at \<nb\> (Northbound) and \<sb\> (Southbound)" | none |

**Error handling:** A missing `OVNCentral` polls and leaves the pass successful:
a `Neutron` applied before its central is an ordinary ordering of two objects in
one manifest. A failed read is returned so the controller backs off. The
cross-cluster message is the one arm that names a field to edit, because from
another cluster the Service addresses resolve nowhere and only the node ports do.

### reconcileOVNClientSecret

**File:** `operators/neutron/internal/controller/reconcile_ovn.go`

**Purpose:** Mirror the client identity the `OVNCentral` publishes into the
namespace the Neutron pods run in, as `{name}-ovn-client`, and return the SHA-256
digest of its three values so the deployment and worker steps roll the pods when
the certificate is reissued. The mirror exists because the source is not
reachable where the pods are: it lives in the central's namespace, and on a
placed `Neutron` even on another cluster. The source is read live through the
central's own children client, so an ownership decision is never made on a cache
that has not caught up; the copy is written through this Neutron's children
client, which is what makes it mountable.

**Condition Contract:** the five arms the OVN coupling table above lists under
this step, `TargetClusterUnavailable`, `OVNClientSecretPending`,
`OVNClientSecretReadError`, `OVNClientSecretIncomplete` and
`OVNClientSecretMirrorFailed`. Each of them overwrites `OVNEndpointsReady`, the
condition the endpoint step left `True`: a resolved address the pods cannot
authenticate against is no usable endpoint.

**Error handling:** An unresolvable target cluster and an absent source Secret
poll without an error, since the central publishes the Secret name in its status
before cert-manager has issued the certificate. A failed read and a failed write
are returned. The mirror is repaired to carry the three source values and nothing
else: a stale certificate left behind by a reissue would authenticate against
nothing, and an extra key would survive in a Secret the operator owns. The digest
hashes the entries in sorted key order, length-prefixed, so a value that ends
where the next one begins cannot collide with the pair that swaps that boundary.

### reconcileConfig

**File:** `operators/neutron/internal/controller/reconcile_config.go`

**Purpose:** Render `neutron.conf`, `ml2_conf.ini` and `uwsgi.ini` into one
immutable, content-addressed ConfigMap, plus `logging.conf` under
`spec.logging.format: json`, and return its name to the steps that mount it. The
section name routes each option to the file its consumer reads, and the routing
covers `spec.extraConfig` too, so a user override lands there as well. The step
keeps no last-good artefact: the two OVN steps ahead of
it short-circuit the pipeline while the addresses or the client identity are
unresolved, so no pass renders against an incomplete projection. See
[Rendered defaults](./neutron-crd.md#rendered-defaults).

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `ConfigError` | "creating config ConfigMap: \<error\>", or "pruning config ConfigMaps: \<error\>" | none (error returned) |

**Error handling:** Both failures flip `SecretsReady` to `False` before the error
is returned, so the aggregate cannot stay stale-`True` at the new generation.
There is no `True` arm: the Secrets step owns it. Pruning keeps three historical
ConfigMaps beside the current one, which is what a rollback reads. The
`ExtraConfigHealthy` guard runs at the top of the step and raises the
`ExtraConfigOwnedKeyOverride` Warning on the transition into `False` and whenever
the overridden-key set changes. See [Controller Events](./neutron-events.md).

### reconcileOVNDBSync

**File:** `operators/neutron/internal/controller/reconcile_ovndbsync.go`

**Purpose:** Project or remove the `{name}-ovn-db-sync` CronJob and report on the
newest run that reached a terminal state. Run visibility is derived rather than
watched: the CronJob controller spawns one Job per firing and prunes them by
history limit, so the step lists the Jobs carrying this CR's sync component
labels, keeps the ones this CronJob controls, and reads the newest terminal one.
`schedule` and `syncMode` are resolved here rather than by the defaulting
webhook, so an unset field keeps tracking the operator default across upgrades.
See [ovnDBSync](./neutron-crd.md#ovndbsync).

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `True` | `OVNDBSyncNotRequired` | "No OVN database synchronisation is scheduled: spec.ovnDBSync is unset" | none |
| `True` | `OVNDBSyncSuspended` | The schedule and the mode that apply when resumed, plus the last failed run when there was one | none |
| `False` | `OVNDBSyncJobFailed` | Names the failed Job and the "Could not retrieve schema from \<address\>" line that tells an unreachable Northbound from drift | none |
| `True` | `OVNDBSyncScheduled` | "OVN database synchronisation scheduled \"\<schedule\>\" in \"\<mode\>\" mode" | none |

**Error handling:** A failed delete, apply or Job listing returns the wrapped
error and sets no condition, so `OVNDBSyncReady` stays absent or stale, the
aggregate stays `False`, and the pipeline attributes the error to this step.
Suspension outranks a failed run: a suspended CronJob spawns no successor to
supersede the failure, so an `OVNDBSyncJobFailed` arm winning there would pin the
aggregate `Ready` down until someone deleted the Job by hand. Every terminal run
feeds the ovn-db-sync metric pair and, on failure, a Warning, both deduped on the
Job UID through an annotation on the CR.

### reconcileDatabase

**File:** `operators/neutron/internal/controller/reconcile_database.go`

**Purpose:** Provision the schema, gate the requested OpenStack release against
the installed one, and run the migration Jobs against the rendered config. The
shared provisioning flow runs first: the MariaDB cluster gate plus
`Database`/`User`/`Grant` in managed mode, a no-op in brownfield, and no
`User`/`Grant` under `credentialsMode: Dynamic`. The SQL user's
`max_user_connections` cap is sized from the CR's own topology, because each API
worker process holds two pooled connections and a cap below the fleet's steady
state crash-loops the last pods to start.

**The `WaitingForConfig` gate.** An empty ConfigMap name means the OVN endpoints
are still unresolved and nothing has been rendered. The migration Jobs mount that
ConfigMap as their whole `/etc/neutron`, so an empty name would render a volume
the API server rejects and every pass would fail on the Job create. The step
reports `WaitingForConfig` and requeues after `RequeueDatabaseWait` (30 s).

**The release gate.** `gateReleaseTransition` validates a change of
`spec.openStackRelease` against `status.installedRelease` before the upgrade flow
is entered. A fresh install and a patch bump pass through. An unparseable release
on either side, a downgrade and a jump of more than one release are refused, each
with the shared reason, a Warning event and a returned error. The gate adds the
one rule the shared flow cannot know about: a release bump that leaves
`spec.image` at the reference recorded in `status.installedImage` migrates
nothing, so the phase Jobs would short-circuit on their own completed runs and
`installedRelease` would advance off the previous release's binary. Its message
names both readings of that state, because the recovery differs. The tag-side
counterpart, `checkImageReleaseMismatch`, compares a tag-pinned `spec.image`
against `spec.openStackRelease` on every pass and requeues without an event.

**The phase Jobs.** An accepted bump walks the shared expand-migrate-contract
machine: `{name}-db-expand` runs `neutron-db-manage upgrade --expand`,
`{name}-db-migrate` runs `neutron-db-manage current`, and `{name}-db-contract`
runs `neutron-db-manage upgrade --contract`, each with a backoff limit of 4 and
each built from `spec.image`. Neutron's alembic tree splits into an expand and a
contract branch with no data-migration command between them, so the migrate phase
prints the revision each branch sits at and exits 0, which keeps the phase
readable in the Job log. Between migrate and contract the flow parks in
`RollingUpdate` and waits for the deployment step's `neutronDeploymentRolledOut`
check, so the contract Jobs drop nothing the old pods still read. Fresh installs
and patch bumps stay on the single-pass `{name}-db-sync` Job, which runs
`neutron-db-manage upgrade head`; there is no schema-check Job, because that
upgrade is idempotent and a second read-only run would assert nothing.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `ClusterNotReady` | "MariaDB cluster \"\<name\>\" is not ready" | `RequeueDatabaseWait` |
| `False` | `WaitingForDatabase` | "MariaDB Database CR is not ready", or "MariaDB User or Grant CR is not ready" | `RequeueDatabaseWait` |
| `False` | `WaitingForConfig` | "Waiting for the resolved OVN endpoints and the rendered config before provisioning the database schema" | `RequeueDatabaseWait` |
| `False` | `ImageReleaseMismatch` | The tag-versus-release disagreement, or the release bump that leaves `spec.image` unchanged | `RequeueDatabaseWait` on the tag check; none on the gate (error returned) |
| `False` | `VersionParseError`, `DowngradeNotSupported`, `UpgradePathInvalid` | The rule that refused the transition, with both release strings | none (error returned) |
| `False` | `DBSyncInProgress` | "db_sync job is running" | `RequeueDatabaseWait` |
| `False` | `DBSyncFailed` | "db_sync job failed: \<error\>" | none (error returned) |
| `False` | `ExpandInProgress`, `MigrateInProgress`, `ContractInProgress` | "\<Phase\> phase running: \<from\> → \<to\>" | `RequeueUpgradeWait` |
| `False` | `ExpandFailed`, `MigrateFailed`, `ContractFailed` | "\<Phase\> job \<name\> failed: \<error\>" | none (error returned) |
| `False` | `UpgradeRollingUpdate` | "Migrate complete, waiting for Deployment rollout: \<from\> → \<to\>" | none, the deployment step drives it |
| `False` | `UpgradeTargetChanged` | "Spec release changed to \<r\> during active upgrade \<from\> → \<to\>; complete or roll back the current upgrade first" | none (error returned) |
| `True` | `DatabaseSynced` | "Database schema is up to date (revision verified)", or "(upgraded \<from\> → \<to\>)" | none |

**Error handling:** Every refused release transition and every failed Job returns
an error, so the controller backs off; a re-run would fail the same way. The wait
states return no error and pace themselves on the two 30 s intervals. Reverting
`spec.openStackRelease` to the installed release stays reachable mid-upgrade,
because it is the shared flow's abort trigger and the image check is skipped for
it; a wedged upgrade can always be unstuck that way.

### reconcileDeployment

**File:** `operators/neutron/internal/controller/reconcile_deployment.go`

**Purpose:** Ensure the API Deployment, its Service on port 9696 and the
PodDisruptionBudget, and stamp `status.endpoint` with the cluster-local Service
URL once the Deployment is available. `status.endpoint` is that URL whether or
not `spec.gateway` is set, since it reports API readiness rather than the ingress
path.

**The launch command.** The neutron image ships no `neutron-server` binary and no
entry script, so the API container runs
`uwsgi --http :9696 … --module neutron.wsgi.api`, with the shared
`deployment.BuildUWSGICommand` emitting the process, thread, keep-alive and
harakiri parameters, and a trailing `--ini /etc/neutron/uwsgi.ini`. That file
carries `start-time = %t`, which uWSGI expands while it reads it; rendering the
same marker on the command line would pass the literal `%t` and kill neutron on
`int('%t')`.

**Config delivery.** A uWSGI-imported application has no argv to carry
`--config-file`, so the API container names its files in the environment instead:
`OS_NEUTRON_CONFIG_DIR` is `/etc/neutron`, the mount point of the rendered
ConfigMap, and `OS_NEUTRON_CONFIG_FILES` is `neutron.conf;ml2_conf.ini`. Those
are the same two files, in the same order, that the worker Deployments, the
migration Jobs and the ovn-db-sync CronJob pass as `--config-file` arguments
through `neutronCommand`. The three credentials travel beside them as oslo.config
overrides (`OS_DATABASE__CONNECTION`, `OS_DEFAULT__TRANSPORT_URL`,
`OS_KEYSTONE_AUTHTOKEN__PASSWORD`), so none of them enters the rendered document.

**The rolled-out gate.** `neutronDeploymentRolledOut` is stricter than the
surge-tolerant readiness the shared ensure helper reports: it requires the
deployment controller to have observed the latest generation and every replica to
be updated, ready and counted, with no surge or old-template pod left. The
readiness signal turns true as soon as the first new-image pod is ready while
old-image pods still serve, and the contract phase drops what those pods still
read, so the `RollingUpdate` to `Contracting` flip waits for this stricter check.
Only that phase is gated on it; a steady-state rollout keeps the surge-tolerant
readiness.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `WaitingForDeployment` | "Neutron API deployment is not yet available" | `RequeueDeploymentPolling` |
| `False` | `WaitingForDeployment` | "Waiting for the upgraded image to finish rolling out before contracting the database schema" | `RequeueDeploymentPolling` |
| `True` | `DeploymentReady` | "Neutron API deployment is available" | `RequeueNextPass` on the pass that flips the upgrade phase; none otherwise |

**Error handling:** A failed apply of the Deployment, the Service or the PDB is
returned with no condition, so the aggregate stays down on the stale condition
while the pipeline attributes the error to this step. The endpoint is not stamped
on the pass that flips `RollingUpdate` to `Contracting`: that pass requeues
immediately so the contract Job starts on the next one. The four content digests
are stamped only when non-empty, so a short-circuited upstream pass rolls no
pods.

### reconcileWorkers

**File:** `operators/neutron/internal/controller/reconcile_workers.go`

**Purpose:** Ensure the two Deployments running the neutron processes that serve
no HTTP: `{name}-periodic-workers` for the recurring maintenance tasks of the ML2
plugin, and `{name}-ovn-maintenance-worker`, which reconciles the Northbound
model against the Neutron database. Both carry the same config mount, the same
OVN client identity and the same four digests as the API pods, and differ in what
they leave out: no ports, no probes, no Service, no HPA and no
PodDisruptionBudget. No client dials them, so a readiness gate would report
nothing a client acts on, and an eviction costs a delayed maintenance pass. There
is no third Deployment for `neutron-rpc-server`: nothing here consumes RPC, and
the rendered config sets both worker counts to zero to match.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `WaitingForWorkers` | "Waiting for the periodic-workers and ovn-maintenance-worker Deployments to become available" | `RequeueDeploymentPolling` |
| `True` | `WorkersReady` | "The periodic-workers and ovn-maintenance-worker Deployments are available" | none |

**Error handling:** A failed apply names the Deployment in the wrapped error and
returns it. Both Deployments are applied before readiness is judged, so a cluster
starting from nothing creates both on its first pass rather than one per polling
interval. Readiness is the conjunction of the two: one worker down holds
`WorkersReady` `False` while `DeploymentReady` stays `True`, which is what
separates an API that serves reads from one that serves nothing.

### reconcileHTTPRoute

**File:** `operators/neutron/internal/controller/reconcile_httproute.go`

**Purpose:** Drive the `spec.gateway` lifecycle through the shared route flow: an
HTTPRoute attached to the parent Gateway, matching the configured hostname with a
path-prefix match, and forwarding to the `{name}` Service on port 9696. Removing
the block deletes the route. The route renders no request timeout, since neutron
answers short JSON requests and the gateway implementation's default is the right
cap. Whether the cluster serves the kind is probed at setup for a local CR and
against the target cluster's RESTMapper for a placed one, so the verdict follows
the cluster the children land on.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `CapabilityProbeFailed` | "Probing the target cluster for the HTTPRoute kind failed: \<error\>" | none |
| `True` | `HTTPRouteNotRequired` | `spec.gateway` is unset, so any previous route was deleted | none |
| `False` | `GatewayAPINotInstalled` | The cluster the children land on does not serve the HTTPRoute kind while `spec.gateway` is set | none |
| `False` | `HTTPRouteNotAccepted` | "HTTPRoute not yet accepted by Gateway" | `RequeueDeploymentPolling` |
| `True` | `HTTPRouteAccepted` | "HTTPRoute accepted by Gateway" | none |

**Error handling:** A failed apply or delete is returned. A cluster without
Gateway API is reported on the condition rather than failing the controller at
start with an unknown kind, which would take down every controller in the binary.

### reconcileHealthCheck

**File:** `operators/neutron/internal/controller/reconcile_healthcheck.go`

**Purpose:** GET the cluster-local API root and report the result. `/` is the
version document, which the API answers without a token and without touching the
database, so a 2xx there means the WSGI application is serving. The probe target
is always the in-cluster Service URL, independent of `spec.gateway`. A successful
probe is cached for 30 seconds per CR, keyed on the CR's UID and the endpoint, so
a steady-state pass fires no synchronous GET. A placed `Neutron` is probed
through the target API server's service proxy, because its Service URL resolves
on that cluster and nowhere else.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `EndpointNotReady` | "endpoint not yet configured" while `status.endpoint` is unset, or "endpoint not resolvable" for a DNS error | `RequeueHealthCheck` |
| `False` | `HealthCheckTimeout` | "health check timed out" | `RequeueHealthCheck` |
| `False` | `ConnectionFailed` | "connection failed: \<error\>" | `RequeueHealthCheck` |
| `False` | `HealthCheckFailed` | "health check failed: \<error\>" | `RequeueHealthCheck` |
| `False` | `APIUnhealthy` | "Neutron API returned HTTP \<code\>", plus an excerpt of the body when there is one | `RequeueHealthCheck` |
| `True` | `APIHealthy` | "Neutron API is responding at \<url\>" | none |

**Error handling:** Every network error requeues without an error return, so a
restarting API paces the retry on the 10 s interval. A cancelled parent context
is the one exception and propagates unchanged: it means a peer of the parallel
group failed, and an aborted probe is no signal about the API. A probe error and
a non-2xx both evict the cache, so a recovery is seen on the next pass and no
stale success masks it.

### reconcileHPA

**File:** `operators/neutron/internal/controller/reconcile_hpa.go`

**Purpose:** Create or remove the HorizontalPodAutoscaler for the API Deployment
through the shared HPA flow. An unset `spec.autoscaling.minReplicas` resolves to
the effective `spec.deployment.replicas`. The two worker Deployments are never
autoscaled: they consume a maintenance queue, so there is no request rate for an
HPA to track.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `True` | `HPANotRequired` | "Autoscaling is not configured" | none |
| `True` | `HPAReady` | "HorizontalPodAutoscaler is configured" | none |

**Error handling:** A failed apply or delete is returned with no condition. Both
arms are `True`, so a cluster without autoscaling still resolves the aggregate.

### reconcileNetworkPolicy

**File:** `operators/neutron/internal/controller/reconcile_networkpolicy.go`

**Purpose:** Create or remove the NetworkPolicy for this CR's pods. Ingress is
TCP 9696 from the listed sources, plus the Gateway's namespace while
`spec.gateway` is set and the operator's own namespace so its health check
reaches the API. The pod selector carries no component key, so one policy covers
the API pods, both worker Deployments and the ovn-db-sync Job pods: all of them
read the same database and dial the same two OVN databases, and a per-component
split would duplicate every rule and let the copies drift.

**Egress rule order.** The auto-derived rules are emitted in a fixed order, and
`spec.networkPolicy.additionalEgress` is appended after them:

| Order | Rule | Emitted when |
| --- | --- | --- |
| 1 | DNS, UDP and TCP 53 | Always |
| 2 | The database port from `spec.database` | Always |
| 3 | The Keystone endpoint's port | Always |
| 4 | The cache port from `spec.cache` | The cache spec yields a port |
| 5 | One port per distinct published OVN member address | The `OVNCentral` has published its addresses |
| 6 | The broker port of the transport URL | The messaging step materialised a URL |

Rules 3 to 6 are port-only, with the destination unrestricted: a Keystone URL, a
broker and an OVN member can each name a Service in another namespace or a host
outside the cluster, so there is no selector to write. The OVN addresses are
comma-separated ovsdb connection strings, which the helper rewrites to
`tcp://<host>:<port>` before the shared builder parses, deduplicates and sorts
the ports out of them, so two members on the same port open one port. Without
rule 3 the API answers 503 on every authenticated request while both readiness
signals keep passing, because both target the unauthenticated version document.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `True` | `NetworkPolicyNotRequired` | "Network isolation is not configured" | none |
| `True` | `NetworkPolicyReady` | "NetworkPolicy is configured" | none |

**Error handling:** A failed apply or delete is returned. An empty ingress list
fails closed with an error and no policy: an ingress rule with no peers admits
every source, which is the opposite of what the field asks for. The CRD and the
webhook reject that spec too; the guard here catches an object that reached etcd
past them.

### reconcileChassis

**File:** `operators/neutron/internal/controller/reconcile_chassis.go`

**Purpose:** Resolve the `OVNChassis` this agent runs alongside and the
`OVNCentral` that chassis attaches to, into the four values the later steps are
parameterised by: the node selector and the tolerations the DaemonSet inherits,
the Southbound address the agent reads the port bindings of its own node from,
and the name of the client Secret its pods mount. Both CRs are read through the
management-cluster client in the agent's own namespace, since `spec.chassisRef`
is namespace-local and every CR of this control plane is written on the
management cluster. The selector and the tolerations are deep-copied, so the
rendered DaemonSet shares no field with the CR it was rendered from. For the
nodes the chassis itself programs see
[OVNChassis CRD](../ovn/ovn-chassis-crd.md).

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `ChassisNotFound` | "OVNChassis \<name\> does not exist in namespace \<ns\>; the agent stays unprojected until it does" | `RequeueSecretPolling` |
| `False` | `ChassisReadError` | "reading OVNChassis \<ns\>/\<name\>: \<error\>" | none (error returned) |
| `False` | `ChassisOnAnotherCluster` | Names the cluster each of the two CRs projects onto and why both have to agree | none |
| `False` | `CentralNotFound` | "OVNCentral \<name\>, which OVNChassis \<name\> attaches to, does not exist in namespace \<ns\>" | `RequeueSecretPolling` |
| `False` | `CentralReadError` | "reading OVNCentral \<ns\>/\<name\>: \<error\>" | none (error returned) |
| `False` | `CentralNotReady` | "Waiting for OVNCentral \<name\> to publish its Southbound address and its client Secret" | `RequeueSecretPolling` |
| `True` | `ChassisResolved` | "The agent runs on the nodes of OVNChassis \<name\> and reads OVNCentral \<name\> at \<address\>" | none |

**Error handling:** A missing chassis or central polls and leaves the pass
successful, the same ordering argument the OVN endpoint step makes. A failed read
is returned. A cluster mismatch sets the condition and returns neither an error
nor a requeue: the agent shares the chassis's nodes and mounts the Secret its
central publishes, neither of which crosses a cluster boundary, and both refs are
immutable, so only deleting and reapplying one of the two CRs repairs it. The
check cannot move into the webhook, because `spec.chassisRef` may name a chassis
that does not exist at admission time.

### reconcileAgentSecrets

**File:** `operators/neutron/internal/controller/reconcile_agent_secrets.go`

**Purpose:** Gate on the credentials the agent pods consume and return the
transport URL's digest for the DaemonSet's pod-template annotation. Both blocks
it gates on are optional: an agent without `spec.novaMetadata` proxies nowhere,
and one without `spec.messaging` opens no broker connection, so a CR that sets
neither reaches `SecretsAvailable` without reading anything. The shared secret
reaches the process as `OS_DEFAULT__METADATA_PROXY_SHARED_SECRET` and the
transport URL as `OS_DEFAULT__TRANSPORT_URL`, so neither enters the ConfigMap
every agent pod mounts.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `WaitingForNovaSharedSecret` | The gate's attribution: the ExternalSecret is absent, it has not synced, or the Secret is missing the configured key | `RequeueSecretPolling` |
| `False` | `WaitingForMessagingCredentials` | The managed `RabbitmqCluster` has published no default-user Secret yet, or the brownfield Secret carries no transport URL | `RequeueSecretPolling` |
| `True` | `SecretsAvailable` | none | none |

**Error handling:** A backend read error is returned. The gate and the container
environment resolve the shared secret's data key through one function, so a pod
never sources a key the gate did not check. Nova rejects an unsigned request when
it carries a secret of its own, which is why a missing shared secret is a wait
rather than a value the agent starts without.

### reconcileAgentConfig

**File:** `operators/neutron/internal/controller/reconcile_agent_config.go`

**Purpose:** Render `neutron_ovn_metadata_agent.ini` into one immutable,
content-addressed ConfigMap, plus `logging.conf` under
`spec.logging.format: json`, and return its name to the DaemonSet step. The
rendered `[ovs] ovsdb_connection` is the local socket
`unix:/run/openvswitch/db.sock` the chassis pods create on the node, and the
`[ovn]` section carries the Southbound address the chassis step resolved together
with the three files of the mounted client keypair. `[DEFAULT] root_helper` and
the privsep helper commands stay at their oslo defaults of `sudo` and
`sudo privsep-helper`, because the image ships `/usr/bin/sudo` and the container
runs as root.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `ConfigError` | "creating config ConfigMap: \<error\>", or "pruning config ConfigMaps: \<error\>" | none (error returned) |

**Error handling:** As `reconcileConfig` on the `Neutron` side: both failures
flip `SecretsReady` to `False` before the error is returned, there is no `True`
arm, and three historical ConfigMaps are retained. The `ExtraConfigHealthy` guard
runs against `MetadataAgentOwnedConfigKeys`, the agent's own ownership registry,
which is separate from the `Neutron` one because the two kinds render different
files.

### reconcileDaemonSet

**File:** `operators/neutron/internal/controller/reconcile_daemonset.go`

**Purpose:** Project the `{name}-metadata-agent` DaemonSet onto the nodes the
chassis selects, mirror `desiredNumberScheduled` and `numberReady` into status,
and stamp `status.installedImage`. The pod runs with `hostNetwork: true` and
mounts `/run/openvswitch` and `/run/netns` from the host, the second with
bidirectional mount propagation so the namespaces the agent creates are visible
to the node. The `wait-for-chassis` init container polls the local Open vSwitch
database until `external_ids:system-id` exists, which is what the chassis's own
`apply-node` init container writes: both workloads select the same nodes and
nothing orders the two DaemonSets, so the gate is per node. Readiness is the
metadata proxy socket rather than the process, tested with
`test -S /var/lib/neutron/metadata_proxy`. See
[Node contract](./neutron-metadata-agent-crd.md#node-contract).

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `DaemonSetError` | "ensuring metadata-agent DaemonSet: \<error\>" | none (error returned) |
| `False` | `DaemonSetProgressing` | "Waiting for the metadata-agent DaemonSet: \<n\> of \<m\> nodes run a ready pod" | `RequeueDeploymentPolling` |
| `True` | `DaemonSetReady` | "The metadata-agent DaemonSet runs a ready pod on \<m\> nodes" | none |

**Error handling:** A failed apply or read sets `DaemonSetError` and returns the
error. The node counters are mirrored before the readiness branch, so a
progressing DaemonSet still reports what it has. `status.installedImage` is
stamped on the ready arm alone, so a rollout that has reached no node leaves the
previous value in place, and that is what tells the two apart. Readiness is
judged against the live object after the apply, since the counters live on the
status subresource the apply strips out.

## Requeue semantics

| Constant | Value | Used by |
| --- | --- | --- |
| `RequeueSecretPolling` (`internal/common/reconcile/intervals.go`) | 15s | The secret-store and credential gates on both kinds, the derived DSN and transport-URL waits, every polling arm of the two OVN steps and of the chassis step, and the target-cluster hold on both controllers |
| `RequeueDeploymentPolling` (`internal/common/reconcile/intervals.go`) | 10s | Deployment and DaemonSet readiness polling: the API Deployment, the two workers, the metadata-agent DaemonSet, and the wait for a Gateway to accept the route |
| `RequeueHealthCheck` (`internal/common/healthcheck/healthcheck.go`) | 10s | Every not-ready outcome of the API probe. The probe itself is bounded by a 10s timeout, and a success is reused for 30s from the shared cache |
| `RequeueDatabaseWait` (`requeue_intervals.go`) | 30s | The MariaDB cluster and CR gates, the `WaitingForConfig` gate, the db-sync Job wait, the tag-side image check, and the finalizer hold while the MariaDB CRs tear down |
| `RequeueUpgradeWait` (`requeue_intervals.go`) | 30s | The expand, migrate and contract Job waits |
| `RequeueNextPass` (`internal/common/reconcile/intervals.go`) | 1s | The single pass after a finalizer was added, and the pass that flips the upgrade phase from `RollingUpdate` to `Contracting` |

None of the wait states reaches the workqueue as an error, so these intervals are
what paces the retries. A returned error goes on the controller's own backoff
instead, capped at 30 seconds by the shared rate limiter
(`bootstrap.TypedControllerOptions`).

## Watches

Both controllers filter their CR's status-only updates through
`watch.CRUpdatePredicate`, so a status write does not re-wake the controller, and
both turn the multicluster builder's cluster-not-found wrapper off: an
unresolvable target cluster is surfaced as `TargetClusterUnavailable` and
requeued, where the wrapper would swallow it as a successful reconcile.

The `Neutron` controller registers two field indexes on the local field indexer.
`spec.secretRefs.name` holds the deduplicated union of
`spec.database.secretRef.name`, `spec.serviceUser.secretRef.name` and
`spec.messaging.secretRef.name`. `spec.ovn.centralRef` holds
`<namespace>/<name>` rather than the bare name, because the ref carries a
namespace and a bare name would collide across namespaces. Both indexes stay
local: they are indexes on a CR kind, which no target cluster holds, and
registering them on the fleet would fail the engagement of every target cluster.

It `Owns` its Deployment, Service, ConfigMap, Secret, Job, CronJob,
PodDisruptionBudget, HorizontalPodAutoscaler and NetworkPolicy. The HTTPRoute
joins that set only when the Gateway API kind is present on the management
cluster, probed at setup through the RESTMapper. Beyond the owned set it watches:

- **Secrets**, over three legs. The CRs that reference the Secret by name,
  resolved through the `spec.secretRefs.name` index; the CRs that own it through
  an OwnerReference, which covers the three derived Secrets; and the CRs driving
  an `OVNCentral` that published this Secret as its client identity. The third
  leg crosses namespaces, because the central and the CRs waiting on its
  certificate are rarely in the same one. The results are unioned by name, so a
  Secret matching several legs yields one request. ESO-managed Secrets are owned
  by the ExternalSecret controller, so an owner-reference watch alone would never
  match them.
- **MariaDB** clusters named by `spec.database.clusterRef`, so an upstream
  database outage reflects in `DatabaseReady` without waiting for the periodic
  requeue.
- Both the cluster-scoped `ClusterSecretStore` and the namespaced `SecretStore` a
  `Neutron` can select, so a backend outage reflects in `SecretsReady` as soon as
  ESO flips the store's Ready condition. A CR that omits `spec.secretStoreRef`
  resolves to the shared cluster store, so the default fan-out is preserved while
  a CR pinned to a namespaced store is woken only by its own.
- **OVNCentral**, mapped through the `spec.ovn.centralRef` index to every
  `Neutron` driving it, listed cluster-wide because the ref is not
  namespace-bound. The leg carries no generation predicate: what the Neutrons wait
  on are the central's status flips, the two addresses being published and the
  client Secret being named, and those leave the generation untouched. It needs no
  remote counterpart, since the central lives on the management cluster whatever
  cluster the children land on.

The `NeutronMetadataAgent` controller `Owns` its DaemonSet, ConfigMap and Secret,
and registers three indexes: `spec.secretRefs.name` and `spec.chassisRef.name` on
the agent, and `spec.centralRef.name` on the `OVNChassis` its second OVN leg hops
over. That third index goes on this manager's own cache, so the ovn-operator that
owns the kind neither has to register it nor conflicts with it. The controller
adds three watches:

- **Secrets**, over the two shared legs alone: the agents that name a Secret in
  `spec.novaMetadata.sharedSecretRef` or `spec.messaging.secretRef`, and the
  agents that own the derived transport-URL Secret. Both are namespace-scoped, as
  an agent only ever references Secrets beside itself.
- **OVNChassis**, mapped through the `spec.chassisRef.name` index to the agents
  in that namespace.
- **OVNCentral**, mapped in two hops: the `OVNChassis` in the central's namespace
  whose `spec.centralRef` names it, then the agents indexed under each of those.
  The hop is what makes the leg necessary at all. An agent names a chassis and
  not a central, while the two values its pods cannot start without live on the
  central's status. Both hops resolve through a field index, so the leg copies
  the chassis it needs rather than every one in the namespace, each of which
  carries one status entry per node it selects.

Both controllers watch their children a second time on the clusters a CR can
project onto (`AddRemoteChildWatches`), and register their input watches on both
sides through `AddInputWatch`. An owner reference does not cross a cluster
boundary, so the ownership labels are what map a remote child back to its CR.
Legs on a target cluster are engaged on all of them, so they drop the events
belonging to a CR that projects somewhere else. The remote child kinds are also
the kinds each deletion sweeps by ownership label: for a `Neutron` the three
Deployments, the Service, the ConfigMaps, the Secrets, the Jobs and the CronJob,
the PodDisruptionBudget, the HorizontalPodAutoscaler, the NetworkPolicy, the
HTTPRoute and the three MariaDB CRs; for a `NeutronMetadataAgent` the DaemonSet,
the ConfigMaps and the derived Secret. A kind missing from either list is a kind
that keeps running after its CR is gone.
