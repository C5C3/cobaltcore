---
title: Keystone Orders Reconciler Architecture
quadrant: operator
---

# Keystone Orders Reconciler Architecture

Reference documentation for the four reconcilers of the Keystone pieces an
owner orders beside a [KeystoneUser](./keystoneuser-crd.md):
`KeystoneProjectReconciler`, `KeystoneRoleAssignmentReconciler`,
`KeystoneCatalogEntryReconciler` and `KeystoneApplicationCredentialReconciler`.
Each owns the lifecycle of one kind and is the single writer of its status: the
teardown finalizer, the K-ORC children in the ControlPlane's namespace, and its
sub-conditions with the aggregate `Ready`.

The four share one scaffold with the `KeystoneUserReconciler`, in
`operators/c5c3/internal/controller/keystoneorder.go`. Like the user, an order
of these kinds may live on a target cluster: a request carries the cluster the
order was seen on, the order, its status and its finalizer are read and written
there, and everything else lives on the management cluster, where K-ORC reads
the admin credential. None of them records Events, for the reason the
[KeystoneUser reconciler](./keystoneuser-reconciler.md) gives.

For the field-level contracts see
[KeystoneProject](./keystoneproject-crd.md),
[KeystoneRoleAssignment](./keystoneroleassignment-crd.md),
[KeystoneCatalogEntry](./keystonecatalogentry-crd.md) and
[KeystoneApplicationCredential](./keystoneapplicationcredential-crd.md).

## Controller Registration

```go
if err := (&controller.KeystoneProjectReconciler{
    Client:                  mgr.GetClient(),
    Scheme:                  mgr.GetScheme(),
    Resolver:                mcMgr,
    MaxConcurrentReconciles: opts.MaxConcurrentReconciles,
}).SetupWithManager(mcMgr); err != nil { ... }
// KeystoneRoleAssignmentReconciler, KeystoneCatalogEntryReconciler and
// KeystoneApplicationCredentialReconciler follow with the same fields.
```

`main.go` registers them after the KeystoneUser reconciler, all five on the
multicluster manager. The `KeystoneApplicationCredentialReconciler` also has a
`Now` field, the clock its schedule reads; production leaves it nil, which
means `time.Now`. `SetupWithManager` delegates to `setupWithOptions`, so
the integration suites register the same watches with `SkipNameValidation`.
Each registers one field indexer on the local manager's indexer,
`spec.controlPlaneRef`, keyed on the resolved `<namespace>/<name>` of the
reference, for the ControlPlane watch.

## The shared scaffold

`keystoneorder.go` holds what does not depend on the kind. An `orderRef`
carries an order's name, namespace and cluster, the kind's prefix segment and
its three ownership label keys.

| Function | Purpose |
| --- | --- |
| `orderRef.childPrefix` | `<name>-<first 8 hex of sha256(<cluster>/<namespace>/<name>)>-<segment>-`, the prefix of every child |
| `orderRef.childLabels`, `claimOrderChild` | the three ownership labels, set on every child; never an owner reference |
| `isOrderChild`, `ownsOrderChild` | a child carries all three labels (or a controller reference to the order) and the prefix |
| `ensureOrderChild`, `orderEnsure` | Server-Side Apply of a child, refusing a live object of that name the order did not create |
| `orderAdmission` | the cluster-name check, the ControlPlane read and the consent lookup, returning the plane and the order's `spec.namespaceAssignments` entry |
| `orderAdminCredentialGate` | `WaitingForAdminCredential` while `AdminCredentialReady` is not True |
| `orderPassResult` | a requeue a step asked for stands; a converged order on the management cluster waits for an event; every other order comes back after 10 minutes |
| `ensureOrderSecret` | read-modify-write of a labelled Secret, so a generated value is kept across passes |
| `stampOrderPush`, `orderPushFresh` | the document hash on a PushSecret, and whether ESO has pushed that document since |
| `orderTeardown`, `sweepOrderList`, `sweepOrderLists` | the patient, fail-open teardown and the label-selected sweep of each child kind, with a selector of the items a sweep keeps |
| `sweepDeliveredSecret` | the delete of the Secret delivered beside the order, when the order controls it |
| `orderControlPlanePredicate` | the ControlPlane updates an order reads: a spec change, a deletion, a flip of `AdminCredentialReady` |
| `orderChildRequests` | maps a child back to its order by the labels, the cluster label becoming the request's cluster |

