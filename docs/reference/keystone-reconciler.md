---
title: Keystone Reconciler Architecture
quadrant: operator
feature: CC-0013, CC-0015, CC-0038, CC-0057, CC-0058, CC-0064, CC-0067, CC-0068
---

# Keystone Reconciler Architecture

Reference documentation for the KeystoneReconciler and its sub-reconciler contracts
(CC-0013). The KeystoneReconciler implements the control loop that drives a Keystone
CR from desired state to a fully operational Keystone Identity Service deployment.

For CRD type definitions and webhooks, see
[Keystone CRD API Reference](./keystone-crd.md). For the shared library functions
used by sub-reconcilers, see
[Kubernetes-Interacting Packages](./kubernetes-packages.md).

## Controller Registration

The KeystoneReconciler is registered with the controller manager in
`operators/keystone/main.go` via the shared bootstrap package:

```go
import (
    keystonev1alpha1 "github.com/c5c3/forge/operators/keystone/api/v1alpha1"
    "github.com/c5c3/forge/operators/keystone/internal/controller"
)

// In init():
utilruntime.Must(keystonev1alpha1.AddToScheme(scheme))
utilruntime.Must(esov1alpha1.SchemeBuilder.AddToScheme(scheme))
utilruntime.Must(esov1beta1.SchemeBuilder.AddToScheme(scheme))
utilruntime.Must(mariadbv1alpha1.AddToScheme(scheme))

// In SetupFunc:
(&controller.KeystoneReconciler{
    Client:   mgr.GetClient(),
    Scheme:   mgr.GetScheme(),
    Recorder: mgr.GetEventRecorder("keystone-controller"),
}).SetupWithManager(mgr)
```

### Scheme Registration

The operator registers these external schemes in `init()` to support typed
interactions with external operator CRDs:

| Module | Scheme | Types Used |
| --- | --- | --- |
| `github.com/external-secrets/external-secrets` | `esov1alpha1.SchemeBuilder` | `PushSecret` |
| `github.com/external-secrets/external-secrets` | `esov1beta1.SchemeBuilder` | `ExternalSecret` |
| `github.com/mariadb-operator/mariadb-operator` | `mariadbv1alpha1.AddToScheme` | `Database`, `User`, `Grant` |

> **Note:** ESO uses separate `v1beta1` and `v1alpha1` scheme builders (not a single
> `AddToScheme`). Both must be registered independently.

### Watches

The controller watches the primary Keystone CR and all owned resources:

| Resource | Watch Type | Effect |
| --- | --- | --- |
| `Keystone` | `For()` | Triggers reconciliation on CR changes |
| `Deployment` | `Owns()` | Triggers reconciliation when owned Deployment changes |
| `Service` | `Owns()` | Triggers reconciliation when owned Service changes |
| `ConfigMap` | `Owns()` | Triggers reconciliation when owned ConfigMap changes |
| `Job` | `Owns()` | Triggers reconciliation when owned Job changes |
| `PodDisruptionBudget` | `Owns()` | Triggers reconciliation when owned PDB changes |
| `HorizontalPodAutoscaler` | `Owns()` | Triggers reconciliation when owned HPA changes |
| `CronJob` | `Owns()` | Triggers reconciliation when owned CronJob changes |
| `Secret` | `Watches()` | Triggers reconciliation for controller-owned Secrets only |
| `MariaDB` | `Watches()` | Propagates upstream DB cluster health into `DatabaseReady` (CC-0047) |
| `ClusterSecretStore` | `Watches()` | Propagates OpenBao-backend health into `SecretsReady` (CC-0047) |

Secrets use `Watches()` with `handler.OnlyControllerOwner()` instead of `Owns()` because
some Secrets (ESO-provided credentials) are not owned by the Keystone CR but still need
to trigger reconciliation. The `MariaDB` and `ClusterSecretStore` watches exist so the
operator reacts immediately to upstream dependency outages without waiting for the next
periodic requeue (CC-0047).

---

## Reconciler Struct

```go
type KeystoneReconciler struct {
    client.Client
    Scheme     *runtime.Scheme
    Recorder   record.EventRecorder
    HTTPClient HTTPDoer
}
```

| Field | Type | Purpose |
| --- | --- | --- |
| `Client` | `client.Client` | Kubernetes API client for CRUD operations |
| `Scheme` | `*runtime.Scheme` | Runtime scheme for owner reference resolution |
| `Recorder` | `record.EventRecorder` | Records Kubernetes events for state transitions |
| `HTTPClient` | `HTTPDoer` | Injectable HTTP client for health checks; falls back to `http.DefaultClient` when nil (CC-0067) |

---

## RBAC Permissions

RBAC markers on the reconciler generate the required ClusterRole:

| API Group | Resources | Verbs |
| --- | --- | --- |
| `keystone.openstack.c5c3.io` | `keystones` | get, list, watch, create, update, patch, delete |
| `keystone.openstack.c5c3.io` | `keystones/status` | get, update, patch |
| `keystone.openstack.c5c3.io` | `keystones/finalizers` | update |
| `apps` | `deployments` | get, list, watch, create, update, patch, delete |
| `core` | `services`, `configmaps`, `secrets` | get, list, watch, create, update, patch, delete |
| `batch` | `jobs`, `cronjobs` | get, list, watch, create, update, patch, delete |
| `k8s.mariadb.com` | `databases`, `users`, `grants` | get, list, watch, create, update, patch, delete |
| `k8s.mariadb.com` | `mariadbs` | get, list, watch |
| `external-secrets.io` | `externalsecrets`, `pushsecrets` | get, list, watch, create, update, patch |
| `external-secrets.io` | `clustersecretstores` | get, list, watch |
| `policy` | `poddisruptionbudgets` | get, list, watch, create, update, patch, delete |
| `autoscaling` | `horizontalpodautoscalers` | get, list, watch, create, update, patch, delete |

---

## Reconciliation Flow

