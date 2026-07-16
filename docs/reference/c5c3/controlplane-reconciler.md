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
owns child CRs (MariaDB, Memcached, Keystone, Horizon, K-ORC
`ApplicationCredential` / `Service` / `Endpoint`) and aggregates their readiness. It does **not**
re-implement the per-service logic those child operators already own. As a
consequence the c5c3 API surface is deliberately smaller than the
[Keystone reconciler](../keystone/keystone-reconciler.md)'s: no parallel
sub-reconciler group and no per-CR metric cardinality. It does install a single
finalizer to sequence K-ORC teardown ahead of Keystone/infrastructure teardown
on deletion — see [Owner-ref / GC model](#owner-ref--gc-model).

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
| `github.com/external-secrets/external-secrets` | `esov1.SchemeBuilder` | `ExternalSecret`, `ClusterSecretStore`, `SecretStore` (K-ORC clouds.yaml gate + per-ControlPlane store selection) |
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
sub-reconcilers project (including the owned ESO `ExternalSecret` and
`PushSecret`), the admin-password `Secret`, and both OpenBao-backed store
kinds (`ClusterSecretStore` and `SecretStore`):

| Resource | Watch Type | Effect |
| --- | --- | --- |
| `ControlPlane` | `For()` | Triggers reconciliation on CR changes |
| `MariaDB` | `Owns()` | Re-reconciles the owning ControlPlane when the managed MariaDB child status changes |
| `Memcached` (unstructured `memcachedGVK`) | `Owns()` | Re-reconciles when the managed Memcached child status changes; owned as `*unstructured.Unstructured` because the kind has no Go module |
| `Keystone` | `Owns()` | Re-reconciles when the projected Keystone child status changes |
| K-ORC `ApplicationCredential` | `Owns()` | Re-reconciles when the minted admin credential's `Available` condition or `status.id` changes |
| K-ORC `Service` | `Owns()` | Re-reconciles when the identity catalog Service changes |
| K-ORC `Endpoint` | `Owns()` | Re-reconciles when the public identity Endpoint changes |
| K-ORC `Role` | `Owns()` | Re-reconciles when a service-account role's unmanaged `Role` import resolves |
| K-ORC `RoleAssignment` | `Owns()` | Re-reconciles when a service-account managed `RoleAssignment` becomes Available |
| `ExternalSecret` | `Owns()` | Re-reconciles when an owned ESO ExternalSecret (DB credential, admin password, K-ORC clouds.yaml) syncs or fails, so the credential conditions track ESO promptly |
| `PushSecret` | `Owns()` | Re-reconciles when the owned admin-credential PushSecret status changes |
| `Secret` | `Watches()` | Maps Secret events to referencing ControlPlane CRs via the `ControlPlaneSecretNameIndexKey` field indexer (`secretToControlPlaneMapper`) |
| `ClusterSecretStore` | `Watches()` | Per-ref fan-out via `storeToControlPlaneMapper` (bound to the shared `watch.StoreRefFanOut` for the cluster kind): a status change on a cluster-scoped store enqueues only the ControlPlanes whose effective `spec.secretStoreRef` resolves to it |
| `SecretStore` | `Watches()` | The namespaced twin, scoped to the store's own namespace, so a ControlPlane pinned to a per-tenant `SecretStore` reacts to its backend health (`storeToControlPlaneMapper` for the namespaced kind) |

The `Secret` watch uses `Watches()` with a `MapFunc` rather than `Owns()`
because the admin-password Secret
(`spec.korc.adminCredential.passwordSecretRef`) is typically **ESO-managed** —
it is owned by the ExternalSecret controller, not by the ControlPlane CR — so an
owner-reference filter would never match it. The index-backed namespace List is
exactly what wakes the ControlPlane when its admin password rotates, so the
re-mint chain (see [K-ORC admin credential chain](#k-orc-admin-credential-chain))
converges on watch delivery instead of waiting for the next periodic requeue.

Both store watches are per-ref fan-outs rather than blanket enqueues: a status
transition (for example ESO losing the backend connection) wakes only the
ControlPlanes whose effective `spec.secretStoreRef` resolves to the changed
store — the cluster watch lists every namespace, the namespaced watch only the
store's own. A ControlPlane pinned to a different store stays untouched. This is
why the DB-credential, admin-password, and admin-credential sub-reconcilers can
flip their conditions to `SecretStoreNotReady` the moment the ControlPlane's own
backend becomes unreachable instead of waiting up to a full ESO refresh interval
(default 1h) for the next per-secret re-sync. The `ExternalSecret`/`PushSecret`
children are owned (controller reference), so `Owns()` wires them directly.

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
| `external-secrets.io` | `clustersecretstores`, `secretstores` | get, list, watch |
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

### Blast radius and namespace scoping

By default the chart binds these markers to a cluster-wide `ClusterRole`, so the
`secrets` rule lets a compromised operator pod read and write every `Secret` in
every namespace; the
[Multi-Tenant Deployment → Security trade-off](../../guides/multi-tenant-deployment.md#security-trade-off-the-cluster-wide-rbac-default)
details that privilege-escalation path. Two specifics apply to this operator:

- It amplifies the exposure itself: `reconcileAdminPassword` and `reconcileKORC`
  project the OpenStack admin password **in cleartext** into a `clouds.yaml`
  `Secret`, so cluster-wide read access exposes every projected admin password.
- Unlike the keystone operator, this `ClusterRole` holds no `roles` /
  `rolebindings` verbs, so it lacks the RoleBinding-forgery escalation
  primitive — the cluster-wide Secret read is the dominant risk.

A single-namespace deployment — one where no service is placed in a namespace of
its own — co-locates every projected resource in the ControlPlane's own
namespace, so it can run the operator namespace-scoped
(`rbac.namespaceScoped: true`), bounding both the RBAC grant and the informer
cache to that namespace. Keep the default only when
[cluster-wide RBAC is still required](../../guides/multi-tenant-deployment.md#when-cluster-wide-rbac-is-still-required).

[Dedicated service namespaces](./controlplane-crd.md#service-namespaces) are
**incompatible with namespace-scoped mode**: placing a service in a namespace of
its own needs cluster-scoped `namespaces` verbs (`create`, `delete`) and
cross-namespace access to the children, which only the default ClusterRole mode
grants. The markers therefore add `core/namespaces` with
`get;list;watch;create;delete`.

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
│  Duplicate guard — park all but the oldest ControlPlane in the namespace     │
│  (Ready=False / DuplicateControlPlane, requeue 30s; see Multi-instance)      │
│         │                                                                    │
│         ▼                                                                    │
│  ┌──────────────────────────┐                                                │
│  │ reconcileNamespaces      │  Ensure the namespaces services are placed in  │
│  │  (gate: none)            │  Sets: NamespacesReady                         │
│  └────────┬─────────────────┘  Requeue: 15s while a namespace is unusable    │
│           │  (True immediately when no service declares a namespace)         │
│           ▼                                                                  │
│  ┌──────────────────────────┐                                                │
│  │ reconcileInfrastructure  │  Ensure managed MariaDB + Memcached children   │
│  │  (gate: none)            │  Sets: InfrastructureReady                     │
│  └────────┬─────────────────┘  Requeue: 15s while a child is not Ready       │
│           │  early-return if !result.IsZero() || err                         │
│           ▼                                                                  │
│  ┌──────────────────────────┐                                                │
│  │ reconcileESOTenantStore  │  Provision the per-tenant SecretStore + SA +   │
│  │  (gate: none)            │  mTLS cert. Sets: ESOTenantStoreReady          │
│  └────────┬─────────────────┘  Requeue: 10s while the store is not Ready     │
│           │  (skipped when spec.secretStoreRef overrides the default)        │
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
│  │ reconcileHorizon         │  Project the Horizon dashboard child CR        │
│  │  (gate: KeystoneReady)   │  Sets: HorizonReady (not-managed when unset)   │
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
│  │  (gate: AdminCredReady)  │  Sets: CatalogReady (Service+Endpoint Available)│
│  └────────┬─────────────────┘  Requeue: 10s gated / not Available / terminal  │
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

All sub-reconcilers run **strictly sequentially** — there is no parallel
group. The chain is a table-driven pipeline over the shared scaffolding in
`internal/common/reconcile` (the same shape the keystone controller uses):
each step is a `commonreconcile.Step` wrapped in `instrumentSubReconciler`
(see [Metrics Instrumentation](#metrics-instrumentation)), and
`commonreconcile.RunPipeline` enforces the early-return contract at a single
call site:

```go
pipeline := []commonreconcile.Step{
    {Name: "Infrastructure", Fn: func(ctx context.Context) (ctrl.Result, error) {
        return r.reconcileInfrastructure(ctx, &cp)
    }},
    // ... DBCredentials, AdminPassword, Keystone, Horizon, KORC, AdminCredential, Catalog
}
result, err := commonreconcile.RunPipeline(ctx, instrumentSubReconciler, pipeline)
return r.updateStatus(ctx, &cp, statusBefore, result, err)
```

This guarantees:

1. A sub-reconciler error **propagates immediately** — subsequent sub-reconcilers
   are skipped.
2. A non-zero result (`RequeueAfter > 0`) causes an **early return** — status is
   persisted and the reconciler exits.
3. Status conditions from the failing/requeuing sub-reconciler are **always
   persisted** via `updateStatus()` before returning.

### Status Update Pattern

`updateStatus()` delegates to the shared `commonreconcile.UpdateStatus`: it
stamps `cp.Status.ObservedGeneration = cp.Generation`, records the per-service
status (`setServicesStatus()`, see below), persists all condition changes via
`r.Status().Update()`, and returns the provided `(result, error)` pair.

`Reconcile` snapshots `cp.Status` immediately after the initial Get and
threads that snapshot into `updateStatus`, which compares the computed status
against it with `equality.Semantic.DeepEqual` and **skips** the write when a
pass left status unchanged — no write means no watch event and no
`resourceVersion` churn on a converged steady-state pass. Together with the
`watch.CRUpdatePredicate` on the controller's `For(...)` watch (which filters
the CR's own status-only updates), this closes the self-wake loop the previous
always-write `updateStatus` and bare `For()` allowed.

When both a reconcile error and the status update fail, both errors are
preserved via `errors.Join` so the original reconcile failure remains visible in
controller-runtime logs:

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

The aggregated sub-condition types (the source-of-truth `subConditionTypes`
slice in `controlplane_controller.go`) are:

```text
InfrastructureReady, ESOTenantStoreReady, DBCredentialsReady, KeystoneReady, HorizonReady, KORCReady, AdminCredentialReady, AdminPasswordReady, CatalogReady, ServiceAccountsReady
```

The `Ready` condition carries `ObservedGeneration = cp.Generation` so clients can
detect a stale aggregate.

One path bypasses the aggregation entirely: a ControlPlane parked by the
duplicate guard (see [Multi-instance](#multi-instance)) gets `Ready=False` with
reason `DuplicateControlPlane` written directly — `setReadyCondition()` would
otherwise overwrite the reason with `NotAllReady` on the next status update.

### Services and Update Phase

`setServicesStatus()` runs on every `updateStatus` call and populates two status
fields that the schema declared but the reconciler previously never wrote:

| Field | Value |
| --- | --- |
| `status.updatePhase` | Fixed at `Idle` — the release-update state machine is not implemented and the other `UpdatePhase` values are reserved, so "no update in progress" is the current state |
| `status.services` | one entry per managed service, in a stable order: `keystone` (present when `spec.services.keystone` is set) then `horizon` (present when `spec.services.horizon` is set). Each entry's `ready` mirrors the matching `KeystoneReady` / `HorizonReady` sub-condition (via `conditions.AllTrue`) and `release` is `spec.openStackRelease`; an unmanaged service is omitted rather than reported |

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

### External keystone mode and the chain

When `spec.services.keystone.mode` is `External`, the ControlPlane manages
identity against a pre-existing, externally-operated Keystone. The chain keeps
its order; External mode changes what each link does. Four sub-reconcilers
short-circuit — `reconcileInfrastructure`, `reconcileDBCredentials`,
`reconcileAdminPassword` and `reconcileKeystone` — each reporting its own
condition with `Status=True` and reason `ExternallyManaged`, and a message naming
`spec.services.keystone.external.authURL`.

Skipped sub-reconcilers **keep their condition types**. The condition schema is
therefore identical across modes, so `subConditionTypes`, `setReadyCondition` and
the `condition_type` drift guard need no mode awareness.

Every skip is keyed on the mode discriminator `cp.IsExternalKeystone()`, never on
the database shape: an External-mode ControlPlane has no `spec.infrastructure`
block at all, so "no *managed* database" and "no database" are different states.
The vocabulary keeps three "nothing was projected" reasons deliberately apart:

| Reason | Meaning |
| --- | --- |
| `ExternallyManaged` | identity is managed against a pre-existing Keystone; this sub-reconciler has nothing to project |
| `KeystoneNotManaged` | `spec.services.keystone` is unset: there is no identity plane at all (staged adoption) |
| `BrownfieldUserSuppliedCredential` | the ControlPlane owns a Keystone, but the *database* is brownfield, so the user supplies its credential Secret |

`services.horizon` is forbidden in External mode, so `reconcileHorizon` always
takes its `HorizonNotManaged` early-exit. The duplicate-ControlPlane parking
guard is mode-agnostic and applies unchanged — External-mode ControlPlanes count
towards the one-per-namespace contract.

`reconcileCatalog` neither skips nor behaves as it does in Managed mode: it
forks. The catalog belongs to the external installation, so External mode is
**import-first** — the existing identity service and its endpoints are imported
read-only, zero catalog entries are created by default, and an import that
resolves to nothing fails loud rather than waiting forever. See
[reconcileCatalog](#reconcilecatalog).

#### Egress and TLS posture

K-ORC — not the c5c3-operator — is what dials the external Keystone. It is
installed by the Flux Kustomization `deploy/flux-system/releases/k-orc.yaml` into
the `orc-system` namespace, and the operator never opens an OpenStack connection
itself: everything stays K-ORC-mediated.

- **Egress.** No `NetworkPolicy` exists anywhere under `deploy/`, and none scopes
  `orc-system`, so nothing restricts K-ORC's egress on the shipped stack —
  out-of-cluster Keystone worked first-pass in the phase-1 spike. A cluster that
  applies a **default-deny egress** policy to `orc-system` must add an explicit
  allow rule to the external endpoint's host and port, or every mint, import and
  catalog call fails as `EndpointUnreachable`.
- **TLS.** An IP-based `authURL` requires an **IP SAN** in the external Keystone's
  server certificate — a CN or DNS SAN alone will not verify. Hostnames resolve
  through cluster DNS via the upstream forwarder, so no extra DNS wiring is
  needed. A privately-signed certificate needs
  `spec.services.keystone.external.caBundleSecretRef`; without it K-ORC reports
  an `x509` failure, classified onto `KORCReady=False/TLSVerificationFailed`.
- **CA-cache aliasing.** K-ORC's provider-client cache keys on the parsed cloud
  struct only — `cacert` is **not** part of the key (`internal/scope/provider.go`)
  — and the entry lives for the token lifetime / 2 (≈30 min at Keystone defaults).
  A rotated or removed CA bundle therefore converges the Secrets immediately but
  the trust store only after cache expiry. Nothing in this operator can shorten
  that window; an upstream fix would have to fold `cacert` into the cache key.

### reconcileNamespaces

| Aspect | Value |
| --- | --- |
| File | `reconcile_namespaces.go` |
| Condition | `NamespacesReady` |
| Gate | none (runs first) |
| Projects / Owns | `Namespace` objects for every service placed in a namespace of its own under the `Managed` lifecycle |
| Requeue | `namespaceRequeueAfter` = **15s** while a namespace is unusable |

`reconcileNamespaces` ensures the namespaces the ControlPlane's services are
placed in outside its own (see
[Service Namespaces](./controlplane-crd.md#service-namespaces)), and runs
**first** because every later sub-reconciler projects into one of them — applying
into a namespace that does not exist fails with an error naming neither the
ControlPlane nor the assignment behind it. A ControlPlane with no assignments (the
default) has nothing to ensure and reports `NamespacesReady=True` immediately, so
the step costs nothing on the common path.

The two lifecycles are asymmetric. Under **`Managed`** the operator creates the
namespace and stamps it with the ownership labels plus
`app.kubernetes.io/managed-by`; a namespace that already exists without those
labels is **never adopted** — the condition fails loud rather than taking over a
namespace it did not create. Under **`External`** the operator only verifies the
namespace exists; a missing one parks the condition and requeues.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `NoDedicatedNamespaces` | No service declares a namespace of its own. |
| `True` | `NamespacesReady` | Every declared service namespace is present (and, for `Managed`, owned). |
| `False` | `NamespaceNotFound` | An `External` namespace does not exist; requeue. |
| `False` | `NamespaceNotOwned` | A `Managed` namespace exists but lacks the operator's ownership labels — never adopted. |
| `False` | `NamespaceTerminating` | The namespace is being deleted; wait and requeue. |
| `False` | `FinalizingNamespaces` | On deletion, waiting for cross-namespace children to be torn down (see [Owner-ref / GC model](#owner-ref--gc-model)). |
| `False` | `NamespaceError` | A create/get against the namespace failed. |

### reconcileInfrastructure

| Aspect | Value |
| --- | --- |
| File | `reconcile_infrastructure.go` |
| Condition | `InfrastructureReady` |
| Gate | none |
| Projects / Owns | Managed-mode `MariaDB` (`k8s.mariadb.com`) and `Memcached` (unstructured `memcached.c5c3.io/v1beta1`) children, each named after its `clusterRef` and created in **the namespace of the service that resolves to it** (`cp.KeystoneNamespace()` / `cp.HorizonNamespace()`, the ControlPlane's own unless a `namespace` assignment places the service elsewhere) |
| Requeue | `infraRequeueAfter` = **15s** while a managed child is not yet Ready |

**Backing services follow the service.** `managedInfraInstances` adds each
service's effective database and cache **at that service's namespace** and
deduplicates on `(kind, namespace, name)`, so the one shared `spec.infrastructure`
block materializes once **per namespace** that consumes it — two instances when
Keystone and Horizon are placed apart, one when they are co-located, exactly one
(today's behavior) when neither is assigned a namespace. A child in a service
namespace carries no owner reference (Kubernetes forbids a cross-namespace one) —
it is stamped with the ownership labels and cleaned up by the finalizer instead;
a same-namespace child keeps its controller owner reference. The dashboard's cache
is enumerated only when the dashboard is **declared**, so a ControlPlane that
places Keystone apart never provisions a phantom cache for an absent Horizon.

`reconcileInfrastructure` provisions the backing services the ControlPlane owns.
That set is the instances its services actually **resolve to**, not the set of
declared blocks: the **shared** instances in `spec.infrastructure`, and the
per-service **dedicated** instances under
`services.<svc>.dedicatedBackingServices` (see
[DedicatedBackingServices](./controlplane-crd.md#dedicatedbackingservices)) that
a service opted into instead. `managedInfraInstances` enumerates them by walking
the effective-instance resolvers per service and deduplicating on the identity of
the child CR they resolve to — so several services on one shared instance ensure
it exactly once, and a **shared instance every service has opted out of has no
consumer and is not provisioned at all**. Keystone is the ControlPlane's only
database consumer, and the defaulting webhook materializes
`spec.infrastructure.database` whenever it is omitted, so provisioning the
declared set instead would leave a full Galera cluster nothing talks to — with
`InfrastructureReady` blocked on it coming up. A backing service is **managed**
when its `clusterRef` is set and **brownfield** (provisions nothing) when
`host`/`servers` are set instead.

Every managed child — shared or dedicated — is ensured in a single pass *before*
readiness is gated, so a half-provisioned control plane (DB created but cache
missing) never occurs; readiness is then evaluated **collectively** across the
whole set. A service whose dedicated database is still converging therefore holds
`InfrastructureReady` `False` even when every other instance is Ready, so that
service's projection stays gated on the database it actually talks to. The
condition message names the pending instance and the spec path it was declared
at; the *reasons* stay per-class (`WaitingForDatabase` / `WaitingForCache`), so
the reason vocabulary is unchanged by the dedicated opt-in.

`ensureMariaDB` / `ensureMemcached` take the **declared instance** rather than
reading `spec.infrastructure` directly, which is what makes a dedicated instance
carry the shared block's lifecycle rather than a parallel one of its own: it is
created with a controller owner reference (so it is garbage-collected with the
ControlPlane), sized from **its** `replicas` / `storageSize`, re-projected on
drift while owned, and **adopted read-only** — never reshaped, never GC-claimed —
when a CR under that name already exists.

`spec.infrastructure` is optional: an **External**-mode Keystone ControlPlane
omits it (the validating webhook forbids it in External mode and requires it
otherwise). In External mode the sub-reconciler provisions nothing and reports
`InfrastructureReady=True` / `ExternallyManaged` immediately. A **non**-External
ControlPlane that nevertheless reaches this point with the block unset is a
webhook-bypass shape (direct etcd write, admission misconfigured); it fails
closed with `InfrastructureNotConfigured` rather than dereferencing the nil
pointer.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| External keystone mode | True | `ExternallyManaged` | no MariaDB/Memcached is provisioned; the message names `external.authURL` |
| MariaDB create/update fails | False | `MariaDBError` | returns the error (controller-runtime backoff) |
| Memcached create/update fails | False | `MemcachedError` | returns the error |
| MariaDB not yet Ready | False | `WaitingForDatabase` | requeue 15s |
| Memcached not yet Ready | False | `WaitingForCache` | requeue 15s |
| `spec.infrastructure` unset, not External | False | `InfrastructureNotConfigured` | requeue 15s; unreachable on the admission path — fails closed for a webhook-bypassed CR |
| All managed children Ready (or pure brownfield) | True | `InfrastructureReady` | — |

> The managed MariaDB child is provisioned with a minimal-but-valid spec —
> `replicas: 3`, `galera.enabled: true`, `storage.size: 100Gi`
> (`infraMariaDBReplicas` / `infraMariaDBStorageSize`) — mirroring the production
> baseline; the mariadb-operator webhook rejects a CR without a storage size.
> Both values come from the **declared instance**, so a dedicated database can be
> sized (and, with `replicas: 1`, taken off Galera) independently of the shared
> cluster. The Memcached child's `spec.replicas` is likewise taken from the
> declared instance's `cache.replicas` (widened to `int64` for unstructured
> nested-field storage). MariaDB readiness is read via
> `conditions.IsReady(mariadb.Status.Conditions)`; Memcached readiness is read
> from the unstructured `status.conditions[type=Ready].status == "True"`
> (`unstructuredReady`), where a missing/malformed list is treated as not-ready
> rather than an error.

### reconcileESOTenantStore

| Aspect | Value |
| --- | --- |
| File | `reconcile_esotenant.go` |
| Condition | `ESOTenantStoreReady` |
| Gate | none — runs after Infrastructure and before every store-consuming sub-reconciler so the per-tenant store exists before they gate on it |
| Projects / Owns | when `spec.secretStoreRef` is omitted: an owner-referenced `ServiceAccount` (`eso-tenant-auth`), a cert-manager mTLS `Certificate` (`eso-tenant-client-tls`), and a namespaced `SecretStore` (`openbao-tenant-store`), all in `childNamespace(cp)`; when `spec.secretStoreRef` is set the sub-reconciler provisions nothing |
| Requeue | `esoTenantStoreRequeueAfter` = **10s** while the `SecretStore` is not yet Ready |

`reconcileESOTenantStore` provisions the in-cluster half of the ControlPlane's
per-tenant OpenBao identity and makes it the **enforced default**: every
ControlPlane that omits `spec.secretStoreRef` routes its (and its children's)
secret traffic through the per-tenant `openbao-tenant-store`, so OpenBao's
templated `eso-tenant` policy — not a naming convention — isolates one control
plane's key material from another. The OpenBao server and Kubernetes-auth mount
are read from the **shared** cluster store (the per-tenant store cannot describe
its own bootstrap). An explicit `spec.secretStoreRef` is an override: the
sub-reconciler provisions nothing and reports `ESOTenantStoreReady=True` with
reason `StoreRefOverridden`, and the store-consuming sub-reconcilers gate on the
selected store's own readiness.

| Scenario | Status | Reason |
| --- | --- | --- |
| `spec.secretStoreRef` set (override) | True | `StoreRefOverridden` |
| per-tenant `SecretStore` Ready | True | `ESOTenantStoreReady` |
| per-tenant `SecretStore` not yet Ready | False | `SecretStoreNotReady` |
| provisioning the objects failed | False | `ProvisioningError` |

### reconcileDBCredentials

| Aspect | Value |
| --- | --- |
| File | `reconcile_dbcredentials.go` |
| Condition | `DBCredentialsReady` |
| Gate | none — runs unconditionally, positioned after Infrastructure and before Keystone so the Keystone CR is never projected before the DB-credential Secret exists |
| Projects / Owns | Managed-mode (`spec.infrastructure.database.clusterRef != nil`), effective **Dynamic** unless `credentialsMode: Static`: an owner-referenced `VaultDynamicSecret` generator, `ServiceAccount` (`keystone-db-creds`), mTLS client `Certificate`, and an `ExternalSecret` named `{controlplane.Name}-keystone-db-credentials` (`dbCredentialSecretName`) drawing from the generator, all in `childNamespace(cp)`; brownfield projects nothing |
| Requeue | `dbCredentialsRequeueAfter` = **10s** while the ExternalSecret is not yet Ready |

`reconcileDBCredentials` projects the per-ControlPlane service database
credential so the projected Keystone CR consumes a DB credential scoped to its
own ControlPlane. It mirrors `reconcileAdminCredential`'s wait/condition
handling.

Every decision below is made on the **effective** database
(`effectiveKeystoneDatabase`) — the Keystone service's
[dedicated](./controlplane-crd.md#dedicatedbackingservices) database when it
opted into one, the shared `spec.infrastructure.database` otherwise — so the
credential follows the instance the service actually connects to. A **dedicated**
managed database always takes the **Static** branch: the OpenBao database engine
carries one connection and one role per *namespace*, bootstrapped against the
shared cluster, so no engine role can issue credentials for a dedicated instance.
The validating webhook rejects an explicit `credentialsMode: Dynamic` there, and
keying the reconciler's decision on the dedicated *declaration* (not only on the
stored mode) makes a webhook-bypassed CR fail closed onto Static rather than
project a generator that could never sync.

The database is **managed** when the effective `clusterRef` is set and
**brownfield** when the user supplies a `host`-based connection:

- **Brownfield is a pure no-op.** When `clusterRef == nil` the user owns the DB
  credential Secret out-of-band, so the operator projects **no** ExternalSecret
  and never references OpenBao or the selected secret store; `DBCredentialsReady`
  is reported `True` immediately so the chain proceeds to Keystone.
- **Managed defaults to Dynamic (engine-issued).** After gating (via
  `secrets.IsStoreRefReady`) on the store the ControlPlane selected through
  `spec.secretStoreRef` — a `ClusterSecretStore` (default `openbao-cluster-store`)
  or a namespaced `SecretStore` resolved in `childNamespace(cp)` — the operator
  projects (all owner-referenced): a `keystone-db-creds` `ServiceAccount`, an mTLS client
  `Certificate` from the cluster-scoped `openbao-ca-issuer`, a
  `generators.external-secrets.io/v1alpha1` `VaultDynamicSecret` reading
  `database/mariadb/creds/keystone-{cp.Namespace}`
  (`dbDynamicCredsPathFor`, keyed on the namespace alone), and an `ExternalSecret`
  (`RefreshInterval` 24h, `Target.CreationPolicy: Owner`) drawing from that generator via
  `dataFrom.sourceRef.generatorRef` — **no** static `Data` refs and **no**
  `SecretStoreRef`. The generator's OpenBao server URL and Kubernetes-auth mount
  are copied from the selected store's Vault provider by `openBaoConnection`
  (falling back to the documented defaults when unreadable), so the generator
  cannot drift from the store the rest of the stack uses. All Secret references
  are same-namespace (the generator is Namespaced), satisfying the OpenBao
  listener's require-and-verify-client-cert gate. The materialised Secret carries
  an engine-issued username and password with a finite lease, so no long-lived
  static DB password remains at rest.
- **Managed Static is the opt-out** — and the only mode a **dedicated** managed
  database has. The operator projects the stage-(a) KV-backed `ExternalSecret`
  (`SecretStoreRef` the selected store — default `openbao-cluster-store`, built via
  `secrets.ESOSecretStoreRef` — with `username`/`password` `Data` reading
  `openstack/keystone/{cp.Namespace}/{cp.Name}/db`) and tears down any leftover
  dynamic-mode objects.

  > **That KV path is seeded by neither the operator nor the bootstrap.** The
  > per-ControlPlane static seed was retired when managed mode moved to
  > engine-issued credentials, so a Static ControlPlane — the explicit opt-out on
  > the shared database, and *every dedicated managed database* — reaches Ready
  > only once the path has been seeded (`username`, `password`) out-of-band; see
  > [Migrate the Keystone DB to dynamic credentials](../../guides/migrate-keystone-db-to-dynamic-credentials.md).
  > Until then `DBCredentialsReady` stays `False` with reason
  > `WaitingForDBCredentialSecret`, and the message names the exact path to seed.

`reconcileKeystone` projects the effective mode onto the Keystone CR's
`spec.database.credentialsMode`, so the Keystone operator consumes the matching
credential shape.

In **External** keystone mode the ControlPlane manages no database at all —
neither a managed one to issue credentials for, nor a brownfield connection to
reference — so neither OpenBao nor any secret store is consulted.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| External keystone mode | True | `ExternallyManaged` | no database is managed; nothing is projected and no secret store is read |
| Brownfield (`clusterRef == nil`) | True | `BrownfieldUserSuppliedCredential` | no ExternalSecret projected; user supplies the DB credential Secret out-of-band |
| Selected secret store not Ready | False | `SecretStoreNotReady` | requeue 10s; managed mode only, checked before projection so an OpenBao/ESO outage surfaces promptly. The message names the store's kind and name |
| Dynamic generator/SA/Certificate/ExternalSecret create/update fails | False | `GeneratorError` | returns the error |
| Static ExternalSecret create/update fails | False | `ExternalSecretError` | returns the error |
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
  ExternalSecret and never references OpenBao or the selected secret store;
  `AdminPasswordReady` is reported `True` immediately so the chain proceeds to
  Keystone.
- **Managed projects the ExternalSecret.** The owned ExternalSecret has
  `RefreshInterval` 1h, its `SecretStoreRef` built from the ControlPlane's
  `spec.secretStoreRef` via `secrets.ESOSecretStoreRef` (default
  `Kind: ClusterSecretStore, Name: openbao-cluster-store`; a namespaced
  `SecretStore` when selected),
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
  applies the ExternalSecret via Server-Side Apply under the shared field manager
  (`forge-operator`), which stamps the ControlPlane controller reference for GC.

The managed-mode effective admin-password ref (`effectiveAdminPasswordSecretRef`)
points the projected Keystone child's `spec.bootstrap.adminPasswordSecretRef` at
this materialised Secret's `password` key
(`{controlplane.Name}-keystone-admin-credentials`); in brownfield mode it stays
the user-declared `spec.korc.adminCredential.passwordSecretRef` verbatim. The
cp-level spec default for `passwordSecretRef` remains `keystone-admin`.

`effectiveAdminPasswordSecretRef` keys its External branch on
`spec.services.keystone.mode`, not on the database shape, and returns
`spec.korc.adminCredential.passwordSecretRef` verbatim. That Secret is the admin
password source: the operator only ever **reads** it, because the external
Keystone's admin password is owned out-of-band. Its SHA-256 feeds
`adminPasswordHashAnnotation`, so **updating that Secret is what drives a
hash-driven re-mint** of the admin application credential — the only supported
rotation path in External mode. The field indexer
(`controlPlaneSecretNameExtractor`) follows the same ref, so an edit to the
user's Secret wakes the ControlPlane immediately.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| External keystone mode | True | `ExternallyManaged` | the admin password is read from the user-supplied `passwordSecretRef` Secret; no ExternalSecret is projected and no OpenBao bootstrap path is seeded |
| Brownfield (`clusterRef == nil`) | True | `BrownfieldUserSuppliedCredential` | no ExternalSecret projected; user supplies the admin-password Secret out-of-band |
| Selected secret store not Ready | False | `SecretStoreNotReady` | requeue 10s; managed mode only, checked before the ExternalSecret is projected. The message names the store's kind and name |
| ExternalSecret create/update or read fails | False | `ExternalSecretError` | returns the error |
| ExternalSecret not yet synced | False | `WaitingForAdminPasswordSecret` | requeue 10s |
| ExternalSecret Ready | True | `AdminPasswordReady` | — |

### Identity-backend watch

`KeystoneIdentityBackend` CRs are authored by the operator, not projected by the
ControlPlane, so they carry no ControlPlane owner reference an `Owns()` could
match. `SetupWithManager` therefore registers a
`Watches(&KeystoneIdentityBackend{}, ...)` bound to
`identityBackendToControlPlaneMapper`, which enqueues the ControlPlane whose
`keystoneName(cp)` equals the backend's `keystoneRef.name`. A backend attached
to a hand-rolled Keystone beside a ControlPlane therefore never wakes it.

`listIdentityBackends` resolves a ControlPlane's backends with a cache-backed
`List` in `childNamespace(cp)` and the same `keystoneRef.name` filter applied in
memory — the set holds one backend per identity provider plus one per LDAP
domain. Backends carrying a `deletionTimestamp` are dropped: a backend's own
`reconcileDelete` never demotes `Ready` while it waits for de-projection, so a
Terminating backend would otherwise keep offering an SSO choice whose
Keystone-side federation objects are being torn down — and would collide with the
same-named replacement its webhook admits during teardown.

RBAC is **read-only** (`get;list;watch`) in both the kubebuilder marker and the
shared Helm rules helper: the reconciler never writes a backend.

Attaching, detaching, or a backend reaching `Ready` re-projects the Horizon
websso choices and the Keystone `trusted_dashboard` immediately, without waiting
for a periodic resync.

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
- **Database / Cache:** `keystone.Spec.Database` and `keystone.Spec.Cache` are
  DeepCopies of the **effective** instances — the service's
  [dedicated](./controlplane-crd.md#dedicatedbackingservices) database/cache when
  it opted into one (`effectiveKeystoneDatabase` / `effectiveKeystoneCache`), the
  shared `spec.infrastructure` instance otherwise (the default). Projecting the
  effective spec is what carries the opt-in through the rest of the chain with no
  per-class special-casing: the keystone-operator derives its logical database,
  its MariaDB `User`/`Grant` CRs, and its NetworkPolicy database/cache egress
  rules from `spec.database` / `spec.cache`, so all of them follow the instance
  the service actually talks to.
- **Bootstrap:** the admin-password Secret ref is the effective ref
  (`effectiveAdminPasswordSecretRef`) — in managed mode the operator-projected
  per-CP Secret `{controlplane.Name}-keystone-admin-credentials` (see
  [reconcileAdminPassword](#reconcileadminpassword)), in brownfield mode the
  user-declared `cp.Spec.KORC.AdminCredential.PasswordSecretRef` verbatim (so
  Keystone and K-ORC agree on the admin-password source) — and the region is
  `cp.Spec.Region`.
- **Replicas:** copied from `spec.services.keystone.replicas` when set.
- **Federation:** `spec.federation.proxyImage` is the
  `spec.services.keystone.federationProxyImage` override when set, else
  `ghcr.io/c5c3/keystone-federation-proxy:latest`;
  `spec.federation.trustedDashboards` is the ControlPlane's own dashboard origin
  (`horizonPublicEndpoint(cp) + "/auth/websso/"`), or `nil` when no dashboard is
  externally reachable. Both are assigned unconditionally, so clearing the
  override or the horizon block reverts the child. The origin is derived
  **top-down from `cp.Spec`**, never from the Horizon child's status, so this
  projection carries no ordering dependency on `reconcileHorizon` (which is
  gated on `KeystoneReady` and therefore runs strictly after it). Both fields
  are inert until a federation backend attaches.
- **Policy:** `policy.MergePolicies(cp.Spec.GlobalPolicyOverrides, cp.Spec.Services.Keystone.PolicyOverrides)`
  (the shared `internal/common/policy` helper) merges the global base with
  per-service overrides (per-service wins on conflict).
- **Rotation:** when `spec.services.keystone.rotationInterval` is set,
  `intervalToCron` converts it to a cron schedule applied to **both**
  `Fernet.RotationSchedule` and `CredentialKeys.RotationSchedule`. Only `168h`
  (weekly, `0 0 * * 0`) and positive whole-day multiples (daily, `0 0 * * *`)
  are supported.

In **External** keystone mode no child is projected — and none is deleted. A
`Managed -> External` flip is rejected at admission (adopting an existing
installation must be a fresh External-mode ControlPlane), so no child can exist;
were one to appear anyway, the fail-safe that preserves a child unless
`c5c3.io/allow-keystone-deletion: "true"` opts in is the only sanctioned teardown
path, because the child's credential/fernet keys are irreplaceable.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `spec.services.keystone` unset | True | `KeystoneNotManaged` | no identity plane at all; a previously-projected child is preserved by default |
| External keystone mode | True | `ExternallyManaged` | identity is managed against `external.authURL`; no child is projected and none is deleted |
| `InfrastructureReady` not True | False | `WaitingForInfrastructure` | requeue 5s; no Keystone CR is created while infra is unready |
| Invalid `rotationInterval` | False | `InvalidRotationInterval` | **returns the error** so the reconcile chain stops at Keystone and the manager requeues with backoff (the validating webhook already rejects unrepresentable intervals at admission, so this is defense-in-depth) |
| Keystone create/update fails | False | `KeystoneError` | returns the error |
| Keystone child not yet Ready | False | `WaitingForKeystone` | requeue 15s |
| Keystone child Ready | True | `KeystoneReady` | — |

### reconcileHorizon

| Aspect | Value |
| --- | --- |
| File | `reconcile_horizon.go` |
| Condition | `HorizonReady` |
| Gate | `KeystoneReady == True` |
| Projects / Owns | one `Horizon` child named `{controlplane.Name}-horizon` (`horizonNameSuffix`) in `childNamespace(cp)` — only when `spec.services.horizon` is set |
| Requeue | `keystoneInfraGateRequeueAfter` = **5s** while gated; `infraRequeueAfter` = **15s** while the child is not Ready |

`reconcileHorizon` is optional: `spec.services.horizon` unset means this
ControlPlane manages no dashboard, and the sub-reconciler reports
`HorizonReady=True` / `HorizonNotManaged` so the aggregate is not blocked (staged
adoption). A previously-projected child is **preserved** unless the ControlPlane
opts in with `c5c3.io/allow-horizon-deletion: "true"` (then the orphan is
deleted) — the same annotation UX as Keystone, though the dashboard is stateless.

When managed, the projection mirrors the Keystone one's *thin* discipline,
reusing the ControlPlane's own specs so the dashboard points at the same backing
services:

- **Image:** repository defaults to `ghcr.io/c5c3/horizon` with the tag derived
  from `spec.openStackRelease`; `spec.services.horizon.image` overrides the whole
  image reference when set.
- **Cache:** a DeepCopy of the **effective** cache (`effectiveHorizonCache`) —
  the dashboard's [dedicated](./controlplane-crd.md#dedicatedbackingservices)
  cache when it opted into one, the shared `spec.infrastructure.cache` otherwise
  — with the same `clusterRef` / servers / replicas, **except** that `Backend` is
  overridden to the Horizon Django default
  `django.core.cache.backends.memcached.PyMemcacheCache`. The shared
  `CacheSpec.Backend` carries the oslo.cache dogpile path Keystone consumes
  (`dogpile.cache.pymemcache`), which Django renders verbatim as a `CACHES`
  backend and rejects with `InvalidCacheBackendError`, so the dashboard would
  never go Ready — only the endpoint-bearing fields are reused unchanged.
- **Keystone endpoint:** derived top-down via `horizonKeystoneEndpoint(cp)` from
  the Keystone child's naming convention, not read from the Keystone child's
  status (no machine consumer reads status endpoints, per the settled
  convention). Always the cluster-local Service URL (the same URL K-ORC
  authenticates against) — never the external `publicEndpoint` or gateway
  hostname, which the dashboard pods may not be able to reach: the dashboard's
  Django backend connects to this URL server-side, the browser never does.
- **SecretKeyRef:** defaults to the kind shim Secret `horizon-secret-key` (key
  `secret-key`), which is pinned to the **default** ControlPlane identity;
  `spec.services.horizon.secretKeyRef` overrides it, and a second ControlPlane
  **must** set its own so each dashboard reads distinct `SECRET_KEY` material.
- **Gateway:** a DeepCopy of `spec.services.horizon.gateway`; a nil source clears
  the projected gateway so removing the block tears the HTTPRoute down.
- **Replicas:** `commonv1.DefaultReplicas`, overridden by
  `spec.services.horizon.replicas` when set (assigned unconditionally so clearing
  the field reverts the child to the default instead of pinning a lost update).
- **WebSSO:** projected from the **Ready** OIDC `KeystoneIdentityBackend` CRs
  attached to the Keystone child (see
  [Identity-backend watch](#identity-backend-watch)). One choice per Ready
  backend, keyed `{identityProvider}_{protocol}` (truncated to a digest-suffixed
  64 characters when the two names together exceed the Horizon CRD's bound on
  `choices[].id`), with the local-credentials fallback leading the list and
  preselected; `keystoneURL` is `keystonePublicEndpoint(cp)` — the
  **browser-facing** endpoint, because the browser follows the SSO redirect.
  At most 16 federated choices are projected (`maxProjectedFederationChoices`);
  the excess is dropped and logged rather than rejected by the API server as a
  `choices`/`idpMapping` overflow that would wedge every later Horizon change.
  `nil` when no OIDC backend is **attached**, so a choice never appears for a
  backend whose federation objects are not provisioned yet — and `nil` too when
  the hand-off could not complete anyway: no trusted dashboard origin
  (`trustedDashboards(cp)`) means Keystone bounces the browser *after* the user
  has entered their corporate credentials, and no `keystonePublicEndpoint(cp)`
  means the redirect targets a cluster-local DNS name the browser cannot
  resolve. Both are logged.
- **MultiDomain:** `enabled` with `defaultDomain: Default` once any LDAP backend
  is Ready, so the login form gains a domain field. `domainChoices` /
  `domainDropdown` are deliberately **not** projected: upstream `openstack_auth`
  turns the domain field into a select bounded by
  `OPENSTACK_KEYSTONE_DOMAIN_CHOICES`, and the operator only ever sees the
  LDAP-backed domains — a dropdown built from them would lock out every user of
  a domain it cannot enumerate (a SQL-backed domain populated out-of-band, or
  the domain an OIDC backend targets). `nil` when no LDAP backend is attached.
- **Detached vs. unhealthy.** Detaching the last backend of a type clears its
  block, so the login page reverts to local credentials. A backend that is
  attached but **not Ready** retains the previously-projected block instead
  (`projectWebSSO` / `projectMultiDomain`): a backend's aggregate `Ready` can
  drop on a failed observation while the Keystone-side federation objects it
  provisioned are untouched, so the SSO button keeps working. Rebuilding the
  block from that view would re-render `local_settings.py`, roll the dashboard
  Deployment, and roll it back on recovery — twice, for a login page that was
  never broken. The retention is logged.

A failure to list the backends surfaces as `HorizonReady=False` with reason
`IdentityBackendsUnavailable` and returns the error so the chain stops — never
an empty websso block, which would silently remove a working SSO button.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `spec.services.horizon` unset | True | `HorizonNotManaged` | staged adoption — does not block the aggregate; a previously-projected child is preserved unless `c5c3.io/allow-horizon-deletion: "true"` is set |
| `KeystoneReady` not True | False | `WaitingForKeystone` | requeue 5s; no Horizon CR is projected while Keystone is unready |
| Horizon create/update fails | False | `HorizonError` | returns the error |
| Projected spec rejected (HTTP 422 Invalid) | False | `HorizonProjectionRejected` | returns the error; the projection violates a Horizon CRD/webhook rule — reconcile the ControlPlane spec to a valid projection to recover |
| Horizon child not yet Ready | False | `WaitingForHorizon` | requeue 15s |
| Horizon child Ready | True | `HorizonReady` | — |

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
- **Mode-aware `clouds.yaml` (`korc_cloudsyaml.go`).** Both builders render
  `auth_url`, `endpoint_type` and `region_name` through three resolvers.
  `korcAuthURL` returns the in-cluster Keystone Service DNS in managed mode and
  `spec.services.keystone.external.authURL` in External mode; `korcEndpointType`
  returns `internal` in managed mode (K-ORC runs in-cluster, so `public` would
  resolve to an unreachable Gateway host) and the configured `endpointType`
  (default `public`) in External mode; `korcRegion` returns `spec.region`
  (default `RegionOne`). Managed-mode output is byte-identical to before, pinned
  by golden tests, so no upgraded ControlPlane churns its Secrets. The key must
  be `endpoint_type`, never `interface` — K-ORC drops gophercloud's `Interface`
  field (the authoritative note lives on `buildAppCredCloudsYAML`).
- **Admin identities.** `buildPasswordCloudsYAML` renders `username`,
  `project_name` and both domain keys from
  `spec.korc.adminCredential.userName`/`.projectName`/`.domainName`, and
  `ensureKORCAdminImports` uses the same `userName`/`domainName` as the `User`
  and `Domain` import filters. **Same-user constraint:** Keystone's default policy
  mints an application credential only for the token's own user, so the
  `clouds.yaml` `username` and the imported `User` (the AC's `UserRef`) must be the
  same user — both derive from `adminUserName`.
- **UserRef.** The required K-ORC `UserRef` points at the deterministic,
  `cp.Name`-scoped `User` CR `{controlplane.Name}-user-admin`, imported as
  unmanaged by `ensureKORCAdminImports`. The CR name is a stable handle; the
  OpenStack user it resolves to comes from the import filter above.
- **CA bundle (`cacert`).** When `spec.services.keystone.external.caBundleSecretRef`
  is set, the referenced bundle is read from the ControlPlane namespace and
  projected **verbatim** as the inline `cacert` key into **both** operator-owned
  credentials Secrets (`setCACertKey`). K-ORC reads that key natively from the
  same Secret as `clouds.yaml`, so there is no mount and no upstream change. The
  password-cloud is what the AC authenticates with directly; the app-credential
  Secret is the PushSecret's whole-Secret source, so the bundle also reaches
  OpenBao and is read back by the `cacert` entry
  `ensureKORCCloudsYAMLExternalSecret` adds — gated on the **resolved bundle**, the
  same predicate `setCACertKey` writes the source key under, so the read-back can
  never point at a property the PushSecret did not push. Clearing the ref deletes
  the key and drops the read-back entry. A missing Secret/key — or a present-but-
  empty key, the transient of a two-step "create then populate" flow — defers the
  mint (`WaitingForCABundle`); see the [CA-cache aliasing
  caveat](#egress-and-tls-posture).
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
  agree on one derivation. The annotation is (re-)stamped **only on a fresh mint or
  when it is absent**, never overwriting a present-but-empty value — that empty value
  is the CredentialRotation reconciler's nudge marker, so preserving it keeps a
  concurrently-cleared nudge from being silently lost (`shouldStampPasswordHash`).
- **Re-mint on immutable resource-block drift.** K-ORC declares the AC's whole
  `spec.resource` block immutable via CEL (`self == oldSelf`), so a legal, webhook-
  admitted change to `restricted` or `accessRules` cannot be reconciled by an
  in-place update — it would be rejected on every pass. `reconcileKORC` detects drift
  on the operator-managed fields (`Unrestricted`, `UserRef`, `SecretRef`,
  `AccessRules` — never the whole struct, so a K-ORC/CRD-defaulted sub-field can never
  read as permanent drift) and routes it through the **same** delete+recreate re-mint
  (`adminACResourceDrifted`).
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
| External CA bundle Secret/key missing or empty | False | `WaitingForCABundle` | requeue 10s; no credentials Secret is written before the endpoint can be verified |
| External CA bundle read fails otherwise | False | `CABundleError` | returns the error |
| Password-cloud ensure fails | False | `PasswordCloudError` | returns the error |
| Hash mismatch → AC deleted for re-mint | False | `ReMinting` | requeue 10s; AC deleted + `value` regenerated, recreated next pass |
| `spec.resource` drift (`restricted`/`accessRules`) → AC deleted for re-mint | False | `ReMinting` | requeue 10s; the block is CEL-immutable, so the change forces a delete+recreate |
| Re-mint stuck `Terminating` past `remintStallTimeout` (5m) | False | `ReMintStalled` | requeue 10s; finalizer cannot revoke the old credential |
| AC create/update/delete/read fails otherwise | False | `ApplicationCredentialError` | returns the error |
| `value` regeneration fails | False | `SecretError` | returns the error |
| AC reports a terminal K-ORC error | False | `ApplicationCredentialFailed` | requeue 10s; gated on `orcv1alpha1.GetTerminalError(ac)` (an unrecoverable/invalid-config Progressing reason, e.g. K-ORC cannot authenticate with the clouds.yaml) so a credential that will never converge is not reported as an eternal wait |
| AC not yet `Available` | False | `WaitingForApplicationCredential` | requeue 10s; gated on `orcv1alpha1.IsAvailable(ac)` (K-ORC uses `Available`, not `Ready`) |
| AC minted and Available | True | `ApplicationCredentialMinted` | — |

Both the `ApplicationCredentialFailed` and `WaitingForApplicationCredential`
messages fold in the admin Domain/User import status (`ensureKORCAdminImports`
returns the admin Domain/User imports, and `korcAdminImports.statusFragment()`
names the first that is terminally failed or not yet Available), so the
documented endpoint/clouds.yaml failure class — where K-ORC swallows a list error
and an import hangs on "created externally" — names the stuck dependency instead of
surfacing as an opaque wait.

#### External-mode failure classification

K-ORC collapses **every** hard failure against a pre-existing Keystone — a wrong
admin password (401), an unresolvable `authURL`, an untrusted private CA, a
region/`endpointType` absent from the catalog — into the same **non-terminal**
`Progressing` condition with `reason=TransientError`. Nothing in the observed
inventory is terminal, so neither `GetTerminalError` nor the reason
discriminates: the failure class survives only in the free-text message.

In External mode the ControlPlane therefore classifies on message substrings and
relays K-ORC's message **verbatim** alongside the reason. Classification walks
the admin Domain, then the admin User, then the ApplicationCredential, so the
*root* stuck dependency is reported rather than the resource that merely blocked
on it. Precedence, most specific first: catalog mismatch, credential drift, TLS,
authentication, reachability.

It is gated on External mode — a managed ControlPlane's `KORCReady` reasons are
byte-identical to before — and never runs against an `Available` credential: K-ORC
leaves the message of the last transient attempt on the `Progressing` condition,
and re-classifying it would flip a converged ControlPlane back to
`AuthenticationFailed` on a failure it has already recovered from.

| Path (External mode only) | Status | Reason | Notes |
| --- | --- | --- | --- |
| message contains `401` / `Unauthorized` | False | `AuthenticationFailed` | the external Keystone rejected the admin credential — typically the password was rotated out-of-band and `passwordSecretRef` is stale |
| message contains `no such host` / `connection refused` / `dial tcp` / `i/o timeout` | False | `EndpointUnreachable` | `external.authURL` could not be dialled |
| message contains `x509` | False | `TLSVerificationFailed` | supply the private CA via `external.caBundleSecretRef` |
| message contains `No suitable endpoint could be found in the service catalog` | False | `CatalogEndpointMismatch` | a wrong `external.endpointType` or `spec.region`; fails loud rather than silently importing nothing. gophercloud never names the interface or region it looked for, so the message appends the **effective** `endpointType` and `spec.region` as the values the external catalog must publish |
| message names `identity:create_application_credential` (403) | False | `CredentialDrift` | an application-credential create against a **stale** resolve-once import id; see [drift](#drift-is-surfaced-never-fought) |
| admin import stuck on "Waiting for OpenStack resource to be created externally" beyond `externalImportStallGrace` | False | `ImportStalled` | the silent-empty detector |

`externalImportStallGrace` is **2 minutes**. In External mode every import target
pre-exists by definition, so an import that keeps waiting to be "created
externally" never resolves on its own — K-ORC is looking in the wrong place. The
message names the stuck import and points at `external.endpointType` and
`spec.region`. The window is deliberately shorter than `remintStallTimeout` /
`orcTeardownStallTimeout` (both 5m): those wait on work that is genuinely in
flight, whereas a resolvable import has nothing to wait for.

#### Drift is surfaced, never fought

The external installation can change under the CR: the admin password is rotated
without updating the referenced Secret, or the admin user is deleted and
recreated. K-ORC imports are **resolve-once** — a resolved `status.id` is never
re-resolved — so after a recreate the import stays `Available=True` with a stale
id while an application-credential create against that id yields a Keystone
**403** (`identity:create_application_credential`; Keystone's default policy
allows creating an application credential only for the token's own user).

The operator **never remediates the external installation**. It signals drift on
the existing sub-conditions with the documented `CredentialDrift` reason, and
`reconcileKORC` emits a `Warning` `CredentialDrift` event on the **transition**
into the drifted state (not on every 10s requeue). The remedy is to update the
Secret `spec.korc.adminCredential.passwordSecretRef` names, which changes its
digest and drives a hash-driven re-mint.

> **Hard CRD dependency.** K-ORC (like Memcached, ESO, MariaDB, and Keystone) is
> a hard dependency: `SetupWithManager` `Owns`/`Watches` its kinds, so the
> manager fails fast at startup if any CRD is absent. A missing K-ORC CRD never
> reaches the reconcile path, so there is no dedicated CRD-not-installed
> condition; a no-match error that could only occur if a CRD were deleted after
> startup propagates as a hard error (`ApplicationCredentialError` /
> `ServiceError`) and the manager requeues with backoff.

### reconcileAdminCredential

| Aspect | Value |
| --- | --- |
| File | `reconcile_admincredential.go` |
| Condition | `AdminCredentialReady` |
| Gate | `KORCReady == True`, the store selected via `spec.secretStoreRef` (default the OpenBao-backed cluster store `openbao-cluster-store`) is Ready, the K-ORC clouds.yaml `ExternalSecret` (`{childNamespace(cp)}/{CloudCredentialsRef.SecretName}`, co-located with the K-ORC CRs per C1) is Ready, the admin app-credential `PushSecret` has actually synced to OpenBao (its `Ready` condition is True), **and** the materialised clouds.yaml Secret semantically matches (parsed application-credential id+secret) the freshly assembled credential |
| Owns | the operator-owned `Secret` `{controlplane.Name}-admin-app-credential` and the `PushSecret` `{controlplane.Name}-admin-app-credential-backup`, both in `childNamespace(cp)` |
| Requeue | `korcRequeueAfter` = **10s** while any gate is unmet (including a stale/absent materialised clouds.yaml) |

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
- **PushSecret to OpenBao.** `secrets.EnsurePushSecret` (applied via server-side
  apply under a fixed field manager that owns only the fields the operator sets,
  so repeated applies of an unchanged desired spec are no-ops at the API server)
  builds the PushSecret to the selected store (default `openbao-cluster-store`;
  its store ref comes from `spec.secretStoreRef` via `secrets.PushSecretStoreRefs`,
  and switching the ref moves the push in place — unchanged name and remote key) at
  the per-ControlPlane remote
  key `openstack/keystone/{cp.Namespace}/{cp.Name}/admin/app-credential`
  (`adminAppCredentialRemoteKeyFor`) with **`DeletionPolicy: None`** — the
  admin credential is a per-ControlPlane persistent bootstrap secret, so deleting
  the PushSecret on ControlPlane teardown (or when rotation is disabled) leaves the
  last-pushed credential intact in OpenBao at that CR's own path, so re-adoption
  works and the admin is never locked out.
- **Forced re-push on credential change.** ESO's PushSecret controller does
  **not** watch its source Secret: its refresh gate reacts only to the PushSecret
  object's own label/annotation hash, so a source-Secret update — e.g. the
  fresh-create handoff from the password-based bootstrap clouds.yaml to the
  minted credential — would otherwise not reach OpenBao until the hourly
  `refreshInterval`, leaving `AdminCredentialReady` stuck False for up to an
  hour. `forceRepushAdminAppCredential` therefore stamps the assembled
  clouds.yaml content hash onto the PushSecret (`c5c3.io/push-content-hash`,
  read-modify-write under `RetryOnConflict`), changing its metadata hash and
  forcing an immediate re-push. The annotation is keyed by the content hash, so
  it fires exactly once per credential change and a steady-state pass is a
  no-op.
- **PushSecret sync gate.** `AdminCredentialReady` is gated on the PushSecret's
  `Ready` condition — not merely on the CR existing — so a backend permission
  failure (e.g. the ESO role missing the push policy) surfaces as
  `WaitingForPushSecret` instead of a false-positive Ready while OpenBao still
  serves the password-based bootstrap clouds.yaml.
- **Live clouds.yaml gate (stale-credential window).** A re-mint revokes the old
  credential immediately, but the `k-orc-clouds-yaml` Secret only refreshes from
  OpenBao at the ExternalSecret's hourly `refreshInterval`, so the PushSecret-Ready
  check above can pass while the materialised Secret K-ORC actually authenticates
  with still holds the revoked credential. After assembling the clouds.yaml,
  `reconcileAdminCredential` stamps the `external-secrets.io/force-sync` annotation
  to nudge ESO to re-materialise immediately. The trigger value is
  `contentHash + "/" + PushSecret.status.syncedResourceVersion`, **not** the
  content hash alone: both nudges (the PushSecret re-push above and this
  force-sync) are stamped in the same reconcile pass, but ESO processes the two
  objects independently — keyed on the content hash alone, the ExternalSecret
  refresh can read OpenBao *before* the re-push has written it, re-materialise
  the stale bootstrap document, and (with the annotation already at its final
  value) never be nudged again, wedging `AdminCredentialReady` at
  `WaitingForCloudsYamlSync` until the hourly refresh. ESO bumps
  `syncedResourceVersion` only *after* a completed push, so folding it in
  re-nudges the ExternalSecret exactly once more as soon as the re-push lands;
  both inputs are stable once converged, so a steady-state pass still leaves the
  ExternalSecret untouched. It then **compares the
  materialised Secret semantically** — by the parsed application-credential id and
  secret, not byte-for-byte, so a benign ESO/OpenBao re-serialisation (a stripped
  trailing newline, reordered keys, requoting) cannot wedge the gate permanently —
  and only reports `AdminCredentialReady=True` when they match. The semantic
  compare — not the best-effort force-sync — is the correctness guarantee: the
  condition never reads True against a stale credential. A sync that never converges
  is bounded: once the materialised Secret has failed to match for longer than
  `cloudsYamlSyncStuckTimeout` (measured from the credential's `LastRotation`), the
  reason escalates from the transient `WaitingForCloudsYamlSync` to the alertable
  `CloudsYamlSyncStuck`, so a permanently broken sync is distinguishable from a
  2-second transient miss.
- **Drift escalation (External mode).** When the `KORCReady` gate is closed
  *because* the external Keystone reports drift, the gate reports
  `CredentialDrift` rather than the opaque `WaitingForKORC`. Note that the **live**
  drift signal is `KORCReady` itself: `reconcileKORC` requeues on the drift and
  `RunPipeline` short-circuits on that non-zero result, so this sub-reconciler is
  not re-entered in the same pass. The escalation is the defense-in-depth path
  that keeps `AdminCredentialReady` honest for any caller that does observe a
  drifted `KORCReady`. An unreachable endpoint, a TLS failure, a catalog mismatch
  or a stalled import are **not** drift and keep `WaitingForKORC` — drift is a
  statement about the credential.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `KORCReady` not True | False | `WaitingForKORC` | requeue 10s |
| `KORCReady` reports drift (`AuthenticationFailed` / `CredentialDrift`), External mode | False | `CredentialDrift` | requeue 10s; names the Secret the operator reads and states that the external installation is never remediated |
| Selected secret store not Ready | False | `SecretStoreNotReady` | requeue 10s; checked after the `KORCReady` gate so an OpenBao/ESO outage surfaces before the clouds.yaml wait. The message names the store's kind and name |
| clouds.yaml ES check errors | False | `CloudsYamlError` | returns the error (also covers a force-sync/materialised-Secret read error) |
| clouds.yaml ES not Ready | False | `WaitingForCloudsYaml` | requeue 10s |
| operator Secret ensure fails | False | `SecretError` | returns the error |
| PushSecret ensure, re-push stamp, or readback fails | False | `PushSecretError` | returns the error |
| PushSecret not yet synced to OpenBao | False | `WaitingForPushSecret` | requeue 10s; the PushSecret's `Ready` condition is not True — the assembled credential has not landed in OpenBao yet |
| materialised clouds.yaml absent or semantically stale | False | `WaitingForCloudsYamlSync` | requeue 10s; force-sync annotation stamped, semantic compare (parsed id+secret) against the assembled document not yet satisfied |
| materialised clouds.yaml stuck stale past `cloudsYamlSyncStuckTimeout` | False | `CloudsYamlSyncStuck` | requeue 10s; the sync has not converged since `LastRotation` — alertable, distinguishable from a transient miss |
| committed, mirrored, and materialised | True | `AdminCredentialReady` | — |

### reconcileCatalog

| Aspect | Value |
| --- | --- |
| File | `reconcile_catalog.go` (Managed), `reconcile_catalog_external.go` (External) |
| Condition | `CatalogReady` |
| Gate | `AdminCredentialReady == True`, **and** every catalog child reports `Available` |
| Owns | Managed mode: a K-ORC identity `Service` (`{controlplane.Name}-identity-service`) and its public `Endpoint` (`{controlplane.Name}-identity-endpoint`). External mode: the same `Service` plus one `Endpoint` per interface (`{controlplane.Name}-identity-endpoint-{interface}`), all unmanaged imports, plus one managed `Service`/`Endpoint` set per declared entry (`{controlplane.Name}-catalog-{type}[-{interface}]`). All in `childNamespace(cp)` |
| Requeue | `korcRequeueAfter` = **10s** while gated, while a child is not yet Available, or on a terminal K-ORC failure |

`reconcileCatalog` drives `CatalogReady`. Everything up to and including the
`AdminCredentialReady` gate and the admin `CloudCredentialsReference` is
mode-agnostic; below it the two postures are opposites, so the reconciler forks
on `cp.IsExternalKeystone()`. K-ORC is a hard CRD dependency (see the note
above), so a missing Service/Endpoint CRD never reaches this path and there is no
CRD-not-installed condition.

#### Managed mode — the control plane owns the catalog

`reconcileCatalog` registers the OpenStack service-catalog entries as owned K-ORC
CRs, driven from a per-service table (`managedCatalogRows`) so a second service is
a table row rather than a copied literal. The only entry today is the identity
(Keystone) service: an `identity`-type `Service` named `keystone`, plus a
`public` `Endpoint` whose URL defaults to the conventional in-cluster identity URL
`http://keystone.<namespace>.svc:5000/v3` and whose `serviceRef` points at the
identity Service. Every child is projected idempotently via Server-Side Apply
under the shared field manager (`forge-operator`).

Registering the child CRs only instructs K-ORC to create the catalog entries — it
does not mean they exist in Keystone — so `CatalogReady` is gated on both children
reporting `Available` for their current generation (`korcAvailableUpToDate`, which
refuses a stale `Available` condition whose `ObservedGeneration` lags the object —
the same generation gate `GetTerminalError` already applies via its `Progressing`
check — so an endpoint/region edit that moves the catalog URL cannot flip
`CatalogReady` True before K-ORC re-reconciles the new value), and a terminal K-ORC
failure (`GetTerminalError`, the documented wrong-endpoint / import-stuck class) is
surfaced as the distinct `CatalogFailed` reason instead of a false-positive Ready.

| Path | Status | Reason | Notes |
| --- | --- | --- | --- |
| `AdminCredentialReady` not True | False | `WaitingForAdminCredential` | requeue 10s |
| Service create/update fails | False | `ServiceError` | returns the error |
| Endpoint create/update fails | False | `EndpointError` | returns the error |
| a catalog entry's Service/Endpoint reports a terminal K-ORC error | False | `CatalogFailed` | requeue 10s (Service before its Endpoints, so the root stuck dependency surfaces) |
| a catalog entry's Service/Endpoint registered but not yet Available | False | `WaitingForCatalog` | requeue 10s |
| every catalog entry registered and Available | True | `CatalogRegistered` | message counts the registered entries; identity is the only entry today |

#### External mode — import-first

The catalog belongs to the pre-existing installation. Keystone enforces no
uniqueness on service names, so registering an identity `Service` against a
populated catalog would silently duplicate rows. `reconcileCatalogExternal`
therefore **imports** instead: the identity `Service` and each of its three
endpoint interfaces become K-ORC CRs with `managementPolicy: unmanaged`, an
import filter, and no desired resource. K-ORC resolves them read-only and writes
nothing; deleting their CRs later removes only the Kubernetes objects.

The default posture creates **zero** catalog entries. Creation survives only as
the `spec.services.keystone.external.catalog.managedEntries` opt-in, projected as
managed `Service`/`Endpoint` CRs authenticating through the operator-owned
`{name}-admin-password-cloud` Secret (see [the deletion resource
set](#external-mode-deletion-resource-set) for why they cannot use the spec's
`cloudCredentialsRef`). On every pass the reconciler also sweeps the
entry CRs it owns that the spec no longer declares, so removing a declaration
deletes exactly that entry. The sweep matches on **both** the controller
reference and the `{controlplane.Name}-catalog-` name prefix, so it can never
catch the unmanaged imports or a CR belonging to somebody else.

**Removal gates readiness exactly as registration does.** A pruned CR stays
`Terminating` behind its `openstack.k-orc.cloud/*` finalizer until K-ORC has taken
the row out of the external catalog, so until then the ControlPlane still owns that
row. `CatalogReady` therefore reports `WaitingForCatalog` naming the CR being
removed, and `CatalogFailed` when K-ORC gives up on the `DELETE`. A fire-and-forget
prune would report `CatalogReady=True` over a live row — and hand the stuck CR to
the [teardown stall escape](#external-mode-deletion-resource-set), which orphans it.
The same reasoning gates a **re-declared** entry whose earlier removal is still in
flight: `controllerutil.CreateOrUpdate` finds the `Terminating` CR, projects a
byte-identical spec and updates nothing, so no generation bump invalidates the
`Available=True` K-ORC left on it. `korcAvailableUpToDate` is generation-aware, not
deletion-aware, so `deletionTimestamp` is checked separately.

`status.catalog.imports` is rebuilt before any failure return, so an unresolved
import is visible as `resolved: false` rather than omitted.

**Import-first inverts the failure modes, and detecting them is the point.** A
K-ORC import that matches **nothing** does not error: it waits indefinitely on
`Available=False`, `reason=Progressing`, *"Waiting for OpenStack resource to be
created externally"* — by conditions indistinguishable from a resource that is
about to appear. For a **gating** import the target pre-exists **by definition**,
so past `externalImportStallGrace` (2m) that wait is a misconfiguration signal,
not a wait. An import that matches **several** entries is terminal in K-ORC
itself, which refuses to guess and stops retrying.

Only two of the four imports gate `CatalogReady`: the identity `Service`, and the
`Endpoint` of the interface `external.endpointType` selects. The control plane
already authenticates through that interface, so a catalog that does not publish
it is not the catalog K-ORC was pointed at. The other two interfaces are imported
for visibility, projected into `status.catalog.imports`, and may stall forever —
or resolve ambiguously — without failing the condition. An external installation
is free not to publish an interface, and free to publish it once per region (which
K-ORC's region-less `EndpointFilter` cannot select among, so no spec edit repairs
it); both are precisely the brownfield posture External mode adopts.

The precedence below reports the most specific cause first, and the `Service`
before the `Endpoint`s so the **root** stuck dependency is named rather than an
endpoint merely blocked on the service it references:

| # | Path | Status | Reason | Notes |
| --- | --- | --- | --- | --- |
| — | `AdminCredentialReady` not True | False | `WaitingForAdminCredential` | requeue 10s; no import CR is reconciled |
| — | import create/update fails | False | `ImportError` | returns the error |
| — | managed entry create/update or sweep fails | False | `CatalogEntryError` | returns the error |
| 1 | an **unresolved** import, entry or removal carries a classifiable K-ORC message | False | `AuthenticationFailed` \| `EndpointUnreachable` \| `TLSVerificationFailed` \| `CatalogEndpointMismatch` \| `CredentialDrift` | requeue 10s; K-ORC's message is relayed verbatim. The write path is classified alongside the imports: nothing K-ORC reports on a managed entry is terminal, so without this every realistic entry failure would fall through to the unbounded wait of row 5. A resolved import is never re-classified — K-ORC leaves the last transient attempt's message on `Progressing`, and classifying it would flip a converged catalog to a failure it has recovered from |
| 2 | an import reports a terminal K-ORC error | False | `CatalogFailed` | requeue 10s; gating or not — K-ORC has given up on it. On the **>1-match** message the hint names `external.catalog.identityServiceName` for the `Service` import, or the region limitation for an `Endpoint` import (K-ORC's `EndpointFilter` carries no region, so no spec field can select among per-region rows). **One exception:** an `InvalidConfiguration` on a **non-gating** import does not fail the condition. A non-gating import has no user-supplied configuration to fix — its filter is entirely operator-derived — so it has no remediation and nothing depends on it; it is tolerated exactly like the 0-match of row 3 and reported as `resolved: false`. The exception is keyed on K-ORC's machine-readable reason, never on the >1-match message text: keying it on the text would turn a K-ORC rewording into a permanent `CatalogReady=False`. An `UnrecoverableError` gates on every import, and so does any terminal error on a gating one |
| 3 | a **gating** import stalled past `externalImportStallGrace` | False | `ImportStalled` | requeue 10s; the **0-match** case. The message names the stuck import, the `authURL`, and `external.endpointType` / `spec.region` — plus, for an `Endpoint` import, that the external catalog may publish no such interface |
| 4 | a declared managed entry, or one being removed, reports a terminal K-ORC error | False | `CatalogFailed` | requeue 10s |
| 5 | a **gating** import is unresolved, a declared entry is not yet Available or is still `Terminating` from an earlier removal, or a removal has not completed | False | `WaitingForCatalog` | requeue 10s; the bounded, legitimate wait. For an entry the message appends K-ORC's own — a policy denial (HTTP 403 on `POST /v3/services`, what a domain-admin adoption hits) is non-terminal and unclassifiable, so this is the only place it surfaces |
| 6 | every gating import resolved, every declared entry Available, every removal complete | True | `CatalogImported` | the message reports how many of the three endpoint interfaces resolved |

`publicEndpoint` is forbidden in External mode, so `keystoneCatalogURL` — the URL
the Managed branch registers — is never consulted here: advertisement visibility
is owned by the imports.

> **Promote-to-managed is reserved, not implemented.** Turning an import into a
> managed entry (to edit its endpoint URL declaratively) is a later phase.
> K-ORC's `managementPolicy` is CEL-immutable, so it will have to be a
> delete-and-recreate of the import CR. Nothing in the deterministic CR names or
> the spec-derived filters chosen here precludes that.

### reconcileServiceAccounts

| Aspect | Value |
| --- | --- |
| File | `reconcile_serviceaccounts.go` |
| Condition | `ServiceAccountsReady` |
| Gate | `AdminCredentialReady == True` **and** the store selected via `spec.secretStoreRef` (default the OpenBao-backed cluster store `openbao-cluster-store`) is Ready (`secrets.IsStoreRefReady`) |
| Requeue | `korcRequeueAfter` = **10s** while a gate is closed or an account is converging |

`reconcileServiceAccounts` projects each `spec.korc.serviceAccounts` entry onto a
managed K-ORC `User` and `Project` with an operator-generated, OpenBao-backed,
rotatable password. It is **mode-independent**: the same rules apply against a
managed in-cluster Keystone and an external one.

Per declared entry it, in order:

1. **Domain handle** — reuses the admin `Domain` import when the effective domain
   matches the admin domain, else creates a per-account unmanaged `Domain` import.
2. **Project** — `project.create: false` is an unmanaged import (referenced, never
   created or deleted); `project.create: true` is a **probe-gated** managed
   `Project`.
3. **User collision gate** — K-ORC's managed create **silently adopts** a same-name
   resource, so a short-lived unmanaged `User` **probe import** decides
   exists/absent before any managed `User` is created. A resolved probe fails loud
   (`ServiceAccountCollision`) unless `adopt: true`; a probe reporting the resource
   does not exist is deleted and the managed `User` created.
4. **Managed `User` + generation-scoped password** — the `User`'s `passwordRef`
   points at a `{cp}-service-account-{name}-password-v{N}` Secret. K-ORC's user
   actuator re-applies the password **only when the passwordRef name changes**, so
   a rotation is a Secret-name flip, driven by the CredentialRotation reconciler
   clearing the `forge.c5c3.io/password-generation` annotation. The superseded
   generation Secret is deleted once K-ORC confirms the new one is applied
   (`status.resource.appliedPasswordRef`).
5. **Role assignments** — for each declared `roles[]` entry, project one
   **unmanaged** `Role` import (filtered by the role name, no domain — Keystone
   roles are global) named `{cp}-service-account-{name}-role-{slug}` plus one
   **managed** `RoleAssignment` named `{cp}-service-account-{name}-assign-{slug}`
   binding that role to the account's user on its project (one per user × project ×
   role). The import rides the spec clouds.yaml; the assignment rides the
   admin-password cloud, so a teardown `Delete` survives the AC revoke (like the
   managed `User`). Readiness is folded into the per-account gate — the account is
   not Ready until every assignment is Available — and a `Role` import stalled past
   the grace window hints the role may be missing from Keystone. `{slug}` is a
   deterministic, name-safe discriminator (a normalized ≤16-char base plus 8 hex of
   `sha256(role)`), so distinct roles never alias.
6. **OpenBao round-trip** (once the current password is applied) — assemble a
   source Secret, `PushSecret` (`DeletionPolicy: Delete`) it to
   `openstack/keystone/{cp.Namespace}/{cp.Name}/service-accounts/{name}`, and
   materialize the consumer Secret via an `ExternalSecret`, mirroring the admin
   app-credential's re-push / force-sync discipline. Both the PushSecret and the
   ExternalSecret take their store ref from `spec.secretStoreRef` (default
   `openbao-cluster-store`) via `secrets.PushSecretStoreRefs` /
   `secrets.ESOSecretStoreRef`. Per-account readiness gates on
   the **materialized password matching the current generation**, so a rotated-away
   password never reads Ready.

**Per-account status.** Each declared entry is projected onto a
`status.serviceAccounts[]` entry keyed by `name`. Its `ready` field mirrors that
account's own convergence — user, project, and a materialized password Secret
matching the current generation — so a single lagging account is attributable
without reading the aggregate `ServiceAccountsReady` message. The entry also
carries the resolved `userID` / `projectID`, the applied `passwordGeneration`, and
`lastPasswordRotation`, alongside the `secretName` handle below.

**Consumption contract.** Consumers read from the materialized Secret
`{controlplane.Name}-service-account-{name}-credentials` (keys `password` and a
ready-to-use `clouds.yaml`), named in `status.serviceAccounts[].secretName`. The
credentials are always read from that Secret (or the OpenBao path directly); after
a rotation, one reload picks up the new password. For which OpenBao paths exist in
each Keystone mode, see
[OpenBao paths per ControlPlane mode](../infrastructure/openbao-bootstrap.md#openbao-paths-per-controlplane-mode).

**Deletion.** A managed `User`/`Project` (created **or adopted** — adoption makes
it operator-owned) and the managed `RoleAssignment`s are deleted from Keystone at
teardown, sequenced through the ORC-teardown finalizer exactly like the admin
credential; a probe / domain import / referenced project / `Role` import is a
CR-only delete. The password/source Secrets, PushSecret, and ExternalSecret are
owner-reference-GC'd, and the OpenBao entry dies with the PushSecret
(`DeletionPolicy: Delete`).

**Deferred.** `rotation.mode: Scheduled` is accepted but not yet implemented; it
emits a one-shot `ScheduledRotationDeferred` event so the deferral is not silent.

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
mint logic. It **dispatches on `spec.target`**: `adminApplicationCredential`
nudges the admin AC (clearing the AC's password-hash annotation), and
`serviceAccountPassword` nudges the named service account (clearing the managed
`User`'s `forge.c5c3.io/password-generation` annotation so
`reconcileServiceAccounts` flips its `passwordRef` on the next pass). Its model:

- **Nudge, never mint or delete.** For the admin target it **clears** (zeroes)
  the `forge.c5c3.io/admin-password-hash` annotation on the owned AC CR via
  `clearPasswordHashAnnotation` (a no-op `Update` when already empty). On its next
  pass `reconcileKORC` observes the mismatch and performs the delete+recreate
  re-mint, re-stamping the fresh hash. The `serviceAccountPassword` target is the
  same discipline against the managed `User`: it requires the named account on the
  ControlPlane (`UnknownServiceAccount` otherwise), and — because there is **no
  external password source to observe** — has no auto-detect path, so a rotation
  fires only on an explicit `reMint` (latched to the spec generation). Keeping the
  resource lifecycle owned solely by the ControlPlane reconciler avoids two
  controllers racing on the same object.
- **`reMint` is one-shot per spec generation.** An explicit `spec.reMint` is
  **latched** on `status.lastTriggeredGeneration`: the reconciler nudges only while
  it differs from `metadata.generation`, then records the generation. A `reMint:
  true` left in the spec therefore fires the nudge **once per edit**, not on every
  cache resync (~10 min via the shared `SyncPeriod`) or operator restart — without
  the latch it would revoke + re-mint the admin credential indefinitely, re-opening
  the stale-credential window each cycle. A pass over an already-latched generation
  reports `NoRotationNeeded`. The auto-detect (password-hash change) path is **not**
  latched: it is self-limiting (it stops once the hash matches) and relies on resync
  to observe an out-of-band password rotation.
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
| hash unchanged, no pending `reMint` (incl. a `reMint` already latched for this generation) | True | `NoRotationNeeded` | nothing to do |
| nudge performed | True | `RotationTriggered` | emits `RotationNudged` event; an explicit `reMint` latches `status.lastTriggeredGeneration` |

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
   (DeletionPolicy: None — per-ControlPlane bootstrap secret survives teardown;
    ESO does not watch the source Secret, so a content-hash annotation nudge
    forces the re-push on credential change, and the clouds.yaml force-sync
    below is keyed on that hash + the completed push's syncedResourceVersion)
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
  object's namespace through the uncached API reader and returns
  `field.Forbidden` naming the incumbent if one already exists. The cluster
  therefore admits **exactly one** ControlPlane per namespace. This is what
  makes the CredentialRotation reconciler's `AmbiguousControlPlane` branch
  defense-in-depth (see
  [CredentialRotation reconciler](#credentialrotation-reconciler)).
- **Duplicate guard (reconciler-enforced).** As defense-in-depth for CRs that
  predate the webhook guard, raced through the API server, or were written with
  the webhook bypassed, `Reconcile` runs `duplicateControlPlaneIncumbent`
  before the sub-reconciler chain: it lists ControlPlanes in the CR's namespace
  and parks every CR except the oldest (by `creationTimestamp`, lexically
  smallest name breaking ties). A parked duplicate gets `Ready=False` with
  reason `DuplicateControlPlane` naming the incumbent, runs **no**
  sub-reconcilers, and requeues every 30s — so it takes over automatically once
  the incumbent is fully deleted (no watch event fires on the duplicate's
  behalf when that happens).
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

A child CR created in **the ControlPlane's own namespace** carries an owner
reference to the ControlPlane CR via `controllerutil.SetControllerReference()`.
This enables both **automatic garbage collection** (deleting the ControlPlane
cascades to its children) and **watch-based reconciliation** (a child change
re-reconciles the owner).

A child created in a **service namespace** — one a
[`namespace` assignment](./controlplane-crd.md#service-namespaces) places
elsewhere — can carry **no owner reference**: Kubernetes garbage collection only
cascades within one namespace, so the API server rejects a cross-namespace
controller reference. Such a child is stamped with two **ownership labels**
instead — `c5c3.io/controlplane-name` and `c5c3.io/controlplane-namespace`,
which together name the owning ControlPlane. They carry the two jobs an owner
reference would have done: `isControlPlaneChild` recognizes the child (so a
colliding object owned by nobody is never reshaped or deleted), and a
label-mapped `Watches()` leg resolves an event on it back to a reconcile request.
Because nothing garbage-collects such a child, the finalizer tears it down
explicitly (see below).

### Deletion ordering — the `c5c3.io/orc-teardown` finalizer

Owner-reference GC alone is **unordered**: deleting the ControlPlane would
garbage-collect every child at once. That is unsafe for the K-ORC CRs the
operator owns (`ApplicationCredential`, `Service`, `Endpoint`, `User`,
`Domain`). Those CRs carry K-ORC finalizers that call the **Keystone API** to
revoke/delete the credentials and catalog entries they minted; if Keystone (and
in managed mode its MariaDB) were torn down concurrently, the K-ORC finalizers
could never complete and the ControlPlane — and its namespace — would hang
indefinitely on Terminating ORC CRs.

The ControlPlane reconciler therefore installs a single finalizer,
`c5c3.io/orc-teardown`, added on the first reconcile before any K-ORC CR is
projected. On deletion it:

1. **Deletes the owned K-ORC CRs first** and holds the ControlPlane CR in etcd.
   Holding the CR defers the owner-reference GC cascade, so Keystone stays
   reachable while K-ORC revokes. While ORC CRs are still Terminating the
   reconciler reports `KORCReady=False` with reason `FinalizingORC` and requeues
   at the K-ORC cadence.
2. **Deletes the owned PushSecrets alongside them.** Their
   `deletionPolicy: Delete` cleanup — ESO deleting the mirrored OpenBao data —
   needs the per-tenant `SecretStore` and its `eso-tenant-auth` ServiceAccount,
   both of which the post-release GC cascade reaps unsequenced. Deleting the
   PushSecrets while the finalizer still holds that infrastructure is what
   keeps the revoked credential from outliving the ControlPlane in OpenBao. A
   PushSecret still stuck past the stall window has its finalizers
   force-removed, and a **Warning** `OpenBaoCleanupStalled` names the OpenBao
   paths that may retain data.
3. **Tears down the cross-namespace children before releasing.** A service
   placed in a namespace of its own — and the backing services, tenant store,
   and credential material that follow it — carries no owner reference, so no GC
   cascade reaches it; releasing the finalizer first would strand every one of
   them. `teardownDedicatedNamespaces` deletes them by hand, in order: the
   service children (`Keystone`/`Horizon`) first, waiting for them (their
   operators run a sequenced ESO cleanup through the tenant store in the same
   namespace), then the namespace per its lifecycle. A **`Managed`** namespace is
   deleted, which cascades everything left in it — but only when it carries the
   ownership labels; an unlabelled one is left standing with a **Warning**
   `NamespaceNotOwned`, because the operator never destroys a namespace it did
   not create. An **`External`** namespace survives, so its residue (backing
   services, credential material, tenant-store trio last) is swept by name, each
   object ownership-checked so a same-named object belonging to somebody else in
   that shared namespace is left alone. While children remain the condition
   reports `NamespacesReady=False/FinalizingNamespaces`; past the
   `orcTeardownStallTimeout` the sweep stops waiting, emits a **Warning**
   `NamespaceTeardownStalled` naming what is stuck, and releases anyway — a wedged
   child must not make a namespace undeletable forever.
4. **Releases the finalizer once the ORC CRs, PushSecrets, and cross-namespace
   children are gone**, letting GC cascade-delete the same-namespace Keystone,
   the infrastructure, and the remaining children.
5. **Releases an unmanaged-only remainder immediately.** K-ORC re-fetches the
   imported resource through an *authenticated* actuator before releasing any
   finalizer, and the unmanaged imports authenticate with the admin application
   credential whose revocation step 1 already triggered — so once every CR
   still present is an `Unmanaged` import, waiting on K-ORC is waiting on a
   dead-credential retry loop. The reconciler force-removes their
   `openstack.k-orc.cloud/*` finalizers right away and emits a **Normal**
   `ORCImportsReleased` event. An import's deletion is CR-only, so the external
   installation is untouched and nothing is orphaned.
6. **Bounds the wait.** If managed ORC CRs stay Terminating longer than the
   `orcTeardownStallTimeout` (5 minutes) — typically because Keystone is already
   gone and K-ORC cannot revoke — the reconciler force-removes the stuck
   `openstack.k-orc.cloud/*` finalizers (preserving any non-K-ORC finalizers),
   emits a **Warning** `ORCTeardownStalled` event, and releases the ControlPlane
   finalizer so deletion completes rather than wedging forever.
7. **Names what the escape orphaned.** The escape strips the very finalizer that
   would have revoked the credential or removed the catalog row, so every
   `Managed` CR it releases leaves its OpenStack resource behind with no
   Kubernetes object naming it. A second **Warning**, `ORCResourcesOrphaned`,
   lists exactly those CRs — the admin `ApplicationCredential` and any
   `managedEntries` rows — and tells the operator to remove them from Keystone by
   hand. `Unmanaged` imports are never listed: their CR delete could not have
   touched OpenStack. The classification is by `ManagementPolicy` and fails loud
   (anything not explicitly `Unmanaged` is reported), because under-reporting a
   leak is worse than over-reporting one.

::: warning `kubectl delete namespace` makes the leak deterministic
Children live in the ControlPlane's own namespace, so the namespace controller
reaps `{name}-admin-password-cloud` — the Secret the managed catalog entries
authenticate with, chosen precisely so they *can* still authenticate during
teardown — concurrently with the entry CRs themselves. K-ORC then has no
credential to delete the rows with, the CRs stall, and after 5 minutes the escape
releases them. Delete the **ControlPlane CR** and let it converge before deleting
its namespace.
:::

This mirrors the Keystone reconciler's sequenced-finalizer discipline (MariaDB
then OpenBao cleanup); see
[Keystone reconciler — finalizer](../keystone/keystone-reconciler.md#finalizer).
The `{name}-admin-app-credential-backup` PushSecret is the one child kept on
`DeletionPolicy: None` so its OpenBao path is not purged on teardown.

> **ControlPlane-scoped children live in the owner's namespace; service children
> follow their service.** The K-ORC CRs, the `clouds.yaml` Secret, the
> PushSecret, and the service-account material belong to the ControlPlane as a
> whole and are created in `childNamespace(cp) = cp.Namespace`, owner-referenced.
> A **service** and the things that follow it (its `MariaDB`, `Memcached`,
> `Keystone`/`Horizon`, tenant store, and credential material) are placed in
> `cp.KeystoneNamespace()` / `cp.HorizonNamespace()` — the ControlPlane's own
> namespace by default, or the one a [`namespace`
> assignment](./controlplane-crd.md#service-namespaces) gives it. A
> cross-namespace owner reference is rejected at admission because Kubernetes GC
> only cascades within a single namespace, so a service child in another
> namespace carries the ownership labels and is torn down by the finalizer
> instead of the GC cascade.

| Resource | Name | Owner | Notes |
| --- | --- | --- | --- |
| `MariaDB` | `{spec.infrastructure.database.clusterRef.name}` | ControlPlane CR | managed mode only |
| `Memcached` (unstructured) | `{spec.infrastructure.cache.clusterRef.name}` | ControlPlane CR | managed mode only |
| `ExternalSecret` (DB credential) | `{name}-keystone-db-credentials` | ControlPlane CR | managed mode only; ESO owns the materialised Secret of the same name |
| `ExternalSecret` (admin password) | `{name}-keystone-admin-credentials` | ControlPlane CR | managed mode only; ESO owns the materialised Secret of the same name |
| `Keystone` | `{name}-keystone` | ControlPlane CR | managed mode only |
| `ApplicationCredential` | `{name}-admin-app-credential` | ControlPlane CR | both modes; carries `forge.c5c3.io/admin-password-hash` |
| `Secret` | `{name}-admin-app-credential` | ControlPlane CR | both modes; data written by K-ORC, not the operator |
| `PushSecret` | `{name}-admin-app-credential-backup` | ControlPlane CR | both modes; `DeletionPolicy: None` |
| `User` (K-ORC) | `{name}-user-admin` | ControlPlane CR | both modes; unmanaged import |
| `Domain` (K-ORC) | `{name}-domain-default` | ControlPlane CR | both modes; unmanaged import |
| `Service` (K-ORC) | `{name}-identity-service` | ControlPlane CR | both modes; managed catalog entry in Managed mode, unmanaged import in External mode |
| `Endpoint` (K-ORC) | `{name}-identity-endpoint` | ControlPlane CR | managed mode only; public interface |
| `Endpoint` (K-ORC) | `{name}-identity-endpoint-{interface}` | ControlPlane CR | External mode only; one unmanaged import per interface (`public`, `internal`, `admin`) |
| `Service` (K-ORC) | `{name}-catalog-{type}` | ControlPlane CR | External mode only; one managed CR per declared `managedEntries` entry |
| `Endpoint` (K-ORC) | `{name}-catalog-{type}-{interface}` | ControlPlane CR | External mode only; one managed CR per declared entry endpoint |

#### External-mode deletion resource set

`orcChildObjects(cp)` derives the swept CR names from the ControlPlane spec, so
Managed mode enumerates exactly the five CRs it always did and External mode adds
the per-interface identity `Endpoint` imports plus the CRs of every declared
managed entry. A name that never existed in the current mode is simply `NotFound`
and is tolerated as already-gone.

In External mode `orcTeardownChildren(cp)` folds in one more source: every
catalog-entry CR the ControlPlane still **owns**, found by `List` + the same
controller-reference-**and**-name-prefix scope the reconcile-time prune uses. The
two enumerations diverge whenever a `managedEntries` declaration is dropped from a
spec the prune never re-observed — the prune lives in `reconcileCatalogExternal`,
which `reconcileCatalog` gates on `AdminCredentialReady` and which never runs once
`deletionTimestamp` is set. Enumerating by spec alone would release the finalizer
while those CRs still existed; garbage collection would then take them (and the
credentials `Secret` they authenticate with) at once, stranding them `Terminating`
behind their `openstack.k-orc.cloud/*` finalizers with the stall escape blind to
them, and `kubectl delete namespace` would hang. A declared entry appears in both
enumerations and is named exactly once.

What a `Delete` does to the external OpenStack installation is decided by each
K-ORC CR's `ManagementPolicy`, not by the ControlPlane's mode:

- **`ApplicationCredential`** — `Managed`. Its K-ORC finalizer revokes the
  credential at the Keystone level *before* the CR delete returns, so
  authenticating with it immediately afterwards yields **404** `Could not find
  Application Credential` (not 401). This is the one identity object the operator
  minted, so it is the one it destroys.
- **`User`, `Domain`** — `Unmanaged` imports. Deleting their CRs removes the
  Kubernetes objects and leaves the OpenStack resources they imported untouched.
  K-ORC's deletion-guard finalizers also enforce the teardown order: a `User`
  cannot go while an `ApplicationCredential` still references it.
- **`Service`, `Endpoint`** — in Managed mode these are the managed catalog
  entries, so the sweep deletes them from Keystone's catalog. In **External** mode
  the identity `Service` and its per-interface `Endpoint`s are `Unmanaged`
  imports, so deleting them is a CR-only delete and the external catalog is left
  bit-for-bit intact.
- **The opt-in managed catalog entries** (External mode only) — `Managed`. They
  are the one thing this ControlPlane created in an external catalog, so they are
  the one thing it removes from it, exactly mirroring the `ApplicationCredential`.
  Because they must *reach* the external Keystone to be deleted, they authenticate
  through the operator-owned `{name}-admin-password-cloud` Secret rather than the
  spec's `cloudCredentialsRef` — the sweep issues every `Delete` in one
  unsequenced pass, and the `ApplicationCredential` the spec's `clouds.yaml`
  carries is being revoked at the same moment. The admin password outlives the
  revocation; the app credential does not.

That holds for a teardown K-ORC can complete. The **stall escape is the deliberate
exception**: past `orcTeardownStallTimeout` it releases every stuck CR by stripping
the finalizer that would have done the revoke or the `DELETE`, so each `Managed` CR
it releases orphans its OpenStack resource. Those are the CRs the
`ORCResourcesOrphaned` Warning names.

The OpenBao-backed Secrets are torn down by owner-reference GC, **except** the
path behind the `{name}-admin-app-credential-backup` PushSecret: its
`DeletionPolicy` is deliberately `None`, so the last-pushed credential survives
at its OpenBao path. Nothing else is touched — a K-ORC CR the ControlPlane does
not own is never swept.

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
`operators/c5c3/internal/controller/instrumentation.go`. The helper delegates to
the shared `internal/common/instrumentation` package — the duration/error metric
pair and the wrapper logic are identical across all forge operators and live
there; the c5c3 file supplies only the `c5c3_operator` prefix and the
`subReconcilerConditionTypes` map. `Reconcile` wraps every sub-reconciler call
with it; a direct call that bypasses the helper is a contract violation.

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
| `instrumentation_test.go` | Wiring smoke test (records through the instrumenter), condition_type drift guard |
| `setupwithmanager_test.go` | `For`/`Owns`/`Watches` wiring, field-indexer registration |
| `helpers_test.go` | `intervalToCron` |
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
    │   ├── reconcile_korc.go                    reconcileKORC (AC mint/re-mint, drift detection)
    │   ├── reconcile_admincredential.go         reconcileAdminCredential (assemble + push + re-push
    │   │                                        nudges, semantic clouds.yaml gate)
    │   ├── reconcile_catalog.go                 reconcileCatalog (mode fork; managed identity
    │   │                                        Service/Endpoint), korcAvailableUpToDate
    │   ├── reconcile_catalog_external.go        reconcileCatalogExternal (import-first: unmanaged
    │   │                                        identity imports, opt-in entries, stall detection)
    │   ├── reconcile_delete.go                  reconcileDelete (ORC-teardown finalizer sequencing)
    │   ├── korc_cloudsyaml.go                   clouds.yaml document builders (app-credential + password bootstrap)
    │   ├── korc_eso.go                          PushSecret + clouds.yaml ExternalSecret builders/ensure
    │   ├── korc_imports.go                      admin Domain/User import projection
    │   ├── korc_secrets.go                      app-credential Secret seeding, computeAdminPasswordHash
    │   ├── reconcile_credentialrotation.go      CredentialRotationReconciler (nudge model)
    │   ├── requeue_intervals.go                 infra/dbCredentials/adminPassword/keystone/korc/credentialRotation backoffs
    │   ├── instrumentation.go                   instrumentSubReconciler + drift-guard map
    │   ├── helpers.go                           intervalToCron
    │   ├── controlplane_controller_test.go      Orchestration tests
    │   ├── reconcile_infrastructure_test.go     Infrastructure tests
    │   ├── reconcile_dbcredentials_test.go      DBCredentials tests
    │   ├── reconcile_adminpassword_test.go      AdminPassword tests
    │   ├── reconcile_keystone_test.go           Keystone projection tests
    │   ├── reconcile_korc_test.go               K-ORC / admin-credential / catalog tests
    │   ├── reconcile_delete_test.go             Deletion-sequencing (finalizer) tests
    │   ├── reconcile_credentialrotation_test.go CredentialRotation tests
    │   ├── credential_invariant_test.go         Security-invariant tests
    │   ├── instrumentation_test.go              Metrics instrumentation + drift guards
    │   ├── setupwithmanager_test.go             Watch/Owns/indexer wiring tests
    │   ├── helpers_test.go                      helper-function tests
    │   └── integration_test.go                  Envtest integration test (tag: integration)
    └── testutil/                                c5c3 envtest setup helpers
```

The `c5c3_operator_*` duration/error metric vectors are registered by the shared
`internal/common/instrumentation` package (the former per-operator
`internal/metrics` package was folded into it); `instrumentation.go` supplies
only the `c5c3_operator` prefix and the name → `condition_type` map.

## Migration: legacy flat paths → per-ControlPlane paths

Earlier releases wrote the admin / K-ORC credentials to cluster-global,
flat OpenBao paths that assumed a single control plane per cluster. The operator
now writes every credential family onto a per-CR path keyed by the owning
ControlPlane's (or projected Keystone CR's) `{namespace}/{name}`, so multiple
control planes (one per
namespace; see [Multi-instance](#multi-instance)) never collide in OpenBao. This is
a one-time operator runbook to migrate an **existing** cluster; new clusters need
no migration.

> **Switching a ControlPlane's secret store** (from the default shared cluster
> store to a namespaced per-tenant `SecretStore`, or between stores) is a separate
> operation from this path migration and is documented in the
> [Multi-Tenant Deployment guide → Per-ControlPlane secret stores and OpenBao identities](../../guides/multi-tenant-deployment.md#per-controlplane-secret-stores-and-openbao-identities).
> Switching the ref moves each PushSecret in place — unchanged name and remote key —
> so the irreplaceable key material is relocated, never re-created.

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
| Service-account passwords | — (new) | `openstack/keystone/{namespace}/{name}/service-accounts/{account}` |

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
| `eso-tenant.hcl` | the per-tenant admin AC, admin-password, fernet-keys, credential-keys, and service-account paths, each templated to the caller's own namespace (`…/keystone/{namespace}/+/…` and `bootstrap/{namespace}/+/admin`) |

The three former wildcard write policies (`push-keystone-keys.hcl`,
`push-keystone-admin.hcl`, `push-app-credentials.hcl`) were retired: they matched
every tenant's paths behind a `+/+` glob, so a leaked shared-identity token could
overwrite any tenant's key material. Per-tenant secret traffic now authenticates
as the `eso-tenant` role through the operator-provisioned per-tenant
`openbao-tenant-store`, and `eso-tenant.hcl` scopes every writable path to the
caller's own namespace.

Until the `eso-tenant` policy is re-applied, ESO's push to the new path returns
`403` and the credential's Ready condition stays `False`.

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