`orderAdmission` writes `ClusterNameTooLong`, `ControlPlaneNotFound` (or, on a
target cluster, `NamespaceNotAssigned` with the message a plane without the
entry writes) and `NamespaceNotAssigned` through the kind's own failure setter.
The consent lookup is the freeze: a refused pass returns before anything in the
ControlPlane's namespace is read or written. Every kind installs its finalizer
only past it.

## Watches

| Kind | Resource | Clusters | Effect |
| --- | --- | --- | --- |
| all four | the order, `For()` | management, and every engaged target cluster that serves the kind (`ClusterServesKind`) | Filtered by `watch.CRUpdatePredicate()` |
| all four | `ControlPlane` | management | Index-backed fan-out to the orders on the management cluster that reference it, filtered by `orderControlPlanePredicate()` |
| KeystoneProject | K-ORC `Project` | management | Mapped back by the `c5c3.io/keystoneproject-*` labels |
| KeystoneProject | `KeystoneRoleAssignment`, `KeystoneApplicationCredential` | management and target clusters serving them | The project an order names, on the order's cluster, so an order leaving wakes a held teardown |
| KeystoneRoleAssignment | K-ORC `RoleAssignment`, `Role` | management | Mapped back by the `c5c3.io/keystoneroleassignment-*` labels |
| KeystoneRoleAssignment | `KeystoneUser`, `KeystoneProject` | management and target clusters serving them | Every assignment in the referenced order's namespace that names it, listed from that cluster's cache. An update passes on a spec change, a deletion, or a change of `Ready` or the condition the gate reads (`UserReady`, `ProjectReady`) |
| KeystoneRoleAssignment | `KeystoneApplicationCredential` | management and target clusters serving it | Every assignment of the credential order's user and project, so a credential order leaving wakes a held teardown |
| KeystoneCatalogEntry | K-ORC `Service`, `Endpoint`, `Region` | management | Mapped back by the `c5c3.io/keystonecatalogentry-*` labels |
| KeystoneApplicationCredential | K-ORC `ApplicationCredential`, `Secret`, `PushSecret` | management | Mapped back by the `c5c3.io/keystoneapplicationcredential-*` labels. K-ORC releasing a superseded credential brings the order back to end its grace period |
| KeystoneApplicationCredential | the delivered `Secret`, `Owns()` | the order's clusters | An edited or deleted Secret is repaired |
| KeystoneApplicationCredential | `KeystoneUser`, `KeystoneProject` | management and target clusters serving them | Every credential order naming it. An update passes on what the assignment's reference watch passes, and on a move of the user's `status.passwordGeneration`, which the `mint-cloud` document follows |
| KeystoneApplicationCredential | `KeystoneRoleAssignment` | management and target clusters serving it | Every credential order of the assignment's user and project. An update passes on a spec change, a deletion, or a change of `status.roleID` |

The KeystoneUser reconciler watches `KeystoneRoleAssignment` and
`KeystoneApplicationCredential` the way the project reconciler does, for its own
holds. No ControlPlane watch reaches an order on a target
cluster; it is reconciled again every 10 minutes (`orderRefreshAfter`,
which every order kind refreshes on).

## RBAC

```go
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneprojects,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneprojects/finalizers,verbs=update
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneroleassignments,verbs=get;list;watch;update;patch
// ... the same for keystoneroleassignments, keystonecatalogentries and
// keystoneapplicationcredentials
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneapplicationcredentials,verbs=get;list;watch
```

No reconciler creates or deletes an order. The KeystoneUser and KeystoneProject
reconcilers read `keystoneroleassignments` for their holds, and the assignment
reads both of them, which their own markers grant. The user, the project and
the assignment read `keystoneapplicationcredentials` for their holds, the last
marker above. The K-ORC kinds they write
and the ControlPlane reads are granted by the ControlPlane's marker block.