```text
┌──────────────────────────────────────────────────────────────────────────────┐
│                       KEYSTONE RECONCILIATION FLOW                          │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  Keystone CR changed (or requeue timer fires)                                │
│         │                                                                    │
│         ▼                                                                    │
│  Fetch Keystone CR (return empty result if NotFound)                         │
│         │                                                                    │
│         ▼                                                                    │
│  ┌──────────────────┐                                                        │
│  │ reconcileSecrets │  Check ESO ExternalSecrets are synced                  │
│  │                  │  Sets: SecretsReady                                    │
│  └────────┬─────────┘  Requeue: 15s                                          │
│           │                                                                  │
│           ▼                                                                  │
│  ┌───────────────────┐                                                       │
│  │ reconcileDatabase │  Managed mode: verify MariaDB cluster health first,   │
│  │                   │  then ensure Database/User/Grant CRs + run db_sync    │
│  │                   │  Job + run schema-check Job (CC-0064)                 │
│  │                   │  Sets: DatabaseReady                                  │
│  └────────┬──────────┘  Requeue: 30s                                         │
│           │                                                                  │
│           ▼                                                                  │
│  ┌─────────────────────┐                                                     │
│  │ reconcileFernetKeys │  Generate keys, CronJob, PushSecret                │
│  │                     │  Sets: FernetKeysReady                              │
│  └────────┬────────────┘  Requeue: none                                      │
│           │                                                                  │
│           ▼                                                                  │
│  ┌──────────────────┐                                                        │
│  │ reconcileConfig  │  Render keystone.conf + api-paste.ini                  │
│  │                  │  Create immutable ConfigMap                             │
│  └────────┬─────────┘  Returns: configMapName                                │
│           │                                                                  │
│           ▼                                                                  │
│  ┌─────────────────────────────────┐                                     │
│  │ reconcilePolicyValidation       │  Validate oslo.policy overrides     │
│  │                                 │  via oslopolicy-validator Job       │
│  │                                 │  Sets: PolicyValidReady             │
│  └────────┬────────────────────────┘  Requeue: 15s                       │
│           │                                                              │
│           ▼                                                              │
│  ┌──────────────────────┐                                                    │
│  │ reconcileDeployment  │  Ensure Deployment + Service                       │
│  │                      │  Sets: DeploymentReady, status.endpoint            │
│  └────────┬─────────────┘  Requeue: 10s                                      │
│           │                                                                  │
│           ▼                                                                  │
│  ┌────────────────────────┐                                                  │
│  │ reconcileHealthCheck   │  HTTP GET to Status.Endpoint                     │
│  │                        │  Sets: KeystoneAPIReady                          │
│  └────────┬───────────────┘  Requeue: 10s (CC-0067)                          │
│           │                                                                  │
│           ▼                                                                  │
│  ┌──────────────┐                                                            │
│  │ reconcileHPA │  Create/update/delete HPA based on spec.autoscaling        │
│  │              │  Sets: HPAReady                                            │
│  └────────┬─────┘  Requeue: none                                             │
│           │                                                                  │
│           ▼                                                                  │
│  ┌─────────────────────┐                                                     │
│  │ reconcileBootstrap  │  Run keystone-manage bootstrap Job                  │
│  │                     │  Sets: BootstrapReady                                │
│  └────────┬────────────┘  Requeue: 60s                                       │
│           │                                                                  │
│           ▼                                                                  │
│  ┌────────────────────────┐                                                  │
│  │ reconcileTrustFlush    │  Create/delete trust_flush CronJob               │
│  │                        │  Sets: TrustFlushReady                           │
│  └────────┬───────────────┘  Requeue: none                                   │
│           │                                                                  │
│           ▼                                                                  │
│  setReadyCondition() — aggregate Ready from all sub-conditions               │
│  updateStatus() — persist to API server                                      │
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Sequential Execution Contract

Sub-reconcilers execute **strictly sequentially**. Each sub-reconciler is called only
if all previous sub-reconcilers succeeded without requesting a requeue. The call
pattern for each sub-reconciler (except `reconcileConfig`) is:

```go
if result, err := r.reconcileX(ctx, &keystone); err != nil || result.RequeueAfter > 0 {
    return r.updateStatus(ctx, &keystone, result, err)
}
```

This guarantees:

1. A sub-reconciler error **propagates immediately** — subsequent sub-reconcilers are
   skipped.
2. A requeue result (`RequeueAfter > 0`) causes an **early return** — status is
   persisted and the reconciler exits.
3. Status conditions from the failing/requeuing sub-reconciler are **always persisted**
   via `updateStatus()` before returning.

### Status Update Pattern

`updateStatus()` persists all condition changes via `r.Status().Update()` and returns
the provided `(result, error)` pair unchanged. If the status update itself fails, the
behavior depends on whether a reconcile error is also present (CC-0068):

| reconcileErr | Status().Update() | Returned error |
| --- | --- | --- |
| nil | succeeds | nil |
| non-nil | succeeds | reconcileErr (unchanged) |
| nil | fails | `fmt.Errorf("updating status: %w", statusErr)` |
| non-nil | fails | `errors.Join(reconcileErr, fmt.Errorf("updating status: %w", statusErr))` |

In the dual-failure case, `errors.Join` preserves both the original reconcile error and
the status update error. Both are unwrappable via `errors.Is` and `errors.As`, and both
appear in the controller-runtime log output (separated by newline). The reconcile error
appears first in the joined error string for readability. When `reconcileErr` is nil,
`errors.Join(nil, statusErr)` discards the nil argument per the Go spec, returning a
joined error containing only the status update error.

### Ready Condition Aggregation

After all sub-reconcilers succeed, `setReadyCondition()` evaluates whether all
sub-condition types are `True` using `conditions.AllTrue()`:

| All Sub-Conditions True | Ready Condition | Reason |
| --- | --- | --- |
| Yes | `Status: True` | `AllReady` |
| No (any missing or False) | `Status: False` | `NotAllReady` |

The `Ready` condition includes `ObservedGeneration` set to `keystone.Generation` so
clients can detect stale status.

---

## Sub-Reconciler Contracts

All sub-reconcilers are private methods on the `KeystoneReconciler` receiver. Each
sub-reconciler is responsible for:

1. Ensuring the resources it manages exist with the correct spec.
2. Setting its designated status condition with a descriptive `Reason` and `Message`.
3. Returning `(ctrl.Result{RequeueAfter: N}, nil)` for transient not-ready states.
4. Returning `(ctrl.Result{}, error)` for failures.
5. Returning `(ctrl.Result{}, nil)` when its phase is complete.

### Sub-Condition Types

| Condition Type | Set By | Description |
| --- | --- | --- |
| `SecretsReady` | `reconcileSecrets` | ESO-provided credentials are synced |
| `DatabaseReady` | `reconcileDatabase` | MariaDB CRs ready and db_sync complete |
| `FernetKeysReady` | `reconcileFernetKeys` | Fernet Secret, CronJob, and PushSecret ensured |
| `PolicyValidReady` | `reconcilePolicyValidation` | Policy override validation passed or not required (CC-0058) |
| `DeploymentReady` | `reconcileDeployment` | Deployment available and Service created |
| `KeystoneAPIReady` | `reconcileHealthCheck` | Keystone API responding to HTTP health check (CC-0067) |
| `HPAReady` | `reconcileHPA` | HPA configured or not required |
| `BootstrapReady` | `reconcileBootstrap` | Bootstrap Job completed successfully |
| `TrustFlushReady` | `reconcileTrustFlush` | Trust flush CronJob configured or not required (CC-0057) |

---

### reconcileSecrets

**File:** `operators/keystone/internal/controller/reconcile_secrets.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcileSecrets(ctx context.Context,
    keystone *keystonev1alpha1.Keystone) (ctrl.Result, error)
