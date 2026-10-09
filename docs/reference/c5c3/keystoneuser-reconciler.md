---
title: KeystoneUser Reconciler Architecture
quadrant: operator
---

# KeystoneUser Reconciler Architecture

Reference documentation for the `KeystoneUserReconciler`. It owns the
[KeystoneUser](./keystoneuser-crd.md) lifecycle and is the single writer of its
status: the teardown finalizer, the K-ORC User and its password generations,
the OpenBao backup, the Secret delivered beside the order, and the
`UserReady` / `DeliveryReady` / `Ready` conditions.

It is the one reconciler in the c5c3-operator whose CR may live on a target
cluster. A request carries the cluster the order was seen on, and the
reconciler reads and writes the order, its status, its finalizer and its
delivered Secret through that cluster's client. Everything else it creates
lives in the ControlPlane's namespace on the management cluster, where K-ORC
reads the admin credential.

It adds no identity-API client and no OpenBao client. K-ORC creates the user,
and ESO pushes the password to OpenBao through the secret store of the
ControlPlane's namespace.

It records no Events. The management cluster's recorder would write them into a
namespace of the wrong cluster for an order on a target cluster, so the
conditions carry everything.

For the field-level contracts see
[KeystoneUser CRD API Reference](./keystoneuser-crd.md). The account path it
reuses is the KeystoneService one, described in
[KeystoneService Reconciler Architecture](./keystoneservice-reconciler.md#account-projection).

## Controller Registration

```go
if err := (&controller.KeystoneUserReconciler{
    Client:                  mgr.GetClient(),
    Scheme:                  mgr.GetScheme(),
    Resolver:                mcMgr,
    MaxConcurrentReconciles: opts.MaxConcurrentReconciles,
}).SetupWithManager(mcMgr); err != nil { ... }
```

`Client` is the management cluster's client. `Resolver` is the multicluster
manager, which resolves a request's cluster name to that cluster's client; the
empty name is the management cluster. `SetupWithManager` delegates to
`setupWithOptions`, so the integration suites register the same watches with
`SkipNameValidation`.

Before the watches it registers one field indexer, on the local manager's
indexer only: with a provider configured the multicluster indexer would
register against every target cluster too, and one without the kind would fail
its engagement.

| Index | Key | Extractor |
| --- | --- | --- |
| `KeystoneUserControlPlaneRefIndexKey` (`spec.controlPlaneRef`) | the resolved `<namespace>/<name>` of the reference | `keystoneUserControlPlaneRefExtractor` |

### Watches

| Resource | Watch | Clusters | Effect |
| --- | --- | --- | --- |
| `KeystoneUser` | `For()` | management, and every engaged target cluster that serves the kind | Filtered by `watch.CRUpdatePredicate()`. The request carries the event's cluster |
| `Secret` | `Owns()` | the same | The delivered Secret beside the order. An edit or a deletion brings the order back with the event's cluster |
| K-ORC `User`, `Secret`, `PushSecret` | `Watches()` | management | Mapped back to the order by the three ownership labels (`keystoneUserChildToRequests`). The cluster label becomes the request's cluster |
| `ControlPlane` | `Watches()` | management | Index-backed fan-out (`controlPlaneToKeystoneUsersMapper`) to the orders on the management cluster that reference it. Filtered by `keystoneUserControlPlanePredicate()`: an update passes on a spec change, a deletion, or a flip of `AdminCredentialReady`, the one status condition the gates read |

The order watch on a target cluster carries the `ClusterServesKind` filter. A
cluster that does not serve the kind is engaged for every other watch and
skipped for this one; a CRD installed later is watched once the cluster is
engaged again, which a change of its registration Secret triggers.

No ControlPlane watch reaches an order on a target cluster. Such an order is
reconciled again every 10 minutes (`keystoneUserRefreshAfter`), which is how a
publication or an admin-domain edit reaches it.

### RBAC

On the management cluster the ClusterRole gains the order verbs:

```go
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneusers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneusers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneusers/finalizers,verbs=update
```

It never creates or deletes an order. The kinds it writes in the ControlPlane's
namespace (K-ORC Users, Secrets, PushSecrets) and the ControlPlane reads are
granted by the ControlPlane's marker block.

On a target cluster the `target-cluster-access` chart grants the same order
verbs and Secret write in each assigned namespace, and read on the order kind
in each placed namespace, because the order watch lists every namespace of the
registration. See [Assigned namespaces](../target-clusters.md#assigned-namespaces).

## Reconciliation Flow

```
Reconcile
  ├─ resolve the order's cluster ──→ unresolvable: log, no requeue
  ├─ deleting? ───────────────────→ reconcileDelete  (see Deletion and Teardown)
  └─ reconcileNormal
       ├─ cluster name ≤ 63        → ClusterNameTooLong, no requeue
       ├─ resolveControlPlane      → ControlPlaneNotFound (10s); on a target
       │                             cluster NamespaceNotAssigned (1m)
       ├─ namespace assignment     → NamespaceNotAssigned (1m), frozen
       ├─ EnsureFinalizer          c5c3.io/keystoneuser-teardown
       ├─ AdminCredentialReady     → WaitingForAdminCredential (10s)
       ├─ provisionUser   (instrumented: KeystoneUserProvision)
       └─ deliverCredentials (instrumented: KeystoneUserDelivery)
```

A request whose cluster does not resolve names a cluster deregistered between
the event and the pass. Nothing can be read there, so the pass logs the cluster
and ends without an error.

The assignment gate is the freeze: without an entry for the order's namespace
on its cluster, both conditions read `NamespaceNotAssigned` and the pass
returns before anything in the ControlPlane's namespace is read or written.
Nothing is provisioned, delivered, repaired or swept, and the status the order
already has stays. The finalizer is installed only past this gate: an order the
operator does not serve has nothing to tear down, and in a namespace that is not
assigned on a target cluster the operator is not granted the update.

The cluster's name is a label value on every child, so an order on a cluster
registered under a name longer than 63 characters is refused first. A missing
ControlPlane reads as `NamespaceNotAssigned` for an order on a target cluster,
with the message a ControlPlane without the assignment writes, so the reason
does not tell an owner there which ControlPlanes exist.

When the provision step has not converged, `DeliveryReady` reads
`WaitingForServiceAccounts` and the delivery is skipped. A pass that asked for
no requeue ends in one of two ways: a delivered order on the management cluster
waits for an event, and every other order comes back after 10 minutes, which
covers the target clusters and the refusals no watch lifts.

## User provisioning

`provisionUser` writes `UserReady` in five steps:

1. A user name that folds, as Keystone compares names, to one the ControlPlane
   creates in its admin domain itself is refused with `ServiceAccountCollision`
   and never provisioned: its admin user, and every built-in service account
   (`keystoneUserReservedNames`), whether or not that service is enabled.
2. The secret store of the ControlPlane's namespace has to be ready, or the
   password could not be backed up (`SecretStoreNotReady`).
3. A short-lived unmanaged User import (`<prefix>user-probe`) decides whether a
   user of that name exists in the admin domain. An existing one is refused
   with `ServiceAccountCollision` and never taken over; an order has no adopt.
   The probe is dropped once the managed User exists.
4. The managed User `<prefix>user` is created in the admin domain with no
   default project, pointing at the password Secret of the declared
   generation, `<prefix>password-v<N>`.
5. Once K-ORC reports the User Available with that Secret applied,
   `status.userID` and `status.passwordGeneration` are set and `UserReady`
   reads `UserProvisioned`.

`spec.passwordGeneration` drives the generation. A value above the current one
writes the new password Secret and points the User at it; K-ORC applies a
password only when that reference changes. The superseded Secret is deleted
once K-ORC reports the new one applied, and a value below the current one never
rolls the password back. A stored zero reads as 1.

A latched transport error on the User is cleared first, so K-ORC retries; any
other terminal error reads `ServiceAccountsFailed`. Against an External-mode
ControlPlane a wait is replaced by the classified K-ORC failure.

## Credential delivery

`deliverCredentials` writes `DeliveryReady`:

1. The auth URL is resolved for the order's cluster. Off Keystone's cluster
   that is the published endpoint; without one the step reports
   `KeystoneNotPublished` and writes nothing.
2. The password of the current generation is read from `<prefix>password-v<N>`.
3. The source Secret `<prefix>source` gets the `clouds.yaml` without project
   keys, the password and the individual auth fields.
4. The PushSecret `<prefix>backup` pushes it to
   `openstack/keystone/<controlplane namespace>/<prefix without the trailing dash>/service-accounts/credentials`
   through the namespace's own store, with `deletionPolicy: Delete`. A changed
   document sets a hash annotation on it, so ESO pushes at once, and records
   the PushSecret's `status.syncedResourceVersion` beside it. ESO moves that
   field only after a successful push, so the step waits for `Ready` and for a
   value other than the recorded one (`BackupNotSynced`). `Ready` alone would
   still read `True` from the push of the previous password.
5. The Secret `<name>-credentials` is applied beside the order with a
   controller owner reference to it. A live Secret of that name the order does
   not own is refused (`DeliveryRefused`). On a target cluster that check reads
   the cluster's API server, not its cache.
6. The apply response is checked. Once its `password` matches,
   `status.secretName` and `status.secretKeys` are set and `DeliveryReady`
   reads `Delivered`.

The apply in step 5 is the repair. Server-Side Apply with forced ownership
gives the operator's field manager the two keys it applies, so an edited key is
rewritten on the event the edit raises, a deleted Secret is applied again, and
a key another field manager adds stays.

## Child Naming and Placement

| Object | Name | Namespace | Cluster | Ownership |
| --- | --- | --- | --- | --- |
| Managed K-ORC User | `<prefix>user` | ControlPlane's | management | labels |
| Collision probe | `<prefix>user-probe` | ControlPlane's | management | labels |
| Password Secret, per generation | `<prefix>password-v<N>` | ControlPlane's | management | labels |
| Source Secret | `<prefix>source` | ControlPlane's | management | labels |
| PushSecret | `<prefix>backup` | ControlPlane's | management | labels |
| Delivered Secret | `<name>-credentials` | the order's | the order's | controller owner reference |

`<prefix>` is `<name>-<first 8 hex of sha256(<cluster>/<namespace>/<name>)>-user-`.
The labels are `c5c3.io/keystoneuser-name`, `c5c3.io/keystoneuser-namespace`
and `c5c3.io/keystoneuser-cluster`, the last empty for the management cluster.

An object in the ControlPlane's namespace is the order's child only when it
carries all three labels and the prefix. Neither test alone is enough: the
namespace holds the plane's own children and those of every KeystoneService and
every other order. The operator refuses to overwrite a live object of a
child's name that is not the order's child.

## Deletion and Teardown

`reconcileDelete` runs the `c5c3.io/keystoneuser-teardown` finalizer without
consulting the assignment, so a frozen order tears down too. It issues the
deletes in this order, in the resolved `controlPlaneRef` namespace:

1. the PushSecret, whose `deletionPolicy` has ESO remove the OpenBao path,
2. the managed User, whose K-ORC finalizer deletes the Keystone user,
3. the probe,
4. every password Secret,
5. the source Secret,

and then the delivered Secret through the order's cluster client.

With the ControlPlane present the finalizer is held while any of them is still
listed, and the pass requeues after 10 seconds. With the ControlPlane gone the
deletes are issued and the finalizer is released at once, because K-ORC has no
credential left to reach Keystone with.

An order whose target cluster was deregistered cannot be read, so it is never
torn down, and its children stay labelled in the ControlPlane's namespace. See
[Deletion Semantics](./keystoneuser-crd.md#deletion-semantics) for the command
that removes them.

## Metrics Instrumentation

Both steps run through the package-scope `instrumenter` the ControlPlane
reconciler shares, so they observe the
`c5c3_operator_reconcile_duration_seconds{sub_reconciler=…}` histogram and the
`c5c3_operator_reconcile_errors_total{sub_reconciler=…,condition_type=…}`
counter.

| `sub_reconciler` | `condition_type` |
| --- | --- |
| `KeystoneUserProvision` | `UserReady` |
| `KeystoneUserDelivery` | `DeliveryReady` |

The names carry the kind as a prefix, as the KeystoneService entries do. The
drift guard `TestSubReconcilerConditionTypesCoversAllNames` accepts a value
from `keystoneUserSubConditionTypes`, and
`TestInstrumenterInstrument_KeystoneUserLabelPairs` proves neither name falls
back to `condition_type=UNKNOWN`.

## Testing

| Layer | Location |
| --- | --- |
| Gates, freeze, mappers, teardown | `operators/c5c3/internal/controller/keystoneuser_controller_test.go` |
| User provisioning | `operators/c5c3/internal/controller/keystoneuser_provision_test.go` |
| Credential delivery | `operators/c5c3/internal/controller/keystoneuser_delivery_test.go` |
| The unscoped `clouds.yaml` | `operators/c5c3/internal/controller/korc_cloudsyaml_test.go` |
| CRD schema | `TestIntegration_KeystoneUser_SchemaValidation` in `integration_test.go` |
| An order on a target cluster | `TestIntegration_Multicluster_ControlPlanePlacement` in `multicluster_integration_test.go` |
| End to end | `tests/e2e/c5c3/keystone-user/` |
| Admission rejection corpus | `tests/e2e/c5c3/invalid-keystoneuser-cr/` |

The unit tests drive a fake client per cluster. The multicluster envtest runs
two API servers, creates the order on the target and checks where each object
appears; it carries K-ORC and ESO as schema without a controller, so the
end-to-end suite is where the order meets Keystone, OpenBao and ESO. See
[ControlPlane E2E Test Suites](../testing/controlplane-e2e-tests.md).
