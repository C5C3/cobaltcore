---
title: Keystone Reconciler Architecture
quadrant: operator
---

# Keystone Reconciler Architecture

Reference documentation for the KeystoneReconciler and its sub-reconciler contracts. The KeystoneReconciler implements the control loop that drives a Keystone
CR from desired state to a fully operational Keystone Identity Service deployment.

For CRD type definitions and webhooks, see
[Keystone CRD API Reference](./keystone-crd.md). For the shared library functions
used by sub-reconcilers, see
[Kubernetes-Interacting Packages](../backend/kubernetes-packages.md).

For the NetworkPolicy that hardens the **keystone-operator pod itself**
(distinct from the per-CR NetworkPolicy emitted by
[`reconcileNetworkPolicy`](#sub-reconciler-contracts) below), see
[Keystone Operator NetworkPolicy](./keystone-operator-networkpolicy.md).

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
| `Keystone` | `For()` | Triggers reconciliation on CR changes, filtered by a predicate (see below) so the controller is not re-woken by its own status writes |
| `Deployment` | `Owns()` | Triggers reconciliation when owned Deployment changes |
| `Service` | `Owns()` | Triggers reconciliation when owned Service changes |
| `ConfigMap` | `Owns()` | Triggers reconciliation when owned ConfigMap changes |
| `Job` | `Owns()` | Triggers reconciliation when owned Job changes |
| `PodDisruptionBudget` | `Owns()` | Triggers reconciliation when owned PDB changes |
| `HorizontalPodAutoscaler` | `Owns()` | Triggers reconciliation when owned HPA changes |
| `CronJob` | `Owns()` | Triggers reconciliation when owned CronJob changes |
| `HTTPRoute` | `Owns()` (optional) | Registered only when the `gateway.networking.k8s.io/v1` CRD is installed; detected at startup via the manager's `RESTMapper`. Triggers reconciliation when owned HTTPRoute changes (only created when `spec.gateway` is set). |
| `Certificate` | `Owns()` (optional) | Registered only when the `cert-manager.io/v1` CRD is installed; detected at startup via the manager's `RESTMapper`. Triggers reconciliation when the managed `<name>-db-client` Certificate changes, so later issuance failures surface in `DatabaseTLSReady` (only created when managed DB TLS is enabled). |
| `Secret` | `Watches()` | Maps Secret events to referencing Keystone CRs via the `KeystoneSecretNameIndexKey` field indexer, with an owner-ref fallback for rotation staging Secrets |
| `MariaDB` | `Watches()` | Propagates upstream DB cluster health into `DatabaseReady` |
| `ClusterSecretStore` | `Watches()` | Propagates OpenBao-backend health into `SecretsReady` |
| `PushSecret` | `Watches()` | Maps backup PushSecret events to the owning Keystone CR via `pushSecretToKeystoneMapper` (name-match against `openBaoBackupPushSecretNames`). A predicate admits only the transitions that affect the [OpenBao Finalizer](#openbao-finalizer) state machine — `esoPushSecretFinalizer` set churn, `DeletionTimestamp` flip, or `Generation` bump — and suppresses ESO's status-only re-emits. Replaces the prior `Owns(PushSecret)` wiring. |

Secrets use `Watches()` with a `MapFunc` instead of `Owns()` because some Secrets
(ESO-provided credentials in `spec.database.secretRef` and
`spec.bootstrap.adminPasswordSecretRef`) are owned by the ExternalSecret controller,
not by the Keystone CR, so an owner-reference filter would never match them. The
mapper therefore combines an indexed reverse lookup with an owner-ref fallback for
rotation staging Secrets — see [Secret Field Indexer](#secret-field-indexer)
below. The `MariaDB` and `ClusterSecretStore` watches exist so the operator reacts
immediately to upstream dependency outages without waiting for the next periodic
requeue. The `PushSecret` watch plays the same role for the
OpenBao-backup finalizer loop: without it the finalizer would requeue at the
`RequeueSecretPolling` (15s) cadence between each `esoPushSecretFinalizer`
adoption check (Pass-0) and each `DeletionTimestamp` check (Pass-1);
with it, each stage transition wakes on watch delivery instead — see [PushSecret Name-Match Mapper](#pushsecret-name-match-mapper)
below.

The `For(Keystone)` watch carries a predicate
(`Or(GenerationChangedPredicate, LabelChangedPredicate, AnnotationChangedPredicate, terminating)`)
so a `Status().Update` — which the controller issues on every reconcile — does
not re-enqueue the CR and spin the loop. The trade-offs are deliberate:
because the CRD has a status subresource, setting `deletionTimestamp` does
**not** bump the generation and touches neither labels nor annotations, so the
dedicated `terminating` predicate admits the live→Terminating transition —
otherwise finalizer cleanup (and `kubectl delete`) would stall until the next
resync; the operator's own annotation
patches pass via `AnnotationChangedPredicate`; the 10-minute informer resync on
the CR itself is filtered, but every `Owns()`/`Watches()` secondary resource
still resyncs and enqueues the owner, so drift repair is preserved. A future
feature that must reconcile on CR *status* written by another actor would be
filtered by this predicate — an intentional part of the contract.

#### Secret Field Indexer

The Keystone controller registers a controller-runtime field indexer on the
`Keystone` kind so that a Secret event is resolved to the referencing Keystone
CR(s) via an O(1) cache lookup instead of an unfiltered namespace-scoped List.
Without the indexer, every Secret create/update/delete event in a namespace
containing ESO-managed Secrets would force the mapper to List every Keystone CR
in that namespace — producing API server load that scales linearly with the
number of Secret events, not with the number of Keystone CRs.

| Aspect | Value |
| --- | --- |
| Index key | `KeystoneSecretNameIndexKey = "spec.secretRefs.name"` (exported package-level constant in `operators/keystone/internal/controller/keystone_controller.go`) |
| Indexed fields | `spec.database.secretRef.name` **and** `spec.bootstrap.adminPasswordSecretRef.name` — the deduplicated union of both is emitted by the extractor; empty strings are skipped so unset optional fields do not pollute the index. |
| Registration site | `SetupWithManager` → `registerSecretNameIndex(ctx, mgr.GetFieldIndexer())`, invoked **before** the `Watches(Secret, …)` chain. Any error from `IndexField` is wrapped with the index key and propagated, so manager startup aborts loudly if registration fails. |
| Lookup site | `secretToKeystoneMapper(mgr.GetClient())` — performs a namespace-scoped `client.List` with `client.MatchingFields{KeystoneSecretNameIndexKey: secret.Name}`. On List error, the error is logged and swallowed (the `handler.MapFunc` contract forbids returning errors) so the owner-ref fallback still runs. |
| Owner-ref fallback | For each `ownerReference` on the Secret where `Kind == "Keystone"` and the parsed group of `APIVersion` equals `keystonev1alpha1.GroupVersion.Group` (`keystone.openstack.c5c3.io`, any version), the mapper enqueues `{Namespace: secret.Namespace, Name: ownerRef.Name}`. A cached `Get` against the informer cache drops owner-refs whose target Keystone no longer exists (stale or spurious refs); any non-`NotFound` error falls through and enqueues anyway so a transient cache blip cannot swallow a legitimate event. Group-only matching means existing Secrets continue to resolve after a future API version bump. This preserves the enqueue path for rotation staging Secrets (`{name}-fernet-keys-rotation`, `{name}-credential-keys-rotation`; see [Key Rotation RBAC Split](#key-rotation-rbac-split)) which are owned by the Keystone CR but not referenced by name from the spec. |
| Deduplication | The indexed-lookup and owner-ref paths are unioned by `types.NamespacedName` before returning, so a Secret that is both name-referenced and owner-referenced to the same Keystone yields exactly one `reconcile.Request`. |

**Adding new Secret references.** When a future change introduces another
`SecretRef` field on `KeystoneSpec`, extend `keystoneSecretNameExtractor` to
emit that field's name alongside the existing two, and add a corresponding
unit-test case. The index key itself (`spec.secretRefs.name`) is intentionally
named as a union key so new indexed fields do not require a new indexer.

#### PushSecret Name-Match Mapper

The backup PushSecrets that the [OpenBao Finalizer](#openbao-finalizer)
reconciles (the Fernet and credential key PushSecrets produced by
`openBaoBackupPushSecretNames(keystone)`) are watched via an explicit
name-matching mapper rather than `Owns()`. The mapper's contract mirrors the
[Secret Field Indexer](#secret-field-indexer) mapper above but uses
direct name matching instead of a field indexer (there are at most two backup
PushSecret names per Keystone CR, so an indexer would not pay for itself).

**Why not `Owns(PushSecret)`?** An earlier iteration wired the backup
PushSecrets through the manager's `Owns(&esov1alpha1.PushSecret{})`. That
form wakes the Keystone workqueue on every change to an owned PushSecret,
including the status-only ticks ESO emits on every successful sync
(`status.syncedResourceVersion` bump, condition `LastTransitionTime`,
`observedGeneration` echo). In a typical deployment ESO syncs each adopted
PushSecret on its configured `refreshInterval` (default 1h but often
minutes), which under `Owns()` translates directly into reconcile wake-ups
the finalizer state machine has nothing to do with — they do not change
`Generation`, do not add or remove finalizers, and do not flip
`DeletionTimestamp`, so each such wake-up is discarded after a Get + status
diff. Replacing `Owns()` with an explicit `Watches()` plus
`pushSecretRelevantChangePredicate` admits only the transitions the Pass-0
adoption gate and Pass-1 delete step actually branch on, eliminating the
per-sync-tick workqueue churn while preserving the sub-15s latency win the
feature targets.

| Aspect | Value |
| --- | --- |
| Name match | Namespace-scoped `client.List` of Keystone CRs in `obj.GetNamespace()`; for each CR, iterate `openBaoBackupPushSecretNames(keystone)` and compare against `obj.GetName()`. A PushSecret event whose namespace matches no Keystone returns `nil`. |
| Namespace scope | The `List` always carries `client.InNamespace(obj.GetNamespace())`. PushSecret is a namespaced resource, so the apiserver guarantees `obj.GetNamespace()` is non-empty in practice and the List is always single-namespace; an event whose namespace matches no Keystone returns `nil`. |
| List error handling | Logged via `log.FromContext(ctx).Error` and swallowed. The `handler.MapFunc` contract forbids returning errors, and a transient List blip must not panic or silently drop the event; on the next relevant PushSecret update the predicate will admit it again and the mapper will retry. |
| Deduplication | Builds a `map[types.NamespacedName]struct{}` before emitting `[]reconcile.Request`, so even if both of a Keystone's backup PushSecrets changed in the same batch the owning CR is enqueued exactly once. |
| Predicate (`pushSecretRelevantChangePredicate`) | `Create`, `Delete`, and `Generic` return `true` unconditionally. `Update` returns `true` iff (a) the finalizer set differs (typically `esoPushSecretFinalizer` added or removed), (b) `DeletionTimestamp` presence flips (`nil` vs non-`nil` between old and new), or (c) `GetGeneration()` differs. Status-only re-emits are filtered out. The presence check uses `== nil` rather than `.IsZero()` so the expression is obviously nil-safe without relying on `metav1.Time.IsZero`'s nil-receiver guard. |

The motivating transitions are:

- **Pass-0 adoption gate.** The operator waits for ESO to stamp
  `esoPushSecretFinalizer` (declared in `reconcile_secrets.go`) onto each
  backup PushSecret before allowing `Delete`; otherwise a racing `Delete`
  could remove the object before ESO's cleanup finalizer runs. Under the
  prior `Owns()` wiring this check requeued at `RequeueSecretPolling`
  (15s); now it wakes on the `esoPushSecretFinalizer`-add update.
- **Pass-1 delete step.** The operator issues `Delete` against
  each backup PushSecret and then waits for `DeletionTimestamp` to be
  non-zero and eventually for the object to disappear. Each of those
  observations was a 15s requeue earlier; they are now watch-driven.

See [OpenBao Finalizer](#openbao-finalizer) for the full Pass-0/Pass-1
state machine the watch feeds, and [RBAC Permissions](#rbac-permissions) for
the `external-secrets.io/pushsecrets` verb set (`get, list, watch, create,
update, patch, delete`).

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
| `HTTPClient` | `HTTPDoer` | Injectable HTTP client for health checks; falls back to `http.DefaultClient` when nil |

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
| `external-secrets.io` | `externalsecrets`, `pushsecrets` | get, list, watch, create, update, patch, delete |
| `external-secrets.io` | `clustersecretstores` | get, list, watch |
| `policy` | `poddisruptionbudgets` | get, list, watch, create, update, patch, delete |
| `autoscaling` | `horizontalpodautoscalers` | get, list, watch, create, update, patch, delete |
| `gateway.networking.k8s.io` | `httproutes` | get, list, watch, create, update, patch, delete |
| `gateway.networking.k8s.io` | `httproutes/status` | get |

---

## Labels and Annotations

The reconciler applies `commonLabels(keystone)` (`app.kubernetes.io/name`,
`app.kubernetes.io/instance`, `app.kubernetes.io/managed-by`) to every owned
resource. In addition, the following forge-specific metadata keys carry
controller-observable semantics and are stable across releases — consumers
(watch predicates, chainsaw tests, dashboards) may rely on them:

| Key | Kind | Applied to | Value | Purpose |
| --- | --- | --- | --- | --- |
| `forge.c5c3.io/rotation-target` | Label | Staging Secrets (`{name}-fernet-keys-rotation`, `{name}-credential-keys-rotation`, `{name}-admin-password-rotation`) | `fernet-keys`, `credential-keys`, `admin-password` | Distinguishes rotation staging Secrets from production key Secrets so the operator's Secret→Keystone mapper can enqueue the owning Keystone on staging PATCHes. |
| `forge.c5c3.io/rotation-completed-at` | Annotation | Staging Secrets (written by the rotation CronJob) | RFC3339 UTC timestamp (e.g. `2026-04-18T12:34:56Z`) | Single-shot commit marker. The operator only applies a staging Secret's data to the production Secret when this annotation is present and parses cleanly; the annotation is removed implicitly when the staging Secret is deleted at the end of a successful apply. |
| `forge.c5c3.io/admin-password-hash` | Annotation | Bootstrap Job pod template (`{name}-bootstrap`) | `hex(SHA-256(password))` of the `password` key of the admin Secret | Carries the admin-password digest into the pod template so a rotated password changes the pod-spec hash and re-runs the idempotent bootstrap Job. See [`reconcileBootstrap`](#reconcilebootstrap). |
| `forge.c5c3.io/pod-spec-hash` | Annotation | Operator-managed Jobs (`{name}-bootstrap`, migration Jobs) | `hex(SHA-256(PodTemplateSpec))` stamped at creation time | Change-detection gate for completed Jobs. `job.RunJob` compares the desired hash against this annotation and recreates the Job (so it re-runs) when they differ, without normalizing API-server defaults. |

The Go constants backing the rotation keys are exported from
`operators/keystone/internal/controller/rotation_staging.go`:

```go
const StagingSecretLabelKey       = "forge.c5c3.io/rotation-target"
const RotationCompletedAnnotation = "forge.c5c3.io/rotation-completed-at"
```

`forge.c5c3.io/pod-spec-hash` is backed by `job.PodSpecHashAnnotation` in
`internal/common/job/job.go`; `forge.c5c3.io/admin-password-hash` by the
unexported `adminPasswordHashAnnotation` const in
`operators/keystone/internal/controller/reconcile_bootstrap.go`.

See [Key Rotation RBAC Split](#key-rotation-rbac-split) under the
Fernet and credential sub-reconciler sections for the full contract.

---

## Reconciliation Flow

```text
┌──────────────────────────────────────────────────────────────────────────────┐
│                       KEYSTONE RECONCILIATION FLOW                           │
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
│  │                  │  Sets: SecretsReady    │  ═════ Parallel             │ │
│  └────────┬─────────┘  Requeue: 15s          └─────────────────────────────┘ │
│           │                                                                  │
│           ▼                                                                  │
│  ┌──────────────────┐                                                        │
│  │ reconcileConfig  │  Render keystone.conf + api-paste.ini                  │
│  │                  │  Create immutable ConfigMap                            │
│  └────────┬─────────┘  Returns: configMapName                                │
│           │                                                                  │
│           ▼                                                                  │
│  ╔═════════════════════════════════════════════════════════════════════════╗ │
│  ║  reconcileParallelGroup                                                 ║ │
│  ║                                                                         ║ │
│  ║  errgroup.WithContext — each goroutine receives a DeepCopy of the CR    ║ │
│  ║                                                                         ║ │
│  ║  ┌──────────────────────┐  ┌──────────────────────────┐                 ║ │
│  ║  │ reconcileFernetKeys  │  │ reconcileCredentialKeys  │  (concurrent)   ║ │
│  ║  │ + script ConfigMap   │  │ + script ConfigMap       │                 ║ │
│  ║  │ Sets: FernetKeysReady│  │ Sets: CredentialKeysReady│                 ║ │
│  ║  └──────────────────────┘  └──────────────────────────┘                 ║ │
│  ║  ┌─────────────────────────┐                                            ║ │
│  ║  │ reconcileNetworkPolicy  │  (concurrent)                              ║ │
│  ║  │ Sets: NetworkPolicyReady│                                            ║ │
│  ║  └─────────────────────────┘                                            ║ │
│  ║                                                                         ║ │
│  ║  g.Wait() → MergeCondition → ShortestRequeue                   ║ │
│  ╚═══════════════════════════════╤═════════════════════════════════════════╝ │
│                                  │                                           │
│           ┌──────────────────────┘                                           │
│           ▼                                                                  │
│  ┌───────────────────┐                                                       │
│  │ reconcileDatabase │  Managed mode: verify MariaDB cluster health first,   │
│  │                   │  then ensure Database/User/Grant CRs + run db_sync    │
│  │                   │  Job + run schema-check Job                           │
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
│  │                         │  Retain 3 historical + current                  │
│  └────────┬────────────────┘  No condition, no requeue                       │
│           │                                                                  │
│           ▼                                                                  │
│  ╔═════════════════════════════════════════════════════════════════════════╗ │
│  ║  reconcileParallelGroup (second group)                                  ║ │
│  ║                                                                         ║ │
│  ║  ┌──────────────────────┐  ┌────────────────────────┐                   ║ │
│  ║  │ reconcileHTTPRoute   │  │ reconcileHealthCheck   │  (concurrent)     ║ │
│  ║  │ Sets: HTTPRouteReady │  │ Sets: KeystoneAPIReady │                   ║ │
│  ║  └──────────────────────┘  └────────────────────────┘                   ║ │
│  ║  ┌──────────────┐  ┌─────────────────────┐  ┌────────────────────────┐  ║ │
│  ║  │ reconcileHPA │  │ reconcileBootstrap  │  │ reconcileTrustFlush    │  ║ │
│  ║  │ Sets:HPAReady│  │ Sets: BootstrapReady│  │ Sets: TrustFlushReady  │  ║ │
│  ║  └──────────────┘  └─────────────────────┘  └────────────────────────┘  ║ │
│  ║                                                                         ║ │
│  ║  g.Wait() → MergeCondition → ShortestRequeue                   ║ │
│  ╚═══════════════════════════════╤═════════════════════════════════════════╝ │
│           ┌──────────────────────┘                                           │
│           ▼                                                                  │
│  ┌────────────────────────────┐                                              │
│  │ reconcilePasswordRotation  │  Ensure admin-password rotation CronJob      │
│  │                            │  (Model B); apply staged rotation            │
│  │                            │  Sets: PasswordRotationReady                 │
│  └────────┬───────────────────┘  Requeue: on apply only                      │
│           │                                                                  │
│           ▼                                                                  │
│  setReadyCondition() — aggregate Ready from all sub-conditions               │
│  updateStatus() — persist to API server                                      │
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Execution Model

Sub-reconcilers execute in a defined order using two execution modes:

1. **Sequential sub-reconcilers** run one at a time. Each is called only if all
   previous sub-reconcilers succeeded without requesting a requeue.
2. **Parallel groups** (`reconcileParallelGroup`) run independent sub-reconcilers
   concurrently via `errgroup.WithContext`. Each goroutine operates on a `DeepCopy` of
   the Keystone CR to prevent data races. There are two groups: the first (after
   Config) runs FernetKeys / CredentialKeys / NetworkPolicy; the second (after
   Deployment and config pruning) runs HTTPRoute / HealthCheck / HPA / Bootstrap /
   TrustFlush, which share no inter-dependency once the Deployment/Service/Config
   outputs exist. `reconcilePasswordRotation` stays sequential after the second
   group because it depends on Bootstrap having seeded the initial admin
   credential. See [Parallel Group Architecture](#parallel-group-architecture) for
   details.

The chain is a table-driven pipeline over the shared scaffolding in
`internal/common/reconcile`. Each step is a `commonreconcile.Step` (`Name` +
`Fn`); `commonreconcile.RunPipeline` attempts the steps in order (named steps
wrapped in `instrumentSubReconciler`, empty-`Name` steps run bare because they
self-instrument or are intentionally uninstrumented) and the single call site
funnels every exit path through `updateStatus` by construction:

```go
result, err := commonreconcile.RunPipeline(ctx, instrumentSubReconciler, pipeline)
return r.updateStatus(ctx, &keystone, statusBefore, result, err)
```

This guarantees:

1. A step error **propagates immediately** — subsequent steps are skipped.
2. A non-zero result (`!result.IsZero()`) causes an **early return** — status is
   persisted and the reconciler exits.
3. Status conditions from the failing/requeuing step are **always persisted**
   via `updateStatus()` before returning.

`reconcileConfig` and `pruneStaleConfigMaps` are ordinary steps in this pipeline.
They signal failure with a bare `error` rather than a status condition of their
own, so each wraps its call to flip `SecretsReady=False` (via `markConfigFailed`)
on failure. Without that, `setReadyCondition` would re-aggregate the still-True
sub-conditions and persist a stale `Ready=True` at the new generation — the
failure would be visible only in logs and the error counter.

The parallel group follows a different contract: all three sub-reconcilers run
simultaneously, errors cancel the errgroup context, and conditions from completed
sub-reconcilers are merged even on partial failure.

### Status Update Pattern

`updateStatus()` delegates to the shared `commonreconcile.UpdateStatus`: it
persists all condition changes via `r.Status().Update()` and returns the
provided `(result, error)` pair unchanged.

`Reconcile` snapshots `keystone.Status` immediately after the initial Get and
threads that snapshot into `updateStatus`. After aggregating `Ready` and stamping
`ObservedGeneration`, `updateStatus` compares the result against the snapshot with
`equality.Semantic.DeepEqual` and **skips** `r.Status().Update()` when nothing
changed. `meta.SetStatusCondition` preserves `LastTransitionTime` on a no-op
upsert, so a converged steady-state pass produces a byte-identical status and
issues no write — no write means no watch event and no `resourceVersion` churn.
A change to any condition, reason, message, or `ObservedGeneration` still writes.

If the status update itself fails, the
behavior depends on whether a reconcile error is also present:

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

The pipeline uses two parallel groups, each driven by `reconcileParallelGroup`:

- **Group 1** — `reconcileFernetKeys`, `reconcileCredentialKeys`, and
  `reconcileNetworkPolicy` — runs after `reconcileConfig` completes and before
  `reconcileDatabase` begins.
- **Group 2** — `reconcileHTTPRoute`, `reconcileHealthCheck`, `reconcileHPA`,
  `reconcileBootstrap`, and `reconcileTrustFlush` — runs after
  `reconcileDeployment` and config pruning. Once the Deployment/Service and the
  config ConfigMap exist, these five share no data dependency on each other.

Both groups' members are eligible for parallelization because they have no data
dependencies on each other (see [Dependency Graph](#dependency-graph) below).
`reconcilePasswordRotation` stays sequential after group 2 because it depends on
Bootstrap having seeded the initial admin credential. A member that returns a
non-zero result no longer short-circuits its siblings: all members run every
pass and `commonreconcile.ShortestRequeue` aggregates their requeues.

**File:** `operators/keystone/internal/controller/keystone_controller.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcileParallelGroup(
    ctx context.Context,
    keystone *keystonev1alpha1.Keystone,
    subs []commonreconcile.ParallelStep[*keystonev1alpha1.Keystone],
) (ctrl.Result, error)
```

The method is a thin binding of the shared
`commonreconcile.RunParallelGroup` (errgroup, per-member `DeepCopy`,
condition merge on partial failure, shortest-requeue aggregation) to the
Keystone CR type. Each parallel sub-reconciler is described by a
`commonreconcile.ParallelStep`:

```go
type ParallelStep[T any] struct {
    Name          string
    ConditionType string
    Fn            func(ctx context.Context, cr T) (ctrl.Result, error)
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
| `reconcileConfig` | CR spec, DB secret | `SecretsReady` (False on failure; *returns configMapName*) | Secrets | no (produces configMapName) |
| `reconcileFernetKeys` | configMapName | `FernetKeysReady` | Config | **yes (group 1)** |
| `reconcileCredentialKeys` | configMapName | `CredentialKeysReady` | Config | **yes (group 1)** |
| `reconcileNetworkPolicy` | CR spec | `NetworkPolicyReady` | none | **yes (group 1)** |
| `reconcileDatabase` | configMapName | `DatabaseReady` | Config | no (complex state machine) |
| `reconcilePolicyValidation` | configMapName | `PolicyValidReady` | Config | no (gates Deployment) |
| `reconcileDeployment` | configMapName | `DeploymentReady` | Database (implicit) | no |
| `pruneStaleConfigMaps` | configMapName | `SecretsReady` (False on failure) | Deployment (must be ready) | no |
| `reconcileHTTPRoute` | CR spec | `HTTPRouteReady` | Deployment (ensures backend Service exists) | **yes (group 2)** |
| `reconcileHealthCheck` | status.endpoint | `KeystoneAPIReady` | Deployment (sets endpoint) | **yes (group 2)** |
| `reconcileHPA` | CR spec | `HPAReady` | Deployment (naming) | **yes (group 2)** |
| `reconcileBootstrap` | configMapName | `BootstrapReady` | Config, DB (runs its own Job) | **yes (group 2)** |
| `reconcileTrustFlush` | configMapName | `TrustFlushReady` | Config | **yes (group 2)** |
| `reconcilePasswordRotation` | `spec.passwordRotation` | `PasswordRotationReady` | Bootstrap (re-bootstrap is the downstream consumer) | no |

Key constraints that prevent further parallelization:

- **reconcileDatabase** has a multi-step state machine (MariaDB CRs → db_sync Job →
  schema-check Job) with 30s requeue waits, and `reconcileDeployment` depends on the
  database being ready.
- **reconcilePolicyValidation** must gate `reconcileDeployment` — invalid policy
  overrides must be caught before reaching running pods.
- **reconcilePasswordRotation** stays sequential after group 2 because it depends
  on Bootstrap having seeded the initial admin credential.

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

When all parallel sub-reconcilers succeed, the shared
`commonreconcile.ShortestRequeue` selects the `ctrl.Result` with the shortest
non-zero `RequeueAfter` from the group:

```go
func ShortestRequeue(results ...ctrl.Result) ctrl.Result
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
etcd. Without a finalizer, the Keystone CR would be deleted
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
   `Deployment`, so the `keystone` Pod kept its MariaDB connections open.
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

> **Note:** The two Events are intentionally asymmetric. A
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
the Fernet and credential signing keys to OpenBao. Without this
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
| `{keystone.Name}-fernet-keys-backup` | `external-secrets.io` | `kv-v2/data/openstack/keystone/{keystone.Namespace}/{keystone.Name}/fernet-keys` |
| `{keystone.Name}-credential-keys-backup` | `external-secrets.io` | `kv-v2/data/openstack/keystone/{keystone.Namespace}/{keystone.Name}/credential-keys` |

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

> **Path scoping:** Both KV-v2 paths are per-CR-scoped via
> `openstack/keystone/{keystone.Namespace}/{keystone.Name}/<leaf>` (where `<leaf>`
> is `fernet-keys` or `credential-keys`), so multiple Keystone CRs — in the same
> namespace or across namespaces — write to disjoint paths and cannot collide. The
> leading `{keystone.Namespace}` segment was the most recent addition, layered on
> top of an earlier per-name scoping. See
> [Migration note: legacy flat paths](#migration-note-legacy-flat-paths)
> for upgrade behaviour and recommended cleanup of orphaned legacy paths.

### Migration note: legacy flat paths

The KV-v2 path layout for these backups has evolved in two steps:

1. **Flat → per-name.** Originally both backup PushSecrets wrote to
   the cluster-global, flat KV-v2 paths `kv-v2/openstack/keystone/fernet-keys`
   and `kv-v2/openstack/keystone/credential-keys`. A first migration scoped them
   per CR by adding the CR-name segment:
   `kv-v2/openstack/keystone/{keystone.Name}/fernet-keys` and
   `…/{keystone.Name}/credential-keys`.
2. **Per-name → namespace+name.** A later migration added
   the leading namespace segment so the paths are unique across namespaces as
   well as within one. The operator now writes to
   `kv-v2/openstack/keystone/{keystone.Namespace}/{keystone.Name}/fernet-keys`
   and `…/{keystone.Namespace}/{keystone.Name}/credential-keys`.

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

The superseded paths (both the original flat paths **and** the
per-name paths that lacked the namespace segment) are **orphaned but
harmless** after upgrade: the live Keystone control plane reads its Fernet
and credential keys from the local Kubernetes `Secret`
(`{name}-fernet-keys`, `{name}-credential-keys`), not from the OpenBao
backup. The OpenBao copy is a disaster-recovery artefact only; the legacy
entries simply stop being refreshed, never get deleted by
`DeletionPolicy=Delete` (no live PushSecret references them anymore), and
are otherwise inert.

Operators who want a clean OpenBao state can purge the superseded entries
manually after upgrade — both the original flat paths and the
per-name (namespace-less) paths:

```sh
# original flat paths
bao kv metadata delete kv-v2/openstack/keystone/fernet-keys
bao kv metadata delete kv-v2/openstack/keystone/credential-keys
# per-name paths that lacked the namespace segment
bao kv metadata delete kv-v2/openstack/keystone/<name>/fernet-keys
bao kv metadata delete kv-v2/openstack/keystone/<name>/credential-keys
```

`metadata delete` removes both the current version and all historical
versions of the secret at that path; this is the canonical KV-v2 purge
operation and the right inverse of the now-superseded write.

#### Admin-password path migration (Model B)

The Model-B admin-password PushSecret moves onto the same
per-CR layout. The flat `bootstrap/keystone-admin` object becomes the per-CR
`bootstrap/{keystone.Namespace}/{keystone.Name}/admin`. As above, the new
RemoteKey lands on operator upgrade, so the matching ACL
(`deploy/openbao/policies/push-keystone-admin.hcl`) must be re-applied first or
concurrently — re-run `deploy/openbao/bootstrap/setup-policies.sh` or
`bao policy write push-keystone-admin …` — otherwise ESO returns `403` on the
push and `PasswordRotationReady` flips to `False`. The legacy
`bootstrap/keystone-admin` object is **orphaned but harmless** after migration
and can be purged once the per-CR path is populated and Ready:

```sh
bao kv metadata delete kv-v2/bootstrap/keystone-admin
```

For the end-to-end, multi-credential operator runbook (admin application
credential + admin password + Fernet/credential keys, with the one-time copy
commands and the full ACL re-apply set), see the c5c3 controlplane reconciler's
[Migration: legacy flat paths → per-ControlPlane paths](../c5c3/controlplane-reconciler.md#migration-legacy-flat-paths--per-controlplane-paths).

### DeletionPolicy=Delete Wiring Through ESO

The Keystone operator has no OpenBao credentials and does not talk to the
OpenBao API directly. Remote purge of the KV-v2 path is delegated to ESO
via the PushSecret field `Spec.DeletionPolicy`. The `RemoteKey` follows the
per-CR layout `openstack/keystone/{keystone.Namespace}/{keystone.Name}/<leaf>`
(scoped by the CR's namespace and name),
so each Keystone CR writes to its own KV-v2 prefix:

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
                RemoteKey: fmt.Sprintf("openstack/keystone/%s/%s/fernet-keys", keystone.Namespace, keystone.Name),
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
   `keystoneOpenBaoFinalizer` (e.g. a CR that predates the finalizer, or whose
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
   passes over the backup PushSecret names (the two-pass design was
   extended with a Pass-0 adoption wait):
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
     The wait is **bounded** by `OpenBaoAdoptionWaitTimeout` (10m): once the
     CR has been deleting longer than that, an unadopted PushSecret stops
     blocking — the handler emits an `ESOAdoptionTimedOut` Warning event and
     proceeds to Pass-1's `Delete` — so a renamed or absent ESO finalizer
     cannot hang CR deletion forever at `WaitingForESOAdoption`.
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
| `WaitingForESOAdoption` | Pre-Delete adoption wait (Pass-0) | A backup PushSecret exists, is not Terminating, and does **not** yet carry ESO's adoption finalizer (`pushsecret.externalsecrets.io/finalizer`). | Resolves itself on ESO workqueue drain. Check `kubectl -n external-secrets logs deploy/external-secrets` for backlog or errors. The wait is bounded by `OpenBaoAdoptionWaitTimeout` (10m); past that the handler emits an `ESOAdoptionTimedOut` Warning and force-deletes so CR deletion never hangs. |
| `OpenBaoFinalizerBlocked` | Post-Delete wait-for-gone (Pass-2) | A backup PushSecret is still present in the API server after `Delete` was issued, typically in Terminating state behind ESO's cleanup finalizer. | ESO is running `DeletionPolicy=Delete` against OpenBao. A persistent block here may indicate OpenBao unreachable or ClusterSecretStore auth revoked. |

The three passes execute in strict order:

1. **Pass-0 — adoption wait.** For each backup PushSecret that exists and
   is not Terminating, the handler `Get`s the object and inspects its
   `metadata.finalizers` for
   `pushsecret.externalsecrets.io/finalizer`. On the
   first unadopted PushSecret it records `WaitingForESOAdoption` and
   returns `(done=false, nil)` **without** firing any `Delete`. This wait is
   bounded by `OpenBaoAdoptionWaitTimeout` (10m): past that deadline the
   unadopted PushSecret no longer blocks — the handler emits an
   `ESOAdoptionTimedOut` Warning and breaks to Pass-1's force-`Delete`. The
   force-delete stays safe when ESO merely renamed its finalizer (the
   PushSecret carries the renamed finalizer, so `Delete` only marks it
   Terminating and ESO still purges the kv-v2 path); the path is orphaned
   only if ESO is genuinely not running during the deletion window.
2. **Pass-1 — parallel Delete.** Once every still-present PushSecret is
   adopted (ESO finalizer present) or already Terminating, the handler
   issues `Delete` on every name in a single pass so the cleanup
   finalizers fire in parallel.
3. **Pass-2 — wait-for-gone.** The handler re-`Get`s each name and, on
   the first still-present PushSecret, records `OpenBaoFinalizerBlocked`
   and returns `(done=false, nil)`. Release of the openbao-finalizer
   requires **all** PushSecrets to return `NotFound`.

#### Motivating race

Without Pass-0, a Keystone CR deleted within 1–2 s of creation can outrun
ESO's first reconcile: the operator calls `Delete` on the PushSecret
before ESO has installed its own cleanup finalizer, the API server
immediately garbage-collects the PushSecret object, and ESO never observes
the `DeletionTimestamp` — so `DeletionPolicy=Delete` never runs and the
referenced kv-v2 path is orphaned in OpenBao. The observed stuck path now
takes the per-CR form `kv-v2/openstack/keystone/{namespace}/{name}/fernet-keys`;
this was originally seen in CI run 24842115250 against the
now-legacy flat path
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
lifecycle concern (initial ExternalSecret sync, ClusterSecretStore
health, finalizer teardown) appears under the same condition type.
The condition is persisted through
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
a condition reflects the current spec or a stale generation. This applies to both
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
| `FernetKeysReady` | `reconcileFernetKeys` | Fernet Secret, script ConfigMap, CronJob, and PushSecret ensured |
| `CredentialKeysReady` | `reconcileCredentialKeys` | Credential keys Secret, script ConfigMap, CronJob, and PushSecret ensured |
| `NetworkPolicyReady` | `reconcileNetworkPolicy` | NetworkPolicy configured or not required |
| `PolicyValidReady` | `reconcilePolicyValidation` | Policy override validation passed or not required |
| `DeploymentReady` | `reconcileDeployment` | Deployment available and Service created |
| `KeystoneAPIReady` | `reconcileHealthCheck` | Keystone API responding to HTTP health check |
| `HTTPRouteReady` | `reconcileHTTPRoute` | HTTPRoute accepted by Gateway, not required (no `spec.gateway`), or Gateway API CRD missing (reason `GatewayAPINotInstalled`) |
| `HPAReady` | `reconcileHPA` | HPA configured or not required |
| `BootstrapReady` | `reconcileBootstrap` | Bootstrap Job completed successfully |
| `TrustFlushReady` | `reconcileTrustFlush` | Trust flush CronJob configured or not required |
| `PasswordRotationReady` | `reconcilePasswordRotation` | Model B admin-password rotation CronJob configured, or disabled/torn down |

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
| 0 | `ClusterSecretStore openbao-cluster-store` | Ready condition |
| 1 | DB credentials `ExternalSecret` | `spec.database.secretRef.name` |
| 2 | Admin credentials `ExternalSecret` | `spec.bootstrap.adminPasswordSecretRef.name` |

The `ClusterSecretStore` check runs first so upstream OpenBao outages surface
as `SecretsReady=False` immediately. Per-ExternalSecret `Ready` conditions
alone would mask outages up to the ESO `refreshInterval` (1h) because the
cached Secret remains valid.

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
  schema-check.
- **Brownfield mode** (`spec.database.host` set): Skips MariaDB CRs entirely and runs
  `db_sync` directly against the external database, then runs schema-check.

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
`database.EnsureDatabaseUser()`, and `job.RunJob()` (db_sync) are wrapped with
context and returned. The `DBSyncFailed` condition is set before returning the error
so that the failure reason is visible in the CR status.

**Shared library calls:** `database.EnsureDatabase()`, `database.EnsureDatabaseUser()`,
`job.RunJob()` (db_sync and schema-check)

**Dynamic credentials mode:** when `spec.database.credentialsMode: Dynamic`, the
OpenBao database engine owns the DB user lifecycle — it issues short-lived MySQL
users on demand and revokes them at lease end. `reconcileDatabase` therefore
**skips** `database.EnsureDatabaseUser()` (no MariaDB `User`/`Grant` CR is
provisioned) while still managing the schema via `database.EnsureDatabase()`. A
pre-existing operator-provisioned `User`/`Grant` from a prior Static deployment is
intentionally **not** deleted, so its grant overlaps the engine-issued logins for
a downtime-free migration; retiring the static user is a documented migration step
(see [Migrate Keystone DB to Dynamic Credentials](/guides/migrate-keystone-db-to-dynamic-credentials)).

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
   `fernet_rotate.sh` script. Uses `config.CreateImmutableConfigMap()`
   which appends a content-hash suffix and sets `immutable: true`.
4. **Ensure rotation CronJob** — Create or update `{name}-fernet-rotate` CronJob
   with the schedule from `spec.fernet.rotationSchedule`.
5. **Ensure PushSecret** — Create or update `{name}-fernet-keys-backup` PushSecret
   targeting `kv-v2/data/openstack/keystone/{namespace}/{name}/fernet-keys` in the `openbao`
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
| Suspend | `spec.fernet.suspend` (default `false`); set `true` to pause rotation during an incident without deleting the CronJob or changing its schedule |
| ServiceAccount | `{name}-fernet-rotate` |
| Init container | Copies keys from `fernet-keys-src` (Secret) to `fernet-keys` (emptyDir) |
| Command | `/scripts/fernet_rotate.sh` |
| Volume `fernet-keys-src` | Secret `{name}-fernet-keys` (read-only source, `defaultMode: 0400`) |
| Volume `fernet-keys` | emptyDir (writable working copy; init container writes files with `install -m 0400`) |
| Volume `credential-keys` | Secret `{name}-credential-keys` (read-only, required by config, `defaultMode: 0400`) |
| Volume `config` | ConfigMap `{configMapName}` |
| Volume `scripts` | ConfigMap `{name}-fernet-rotate-script-{hash}` (`defaultMode: 0555`) |
| Pod `fsGroup` | `42424` (openstack GID — grants the rotation Pod's UID 42424 group access to projected key files at mode `0400`) |
| Rationale | Mirrors upstream Keystone's `keystone.common.fernet_utils._check_key_repository`, which logs a `WARNING` when the key directory or any key file is world-readable (`stat.S_IROTH`); pinning `defaultMode: 0400` plus `fsGroup: 42424` keeps the projected files group-readable to UID 42424 only and silences the warning |

**PushSecret:**

| Field | Value |
| --- | --- |
| Name | `{name}-fernet-keys-backup` |
| Store | `ClusterSecretStore/openbao` |
| Source Secret | `{name}-fernet-keys` |
| Remote Key | `kv-v2/data/openstack/keystone/{namespace}/{name}/fernet-keys` |

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `GeneratingKeys` | "Initial Fernet keys have been generated" | `RequeueSecretPolling` (15s) |
| `True` | `FernetKeysRotated` | "rotation applied; staging secret cleared" | `RequeueSecretPolling` (15s) (transient: apply-success short-circuit in `reconcileFernetKeys`; operators see this immediately after a rotation apply via `kubectl describe`, before the next reconcile transitions to the steady-state Reason) |
| `True` | `FernetKeysAvailable` | "Fernet keys Secret exists and rotation CronJob is configured" | — |

**Versioned Script ConfigMap:**

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

#### Key Rotation RBAC Split

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
atomic-swap semantic the rotation flow requires. A strategic-merge PATCH on this field
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
with credential migration, and disaster recovery backup to OpenBao.

**Steps (in order):**

1. **Ensure credential keys Secret** — If `{name}-credential-keys` Secret does not
   exist, generate initial keys and create the Secret with a controller owner reference.
2. **Ensure rotation RBAC** — Create or update the ServiceAccount, Role, and
   RoleBinding (`{name}-credential-rotate`) for the rotation CronJob.
3. **Create script ConfigMap** — Create an immutable, versioned ConfigMap
   `{name}-credential-rotate-script-{hash}` containing the embedded
   `credential_rotate.sh` script. Uses `config.CreateImmutableConfigMap()`
   which appends a content-hash suffix and sets `immutable: true`.
4. **Ensure rotation CronJob** — Create or update `{name}-credential-rotate` CronJob
   with the schedule from `spec.credentialKeys.rotationSchedule`.
5. **Ensure PushSecret** — Create or update `{name}-credential-keys-backup` PushSecret
   targeting `kv-v2/data/openstack/keystone/{namespace}/{name}/credential-keys` in the `openbao`
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
| Suspend | `spec.credentialKeys.suspend` (default `false`); set `true` to pause rotation during an incident without deleting the CronJob or changing its schedule |
| ServiceAccount | `{name}-credential-rotate` |
| Init container | Copies keys from `credential-keys-src` (Secret) to `credential-keys` (emptyDir) |
| Command | `/scripts/credential_rotate.sh` |
| Volume `credential-keys-src` | Secret `{name}-credential-keys` (read-only source, `defaultMode: 0400`) |
| Volume `credential-keys` | emptyDir (writable working copy; init container writes files with `install -m 0400`) |
| Volume `fernet-keys` | Secret `{name}-fernet-keys` (read-only, required by config, `defaultMode: 0400`) |
| Volume `config` | ConfigMap `{configMapName}` |
| Volume `scripts` | ConfigMap `{name}-credential-rotate-script-{hash}` (`defaultMode: 0555`) |
| Pod `fsGroup` | `42424` (openstack GID — grants the rotation Pod's UID 42424 group access to projected key files at mode `0400`) |
| Rationale | Mirrors upstream Keystone's `keystone.common.fernet_utils._check_key_repository`, which logs a `WARNING` when the key directory or any key file is world-readable (`stat.S_IROTH`); pinning `defaultMode: 0400` plus `fsGroup: 42424` keeps the projected files group-readable to UID 42424 only and silences the warning |

> **Note:** The `credential_rotate.sh` script runs both `credential_rotate` and
> `credential_migrate`. The migrate step re-encrypts existing credentials in the
> database with the new primary key, which is critical to prevent data loss when
> old keys are eventually purged.

**PushSecret:**

| Field | Value |
| --- | --- |
| Name | `{name}-credential-keys-backup` |
| Store | `ClusterSecretStore/openbao` |
| Source Secret | `{name}-credential-keys` |
| Remote Key | `kv-v2/data/openstack/keystone/{namespace}/{name}/credential-keys` |

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `GeneratingKeys` | "Initial credential keys have been generated" | `RequeueSecretPolling` (15s) |
| `True` | `CredentialKeysRotated` | "rotation applied; staging secret cleared" | `RequeueSecretPolling` (15s) (transient: apply-success short-circuit in `reconcileCredentialKeys`; operators see this immediately after a rotation apply via `kubectl describe`, before the next reconcile transitions to the steady-state Reason) |
| `True` | `CredentialKeysAvailable` | "Credential keys Secret exists and rotation CronJob is configured" | — |

**Versioned Script ConfigMap:**

Uses the same versioned, immutable ConfigMap pattern as `reconcileFernetKeys`. See the
description in that section for details on content-hash naming and immutability.

**Error handling:** Errors from Secret creation, RBAC ensure, script ConfigMap creation,
CronJob ensure, or PushSecret ensure are wrapped with context and returned directly.
No requeue delays — errors trigger controller-runtime exponential backoff.

**Idempotency:** If the `{name}-credential-keys` Secret already exists, it is not
modified. This prevents overwriting keys that have been rotated by the CronJob.

**Shared library calls:** `config.CreateImmutableConfigMap()`, `job.EnsureCronJob()`,
`secrets.EnsurePushSecret()`

#### Key Rotation RBAC Split

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
commits the Secret swap — no data loss when old keys age out.

> **Key-rollover window.** There
> is a ~60s window between `credential_migrate` completion and the kubelet
> refreshing the in-place Secret projection. During that window,
> running Keystone pods still have the old credential keyset mounted —
> database rows are already encrypted under the new primary, but the pods
> cannot decrypt them yet. This is an inherent property of the rotation
> flow and is tracked as a known limitation; it should be considered when
> sizing the rotation schedule against request volume.

**Production `Secret.Data` field ownership.** Same contract as Fernet: the
operator owns the production `.data` map, writes are `Update`-based under
the controller-owned `ResourceVersion`, and the Update fully replaces the
map so stale indices are removed atomically. Strategic-merge PATCH
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
> `(string, error)` instead of `(ctrl.Result, error)`, and `reconcileConfig`
> itself sets no status condition and requests no requeue. Its pipeline step
> wraps the call so that a failure flips `SecretsReady=False` (via
> `markConfigFailed`); otherwise the aggregate `Ready` would re-aggregate the
> still-True sub-conditions and persist a stale `Ready=True` at the new
> generation. `SecretsReady` is reused rather than introducing a
> dedicated `ConfigReady`, matching the `subReconcilerConditionTypes`
> `"Config" -> "SecretsReady"` mapping.

**Configuration Pipeline:**

```text
Spec fields → Operator defaults → Secret injection → Plugin config merge
  → ExtraConfig merge → Policy override → INI rendering → Immutable ConfigMap
```

**Step 1: Build operator defaults**

The following INI sections are generated from CRD spec fields:

| Section | Key | Value Source |
| --- | --- | --- |
| `DEFAULT` | `use_stderr` | `true` (operator default) |
| `DEFAULT` | `debug` | `spec.logging.debug` |
| `DEFAULT` | `default_log_levels` | Deterministic CSV from `spec.logging.perLoggerLevels`, omitted when empty |
| `DEFAULT` | `log_config_append` | `/etc/keystone/keystone.conf.d/logging.conf` — emitted only when `spec.logging.format == "json"` |
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

- The password is always read from `spec.database.secretRef` via
  `secrets.GetSecretValue()`. The username source depends on the mode: in
  **Static** managed mode it is derived from the CR name (the MariaDB `User` CR
  name); in **brownfield** and **Dynamic** managed mode it is read from the
  Secret's `username` key (the engine issues an ephemeral username alongside the
  password). A missing `username` key sets `SecretsReady=False` with reason
  `WaitingForDBCredentials`.
- Default port is 3306 when `spec.database.port` is 0.
- `reconcileDBConnectionSecret` returns the SHA-256 of the assembled DSN, which
  the Deployment step stamps as the `keystone.c5c3.io/db-connection-hash`
  pod-template annotation in Dynamic mode (see the Deployment section) so a
  rotated engine-issued credential rolls the Deployment.

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
| Data keys | `keystone.conf`, `api-paste.ini`, `policy.yaml` (if `spec.policyOverrides` set), `logging.conf` (if `spec.logging.format == "json"`) |
| Immutable | `true` |

When `spec.logging.format == "json"`, the `logging.conf` data key wires
`oslo_log.formatters.JSONFormatter` to a stderr `StreamHandler` and Step 1 emits
`[DEFAULT].log_config_append` pointing at the on-pod path so oslo.log loads it on
startup. Toggling back to `format: "text"` drops both the data
key and the `log_config_append` entry, changing the content hash and rolling the
Deployment.

#### `renderLoggingConf` template deviation

`renderLoggingConf` (`reconcile_config.go:renderLoggingConf`) intentionally
deviates from the template sketched in the design brief on two points,
and the deviation is locked in by
`TestReconcileConfig_LoggingJSONPlusPerLoggerLevels`:

- The `[loggers]` section emits `keys = root` only — there is no
  `[logger_keystone]` (or any other named-logger) subsection. This makes
  `spec.logging.perLoggerLevels` (rendered as `[DEFAULT].default_log_levels`)
  the single config surface that owns per-logger filtering. Splitting that
  responsibility between a `[logger_<name>]` block in `logging.conf` and the
  CSV in `keystone.conf` would create two interleaved sources of truth and
  invite drift between them on future template edits.
- `[handler_stderr]` pins `level = NOTSET` rather than mirroring the root
  logger's level. The handler therefore emits every record the root logger
  forwards; hardcoding the level here would silently shadow
  `spec.logging.level` for all records that do not also clear an explicit
  per-handler filter.

These shape constraints are intentional and load-bearing — do not "fix" the
renderer to match the original four-section template without first updating
the corresponding test invariants.

**Error handling:** All errors are wrapped with context and returned. No conditions
are set by this sub-reconciler.

**Shared library calls:** `secrets.GetSecretValue()`,
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
Deployment is updated. This sub-reconciler gates the Deployment rollout:
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
| TTLSecondsAfterFinished | not set |
| RestartPolicy | Never |
| SecurityContext | `restrictedSecurityContext()` (PSS Restricted) |
| TerminationMessagePolicy | `FallbackToLogsOnError` |

**Volume Mounts:**

| Volume Name | Mount Path | Source | ReadOnly |
| --- | --- | --- | --- |
| `config` | `/etc/keystone/keystone.conf.d/` | ConfigMap `{configMapName}` | Yes |

**Error Extraction (`getValidationErrorMessage`):**

When a validation Job fails, the reconciler extracts a descriptive error message from the
failed Pod's termination message:

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

**In-Place Key Rotation:**

Fernet and credential key rotation is handled in-place via kubelet Secret
projection. When the rotation CronJob updates a Secret, the kubelet
automatically projects the new data into running pods without requiring a
Deployment rollout. The pod template does not include hash annotations for the
fernet/credential key Secrets, so those Secret changes do not trigger rolling
restarts. This preserves Keystone availability, PDB budget, and uWSGI/Memcached
connections during routine key rotation.

```text
CronJob rotates keys → Secret data changes → kubelet projects new keys
  → running pods see updated key files (no rollout)
```

The database connection string is the deliberate exception. It is consumed via
the `OS_DATABASE__CONNECTION` environment variable (not a mounted volume), so a
rotated credential only takes effect on a Pod restart. In **Dynamic** credentials
mode (`spec.database.credentialsMode: Dynamic`) the DSN rotates at the engine
lease TTL, so `reconcileDeployment` stamps a `keystone.c5c3.io/db-connection-hash`
pod-template annotation (the SHA-256 of the DSN returned by
`reconcileDBConnectionSecret`) to roll the Deployment when the engine-issued
credential changes. Static and brownfield modes leave the annotation absent,
preserving the no-rollout behavior.

**Deployment Spec:**

| Field | Value |
| --- | --- |
| Name | `{name}` (bare CR name) |
| Replicas | `spec.deployment.replicas`; left `nil` when `spec.autoscaling` is set, so the HorizontalPodAutoscaler owns the count and the operator does not reset it each reconcile |
| Labels | `app.kubernetes.io/name=keystone`, `app.kubernetes.io/instance={name}`, `app.kubernetes.io/managed-by=keystone-operator` |
| Selector | `app.kubernetes.io/name=keystone`, `app.kubernetes.io/instance={name}` |
| Container name | `keystone` |
| Image | `{spec.image.repository}:{spec.image.tag}` |
| Port | 5000 (named `keystone`) |

**Probes:**

| Probe | Type | Target | Port | InitialDelay | Period |
| --- | --- | --- | --- | --- | --- |
| Liveness | TCPSocket | — | 5000 | 15s | 20s |
| Readiness | HTTPGet | `/v3` | 5000 | 5s | 10s |

The liveness and readiness probes are intentionally separated. The liveness probe uses a TCP socket check that only verifies the uWSGI process is accepting connections, without exercising the database code path. This prevents the kubelet from killing pods during transient database outages (e.g., MariaDB maintenance), avoiding CrashLoopBackOff cascades and thundering-herd restarts. The readiness probe continues to use HTTP GET `/v3`, which exercises the full stack including the database. When the database is unavailable, the readiness probe fails and the pod is removed from Service endpoints, preventing HTTP 500 responses to clients. Once the database recovers, the pod re-enters Service endpoints within one readiness probe period (10s).

**Volume Mounts:**

| Volume Name | Mount Path | Source | ReadOnly |
| --- | --- | --- | --- |
| `config` | `/etc/keystone/keystone.conf.d/` | ConfigMap `{configMapName}` | Yes |
| `fernet-keys` | `/etc/keystone/fernet-keys/` | Secret `keystone-fernet-keys` (`defaultMode: 0400`) | Yes |
| `credential-keys` | `/etc/keystone/credential-keys/` | Secret `keystone-credential-keys` (`defaultMode: 0400`) | Yes |

**Pod-level Security:**

| Field | Value |
| --- | --- |
| `spec.template.spec.securityContext.fsGroup` | `42424` (openstack GID — grants the API Pod's UID 42424 group access to projected key files at mode `0400`) |
| Rationale | Mirrors upstream Keystone's `keystone.common.fernet_utils._check_key_repository`, which logs a `WARNING` when the key directory or any key file is world-readable (`stat.S_IROTH`); pinning `defaultMode: 0400` plus `fsGroup: 42424` keeps the projected files group-readable to UID 42424 only and silences the warning |

**Service Spec:**

| Field | Value |
| --- | --- |
| Name | `{name}` (bare CR name) |
| Selector | `app.kubernetes.io/name=keystone`, `app.kubernetes.io/instance={name}` |
| Port | 5000 TCP |

**Status Endpoint:**

When the Deployment becomes ready, `status.endpoint` is set via the
`keystoneStatusEndpoint` helper (defined in `reconcile_httproute.go`):

| `spec.gateway` | Resulting `status.endpoint` |
| --- | --- |
| nil | `http://{name}.{namespace}.svc.cluster.local:5000/v3` |
| set (hostname `api.example.com`) | `https://api.example.com/v3` |

The helper is owned by `reconcile_httproute.go` but invoked from
`reconcileDeployment` because the endpoint must be populated before the
`reconcileHealthCheck` step reads it. `reconcileHTTPRoute` runs later in the
sequence and does not mutate `status.endpoint`.

**PodDisruptionBudget:**

After ensuring the Deployment and Service, `reconcileDeployment` creates or updates
a PodDisruptionBudget via `deployment.EnsurePDB()`. The PDB uses a replica-aware
disruption budget strategy:

| Replicas | Field | Value | Rationale |
| --- | --- | --- | --- |
| `> 1` | `minAvailable` | `1` | Guarantees at least one pod remains during voluntary disruptions |
| `<= 1` | `maxUnavailable` | `1` | Avoids drain deadlock — a PDB with `minAvailable=1` on a single-replica deployment would block all evictions |

| PDB Field | Value |
| --- | --- |
| Name | `{name}` |
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
of `{name}-config-{hash}` ConfigMaps in the namespace.

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
Keystone API outside the cluster via a pre-existing `Gateway`. The
Gateway is owned by the platform team; this sub-reconciler only ensures the
route that attaches the Keystone Service to it. Four lifecycle paths:

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
   existing `{name}` HTTPRoute and set `HTTPRouteReady=True` with reason
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
so the backend Service (`{name}`) is guaranteed to exist before the HTTPRoute
references it, and before `reconcileHealthCheck` reads `status.endpoint`. The
route is deliberately not part of `reconcileParallelGroup` because it has a
transitive dependency on the Service created by `reconcileDeployment`.

**HTTPRoute Construction (`buildKeystoneHTTPRoute`):**

| HTTPRoute Field | Source |
| --- | --- |
| `metadata.name` | `{name}` (shared with the Keystone API Service) |
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
| `spec.rules[0].backendRefs[0].name` | `{name}` |
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

**Acceptance Detection (`isHTTPRouteAccepted`):** Iterates
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

**`status.endpoint` Derivation:** The gateway-aware helper
`keystoneStatusEndpoint()` is defined alongside this sub-reconciler but called
from `reconcileDeployment`. When `spec.gateway.hostname` is set, the helper
returns `https://{hostname}/v3`; otherwise it returns the cluster-local
`http://{name}.{namespace}.svc.cluster.local:5000/v3`. The gateway path
prefix from `spec.gateway.path` is used for HTTPRoute routing only; it is not
appended to `status.endpoint`. The `https` scheme is emitted unconditionally
when a gateway hostname is configured — gateways are the public-ingress hop
and terminate TLS.

**Interaction with `reconcileNetworkPolicy`:** When `spec.gateway`
is set, `reconcileNetworkPolicy` appends an extra ingress peer that selects
the gateway's namespace by the well-known label
`kubernetes.io/metadata.name={parentRef.namespace or CR namespace}`. This lets
Gateway data-plane pods reach the Keystone API Service on port 5000 without
widening the base NetworkPolicy. The peer is added only while `spec.gateway`
is non-nil and is removed automatically on the next reconcile after
`spec.gateway` is cleared. The operator's own namespace is whitelisted by a
separate, always-on ingress peer (see [`reconcileHealthCheck`](#reconcilehealthcheck)),
independent of `spec.gateway`.

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
reports ready, verifying that the API is actually responding to requests.
This catches cases where pods pass their readiness probe but the API is not
functionally healthy.

**Endpoint:** Uses `keystone.Status.Endpoint` which is set by `reconcileDeployment`
(e.g., `http://keystone.{namespace}.svc.cluster.local:5000/v3`). If the endpoint
is empty (not yet configured), the health check sets `KeystoneAPIReady=False` with
reason `EndpointNotReady` and requeues.

**NetworkPolicy interaction:** The operator pod runs in a dedicated namespace
(e.g. `keystone-system`) distinct from the workload namespace, so a
`spec.networkPolicy` whose ingress admits only the user-declared sources would
block this health check and pin `KeystoneAPIReady=False` for a healthy
deployment. To prevent that, `reconcileNetworkPolicy` appends an always-on
ingress peer for the operator namespace — resolved at startup from
`POD_NAMESPACE` or the mounted ServiceAccount namespace file
(`DetectOperatorNamespace`) — so a correctly deployed operator can always reach
the API on TCP 5000. On the egress side the same NetworkPolicy auto-derives
rules for DNS, the kube-apiserver (used by the rotation CronJob pods that share
the policy's pod selector), the database (port from `spec.database.port`), and
the cache, in both managed and brownfield modes, so neither the readiness probe
nor the scheduled key rotations are blocked. See
[NetworkPolicySpec](./keystone-crd.md#networkpolicyspec) for the full rule set.

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

**Purpose:** Manage the HorizontalPodAutoscaler for the Keystone API Deployment. Three lifecycle paths:

1. **Autoscaling disabled** (`spec.autoscaling` is nil): Delete any existing HPA
   and set `HPAReady=True` with reason `HPANotRequired`.
2. **Autoscaling enabled** (`spec.autoscaling` is set): Build the desired HPA via
   `buildKeystoneHPA()` and call `deployment.EnsureHPA()` to create or update it.
   Set `HPAReady=True` with reason `HPAReady`.
3. **Error**: Propagate errors from ensure/delete operations with descriptive context.

**HPA Construction (`buildKeystoneHPA`):**

| HPA Field | Value |
| --- | --- |
| Name | `{name}` |
| Labels | `commonLabels(keystone)` |
| `scaleTargetRef.apiVersion` | `apps/v1` |
| `scaleTargetRef.kind` | `Deployment` |
| `scaleTargetRef.name` | `{name}` |
| `minReplicas` | `spec.autoscaling.minReplicas` (falls back to `spec.deployment.replicas` when nil) |
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
| TTLSecondsAfterFinished | not set |
| RestartPolicy | OnFailure |

> **Note:** `TTLSecondsAfterFinished` is intentionally unset. The completed Job
> lingers as the `job.RunJob` pod-spec-hash state record and is garbage-collected
> via its `ownerReference` when the Keystone CR is deleted. Setting a TTL would let
> the TTL-after-finished controller delete the finished Job and trigger re-creation
> on the next reconcile.

**Bootstrap Arguments:**

| Argument | Value Source |
| --- | --- |
| `--bootstrap-password` | `$(BOOTSTRAP_PASSWORD)` env var from `spec.bootstrap.adminPasswordSecretRef` Secret, key `password` |
| `--bootstrap-admin-url` | `http://keystone.{namespace}.svc.cluster.local:5000/v3` |
| `--bootstrap-internal-url` | `http://keystone.{namespace}.svc.cluster.local:5000/v3` |
| `--bootstrap-public-url` | `http://keystone.{namespace}.svc.cluster.local:5000/v3` |
| `--bootstrap-region-id` | `spec.bootstrap.region` |

**Admin-password rotation re-run:** Before building the Job,
the reconciler reads the `password` key of the admin Secret named by
`spec.bootstrap.adminPasswordSecretRef.Name` and stamps its digest —
`hex(SHA-256(password))` — onto the bootstrap Job's pod template as the
`forge.c5c3.io/admin-password-hash` annotation. Because `job.PodSpecHash` hashes
the **full** `PodTemplateSpec` (metadata included), a rotated password changes
this annotation and therefore the `forge.c5c3.io/pod-spec-hash` gate. On the next
reconcile `job.RunJob` sees the desired hash diverge from the completed
`{name}-bootstrap` Job's stored hash, deletes the stale Job, and recreates it so
`keystone-manage bootstrap` re-runs against the new admin credentials. A missing
or unreadable admin Secret, an absent `password` key, or an empty value is a hard
precondition failure: the reconciler sets `BootstrapReady=False` with reason
`AdminSecretInvalid`, emits a `Warning` event, and returns the error (requeue
with backoff) rather than building a Job with empty credentials.

**Pod-template annotation:** `forge.c5c3.io/admin-password-hash` is written to the
Job's `Spec.Template.ObjectMeta.Annotations` (the pod template), not the Job's own
metadata, so it participates in `job.PodSpecHash`. See the
[Labels and Annotations](#labels-and-annotations) table for the cross-reconciler
contract; the backing const is the unexported `adminPasswordHashAnnotation` in
`reconcile_bootstrap.go`.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter |
| --- | --- | --- | --- |
| `False` | `AdminSecretInvalid` | "Admin password Secret {ns}/{name} is missing, unreadable, or has an empty \"password\" value" | — (error returned) |
| `False` | `BootstrapFailed` | "Keystone bootstrap job failed: {error}" | — (error returned) |
| `False` | `BootstrapInProgress` | "Keystone bootstrap job is running" | 60s |
| `True` | `BootstrapComplete` | "Keystone bootstrap completed successfully" | — |

**Error handling:** The `AdminSecretInvalid` and `BootstrapFailed` conditions are
set before returning the error, so the failure reason is visible in the CR status
even when the error triggers controller-runtime backoff.

**Idempotency:** The bootstrap Job is idempotent — `keystone-manage bootstrap` can be
run multiple times without side effects. This idempotency is what makes the
admin-password-triggered re-run above safe: re-running bootstrap against an
already-bootstrapped database simply re-applies the (now rotated) admin credentials
rather than failing or duplicating the initial admin user, project, roles, and
catalog entries.

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
delegations from the Keystone database. Three lifecycle paths:

1. **Trust flush configured** (`spec.trustFlush` is set — the production path,
   including default-on objects materialized by the webhook): Build the desired
   CronJob via `trustFlushCronJob()` and call `job.EnsureCronJob()` to create or
   update it. Set `TrustFlushReady=True` with reason `TrustFlushReady`. On a
   webhook-enabled cluster the CR always reaches the reconciler in this state
   because the defaulting webhook materializes
   `spec.trustFlush = {schedule: "0 * * * *", suspend: false}` whenever the
   field is unset. Setting `spec.trustFlush.suspend: true` keeps this path:
   the CronJob is ensured with `spec.suspend: true` and the condition reason
   remains `TrustFlushReady`.
2. **Legacy bypass — webhook did not run** (`spec.trustFlush` is nil): Reachable
   only on envtest fixtures or other clusters where the defaulting webhook is
   not wired up, or when programmatic callers (envtest/unit tests) invoke
   `reconcileTrustFlush` directly without going through admission. The
   reconciler emits (a) a structured log entry with `reason=webhook-bypass`
   plus the CR's `namespace`/`name` for log pipelines and (b) a Kubernetes
   Warning Event of reason `TrustFlushBypass` on the Keystone CR so the
   bypass surfaces in `kubectl describe`. It then deletes any existing
   `{name}-trust-flush` CronJob and sets `TrustFlushReady=True` with reason
   `TrustFlushNotRequired` and a message identifying the bypass posture.
   Production CRs never enter this branch.
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

Invoked only on the legacy-bypass path (`spec.trustFlush` is `nil`).
`deleteCronJob()` issues a `client.Delete` for the CronJob by name
and uses `client.IgnoreNotFound` so the operation is a no-op if the CronJob
does not exist (e.g., first reconciliation of an envtest CR that never had a
trust-flush CronJob).

**Condition Contract:**

| Status | Reason | Message | RequeueAfter | Path |
| --- | --- | --- | --- | --- |
| `True` | `TrustFlushNotRequired` | "Trust flush bypass: spec.trustFlush is nil (webhook did not default this object)" | — | Legacy bypass only — `spec.trustFlush == nil` |
| `True` | `TrustFlushReady` | "Trust flush CronJob is configured" | — | Production path (default-on or explicit) |

**Error handling:** Errors from `job.EnsureCronJob()` are wrapped with
"ensuring trust flush CronJob" context. Errors from `deleteCronJob()` are wrapped
with "deleting trust flush CronJob" context. Both are returned directly to
controller-runtime for exponential backoff.

**Shared library calls:** `job.EnsureCronJob()`

---

### reconcilePasswordRotation

**File:** `operators/keystone/internal/controller/reconcile_passwordrotation.go`

**Signature:**

```go
func (r *KeystoneReconciler) reconcilePasswordRotation(ctx context.Context,
    keystone *keystonev1alpha1.Keystone, _ string) (ctrl.Result, error)
```

The third parameter is the shared config ConfigMap name. It is accepted only for
sub-reconciler call-site symmetry with `reconcileFernetKeys` and
`reconcileTrustFlush` — the controller wires this sub-reconciler as
`instrumentSubReconciler(ctx, "PasswordRotation", ...)` passing `configMapName`.
It is intentionally unused (named `_`) because the rotate
script never runs `keystone-manage` and therefore needs no keystone
configuration.

**Purpose:** Drive Model B scheduled admin-password rotation. A CronJob mints a fresh strong password and PATCHes it onto a
staging Secret; the operator validates and commits it onto an operator-owned
push-source Secret; a PushSecret mirrors that to OpenBao at the per-CR path
`bootstrap/{keystone.Namespace}/{keystone.Name}/admin`; External Secrets
Operator (ESO) then syncs it back into the admin Secret and
[`reconcileBootstrap`](#reconcilebootstrap) re-runs `keystone-manage bootstrap`
(admin-password-hash gate) to apply it.

Two lifecycle paths:

1. **Disabled / teardown** (`spec.passwordRotation` is nil OR
   `enabled: false`): `teardownPasswordRotation` deletes every Model B resource
   (CronJob, staging Secret, push-source Secret, ServiceAccount/Role/RoleBinding,
   PushSecret, and all hash-suffixed script ConfigMaps), every delete tolerating
   `NotFound` so teardown is idempotent and safe on a CR that never enabled
   rotation. Sets `PasswordRotationReady=True` with reason `RotationDisabled`. A
   nil pointer means the feature was never opted into (the defaulting webhook
   deliberately does not materialize it); `enabled: false` means it was switched
   off — both branches tear down. The PushSecret uses `DeletionPolicy=None` (see
   below), so its deletion leaves the last-pushed password intact in OpenBao —
   disabling rotation never locks the admin out.
2. **Enabled**: the ordered steps below.

**Steps (in order):**

1. **Ensure push-source Secret** — `ensureAdminPasswordPushSourceSecret` ensures
   the operator-owned `{name}-admin-password-next` Secret exists (metadata and
   owner reference only). Its `.data` is owned exclusively by
   `applyAdminPasswordRotation` and is never clobbered on reconcile.
2. **Ensure staging Secret** — `ensureStagingSecret(..., "admin-password")`
   ensures `{name}-admin-password-rotation`, which the CronJob PATCHes rotated
   passwords into.
3. **Refresh rotation-age gauge** — `observeRotationAge(..., key_type
   "admin-password")` refreshes the rotation-age gauge from the push-source
   Secret's `forge.c5c3.io/rotation-completed-at` annotation, falling back to the
   staging Secret during the pre-first-apply window. Called before the apply so
   the next reconcile picks up the freshest timestamp once the apply re-stamps
   the push-source annotation.
4. **Apply any completed rotation** — `applyAdminPasswordRotation` validates and
   commits a staged rotation if one is present. On a
   successful apply it sets `PasswordRotationReady=True` with reason
   `AdminPasswordRotated` ("rotation applied; staging secret cleared") and
   short-circuits with `Requeue: true` so the next pass re-enters the happy path
   with the push-source Secret already updated.
5. **Ensure rotation RBAC** — `ensureAdminPasswordRotationRBAC` creates or
   updates the split ServiceAccount, Role, and RoleBinding
   (`{name}-admin-password-rotate`).
6. **Create script ConfigMap** — `config.CreateImmutableConfigMap()` of the
   embedded `admin_password_rotate.sh` script into
   `{name}-admin-password-rotate-script-{hash}`.
7. **Ensure rotation CronJob** — `job.EnsureCronJob()` creates or updates
   `{name}-admin-password-rotate`.
8. **Clobber-safe PushSecret** — only ensure `adminPasswordPushSecret` once
   `adminPasswordPushSourceReady` reports the push-source Secret holds a valid
   password (≥ minLength). Before the first rotation completes the push-source is
   empty, and pushing it would overwrite the seeded per-CR
   `bootstrap/{keystone.Namespace}/{keystone.Name}/admin` value with nothing.
9. **Report ready** — sets `PasswordRotationReady=True` with reason
   `PasswordRotationConfigured` ("Admin password rotation CronJob is
   configured").

**Rotation CronJob (`adminPasswordRotationCronJob`):**

| Field | Value |
| --- | --- |
| Name | `{name}-admin-password-rotate` |
| Labels | `commonLabels(keystone)` |
| Schedule | `spec.passwordRotation.schedule` |
| Suspend | `&spec.passwordRotation.suspend` (pointer to CRD bool) |
| ServiceAccount | `{name}-admin-password-rotate` |
| Container name | `admin-password-rotate` |
| Image | `{spec.image.repository}:{spec.image.tag}` |
| Command | `["/scripts/admin_password_rotate.sh"]` |
| SecurityContext | `restrictedSecurityContext()` (PSS Restricted) |
| RestartPolicy | `OnFailure` |
| Env `SECRET_NAME` | `{name}-admin-password-rotation` (staging Secret) |
| Env `SECRET_NAMESPACE` | `fieldRef` to `metadata.namespace` |
| Env `PASSWORD_LENGTH` | `normalizedAdminPasswordLength` (webhook default 32, defense-in-depth floor 24) |
| Volume `scripts` | ConfigMap `{name}-admin-password-rotate-script-{hash}` (`defaultMode: 0555`), mounted read-only at `/scripts` |

The CronJob mounts **only** the rotation script — no keystone configuration and
no key repositories — because it never runs `keystone-manage`. `SECRET_NAME`
points only at the staging Secret; the CronJob ServiceAccount can never patch the
push-source Secret (see the RBAC split below).

**PushSecret (`adminPasswordPushSecret`):**

| Field | Value |
| --- | --- |
| Name | `{name}-admin-password-backup` |
| Store | `ClusterSecretStore/openbao-cluster-store` |
| Source Secret | `{name}-admin-password-next` (push-source) |
| Remote Key | `bootstrap/{keystone.Namespace}/{keystone.Name}/admin` (per-CR path) |
| Property | `password` |
| DeletionPolicy | `None` |

The `RemoteKey` is CR-scoped to `bootstrap/{keystone.Namespace}/{keystone.Name}/admin`,
so each Model-B-enabled Keystone CR writes its admin password to
its own OpenBao object and multiple Keystone CRs never collide on a shared
`bootstrap/keystone-admin` key. `DeletionPolicy=None` is chosen because this is a
per-Keystone-CR **persistent** bootstrap secret: keeping the last-pushed password in
OpenBao on teardown (or when rotation is disabled — the teardown path deletes this
PushSecret) means re-adoption works and the admin is never locked out.

**Condition Contract:**

| Status | Reason | Message | RequeueAfter | Path |
| --- | --- | --- | --- | --- |
| `True` | `RotationDisabled` | "Scheduled admin password rotation is disabled; Model B resources removed" | — | Disabled/teardown — `spec.passwordRotation` nil or `enabled: false` |
| `True` | `AdminPasswordRotated` | "rotation applied; staging secret cleared" | — (transient: apply-success short-circuit, requeues — operators see this immediately after an apply via `kubectl describe`, before the next reconcile transitions to steady state) | Enabled path |
| `True` | `PasswordRotationConfigured` | "Admin password rotation CronJob is configured" | — | Enabled steady state |

**Error handling:** Errors from the teardown, push-source/staging Secret ensure,
the rotation apply, RBAC ensure, script ConfigMap creation, CronJob ensure, or
PushSecret ensure are wrapped with context and returned directly. No requeue
delays — errors trigger controller-runtime exponential backoff.

**Idempotency:** The push-source Secret's metadata is reconciled but its `.data`
is never touched outside `applyAdminPasswordRotation`, so a reconcile never
clobbers a previously committed password. The script ConfigMap uses
content-based naming — an unchanged script returns the existing ConfigMap name.
All teardown deletes tolerate `NotFound`. The apply step is a no-op unless a
completed rotation is staged, so reconciles in the steady state converge without
side effects.

**Shared library calls:** `config.CreateImmutableConfigMap()`,
`job.EnsureCronJob()`, `secrets.EnsurePushSecret()`

#### Admin Password Rotation RBAC Split

The rotation path separates the **compute** of the new password (performed by
the rotation CronJob) from the **write** onto the push-source Secret and the
commit (performed by the operator). `ensureAdminPasswordRotationRBAC` creates a
ServiceAccount, Role, and RoleBinding all named `{name}-admin-password-rotate`,
mirroring `ensureFernetRotationRBAC`'s split shape.

**Staging Secret naming.** Per `adminPasswordStagingSecretName`, the staging
Secret is `{keystone.Name}-admin-password-rotation`. It is created and owned by
the operator via `ensureStagingSecret`:

- Empty `Data` on creation; the CronJob PATCHes `Data` on rotation.
- Labels: `commonLabels(keystone)` + `forge.c5c3.io/rotation-target=admin-password`
  (see [Labels and Annotations](#labels-and-annotations)).
- Owner reference: the Keystone CR (garbage-collected with the CR).

**Split Role.** The Role has **two** PolicyRules:

- Rule 1 — `get` on the push-source Secret `{name}-admin-password-next`.
- Rule 2 — `get`, `patch` scoped to the staging Secret
  `{name}-admin-password-rotation`.

The CronJob ServiceAccount has **no `create`, `update`, or `delete`** on either
Secret. The operator — not the CronJob — writes the push-source Secret via
`applyAdminPasswordRotation`'s GET-then-`Update` full replacement, keeping the
token-forgery primitive (write access to a Secret a privileged workload consumes)
out of the CronJob's attack surface.

**RBAC verb matrix.** Two principals touch the push-source and staging Secrets,
with strictly disjoint capabilities:

| Principal | Resource | Verbs | Source |
| --- | --- | --- | --- |
| CronJob ServiceAccount (`{name}-admin-password-rotate`) | Secret `{name}-admin-password-next` (push-source) | `get` | Role rule 1 in `ensureAdminPasswordRotationRBAC` |
| CronJob ServiceAccount (`{name}-admin-password-rotate`) | Secret `{name}-admin-password-rotation` (staging) | `get`, `patch` | Role rule 2 in `ensureAdminPasswordRotationRBAC` |
| Operator ServiceAccount | Secret `{name}-admin-password-next` (push-source) | `get`, `create`, `update`, `patch`, `delete`, `list`, `watch` | Cluster-scoped core `secrets` verbs (see [RBAC Permissions](#rbac-permissions)) |
| Operator ServiceAccount | Secret `{name}-admin-password-rotation` (staging) | `get`, `create`, `update`, `patch`, `delete`, `list`, `watch` | Cluster-scoped core `secrets` verbs |

**Operator output contract.** `validateAdminPasswordRotationOutput` requires a non-empty `password` value of at least minLength bytes, where
minLength is the normalized `PasswordLength` (webhook default 32, defense-in-depth
floor 24 via `adminPasswordMinLength`). On rejection the operator emits a Warning
event `AdminPasswordRotationRejected` and **retains the staging Secret** for
inspection. A malformed `forge.c5c3.io/rotation-completed-at` value emits a
Warning event `AdminPasswordRotationAnnotationInvalid` and likewise retains
staging. The password value is never logged or echoed in events.

**Apply algorithm.** On a valid staging Secret, `applyAdminPasswordRotation`:

1. GETs the staging Secret (absent ⇒ no-op).
2. Requires `forge.c5c3.io/rotation-completed-at` to be present and RFC3339-parseable.
3. Validates the staged password.
4. GETs the push-source Secret, replaces its `.data` verbatim, stamps the
   `rotation-completed-at` annotation, and issues an `Update` under the
   controller-owned `ResourceVersion` (optimistic concurrency).
5. DELETEs the staging Secret (tolerating `NotFound`).
6. Emits a Normal event `AdminPasswordRotated`.

UPDATE-then-DELETE ordering is deliberate: the push-source Secret is the durable
record of the last successful rotation timestamp once staging is deleted, so the
rotation-age gauge can refresh on every reconcile.

**Downstream consumer.** [`reconcileBootstrap`](#reconcilebootstrap) is the
consumer of the rotated password: once ESO syncs the new value from
`bootstrap/{keystone.Namespace}/{keystone.Name}/admin` into the admin Secret, the bootstrap reconciler's
`forge.c5c3.io/admin-password-hash` gate re-runs the idempotent bootstrap Job to
apply it. See the [Labels and Annotations](#labels-and-annotations) entries for
`forge.c5c3.io/rotation-target` (value `admin-password` for the staging Secret),
`forge.c5c3.io/rotation-completed-at`, and `forge.c5c3.io/admin-password-hash`.

---

## Error Handling Summary

| Sub-Reconciler | Transient State | RequeueAfter | Permanent Failure |
| --- | --- | --- | --- |
| `reconcileSecrets` | ESO not synced | 15s | API error → exponential backoff |
| `reconcileDatabase` | MariaDB CRs not ready | 30s | `ErrJobFailed` from db_sync |
| `reconcileDatabase` | db_sync running | 30s | API error → exponential backoff |
| `reconcileFernetKeys` | Initial key generation / rotation applied | 15s | API error → exponential backoff |
| `reconcileCredentialKeys` | Initial key generation / rotation applied | 15s | API error → exponential backoff |
| `reconcileNetworkPolicy` | — | — | API error → exponential backoff |
| `reconcileConfig` | — | — | Secret read / render / ConfigMap failure → `SecretsReady=False`, exponential backoff |
| `reconcilePolicyValidation` | Job running | 15s | `ErrJobFailed` from validation → descriptive error |
| `reconcileDeployment` | Deployment not available | 10s | API error → exponential backoff |
| `pruneStaleConfigMaps` | — | — | List/delete failure → `SecretsReady=False`, exponential backoff |
| `reconcileHTTPRoute` | Gateway has not yet set `Accepted=True` on parent status | 10s | API error on ensure/get/delete → exponential backoff |
| `reconcileHealthCheck` | Non-2xx, timeout, DNS, connection refused | 10s | Malformed URL → exponential backoff |
| `reconcileHPA` | — | — | API error → exponential backoff |
| `reconcileBootstrap` | Job running | 60s | `ErrJobFailed` from bootstrap |
| `reconcileTrustFlush` | — | — | API error → exponential backoff |
| `reconcilePasswordRotation` | Rotation applied (short-circuit) | `Requeue: true` | API error → exponential backoff |

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
| Secret | `{name}-fernet-keys-rotation` | Keystone CR (rotation staging) |
| CronJob | `{name}-fernet-rotate` | Keystone CR |
| PushSecret | `{name}-fernet-keys-backup` | Keystone CR |
| Secret | `{name}-credential-keys` | Keystone CR |
| Secret | `{name}-credential-keys-rotation` | Keystone CR (rotation staging) |
| CronJob | `{name}-credential-rotate` | Keystone CR |
| PushSecret | `{name}-credential-keys-backup` | Keystone CR |
| ConfigMap | `{name}-config-{hash}` | Keystone CR |
| Job | `keystone-db-sync` | Keystone CR | <!-- TODO: align to {name}-* pattern -->
| Job | `keystone-bootstrap` | Keystone CR | <!-- TODO: align to {name}-* pattern -->
| Deployment | `{name}` | Keystone CR (bare CR name) |
| Service | `{name}` | Keystone CR (bare CR name) |
| PodDisruptionBudget | `{name}` | Keystone CR (bare CR name) |
| HorizontalPodAutoscaler | `{name}` | Keystone CR (only when `spec.autoscaling` is set; bare CR name) |
| HTTPRoute | `{name}` | Keystone CR (only when `spec.gateway` is set; bare CR name) |
| Job | `{name}-policy-validation` | Keystone CR (only when `spec.policyOverrides` is set) |
| CronJob | `{name}-trust-flush` | Keystone CR (only when `spec.trustFlush` is set) |
| Secret | `{name}-admin-password-rotation` | Keystone CR (rotation staging; only when `spec.passwordRotation.enabled`) |
| Secret | `{name}-admin-password-next` | Keystone CR (rotation push-source; only when `spec.passwordRotation.enabled`) |
| CronJob | `{name}-admin-password-rotate` | Keystone CR (only when `spec.passwordRotation.enabled`) |
| PushSecret | `{name}-admin-password-backup` | Keystone CR (only when `spec.passwordRotation.enabled`) |
| ServiceAccount | `{name}-admin-password-rotate` | Keystone CR (only when `spec.passwordRotation.enabled`) |
| Role | `{name}-admin-password-rotate` | Keystone CR (only when `spec.passwordRotation.enabled`) |
| RoleBinding | `{name}-admin-password-rotate` | Keystone CR (only when `spec.passwordRotation.enabled`) |
| ConfigMap | `{name}-admin-password-rotate-script-{hash}` | Keystone CR (only when `spec.passwordRotation.enabled`) |
| ConfigMap | `{name}-fernet-rotate-script-{hash}` | Keystone CR |
| ConfigMap | `{name}-credential-rotate-script-{hash}` | Keystone CR |
| Database | `keystone` | Keystone CR (managed mode only; additionally cleaned up by the finalizer) |
| User | `keystone` | Keystone CR (managed mode only; additionally cleaned up by the finalizer) |
| Grant | `keystone` | Keystone CR (managed mode only; additionally cleaned up by the finalizer) |

---

## Database credentials and oslo.config env-var overrides

Keystone's `[database] connection` URL embeds the MySQL username and password.
Earlier, the rendered URL was written directly into `keystone.conf`
inside the operator-managed immutable ConfigMap. ConfigMaps lack encryption at
rest and are routinely granted broad `get` RBAC for observability workflows,
which caused production database credentials to be readable by any actor with
`get configmap` on the Keystone namespace. The current design replaces that
by (1) writing a non-secret placeholder into the ConfigMap and (2) materialising the
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
| `CronJob` `{name}-trust-flush` | `trustFlushCronJob` (`reconcile_trustflush.go`) | Periodic expired trust cleanup — default-on hourly via webhook materialization |
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
[Keystone E2E Test Suites](../testing/keystone-e2e-tests.md).

### Running Tests

| Scope | Command |
| --- | --- |
| All controller tests | `go test ./operators/keystone/internal/controller/...` |
| Specific sub-reconciler | `go test -run TestReconcileSecrets ./operators/keystone/internal/controller/` |

### ObservedGeneration Test Convention

Every sub-reconciler test file that sets conditions includes a dedicated
`TestReconcile{SubReconciler}_ConditionObservedGeneration` test function.
This test exercises at least one `True` and one `False` condition path with distinct,
non-default generation values to verify that `ObservedGeneration` is propagated on
every code path. When adding a new sub-reconciler, copy this test pattern from any
existing file (e.g. `reconcile_hpa_test.go`).

### Test Files

| File | Coverage |
| --- | --- |
| `keystone_controller_test.go` | Reconcile() orchestration, sequential execution, parallel group, early return, Ready aggregation, idempotency, benchmark, finalizer install/remove and termination branching + Events |
| `reconcile_secrets_test.go` | DB/admin credential readiness, error propagation, condition messages, ObservedGeneration |
| `reconcile_database_test.go` | Managed/brownfield modes, MariaDB CRs, db_sync lifecycle, stale Job detection, ObservedGeneration, finalizeDatabaseResources cleanup + idempotency |
| `reconcile_fernet_test.go` | Key generation, Secret idempotency, script ConfigMap creation, CronJob schedule/volumes, PushSecret, key validity, ObservedGeneration |
| `reconcile_credential_test.go` | Key generation, Secret idempotency, script ConfigMap creation, CronJob schedule/volumes, PushSecret, RBAC, key validity, ObservedGeneration |
| `reconcile_networkpolicy_test.go` | NetworkPolicy creation, update, deletion, ingress rules, condition contract |
| `reconcile_config_test.go` | INI generation, extraConfig merge, plugin config, policy overrides, ConfigMap hashing |
| `reconcile_policyvalidation_test.go` | Policy validation lifecycle, condition contract, error extraction, Job spec, ObservedGeneration |
| `reconcile_deployment_test.go` | Deployment spec, Service creation, readiness, endpoint, owner references, stable pod template, ObservedGeneration |
| `reconcile_healthcheck_test.go` | Health check happy/unhealthy paths, timeout, DNS, connection refused, empty endpoint, response body close, HTTPDoer injection, ObservedGeneration |
| `reconcile_hpa_test.go` | HPA creation, update, deletion, metrics (CPU/memory), minReplicas defaulting, condition contract, error propagation, ObservedGeneration |
| `reconcile_httproute_test.go` | HTTPRoute creation, update, deletion, gateway namespace defaulting, PathPrefix match, backend Service port 5000, parent Accepted reflection, status.endpoint derivation, condition contract, ObservedGeneration |
| `reconcile_trustflush_test.go` | CronJob creation, deletion, schedule/suspend/args, security context, volume mounts, condition contract, error propagation, ObservedGeneration |
| `reconcile_passwordrotation_test.go` | CronJob shape/suspend, staging + push-source Secrets, split-Role RBAC shape, apply/validate commit + reject (short/missing/malformed-annotation) paths, clobber-safe PushSecret gating, teardown idempotency (disabled and nil-spec), condition contract |
| `reconcile_bootstrap_test.go` | Job creation, completion, failure, stale detection, backoff, ObservedGeneration |
| `integration_test.go` | Full reconciliation envtest: CronJob spec, bootstrap Job spec, brownfield mode, condition progression, ObservedGeneration |

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
    │   ├── keystone_controller.go              Reconciler struct, Reconcile() pipeline, SetupWithManager
    │   ├── keystone_watches.go                 Secret/MariaDB/ClusterSecretStore/PushSecret event mappers + predicate
    │   ├── reconcile_secrets.go                reconcileSecrets sub-reconciler
    │   ├── reconcile_database.go               reconcileDatabase sub-reconciler
    │   ├── reconcile_fernet.go                 reconcileFernetKeys sub-reconciler
    │   ├── reconcile_credential.go             reconcileCredentialKeys sub-reconciler
    │   ├── reconcile_networkpolicy.go          reconcileNetworkPolicy sub-reconciler
    │   ├── reconcile_config.go                 reconcileConfig + pruneStaleConfigMaps
    │   ├── reconcile_policyvalidation.go        reconcilePolicyValidation sub-reconciler
    │   ├── reconcile_deployment.go             reconcileDeployment sub-reconciler
    │   ├── reconcile_healthcheck.go           reconcileHealthCheck sub-reconciler
    │   ├── reconcile_hpa.go                   reconcileHPA sub-reconciler
    │   ├── reconcile_httproute.go             reconcileHTTPRoute sub-reconciler + keystoneStatusEndpoint helper
    │   ├── reconcile_trustflush.go            reconcileTrustFlush sub-reconciler
    │   ├── reconcile_passwordrotation.go       reconcilePasswordRotation sub-reconciler (Model B)
    │   ├── reconcile_bootstrap.go              reconcileBootstrap sub-reconciler
    │   ├── scripts/
    │   │   ├── fernet_rotate.sh               Fernet key rotation script
    │   │   ├── credential_rotate.sh           Credential key rotation script
    │   │   └── admin_password_rotate.sh       Admin password rotation script (Model B)
    │   ├── keystone_controller_test.go         Orchestration tests
    │   ├── reconcile_secrets_test.go           Secrets tests
    │   ├── reconcile_database_test.go          Database tests
    │   ├── reconcile_fernet_test.go            Fernet tests
    │   ├── reconcile_credential_test.go        Credential keys tests
    │   ├── reconcile_networkpolicy_test.go     NetworkPolicy tests
    │   ├── reconcile_config_test.go            Config tests
    │   ├── reconcile_policyvalidation_test.go   Policy validation tests
    │   ├── reconcile_deployment_test.go        Deployment tests
    │   ├── reconcile_healthcheck_test.go      Health check tests
    │   ├── reconcile_hpa_test.go              HPA tests
    │   ├── reconcile_httproute_test.go        HTTPRoute tests
    │   ├── reconcile_trustflush_test.go       Trust flush tests
    │   ├── reconcile_passwordrotation_test.go  Admin password rotation tests (Model B)
    │   ├── reconcile_bootstrap_test.go         Bootstrap tests
    │   └── integration_test.go                 Envtest integration tests
    └── testutil/
        └── envtest_setup.go                    Keystone-specific envtest helper
```

---

## Metrics Instrumentation

Every sub-reconciler invocation is instrumented for Prometheus via a
single helper, `instrumentSubReconciler`, defined in
`operators/keystone/internal/controller/instrumentation.go`. The orchestration `Reconcile` wraps every
sub-reconciler call with this helper; direct calls that bypass it are a
contract violation.

For the authoritative catalogue of registered metrics — names, labels,
and histogram buckets — see
[Keystone Operator Prometheus Metrics](../keystone-operator-metrics.md).

### The `instrumentSubReconciler` helper

```go
func instrumentSubReconciler(
    ctx context.Context,
    name string,
    fn  func(context.Context) (ctrl.Result, error),
) (ctrl.Result, error)
```

Behavioural contract:

- **Always** records one observation in
  `keystone_operator_reconcile_duration_seconds{sub_reconciler=name}`
  via `defer` — the observation is emitted on the success path, the
  error path, and even when `fn` panics (the deferred call runs before
  the stack unwinds).
- **Only** increments
  `keystone_operator_reconcile_errors_total{sub_reconciler=name, condition_type=…}`
  when `fn` returns a non-nil error.
- Does **not** recover from panics — the caller sees the same
  `panic`/error that `fn` produced.
- Carries no per-CR labels. The `sub_reconciler` label is bounded by
  the number of sub-reconciler names, keeping series count
  fleet-independent.

### Name → `condition_type` lookup

The `condition_type` label on the error counter is resolved from the
package-private table `subReconcilerConditionTypes` in
`instrumentation.go`. Each entry maps a sub-reconciler name to the
Ready sub-condition whose `True` transition that sub-reconciler is
responsible for driving:

| `sub_reconciler` | `condition_type` |
| --- | --- |
| `Secrets`, `DBConnectionSecret`, `Config` | `SecretsReady` |
| `FernetKeys` | `FernetKeysReady` |
| `CredentialKeys` | `CredentialKeysReady` |
| `NetworkPolicy` | `NetworkPolicyReady` |
| `Database` | `DatabaseReady` |
| `PolicyValidation` | `PolicyValidReady` |
| `Deployment` | `DeploymentReady` |
| `HTTPRoute` | `HTTPRouteReady` |
| `HealthCheck` | `KeystoneAPIReady` |
| `HPA` | `HPAReady` |
| `Bootstrap` | `BootstrapReady` |
| `TrustFlush` | `TrustFlushReady` |

The collapsed `SecretsReady` mapping for `Secrets`, `DBConnectionSecret`,
and `Config` is intentional — those three sub-reconcilers form the
earliest readiness gate and share a single condition type. The
cardinality drift-guard
`TestSubReconcilerConditionTypesCoversAllNames` asserts that every
value in this table appears in `subConditionTypes`, so a rename in one
place without the other fails CI.

### Contract: wrap every new sub-reconciler

Whenever a new sub-reconciler is added to `Reconcile` (as a pipeline
`commonreconcile.Step` or a `reconcileParallelGroup` member), it MUST be:

1. Added to the `pipeline` in `Reconcile` as a `commonreconcile.Step` with a
   non-empty `Name`. `RunPipeline` wraps named steps in
   `instrumentSubReconciler(ctx, s.Name, s.Fn)`; only the parallel group
   (whose members self-instrument) and config pruning use an empty name and
   bypass the wrapper.
2. Added to `subConditionTypes` in `keystone_controller.go` (so its
   readiness participates in the aggregate `Ready`).
3. Added to `subReconcilerConditionTypes` in `instrumentation.go` with
   a `condition_type` that is a member of `subConditionTypes`.

Three regression tests guard this contract:

- `TestReconcileEmitsDurationForEverySubReconciler` — every name
  registered in `subReconcilerConditionTypes` must emit at least one
  duration sample per `Reconcile` pass.
- `TestReconcileParallelGroupErrorCountsAreAttributed` — an induced
  failure in a parallel-group member must increment the error counter
  with the correct `sub_reconciler` label (not the group's).
- `TestSubReconcilerConditionTypesCoversAllNames` — every mapped
  `condition_type` must exist in `subConditionTypes`.

For CR lifecycle hygiene, `reconcileDelete` calls
`metrics.DeleteForKeystone(name, namespace)` after the finalizer is
removed so per-CR series (`key_rotation_age_seconds`, `db_sync_*`) do
not leak across the lifetime of a cluster.