```

**Purpose:** Verify that ESO has synced credentials from OpenBao before proceeding.
This sub-reconciler does not create any resources — it only checks readiness of
ExternalSecrets managed by the External Secrets Operator.

**Checks (in order):**

| Step | Resource | Source |
| --- | --- | --- |
| 0 | `ClusterSecretStore openbao-cluster-store` | Ready condition (CC-0047) |
| 1 | DB credentials `ExternalSecret` | `spec.database.secretRef.name` |
| 2 | Admin credentials `ExternalSecret` | `spec.bootstrap.adminPasswordSecretRef.name` |

The `ClusterSecretStore` check runs first so upstream OpenBao outages surface
as `SecretsReady=False` immediately. Per-ExternalSecret `Ready` conditions
alone would mask outages up to the ESO `refreshInterval` (1h) because the
cached Secret remains valid (CC-0047).

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `SecretStoreNotReady` | `"ClusterSecretStore \"openbao-cluster-store\" is not ready; upstream secret backend unreachable"` | 15s |
| `False` | `WaitingForDBCredentials` | "Waiting for ESO to sync database credentials from OpenBao" | 15s |
| `False` | `WaitingForAdminCredentials` | "Waiting for ESO to sync admin credentials from OpenBao" | 15s |
| `True` | `SecretsAvailable` | — | — |

**Error handling:** API errors from `secrets.IsClusterSecretStoreReady()` and
`secrets.WaitForExternalSecret()` are returned directly (no condition set),
causing controller-runtime exponential backoff.

**Shared library calls:** `secrets.IsClusterSecretStoreReady()`,
`secrets.WaitForExternalSecret()`

---

### reconcileDatabase

**File:** `operators/keystone/internal/controller/reconcile_database.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcileDatabase(ctx context.Context,
    keystone *keystonev1alpha1.Keystone) (ctrl.Result, error)
```

**Purpose:** Provision the Keystone database schema and verify schema integrity.
Supports two modes:

- **Managed mode** (`spec.database.clusterRef` set): Creates MariaDB Database, User,
  and Grant CRs within the referenced cluster, then runs `db_sync`, then runs
  schema-check (CC-0064).
- **Brownfield mode** (`spec.database.host` set): Skips MariaDB CRs entirely and runs
  `db_sync` directly against the external database, then runs schema-check (CC-0064).

After `db_sync` completes successfully, a schema-check Job verifies that the database
schema matches the expected Alembic migration head. See
[Schema Drift Detection](./keystone-schema-drift-detection.md) for details.

**Managed Mode Resources:**

| Resource | Name | Key Spec Fields |
| --- | --- | --- |
| `Database` | `keystone` | CharacterSet: `utf8mb4`, Collate: `utf8mb4_general_ci`, MariaDBRef from `spec.database.clusterRef` |
| `User` | `keystone` | Password from `spec.database.secretRef`, MariaDBRef from `spec.database.clusterRef` |
| `Grant` | `keystone` | Privileges: `ALL PRIVILEGES`, Database: `keystone`, Table: `*`, Username: `keystone` |

**db_sync Job:**

| Field | Value |
| --- | --- |
| Name | `keystone-db-sync` |
| Image | `{spec.image.repository}:{spec.image.tag}` |
| Command | `keystone-manage db_sync` |
| BackoffLimit | 4 |
| RestartPolicy | OnFailure |

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `WaitingForDatabase` | "MariaDB Database CR is not ready" | 30s |
| `False` | `WaitingForDatabase` | "MariaDB User or Grant CR is not ready" | 30s |
| `False` | `DBSyncInProgress` | "db_sync job is running" | 30s |
| `False` | `DBSyncFailed` | "db_sync job failed: {error}" | — (error returned) |
| `False` | `SchemaCheckInProgress` | "schema-check job is running" | 30s |
| `False` | `SchemaDriftDetected` | "schema-check job failed: {error}" | — (error returned) |
| `True` | `DatabaseSynced` | "Database schema is up to date (revision verified)" | — |

**Error handling:** Errors from `database.EnsureDatabase()`,
`database.EnsureDatabaseUser()`, and `database.RunDBSyncJob()` are wrapped with
context and returned. The `DBSyncFailed` condition is set before returning the error
so that the failure reason is visible in the CR status.

**Shared library calls:** `database.EnsureDatabase()`, `database.EnsureDatabaseUser()`,
`database.RunDBSyncJob()`, `job.RunJob()` (schema-check, CC-0064)

---

### reconcileFernetKeys

**File:** `operators/keystone/internal/controller/reconcile_fernet.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcileFernetKeys(ctx context.Context,
    keystone *keystonev1alpha1.Keystone) (ctrl.Result, error)