On a target cluster the `target-cluster-access` chart grants the order verbs
on the five order kinds in each assigned namespace, and read on them in each
placed namespace. See [Assigned namespaces](../target-clusters.md#assigned-namespaces).

## Project provisioning

`provisionProject` writes `ProjectReady`:

1. A project name that folds, as Keystone compares names, to the admin project
   or a built-in service project (`keystoneProjectReservedNames`) is refused
   with `ProjectCollision` and never provisioned.
2. A short-lived unmanaged Project import (`<prefix>project-probe`) in the
   admin domain decides whether the project exists. An existing one is refused
   with `ProjectCollision`; an order has no adopt.
3. The managed Project `<prefix>project` is applied in the admin domain,
   authenticating through the operator's admin password cloud.
4. A latched transport error is cleared first; a terminal one reads
   `ProjectFailed`, a pending Project `WaitingForProject`.
5. On Available, `status.projectID`, `status.projectName` and
   `status.domainName` are set and `ProjectReady` reads `ProjectProvisioned`.

## Role assignment

`reconcileNormal` of the KeystoneRoleAssignment runs, past the shared gates:

1. The role allowlist: a role outside the entry's `allowedRoles` reads
   `RoleNotAllowed` with the list named. It freezes the order.
2. The references, read in the order's namespace on its cluster: a missing
   KeystoneUser or KeystoneProject reads `UserNotFound` or `ProjectNotFound`, a
   reference to another ControlPlane `ControlPlaneMismatch`, a referenced order
   not provisioned yet `WaitingForUser` or `WaitingForProject`.
3. The duplicate check: an older order in the namespace with the same user,
   project and role makes this one `DuplicateRoleAssignment`. It costs one
   namespaced List on the order's cluster cache per pass.
4. `assignRole` applies the unmanaged Role import `<prefix>role` and the
   managed RoleAssignment `<prefix>assignment` through `applyAccountRole`, the
   helper a KeystoneService account uses. The RoleAssignment binds the user's
   `<user prefix>user` to the project's `<project prefix>project`.
5. On Available, `status.roleID`, `status.userID` and `status.projectID` come
   from the RoleAssignment's `status.resource` and `AssignmentReady` reads
   `RoleAssigned`.

Each refusal of steps 1 to 3 returns the pass result of an unconverged order:
the reference watches and the ControlPlane watch bring the order back on a
change, and the 10-minute refresh covers the rest.

## Catalog registration

`reconcileNormal` of the KeystoneCatalogEntry checks `allowCatalogEntries`
right after `orderAdmission` and before the finalizer: without it the order
reads `CatalogNotAllowed`, re-reads the ControlPlane every minute, and creates
nothing.

`registerCatalogEntry` is the KeystoneService catalog projection over the
order, without adopt: the probe `<prefix>service-probe` filtered on type and
name, then the managed Service `<prefix>service`, the Region import
`<prefix>region` of the ControlPlane's region, and one managed Endpoint
`<prefix>endpoint-<interface>` per declared interface. Terminal errors are
reported Service first, then Region, then Endpoints (`CatalogFailed`), and the
waits in the same order (`WaitingForCatalog`). Every owned Endpoint the spec
no longer declares is deleted on every pass, before those outcomes are read, so
a dropped row never waits on a declared one. Once every declared row is
Available and a dropped one is still listed, the pass reports
`WaitingForCatalog`. `status.endpoints` is rebuilt on every pass.

## Application credential provisioning

`reconcileNormal` of the KeystoneApplicationCredential runs, past the shared
gates, three reference gates on every pass. The user and the project go through
`roleAssignmentReference.gate`, as for an assignment (`UserNotFound`,
`ProjectNotFound`, `ControlPlaneMismatch`, `WaitingForUser`,
`WaitingForProject`). `roleAssignedOnProject` then lists the
KeystoneRoleAssignments of the namespace and requires one of the same user and
project that reports `status.roleID`; without it the order reads
`NoRoleOnProject`. A refusal returns the pass result of an unconverged order,
writes `DeliveryReady=False/WaitingForCredential` only before the first
credential, and pauses the schedule.

`provisionCredential` writes `CredentialReady`:

1. `ensureMintCloud` writes `<prefix>mint-cloud`, the project-scoped password
   `clouds.yaml` of the user (`buildServiceAccountCloudsYAML`), read from the
   Secret of the user's `status.passwordGeneration`, with the CA bundle beside
   it. It waits on `WaitingForUser`, `WaitingForProject` or
   `WaitingForCABundle`.
2. `credentialChildren` lists the `credential-v<N>` and `secret-v<N>` children
   by the order's labels and keys them by generation.
3. `desiredGeneration` decides the generation the pass works toward: the
   declared one before the first credential, the highest one in flight (one
   K-ORC failed terminally only until a higher generation is declared), or the
   next one when the declared generation is higher, the schedule is due, the
   credential expired, or its secret is lost. No rotation starts while
   `status.previousCredentialDeleteAt` is set.
4. `pruneStrayGenerations` deletes every generation other than the live one,
   the superseded one during its grace period, and the one the pass works
   toward.
5. When that generation is above the live one, `mintGeneration` writes
   `secret-v<N>` once (`generatedAppCredValueMutator`) and creates
   `credential-v<N>`, never applying it again because K-ORC keeps its resource
   block immutable. The credential names the user's managed `User`, carries
   `unrestricted: false`, and, with the schedule on, an expiry of the pass's
   clock plus the interval plus the grace period. A latched transport error is
   cleared first; a terminal one reads `CredentialFailed`. While the successor
   is not Available, `CredentialReady` stays `True` once a credential is
   delivered and reads `WaitingForCredential` before. Once it is Available,
   status switches to it and starts the grace period of the superseded one.
6. `sweepGrace` deletes the superseded `credential-v<N>` when its grace period
   has ended, and on the pass that no longer lists it deletes its secret and
   clears the three `previous*` fields.
7. `nextRotation` is recomputed: the earlier of `lastRotation` plus the
   interval and `credentialExpiresAt` minus the grace period, absent with the
   schedule off.

The step returns the time to `nextRotation`, to `previousCredentialDeleteAt`
and to `credentialExpiresAt` through `commonreconcile.ShortestRequeue`.
`keystoneApplicationCredentialPassResult` keeps that timer for a converged
order on the management cluster and caps it at 10 minutes everywhere else. A
requeue held in the work queue is lost on a restart, and the start-up pass
re-reads `nextRotation`, so a rotation is late by at most the restart.

## Application credential delivery

`deliverCredential` writes `DeliveryReady` for the generation
`status.credentialGeneration` names. It resolves the auth URL for the order's
cluster (`KeystoneNotPublished`), reads the generation's secret
(`WaitingForCredential` before the first credential), and renders the document
with `buildAppCredCloudsYAML`. The source Secret `<prefix>source`
and the PushSecret `<prefix>backup` push it to
`openstack/keystone/<controlplane namespace>/<prefix>/service-accounts/application-credential`.
`stampOrderPush` sets the document hash under
`c5c3.io/keystoneapplicationcredential-push-hash`, and nothing is delivered
before `orderPushFresh` reports a push of it (`BackupNotSynced`). The Secret
`<name>-credentials` is then applied beside the order with a controller owner
reference, after a live read that refuses a Secret the order does not control
(`DeliveryRefused`). A switch in the provision step therefore reaches the
Secret in the same pass once the backup holds the new document.

## Deletion and Teardown

| Kind | Before the sweep | Sweep order in the ControlPlane's namespace |
| --- | --- | --- |
| KeystoneUser | hold while an assignment or a credential order names it as `userRef` | PushSecret, User, probe, password Secrets, source Secret, then the delivered Secret |
| KeystoneProject | hold while an assignment or a credential order names it as `projectRef` | managed Project, probe |
| KeystoneRoleAssignment | hold while a credential order has the same user and project | RoleAssignment, Role import |
| KeystoneCatalogEntry | none | Endpoints, managed Service, probe, Region import |
| KeystoneApplicationCredential | none | while an ApplicationCredential is listed: PushSecret, every `credential-v<N>`, every `secret-v<N>`, source Secret; once none is: `mint-cloud`, then the delivered Secret |

A hold lists the KeystoneRoleAssignments in the order's namespace through the
order's cluster client (`referencingRoleAssignments`; a cluster that does not
serve the kind has none). While one matches, the sub-condition reads
`ReferencedByRoleAssignments` naming them, nothing is deleted, and the pass
requeues after a minute. K-ORC guards a User and a Project with a finalizer
while a RoleAssignment references them, so a sweep would wedge, and deleting
either would take the assignment with it.

