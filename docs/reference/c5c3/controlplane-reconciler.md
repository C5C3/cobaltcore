---
title: ControlPlane Reconciler Architecture
quadrant: operator
---

# ControlPlane Reconciler Architecture

Reference documentation for the `ControlPlaneReconciler`, the
`CredentialRotationReconciler`, and their sub-reconciler contracts.
The `ControlPlaneReconciler` implements the control loop that drives a
`ControlPlane` CR from desired state to a fully operational Keystone control
plane: backing infrastructure, the projected Keystone service, the K-ORC admin
application credential, and the OpenStack service-catalog entries.

For CRD type definitions and webhooks, see
[ControlPlane CRD API Reference](./controlplane-crd.md). For the shared
controller-manager bootstrap pattern (`internal/common/bootstrap`) the c5c3
operator reuses verbatim, see the
[Keystone Reconciler — Controller Registration](../keystone/keystone-reconciler.md#controller-registration)
section. For the library functions used by the sub-reconcilers, see
[Kubernetes-Interacting Packages](../backend/kubernetes-packages.md). For the
infrastructure stack (MariaDB, Memcached, K-ORC, OpenBao) the operator targets,
see [Infrastructure Manifests](../infrastructure/infrastructure-manifests.md).

The c5c3 operator is intentionally a *thin orchestrator*: it provisions and
owns child CRs (MariaDB, Memcached, Keystone, K-ORC `ApplicationCredential` /
`Service` / `Endpoint`) and aggregates their readiness. It does **not**
re-implement the per-service logic those child operators already own. As a
consequence the c5c3 API surface is deliberately smaller than the
[Keystone reconciler](../keystone/keystone-reconciler.md)'s: there is no
finalizer, no parallel sub-reconciler group, and no per-CR metric cardinality.

## Controller Registration

The c5c3 operator registers **two** reconcilers and an optional webhook with the
controller manager in `operators/c5c3/main.go` via the shared bootstrap package
(`github.com/c5c3/forge/internal/common/bootstrap`). The bootstrap helper builds
the manager, wires leader election, and invokes the operator's `SetupFunc`; the
same pattern is documented in detail under
[Keystone Reconciler — Controller Registration](../keystone/keystone-reconciler.md#controller-registration).

```go
import (
    "github.com/c5c3/forge/internal/common/bootstrap"
    c5c3v1alpha1 "github.com/c5c3/forge/operators/c5c3/api/v1alpha1"
    "github.com/c5c3/forge/operators/c5c3/internal/controller"
)

const leaderElectionID = "c5c3.openstack.c5c3.io"

bootstrap.Run(bootstrap.ManagerConfig{
    Scheme:           scheme,
    LeaderElectionID: leaderElectionID,
    SetupFunc: func(mgr ctrl.Manager, webhooks bool) error {
        if err := (&controller.ControlPlaneReconciler{
            Client:   mgr.GetClient(),
            Scheme:   mgr.GetScheme(),
            Recorder: mgr.GetEventRecorderFor("controlplane-controller"),
        }).SetupWithManager(mgr); err != nil {
            return err
        }
        if err := (&controller.CredentialRotationReconciler{
            Client:   mgr.GetClient(),
            Scheme:   mgr.GetScheme(),
            Recorder: mgr.GetEventRecorderFor("credentialrotation-controller"),
        }).SetupWithManager(mgr); err != nil {
            return err
        }
        if webhooks {
            return (&c5c3v1alpha1.ControlPlaneWebhook{Client: mgr.GetClient()}).
                SetupWebhookWithManager(mgr)
        }
        return nil
    },
})
```

| Element | Value |
| --- | --- |
| `LeaderElectionID` | `c5c3.openstack.c5c3.io` (a package-level constant in `main.go`; referenced by the deploy-stack RBAC and asserted by `main_test.go` so a rename cannot silently break leader election) |
| Primary reconciler | `ControlPlaneReconciler` (event recorder `controlplane-controller`) |
| Secondary reconciler | `CredentialRotationReconciler` (event recorder `credentialrotation-controller`) |
| Webhook | `ControlPlaneWebhook`, registered **only** when `bootstrap.Run` passes `webhooks == true` to `SetupFunc` (the bool is resolved once by the bootstrap layer from the manager environment) |

### Scheme Registration

The operator registers these schemes in `main.go`'s `init()` so the reconcilers
can interact with the typed child CRDs:

| Module | Scheme | Types Used |
| --- | --- | --- |
| `k8s.io/client-go/kubernetes/scheme` | `clientgoscheme.AddToScheme` | core Kubernetes types (`Secret`, `Event`) |
| `github.com/c5c3/forge/operators/c5c3/api/v1alpha1` | `c5c3v1alpha1.AddToScheme` | `ControlPlane`, `CredentialRotation`, `SecretAggregate` (own API) |
| `github.com/c5c3/forge/operators/keystone/api/v1alpha1` | `keystonev1alpha1.AddToScheme` | `Keystone` (projected and owned child) |
| `github.com/mariadb-operator/mariadb-operator` | `mariadbv1alpha1.AddToScheme` | `MariaDB` (projected and owned child) |
| `github.com/external-secrets/external-secrets` | `esov1alpha1.SchemeBuilder` | `PushSecret` (admin-credential mirror) |
| `github.com/external-secrets/external-secrets` | `esov1.SchemeBuilder` | `ExternalSecret`, `ClusterSecretStore` (K-ORC clouds.yaml gate) |
| `github.com/k-orc/openstack-resource-controller/v2` | `orcv1alpha1.AddToScheme` | `ApplicationCredential`, `Service`, `Endpoint` |

> **Note (Memcached is unstructured):** `memcached.c5c3.io` ships **no Go
> module**, so the `Memcached` child is **deliberately not** registered in the
> scheme. `reconcileInfrastructure` builds and applies it as an
> `*unstructured.Unstructured` carrying the shared `memcachedGVK`
> (`memcached.c5c3.io/v1beta1`, kind `Memcached`), and `SetupWithManager`
> `Owns` the same unstructured GVK. The GVK is resolved against the cluster
> `RESTMapper` at runtime, so no scheme entry is required.

### Watches

The controller watches the primary `ControlPlane` CR, every child CR the
sub-reconcilers project, and the admin-password `Secret`:

| Resource | Watch Type | Effect |
| --- | --- | --- |
| `ControlPlane` | `For()` | Triggers reconciliation on CR changes |
| `MariaDB` | `Owns()` | Re-reconciles the owning ControlPlane when the managed MariaDB child status changes |
| `Memcached` (unstructured `memcachedGVK`) | `Owns()` | Re-reconciles when the managed Memcached child status changes; owned as `*unstructured.Unstructured` because the kind has no Go module |
| `Keystone` | `Owns()` | Re-reconciles when the projected Keystone child status changes |
| K-ORC `ApplicationCredential` | `Owns()` | Re-reconciles when the minted admin credential's `Available` condition or `status.id` changes |
| K-ORC `Service` | `Owns()` | Re-reconciles when the identity catalog Service changes |
| K-ORC `Endpoint` | `Owns()` | Re-reconciles when the public identity Endpoint changes |
| `Secret` | `Watches()` | Maps Secret events to referencing ControlPlane CRs via the `ControlPlaneSecretNameIndexKey` field indexer (`secretToControlPlaneMapper`) |

The `Secret` watch uses `Watches()` with a `MapFunc` rather than `Owns()`
because the admin-password Secret
(`spec.korc.adminCredential.passwordSecretRef`) is typically **ESO-managed** —
it is owned by the ExternalSecret controller, not by the ControlPlane CR — so an
owner-reference filter would never match it. The index-backed namespace List is
exactly what wakes the ControlPlane when its admin password rotates, so the
re-mint chain (see [K-ORC admin credential chain](#k-orc-admin-credential-chain))
converges on watch delivery instead of waiting for the next periodic requeue.

#### Secret Field Indexer

The controller registers a controller-runtime field indexer on the
`ControlPlane` kind so that a Secret event resolves to the referencing
ControlPlane CR(s) via an O(1) cache lookup instead of an unfiltered
namespace-scoped List, mirroring the keystone operator's
`KeystoneSecretNameIndexKey`.

| Aspect | Value |
| --- | --- |
| Index key | `ControlPlaneSecretNameIndexKey = "spec.korc.adminCredential.passwordSecretRef.name"` (exported package-level constant in `operators/c5c3/internal/controller/controlplane_controller.go`) |
| Indexed fields | `spec.korc.adminCredential.passwordSecretRef.name` — currently the only Secret a ControlPlane references. The extractor (`controlPlaneSecretNameExtractor`) returns an empty slice when the name is unset so an unset field does not pollute the index, and returns `nil` if invoked with the wrong type rather than panicking. |
| Registration site | `SetupWithManager` → `registerControlPlaneSecretNameIndex(ctx, mgr.GetFieldIndexer())`, invoked **before** the `Watches(Secret, …)` chain. Any error from `IndexField` is wrapped with the index key and propagated, so manager startup aborts loudly if registration fails. |
| Lookup site | `secretToControlPlaneMapper(mgr.GetClient())` — performs a namespace-scoped `client.List` with `client.MatchingFields{ControlPlaneSecretNameIndexKey: secret.Name}`. On List error the error is logged via `log.FromContext` and the mapper returns `nil` per the `handler.MapFunc` contract (it must not return errors). |
| Result | Each matching ControlPlane in the Secret's namespace is enqueued as a `reconcile.Request`; an event matching no ControlPlane returns `nil`. |

> **Why no owner-ref fallback?** Unlike the keystone operator, the c5c3 Secret
> mapper has a **pure index-backed** lookup with no owner-reference fallback
> branch — the ControlPlane projects no rotation-staging Secrets that are
> owned-but-unreferenced, so the union/owner-ref complexity of
> `secretToKeystoneMapper` is not needed here.

---

## Reconciler Struct

```go
type ControlPlaneReconciler struct {
    client.Client
    Scheme   *runtime.Scheme
    Recorder record.EventRecorder
}
```

| Field | Type | Purpose |
| --- | --- | --- |
| `Client` | `client.Client` | Kubernetes API client for CRUD operations (embedded) |
| `Scheme` | `*runtime.Scheme` | Runtime scheme for owner-reference resolution |
| `Recorder` | `record.EventRecorder` | Records Kubernetes events for state transitions |

The `CredentialRotationReconciler` has the identical three-field shape
(`client.Client` embedded, `Scheme`, `Recorder`).

---

## RBAC Permissions

RBAC markers on the two reconcilers generate the required ClusterRole. The
`ControlPlaneReconciler` markers (in `controlplane_controller.go`):

| API Group | Resources | Verbs |
| --- | --- | --- |
| `c5c3.io` | `controlplanes` | get, list, watch, create, update, patch, delete |
| `c5c3.io` | `controlplanes/status` | get, update, patch |
| `c5c3.io` | `controlplanes/finalizers` | update |
| `c5c3.io` | `credentialrotations` | get, list, watch, create, update, patch, delete |
| `c5c3.io` | `credentialrotations/status` | get, update, patch |
| `c5c3.io` | `secretaggregates` | get, list, watch |
| `k8s.mariadb.com` | `mariadbs` | get, list, watch, create, update, patch, delete |
| `memcached.c5c3.io` | `memcacheds` | get, list, watch, create, update, patch, delete |
| `keystone.openstack.c5c3.io` | `keystones` | get, list, watch, create, update, patch, delete |
| `openstack.k-orc.cloud` | `applicationcredentials`, `services`, `endpoints` | get, list, watch, create, update, patch, delete |
| `external-secrets.io` | `externalsecrets`, `pushsecrets` | get, list, watch, create, update, patch, delete |
| `external-secrets.io` | `clustersecretstores` | get, list, watch |
| `core` | `secrets` | get, list, watch, create, update, patch, delete |
| `core` | `events` | create, patch |

The `CredentialRotationReconciler` markers (in
`reconcile_credentialrotation.go`) are scoped tighter — it never mints, so it
holds only `update`/`patch` (not `create`/`delete`) on K-ORC
`applicationcredentials` and read-only access to `controlplanes`:

| API Group | Resources | Verbs |
| --- | --- | --- |
| `c5c3.io` | `credentialrotations` | get, list, watch, create, update, patch, delete |
| `c5c3.io` | `credentialrotations/status` | get, update, patch |
| `c5c3.io` | `controlplanes` | get, list, watch |
| `openstack.k-orc.cloud` | `applicationcredentials` | get, list, watch, update, patch |
| `core` | `secrets` | get, list, watch |
| `core` | `events` | create, patch |

---

## Reconciliation Flow

```text
┌──────────────────────────────────────────────────────────────────────────────┐
│                    CONTROLPLANE RECONCILIATION FLOW                          │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ControlPlane CR changed (or requeue timer fires)                            │
│         │                                                                    │
│         ▼                                                                    │
│  Fetch ControlPlane CR (return empty result if NotFound)                     │
│         │                                                                    │
│         ▼                                                                    │
│  ┌──────────────────────────┐                                                │
│  │ reconcileInfrastructure  │  Ensure managed MariaDB + Memcached children   │
│  │  (gate: none)            │  Sets: InfrastructureReady                     │
│  └────────┬─────────────────┘  Requeue: 15s while a child is not Ready       │
│           │  early-return if !result.IsZero() || err                         │
│           ▼                                                                  │
│  ┌──────────────────────────┐                                                │
│  │ reconcileDBCredentials   │  Project per-CP DB-credential ExternalSecret   │
│  │  (gate: none)            │  Sets: DBCredentialsReady                      │
│  └────────┬─────────────────┘  Requeue: 10s while the ES is not yet synced   │
│           │                                                                  │
│           ▼                                                                  │
│  ┌──────────────────────────┐                                                │
│  │ reconcileAdminPassword   │  Project per-CP admin-password ExternalSecret  │
│  │  (gate: none)            │  Sets: AdminPasswordReady                      │
│  └────────┬─────────────────┘  Requeue: 10s while the ES is not yet synced   │
│           │                                                                  │
│           ▼                                                                  │
│  ┌──────────────────────────┐                                                │
│  │ reconcileKeystone        │  Project the Keystone child CR                 │
│  │  (gate: InfraReady)      │  Sets: KeystoneReady                           │
│  └────────┬─────────────────┘  Requeue: 5s gated / 15s child not Ready       │
│           │                                                                  │
│           ▼                                                                  │
│  ┌──────────────────────────┐                                                │
│  │ reconcileKORC            │  Mint the admin ApplicationCredential          │
│  │  (gate: none*)           │  Sets: KORCReady                               │
│  └────────┬─────────────────┘  Requeue: 10s while AC not Available           │
│           │                                                                  │
│           ▼                                                                  │
│  ┌──────────────────────────┐                                                │
│  │ reconcileAdminCredential │  Commit minted Secret + PushSecret to OpenBao  │
│  │  (gate: KORCReady)       │  Sets: AdminCredentialReady                    │
│  └────────┬─────────────────┘  Requeue: 10s gated / clouds.yaml not Ready    │
│           │                                                                  │
│           ▼                                                                  │
│  ┌──────────────────────────┐                                                │
│  │ reconcileCatalog         │  Register identity Service + public Endpoint   │
│  │  (gate: AdminCredReady)  │  Sets: CatalogReady                            │
│  └────────┬─────────────────┘  Requeue: 10s while gated / CRD missing        │
│           │                                                                  │
│           ▼                                                                  │
│  setReadyCondition()  — aggregate Ready = AllTrue(subConditionTypes)         │
│  updateStatus()       — stamp status.observedGeneration, persist             │
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘

  * reconcileKORC has no condition gate, but it defers (KORCReady=False,
    requeue) until the admin-password Secret can be read.
```

### Execution Model

All seven sub-reconcilers run **strictly sequentially** — there is no parallel
group. Each sub-reconciler call is wrapped in `instrumentSubReconciler` (see
[Metrics Instrumentation](#metrics-instrumentation)) and follows the same
early-return contract:

```go
if result, err := instrumentSubReconciler(ctx, "Infrastructure", func(ctx context.Context) (ctrl.Result, error) {
    return r.reconcileInfrastructure(ctx, &cp)
}); !result.IsZero() || err != nil {
    return r.updateStatus(ctx, &cp, result, err)
}
```

This guarantees:

1. A sub-reconciler error **propagates immediately** — subsequent sub-reconcilers
   are skipped.
2. A non-zero result (`RequeueAfter > 0`) causes an **early return** — status is
   persisted and the reconciler exits.
3. Status conditions from the failing/requeuing sub-reconciler are **always
   persisted** via `updateStatus()` before returning.

Only when all seven sub-reconcilers return a zero result with no error does
control reach `setReadyCondition(&cp)` and the final `updateStatus(ctx, &cp,
ctrl.Result{}, nil)`.

### Status Update Pattern

`updateStatus()` stamps `cp.Status.ObservedGeneration = cp.Generation`, persists
all condition changes via `r.Status().Update()`, and returns the provided
`(result, error)` pair. When both a reconcile error and the status update fail,
both errors are preserved via `errors.Join` so the original reconcile failure
remains visible in controller-runtime logs:

| reconcileErr | `Status().Update()` | Returned error |
| --- | --- | --- |
| nil | succeeds | nil |
| non-nil | succeeds | reconcileErr (unchanged) |
| nil | fails | `errors.Join(nil, fmt.Errorf("updating status: %w", statusErr))` |
| non-nil | fails | `errors.Join(reconcileErr, fmt.Errorf("updating status: %w", statusErr))` |

Because `ObservedGeneration` is stamped on **every** `updateStatus` call (early
return or final), a stale status is always distinguishable from a current one.

### Ready Condition Aggregation

After all sub-reconcilers succeed, `setReadyCondition()` evaluates whether every
sub-condition type is `True` using `aggregateReady()`, which delegates to
`conditions.AllTrue(conds, subConditionTypes...)`:

| All Sub-Conditions True | Ready Condition | Reason | Message |
| --- | --- | --- | --- |
| Yes | `Status: True` | `AllReady` | `All sub-conditions are ready` |
| No (any missing or False) | `Status: False` | `NotAllReady` | `One or more sub-conditions are not ready` |

The seven aggregated sub-condition types (the source-of-truth `subConditionTypes`
slice in `controlplane_controller.go`) are:

```text
InfrastructureReady, DBCredentialsReady, KeystoneReady, KORCReady, AdminCredentialReady, AdminPasswordReady, CatalogReady
```

The `Ready` condition carries `ObservedGeneration = cp.Generation` so clients can
detect a stale aggregate.

---

## Sub-Reconciler Contracts

Each sub-reconciler owns exactly one Ready sub-condition. The tables below give
each one's gate, what it projects/owns, and the condition reasons it sets on the
`True`, requeue, and error paths. All condition constants are the exported
source-of-truth strings in `controlplane_controller.go`; sub-reconcilers
reference the constants (never inline literals) so a rename is a compile error
and is caught by the no-inline-literals drift guard.

Every condition is stamped with `ObservedGeneration = cp.Generation` on every
path.

### reconcileInfrastructure

| Aspect | Value |
| --- | --- |
| File | `reconcile_infrastructure.go` |
| Condition | `InfrastructureReady` |
| Gate | none (runs first) |
| Projects / Owns | Managed-mode `MariaDB` (`k8s.mariadb.com`) and `Memcached` (unstructured `memcached.c5c3.io/v1beta1`) children, each named after its `clusterRef` and created in `childNamespace(cp)` |
| Requeue | `infraRequeueAfter` = **15s** while a managed child is not yet Ready |

`reconcileInfrastructure` provisions the shared backing services declared in
`spec.infrastructure`. A backing service is **managed** when its `clusterRef` is
set and **brownfield** (provisions nothing) when `host`/`servers` are set
instead. Both managed children are ensured in a single pass *before* readiness is
gated, so a half-provisioned control plane (DB created but cache missing) never
occurs; readiness is evaluated collectively afterwards.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| MariaDB create/update fails | False | `MariaDBError` | returns the error (controller-runtime backoff) |
| Memcached create/update fails | False | `MemcachedError` | returns the error |
| MariaDB not yet Ready | False | `WaitingForDatabase` | requeue 15s |
| Memcached not yet Ready | False | `WaitingForCache` | requeue 15s |
| All managed children Ready (or pure brownfield) | True | `InfrastructureReady` | — |

> The managed MariaDB child is provisioned with a minimal-but-valid spec —
> `replicas: 3`, `galera.enabled: true`, `storage.size: 100Gi`
> (`infraMariaDBReplicas` / `infraMariaDBStorageSize`) — mirroring the production
> baseline; the mariadb-operator webhook rejects a CR without a storage size.
> The Memcached child's `spec.replicas` is taken from
> `spec.infrastructure.cache.replicas` (widened to `int64` for unstructured
> nested-field storage). MariaDB readiness is read via
> `conditions.IsReady(mariadb.Status.Conditions)`; Memcached readiness is read
> from the unstructured `status.conditions[type=Ready].status == "True"`
> (`unstructuredReady`), where a missing/malformed list is treated as not-ready
> rather than an error.

### reconcileDBCredentials

| Aspect | Value |
| --- | --- |
| File | `reconcile_dbcredentials.go` |
| Condition | `DBCredentialsReady` |
| Gate | none — runs unconditionally, positioned after Infrastructure and before Keystone so the Keystone CR is never projected before the DB-credential Secret exists |
| Projects / Owns | Managed-mode (`spec.infrastructure.database.clusterRef != nil`) one owner-referenced `external-secrets.io/v1` `ExternalSecret` named `{controlplane.Name}-keystone-db-credentials` (`dbCredentialSecretName`) in `childNamespace(cp)`; brownfield projects nothing |
| Requeue | `dbCredentialsRequeueAfter` = **10s** while the ExternalSecret is not yet Ready |

`reconcileDBCredentials` projects the per-ControlPlane service database
credential as an OpenBao-backed `ExternalSecret`, so the projected Keystone CR
consumes a DB credential scoped to its own ControlPlane. It mirrors
`reconcileAdminCredential`'s wait/condition
handling. The database is **managed** when `spec.infrastructure.database.clusterRef`
is set and **brownfield** when the user supplies a `host`-based connection:

- **Brownfield is a pure no-op.** When `clusterRef == nil` the user owns the DB
  credential Secret out-of-band, so the operator projects **no** ExternalSecret
  and never references OpenBao or the `ClusterSecretStore`; `DBCredentialsReady`
  is reported `True` immediately so the chain proceeds to Keystone.
- **Managed projects the ExternalSecret.** The owned ExternalSecret has
  `RefreshInterval` 1h, `SecretStoreRef` `Kind: ClusterSecretStore, Name: openbao-cluster-store`,
  and `Target.CreationPolicy: Owner` (so ESO owns the materialised Secret of the
  same name). Its `username` / `password` `Data` keys both read from the per-CP
  remote key `openstack/keystone/{cp.Namespace}/{cp.Name}/db`
  (`dbCredentialRemoteKeyFor`) via the matching `Property`. The builder
  `dbCredentialExternalSecret(cp)` sets **no** owner reference; the reconciler
  sets the ControlPlane controller reference inside the `CreateOrUpdate` mutate
  closure for GC.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| Brownfield (`clusterRef == nil`) | True | `BrownfieldUserSuppliedCredential` | no ExternalSecret projected; user supplies the DB credential Secret out-of-band |
| ExternalSecret create/update or read fails | False | `ExternalSecretError` | returns the error |
| ExternalSecret not yet synced | False | `WaitingForDBCredentialSecret` | requeue 10s |
| ExternalSecret Ready | True | `DBCredentialsReady` | — |

### reconcileAdminPassword

| Aspect | Value |
| --- | --- |
| File | `reconcile_adminpassword.go` |
| Condition | `AdminPasswordReady` |
| Gate | none — runs unconditionally, positioned after DBCredentials and before Keystone so the Keystone CR is never projected before the admin-password Secret exists |
| Projects / Owns | Managed-mode (`spec.infrastructure.database.clusterRef != nil`) one owner-referenced `external-secrets.io/v1` `ExternalSecret` named `{controlplane.Name}-keystone-admin-credentials` (`adminPasswordSecretName`) in `childNamespace(cp)`; brownfield projects nothing |
| Requeue | `adminPasswordRequeueAfter` = **10s** while the ExternalSecret is not yet Ready |

`reconcileAdminPassword` projects the per-ControlPlane Keystone admin password
as an OpenBao-backed `ExternalSecret`, so the projected Keystone CR's bootstrap
admin-password ref consumes a credential scoped to its own ControlPlane. It
mirrors `reconcileDBCredentials`'s wait/condition handling. The database is
**managed** when `spec.infrastructure.database.clusterRef` is set and
**brownfield** when the user supplies a `host`-based connection:

- **Brownfield is a pure no-op.** When `clusterRef == nil` the user owns the
  admin-password Secret out-of-band, so the operator projects **no**
  ExternalSecret and never references OpenBao or the `ClusterSecretStore`;
  `AdminPasswordReady` is reported `True` immediately so the chain proceeds to
  Keystone.
- **Managed projects the ExternalSecret.** The owned ExternalSecret has
  `RefreshInterval` 1h, `SecretStoreRef` `Kind: ClusterSecretStore, Name: openbao-cluster-store`,
  and `Target.CreationPolicy: Owner` (so ESO owns the materialised Secret of the
  same name). Its single `password` `Data` key reads from the per-CP remote key
  `bootstrap/{cp.Namespace}/{cp.Name}-keystone/admin`
  (`adminPasswordRemoteKeyFor`) with `Property: password`. Unlike the
  DB-credential path this key is **Keystone-name-scoped**
  (`{cp.Name}-keystone`, not `{cp.Name}`) so it matches the bootstrap seeder and
  the keystone-operator's Model-B rotation `PushSecret` at
  `bootstrap/{keystone.Namespace}/{keystone.Name}/admin`; the
  `{namespace}/{keystone-name}` scoping still keeps two ControlPlanes from
  colliding on the cluster-global OpenBao backend. The builder
  `adminPasswordExternalSecret(cp)` sets **no** owner reference; the reconciler
  sets the ControlPlane controller reference inside the `CreateOrUpdate` mutate
  closure for GC.

The managed-mode effective admin-password ref (`effectiveAdminPasswordSecretRef`)
points the projected Keystone child's `spec.bootstrap.adminPasswordSecretRef` at
this materialised Secret's `password` key
(`{controlplane.Name}-keystone-admin-credentials`); in brownfield mode it stays
the user-declared `spec.korc.adminCredential.passwordSecretRef` verbatim. The
cp-level spec default for `passwordSecretRef` remains `keystone-admin`.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| Brownfield (`clusterRef == nil`) | True | `BrownfieldUserSuppliedCredential` | no ExternalSecret projected; user supplies the admin-password Secret out-of-band |
| ExternalSecret create/update or read fails | False | `ExternalSecretError` | returns the error |
| ExternalSecret not yet synced | False | `WaitingForAdminPasswordSecret` | requeue 10s |
| ExternalSecret Ready | True | `AdminPasswordReady` | — |

### reconcileKeystone

| Aspect | Value |
| --- | --- |
| File | `reconcile_keystone.go` |
| Condition | `KeystoneReady` |
| Gate | `InfrastructureReady == True` |
| Projects / Owns | one `Keystone` child named `{controlplane.Name}-keystone` (`keystoneNameSuffix`) in `childNamespace(cp)` |
| Requeue | `keystoneInfraGateRequeueAfter` = **5s** while gated; `infraRequeueAfter` = **15s** while the child is not Ready |

`reconcileKeystone` projects `spec.services.keystone` into an owned `Keystone`
CR. The projection is deliberately *thin* — it reuses the ControlPlane's own
infrastructure specs verbatim so Keystone points at the same backing services
the ControlPlane provisioned:

- **Image:** repository defaults to `ghcr.io/c5c3/keystone` with the tag derived
  from `spec.openStackRelease`; `spec.services.keystone.image` overrides the
  whole image reference when set.
- **Database / Cache:** `keystone.Spec.Database = cp.Spec.Infrastructure.Database`
  and `keystone.Spec.Cache = cp.Spec.Infrastructure.Cache` (the same `clusterRef`s,
  reused unchanged).
- **Bootstrap:** the admin-password Secret ref is the effective ref
  (`effectiveAdminPasswordSecretRef`) — in managed mode the operator-projected
  per-CP Secret `{controlplane.Name}-keystone-admin-credentials` (see
  [reconcileAdminPassword](#reconcileadminpassword)), in brownfield mode the
  user-declared `cp.Spec.KORC.AdminCredential.PasswordSecretRef` verbatim (so
  Keystone and K-ORC agree on the admin-password source) — and the region is
  `cp.Spec.Region`.
- **Replicas:** copied from `spec.services.keystone.replicas` when set.
- **Policy:** `projectPolicyOverrides(cp.Spec.Global, cp.Spec.Services.Keystone.PolicyOverrides)`
  merges the global base with per-service overrides (per-service wins on
  conflict).
- **Rotation:** when `spec.services.keystone.rotationInterval` is set,
  `intervalToCron` converts it to a cron schedule applied to **both**
  `Fernet.RotationSchedule` and `CredentialKeys.RotationSchedule`. Only `168h`
  (weekly, `0 0 * * 0`) and positive whole-day multiples (daily, `0 0 * * *`)
  are supported.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `InfrastructureReady` not True | False | `WaitingForInfrastructure` | requeue 5s; no Keystone CR is created while infra is unready |
| Invalid `rotationInterval` | False | `InvalidRotationInterval` | **no requeue, no error** — a bad interval surfaces a clean condition rather than a partial apply or backoff loop |
| Keystone create/update fails | False | `KeystoneError` | returns the error |
| Keystone child not yet Ready | False | `WaitingForKeystone` | requeue 15s |
| Keystone child Ready | True | `KeystoneReady` | — |

### reconcileKORC

| Aspect | Value |
| --- | --- |
| File | `reconcile_korc.go` |
| Condition | `KORCReady` |
| Gate | none (but defers until the admin-password Secret is readable) |
| Projects / Owns | one K-ORC `ApplicationCredential` named `{controlplane.Name}-admin-app-credential` and the password-based clouds.yaml Secret `{controlplane.Name}-admin-password-cloud`, both in `childNamespace(cp)` |
| Requeue | `korcRequeueAfter` = **10s** while deferring, while the CRD is missing, while a re-mint is in progress, or while the AC is not yet Available |

`reconcileKORC` create-or-updates an **owned** K-ORC `ApplicationCredential` CR
that instructs K-ORC to mint the admin application credential, and drives re-mint. Key behaviours:

- **Restricted → Unrestricted inversion (CRITICAL).** Our
  `ApplicationCredentialSpec.restricted` is the inverse of K-ORC's
  `spec.resource.unrestricted`: `restricted=true ⇒ Unrestricted=false`
  (`ptr.To(!restricted)`). `restricted` defaults to `true` (least-privilege)
  when unset, matching the defaulting webhook.
- **Password-cloud (breaks the self-referential deadlock).** The AC authenticates
  via an operator-owned, password-based clouds.yaml Secret
  `{controlplane.Name}-admin-password-cloud` (`ensureAdminPasswordCloud`), **not**
  `k-orc-clouds-yaml`. That matters because `k-orc-clouds-yaml` *is* the minted
  application credential itself, so deleting the AC to re-mint would invalidate the
  very clouds.yaml needed to re-authenticate; a restricted application credential
  also cannot mint a new application credential. The password-cloud is re-rendered
  from the live admin password on every pass (so a rotation flows through to it)
  and is not churned when the password is unchanged. The Domain/User imports and
  the catalog Service/Endpoint keep using the spec's `CloudCredentialsRef`
  (`k-orc-clouds-yaml`) and tolerate the brief auth gap during a re-mint by
  requeueing.
- **UserRef.** The required K-ORC `UserRef` is derived conventionally from the
  admin `cloudName` (defaulting to `"admin"`), assuming a sibling K-ORC `User` CR
  of that name (imported as unmanaged by `ensureKORCAdminImports`).
- **Access rules.** `projectAccessRules` maps our `{service, method, path}` list
  onto K-ORC's rule shape: `service` becomes a `serviceRef` (Kubernetes name ref
  to an ORC `Service` CR, e.g. `identity`), `method` becomes the typed
  `HTTPMethod` enum, and `path` becomes a string pointer.
- **Re-mint trigger (delete + recreate).** K-ORC's AC actuator implements only
  Create + Delete, so a rotated admin password cannot re-mint in place. The SHA-256
  of the admin password is stamped onto the AC CR under the
  `forge.c5c3.io/admin-password-hash` annotation (`adminPasswordHashAnnotation`);
  on a later pass a mismatch (the hash moved, or the CredentialRotation reconciler
  zeroed the annotation to nudge) drives `reconcileKORC` to **delete** the AC — the
  finalizer revokes the old Keystone credential, authenticating via the
  password-cloud — and **regenerate** the secret `value`, so the next pass recreates
  the AC for a fresh mint. The hash is computed by the package-level
  `computeAdminPasswordHash`, shared with the CredentialRotation reconciler so both
  agree on one derivation.
- **Re-mint progress / stall.** While the old AC is `Terminating` the condition is
  `KORCReady=False/ReMinting`; if it stays terminating longer than
  `remintStallTimeout` (**5m**) — a finalizer K-ORC cannot clear, e.g. it cannot
  reach Keystone to revoke — it escalates to `KORCReady=False/ReMintStalled`.
- **Status reflection.** `updateAdminApplicationCredentialStatus` reflects the
  observed AC into `cp.Status.AdminApplicationCredential` (`ID`, the inverted
  `Restricted`, and a `LastRotation` re-stamped whenever the credential ID changes —
  i.e. advanced by a completed re-mint).
- **Missing-CRD safety.** If the K-ORC CRD is absent the apiserver/RESTMapper
  returns a no-match error, detected via `meta.IsNoMatchError` and surfaced as a
  clean condition **without** crash-looping the operator.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| Admin password Secret/key missing | False | `WaitingForAdminPassword` | requeue 10s (via `secrets.IsMissingSecretOrKey`) |
| Admin password read fails otherwise | False | `AdminPasswordError` | returns the error |
| Password-cloud ensure fails | False | `PasswordCloudError` | returns the error |
| K-ORC AC CRD not installed | False | `KORCCRDNotInstalled` | requeue 10s (no hard error) |
| Hash mismatch → AC deleted for re-mint | False | `ReMinting` | requeue 10s; AC deleted + `value` regenerated, recreated next pass |
| Re-mint stuck `Terminating` past `remintStallTimeout` (5m) | False | `ReMintStalled` | requeue 10s; finalizer cannot revoke the old credential |
| AC create/update/delete/read fails otherwise | False | `ApplicationCredentialError` | returns the error |
| `value` regeneration fails | False | `SecretError` | returns the error |
| AC not yet `Available` | False | `WaitingForApplicationCredential` | requeue 10s; gated on `orcv1alpha1.IsAvailable(ac)` (K-ORC uses `Available`, not `Ready`) |
| AC minted and Available | True | `ApplicationCredentialMinted` | — |

### reconcileAdminCredential

| Aspect | Value |
| --- | --- |
| File | `reconcile_korc.go` |
| Condition | `AdminCredentialReady` |
| Gate | `KORCReady == True` **and** the K-ORC clouds.yaml `ExternalSecret` (`{childNamespace(cp)}/{CloudCredentialsRef.SecretName}`, co-located with the K-ORC CRs per C1) is Ready |
| Owns | the operator-owned `Secret` `{controlplane.Name}-admin-app-credential` and the `PushSecret` `{controlplane.Name}-admin-app-credential-backup`, both in `childNamespace(cp)` |
| Requeue | `korcRequeueAfter` = **10s** while either gate is unmet |

`reconcileAdminCredential` commits the minted credential and mirrors it to
OpenBao:

- **Clobber-safe operator Secret.** The Secret K-ORC writes the minted
  credential into is ensured by the operator, but the `CreateOrUpdate` mutate
  closure **never touches `secret.Data`** — only the owner reference. K-ORC owns
  the data, so a reconcile can never overwrite a freshly minted credential.
- **clouds.yaml gate.** Readiness is checked via
  `secrets.WaitForExternalSecret(childNamespace(cp)/CloudCredentialsRef.SecretName)`
  so the credential is never published before K-ORC can actually authenticate.
  The Secret is co-located with the K-ORC CRs (C1) because K-ORC resolves
  `CloudCredentialsRef` in the resource's own namespace; on a fresh cluster
  `reconcileKORC` itself seeds a password-based bootstrap clouds.yaml into the
  `{controlplane.Name}-admin-app-credential` Secret (`seedBootstrapCloudsYAML`,
  write-if-empty) and the PushSecret mirrors it to the per-ControlPlane
  OpenBao path, so the operator-created per-CR ExternalSecret can materialise
  before any credential is minted — once the AC is minted the PushSecret carries
  the minted credential-based clouds.yaml instead.
- **PushSecret to OpenBao.** `secrets.EnsurePushSecret` (idempotent; only Updates
  on a `DeepEqual` diff so ESO is not woken to re-push an unchanged credential)
  builds the PushSecret to `openbao-cluster-store` at the per-ControlPlane remote
  key `openstack/keystone/{cp.Namespace}/{cp.Name}/admin/app-credential`
  (`adminAppCredentialRemoteKeyFor`) with **`DeletionPolicy: None`** — the
  admin credential is a per-ControlPlane persistent bootstrap secret, so deleting
  the PushSecret on ControlPlane teardown (or when rotation is disabled) leaves the
  last-pushed credential intact in OpenBao at that CR's own path, so re-adoption
  works and the admin is never locked out.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `KORCReady` not True | False | `WaitingForKORC` | requeue 10s |
| clouds.yaml ES check errors | False | `CloudsYamlError` | returns the error |
| clouds.yaml ES not Ready | False | `WaitingForCloudsYaml` | requeue 10s |
| operator Secret ensure fails | False | `SecretError` | returns the error |
| PushSecret ensure fails | False | `PushSecretError` | returns the error |
| committed and mirrored | True | `AdminCredentialReady` | — |

### reconcileCatalog

| Aspect | Value |
| --- | --- |
| File | `reconcile_korc.go` |
| Condition | `CatalogReady` (also sets `cp.Status.CatalogReady = true`) |
| Gate | `AdminCredentialReady == True` |
| Owns | a K-ORC identity `Service` (`{controlplane.Name}-identity-service`) and its public `Endpoint` (`{controlplane.Name}-identity-endpoint`) in `childNamespace(cp)` |
| Requeue | `korcRequeueAfter` = **10s** while gated or while the K-ORC CRD is missing |

`reconcileCatalog` registers the OpenStack service-catalog entries for Keystone
as owned K-ORC CRs: an `identity`-type `Service` named
`keystone`, plus a `public` `Endpoint` whose URL defaults to the conventional
in-cluster identity URL `http://keystone.<namespace>.svc:5000/v3` and whose
`serviceRef` points at the identity Service. Both children are idempotent
create-or-updates; the K-ORC missing-CRD safety mirrors `reconcileKORC` via
`catalogCRDMissing`.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `AdminCredentialReady` not True | False | `WaitingForAdminCredential` | requeue 10s |
| K-ORC Service/Endpoint CRD missing | False | `KORCCRDNotInstalled` | requeue 10s (no hard error) |
| Service create/update fails | False | `ServiceError` | returns the error |
| Endpoint create/update fails | False | `EndpointError` | returns the error |
| both registered | True | `CatalogRegistered` | also sets `status.catalogReady = true` |

### CredentialRotation reconciler

| Aspect | Value |
| --- | --- |
| File | `reconcile_credentialrotation.go` |
| `For()` | `CredentialRotation` |
| Condition | `Ready` (`conditionTypeRotationReady`) |
| Owns / mints | **nothing** — it never mints |
| Requeue | `credentialRotationWaitInterval` = **10s** while waiting for the ControlPlane reconciler or for a dependency to appear |

The `CredentialRotationReconciler` drives one-shot rotations of a control-plane
credential by **nudging** the ControlPlane reconciler rather than duplicating any
mint logic. Its model:

- **Nudge, never mint or delete.** To force a re-mint it simply **clears** (zeroes)
  the `forge.c5c3.io/admin-password-hash` annotation on the owned AC CR via
  `clearPasswordHashAnnotation` (a no-op `Update` when already empty). On its next
  pass `reconcileKORC` observes the mismatch and performs the delete+recreate
  re-mint, re-stamping the fresh hash. Keeping the AC's resource lifecycle
  (including the delete) owned solely by the ControlPlane reconciler avoids two
  controllers racing on the same object.
- **ControlPlane resolution (one-per-namespace).** A `CredentialRotation`
  carries no explicit ControlPlane reference, so `resolveControlPlane` lists
  ControlPlanes in the CredentialRotation's **own** namespace and requires
  exactly one. Zero → `Ready=False` reason `NoControlPlane` with a short requeue;
  multiple → `Ready=False` reason `AmbiguousControlPlane` with **no** requeue (an
  arbitrary pick could rotate the wrong credential). The one-ControlPlane-per-namespace
  contract is now enforced at admission by the ControlPlane validating webhook
  (`validateUniqueInNamespace`), so the `AmbiguousControlPlane` branch is
  **defense-in-depth**: it is retained as a safety fallback but is unreachable while
  the webhook is active.
- **Bootstrap is idempotent.** With `spec.bootstrap`, an already-existing AC is a
  no-op success (`BootstrapComplete`); a missing AC waits (`WaitingForBootstrap`)
  for the ControlPlane reconciler to mint it.
- **Scheduled fields are read-but-ignored.** `intervalDays` / `preRotationDays`
  / `gracePeriodDays` are accepted but deferred to a later level; when set, an
  informational `ScheduledRotationDeferred` event is emitted but **no** loop runs
  and **no** error is raised.
- **Target enum.** Only `adminApplicationCredential` is supported; any other
  target finishes `Ready=False` reason `UnsupportedTarget`.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| target not `adminApplicationCredential` | False | `UnsupportedTarget` | no requeue |
| no ControlPlane in namespace | False | `NoControlPlane` | requeue 10s |
| multiple ControlPlanes | False | `AmbiguousControlPlane` | no requeue; defense-in-depth — unreachable while the one-per-namespace webhook is active |
| ControlPlane List errors | False | `ControlPlaneListError` | no requeue |
| bootstrap, AC exists | True | `BootstrapComplete` | no-op success |
| bootstrap, AC absent | False | `WaitingForBootstrap` | requeue 10s |
| rotation, AC absent | False | `WaitingForApplicationCredential` | requeue 10s |
| admin password not yet readable | False | `WaitingForAdminPassword` | requeue 10s |
| hash unchanged, `reMint` not set | True | `NoRotationNeeded` | nothing to do |
| nudge performed | True | `RotationTriggered` | emits `RotationNudged` event |

The CredentialRotation reconciler is registered with the manager via a plain
`For(&CredentialRotation{})` — it owns no children and registers no watches or
field indexers.

---

## K-ORC admin credential chain

The end-to-end path that delivers the admin application credential to the K-ORC
controller spans three sub-reconcilers and the ESO/OpenBao backend:

```text
OpenBao kv  bootstrap/{cp.Namespace}/{cp.Name}-keystone/admin   (admin password)
        │  (managed mode; reconcileAdminPassword, owner-ref'd to the ControlPlane)
        ▼
ExternalSecret  →  {control-plane ns}/{controlplane.Name}-keystone-admin-credentials
        │            (ESO owns the materialised Secret; CreationPolicy: Owner)
        ▼
admin-password Secret (the effective admin-password ref; read by c5c3-operator)
        │  SHA-256 → forge.c5c3.io/admin-password-hash annotation
        ▼
c5c3-operator mints a RESTRICTED ApplicationCredential        (reconcileKORC)
   restricted:true  ⇒  K-ORC spec.resource.unrestricted=false  (INVERSION)
        │
        ▼
K-ORC writes the minted credential into the operator-owned Secret
   {controlplane.Name}-admin-app-credential   (Resource.SecretRef target)
        │
        ▼  (reconcileAdminCredential, gated on KORCReady + clouds.yaml ES)
PushSecret  →  OpenBao kv  openstack/keystone/{cp.Namespace}/{cp.Name}/admin/app-credential
   (DeletionPolicy: None — per-ControlPlane bootstrap secret survives teardown)
        │
        ▼
ExternalSecret  →  {control-plane ns}/k-orc-clouds-yaml  (the clouds.yaml gate;
        │            operator-created per-CR by reconcileKORC, owner-ref'd to
        │            the ControlPlane; the orc-system copy is the retained
        │            STATIC manifest for K-ORC's global mount)
        ▼
K-ORC controller authenticates with the admin clouds.yaml and reconciles
   the catalog Service + Endpoint                              (reconcileCatalog)
```

**Re-mint trigger.** A rotation is signalled by comparing
`SHA-256(admin password)` against the `forge.c5c3.io/admin-password-hash`
annotation last stamped on the AC CR. `reconcileKORC` re-stamps and re-mints when
they differ; the CredentialRotation reconciler forces the same path by clearing
the annotation (which guarantees a mismatch). The admin-password Secret watch
(see [Secret Field Indexer](#secret-field-indexer)) wakes the ControlPlane the
moment the password rotates so the chain converges without waiting for the next
periodic requeue.

---

## Multi-instance

The ControlPlane reconciler scopes every admin / K-ORC credential it owns to
the individual ControlPlane CR, so multiple control planes can coexist in a single
cluster without sharing OpenBao state.

- **One ControlPlane per namespace (admission-enforced).** The ControlPlane
  validating webhook's `validateUniqueInNamespace` check runs in
  `ValidateCreate` **only** (not `ValidateUpdate`): it lists ControlPlanes in the
  object's namespace and returns `field.Forbidden` naming the incumbent if one
  already exists. The cluster therefore admits **exactly one** ControlPlane per
  namespace. This is what makes the CredentialRotation reconciler's
  `AmbiguousControlPlane` branch defense-in-depth (see
  [CredentialRotation reconciler](#credentialrotation-reconciler)).
- **Per-CR OpenBao path for the admin AC.** The admin application credential is
  pushed to the per-ControlPlane key
  `openstack/keystone/{cp.Namespace}/{cp.Name}/admin/app-credential`
  (`adminAppCredentialRemoteKeyFor`), so two ControlPlanes never write to the same
  OpenBao object. The K-ORC admin `User` CR is named `{cp.Name}-user-admin` (its
  Kubernetes `metadata.name`), while the **OpenStack** username it imports stays
  `admin` (set via the import `Filter.Name`) — the K8s-name and the OpenStack-name
  are deliberately split so the per-CR Kubernetes object is unique while the
  OpenStack identity is unchanged.
- **Per-CR OpenBao path for the service DB credential.** The managed-mode service
  database credential is read from the per-ControlPlane key
  `openstack/keystone/{cp.Namespace}/{cp.Name}/db`
  (`dbCredentialRemoteKeyFor`), scoped by **both** namespace and name (mirroring
  `adminAppCredentialRemoteKeyFor`) so two ControlPlanes never collide on the
  cluster-global OpenBao backend. `reconcileDBCredentials`
  projects an owned ExternalSecret reading `username`/`password` from this key.
- **Running multiple control planes.** Because the webhook caps a namespace at one
  ControlPlane and the OpenBao paths are keyed by `{namespace}/{name}`, operating
  several control planes means deploying each into its **own** namespace. Each gets
  a disjoint OpenBao prefix
  (`openstack/keystone/{namespace}/{name}/admin/app-credential`), disjoint child CRs
  in its own namespace, and an independent rotation lifecycle — no two control
  planes can clobber one another's credentials.

See [Migration: legacy flat paths → per-ControlPlane paths](#migration-legacy-flat-paths--per-controlplane-paths)
for moving an existing single-instance cluster onto the per-CR layout.

---

## Owner-ref / GC model

All child CRs created by the sub-reconcilers carry an owner reference to the
ControlPlane CR via `controllerutil.SetControllerReference()`. This enables both
**automatic garbage collection** (deleting the ControlPlane cascades to its
children) and **watch-based reconciliation** (a child change re-reconciles the
owner).

> **No finalizer.** Unlike the
> [Keystone reconciler](../keystone/keystone-reconciler.md#finalizer), the
> ControlPlane reconciler installs **no finalizer**. Teardown is driven entirely
> by owner-reference garbage collection — there is no ordered external cleanup
> the operator must perform before the CR leaves etcd.

> **Children live in the owner's namespace.** Every projected child is created in
> `childNamespace(cp) = cp.Namespace`, **not** a hardcoded `openstack`. A
> cross-namespace owner reference is rejected at admission ("cross-namespace
> owner references are disallowed") because Kubernetes GC only cascades within a
> single namespace; co-locating children with their owner keeps the owner
> reference valid and the GC cascade intact. In production the ControlPlane is
> deployed into the `openstack` namespace, so the children land there exactly as
> before — the namespace is now *derived from the owner* rather than assumed.

| Resource | Name | Owner | Notes |
| --- | --- | --- | --- |
| `MariaDB` | `{spec.infrastructure.database.clusterRef.name}` | ControlPlane CR | managed mode only |
| `Memcached` (unstructured) | `{spec.infrastructure.cache.clusterRef.name}` | ControlPlane CR | managed mode only |
| `ExternalSecret` (DB credential) | `{name}-keystone-db-credentials` | ControlPlane CR | managed mode only; ESO owns the materialised Secret of the same name |
| `ExternalSecret` (admin password) | `{name}-keystone-admin-credentials` | ControlPlane CR | managed mode only; ESO owns the materialised Secret of the same name |
| `Keystone` | `{name}-keystone` | ControlPlane CR | — |
| `ApplicationCredential` | `{name}-admin-app-credential` | ControlPlane CR | carries `forge.c5c3.io/admin-password-hash` |
| `Secret` | `{name}-admin-app-credential` | ControlPlane CR | data written by K-ORC, not the operator |
| `PushSecret` | `{name}-admin-app-credential-backup` | ControlPlane CR | `DeletionPolicy: None` |
| `Service` (K-ORC) | `{name}-identity-service` | ControlPlane CR | identity catalog entry |
| `Endpoint` (K-ORC) | `{name}-identity-endpoint` | ControlPlane CR | public interface |

### Security invariant

The admin password and the minted application-credential Secret are read **only**
by the c5c3-operator and the K-ORC controller pods — they are **never** mounted
into Keystone or any OpenStack service workload. Keystone
receives the admin password solely through its own bootstrap Secret ref for the
one-time `keystone-manage bootstrap`; the long-lived application credential lives
exclusively on the c5c3↔K-ORC↔OpenBao path. `restricted: true` (the default)
further bounds the blast radius by scoping the minted credential. These
invariants are enforced by the `credential_invariant_test.go` checks
(`TestCredentialInvariant_MintedACIsRestricted`,
`TestCredentialInvariant_AppCredentialSecretAbsentFromKeystoneSpec`,
`TestCredentialInvariant_AppCredentialSecretReferencedOnlyByPushSecretAndAC`,
`TestCredentialInvariant_NoWorkloadReferencesAppCredentialSecret`).

The `PushSecret`'s `DeletionPolicy: None` is the one deliberate exception to the
GC cascade: tearing down a ControlPlane removes the PushSecret CR but leaves the
last-pushed credential in OpenBao at this ControlPlane's own per-CR path
(`openstack/keystone/{cp.Namespace}/{cp.Name}/admin/app-credential`), so a
re-created control plane in the same namespace re-adopts that per-ControlPlane
bootstrap secret rather than being locked out mid-rotation.

---

## Metrics Instrumentation

Every sub-reconciler invocation is instrumented for Prometheus via a single
helper, `instrumentSubReconciler`, defined in
`operators/c5c3/internal/controller/instrumentation.go`. `Reconcile` wraps every
sub-reconciler call with it; a direct call that bypasses the helper is a contract
violation.

```go
func instrumentSubReconciler(
    ctx  context.Context,
    name string,
    fn   func(context.Context) (ctrl.Result, error),
) (ctrl.Result, error)
```

Behavioural contract:

- **Always** records one observation in
  `c5c3_operator_reconcile_duration_seconds{sub_reconciler=name}` via `defer` —
  on the success path, the error path, and even when `fn` panics (the deferred
  call runs before the stack unwinds).
- **Only** increments
  `c5c3_operator_reconcile_errors_total{sub_reconciler=name, condition_type=…}`
  when `fn` returns a non-nil error.
- Does **not** recover from panics — they propagate to the caller.
- Carries **no per-CR labels** (no `controlplane` / `namespace`). The two label
  dimensions (`sub_reconciler`, and `condition_type` on the error counter) are
  bounded by the number of sub-reconcilers, keeping the series count
  fleet-independent. Per-CR collectors are intentionally out of scope.

Both vectors are registered exactly once on the controller-runtime registry via
`sync.Once`; the histogram buckets are a fixed contract
(`0.01 … 30s`).

### Name → `condition_type` lookup and the drift guard

The `condition_type` label is resolved from the package-private
`subReconcilerConditionTypes` map in `instrumentation.go`:

| `sub_reconciler` | `condition_type` |
| --- | --- |
| `Infrastructure` | `InfrastructureReady` |
| `DBCredentials` | `DBCredentialsReady` |
| `Keystone` | `KeystoneReady` |
| `KORC` | `KORCReady` |
| `AdminCredential` | `AdminCredentialReady` |
| `AdminPassword` | `AdminPasswordReady` |
| `Catalog` | `CatalogReady` |

If `instrumentSubReconciler` is ever called with a name absent from the map, the
helper emits the sentinel `condition_type=UNKNOWN`
(`subReconcilerConditionTypeUnknown`) rather than an empty label, so any drift is
visible in dashboards/alerts. Two static drift guards keep the map honest:
`TestSubReconcilerConditionTypesCoversAllNames` asserts that every mapped
`condition_type` is a member of `subConditionTypes`, and
`TestSubReconcilerConditionTypesCoversAllCallSites` walks the source AST to
assert every `instrumentSubReconciler` call-site name is a map key. Adding a new
sub-reconciler therefore requires updating `subConditionTypes` **and**
`subReconcilerConditionTypes` or CI fails.

---

## Testing

The reconcilers have comprehensive unit tests using the controller-runtime fake
client with `gomega` (`NewGomegaWithT(t)`), plus a single envtest integration
test that drives the full chain in a real manager against a live API server.

### Running Tests

| Scope | Command |
| --- | --- |
| All controller unit tests | `go test ./operators/c5c3/internal/controller/...` |
| Integration (envtest) | `go test -tags integration -run TestIntegration_FullReconcile_ManagedToReady ./operators/c5c3/internal/controller/` |

### Integration test

`TestIntegration_FullReconcile_ManagedToReady` (`integration_test.go`, build tag
`integration`) registers the real controller wiring (the inline
builder is kept byte-for-byte in step with `SetupWithManager`) and drives a
managed-mode ControlPlane through every sub-reconciler to the aggregate
`Ready=True`. It simulates each external dependency's readiness **in dependency
order** — MariaDB and Memcached Ready → the operator-created admin-password
`ExternalSecret` synced → Keystone child Ready → K-ORC
`ApplicationCredential` `Available` with a `status.id` → the
`{control-plane ns}/k-orc-clouds-yaml` `ExternalSecret` synced — and asserts that
every sub-condition and the aggregate `Ready` (reason `AllReady`) reach `True`,
that `status.observedGeneration` and every condition's `ObservedGeneration` match
the CR generation, and that `status.adminApplicationCredential` mirrors the
simulated AC. Beyond the aggregate condition it also asserts the **intermediate
projected specs** so a projection regression is caught: the Keystone
image tag derived from `openStackRelease`, the database/cache `clusterRef`s wired
to the infra CRs, the merged `policyOverrides`, the `restricted→Unrestricted=false`
inversion on the AC, and the identity `Service`/`Endpoint` shape.

A phase between Infrastructure and Keystone exercises the new admin-password
projection: it waits for the operator-created per-CP admin-password
`ExternalSecret`, asserts its `RemoteRef.Key` equals `adminPasswordRemoteKeyFor(cp)`
and that it is controller-owned by the ControlPlane, then simulates the ESO sync —
`SimulateExternalSecretSync` patches **only** the ExternalSecret's status, so the
renamed plain Secret (pre-created under the same name) stays the cleartext source
the operator reads — and waits for `AdminPasswordReady` to reach `True` before the
Keystone child is projected.

### Test Files

| File | Coverage |
| --- | --- |
| `controlplane_controller_test.go` | `Reconcile` orchestration, sequential early-return, Ready aggregation, `updateStatus` error-join, idempotency |
| `reconcile_infrastructure_test.go` | Managed/brownfield MariaDB + Memcached, unstructured readiness, condition contract, `ObservedGeneration` |
| `reconcile_dbcredentials_test.go` | Managed ExternalSecret projection (name/store/data/owner-ref), brownfield no-op `Ready=True`, not-ready requeue + condition contract, distinct per-CP remote key/secret name |
| `reconcile_adminpassword_test.go` | Managed ExternalSecret projection (name/store/data/owner-ref), brownfield no-op `Ready=True`, not-ready requeue + condition contract, distinct per-CP remote key/secret name |
| `reconcile_keystone_test.go` | Keystone projection, infra gate, image/rotation/policy projection, condition contract, `ObservedGeneration` |
| `reconcile_korc_test.go` | AC mint, restricted↔unrestricted inversion, hash annotation/re-mint, missing-CRD safety, admin-credential push, catalog, condition contract |
| `reconcile_credentialrotation_test.go` | Nudge model, one-per-namespace resolution, bootstrap, deferred scheduled fields, target enum |
| `credential_invariant_test.go` | Security invariants (restricted mint, app-credential Secret not on any workload) |
| `instrumentation_test.go` | Duration/error emission, name→`condition_type` resolution, drift guards |
| `setupwithmanager_test.go` | `For`/`Owns`/`Watches` wiring, field-indexer registration |
| `helpers_test.go` | `intervalToCron`, `projectPolicyOverrides` |
| `integration_test.go` | Full envtest reconciliation to `Ready=True` (build tag `integration`) |

---

## File Layout

```text
operators/c5c3/
├── main.go                                     Scheme registration + bootstrap wiring, leaderElectionID
├── api/v1alpha1/
│   ├── controlplane_types.go                   ControlPlane CRD types
│   ├── credentialrotation_types.go             CredentialRotation CRD types
│   ├── secretaggregate_types.go                SecretAggregate CRD types
│   ├── controlplane_webhook.go                 ControlPlaneWebhook (validating + defaulting)
│   └── ...
└── internal/
    ├── controller/
    │   ├── controlplane_controller.go          Reconciler struct, Reconcile(), setReadyCondition,
    │   │                                        aggregateReady, updateStatus, secret field indexer,
    │   │                                        SetupWithManager
    │   ├── reconcile_infrastructure.go          reconcileInfrastructure (MariaDB + Memcached),
    │   │                                        childNamespace, memcachedGVK
    │   ├── reconcile_dbcredentials.go           reconcileDBCredentials (per-CP DB-credential ExternalSecret)
    │   ├── reconcile_adminpassword.go           reconcileAdminPassword (per-CP admin-password ExternalSecret),
    │   │                                        effectiveAdminPasswordSecretRef
    │   ├── reconcile_keystone.go                reconcileKeystone projection
    │   ├── reconcile_korc.go                    reconcileKORC + reconcileAdminCredential +
    │   │                                        reconcileCatalog, computeAdminPasswordHash
    │   ├── reconcile_credentialrotation.go      CredentialRotationReconciler (nudge model)
    │   ├── requeue_intervals.go                 infra/dbCredentials/adminPassword/keystone/korc/credentialRotation backoffs
    │   ├── instrumentation.go                   instrumentSubReconciler + drift-guard map
    │   ├── helpers.go                           intervalToCron, projectPolicyOverrides
    │   ├── controlplane_controller_test.go      Orchestration tests
    │   ├── reconcile_infrastructure_test.go     Infrastructure tests
    │   ├── reconcile_dbcredentials_test.go      DBCredentials tests
    │   ├── reconcile_adminpassword_test.go      AdminPassword tests
    │   ├── reconcile_keystone_test.go           Keystone projection tests
    │   ├── reconcile_korc_test.go               K-ORC / admin-credential / catalog tests
    │   ├── reconcile_credentialrotation_test.go CredentialRotation tests
    │   ├── credential_invariant_test.go         Security-invariant tests
    │   ├── instrumentation_test.go              Metrics instrumentation + drift guards
    │   ├── setupwithmanager_test.go             Watch/Owns/indexer wiring tests
    │   ├── helpers_test.go                      helper-function tests
    │   └── integration_test.go                  Envtest integration test (tag: integration)
    ├── metrics/
    │   └── collectors.go                        c5c3_operator_* duration/error vectors
    └── testutil/                                c5c3 envtest setup helpers
```

## Migration: legacy flat paths → per-ControlPlane paths

Earlier releases wrote the admin / K-ORC credentials to cluster-global,
flat OpenBao paths that assumed a single control plane per cluster. The operator
now writes every credential family onto a per-CR path keyed by the owning
ControlPlane's (or projected Keystone CR's) `{namespace}/{name}`, so multiple
control planes (one per
namespace; see [Multi-instance](#multi-instance)) never collide in OpenBao. This is
a one-time operator runbook to migrate an **existing** cluster; new clusters need
no migration.

The new `RemoteKey` lands the moment the operator is upgraded — the next reconcile
of each CR emits the per-CR path — so re-apply the OpenBao ACLs **first or
concurrently** with the operator upgrade. Without the updated policies ESO returns
`403` on the backup/push and the corresponding Ready conditions flip `False`
(`AdminCredentialReady` for the admin AC; `PasswordRotationReady` for the Model-B
admin password; `FernetKeysReady` / `CredentialKeysReady` for the signing keys).

**Path mapping (legacy → per-CR):**

| Credential family | Legacy flat path | Per-CR path |
| --- | --- | --- |
| Admin application credential (K-ORC) | `openstack/keystone/admin/app-credential` | `openstack/keystone/{namespace}/{name}/admin/app-credential` |
| Admin bootstrap password (Model B) | `bootstrap/keystone-admin` | `bootstrap/{namespace}/{name}/admin` |
| Fernet / credential keys (boundary-4) | `openstack/keystone/{name}/{fernet,credential}-keys` | `openstack/keystone/{namespace}/{name}/{fernet,credential}-keys` |

For the admin AC the `{namespace}/{name}` is the **ControlPlane** CR's
(`adminAppCredentialRemoteKeyFor`); for the admin password and the Fernet /
credential keys it is the projected **Keystone** CR's (`{cp.Name}-keystone`). The
Fernet / credential move adds the namespace segment **on top of** the prior
flat→per-name migration (see the keystone reconciler's
[Migration note: legacy flat paths](../keystone/keystone-reconciler.md#migration-note-legacy-flat-paths));
this change only adds the leading `{namespace}/` segment.

**One-time copy (preserve the last-pushed value so nothing is locked out):**

```sh
# admin application credential (per ControlPlane <ns>/<cp>)
bao kv get kv-v2/openstack/keystone/admin/app-credential
bao kv put kv-v2/openstack/keystone/<ns>/<cp>/admin/app-credential clouds.yaml=@-
# admin bootstrap password (per Keystone CR <ns>/<name>, name = <cp>-keystone)
bao kv get kv-v2/bootstrap/keystone-admin
bao kv put kv-v2/bootstrap/<ns>/<name>/admin password=<value>
# fernet / credential keys (per Keystone CR <ns>/<name>)
bao kv get kv-v2/openstack/keystone/<name>/fernet-keys
bao kv put kv-v2/openstack/keystone/<ns>/<name>/fernet-keys value=<value>
bao kv get kv-v2/openstack/keystone/<name>/credential-keys
bao kv put kv-v2/openstack/keystone/<ns>/<name>/credential-keys value=<value>
```

**Re-apply the OpenBao ACLs.** Re-run
`deploy/openbao/bootstrap/setup-policies.sh` (the kind/dev path; also invoked by
`hack/deploy-infra.sh`), or for production clusters managed outside the bootstrap
flow apply the updated policy files directly with `bao policy write …`:

| Policy file | Grants write to |
| --- | --- |
| `push-app-credentials.hcl` | the per-CR admin AC path `…/keystone/+/+/admin/app-credential` |
| `push-keystone-admin.hcl` | the per-CR admin-password path `bootstrap/+/+/admin` |
| `push-keystone-keys.hcl` | the per-CR `…/keystone/+/+/{fernet,credential}-keys` paths |

Until the matching policy is re-applied, ESO's push to the new path returns `403`
and the credential's Ready condition stays `False`.

**Orphaned but harmless.** After migration the legacy flat paths are **orphaned but
harmless**: the live control plane no longer reads or refreshes them, no live
PushSecret references them, and they are otherwise inert. Operators who want a clean
OpenBao state can purge them once the per-CR paths are confirmed populated and Ready:

```sh
bao kv metadata delete kv-v2/openstack/keystone/admin/app-credential
bao kv metadata delete kv-v2/bootstrap/keystone-admin
bao kv metadata delete kv-v2/openstack/keystone/<name>/fernet-keys
bao kv metadata delete kv-v2/openstack/keystone/<name>/credential-keys
```

`metadata delete` removes the current version and all historical versions at the
path — the canonical KV-v2 purge and the right inverse of the now-superseded write.
(The Fernet / credential families were previously migrated flat→per-name in an
earlier release, and the boundary-4 change layers the namespace segment on top.)

## Architecture references

The `ControlPlane` reconciler and the K-ORC self-credentialing chain implement
the following upstream architecture chapters (in the `architecture/` submodule,
[github.com/C5C3/C5C3](https://github.com/C5C3/C5C3)). They are the authoritative
design source for this reconciler:

- `architecture/docs/09-implementation/08-c5c3-operator.md` — the c5c3-operator
  `ControlPlane` reconciler contract and sub-reconciler ordering.
- `architecture/docs/03-components/01-control-plane/05-korc.md` — the K-ORC
  component, the per-resource `cloudCredentialsRef` resolution model (resolved in
  the resource's own namespace, the basis for the C1 co-location fix), and the
  chart constraint.
- `architecture/docs/05-deployment/01-gitops-fluxcd/01-credential-lifecycle.md` —
  the restricted, password-driven admin Application Credential lifecycle and the
  operator bootstrap-seed → mint → PushSecret → operator-owned per-CR
  ExternalSecret round-trip.