```

**Purpose:** Manage Fernet token signing keys — initial generation, rotation schedule,
and disaster recovery backup to OpenBao.

**Steps (in order):**

1. **Ensure Fernet keys Secret** — If `keystone-fernet-keys` Secret does not exist,
   generate initial keys and create the Secret with a controller owner reference.
2. **Ensure rotation CronJob** — Create or update `keystone-fernet-rotate` CronJob
   with the schedule from `spec.fernet.rotationSchedule`.
3. **Ensure PushSecret** — Create or update `keystone-fernet-keys-backup` PushSecret
   targeting `kv-v2/data/openstack/keystone/fernet-keys` in the `openbao`
   ClusterSecretStore.

**Key Generation:**

| Property | Value |
| --- | --- |
| Algorithm | `crypto/rand` (32 bytes) |
| Encoding | URL-safe base64 without padding (`base64.URLEncoding.WithPadding(base64.NoPadding)`) |
| Key count | `max(spec.fernet.maxActiveKeys, 3)` |
| Secret data keys | String indices: `"0"`, `"1"`, `"2"`, ... |
| Secret name | `keystone-fernet-keys` |

**Rotation CronJob:**

| Field | Value |
| --- | --- |
| Name | `keystone-fernet-rotate` |
| Schedule | `spec.fernet.rotationSchedule` |
| Command | `keystone-manage fernet_rotate --keystone-user keystone --keystone-group keystone` |
| Volume | `keystone-fernet-keys` Secret at `/etc/keystone/fernet-keys` |

**PushSecret:**

| Field | Value |
| --- | --- |
| Name | `keystone-fernet-keys-backup` |
| Store | `ClusterSecretStore/openbao` |
| Source Secret | `keystone-fernet-keys` |
| Remote Key | `kv-v2/data/openstack/keystone/fernet-keys` |

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `GeneratingKeys` | "Initial Fernet keys have been generated" | — |
| `True` | `FernetKeysAvailable` | "Fernet keys Secret exists and rotation CronJob is configured" | — |

**Error handling:** Errors from Secret creation, CronJob ensure, or PushSecret ensure
are wrapped with context and returned directly. No requeue delays — errors trigger
controller-runtime exponential backoff.

**Idempotency:** If the `keystone-fernet-keys` Secret already exists, it is not
modified. This prevents overwriting keys that have been rotated by the CronJob.

**Shared library calls:** `job.EnsureCronJob()`, `secrets.EnsurePushSecret()`

---

### reconcileConfig

**File:** `operators/keystone/internal/controller/reconcile_config.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcileConfig(ctx context.Context,
    keystone *keystonev1alpha1.Keystone) (string, error)
```

**Purpose:** Build the Keystone configuration files and store them in an immutable
ConfigMap with a content-hash suffix. Returns the ConfigMap name for use by
`reconcileDeployment`.

> **Note:** This sub-reconciler has a different signature — it returns
> `(string, error)` instead of `(ctrl.Result, error)`. It does not set any status
> condition and does not request requeues.

**Configuration Pipeline:**

```text
Spec fields → Operator defaults → Secret injection → Plugin config merge
  → ExtraConfig merge → Policy override → INI rendering → Immutable ConfigMap
```

**Step 1: Build operator defaults**

The following INI sections are generated from CRD spec fields:

| Section | Key | Value Source |
| --- | --- | --- |
| `DEFAULT` | `log_config_append` | `/etc/keystone/logging.conf` (hardcoded) |
| `token` | `provider` | `fernet` (hardcoded) |
| `fernet_tokens` | `key_repository` | `/etc/keystone/fernet-keys` (hardcoded) |
| `fernet_tokens` | `max_active_keys` | `spec.fernet.maxActiveKeys` |
| `cache` | `enabled` | `true` (hardcoded) |
| `cache` | `backend` | `spec.cache.backend` |
| `cache` | `memcache_servers` | Resolved from spec (see below) |
| `memcache` | `servers` | Same as `cache.memcache_servers` |
| `oslo_middleware` | `enable_proxy_headers_parsing` | `true` (hardcoded) |
| `identity` | `default_domain_id` | `default` (hardcoded) |
| `database` | `connection` | Resolved connection string (see below) |
| `database` | `max_retries` | `-1` (hardcoded) |
| `database` | `connection_recycle_time` | `600` (hardcoded) |

**Step 2: Resolve cache servers**

| Mode | Source | Format |
| --- | --- | --- |
| Brownfield | `spec.cache.servers` | Comma-joined list |
| Managed | `spec.cache.clusterRef.name` | `memcached-0.{name}:11211,memcached-1.{name}:11211,memcached-2.{name}:11211` |

**Step 3: Resolve database connection string**

| Mode | Host Resolution | Format |
| --- | --- | --- |
| Managed | `{clusterRef.name}.{namespace}.svc:{port}` | `mysql+pymysql://{username}:{password}@{host}:{port}/keystone` |
| Brownfield | `{spec.database.host}:{port}` | `mysql+pymysql://{username}:{password}@{host}:{port}/keystone` |

- Username and password are read from `spec.database.secretRef` via
  `secrets.GetSecretValue()`.
- Default port is 3306 when `spec.database.port` is 0.

**Step 4: Merge plugin config** — If `spec.plugins` is non-empty,
`plugins.RenderPluginConfig()` generates INI sections that are merged with
`config.MergeDefaults()`.

**Step 5: Merge extraConfig** — If `spec.extraConfig` is set, it is merged as the
**higher-priority** input to `config.MergeDefaults()`, so user-provided values override
operator defaults.

**Step 6: Handle policyOverrides** — If `spec.policyOverrides` is set:

1. Load external rules from `configMapRef` (if set) via
   `policy.LoadPolicyFromConfigMap()`.
2. Merge inline `rules` over external rules (inline wins).
3. Render `policy.yaml` via `policy.RenderPolicyYAML()`.
4. Inject `oslo_policy` section into `keystone.conf` via
   `config.InjectOsloPolicyConfig()` with path `/etc/keystone/policy.yaml`.

**Step 7: Render api-paste.ini** — Uses `plugins.RenderPastePipelineINI()` with:

| Field | Value |
| --- | --- |
| Pipeline name | `public_api` |
| App name | `admin_service` |
| Base filters | `cors`, `sizelimit`, `http_proxy_to_wsgi`, `osprofiler`, `url_normalize`, `request_id`, `authtoken` |
| Middleware | `spec.middleware` |

**Step 8: Create immutable ConfigMap**

| Field | Value |
| --- | --- |
| Base name | `keystone-config` |
| Actual name | `keystone-config-{8-char-sha256}` |
| Data keys | `keystone.conf`, `api-paste.ini`, `policy.yaml` (if policyOverrides set) |
| Immutable | `true` |

**Error handling:** All errors are wrapped with context and returned. No conditions
are set by this sub-reconciler.

**Shared library calls:** `secrets.GetSecretValue()`, `config.InjectSecrets()`,
`config.MergeDefaults()`, `config.InjectOsloPolicyConfig()`, `config.RenderINI()`,
`config.CreateImmutableConfigMap()`, `plugins.RenderPluginConfig()`,
`plugins.RenderPastePipelineINI()`, `policy.LoadPolicyFromConfigMap()`,
`policy.RenderPolicyYAML()`

---

### reconcilePolicyValidation

**File:** `operators/keystone/internal/controller/reconcile_policyvalidation.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcilePolicyValidation(ctx context.Context,
    keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error)
```

**Purpose:** Validate custom oslo.policy overrides via `oslopolicy-validator` before the
Deployment is updated (CC-0058). This sub-reconciler gates the Deployment rollout:
invalid policy overrides are caught before reaching running pods. Two lifecycle paths:

1. **No policy overrides** (`spec.policyOverrides` is nil): Delete any existing
   validation Job and set `PolicyValidReady=True` with reason `NotRequired`.