The second hold lists the KeystoneApplicationCredentials the same way
(`referencingApplicationCredentials`) and reads
`ReferencedByApplicationCredentials`, on the user, the project, and every
assignment of the credential order's user and project. Deleting the user
deletes its credentials in Keystone before K-ORC could, the credentials are
scoped to the project, and K-ORC creates and deletes them with a token scoped
to the project, which needs the role. The user checks the assignments first.

The credential order's two phases keep `<prefix>mint-cloud` while K-ORC still
holds a credential, because K-ORC authenticates the Keystone delete with it and
guards it with a finalizer. With the ControlPlane gone, both phases run in one
pass.

`orderTeardown` then runs the sweep. With the ControlPlane present the
finalizer is held while any child is listed and the pass requeues after 10
seconds; with it gone the deletes are issued and the finalizer is released at
once. The assignment is not consulted, so a frozen order tears down too.

## Metrics Instrumentation

Each provision step runs through the package-scope `instrumenter`:

| `sub_reconciler` | `condition_type` |
| --- | --- |
| `KeystoneProjectProvision` | `ProjectReady` |
| `KeystoneRoleAssignmentProvision` | `AssignmentReady` |
| `KeystoneCatalogEntryProvision` | `CatalogReady` |
| `KeystoneApplicationCredentialProvision` | `CredentialReady` |
| `KeystoneApplicationCredentialDelivery` | `DeliveryReady` |

