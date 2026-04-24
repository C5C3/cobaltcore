---
title: Keystone Reconciler Architecture
quadrant: operator
feature: CC-0013, CC-0015, CC-0038, CC-0057, CC-0058, CC-0064, CC-0065, CC-0067, CC-0068, CC-0071, CC-0072, CC-0073, CC-0074, CC-0077, CC-0078, CC-0079, CC-0080, CC-0081, CC-0087
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
| `HTTPRoute` | `Owns()` (optional) | Registered only when the `gateway.networking.k8s.io/v1` CRD is installed; detected at startup via the manager's `RESTMapper`. Triggers reconciliation when owned HTTPRoute changes (only created when `spec.gateway` is set). |
| `Secret` | `Watches()` | Maps Secret events to referencing Keystone CRs via the `KeystoneSecretNameIndexKey` field indexer, with an owner-ref fallback for rotation staging Secrets (CC-0087) |
| `MariaDB` | `Watches()` | Propagates upstream DB cluster health into `DatabaseReady` (CC-0047) |
| `ClusterSecretStore` | `Watches()` | Propagates OpenBao-backend health into `SecretsReady` (CC-0047) |

Secrets use `Watches()` with a `MapFunc` instead of `Owns()` because some Secrets
(ESO-provided credentials in `spec.database.secretRef` and
`spec.bootstrap.adminPasswordSecretRef`) are owned by the ExternalSecret controller,
not by the Keystone CR, so an owner-reference filter would never match them. The
mapper therefore combines an indexed reverse lookup with an owner-ref fallback for
rotation staging Secrets — see [Secret Field Indexer](#secret-field-indexer-cc-0087)
below. The `MariaDB` and `ClusterSecretStore` watches exist so the operator reacts
immediately to upstream dependency outages without waiting for the next periodic
requeue (CC-0047).

#### Secret Field Indexer (CC-0087)

The Keystone controller registers a controller-runtime field indexer on the
`Keystone` kind so that a Secret event is resolved to the referencing Keystone
CR(s) via an O(1) cache lookup instead of an unfiltered namespace-scoped List.
Without the indexer, every Secret create/update/delete event in a namespace
containing ESO-managed Secrets would force the mapper to List every Keystone CR
in that namespace — producing API server load that scales linearly with the
number of Secret events, not with the number of Keystone CRs (CC-0087, REQ-001).

| Aspect | Value |
| --- | --- |
| Index key | `KeystoneSecretNameIndexKey = "spec.secretRefs.name"` (exported package-level constant in `operators/keystone/internal/controller/keystone_controller.go`) |
| Indexed fields | `spec.database.secretRef.name` **and** `spec.bootstrap.adminPasswordSecretRef.name` — the deduplicated union of both is emitted by the extractor; empty strings are skipped so unset optional fields do not pollute the index. |
| Registration site | `SetupWithManager` → `registerSecretNameIndex(ctx, mgr.GetFieldIndexer())`, invoked **before** the `Watches(Secret, …)` chain. Any error from `IndexField` is wrapped with the index key and propagated, so manager startup aborts loudly if registration fails (REQ-006). |
| Lookup site | `secretToKeystoneMapper(mgr.GetClient())` — performs a namespace-scoped `client.List` with `client.MatchingFields{KeystoneSecretNameIndexKey: secret.Name}`. On List error, the error is logged and swallowed (the `handler.MapFunc` contract forbids returning errors) so the owner-ref fallback still runs. |
| Owner-ref fallback | For each `ownerReference` on the Secret where `Kind == "Keystone"` and the parsed group of `APIVersion` equals `keystonev1alpha1.GroupVersion.Group` (`keystone.openstack.c5c3.io`, any version), the mapper enqueues `{Namespace: secret.Namespace, Name: ownerRef.Name}`. A cached `Get` against the informer cache drops owner-refs whose target Keystone no longer exists (stale or spurious refs); any non-`NotFound` error falls through and enqueues anyway so a transient cache blip cannot swallow a legitimate event. Group-only matching means existing Secrets continue to resolve after a future API version bump (CC-0087 review #1). This preserves the enqueue path for rotation staging Secrets (`{name}-fernet-keys-rotation`, `{name}-credential-keys-rotation`; see [Key Rotation RBAC Split](#key-rotation-rbac-split-cc-0081)) which are owned by the Keystone CR but not referenced by name from the spec (CC-0081). |
| Deduplication | The indexed-lookup and owner-ref paths are unioned by `types.NamespacedName` before returning, so a Secret that is both name-referenced and owner-referenced to the same Keystone yields exactly one `reconcile.Request`. |

**Adding new Secret references.** When a future change introduces another
`SecretRef` field on `KeystoneSpec`, extend `keystoneSecretNameExtractor` to
emit that field's name alongside the existing two, and add a corresponding
unit-test case. The index key itself (`spec.secretRefs.name`) is intentionally
named as a union key so new indexed fields do not require a new indexer.

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
| `external-secrets.io` | `externalsecrets`, `pushsecrets` | get, list, watch, create, update, patch, delete (CC-0079) |
| `external-secrets.io` | `clustersecretstores` | get, list, watch |
| `policy` | `poddisruptionbudgets` | get, list, watch, create, update, patch, delete |
| `autoscaling` | `horizontalpodautoscalers` | get, list, watch, create, update, patch, delete |
| `gateway.networking.k8s.io` | `httproutes` | get, list, watch, create, update, patch, delete (CC-0065) |
| `gateway.networking.k8s.io` | `httproutes/status` | get (CC-0065) |

---

## Labels and Annotations

The reconciler applies `commonLabels(keystone)` (`app.kubernetes.io/name`,
`app.kubernetes.io/instance`, `app.kubernetes.io/managed-by`) to every owned
resource. In addition, the following forge-specific metadata keys carry
controller-observable semantics and are stable across releases — consumers
(watch predicates, chainsaw tests, dashboards) may rely on them:

| Key | Kind | Applied to | Value | Feature | Purpose |
| --- | --- | --- | --- | --- | --- |
| `forge.c5c3.io/rotation-target` | Label | Staging Secrets (`{name}-fernet-keys-rotation`, `{name}-credential-keys-rotation`) | `fernet-keys`, `credential-keys` | CC-0081 | Distinguishes rotation staging Secrets from production key Secrets so the operator's Secret→Keystone mapper can enqueue the owning Keystone on staging PATCHes. |
| `forge.c5c3.io/rotation-completed-at` | Annotation | Staging Secrets (written by the rotation CronJob) | RFC3339 UTC timestamp (e.g. `2026-04-18T12:34:56Z`) | CC-0081 | Single-shot commit marker. The operator only applies a staging Secret's data to the production Secret when this annotation is present and parses cleanly; the annotation is removed implicitly when the staging Secret is deleted at the end of a successful apply. |

The Go constants backing these keys are exported from
`operators/keystone/internal/controller/rotation_staging.go`:

```go
const StagingSecretLabelKey       = "forge.c5c3.io/rotation-target"        // CC-0081
const RotationCompletedAnnotation = "forge.c5c3.io/rotation-completed-at"  // CC-0081
```

See [Key Rotation RBAC Split](#key-rotation-rbac-split-cc-0081) under the
Fernet and credential sub-reconciler sections for the full contract.

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
│         ▼                                    ┌─────────────────────────────┐ │
│  ┌──────────────────┐                        │         LEGEND              │ │
│  │ reconcileSecrets │  Check ESO synced      │  ───── Sequential           │ │
│  │                  │  Sets: SecretsReady     │  ═════ Parallel (CC-0071)   │ │
│  └────────┬─────────┘  Requeue: 15s          └─────────────────────────────┘ │
│           │                                                                  │
│           ▼                                                                  │
│  ┌──────────────────┐                                                        │
│  │ reconcileConfig  │  Render keystone.conf + api-paste.ini                  │
│  │                  │  Create immutable ConfigMap                             │
│  └────────┬─────────┘  Returns: configMapName                                │
│           │                                                                  │
│           ▼                                                                  │
│  ╔══════════════════════════════════════════════════════════════════════════╗ │
│  ║  reconcileParallelGroup (CC-0071)                                      ║ │
│  ║                                                                        ║ │
│  ║  errgroup.WithContext — each goroutine receives a DeepCopy of the CR   ║ │
│  ║                                                                        ║ │
│  ║  ┌─────────────────────┐  ┌──────────────────────────┐                 ║ │
│  ║  │ reconcileFernetKeys │  │ reconcileCredentialKeys   │  (concurrent)  ║ │
│  ║  │ + script ConfigMap  │  │ + script ConfigMap        │  (CC-0073)     ║ │
│  ║  │ Sets: FernetKeysReady│ │ Sets: CredentialKeysReady │                ║ │
│  ║  └─────────────────────┘  └──────────────────────────┘                 ║ │
│  ║  ┌────────────────────────┐                                            ║ │
│  ║  │ reconcileNetworkPolicy │  (concurrent)                              ║ │
│  ║  │ Sets: NetworkPolicyReady│                                            ║ │
│  ║  └────────────────────────┘                                            ║ │
│  ║                                                                        ║ │
│  ║  g.Wait() → mergeParallelConditions → shortestRequeue                  ║ │
│  ╚═══════════════════════════════╤════════════════════════════════════════╝ │
│                                  │                                           │
│           ┌──────────────────────┘                                           │
│           ▼                                                                  │
│  ┌───────────────────┐                                                       │
│  │ reconcileDatabase │  Managed mode: verify MariaDB cluster health first,   │
│  │                   │  then ensure Database/User/Grant CRs + run db_sync    │
│  │                   │  Job + run schema-check Job (CC-0064)                 │
│  │                   │  Sets: DatabaseReady                                  │
│  └────────┬──────────┘  Requeue: 30s                                         │
│           │                                                                  │
│           ▼                                                                  │
│  ┌─────────────────────────────────┐                                         │
│  │ reconcilePolicyValidation       │  Validate oslo.policy overrides         │
│  │                                 │  via oslopolicy-validator Job           │
│  │                                 │  Sets: PolicyValidReady                 │
│  └────────┬────────────────────────┘  Requeue: 15s                           │
│           │                                                                  │
│           ▼                                                                  │
│  ┌──────────────────────┐                                                    │
│  │ reconcileDeployment  │  Ensure Deployment + Service                       │
│  │                      │  Sets: DeploymentReady, status.endpoint            │
│  └────────┬─────────────┘  Requeue: 10s                                      │
│           │                                                                  │
│           ▼                                                                  │
│  ┌─────────────────────────┐                                                 │
│  │ pruneStaleConfigMaps    │  Delete old {name}-config-{hash} ConfigMaps     │
│  │                         │  Retain 3 historical + current (CC-0077)        │
│  └────────┬────────────────┘  No condition, no requeue                       │
│           │                                                                  │
│           ▼                                                                  │
│  ┌─────────────────────────┐                                                 │
│  │ reconcileHTTPRoute      │  Create/update/delete HTTPRoute based on        │
│  │                         │  spec.gateway; reflect parent Accepted status   │
│  │                         │  Sets: HTTPRouteReady                           │
│  └────────┬────────────────┘  Requeue: 10s while not Accepted (CC-0065)      │
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

### Execution Model

Sub-reconcilers execute in a defined order using two execution modes (CC-0071):

1. **Sequential sub-reconcilers** run one at a time. Each is called only if all
   previous sub-reconcilers succeeded without requesting a requeue.
2. **Parallel group** (`reconcileParallelGroup`) runs three independent sub-reconcilers
   concurrently via `errgroup.WithContext`. Each goroutine operates on a `DeepCopy` of
   the Keystone CR to prevent data races. See
   [Parallel Group Architecture](#parallel-group-architecture) for details.

The sequential call pattern for each sub-reconciler (except `reconcileConfig`) is:

```go
if result, err := r.reconcileX(ctx, &keystone); !result.IsZero() || err != nil {
    return r.updateStatus(ctx, &keystone, result, err)
}
```

This guarantees:

1. A sub-reconciler error **propagates immediately** — subsequent sub-reconcilers are
   skipped.
2. A non-zero result (`RequeueAfter > 0` or `Requeue: true`) causes an **early
   return** — status is persisted and the reconciler exits.
3. Status conditions from the failing/requeuing sub-reconciler are **always persisted**
   via `updateStatus()` before returning.

The parallel group follows a different contract: all three sub-reconcilers run
simultaneously, errors cancel the errgroup context, and conditions from completed
sub-reconcilers are merged even on partial failure.

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

### Parallel Group Architecture

Three sub-reconcilers — `reconcileFernetKeys`, `reconcileCredentialKeys`, and
`reconcileNetworkPolicy` — run concurrently via `reconcileParallelGroup` after
`reconcileConfig` completes and before `reconcileDatabase` begins (CC-0071). These
sub-reconcilers are eligible for parallelization because they have no data
dependencies on each other (see [Dependency Graph](#dependency-graph) below).

**File:** `operators/keystone/internal/controller/keystone_controller.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcileParallelGroup(
    ctx context.Context,
    keystone *keystonev1alpha1.Keystone,
    subs []parallelSubReconciler,
) (ctrl.Result, error)
```

Each parallel sub-reconciler is described by a `parallelSubReconciler` struct:

```go
type parallelSubReconciler struct {
    conditionType string
    fn            func(ctx context.Context, keystone *keystonev1alpha1.Keystone) (ctrl.Result, error)
}
```

#### Dependency Graph

The dependency graph determines which sub-reconcilers can run in parallel. A
sub-reconciler is eligible for parallelization when it has no data dependency on any
other parallelizable sub-reconciler and no downstream sub-reconciler depends on its
output (other than conditions merged after the group completes).

| Sub-Reconciler | Inputs | Condition Type | Dependencies | Parallel |
| --- | --- | --- | --- | --- |
| `reconcileSecrets` | CR spec | `SecretsReady` | none | no (must run first) |
| `reconcileConfig` | CR spec, DB secret | *(returns configMapName)* | Secrets | no (produces configMapName) |
| `reconcileFernetKeys` | configMapName | `FernetKeysReady` | Config | **yes** |
| `reconcileCredentialKeys` | configMapName | `CredentialKeysReady` | Config | **yes** |
| `reconcileNetworkPolicy` | CR spec | `NetworkPolicyReady` | none | **yes** |
| `reconcileDatabase` | configMapName | `DatabaseReady` | Config | no (complex state machine) |
| `reconcilePolicyValidation` | configMapName | `PolicyValidReady` | Config | no (gates Deployment) |
| `reconcileDeployment` | configMapName | `DeploymentReady` | Database (implicit) | no |
| `pruneStaleConfigMaps` | configMapName | *(none)* | Deployment (must be ready) | no |
| `reconcileHTTPRoute` | CR spec | `HTTPRouteReady` | Deployment (ensures backend Service exists) | no |
| `reconcileHealthCheck` | status.endpoint | `KeystoneAPIReady` | Deployment (sets endpoint) | no |
| `reconcileHPA` | CR spec | `HPAReady` | Deployment (naming) | no |
| `reconcileBootstrap` | configMapName | `BootstrapReady` | Deployment (API must be running) | no |
| `reconcileTrustFlush` | configMapName | `TrustFlushReady` | Config | no |

Key constraints that prevent further parallelization:

- **reconcileDatabase** has a multi-step state machine (MariaDB CRs → db_sync Job →
  schema-check Job) with 30s requeue waits, and `reconcileDeployment` depends on the
  database being ready.
- **reconcilePolicyValidation** must gate `reconcileDeployment` — invalid policy
  overrides must be caught before reaching running pods.
- **reconcileBootstrap** requires the API to be running (depends on Deployment).

#### DeepCopy Condition Merge Pattern

Each parallel sub-reconciler receives its own `DeepCopy` of the Keystone CR. This
eliminates shared mutable state by construction — `conditions.SetCondition` writes to
the copy's `Status.Conditions` slice, not the original. No `sync.Mutex` is needed.

```text
Keystone CR (original, with SecretsReady set)
    │
    ├─ DeepCopy → ksCopyFernet ──[goroutine 1]──→ FernetKeysReady on copy
    ├─ DeepCopy → ksCopyCred   ──[goroutine 2]──→ CredentialKeysReady on copy
    └─ DeepCopy → ksCopyNetpol ──[goroutine 3]──→ NetworkPolicyReady on copy
                                                     │
                                          errgroup.Wait()
                                                     │
                                    mergeParallelConditions (sequential):
                                      copy1 → FernetKeysReady     → original
                                      copy2 → CredentialKeysReady → original
                                      copy3 → NetworkPolicyReady  → original
```

After `g.Wait()` returns, conditions from each copy are merged sequentially into the
original via `mergeParallelConditions(dst, src, conditionType)`:

```go
func mergeParallelConditions(dst, src *keystonev1alpha1.Keystone, conditionType string) {
    cond := conditions.GetCondition(src.Status.Conditions, conditionType)
    if cond == nil {
        return
    }
    conditions.SetCondition(&dst.Status.Conditions, *cond)
}
```

Merge behavior:

- Pre-existing conditions on the destination are **preserved**.
- If the source copy does not contain a condition of the expected type (e.g., the
  goroutine was cancelled before setting it), the destination is **left unchanged**.
- Conditions from sub-reconcilers that completed before an error are **still merged**,
  so partial progress is visible in the CR status.

#### errgroup Usage

The parallel group uses `errgroup.WithContext(ctx)` for two properties:

1. **Error propagation** — The first error from any goroutine is returned by
   `g.Wait()`.
2. **Context cancellation** — When one goroutine returns an error, the derived context
   (`gctx`) is cancelled, signalling remaining goroutines to exit promptly.

```go
g, gctx := errgroup.WithContext(ctx)
// Each goroutine receives gctx, not the parent ctx
```

The error returned by `g.Wait()` is returned as the `Reconcile` error, triggering
controller-runtime's exponential backoff. Conditions from all completed goroutines —
including those that succeeded before the error — are merged before returning.

#### Requeue Resolution

When all parallel sub-reconcilers succeed, `shortestRequeue` selects the `ctrl.Result`
with the shortest non-zero `RequeueAfter` from the group:

```go
func shortestRequeue(results ...ctrl.Result) ctrl.Result
```

| All results zero | Shortest non-zero | Returned result |
| --- | --- | --- |
| Yes | — | `ctrl.Result{}` (no requeue) |
| No | e.g. 15s | `ctrl.Result{RequeueAfter: 15s}` |

This ensures the reconcile loop runs at the pace of the most urgent sub-reconciler in
the group.

---

## Finalizer

The KeystoneReconciler installs a finalizer on every Keystone CR so that the
MariaDB `Database`, `User`, and `Grant` CRs owned by the Keystone are
deterministically torn down **before** the Keystone CR itself is removed from
etcd (CC-0078). Without a finalizer, the Keystone CR would be deleted
immediately on `kubectl delete keystone <name>` and the controller would have
no opportunity to clean up the MariaDB resources it orchestrated — leaving them
orphaned when redeploying or tearing down the service.

### Finalizer Constant

The finalizer name is declared once as a package-level constant in
`operators/keystone/internal/controller/keystone_controller.go`:

```go
const keystoneFinalizer = "keystone.openstack.c5c3.io/finalizer"
```

The value uses the canonical CRD group prefix (`keystone.openstack.c5c3.io`) so
that it is unambiguous under `kubectl get keystone -o yaml` and cannot collide
with finalizers owned by other controllers. The constant is the single source
of truth used by `Reconcile`, `reconcileDelete`, `finalizeDatabaseResources`,
`hasLiveMariaDBResources`, the unit and integration tests, and this
documentation.

### Resources Cleaned Up

When the Keystone CR is deleted, the finalizer cleanup deletes these MariaDB
CRs (all matched by the Keystone CR's `metadata.name` in the same
`metadata.namespace`):

| Resource | API Group | Name | Namespace |
| --- | --- | --- | --- |
| `Database` | `k8s.mariadb.com` | `{keystone.Name}` | `{keystone.Namespace}` |
| `User` | `k8s.mariadb.com` | `{keystone.Name}` | `{keystone.Namespace}` |
| `Grant` | `k8s.mariadb.com` | `{keystone.Name}` | `{keystone.Namespace}` |

These three CRs are the only resources the finalizer manages. Every other
resource owned by the Keystone CR (Deployment, Service, ConfigMap, Secret,
Job, CronJob, PDB, HPA, NetworkPolicy) is reclaimed by the built-in Kubernetes
garbage collector via owner references — the finalizer does not touch them.
See [Owned Resources](#owned-resources) for the full owner-reference list.

### Reconcile Branching on DeletionTimestamp

`Reconcile` inspects `metadata.deletionTimestamp` immediately after the CR
`Get` and before any sub-reconciler executes:

```text
Fetch Keystone CR
    │
    ├─ DeletionTimestamp != zero ──► reconcileDelete ──► (no sub-reconcilers)
    │
    └─ DeletionTimestamp == zero ──► AddFinalizer if missing ──► sub-reconcilers
```

Running sub-reconcilers against a Terminating CR is not safe: sub-reconcilers
such as `reconcileDatabase` would re-create the very MariaDB CRs the finalizer
is deleting, producing an infinite reconcile loop. The early branch avoids
this entirely — Terminating CRs only ever flow through `reconcileDelete`.

On the live-CR path, `controllerutil.AddFinalizer` is called if the finalizer
is missing and the CR is `Update`d, followed by an early `Requeue: true`
return. This ensures the next reconcile pass observes the finalizer already
persisted in etcd rather than a transient in-memory copy, and it makes the
finalizer installation a single-pass, conflict-safe operation under
controller-runtime's retry semantics.

### reconcileDelete

```go
func (r *KeystoneReconciler) reconcileDelete(
    ctx context.Context,
    keystone *keystonev1alpha1.Keystone,
) (ctrl.Result, error)
```

The deletion handler proceeds in four steps, all within a single reconcile
pass:

1. **No-op guard.** If the Keystone CR never carried the finalizer (e.g. a CR
   created by an earlier operator version that did not install finalizers, or
   one whose finalizer was already released on a prior pass), `reconcileDelete`
   returns `(ctrl.Result{}, nil)` immediately. No Delete calls are issued, no
   Events are emitted.
2. **Cleanup-work announcement.** `hasLiveMariaDBResources` probes whether any
   of the three MariaDB CRs is still live (i.e. exists *and* has
   `DeletionTimestamp == 0`). If so, a single Normal Event with reason
   `FinalizingDatabase` is emitted. Brownfield CRs skip the event because they
   never created the MariaDB CRs and there is no cleanup work to announce.
3. **Cleanup.** `finalizeDatabaseResources` issues `Delete` for each of
   `Database`, `User`, `Grant` and returns as soon as every Delete is accepted
   (or tolerated as NotFound). It does **not** block on the MariaDB operator
   completing its own teardown — see
   [Why the finalizer does not wait](#why-the-finalizer-does-not-wait).
4. **Finalizer release.** A Normal Event with reason `DatabaseFinalized` is
   emitted, the finalizer is removed via `controllerutil.RemoveFinalizer`, and
   the CR is `Update`d. The API server then observes an empty finalizers list
   and garbage-collects the Keystone CR from etcd.

### Why the finalizer does not wait

An earlier implementation re-`Get`d each MariaDB CR after `Delete` and only
released the Keystone finalizer when all three were confirmed `NotFound`. Under
concurrent Keystone deletions (chainsaw `parallel: 4`) this created a deadlock:

1. The Keystone finalizer kept the Keystone CR in etcd.
2. Kubernetes garbage collection therefore did **not** cascade-delete the owned
   `Deployment`, so the `keystone-api` Pod kept its MariaDB connections open.
3. The MariaDB operator could not run `DROP DATABASE` while connections were
   live, so the `Database` CR stayed in Terminating state.
4. Goto 1 — the finalizer never released, the 2 min `delete:` timeout in the
   `deletion-cleanup` chainsaw test expired, and cascading test cleanups
   stacked up behind the same block.

Releasing the Keystone finalizer as soon as the `Delete` requests are issued
breaks the cycle. GC cascade-deletes the `Deployment`, Pods terminate,
connections close, and the MariaDB operator completes the drop asynchronously.
The owner references set by `reconcileDatabase`
(`controllerutil.SetControllerReference` in `EnsureDatabase` /
`EnsureDatabaseUser`) guarantee that even if the explicit Delete is a no-op
(e.g., the MariaDB operator has already started its own teardown), the CRs are
still reclaimed.

### NotFound Tolerance and Idempotency

`Delete` on an absent MariaDB CR returns `NotFound`, which
`finalizeDatabaseResources` logs at `V(1)` and treats as success — the CR was
already garbage-collected, externally deleted, or never existed.

This tolerance makes `finalizeDatabaseResources` **idempotent**: calling it
twice in a row with no MariaDB CRs present returns `nil` both times, and no
additional side effects are produced on the second call. Idempotency is
essential because:

- Controller-runtime retries `Reconcile` with exponential backoff on transient
  errors, so any finalizer pass may be replayed.
- The MariaDB operator may have already collected the CRs via its own
  owner-reference chain before the finalizer runs.
- An external actor (SRE, GitOps reconciliation) may have manually deleted the
  CRs before the Keystone deletion.

In all three cases the finalizer converges in a single reconcile pass without
surfacing spurious errors or Events.

### Brownfield No-Op Behaviour

Brownfield deployments (`spec.database.host` set, `spec.database.clusterRef`
nil) never create MariaDB CRs — the operator connects to a pre-existing
external MariaDB cluster. The finalizer path is intentionally **branch-free**:
brownfield CRs still receive the finalizer on first reconcile and still flow
through `reconcileDelete` on deletion, but every `Delete` call is a no-op
NotFound. Consequences:

- The finalizer is removed in the same reconcile pass that observes deletion —
  the same pass-count as managed mode after the wait was removed.
- No `FinalizingDatabase` event is emitted (there is no real cleanup work to
  announce — `hasLiveMariaDBResources` returns `false` because the probe
  observes zero live MariaDB CRs).
- A `DatabaseFinalized` event **is** emitted — it is the common signal that
  the finalizer has released the CR.
- No spurious NotFound errors reach the user.

This keeps brownfield and managed-mode deletion paths symmetric in code while
preserving the correct observable behaviour.

### Upgrade Path

Keystone CRs created by an earlier operator version that did not install
finalizers gain the finalizer on their next reconcile under the new version:

1. The live-CR branch sees `ContainsFinalizer == false`, calls
   `AddFinalizer` + `Update`, and requeues.
2. The next reconcile observes the persisted finalizer and proceeds through
   the normal sub-reconciler pipeline — `Status` and existing conditions are
   unchanged by the finalizer addition itself.
3. On subsequent deletion, the full cleanup flow runs as described above.

No manual migration of existing CRs is required.

### Events

Two Normal Events are emitted on the Keystone CR during finalizer-driven
cleanup, captured via `record.EventRecorder`:

| Reason | Type | Message | Emitted When |
| --- | --- | --- | --- |
| `FinalizingDatabase` | `Normal` | `"Cleaning up MariaDB Database, User, and Grant before removing Keystone"` | First terminating reconcile pass where at least one MariaDB CR is still live (not emitted for brownfield CRs or when all CRs are already gone) |
| `DatabaseFinalized` | `Normal` | `"MariaDB Database, User, and Grant removed; releasing finalizer"` | Once per termination, immediately before `RemoveFinalizer` + `Update` |

> **Note (CC-0078):** The two Events are intentionally asymmetric. A
> brownfield-terminating CR (or a managed CR whose MariaDB resources were
> already removed externally before deletion) emits only `DatabaseFinalized`,
> **not** `FinalizingDatabase`, because there is no real cleanup work to
> announce. `DatabaseFinalized` is therefore the common, authoritative signal
> that the finalizer has released the CR; `FinalizingDatabase` is a
> supplementary signal that real cleanup was observed at least once. See
> [Brownfield No-Op Behaviour](#brownfield-no-op-behaviour) for the full
> state-machine reasoning.

No `Warning` Event is emitted on cleanup errors — controller-runtime retries
the reconcile with exponential backoff and the underlying API error is logged
via `log.FromContext(ctx)`. Error-level Events would only add noise to a retry
loop that already has a structured-logging record.

All finalizer-related log lines include the Keystone `name` and `namespace`
via `log.FromContext(ctx).WithValues("keystone", ...)`, keeping log correlation
consistent with the rest of the reconciler.

### Owner References vs Finalizer

Every MariaDB CR created by `reconcileDatabase` already carries an
`ownerReference` to the Keystone CR (via
`controllerutil.SetControllerReference` in `internal/common/database/database.go`).
Kubernetes' built-in garbage collector would normally suffice to cascade the
deletion. The finalizer is deliberately additive, not a replacement:

| Mechanism | What it guarantees |
| --- | --- |
| Owner references | MariaDB CRs are eventually garbage-collected after Keystone CR removal |
| Finalizer | Keystone CR is removed **only after** all three MariaDB CRs are confirmed NotFound; cleanup is observable via Events and logs |

The two mechanisms do not conflict: `Delete` is idempotent under `NotFound`,
so if GC removes a CR before the finalizer's `Delete` call, the finalizer
simply observes `NotFound` and continues. The finalizer adds deterministic
ordering and observability on top of GC's eventual-consistency guarantee.

---

## OpenBao Finalizer

In addition to the MariaDB finalizer, every Keystone CR carries a dedicated
finalizer that drives cleanup of the backup PushSecrets ESO uses to persist
the Fernet and credential signing keys to OpenBao (CC-0079). Without this
finalizer, deleting a Keystone CR would garbage-collect the PushSecret CRs
via owner references **without** triggering the remote delete on the KV-v2
path — leaving stale cryptographic material in OpenBao after the Keystone
CR is gone. The two finalizers are independent: each is installed, tracked,
and released by its own handler, and they can complete in either order.

### Finalizer Constant

The finalizer name is declared once as a package-level constant in
`operators/keystone/internal/controller/reconcile_secrets.go`:

```go
const keystoneOpenBaoFinalizer = "keystone.openstack.c5c3.io/openbao-finalizer"
```

The `-finalizer` suffix differentiates it from the MariaDB finalizer
(`keystone.openstack.c5c3.io/finalizer`) so that `kubectl get keystone -o yaml`
shows both entries unambiguously under `metadata.finalizers`. The constant
is the single source of truth used by `Reconcile`, `reconcileDeleteOpenBao`,
`finalizeOpenBaoSecrets`, `hasLiveOpenBaoBackupPushSecrets`, all unit and
integration tests, and this documentation.

### Resources Cleaned Up

When the Keystone CR is deleted, the finalizer cleanup deletes these backup
PushSecret CRs (both in the Keystone's namespace):

| PushSecret | API Group | KV-v2 Path (OpenBao) |
| --- | --- | --- |
| `{keystone.Name}-fernet-keys-backup` | `external-secrets.io` | `kv-v2/data/openstack/keystone/{keystone.Name}/fernet-keys` |
| `{keystone.Name}-credential-keys-backup` | `external-secrets.io` | `kv-v2/data/openstack/keystone/{keystone.Name}/credential-keys` |

The names are produced by `openBaoBackupPushSecretNames(keystone)` so that
adding a third backup target in the future is a one-line change. Both
PushSecrets share a single builder convention in `reconcile_fernet.go` and
`reconcile_credential.go` respectively; neither carries secret material of
its own — the live `Secret` referenced by `Spec.Selector.Secret.Name` is the
source of truth, and the PushSecret is the control-plane object that tells
ESO what to push (and, on deletion, what to purge).

The finalizer does **not** touch any other Keystone-owned resource
(Deployment, ConfigMap, Service, CronJob, etc.). Those are reclaimed by the
built-in Kubernetes garbage collector via owner references — see
[Owned Resources](#owned-resources).

> **Path scoping (CC-0093):** Both KV-v2 paths are per-CR-scoped via
> `openstack/keystone/{keystone.Name}/<leaf>` (where `<leaf>` is `fernet-keys`
> or `credential-keys`), so multiple Keystone CRs in the same namespace write
> to disjoint paths and cannot collide. See
> [Migration note: legacy flat paths (CC-0093)](#migration-note-legacy-flat-paths-cc-0093)
> for upgrade behaviour and recommended cleanup of orphaned pre-CC-0093 paths.

### Migration note: legacy flat paths (CC-0093)

Before CC-0093, both backup PushSecrets wrote to the cluster-global, flat
KV-v2 paths `kv-v2/openstack/keystone/fernet-keys` and
`kv-v2/openstack/keystone/credential-keys`. Starting with CC-0093 the
operator writes to the per-CR-scoped paths
`kv-v2/openstack/keystone/{keystone.Name}/fernet-keys` and
`kv-v2/openstack/keystone/{keystone.Name}/credential-keys`.

The RemoteKey change lands the moment the Keystone operator is upgraded —
the next reconcile of each Keystone CR emits the new path. For existing
clusters the corresponding OpenBao ACL
(`deploy/openbao/policies/push-keystone-keys.hcl`) must also be re-applied
so ESO is authorised to write to the new paths; otherwise ESO will return
`403` on the backup step and `FernetKeysReady` / `CredentialKeysReady` will
flip to `False`. For kind/dev clusters this happens automatically when
`hack/deploy-infra.sh` (or `deploy/openbao/bootstrap/setup-policies.sh`)
is re-run; for production clusters managed outside the bootstrap flow the
equivalent is a single `bao policy write push-keystone-keys …` against the
updated HCL file.

The pre-CC-0093 flat paths are **orphaned but harmless** after upgrade:
the live Keystone control plane reads its Fernet and credential keys from
the local Kubernetes `Secret` (`{name}-fernet-keys`, `{name}-credential-keys`),
not from the OpenBao backup. The OpenBao copy is a disaster-recovery
artefact only; the legacy entries simply stop being refreshed, never get
deleted by `DeletionPolicy=Delete` (no live PushSecret references them
anymore), and are otherwise inert.

Operators who want a clean OpenBao state can purge the legacy entries
manually after upgrade:

```sh
bao kv metadata delete kv-v2/openstack/keystone/fernet-keys
bao kv metadata delete kv-v2/openstack/keystone/credential-keys
```

`metadata delete` removes both the current version and all historical
versions of the secret at that path; this is the canonical KV-v2 purge
operation and the right inverse of the now-superseded write.

### DeletionPolicy=Delete Wiring Through ESO

The Keystone operator has no OpenBao credentials and does not talk to the
OpenBao API directly. Remote purge of the KV-v2 path is delegated to ESO
via the PushSecret field `Spec.DeletionPolicy`. The `RemoteKey` follows the
per-CR layout `openstack/keystone/{keystone.Name}/<leaf>` introduced by
CC-0093, so each Keystone CR writes to its own KV-v2 prefix:

```go
Spec: esov1alpha1.PushSecretSpec{
    DeletionPolicy: esov1alpha1.PushSecretDeletionPolicyDelete,
    SecretStoreRefs: []esov1alpha1.PushSecretStoreRef{{
        Kind: "ClusterSecretStore",
        Name: "openbao-cluster-store",
    }},
    // ...
    Data: []esov1alpha1.PushSecretData{{
        Match: esov1alpha1.PushSecretMatch{
            RemoteRef: esov1alpha1.PushSecretRemoteRef{
                RemoteKey: fmt.Sprintf("openstack/keystone/%s/fernet-keys", keystone.Name),
            },
        },
    }},
}
```

`DeletionPolicy=Delete` instructs ESO to issue an OpenBao `DELETE` against
the configured `RemoteKey` as part of the PushSecret's own teardown. ESO
installs a cleanup finalizer on the PushSecret, holds the object in
Terminating state until the remote delete succeeds, then releases its
finalizer so the API server garbage-collects the PushSecret.

This means the Keystone finalizer does not need OpenBao credentials — the
single component that talks to OpenBao is still ESO, and the abstraction
boundary (ClusterSecretStore, policy, auth) stays in one place. The
Keystone finalizer's responsibility is purely ordering: ensure the Delete
happens **before** the Keystone CR leaves etcd.

### Reconcile Branching on DeletionTimestamp

`Reconcile` dispatches terminating CRs to both finalizer handlers
immediately after the CR `Get` and before any sub-reconciler executes:

```text
Fetch Keystone CR
    │
    ├─ DeletionTimestamp != zero ──► reconcileDelete (MariaDB finalizer)
    │                                reconcileDeleteOpenBao (OpenBao finalizer)
    │                                (no sub-reconcilers)
    │
    └─ DeletionTimestamp == zero ──► AddFinalizer(keystoneFinalizer) if missing
                                     AddFinalizer(keystoneOpenBaoFinalizer) if missing
                                     sub-reconcilers
```

Running sub-reconcilers against a Terminating CR would be unsafe:
`reconcileFernetKeys` and `reconcileCredentialKeys` build the live `Secret`
and the backup `PushSecret` on every pass, and would therefore re-create
the PushSecrets the finalizer just deleted — classic infinite reconcile
loop. The early branch avoids this entirely.

On the live-CR path, `controllerutil.AddFinalizer(keystone, keystoneOpenBaoFinalizer)`
is called if the finalizer is missing, the CR is `Update`d, and an early
`Requeue: true` return ensures the next reconcile pass observes the
finalizer already persisted in etcd. The MariaDB and OpenBao finalizer
additions are independent `Update`+`Requeue` steps; both converge within
two requeues on a fresh CR and within a single pass on subsequent
reconciles.

### reconcileDeleteOpenBao

```go
func (r *KeystoneReconciler) reconcileDeleteOpenBao(
    ctx context.Context,
    keystone *keystonev1alpha1.Keystone,
) (ctrl.Result, error)
```

The deletion handler proceeds as follows:

1. **No-op guard.** If the Keystone CR does not carry
   `keystoneOpenBaoFinalizer` (e.g. a CR that pre-dates CC-0079, or whose
   finalizer was already released on a prior pass), `reconcileDeleteOpenBao`
   returns `(ctrl.Result{}, nil)` immediately — no Delete calls, no Events.
2. **Cleanup-work announcement.** `hasLiveOpenBaoBackupPushSecrets` probes
   whether any backup PushSecret is still live (exists *and*
   `DeletionTimestamp == 0`). If so, a single Normal Event with reason
   `FinalizingOpenBaoSecrets` is emitted. Once the PushSecret transitions
   to Terminating (DeletionTimestamp set) or disappears, subsequent
   requeues observe no live PushSecret and suppress the emit, giving
   exactly-once semantics per termination across requeue loops.
3. **Cleanup.** `finalizeOpenBaoSecrets` runs in **three** sequential
   passes over the backup PushSecret names (CC-0091 extended the original
   CC-0079 two-pass design with a Pass-0 adoption wait):
   - **Pass-0 — adoption wait.** For each backup PushSecret that still
     exists and is not already Terminating, verify that ESO's cleanup
     finalizer (`pushsecret.externalsecrets.io/finalizer`)
     is present. On the first unadopted PushSecret, set `done=false`,
     record `SecretsReady=False / WaitingForESOAdoption` via
     `setOpenBaoWaitingForESOAdoptionCondition`, and return **without**
     firing any `Delete`. This closes the race where a Keystone CR
     deleted seconds after creation outruns ESO's first reconcile — a
     racing `Delete` would remove the PushSecret object outright before
     ESO had a chance to install `DeletionPolicy=Delete`, orphaning the
     kv-v2 path in OpenBao (see
     [Three-pass lifecycle with two blocked states](#three-pass-lifecycle-with-two-blocked-states)).
   - **Pass-1 — parallel Delete.** Issue `Delete` on every backup
     PushSecret (tolerating `NotFound`), so ESO's cleanup finalizers
     fire on all of them in parallel rather than in a serialised
     Delete→Get loop that would double the worst-case deletion window.
   - **Pass-2 — wait-for-gone.** Re-`Get` each name; `done=true` only
     when **every** PushSecret returns `NotFound`. On the first still-
     present PushSecret (typically Terminating behind ESO's cleanup
     finalizer) the handler sets `done=false` and records
     `SecretsReady=False / OpenBaoFinalizerBlocked` via
     `setOpenBaoFinalizerBlockedCondition`, so the stuck object's name
     surfaces on `kubectl get keystone` rather than being returned by
     the function (the signature is `(done bool, err error)` only).
4. **Requeue while blocked.** If `done=false`, the handler records the
   `SecretsReady=False / OpenBaoFinalizerBlocked` condition (see
   [SecretsReady=False / OpenBaoFinalizerBlocked](#secretsreadyfalse--openbaofinalizerblocked)),
   returns `ctrl.Result{RequeueAfter: RequeueSecretPolling}` (15s), and
   leaves the finalizer in place so the Keystone CR stays alive until ESO
   finishes.
5. **Finalizer release.** When `done=true`, a Normal Event with reason
   `OpenBaoSecretsFinalized` is emitted, the finalizer is removed via
   `controllerutil.RemoveFinalizer`, and the CR is `Update`d. The API
   server then observes the emptied finalizers list and, if no other
   finalizers remain, garbage-collects the Keystone CR.

### Why the finalizer waits

Unlike the MariaDB finalizer (which deliberately does **not** wait for the
MariaDB CRs to disappear from etcd — see
[Why the finalizer does not wait](#why-the-finalizer-does-not-wait)), the
OpenBao finalizer **must** wait for the PushSecrets to be fully
garbage-collected. The asymmetry is driven by the cleanup mechanism:

- MariaDB: the `Database` CR owns the actual remote resource (the
  database) via its own finalizer; releasing the Keystone finalizer early
  does not orphan the database because owner references and the MariaDB
  operator both handle the cascade.
- OpenBao: the remote resource (the KV-v2 path) is purged **only as a
  side-effect of the PushSecret's Delete**. If the Keystone finalizer is
  released before ESO has finished its own teardown, and if ESO then fails
  for any reason (auth revoked, OpenBao unreachable, operator crashed), the
  PushSecret may be garbage-collected by the API server while the remote
  path still exists — leaking cryptographic material.

Waiting on the re-`Get` therefore provides the critical guarantee:
"Keystone CR gone" implies "remote KV-v2 path purged (or ESO has surfaced
the failure via its own status)".

### NotFound Tolerance and Idempotency

`finalizeOpenBaoSecrets` treats `NotFound` as success on **both** the
`Delete` call and the follow-up `Get`:

- `Delete` returning `NotFound` → the PushSecret was already removed by GC,
  a prior finalizer pass, or an external actor; log at `V(1)` and continue.
- `Get` returning `NotFound` → the PushSecret (and, via ESO's
  `DeletionPolicy=Delete`, the remote KV-v2 path) is fully gone; count
  this object as done.

This tolerance makes `finalizeOpenBaoSecrets` **idempotent**: calling it
twice in a row with no PushSecrets present returns `(true, nil)` both
times and issues a second batch of Delete calls that are all no-op
NotFounds. The reconcile pass that first observes `done=true` is the only
one that emits `OpenBaoSecretsFinalized` and calls `RemoveFinalizer` — no
duplicate Events are produced under requeue loops. Idempotency matters
because:

- Controller-runtime retries `Reconcile` with exponential backoff on
  transient errors; any finalizer pass may be replayed.
- ESO may have completed the PushSecret deletion before the Keystone
  finalizer's next reconcile observes it.
- An SRE may have manually deleted the PushSecrets before the Keystone
  deletion.

In all three cases the finalizer converges without surfacing spurious
errors.

### Three-pass lifecycle with two blocked states

A single Keystone CR deletion drives the openbao-finalizer through three
sequential passes (Pass-0, Pass-1, Pass-2). Two of those passes can
**block** the handler waiting on ESO; the third (Pass-1) is a non-blocking
fire-and-forget Delete. The two blocked states surface on the
`SecretsReady` condition with `Status=False` but use distinct `Reason`
values so that `kubectl describe keystone` distinguishes them without
reading controller logs:

| Reason | Pass | When Emitted | Typical Remediation |
| --- | --- | --- | --- |
| `WaitingForESOAdoption` | Pre-Delete adoption wait (Pass-0) | A backup PushSecret exists, is not Terminating, and does **not** yet carry ESO's cleanup finalizer. | Resolves itself on ESO workqueue drain. Check `kubectl -n external-secrets logs deploy/external-secrets` for backlog or errors. |
| `OpenBaoFinalizerBlocked` | Post-Delete wait-for-gone (Pass-2) | A backup PushSecret is still present in the API server after `Delete` was issued, typically in Terminating state behind ESO's cleanup finalizer. | ESO is running `DeletionPolicy=Delete` against OpenBao. A persistent block here may indicate OpenBao unreachable or ClusterSecretStore auth revoked. |

The three passes execute in strict order:

1. **Pass-0 — adoption wait.** For each backup PushSecret that exists and
   is not Terminating, the handler `Get`s the object and inspects its
   `metadata.finalizers` for
   `pushsecret.externalsecrets.io/finalizer`. On the
   first unadopted PushSecret it records `WaitingForESOAdoption` and
   returns `(done=false, nil)` **without** firing any `Delete`.
2. **Pass-1 — parallel Delete.** Once every still-present PushSecret is
   adopted (ESO finalizer present) or already Terminating, the handler
   issues `Delete` on every name in a single pass so the cleanup
   finalizers fire in parallel.
3. **Pass-2 — wait-for-gone.** The handler re-`Get`s each name and, on
   the first still-present PushSecret, records `OpenBaoFinalizerBlocked`
   and returns `(done=false, nil)`. Release of the openbao-finalizer
   requires **all** PushSecrets to return `NotFound`.

#### Motivating race (CC-0091)

Without Pass-0, a Keystone CR deleted within 1–2 s of creation can outrun
ESO's first reconcile: the operator calls `Delete` on the PushSecret
before ESO has installed its own cleanup finalizer, the API server
immediately garbage-collects the PushSecret object, and ESO never observes
the `DeletionTimestamp` — so `DeletionPolicy=Delete` never runs and the
referenced kv-v2 path is orphaned in OpenBao. The observed stuck path now
takes the per-CR form `kv-v2/openstack/keystone/{name}/fernet-keys` (CC-0093);
this was originally seen in CI run 24842115250, which predated CC-0093 and
therefore observed the now-legacy flat path
`kv-v2/openstack/keystone/fernet-keys`. The race itself is path-shape
independent — the only difference is the kv-v2 key that would be left
orphaned without Pass-0. Pass-0 gates the operator's `Delete` on ESO having
signalled adoption by installing its cleanup finalizer, so the race
collapses into an observable `WaitingForESOAdoption` state that resolves
on the next ESO reconcile instead of a silent leak.

#### Symmetric coverage of both PushSecrets

Both `{name}-fernet-keys-backup` (Fernet token-signing material) and
`{name}-credential-keys-backup` (credential-encryption keys) are covered
by the same three-pass handler via the single `openBaoBackupPushSecretNames`
iteration — there are no per-resource branches. A mixed adoption state
(one PushSecret adopted by ESO, the other not) blocks on the unadopted
resource in Pass-0 and fires **zero** `Delete` calls in that pass, so
the existing Pass-1 "fire all deletes in parallel" property is preserved
once adoption is confirmed. Adding a third backup target in the future
remains a one-line change in `openBaoBackupPushSecretNames` — Pass-0,
Pass-1, and Pass-2 all iterate the same list.

### SecretsReady=False / WaitingForESOAdoption

While the finalizer is waiting for ESO to adopt a backup PushSecret (i.e.
install its own cleanup finalizer) before the operator issues `Delete`,
the `SecretsReady` condition surfaces the pre-Delete adoption wait:

| Field | Value |
| --- | --- |
| `Type` | `SecretsReady` |
| `Status` | `False` |
| `Reason` | `WaitingForESOAdoption` |
| `Message` | `Waiting for ESO to adopt PushSecret "<name>" (cleanup finalizer not yet installed)` |
| `ObservedGeneration` | `keystone.Generation` |

The message names the first unadopted PushSecret encountered in the
`openBaoBackupPushSecretNames` iteration. On the next reconcile after that
PushSecret gains ESO's cleanup finalizer, Pass-0 advances past it and, if
the other PushSecret is still unadopted, the condition message rotates
to name the next one; otherwise Pass-1 fires `Delete` and the condition
transitions to `OpenBaoFinalizerBlocked` (or clears if all PushSecrets
terminate quickly).

No `Warning` Event is emitted for this state. The structured log line
`openbao finalizer waiting for ESO adoption` is produced at `V(1)` with
`pushsecret=<name>`, and the `SecretsReady=False` condition is the
primary observability signal. `reconcileDeleteOpenBao` returns
`ctrl.Result{RequeueAfter: RequeueSecretPolling}` (15 s — see
`requeue_intervals.go`) on `(done=false, nil)`, so the adoption wait is
actively polled every 15 s until ESO installs its cleanup finalizer.
The handler never force-deletes: if ESO is permanently broken, the
Keystone CR correctly stays Terminating until an operator investigates
(see [Why the finalizer waits](#why-the-finalizer-waits)).

### SecretsReady=False / OpenBaoFinalizerBlocked

While the finalizer is waiting for ESO to finish garbage-collecting a
backup PushSecret, the existing `SecretsReady` condition is re-used to
surface the blocked state:

| Field | Value |
| --- | --- |
| `Type` | `SecretsReady` |
| `Status` | `False` |
| `Reason` | `OpenBaoFinalizerBlocked` |
| `Message` | `Waiting for PushSecret "<name>" to be garbage-collected before releasing openbao-finalizer` |
| `ObservedGeneration` | `keystone.Generation` |

The message names the specific PushSecret (first stuck one encountered)
so `kubectl get keystone -o yaml` surfaces which backup is wedged without
attaching a debugger. Reusing `SecretsReady` (instead of introducing a new
condition type) keeps the status surface coherent — any secret-store
lifecycle concern appears under the same condition type
(CC-0013 for initial ExternalSecret sync, CC-0047 for ClusterSecretStore
health, CC-0079 for finalizer teardown). The condition is persisted through
the existing `updateStatus` path so the message stays fresh across
requeues at the `RequeueSecretPolling` (15s) interval.

The condition clears together with the Keystone CR itself: once both
PushSecrets are `NotFound`, `reconcileDeleteOpenBao` removes the finalizer
and the CR is garbage-collected. There is no explicit "clearing" write —
the condition disappears when its owning object is reaped.

No `Warning` Event is emitted for the blocked state. Controller-runtime
retries at `RequeueSecretPolling`, the structured log line
`openbao finalizer blocked on PushSecret garbage collection` is produced
at `V(1)` with `pushsecret=<name>`, and the `SecretsReady=False`
condition is the primary observability signal.

### Events

Two Normal Events are emitted on the Keystone CR during OpenBao
finalizer-driven cleanup:

| Reason | Type | Message | Emitted When |
| --- | --- | --- | --- |
| `FinalizingOpenBaoSecrets` | `Normal` | `"Cleaning up OpenBao backup PushSecrets before removing Keystone"` | First terminating reconcile pass where at least one backup PushSecret is still live (not Terminating). Subsequent requeues observe the PushSecret Terminating (DeletionTimestamp set) or absent and suppress the emit, giving exactly-once semantics. |
| `OpenBaoSecretsFinalized` | `Normal` | `"OpenBao backup PushSecrets deleted; releasing openbao-finalizer"` | Once per termination, immediately before `RemoveFinalizer` + `Update`. |

If both PushSecrets are already `NotFound` on the first terminating
reconcile (re-runs, pre-deleted PushSecrets), the start Event is skipped
and only the completion Event fires.

All OpenBao-finalizer log lines include the Keystone `name` and
`namespace` via
`log.FromContext(ctx).WithValues("keystone", client.ObjectKeyFromObject(keystone))`,
keeping log correlation consistent with the rest of the reconciler.

### Upgrade Path

Keystone CRs created by an earlier operator version that did not install
the OpenBao finalizer gain it on their next reconcile under the new
version: the live-CR branch sees
`ContainsFinalizer(keystone, keystoneOpenBaoFinalizer) == false`, calls
`AddFinalizer` + `Update`, and requeues. The CR's `status` and `spec` are
otherwise unchanged by the finalizer addition, and subsequent deletions
flow through the full cleanup path. No manual migration of existing CRs is
required.

### Owner References vs OpenBao Finalizer

Both backup PushSecrets carry an `ownerReference` to the Keystone CR
(set by `reconcileFernetKeys` and `reconcileCredentialKeys` via
`controllerutil.SetControllerReference`). Kubernetes' built-in garbage
collector would normally cascade the deletion of the PushSecret objects,
but GC alone is insufficient here:

| Mechanism | What it guarantees |
| --- | --- |
| Owner references | PushSecret CRs are eventually garbage-collected after Keystone CR removal |
| `Spec.DeletionPolicy=Delete` | ESO purges the remote KV-v2 path when the PushSecret's own `Delete` is processed |
| OpenBao finalizer | Keystone CR is removed **only after** both PushSecrets are confirmed `NotFound`, guaranteeing the remote purge has actually run |

Without the finalizer, cascade-deletion of the PushSecret would race
against ESO's own reconciliation — the PushSecret object could be
garbage-collected by the API server before ESO has had a chance to run its
`DeletionPolicy=Delete` path. The finalizer's `Delete` + re-`Get` loop
forces the ordering to be deterministic and observable.

---

## Sub-Reconciler Contracts

All sub-reconcilers are private methods on the `KeystoneReconciler` receiver. Each
sub-reconciler is responsible for:

1. Ensuring the resources it manages exist with the correct spec.
2. Setting its designated status condition with `ObservedGeneration`, `Reason`, and `Message`.
3. Returning `(ctrl.Result{RequeueAfter: N}, nil)` for transient not-ready states.
4. Returning `(ctrl.Result{}, error)` for failures.
5. Returning `(ctrl.Result{}, nil)` when its phase is complete.

### ObservedGeneration Convention

Every `conditions.SetCondition` call **must** include `ObservedGeneration: keystone.Generation`
so that external tooling (ArgoCD health checks, status controllers) can distinguish whether
a condition reflects the current spec or a stale generation (CC-0072). This applies to both
`True` and `False` condition paths — no condition may omit the field.

```go
conditions.SetCondition(&keystone.Status.Conditions, metav1.Condition{
    Type:               "SecretsReady",
    Status:             metav1.ConditionFalse,
    ObservedGeneration: keystone.Generation,
    Reason:             "SecretStoreNotReady",
    Message:            "ClusterSecretStore is not ready",
})
```

Because Go struct literals allow zero-value omission, the compiler cannot enforce
the presence of `ObservedGeneration`. Enforcement is instead provided by a dedicated
unit test in each sub-reconciler test file, following the naming convention
`TestReconcile{SubReconciler}_ConditionObservedGeneration`. Each test exercises at
least the `True` and one `False` condition path with distinct, non-default generation
values (e.g. 7 and 12) to verify the field is propagated correctly.

The integration test `TestIntegration_ObservedGeneration` additionally verifies that
after a full reconcile loop, every condition in the status carries the correct
`ObservedGeneration`.

### Sub-Condition Types

| Condition Type | Set By | Description |
| --- | --- | --- |
| `SecretsReady` | `reconcileSecrets` | ESO-provided credentials are synced |
| `DatabaseReady` | `reconcileDatabase` | MariaDB CRs ready and db_sync complete |
| `FernetKeysReady` | `reconcileFernetKeys` | Fernet Secret, script ConfigMap, CronJob, and PushSecret ensured (CC-0073) |
| `CredentialKeysReady` | `reconcileCredentialKeys` | Credential keys Secret, script ConfigMap, CronJob, and PushSecret ensured (CC-0036, CC-0073) |
| `NetworkPolicyReady` | `reconcileNetworkPolicy` | NetworkPolicy configured or not required (CC-0039) |
| `PolicyValidReady` | `reconcilePolicyValidation` | Policy override validation passed or not required (CC-0058) |
| `DeploymentReady` | `reconcileDeployment` | Deployment available and Service created |
| `KeystoneAPIReady` | `reconcileHealthCheck` | Keystone API responding to HTTP health check (CC-0067) |
| `HTTPRouteReady` | `reconcileHTTPRoute` | HTTPRoute accepted by Gateway, not required (no `spec.gateway`), or Gateway API CRD missing (reason `GatewayAPINotInstalled`) |
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
    keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error)
```

**Purpose:** Manage Fernet token signing keys — initial generation, rotation schedule,
and disaster recovery backup to OpenBao.

**Steps (in order):**

1. **Ensure Fernet keys Secret** — If `{name}-fernet-keys` Secret does not exist,
   generate initial keys and create the Secret with a controller owner reference.
2. **Ensure rotation RBAC** — Create or update the ServiceAccount, Role, and
   RoleBinding (`{name}-fernet-rotate`) for the rotation CronJob.
3. **Create script ConfigMap** — Create an immutable, versioned ConfigMap
   `{name}-fernet-rotate-script-{hash}` containing the embedded
   `fernet_rotate.sh` script (CC-0073). Uses `config.CreateImmutableConfigMap()`
   which appends a content-hash suffix and sets `immutable: true`.
4. **Ensure rotation CronJob** — Create or update `{name}-fernet-rotate` CronJob
   with the schedule from `spec.fernet.rotationSchedule`.
5. **Ensure PushSecret** — Create or update `{name}-fernet-keys-backup` PushSecret
   targeting `kv-v2/data/openstack/keystone/{name}/fernet-keys` in the `openbao`
   ClusterSecretStore.

**Key Generation:**

| Property | Value |
| --- | --- |
| Algorithm | `crypto/rand` (32 bytes) |
| Encoding | URL-safe base64 without padding (`base64.URLEncoding.WithPadding(base64.NoPadding)`) |
| Key count | `max(spec.fernet.maxActiveKeys, 3)` |
| Secret data keys | String indices: `"0"`, `"1"`, `"2"`, ... |
| Secret name | `{name}-fernet-keys` |

**Rotation CronJob:**

| Field | Value |
| --- | --- |
| Name | `{name}-fernet-rotate` |
| Schedule | `spec.fernet.rotationSchedule` |
| ServiceAccount | `{name}-fernet-rotate` |
| Init container | Copies keys from `fernet-keys-src` (Secret) to `fernet-keys` (emptyDir) |
| Command | `/scripts/fernet_rotate.sh` (CC-0073) |
| Volume `fernet-keys-src` | Secret `{name}-fernet-keys` (read-only source) |
| Volume `fernet-keys` | emptyDir (writable working copy) |
| Volume `credential-keys` | Secret `{name}-credential-keys` (read-only, required by config) |
| Volume `config` | ConfigMap `{configMapName}` |
| Volume `scripts` | ConfigMap `{name}-fernet-rotate-script-{hash}` (`defaultMode: 0555`) (CC-0073) |

**PushSecret:**

| Field | Value |
| --- | --- |
| Name | `{name}-fernet-keys-backup` |
| Store | `ClusterSecretStore/openbao` |
| Source Secret | `{name}-fernet-keys` |
| Remote Key | `kv-v2/data/openstack/keystone/{name}/fernet-keys` |

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `GeneratingKeys` | "Initial Fernet keys have been generated" | — |
| `True` | `FernetKeysRotated` | "rotation applied; staging secret cleared" | — (transient: apply-success short-circuit at `reconcile_fernet.go:97-103`; operators see this immediately after a rotation apply via `kubectl describe`, before the next reconcile transitions to the steady-state Reason) |
| `True` | `FernetKeysAvailable` | "Fernet keys Secret exists and rotation CronJob is configured" | — |

**Versioned Script ConfigMap (CC-0073):**

The rotation script is embedded in the Go binary via `go:embed` and mounted into the
CronJob pod through a versioned, immutable ConfigMap. The ConfigMap name includes a
content-hash suffix (e.g., `{name}-fernet-rotate-script-abc123`), which ensures that
changes to the script trigger a new CronJob spec and thus a rolling update. The
ConfigMap is created with `immutable: true` to prevent accidental modification and
enable kube-apiserver caching optimizations.

**Error handling:** Errors from Secret creation, RBAC ensure, script ConfigMap creation,
CronJob ensure, or PushSecret ensure are wrapped with context and returned directly.
No requeue delays — errors trigger controller-runtime exponential backoff.

**Idempotency:** If the `{name}-fernet-keys` Secret already exists, it is not
modified. This prevents overwriting keys that have been rotated by the CronJob.
The script ConfigMap uses content-based naming — if the script has not changed,
`config.CreateImmutableConfigMap()` returns the existing ConfigMap name without
creating a new one.

**Shared library calls:** `config.CreateImmutableConfigMap()`, `job.EnsureCronJob()`,
`secrets.EnsurePushSecret()`

#### Key Rotation RBAC Split (CC-0081)

The Fernet rotation path separates the **compute** of new keys (performed by
the rotation CronJob) from the **write** onto the production Secret
(performed by the operator). The CronJob ServiceAccount has no verb that can
mutate the production `{name}-fernet-keys` Secret — eliminating the
token-forgery primitive from the CronJob's attack surface.

**Staging Secret naming.** Per `fernetStagingSecretName`, the staging Secret
is `{keystone.Name}-fernet-keys-rotation`. It is created and owned by the
operator via `ensureFernetStagingSecret`:

- Empty `Data` on creation; the CronJob PATCHes `Data` on rotation.
- Labels: `commonLabels(keystone)` + `forge.c5c3.io/rotation-target=fernet-keys`.
- Owner reference: the Keystone CR (garbage-collected with the CR).

**Completion annotation contract.** The CronJob's `fernet_rotate.sh` PATCH
writes **both** the new `data` map and the
`forge.c5c3.io/rotation-completed-at` annotation in a single atomic
strategic-merge PATCH. Format: `datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds").replace("+00:00","Z")`
— an RFC3339 UTC timestamp such as `2026-04-18T12:34:56Z`. The operator
treats the annotation as the **single-shot commit marker**: it never rewrites
the production Secret unless the annotation is present and parses cleanly.

**Operator validation rules.** Before copying staged data to production,
`applyRotationOutput` calls `validateRotationOutput` (see
`operators/keystone/internal/controller/rotation_validation.go`) which
enforces all of:

- **Key count:** `minKeys=3`, `maxKeys=normalizedFernetMaxActiveKeys(keystone)+1`. The `+1` tolerates the brief window in which the newly staged primary coexists with the existing active set. Violations return `ErrKeyCountOutOfRange`.
- **Key format:** each value is exactly 44 bytes and base64url-decodes to 32 bytes — the `generateFernetKey` output shape. Violations return `ErrInvalidKeyFormat`.
- **Uniqueness:** no two values are byte-equal. Violations return `ErrDuplicateKeys`.

On rejection the operator emits a Warning event `RotationRejected` on the
Keystone CR and **retains the staging Secret** for human inspection. On a
malformed `rotation-completed-at` value the operator emits
`RotationAnnotationInvalid` and leaves staging in place, allowing the next
CronJob run to overwrite with a valid payload.

**Apply algorithm.** On a valid staging Secret, `applyRotationOutput` GETs the
production Secret, replaces its `.data` map with the staging payload verbatim,
issues an `Update`, deletes the staging Secret, and emits a Normal event
`FernetKeysRotated`. UPDATE-then-DELETE ordering is deliberate: if DELETE
fails the production Secret is already updated, and a subsequent reconcile
will no-op until the next CronJob run writes a new annotation timestamp.

**Production `Secret.Data` field ownership.** The production Fernet Secret's
`.data` map is owned solely by the operator. Writes happen exclusively through
the `applyRotationOutput` GET-then-`Update` round-trip under the
controller-owned `ResourceVersion`, which guarantees optimistic concurrency
(a concurrent writer triggers a 409 Conflict and the reconciler requeues).
The Update fully replaces the map — stale key indices absent from the staging
payload (e.g. those renumbered by `keystone-manage fernet_rotate` or trimmed
by a reduction in `spec.fernet.maxActiveKeys`) are removed, which is the
atomic-swap semantic REQ-006 requires. A strategic-merge PATCH on this field
would merge by key and allow decommissioned keys to accumulate, so it is
intentionally avoided.

**RBAC verb matrix.** Two principals touch the production and staging
Secrets, with strictly disjoint capabilities:

| Principal | Resource | Verbs | Source |
| --- | --- | --- | --- |
| CronJob ServiceAccount (`{name}-fernet-rotate`) | Secret `{name}-fernet-keys` (production) | `get` | Role rule 1 in `ensureFernetRotationRBAC` |
| CronJob ServiceAccount (`{name}-fernet-rotate`) | Secret `{name}-fernet-keys-rotation` (staging) | `get`, `patch` | Role rule 2 in `ensureFernetRotationRBAC` |
| Operator ServiceAccount | Secret `{name}-fernet-keys` (production) | `get`, `patch`, `create`, `update`, `delete`, `list`, `watch` | Cluster-scoped core `secrets` verbs (see [RBAC Permissions](#rbac-permissions)) |
| Operator ServiceAccount | Secret `{name}-fernet-keys-rotation` (staging) | `get`, `create`, `update`, `patch`, `delete`, `list`, `watch` | Cluster-scoped core `secrets` verbs |

The CronJob ServiceAccount has **no `create`, `update`, or `delete`** on
either Secret — only `get` (both) and `patch` (staging only). Chainsaw tests
in `tests/e2e/keystone/fernet-rotation/chainsaw-test.yaml` assert this
exact verb split.

---

### reconcileCredentialKeys

**File:** `operators/keystone/internal/controller/reconcile_credential.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcileCredentialKeys(ctx context.Context,
    keystone *keystonev1alpha1.Keystone, configMapName string) (ctrl.Result, error)
```

**Purpose:** Manage credential encryption keys — initial generation, rotation schedule
with credential migration, and disaster recovery backup to OpenBao (CC-0036).

**Steps (in order):**

1. **Ensure credential keys Secret** — If `{name}-credential-keys` Secret does not
   exist, generate initial keys and create the Secret with a controller owner reference.
2. **Ensure rotation RBAC** — Create or update the ServiceAccount, Role, and
   RoleBinding (`{name}-credential-rotate`) for the rotation CronJob.
3. **Create script ConfigMap** — Create an immutable, versioned ConfigMap
   `{name}-credential-rotate-script-{hash}` containing the embedded
   `credential_rotate.sh` script (CC-0073). Uses `config.CreateImmutableConfigMap()`
   which appends a content-hash suffix and sets `immutable: true`.
4. **Ensure rotation CronJob** — Create or update `{name}-credential-rotate` CronJob
   with the schedule from `spec.credentialKeys.rotationSchedule`.
5. **Ensure PushSecret** — Create or update `{name}-credential-keys-backup` PushSecret
   targeting `kv-v2/data/openstack/keystone/{name}/credential-keys` in the `openbao`
   ClusterSecretStore.

**Key Generation:**

| Property | Value |
| --- | --- |
| Algorithm | `crypto/rand` (32 bytes), same format as Fernet keys |
| Encoding | URL-safe base64 (`base64.URLEncoding.EncodeToString`) |
| Key count | `max(spec.credentialKeys.maxActiveKeys, 3)` |
| Secret data keys | String indices: `"0"`, `"1"`, `"2"`, ... |
| Secret name | `{name}-credential-keys` |

**Rotation CronJob:**

| Field | Value |
| --- | --- |
| Name | `{name}-credential-rotate` |
| Schedule | `spec.credentialKeys.rotationSchedule` |
| ServiceAccount | `{name}-credential-rotate` |
| Init container | Copies keys from `credential-keys-src` (Secret) to `credential-keys` (emptyDir) |
| Command | `/scripts/credential_rotate.sh` (CC-0073) |
| Volume `credential-keys-src` | Secret `{name}-credential-keys` (read-only source) |
| Volume `credential-keys` | emptyDir (writable working copy) |
| Volume `fernet-keys` | Secret `{name}-fernet-keys` (read-only, required by config) |
| Volume `config` | ConfigMap `{configMapName}` |
| Volume `scripts` | ConfigMap `{name}-credential-rotate-script-{hash}` (`defaultMode: 0555`) (CC-0073) |

> **Note:** The `credential_rotate.sh` script runs both `credential_rotate` and
> `credential_migrate`. The migrate step re-encrypts existing credentials in the
> database with the new primary key, which is critical to prevent data loss when
> old keys are eventually purged (CC-0036).

**PushSecret:**

| Field | Value |
| --- | --- |
| Name | `{name}-credential-keys-backup` |
| Store | `ClusterSecretStore/openbao` |
| Source Secret | `{name}-credential-keys` |
| Remote Key | `kv-v2/data/openstack/keystone/{name}/credential-keys` |

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `GeneratingKeys` | "Initial credential keys have been generated" | — |
| `True` | `CredentialKeysRotated` | "rotation applied; staging secret cleared" | — (transient: apply-success short-circuit at `reconcile_credential.go:107-113`; operators see this immediately after a rotation apply via `kubectl describe`, before the next reconcile transitions to the steady-state Reason) |
| `True` | `CredentialKeysAvailable` | "Credential keys Secret exists and rotation CronJob is configured" | — |

**Versioned Script ConfigMap (CC-0073):**

Uses the same versioned, immutable ConfigMap pattern as `reconcileFernetKeys`. See the
description in that section for details on content-hash naming and immutability.

**Error handling:** Errors from Secret creation, RBAC ensure, script ConfigMap creation,
CronJob ensure, or PushSecret ensure are wrapped with context and returned directly.
No requeue delays — errors trigger controller-runtime exponential backoff.

**Idempotency:** If the `{name}-credential-keys` Secret already exists, it is not
modified. This prevents overwriting keys that have been rotated by the CronJob.

**Shared library calls:** `config.CreateImmutableConfigMap()`, `job.EnsureCronJob()`,
`secrets.EnsurePushSecret()`

#### Key Rotation RBAC Split (CC-0081)

The credential rotation path mirrors the Fernet split exactly: the
`{name}-credential-rotate` CronJob computes rotated keys, PATCHes them into
a dedicated staging Secret, and the operator performs the final write onto
the production `{name}-credential-keys` Secret.

**Staging Secret naming.** Per `credentialStagingSecretName`, the staging
Secret is `{keystone.Name}-credential-keys-rotation`. It is created and
owned by the operator via `ensureCredentialStagingSecret`:

- Empty `Data` on creation; the CronJob PATCHes `Data` on rotation.
- Labels: `commonLabels(keystone)` + `forge.c5c3.io/rotation-target=credential-keys`.
- Owner reference: the Keystone CR (garbage-collected with the CR).

**Completion annotation contract.** `credential_rotate.sh` runs
`keystone-manage credential_rotate` and then `keystone-manage credential_migrate`
(in that order — migrate re-encrypts existing stored credentials with the new
primary key) before emitting a single atomic PATCH that sets both `data`
and the `forge.c5c3.io/rotation-completed-at` annotation (RFC3339 UTC, `Z`
suffix). As with Fernet, the annotation is the single-shot commit marker;
absence or malformed format blocks the operator's apply path.

**Operator validation rules.** Identical to Fernet, with one parameter
difference — `maxKeys=normalizedCredentialMaxActiveKeys(keystone)+1`:

- **Key count:** `[3, normalizedCredentialMaxActiveKeys(keystone)+1]` inclusive. `ErrKeyCountOutOfRange` on violation.
- **Key format:** 44-byte base64url decoding to 32 bytes. `ErrInvalidKeyFormat` on violation.
- **Uniqueness:** byte-distinct values. `ErrDuplicateKeys` on violation.

Rejection emits `RotationRejected` (Warning, on the CR) and retains the
staging Secret. A malformed `rotation-completed-at` emits
`RotationAnnotationInvalid` and leaves staging intact.

**Apply algorithm.** On a valid staging Secret, `applyRotationOutput` GETs
the production Secret, replaces its `.data` map with the staging payload
verbatim, issues an `Update`, deletes the staging Secret, and emits a
Normal event `CredentialKeysRotated`. Because the CronJob already ran
`credential_migrate` before PATCHing staging, every credential row in the
database is re-encrypted with the new primary by the time the operator
commits the Secret swap — no data loss when old keys age out (CC-0036).

> **Key-rollover window (pre-existing, not introduced by CC-0081).** There
> is a ~60s window between `credential_migrate` completion and the kubelet
> refreshing the in-place Secret projection (CC-0074). During that window,
> running Keystone pods still have the old credential keyset mounted —
> database rows are already encrypted under the new primary, but the pods
> cannot decrypt them yet. This is a pre-existing property of the rotation
> flow and is not a regression introduced by CC-0081; it is tracked
> separately under CC-0074 and should be considered when sizing the
> rotation schedule against request volume.

**Production `Secret.Data` field ownership.** Same contract as Fernet: the
operator owns the production `.data` map, writes are `Update`-based under
the controller-owned `ResourceVersion`, and the Update fully replaces the
map so stale indices are removed atomically (REQ-006). Strategic-merge PATCH
is intentionally avoided here for the same merge-vs-replace reason
documented in the Fernet section.

**RBAC verb matrix.** The CronJob ServiceAccount and the operator
ServiceAccount have strictly disjoint capabilities on the production and
staging Secrets:

| Principal | Resource | Verbs | Source |
| --- | --- | --- | --- |
| CronJob ServiceAccount (`{name}-credential-rotate`) | Secret `{name}-credential-keys` (production) | `get` | Role rule 1 in `ensureCredentialRotationRBAC` |
| CronJob ServiceAccount (`{name}-credential-rotate`) | Secret `{name}-credential-keys-rotation` (staging) | `get`, `patch` | Role rule 2 in `ensureCredentialRotationRBAC` |
| Operator ServiceAccount | Secret `{name}-credential-keys` (production) | `get`, `patch`, `create`, `update`, `delete`, `list`, `watch` | Cluster-scoped core `secrets` verbs (see [RBAC Permissions](#rbac-permissions)) |
| Operator ServiceAccount | Secret `{name}-credential-keys-rotation` (staging) | `get`, `create`, `update`, `patch`, `delete`, `list`, `watch` | Cluster-scoped core `secrets` verbs |

The CronJob ServiceAccount has **no `create`, `update`, or `delete`** on
either Secret — only `get` (both) and `patch` (staging only). Chainsaw tests
in `tests/e2e/keystone/credential-rotation/chainsaw-test.yaml` assert this
exact verb split.

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

**In-Place Key Rotation (CC-0074):**

Fernet and credential key rotation is handled in-place via kubelet Secret
projection. When the rotation CronJob updates a Secret, the kubelet
automatically projects the new data into running pods without requiring a
Deployment rollout. The pod template does not include hash annotations for
Secret data, so Secret changes do not trigger rolling restarts. This preserves
Keystone availability, PDB budget, and uWSGI/Memcached connections during
routine key rotation.

```text
CronJob rotates keys → Secret data changes → kubelet projects new keys
  → running pods see updated key files (no rollout)
```

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

When the Deployment becomes ready, `status.endpoint` is set via the
`keystoneStatusEndpoint` helper (defined in `reconcile_httproute.go`, CC-0065,
REQ-004):

| `spec.gateway` | Resulting `status.endpoint` |
| --- | --- |
| nil | `http://{name}-api.{namespace}.svc.cluster.local:5000/v3` |
| set (hostname `api.example.com`) | `https://api.example.com/v3` |

The helper is owned by `reconcile_httproute.go` but invoked from
`reconcileDeployment` because the endpoint must be populated before the
`reconcileHealthCheck` step reads it. `reconcileHTTPRoute` runs later in the
sequence and does not mutate `status.endpoint`.

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

### pruneStaleConfigMaps

**File:** `operators/keystone/internal/controller/reconcile_config.go`

**Signature:**

```go
func (r *KeystoneReconciler) pruneStaleConfigMaps(ctx context.Context,
    keystone *keystonev1alpha1.Keystone, configMapName string) error
```

**Purpose:** Remove historical immutable ConfigMaps that exceed the retain count
after the Deployment has rolled out successfully. This prevents unbounded accumulation
of `{name}-config-{hash}` ConfigMaps in the namespace (CC-0077).

> **Note:** This is not a sub-reconciler — it does not set any status condition and
> returns only `error`. It is a thin wrapper around `config.PruneImmutableConfigMaps`
> that derives the `baseName` from the Keystone CR name and passes the hardcoded
> retain count.

**Placement rationale:** Pruning runs after `reconcileDeployment` returns ready to
ensure all pods are running the new configuration before old ConfigMaps are deleted.
During a rolling update, old ReplicaSet pods still reference the previous ConfigMap —
pruning before Deployment readiness could delete a ConfigMap that is still mounted.

**Parameters:**

| Name | Source | Description |
| --- | --- | --- |
| `keystone` | CR | Owning Keystone CR (provides UID for owner-reference filtering and name for baseName) |
| `configMapName` | `reconcileConfig` return value | Currently active ConfigMap name (never deleted) |

**Delegation:**

```go
baseName := fmt.Sprintf("%s-config", keystone.Name)
config.PruneImmutableConfigMaps(ctx, r.Client, keystone,
    baseName, keystone.Namespace, configMapName, defaultConfigMapRetainCount)
```

**Constants:**

| Name | Value | Description |
| --- | --- | --- |
| `defaultConfigMapRetainCount` | `3` | Number of historical ConfigMaps to keep beyond the current active one. Keeps current + 3 historical = 4 total, sufficient for rollback. Not CRD-configurable by design. |

**Behavior:**

| State | Result |
| --- | --- |
| 5 historical ConfigMaps, retain=3 | 2 oldest deleted, 3 newest + current remain (4 total) |
| 2 historical ConfigMaps, retain=3 | No-op (fewer than retain count) |
| 0 historical ConfigMaps | No-op |
| ConfigMap owned by different CR | Skipped (owner UID mismatch) |
| ConfigMap with no owner reference | Skipped |
| ConfigMap deleted between list and delete | `NotFound` silently ignored via `client.IgnoreNotFound` |

**Auditability:** Each pruned ConfigMap name is logged at info level with structured
fields `name` and `namespace`.

**Error handling:** Errors from listing or deleting ConfigMaps are wrapped with
context and returned to the reconcile loop, which applies exponential backoff via
controller-runtime. All preceding steps (config creation, deployment) are idempotent,
so requeue after a pruning error is safe.

**Shared library calls:** `config.PruneImmutableConfigMaps()`

---

### reconcileHTTPRoute

**File:** `operators/keystone/internal/controller/reconcile_httproute.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcileHTTPRoute(ctx context.Context,
    keystone *keystonev1alpha1.Keystone) (ctrl.Result, error)
```

**Purpose:** Manage the optional Gateway API `HTTPRoute` that exposes the
Keystone API outside the cluster via a pre-existing `Gateway` (CC-0065). The
Gateway is owned by the platform team; this sub-reconciler only ensures the
route that attaches the Keystone Service to it. Four lifecycle paths
(REQ-001, REQ-002, REQ-005):

0. **Gateway API not installed** (`r.gatewayAPIAvailable` is false): The
   CRD presence check in `SetupWithManager` found no mapping for
   `HTTPRoute.gateway.networking.k8s.io/v1`, so the `Owns(HTTPRoute)` watch
   was skipped. When `spec.gateway` is nil, set `HTTPRouteReady=True` with
   reason `HTTPRouteNotRequired` (no delete attempt — `c.Delete` would fail
   with `no matches for kind`). When `spec.gateway` is set, set
   `HTTPRouteReady=False` with reason `GatewayAPINotInstalled` and a message
   pointing the user at the missing CRD; the operator otherwise keeps
   reconciling so that Keystone CRs without `spec.gateway` still become
   Ready. Runtime installation of the CRD requires an operator restart for
   the RESTMapper probe and the `Owns()` watch to pick it up.
1. **Gateway disabled** (`spec.gateway` is nil, CRD present): Delete any
   existing `{name}-api` HTTPRoute and set `HTTPRouteReady=True` with reason
   `HTTPRouteNotRequired`.
2. **Gateway enabled** (`spec.gateway` is set, CRD present): Build the
   desired HTTPRoute via `buildKeystoneHTTPRoute()` and call
   `ensureHTTPRoute()` to create or update it. Re-fetch to read the parent
   `Accepted` condition written by the Gateway controller, then reflect
   that status as `HTTPRouteReady`: `Accepted=True` → condition `True` with
   reason `HTTPRouteAccepted`; otherwise condition `False` with reason
   `HTTPRouteNotAccepted` and a 10s requeue.
3. **Error**: Propagate errors from ensure/delete/get operations with
   descriptive context.

**Placement rationale:** Runs after `reconcileDeployment` + `pruneStaleConfigMaps`
so the backend Service (`{name}-api`) is guaranteed to exist before the HTTPRoute
references it, and before `reconcileHealthCheck` reads `status.endpoint`. The
route is deliberately not part of `reconcileParallelGroup` because it has a
transitive dependency on the Service created by `reconcileDeployment`.

**HTTPRoute Construction (`buildKeystoneHTTPRoute`, REQ-001, REQ-003, REQ-006):**

| HTTPRoute Field | Source |
| --- | --- |
| `metadata.name` | `{name}-api` (shared with the Keystone API Service) |
| `metadata.namespace` | Keystone CR namespace |
| `metadata.labels` | `commonLabels(keystone)` |
| `metadata.annotations` | `spec.gateway.annotations` (copied; operator-managed keys stay authoritative on merge) |
| `spec.parentRefs[0].name` | `spec.gateway.parentRef.name` |
| `spec.parentRefs[0].namespace` | `spec.gateway.parentRef.namespace` (omitted when empty; defaults to CR namespace) |
| `spec.parentRefs[0].sectionName` | `spec.gateway.parentRef.sectionName` (omitted when empty) |
| `spec.hostnames[0]` | `spec.gateway.hostname` |
| `spec.rules[0].matches[0].path.type` | `PathPrefix` |
| `spec.rules[0].matches[0].path.value` | `spec.gateway.path` (defaults to `/` when empty, `defaultHTTPRoutePath`) |
| `spec.rules[0].backendRefs[0].kind` | `Service` |
| `spec.rules[0].backendRefs[0].name` | `{name}-api` |
| `spec.rules[0].backendRefs[0].port` | `5000` (`keystoneAPIPort`) |
| `metadata.ownerReferences` | Keystone CR (controller owner via `controllerutil.SetControllerReference`) |

**Merge strategy (`ensureHTTPRoute`):** Mirrors `ensureNetworkPolicy`. On
update, the `Spec` is overwritten with the desired state, `labels` and
`annotations` are merged additively so user-added metadata is preserved while
operator-managed keys remain authoritative, and the owner reference is
re-asserted. `apiequality.Semantic.DeepEqual` guards against no-op writes —
`client.Update` is called only when `Spec`, labels, annotations, or owner
references actually changed. Empty maps are normalized to nil via
`hrNormalizeMap` so nil-vs-empty does not trigger a spurious diff.

**Acceptance Detection (`isHTTPRouteAccepted`, REQ-005):** Iterates
`status.parents[*].conditions` and returns `true` as soon as any parent reports
`Accepted=True`. An empty `Parents` slice (Gateway controller has not yet
observed the route) is treated as "not yet accepted", yielding a `False`
condition with a short requeue rather than a permanent error.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `True` | `HTTPRouteNotRequired` | `"External API exposure via Gateway API is not configured"` | — |
| `True` | `HTTPRouteAccepted` | `"HTTPRoute accepted by Gateway"` | — |
| `False` | `HTTPRouteNotAccepted` | `"HTTPRoute not yet accepted by Gateway"` | 10s |

**Requeue Constants:**

| Constant | Value | Purpose |
| --- | --- | --- |
| `requeueHTTPRouteAccepted` | `RequeueDeploymentPolling` (10s) | Interval for requeuing while waiting for the Gateway controller to set `Accepted=True` on the route's parent status |
| `keystoneAPIPort` | `gatewayv1.PortNumber(5000)` | Backend Service port targeted by the route |
| `defaultHTTPRoutePath` | `"/"` | Path prefix applied when `spec.gateway.path` is empty |

**`status.endpoint` Derivation (REQ-004):** The gateway-aware helper
`keystoneStatusEndpoint()` is defined alongside this sub-reconciler but called
from `reconcileDeployment`. When `spec.gateway.hostname` is set, the helper
returns `https://{hostname}/v3`; otherwise it returns the cluster-local
`http://{name}-api.{namespace}.svc.cluster.local:5000/v3`. The gateway path
prefix from `spec.gateway.path` is used for HTTPRoute routing only; it is not
appended to `status.endpoint`. The `https` scheme is emitted unconditionally
when a gateway hostname is configured — gateways are the public-ingress hop
and terminate TLS.

**Interaction with `reconcileNetworkPolicy` (REQ-008):** When `spec.gateway`
is set, `reconcileNetworkPolicy` appends an extra ingress peer that selects
the gateway's namespace by the well-known label
`kubernetes.io/metadata.name={parentRef.namespace or CR namespace}`. This lets
Gateway data-plane pods reach the Keystone API Service on port 5000 without
widening the base NetworkPolicy. The peer is added only while `spec.gateway`
is non-nil and is removed automatically on the next reconcile after
`spec.gateway` is cleared.

**Error handling:** Errors from `ensureHTTPRoute()` are wrapped with
"ensuring HTTPRoute" context; errors from `deleteHTTPRoute()` with "deleting
HTTPRoute"; the post-ensure `Get` with "getting HTTPRoute {ns}/{name}". All
are returned directly to controller-runtime for exponential backoff.
Acceptance failures are **not** returned as errors — they set a False
condition and requeue at a fixed interval.

**Shared library calls:** none. All helpers (`ensureHTTPRoute`,
`deleteHTTPRoute`, `buildKeystoneHTTPRoute`, `isHTTPRouteAccepted`,
`hrNormalizeMap`, `keystoneStatusEndpoint`) are defined in
`reconcile_httproute.go`.

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
| `reconcileCredentialKeys` | — | — | API error → exponential backoff |
| `reconcileNetworkPolicy` | — | — | API error → exponential backoff |
| `reconcileConfig` | — | — | Secret read failure, render failure → exponential backoff |
| `reconcilePolicyValidation` | Job running | 15s | `ErrJobFailed` from validation → descriptive error |
| `reconcileDeployment` | Deployment not available | 10s | API error → exponential backoff |
| `pruneStaleConfigMaps` | — | — | List/delete failure → exponential backoff (CC-0077) |
| `reconcileHTTPRoute` | Gateway has not yet set `Accepted=True` on parent status | 10s | API error on ensure/get/delete → exponential backoff (CC-0065) |
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
| Secret | `{name}-fernet-keys` | Keystone CR |
| Secret | `{name}-fernet-keys-rotation` | Keystone CR (rotation staging, CC-0081) |
| CronJob | `{name}-fernet-rotate` | Keystone CR |
| PushSecret | `{name}-fernet-keys-backup` | Keystone CR |
| Secret | `{name}-credential-keys` | Keystone CR |
| Secret | `{name}-credential-keys-rotation` | Keystone CR (rotation staging, CC-0081) |
| CronJob | `{name}-credential-rotate` | Keystone CR |
| PushSecret | `{name}-credential-keys-backup` | Keystone CR |
| ConfigMap | `{name}-config-{hash}` | Keystone CR |
| Job | `keystone-db-sync` | Keystone CR | <!-- TODO: align to {name}-* pattern (pre-existing, CC-0073 W-002) -->
| Job | `keystone-bootstrap` | Keystone CR | <!-- TODO: align to {name}-* pattern (pre-existing, CC-0073 W-002) -->
| Deployment | `keystone-api` | Keystone CR | <!-- TODO: align to {name}-* pattern (pre-existing, CC-0073 W-002) -->
| Service | `keystone-api` | Keystone CR | <!-- TODO: align to {name}-* pattern (pre-existing, CC-0073 W-002) -->
| PodDisruptionBudget | `{name}-api` | Keystone CR |
| HorizontalPodAutoscaler | `{name}-api` | Keystone CR (only when `spec.autoscaling` is set) |
| HTTPRoute | `{name}-api` | Keystone CR (only when `spec.gateway` is set, CC-0065) |
| Job | `{name}-policy-validation` | Keystone CR (only when `spec.policyOverrides` is set) |
| CronJob | `{name}-trust-flush` | Keystone CR (only when `spec.trustFlush` is set) |
| ConfigMap | `{name}-fernet-rotate-script-{hash}` | Keystone CR (CC-0073) |
| ConfigMap | `{name}-credential-rotate-script-{hash}` | Keystone CR (CC-0073) |
| Database | `keystone` | Keystone CR (managed mode only; additionally cleaned up by the finalizer — CC-0078) |
| User | `keystone` | Keystone CR (managed mode only; additionally cleaned up by the finalizer — CC-0078) |
| Grant | `keystone` | Keystone CR (managed mode only; additionally cleaned up by the finalizer — CC-0078) |

---

## Database credentials and oslo.config env-var overrides (CC-0080)

Keystone's `[database] connection` URL embeds the MySQL username and password.
Prior to CC-0080, the rendered URL was written directly into `keystone.conf`
inside the operator-managed immutable ConfigMap. ConfigMaps lack encryption at
rest and are routinely granted broad `get` RBAC for observability workflows,
which caused production database credentials to be readable by any actor with
`get configmap` on the Keystone namespace. CC-0080 replaces that design by (1)
writing a non-secret placeholder into the ConfigMap and (2) materialising the
real connection URL into a derived Kubernetes `Secret` which every pod consumes
through the oslo.config `OS_<GROUP>__<OPTION>` environment-variable override
mechanism.

### Derived `<name>-db-connection` Secret

The `reconcileDBConnectionSecret` sub-reconciler (in
`operators/keystone/internal/controller/reconcile_dbconnection_secret.go`) runs
between `reconcileSecrets` and `reconcileConfig`. On every reconcile it:

1. Reads the upstream DB credentials `Secret` referenced by
   `spec.database.secretRef`. In managed mode the username is the Keystone CR
   name (matching the MariaDB `User` CR); in brownfield mode both `username`
   and `password` keys are read from the upstream Secret.
2. Builds the pymysql URL using `url.UserPassword()` for RFC 3986-compliant
   percent-encoding of reserved characters in the userinfo component, the
   shared `resolveDatabaseHost()` / `dbPort()` helpers for host resolution, and
   `?charset=utf8` as the fixed query string. The URL scheme is
   `mysql+pymysql`.
3. Writes the URL to a derived `Secret` named `<keystone.Name>-db-connection`
   in the Keystone namespace under the single data key `connection`. The
   derived Secret carries a controller owner reference to the Keystone CR so
   Kubernetes garbage collection deletes it when the Keystone CR is deleted.

**Important contracts:**

| Contract | Enforcement |
| --- | --- |
| Exactly one data key (`connection`) | `reconcileDBConnectionSecret` replaces `Data` wholesale on any drift (extra keys removed, stale values rewritten) |
| Owner reference to the Keystone CR | Set on create via `controllerutil.SetControllerReference`; re-enqueues on change via the existing `secretToKeystoneMapper` |
| Upstream Secret missing or missing `username`/`password` key | Sets `SecretsReady=False` with reason `WaitingForDBCredentials`, requeues after `RequeueSecretPolling`, does NOT create an invalid derived Secret |
| Password rotation | Upstream password change triggers an in-place `Update` of the derived Secret — `Name`/`UID` remain stable so Pod env consumers pick up the new value without restart churn |
| No ESO artefacts | The derived Secret is a plain `corev1.Secret`. No `ExternalSecret` or `PushSecret` is created for it — it is a pure materialization from the already-synced upstream Secret |

The derived Secret is listed as an owned resource in the
[Owned Resources](#owned-resources) table and triggers reconciliation through
the existing `Watches(Secret)` filter using `handler.OnlyControllerOwner()`.

### ConfigMap placeholder

`reconcileConfig` sets `[database] connection` in `keystone.conf` to the
package-level constant `dbConnectionPlaceholder = "mysql+pymysql://placeholder"`.
The placeholder is intentionally a syntactically valid pymysql URL so that
oslo.config can parse the file on startup before the environment override is
applied — parsing errors in the file layer would prevent the process from
reaching the override layer. All other keys in the `[database]` section
(`max_retries`, `connection_recycle_time`) are unchanged.

Tests guarantee that no credential byte (username, password, or their
percent-encoded forms) ever appears in the rendered `keystone.conf`. See
`TestReconcileConfig_BrownfieldDatabase_PlaceholderInsteadOfPassword`,
`TestReconcileConfig_ManagedDatabase_NoCredentialsInConfigMap`, and
`TestReconcileConfig_SpecialCharactersInCredentials_DoNotLeakToConfigMap` in
`reconcile_config_test.go`.

### `OS_<GROUP>__<OPTION>` environment-variable override

Upstream oslo.config supports sourcing any option value from an environment
variable whose name is derived from the option's group and key, overriding the
value present in any configuration file. The encoding is:

```text
OS_<GROUP>__<OPTION>
```

- `<GROUP>` — the INI section name (e.g. `database`), uppercased.
- `<OPTION>` — the INI key (e.g. `connection`), uppercased.
- The separator between group and option is a **double underscore** (`__`).

For Keystone's database URL this yields `OS_DATABASE__CONNECTION`. The
operator wires this env var on every container that loads `keystone.conf` and
needs database access, sourcing the value from the derived
`<name>-db-connection` Secret via a `SecretKeyRef`:

```yaml
env:
  - name: OS_DATABASE__CONNECTION
    valueFrom:
      secretKeyRef:
        name: <keystone.Name>-db-connection
        key: connection
```

The helper `buildDBConnectionEnvVar()` in `reconcile_deployment.go` constructs
this `corev1.EnvVar` value and is invoked from every pod-spec builder in the
operator. Containers that use the env var include:

| Workload | Builder | Purpose |
| --- | --- | --- |
| `Deployment` | `buildKeystoneDeployment` (`reconcile_deployment.go`) | Keystone API pods |
| `Job` `{name}-bootstrap` | `buildBootstrapJob` (`reconcile_bootstrap.go`) | Initial bootstrap of admin user/project/roles |
| `Job` `keystone-db-sync` and variants (expand, migrate, contract, schema-check) | `buildDBJob` (`reconcile_database.go`) | Database schema provisioning and drift checks |
| `CronJob` `{name}-trust-flush` | `trustFlushCronJob` (`reconcile_trustflush.go`) | Periodic expired trust cleanup |
| `CronJob` `{name}-fernet-rotate` | `fernetRotationCronJob` (`reconcile_fernet.go`) | Fernet key rotation — appended alongside the pre-existing `OS_fernet_tokens__max_active_keys` override |
| `CronJob` `{name}-credential-rotate` | `credentialRotationCronJob` (`reconcile_credential.go`) | Credential key rotation |

At process start, oslo.config loads `keystone.conf` (seeing the placeholder),
then walks its option registry for environment overrides. The
`OS_DATABASE__CONNECTION` value replaces the placeholder before any option
consumer (SQLAlchemy engine creation, migrations, etc.) reads it. This means
the password never reaches the file layer on any Pod's filesystem and is not
readable through the `get configmap` verb.

### References

- Upstream oslo.config change that introduced the
  `OS_<GROUP>__<OPTION>` environment override mechanism:
  [openstack/oslo.config change 585850](https://review.opendev.org/c/openstack/oslo.config/+/585850).
- Related sub-reconciler contracts: [`reconcileSecrets`](#reconcilesecrets)
  (gates DB credential readiness), [`reconcileConfig`](#reconcileconfig)
  (renders the placeholder), [`reconcileDeployment`](#reconciledeployment)
  (mounts the env var on Keystone API pods).

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

### ObservedGeneration Test Convention

Every sub-reconciler test file that sets conditions includes a dedicated
`TestReconcile{SubReconciler}_ConditionObservedGeneration` test function (CC-0072).
This test exercises at least one `True` and one `False` condition path with distinct,
non-default generation values to verify that `ObservedGeneration` is propagated on
every code path. When adding a new sub-reconciler, copy this test pattern from any
existing file (e.g. `reconcile_hpa_test.go`).

### Test Files

| File | Coverage |
| --- | --- |
| `keystone_controller_test.go` | Reconcile() orchestration, sequential execution, parallel group (CC-0071), early return, Ready aggregation, idempotency, benchmark, finalizer install/remove and termination branching + Events (CC-0078) |
| `reconcile_secrets_test.go` | DB/admin credential readiness, error propagation, condition messages, ObservedGeneration (CC-0072) |
| `reconcile_database_test.go` | Managed/brownfield modes, MariaDB CRs, db_sync lifecycle, stale Job detection, ObservedGeneration (CC-0072), finalizeDatabaseResources cleanup + idempotency (CC-0078) |
| `reconcile_fernet_test.go` | Key generation, Secret idempotency, script ConfigMap creation, CronJob schedule/volumes, PushSecret, key validity (CC-0073), ObservedGeneration (CC-0072) |
| `reconcile_credential_test.go` | Key generation, Secret idempotency, script ConfigMap creation, CronJob schedule/volumes, PushSecret, RBAC, key validity (CC-0036, CC-0073), ObservedGeneration (CC-0072) |
| `reconcile_networkpolicy_test.go` | NetworkPolicy creation, update, deletion, ingress rules, condition contract (CC-0039) |
| `reconcile_config_test.go` | INI generation, extraConfig merge, plugin config, policy overrides, ConfigMap hashing |
| `reconcile_policyvalidation_test.go` | Policy validation lifecycle, condition contract, error extraction, Job spec (CC-0058), ObservedGeneration |
| `reconcile_deployment_test.go` | Deployment spec, Service creation, readiness, endpoint, owner references, stable pod template (CC-0074), ObservedGeneration (CC-0072) |
| `reconcile_healthcheck_test.go` | Health check happy/unhealthy paths, timeout, DNS, connection refused, empty endpoint, response body close, HTTPDoer injection (CC-0067), ObservedGeneration |
| `reconcile_hpa_test.go` | HPA creation, update, deletion, metrics (CPU/memory), minReplicas defaulting, condition contract, error propagation (CC-0038), ObservedGeneration |
| `reconcile_httproute_test.go` | HTTPRoute creation, update, deletion, gateway namespace defaulting, PathPrefix match, backend Service port 5000, parent Accepted reflection, status.endpoint derivation, condition contract (CC-0065), ObservedGeneration |
| `reconcile_trustflush_test.go` | CronJob creation, deletion, schedule/suspend/args, security context, volume mounts, condition contract, error propagation (CC-0057), ObservedGeneration |
| `reconcile_bootstrap_test.go` | Job creation, completion, failure, stale detection, TTL/backoff, ObservedGeneration (CC-0072) |
| `integration_test.go` | Full reconciliation envtest: CronJob spec, bootstrap Job spec, brownfield mode, condition progression (CC-0015), ObservedGeneration (CC-0072, pre-existing) |

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
    │   ├── reconcile_credential.go             reconcileCredentialKeys sub-reconciler (CC-0036)
    │   ├── reconcile_networkpolicy.go          reconcileNetworkPolicy sub-reconciler (CC-0039)
    │   ├── reconcile_config.go                 reconcileConfig + pruneStaleConfigMaps (CC-0077)
    │   ├── reconcile_policyvalidation.go        reconcilePolicyValidation sub-reconciler (CC-0058)
    │   ├── reconcile_deployment.go             reconcileDeployment sub-reconciler
    │   ├── reconcile_healthcheck.go           reconcileHealthCheck sub-reconciler (CC-0067)
    │   ├── reconcile_hpa.go                   reconcileHPA sub-reconciler (CC-0038)
    │   ├── reconcile_httproute.go             reconcileHTTPRoute sub-reconciler + keystoneStatusEndpoint helper (CC-0065)
    │   ├── reconcile_trustflush.go            reconcileTrustFlush sub-reconciler (CC-0057)
    │   ├── reconcile_bootstrap.go              reconcileBootstrap sub-reconciler
    │   ├── scripts/
    │   │   ├── fernet_rotate.sh               Fernet key rotation script (CC-0073)
    │   │   └── credential_rotate.sh           Credential key rotation script (CC-0073)
    │   ├── keystone_controller_test.go         Orchestration tests
    │   ├── reconcile_secrets_test.go           Secrets tests
    │   ├── reconcile_database_test.go          Database tests
    │   ├── reconcile_fernet_test.go            Fernet tests (CC-0073)
    │   ├── reconcile_credential_test.go        Credential keys tests (CC-0036, CC-0073)
    │   ├── reconcile_networkpolicy_test.go     NetworkPolicy tests (CC-0039)
    │   ├── reconcile_config_test.go            Config tests
    │   ├── reconcile_policyvalidation_test.go   Policy validation tests (CC-0058)
    │   ├── reconcile_deployment_test.go        Deployment tests
    │   ├── reconcile_healthcheck_test.go      Health check tests (CC-0067)
    │   ├── reconcile_hpa_test.go              HPA tests (CC-0038)
    │   ├── reconcile_httproute_test.go        HTTPRoute tests (CC-0065)
    │   ├── reconcile_trustflush_test.go       Trust flush tests (CC-0057)
    │   ├── reconcile_bootstrap_test.go         Bootstrap tests
    │   └── integration_test.go                 Envtest integration tests (CC-0015)
    └── testutil/
        └── envtest_setup.go                    Keystone-specific envtest helper
```