2. **Policy overrides set** (`spec.policyOverrides` is set): Build and run a validation
   Job via `job.RunJob()`. Track lifecycle through InProgress → Passed/Failed states.

> **Note:** This sub-reconciler accepts the `configMapName` returned by
> `reconcileConfig` to mount the correct immutable ConfigMap (containing the rendered
> `policy.yaml`) in the validation Job pod spec.

**Validation Job:**

| Field | Value |
| --- | --- |
| Name | `{name}-policy-validation` |
| Image | `{spec.image.repository}:{spec.image.tag}` |
| Command | `oslopolicy-validator --namespace keystone --config-dir /etc/keystone/keystone.conf.d/` |
| BackoffLimit | 2 |
| TTLSecondsAfterFinished | 300 |
| RestartPolicy | Never |
| SecurityContext | `restrictedSecurityContext()` (PSS Restricted) |
| TerminationMessagePolicy | `FallbackToLogsOnError` |

**Volume Mounts:**

| Volume Name | Mount Path | Source | ReadOnly |
| --- | --- | --- | --- |
| `config` | `/etc/keystone/keystone.conf.d/` | ConfigMap `{configMapName}` | Yes |

**Error Extraction (`getValidationErrorMessage`):**

When a validation Job fails, the reconciler extracts a descriptive error message from the
failed Pod's termination message (CC-0058, REQ-006):

1. Lists Pods by `job-name` label selector in the Job's namespace.
2. Sorts by creation timestamp (most recent first).
3. Searches container termination messages for a non-empty `Terminated.Message`.
4. Truncates to 500 characters if the message exceeds that length.
5. Falls back to a `kubectl logs` reference if no termination message is available.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `True` | `NotRequired` | "No policy overrides configured" | — |
| `False` | `PolicyValidationInProgress` | "Policy validation job is running" | 15s |
| `True` | `PolicyValidationPassed` | "Policy validation completed successfully" | — |
| `False` | `PolicyValidationFailed` | Descriptive error from Pod termination message | — (error returned) |

**Error handling:** Errors from `job.RunJob()` are wrapped with "running policy validation"
context. When `job.ErrJobFailed` is detected, `getValidationErrorMessage()` extracts the
failure detail before setting the condition. The `PolicyValidationFailed` condition is set
before returning the error so that the failure reason is visible in the CR status.

**Shared library calls:** `job.RunJob()`

---

### reconcileDeployment

**File:** `operators/keystone/internal/controller/reconcile_deployment.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcileDeployment(ctx context.Context,
    keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error)
```

**Purpose:** Ensure the Keystone API Deployment and Service exist with the correct
spec. Sets `status.endpoint` when the Deployment becomes available.

> **Note:** This sub-reconciler accepts the `configMapName` returned by
> `reconcileConfig` to reference the correct immutable ConfigMap in volume mounts.

**Fernet Keys Hash Annotation (CC-0015):**

Before building the Deployment, `reconcileDeployment` calls `fernetKeysHash()` to
compute a SHA-256 digest of the `{name}-fernet-keys` Secret data. This hash is set
as the pod template annotation `keystone.c5c3.io/fernet-keys-hash`, which triggers a
rolling restart whenever the Fernet rotation CronJob updates the Secret.

```text
CronJob rotates keys → Secret data changes → secretToKeystoneMapper watch
  → Reconcile() → reconcileDeployment() reads Secret → computes hash
  → annotation value changes → Kubernetes triggers rolling restart
```

The `fernetKeysHash()` helper:

| Behavior | Detail |
| --- | --- |
| Secret name | `{keystone.Name}-fernet-keys` |
| Algorithm | SHA-256 of `json.Marshal(secret.Data)` |
| Output | Hex-encoded digest (64 characters) |
| Secret not found | Returns empty string (no error) — safe because `reconcileFernetKeys` runs before `reconcileDeployment` |
| Determinism | `json.Marshal` on `map[string][]byte` sorts keys alphabetically |

**Deployment Spec:**

| Field | Value |
| --- | --- |
| Name | `keystone-api` |
| Replicas | `spec.replicas` |
| Labels | `app.kubernetes.io/name=keystone`, `app.kubernetes.io/instance={name}`, `app.kubernetes.io/managed-by=keystone-operator` |
| Selector | `app.kubernetes.io/name=keystone`, `app.kubernetes.io/instance={name}` |
| Container name | `keystone-api` |
| Image | `{spec.image.repository}:{spec.image.tag}` |
| Port | 5000 (named `keystone-api`) |

**Pod Template Annotations:**

| Annotation | Value | Purpose |
| --- | --- | --- |
| `keystone.c5c3.io/fernet-keys-hash` | SHA-256 hex digest of fernet-keys Secret data | Triggers rolling restart on Fernet key rotation (CC-0015) |

**Probes:**

| Probe | Type | Target | Port | InitialDelay | Period |
| --- | --- | --- | --- | --- | --- |
| Liveness | TCPSocket | — | 5000 | 15s | 20s |
| Readiness | HTTPGet | `/v3` | 5000 | 5s | 10s |

The liveness and readiness probes are intentionally separated (CC-0062). The liveness probe uses a TCP socket check that only verifies the uWSGI process is accepting connections, without exercising the database code path. This prevents the kubelet from killing pods during transient database outages (e.g., MariaDB maintenance), avoiding CrashLoopBackOff cascades and thundering-herd restarts. The readiness probe continues to use HTTP GET `/v3`, which exercises the full stack including the database. When the database is unavailable, the readiness probe fails and the pod is removed from Service endpoints, preventing HTTP 500 responses to clients. Once the database recovers, the pod re-enters Service endpoints within one readiness probe period (10s).

**Volume Mounts:**

| Volume Name | Mount Path | Source | ReadOnly |
| --- | --- | --- | --- |
| `config` | `/etc/keystone/keystone.conf.d/` | ConfigMap `{configMapName}` | Yes |
| `fernet-keys` | `/etc/keystone/fernet-keys/` | Secret `keystone-fernet-keys` | Yes |
| `credential-keys` | `/etc/keystone/credential-keys/` | Secret `keystone-credential-keys` | Yes |

**Service Spec:**

| Field | Value |
| --- | --- |
| Name | `keystone-api` |
| Selector | `app.kubernetes.io/name=keystone`, `app.kubernetes.io/instance={name}` |
| Port | 5000 TCP |

**Status Endpoint:**

When the Deployment becomes ready, `status.endpoint` is set to:

```
http://keystone-api.{namespace}.svc.cluster.local:5000/v3
```