The drift guard `TestSubReconcilerConditionTypesCoversAllNames` accepts a value
from each kind's sub-condition list.

## Testing

| Layer | Location |
| --- | --- |
| The shared scaffold | `operators/c5c3/internal/controller/keystoneorder_test.go` |
| Project provisioning, hold, teardown | `operators/c5c3/internal/controller/keystoneproject_controller_test.go` |
| Role assignment, references, duplicates, watches | `operators/c5c3/internal/controller/keystoneroleassignment_controller_test.go` |
| Catalog registration, consent, sweep | `operators/c5c3/internal/controller/keystonecatalogentry_controller_test.go` |
| Credential gates, watches, teardown | `operators/c5c3/internal/controller/keystoneapplicationcredential_controller_test.go` |
| Mint document, generations, schedule, grace sweep | `operators/c5c3/internal/controller/keystoneapplicationcredential_provision_test.go` |
| Credential backup and delivery | `operators/c5c3/internal/controller/keystoneapplicationcredential_delivery_test.go` |
| The user's holds | `TestKeystoneUser_DeleteHoldsWhileRoleAssignmentsReferenceIt`, `TestKeystoneUser_DeleteHoldsWhileApplicationCredentialsReferenceIt` in `keystoneuser_controller_test.go` |
| CRD schemas | `TestIntegration_KeystoneProject_SchemaValidation`, `TestIntegration_KeystoneRoleAssignment_SchemaValidation`, `TestIntegration_KeystoneCatalogEntry_SchemaValidation`, `TestIntegration_KeystoneApplicationCredential_SchemaValidation` in `integration_test.go` |
| Orders on a target cluster | `TestIntegration_Multicluster_ControlPlanePlacement` in `multicluster_integration_test.go` |
| End to end | `tests/e2e/c5c3/keystone-user/` |
| Admission rejection corpora | `tests/e2e/c5c3/invalid-keystoneproject-cr/`, `invalid-keystoneroleassignment-cr/`, `invalid-keystonecatalogentry-cr/`, `invalid-keystoneapplicationcredential-cr/` |

The unit tests drive a fake client per cluster, and the credential tests a
clock of their own through `Now`. The multicluster envtest orders the four
pieces beside a user on the target cluster and checks that their children
appear on the management cluster with labels only, that the catalog entry waits
for its consent, that the credential order writes its `mint-cloud` document and
generation 1 and waits for K-ORC, and that the user's deletion holds until the
assignment is gone. See
[ControlPlane E2E Test Suites](../testing/controlplane-e2e-tests.md).