**PodDisruptionBudget (CC-0037):**

After ensuring the Deployment and Service, `reconcileDeployment` creates or updates
a PodDisruptionBudget via `deployment.EnsurePDB()`. The PDB uses a replica-aware
disruption budget strategy:

| Replicas | Field | Value | Rationale |
| --- | --- | --- | --- |
| `> 1` | `minAvailable` | `1` | Guarantees at least one pod remains during voluntary disruptions |
| `<= 1` | `maxUnavailable` | `1` | Avoids drain deadlock — a PDB with `minAvailable=1` on a single-replica deployment would block all evictions |

| PDB Field | Value |
| --- | --- |
| Name | `{name}-api` |
| Labels | Same as Deployment (`commonLabels`) |
| Selector | Same as Deployment (`selectorLabels`) |

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `WaitingForDeployment` | "Keystone API deployment is not yet available" | 10s |
| `True` | `DeploymentReady` | "Keystone API deployment is available" | — |

**Error handling:** Errors from `deployment.EnsureDeployment()`,
`deployment.EnsureService()`, and `deployment.EnsurePDB()` are wrapped with context
and returned.

**Shared library calls:** `deployment.EnsureDeployment()`,
`deployment.EnsureService()`, `deployment.EnsurePDB()`

---

### reconcileHealthCheck

**File:** `operators/keystone/internal/controller/reconcile_healthcheck.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcileHealthCheck(ctx context.Context,
    keystone *keystonev1alpha1.Keystone) (ctrl.Result, error)
```

**Purpose:** Perform an HTTP GET to the Keystone `/v3` endpoint after the Deployment
reports ready, verifying that the API is actually responding to requests (CC-0067).
This catches cases where pods pass their readiness probe but the API is not
functionally healthy.

**Endpoint:** Uses `keystone.Status.Endpoint` which is set by `reconcileDeployment`
(e.g., `http://keystone-api.{namespace}.svc.cluster.local:5000/v3`). If the endpoint
is empty (not yet configured), the health check sets `KeystoneAPIReady=False` with
reason `EndpointNotReady` and requeues.

**HTTPDoer Interface:**

```go
type HTTPDoer interface {
    Do(*http.Request) (*http.Response, error)
}
```

The `HTTPDoer` interface abstracts `*http.Client` so that tests can inject mock HTTP
servers via `httptest.NewServer`. The `httpClient()` helper returns the injected
`HTTPClient` field if non-nil, otherwise falls back to `http.DefaultClient`.

**Timeout:** The HTTP request uses a derived context with `HealthCheckTimeout`
(10 seconds), preventing a hanging Keystone API from blocking the reconcile loop.

**Error Classification:**

Network errors are classified into descriptive condition reasons via
`classifyHealthCheckError()`:

| Error Type | Detection | Reason | Message |
| --- | --- | --- | --- |
| Context deadline exceeded | `errors.Is(err, context.DeadlineExceeded)` | `HealthCheckTimeout` | `"health check timed out"` |
| DNS resolution failure | `errors.As(err, &net.DNSError)` | `EndpointNotReady` | `"endpoint not resolvable"` |
| Connection refused | `strings.Contains(err.Error(), "connection refused")` | `ConnectionFailed` | `"connection failed: {error}"` |
| Other network error | fallthrough | `HealthCheckFailed` | `"health check failed: {error}"` |

All network errors result in a condition False + requeue, **not** a returned error.
Returning network errors would trigger controller-runtime's exponential backoff,
delaying recovery. Instead, a descriptive condition is set and the reconciler requeues
at a fixed interval (`RequeueHealthCheck = 10s`).

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `EndpointNotReady` | `"endpoint not yet configured"` | 10s |
| `False` | `EndpointNotReady` | `"endpoint not resolvable"` | 10s |
| `False` | `HealthCheckTimeout` | `"health check timed out"` | 10s |
| `False` | `ConnectionFailed` | `"connection failed: {error}"` | 10s |
| `False` | `HealthCheckFailed` | `"health check failed: {error}"` | 10s |
| `False` | `APIUnhealthy` | `"Keystone API returned HTTP {status}"` | 10s |
| `True` | `APIHealthy` | `"Keystone API is responding at {endpoint}"` | — |

**Requeue Constants (defined in `requeue_intervals.go`):**

| Constant | Value | Purpose |
| --- | --- | --- |
| `RequeueHealthCheck` | `10s` | Interval for requeuing on health check failure |
| `HealthCheckTimeout` | `10s` | Bounded timeout for the HTTP health check request |

**Response body handling:** The HTTP response body is always closed via `defer
resp.Body.Close()`, even on non-2xx responses. Go's `net/http` returns a non-nil
`Body` for all responses regardless of status code.

**Error handling:** Only the `http.NewRequestWithContext` error (malformed URL) is
returned as a reconcile error. All HTTP transport errors and non-2xx responses set a
condition and requeue — they are never returned as errors.

---

### reconcileHPA

**File:** `operators/keystone/internal/controller/reconcile_hpa.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcileHPA(ctx context.Context,
    keystone *keystonev1alpha1.Keystone) (ctrl.Result, error)
```

**Purpose:** Manage the HorizontalPodAutoscaler for the Keystone API Deployment
(CC-0038). Three lifecycle paths:

1. **Autoscaling disabled** (`spec.autoscaling` is nil): Delete any existing HPA
   and set `HPAReady=True` with reason `HPANotRequired`.
2. **Autoscaling enabled** (`spec.autoscaling` is set): Build the desired HPA via
   `buildKeystoneHPA()` and call `deployment.EnsureHPA()` to create or update it.
   Set `HPAReady=True` with reason `HPAReady`.
3. **Error**: Propagate errors from ensure/delete operations with descriptive context.

**HPA Construction (`buildKeystoneHPA`):**

| HPA Field | Value |
| --- | --- |
| Name | `{name}-api` |
| Labels | `commonLabels(keystone)` |
| `scaleTargetRef.apiVersion` | `apps/v1` |
| `scaleTargetRef.kind` | `Deployment` |
| `scaleTargetRef.name` | `{name}-api` |
| `minReplicas` | `spec.autoscaling.minReplicas` (falls back to `spec.replicas` when nil) |
| `maxReplicas` | `spec.autoscaling.maxReplicas` |

**Metrics:**

| Target Field Set | Metric Type | Resource | Target |
| --- | --- | --- | --- |
| `targetCPUUtilization` | `Resource` | `cpu` | `AverageUtilization` at specified percentage |
| `targetMemoryUtilization` | `Resource` | `memory` | `AverageUtilization` at specified percentage |

Both metrics can be set simultaneously. At least one is required (enforced by CEL
validation on the CRD).

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `True` | `HPANotRequired` | "Autoscaling is not configured" | — |
| `True` | `HPAReady` | "HorizontalPodAutoscaler is configured" | — |

**Error handling:** Errors from `deployment.EnsureHPA()` are wrapped with
"ensuring HorizontalPodAutoscaler" context. Errors from `deployment.DeleteHPA()`
are wrapped with "deleting HorizontalPodAutoscaler" context. Both are returned
directly to controller-runtime for exponential backoff.

**Shared library calls:** `deployment.EnsureHPA()`, `deployment.DeleteHPA()`

---

### reconcileBootstrap

**File:** `operators/keystone/internal/controller/reconcile_bootstrap.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcileBootstrap(ctx context.Context,
    keystone *keystonev1alpha1.Keystone) (ctrl.Result, error)
```

**Purpose:** Run the Keystone bootstrap Job that creates the initial admin user,
project, roles, and service catalog entries.

**Bootstrap Job:**

| Field | Value |
| --- | --- |
| Name | `keystone-bootstrap` |
| Image | `{spec.image.repository}:{spec.image.tag}` |
| Command | `keystone-manage bootstrap` |
| BackoffLimit | 4 |
| TTLSecondsAfterFinished | 300 |
| RestartPolicy | OnFailure |

**Bootstrap Arguments:**

| Argument | Value Source |
| --- | --- |
| `--bootstrap-password` | `$(BOOTSTRAP_PASSWORD)` env var from `spec.bootstrap.adminPasswordSecretRef` Secret, key `password` |
| `--bootstrap-admin-url` | `http://keystone-api.{namespace}.svc.cluster.local:5000/v3` |
| `--bootstrap-internal-url` | `http://keystone-api.{namespace}.svc.cluster.local:5000/v3` |
| `--bootstrap-public-url` | `http://keystone-api.{namespace}.svc.cluster.local:5000/v3` |
| `--bootstrap-region-id` | `spec.bootstrap.region` |

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `BootstrapFailed` | "Keystone bootstrap job failed: {error}" | — (error returned) |
| `False` | `BootstrapInProgress` | "Keystone bootstrap job is running" | 60s |
| `True` | `BootstrapComplete` | "Keystone bootstrap completed successfully" | — |

**Error handling:** The `BootstrapFailed` condition is set before returning the error,
so the failure reason is visible in the CR status even when the error triggers
controller-runtime backoff.

**Idempotency:** The bootstrap Job is idempotent — `keystone-manage bootstrap` can be
run multiple times without side effects.

**Shared library calls:** `job.RunJob()`

---

### reconcileTrustFlush

**File:** `operators/keystone/internal/controller/reconcile_trustflush.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcileTrustFlush(ctx context.Context,
    keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error)
```

**Purpose:** Manage the trust flush CronJob that periodically purges expired trust
delegations from the Keystone database (CC-0057). Three lifecycle paths:

1. **Trust flush disabled** (`spec.trustFlush` is nil): Delete any existing
   `{name}-trust-flush` CronJob and set `TrustFlushReady=True` with reason
   `TrustFlushNotRequired`.
2. **Trust flush enabled** (`spec.trustFlush` is set): Build the desired CronJob via
   `trustFlushCronJob()` and call `job.EnsureCronJob()` to create or update it.
   Set `TrustFlushReady=True` with reason `TrustFlushReady`.
3. **Error**: Propagate errors from ensure/delete operations with descriptive context.

> **Note:** This sub-reconciler accepts the `configMapName` returned by
> `reconcileConfig` to mount the correct immutable ConfigMap in the CronJob
> pod spec.

**CronJob Construction (`trustFlushCronJob`):**

| CronJob Field | Value |
| --- | --- |
| Name | `{name}-trust-flush` |
| Labels | `commonLabels(keystone)` |
| Schedule | `spec.trustFlush.schedule` |
| Suspend | `&spec.trustFlush.suspend` (pointer to CRD bool) |
| Container name | `trust-flush` |
| Image | `{spec.image.repository}:{spec.image.tag}` |
| Command | `["keystone-manage", "--config-dir=/etc/keystone/keystone.conf.d/", "trust_flush"]` + `spec.trustFlush.args` |
| SecurityContext | `restrictedSecurityContext()` (PSS Restricted) |
| RestartPolicy | `OnFailure` |

**Volume Mounts:**

| Volume Name | Mount Path | Source | ReadOnly |
| --- | --- | --- | --- |
| `config` | `/etc/keystone/keystone.conf.d/` | ConfigMap `{configMapName}` | Yes |
| `fernet-keys` | `/etc/keystone/fernet-keys` | Secret `{name}-fernet-keys` | Yes |
| `credential-keys` | `/etc/keystone/credential-keys` | Secret `{name}-credential-keys` | Yes |

**Deletion Helper (`deleteCronJob`):**

When `spec.trustFlush` is `nil`, `deleteCronJob()` issues a `client.Delete` for the
CronJob by name. It uses `client.IgnoreNotFound` so the operation is a no-op if the
CronJob does not exist (e.g., first reconciliation of a CR that never had trust flush
enabled).

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `True` | `TrustFlushNotRequired` | "Trust flush is not configured" | — |
| `True` | `TrustFlushReady` | "Trust flush CronJob is configured" | — |

**Error handling:** Errors from `job.EnsureCronJob()` are wrapped with
"ensuring trust flush CronJob" context. Errors from `deleteCronJob()` are wrapped
with "deleting trust flush CronJob" context. Both are returned directly to
controller-runtime for exponential backoff.

**Shared library calls:** `job.EnsureCronJob()`

---

## Error Handling Summary

| Sub-Reconciler | Transient State | RequeueAfter | Permanent Failure |
| --- | --- | --- | --- |
| `reconcileSecrets` | ESO not synced | 15s | API error → exponential backoff |
| `reconcileDatabase` | MariaDB CRs not ready | 30s | `ErrJobFailed` from db_sync |
| `reconcileDatabase` | db_sync running | 30s | API error → exponential backoff |
| `reconcileFernetKeys` | — | — | API error → exponential backoff |
| `reconcileConfig` | — | — | Secret read failure, render failure → exponential backoff |
| `reconcilePolicyValidation` | Job running | 15s | `ErrJobFailed` from validation → descriptive error |
| `reconcileDeployment` | Deployment not available | 10s | API error → exponential backoff |
| `reconcileHealthCheck` | Non-2xx, timeout, DNS, connection refused | 10s | Malformed URL → exponential backoff (CC-0067) |
| `reconcileHPA` | — | — | API error → exponential backoff |
| `reconcileBootstrap` | Job running | 60s | `ErrJobFailed` from bootstrap |
| `reconcileTrustFlush` | — | — | API error → exponential backoff |

All errors are wrapped with descriptive context via `fmt.Errorf("...: %w", err)`.
Unrecoverable API errors (e.g., permission denied, schema validation failure) are
returned directly to controller-runtime, which applies exponential backoff with jitter.

---

## Owned Resources

All resources created by the reconciler carry an owner reference pointing to the
Keystone CR via `controllerutil.SetControllerReference()`. This enables:

- **Automatic garbage collection** — Deleting the Keystone CR cascades to all owned
  resources.
- **Watch-based reconciliation** — Changes to owned resources trigger re-reconciliation
  of the owning Keystone CR.

| Resource | Name | Owner |
| --- | --- | --- |
| Secret | `keystone-fernet-keys` | Keystone CR |
| CronJob | `keystone-fernet-rotate` | Keystone CR |
| PushSecret | `keystone-fernet-keys-backup` | Keystone CR |
| ConfigMap | `keystone-config-{hash}` | Keystone CR |
| Job | `keystone-db-sync` | Keystone CR |
| Job | `keystone-bootstrap` | Keystone CR |
| Deployment | `keystone-api` | Keystone CR |
| Service | `keystone-api` | Keystone CR |
| PodDisruptionBudget | `{name}-api` | Keystone CR |
| HorizontalPodAutoscaler | `{name}-api` | Keystone CR (only when `spec.autoscaling` is set) |
| Job | `{name}-policy-validation` | Keystone CR (only when `spec.policyOverrides` is set) |
| CronJob | `{name}-trust-flush` | Keystone CR (only when `spec.trustFlush` is set) |
| Database | `keystone` | Keystone CR (managed mode only) |
| User | `keystone` | Keystone CR (managed mode only) |
| Grant | `keystone` | Keystone CR (managed mode only) |

---

## Testing

The reconciler has comprehensive unit tests using `gomega` with `NewGomegaWithT(t)`.
For end-to-end Chainsaw tests that validate the reconciler in a real cluster, see
[Keystone E2E Test Suites](./keystone-e2e-tests.md) (CC-0016).

### Running Tests

| Scope | Command |
| --- | --- |
| All controller tests | `go test ./operators/keystone/internal/controller/...` |
| Specific sub-reconciler | `go test -run TestReconcileSecrets ./operators/keystone/internal/controller/` |

### Test Files

| File | Coverage |
| --- | --- |
| `keystone_controller_test.go` | Reconcile() orchestration, sequential execution, early return, Ready aggregation, idempotency |
| `reconcile_secrets_test.go` | DB/admin credential readiness, error propagation, condition messages |
| `reconcile_database_test.go` | Managed/brownfield modes, MariaDB CRs, db_sync lifecycle, stale Job detection |
| `reconcile_fernet_test.go` | Key generation, Secret idempotency, CronJob schedule, PushSecret, key validity |
| `reconcile_config_test.go` | INI generation, extraConfig merge, plugin config, policy overrides, ConfigMap hashing |
| `reconcile_policyvalidation_test.go` | Policy validation lifecycle, condition contract, error extraction, Job spec (CC-0058) |
| `reconcile_deployment_test.go` | Deployment spec, Service creation, readiness, endpoint, owner references, fernet-keys hash annotation (CC-0015) |
| `reconcile_healthcheck_test.go` | Health check happy/unhealthy paths, timeout, DNS, connection refused, empty endpoint, response body close, HTTPDoer injection (CC-0067) |
| `reconcile_hpa_test.go` | HPA creation, update, deletion, metrics (CPU/memory), minReplicas defaulting, condition contract, error propagation (CC-0038) |
| `reconcile_trustflush_test.go` | CronJob creation, deletion, schedule/suspend/args, security context, volume mounts, condition contract, error propagation (CC-0057) |
| `reconcile_bootstrap_test.go` | Job creation, completion, failure, stale detection, TTL/backoff |
| `integration_test.go` | Full reconciliation envtest: CronJob spec, bootstrap Job spec, brownfield mode, condition progression (CC-0015) |

---

## File Layout

```text
operators/keystone/
├── main.go                                     Scheme registration + bootstrap wiring
├── api/v1alpha1/
│   ├── keystone_types.go                       CRD types
│   ├── keystone_webhook.go                     Webhooks
│   └── ...
└── internal/
    ├── controller/
    │   ├── keystone_controller.go              Reconciler struct, Reconcile(), SetupWithManager
    │   ├── reconcile_secrets.go                reconcileSecrets sub-reconciler
    │   ├── reconcile_database.go               reconcileDatabase sub-reconciler
    │   ├── reconcile_fernet.go                 reconcileFernetKeys sub-reconciler
    │   ├── reconcile_config.go                 reconcileConfig sub-reconciler
    │   ├── reconcile_policyvalidation.go        reconcilePolicyValidation sub-reconciler (CC-0058)
    │   ├── reconcile_deployment.go             reconcileDeployment sub-reconciler
    │   ├── reconcile_healthcheck.go           reconcileHealthCheck sub-reconciler (CC-0067)
    │   ├── reconcile_hpa.go                   reconcileHPA sub-reconciler (CC-0038)
    │   ├── reconcile_trustflush.go            reconcileTrustFlush sub-reconciler (CC-0057)
    │   ├── reconcile_bootstrap.go              reconcileBootstrap sub-reconciler
    │   ├── keystone_controller_test.go         Orchestration tests
    │   ├── reconcile_secrets_test.go           Secrets tests
    │   ├── reconcile_database_test.go          Database tests
    │   ├── reconcile_fernet_test.go            Fernet tests
    │   ├── reconcile_config_test.go            Config tests
    │   ├── reconcile_policyvalidation_test.go   Policy validation tests (CC-0058)
    │   ├── reconcile_deployment_test.go        Deployment tests
    │   ├── reconcile_healthcheck_test.go      Health check tests (CC-0067)
    │   ├── reconcile_hpa_test.go              HPA tests (CC-0038)
    │   ├── reconcile_trustflush_test.go       Trust flush tests (CC-0057)
    │   ├── reconcile_bootstrap_test.go         Bootstrap tests
    │   └── integration_test.go                 Envtest integration tests (CC-0015)
    └── testutil/
        └── envtest_setup.go                    Keystone-specific envtest helper
```
