---
title: KeystoneService Reconciler Architecture
quadrant: operator
---

# KeystoneService Reconciler Architecture

Reference documentation for the `KeystoneServiceReconciler`, the second
reconciler the c5c3 operator registers. It owns the
[KeystoneService](./keystoneservice-crd.md) CR lifecycle and is the **single
writer** of its status: the teardown finalizer, the per-block projection of
K-ORC children, the OpenBao round-trip behind the account's consumer Secret,
and the `CatalogReady` / `AccountReady` / `Ready` conditions.

For CRD type definitions, the field-level contracts, and the webhooks, see
[KeystoneService CRD API Reference](./keystoneservice-crd.md). For the
ControlPlane's own control loop, its admin-credential chain, and the shared
controller-manager bootstrap both reconcilers reuse, see
[ControlPlane Reconciler Architecture](./controlplane-reconciler.md).

Two properties shape everything below.

**K-ORC is the single writer of OpenStack state.** The reconciler adds no
identity-API client of its own; it projects K-ORC CRs (Service, Endpoint, User,
Project, Domain, Role, RoleAssignment) and reads their conditions back. That is
why a registration works identically against a Managed and an External
Keystone, and why every wait condition ultimately reports what K-ORC reported.

**Delivery is management-cluster only.** The reconciler carries no cluster
Resolver, unlike the ControlPlane's sub-reconcilers. A registration delivers
its credentials into its own namespace, beside the workload that consumes them;
only the ControlPlane-projected built-in registrations keep the placed-service
delivery legs.

The ControlPlane projects a `KeystoneService` child per built-in service
(Glance, Placement, Barbican), so those registrations run through this same
reconciler. Their projection is the ControlPlane's, documented with it.

## Controller Registration

```go
if err := (&controller.KeystoneServiceReconciler{
    Client:                  mgr.GetClient(),
    Scheme:                  mgr.GetScheme(),
    Recorder:                mgr.GetEventRecorderFor("keystoneservice-controller"),
    MaxConcurrentReconciles: maxConcurrentReconciles,
}).SetupWithManager(mgr); err != nil { ... }
```

`SetupWithManager` delegates to `setupWithOptions`, which takes the controller
options as a parameter so the integration suite registers this exact watch
chain with `SkipNameValidation`, not a hand-built copy that drifts.

Before any watch leg is wired, it registers the field indexer the ControlPlane
watch depends on:

| Index | Key | Extractor |
| --- | --- | --- |
| `KeystoneServiceControlPlaneRefIndexKey` (`spec.controlPlaneRef`) | the **resolved** `<namespace>/<name>` of the reference | `keystoneServiceControlPlaneRefExtractor` |

The key is namespace-qualified for two reasons: a ControlPlane event must not
wake registrations pointing at a same-named plane in another namespace, and the
qualification is what lets the mapper's List stay cluster-wide now that the
allowlist admits foreign namespaces. A CR with an empty reference name (one
that bypassed admission) indexes nothing, not a dangling key.

### Watches

| Resource | Watch Type | Effect |
| --- | --- | --- |
| `KeystoneService` | `For()` | Filtered by `watch.CRUpdatePredicate()` so the controller's own status writes do not re-wake it |
| K-ORC `Service`, `Endpoint`, `User`, `Domain`, `Project`, `Role`, `RoleAssignment` | `Watches()` | Mapped back to the owning CR by the ownership labels every child carries (`keystoneServiceChildToRequest`). These children live in the ControlPlane's namespace, where an owner reference to a CR in another namespace is illegal, so `Owns()` would never see them |
| `Secret` | `Watches()` | Same label mapper: it reaches the generation-scoped password Secret beside the User and the assembled source Secret beside the consumer |
| `ExternalSecret`, `PushSecret` | `Owns()` | The delivery objects stay in the CR's own namespace, where a controller reference is legal and the garbage collector reaps them |
| `ControlPlane` | `Watches()` | Index-backed fan-out (`controlPlaneToKeystoneServicesMapper`) with **no** generation predicate: the `AdminCredentialReady` flip the shared gate waits on and the allowlist edits that admit or de-list a namespace both arrive as ControlPlane updates, and both must re-enqueue at watch latency rather than at the next poll |
| `Secret` (consumer) | `Watches()` | `credentialsSecretToKeystoneServiceMapper` maps a Secret back through the `<cr-name>-credentials` name contract. The consumer Secret is ESO-owned (`CreationPolicy: Owner`), so no owner reference points at the CR and the `Owns()` legs never fire for it |

The label mapper reaches both placements at once: a co-located child is stamped
with the same labels on top of its owner reference, so one leg per kind covers
every registration. An object carrying neither label belongs to something else
(the ControlPlane's own children share these namespaces) and maps to nothing.

Watch delivery is the fast path, not the only one. Waiting states requeue on
`korcRequeueAfter` (10s), so a K-ORC child that converges without an event
still resolves.

### RBAC

On its own kind the controller is narrow:

```go
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneservices,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=c5c3.io,resources=keystoneservices/finalizers,verbs=update
```

It reads registrations and updates them only to install and release the
teardown finalizer; it never creates or deletes a KeystoneService. Every other
kind it touches (the K-ORC kinds, the ESO kinds, Secrets, events, ControlPlane
reads) is already granted by the ControlPlane and CredentialRotation marker
blocks, and the Helm chart mirrors the deduplicated union.

## Reconciliation Flow

```
Reconcile
  ├─ deleting? ─────────────────→ reconcileDelete  (see Deletion and Teardown)
  ├─ EnsureFinalizer            c5c3.io/keystoneservice-teardown, before any projection
  └─ reconcileNormal
       ├─ setUndeclaredBlockConditions   stamp NotDeclared on absent blocks
       ├─ resolveControlPlane            → ControlPlaneNotFound
       ├─ namespace consent              → NamespaceNotAllowed
       ├─ AdminCredentialReady gate      → WaitingForAdminCredential
       ├─ ensureCatalog    (instrumented: KeystoneServiceCatalog)
       ├─ ensureAccount    (instrumented: KeystoneServiceAccount)
       └─ sweepChildren                  prune what the spec stopped declaring
```

The finalizer is installed **before** any projection, so a deletion issued
between that write and the next pass funnels through `reconcileDelete` and
finds every child.

The undeclared blocks are stamped **first**, so both sub-condition types exist
however the gates below resolve. Without it, a gate failure on a single-block
CR would leave the other type absent and the aggregate `Ready` permanently
False with nothing on the object explaining which sub-condition was missing.

### The shared gates

Each gate writes its condition onto every **declared** block and returns. An
undeclared block keeps its `NotDeclared` True: a gate the registration never
reaches for that block is not a failure of it.

| Gate | Reason | Behaviour |
| --- | --- | --- |
| `spec.controlPlaneRef` resolves | `ControlPlaneNotFound` | A status condition, not an admission error, because GitOps may apply the registration before the plane. Any read failure other than NotFound propagates instead, so the workqueue backs off instead of reporting a dangling reference it did not observe |
| Namespace consent | `NamespaceNotAllowed` | `keystoneServiceNamespaceAllowed` admits the plane's own namespace, its dedicated service namespaces, then `spec.korc.serviceRegistrations.allowedNamespaces`. A nil block and an empty list are identical: both admit nothing beyond the implicit two. Nothing is projected |
| `AdminCredentialReady` on the plane | `WaitingForAdminCredential` | K-ORC cannot talk to Keystone before the admin credential is minted |

De-listing a namespace **freezes** its registrations here instead of tearing
them down. The gate stops the projection; it never deletes what earlier passes
created. Teardown happens only through deletion of the CR itself, so an
allowlist edit can never destroy credentials a running service depends on.

### Credential references

Past the gates, the reconciler resolves two clouds.yaml references, and which
child gets which matters:

- **`credRef`** — the ControlPlane's admin credential, used by the read-only
  collision probes and the unmanaged imports.
- **`managedCredRef`** — the operator-owned admin **password** cloud, used by
  every managed child. The ApplicationCredential's K-ORC finalizer revokes the
  credential at teardown, so a managed child still authenticating through it
  would get a 404 on its own DELETE and orphan the Keystone resource.

### Block independence

The two blocks are independent: a catalog failure never defers the account and
vice versa. Each writes its own condition, and the pass requeues if either
asked for it. Only a Kubernetes-level error short-circuits the pass, and the
condition explaining it is already written by the time it propagates.

Each block runs through the shared instrumenter, so its duration and errors
land on the `c5c3_operator` metric vectors under its own `sub_reconciler`
label. See [Metrics Instrumentation](#metrics-instrumentation).

### Pruning

`sweepChildren` removes every child this CR owns that the current spec no
longer declares (a removed role, a removed endpoint interface, a whole removed
block) and reports what it swept, split by the block that owns the kind. The
split is by kind, which is exact here: Service and Endpoint are only ever
catalog children, and User / Project / Domain / Role / RoleAssignment and the
delivery objects only ever account children.

A swept name is reported on the pass that issued its Delete, not once the
object is gone: a K-ORC child stays Terminating behind its finalizer while the
Keystone row is removed. The owning block's condition is therefore held
(`WaitingForCatalog` or `WaitingForServiceAccounts`) until a later pass no
longer lists it.

Two safeguards keep the sweep from reaching too far. Both the ownership test
and the name test must pass, so a foreign object sharing the namespace can
never be swept, which matters because the K-ORC children sit beside the
plane's own children and those of every other registration. And the
generation-scoped password Secrets are guarded by prefix, not by the
declared-name set: their lifecycle belongs to the user projection, and a sweep
must not race it into deleting the generation the managed User currently
references.

## Catalog Projection

`ensureCatalog` projects one managed K-ORC Service for the catalog row plus one
managed Endpoint per declared interface.

It is mode-independent, and unlike the ControlPlane's own catalog
reconciler. That one imports instead of creating in External mode, because the
identity row it owns already exists in a brownfield catalog. A KeystoneService
registers a service the plane does **not** carry, so it creates its row against
a Managed and an External Keystone alike.

1. **Collision gate.** A short-lived unmanaged Service import filtered by type
   and effective name probes whether the row already exists. The verdict is
   read by the shared `interpretProbe`. Resolved without `catalog.adopt` gives
   `ServiceCollision`; still resolving gives `ProbingForCollision`; absent, or
   adopt consent, or a managed Service the operator already owns, proceeds.
   The probe covers the **service row only**: an endpoint is scoped to its
   service, so once the row is ours the endpoints under it are ours too.
2. **Projection.** The Service and the Endpoints are pure projections of the
   spec, applied through Server-Side Apply.
3. **Terminal errors.** The Service's terminal error is reported before the
   Endpoints', so the root stuck dependency surfaces instead of an Endpoint
   merely blocked behind it. Either gives `CatalogFailed`: K-ORC has stopped
   retrying, so a bounded wait would never resolve.
4. **Availability.** Registering the CRs only instructs K-ORC to create the
   rows. The block reports ready only once every child is Available for its
   current generation, or a failing registration would read Ready while the
   catalog stayed empty.

## Account Projection

`ensureAccount` projects the Keystone identity and delivers its credentials. A
CR carries one account, so the precedence between outcomes is a chain of early
returns, not a pass over a list.

**(0) Admin-identity refusal.** A registration whose effective user name and
domain resolve to the plane's own admin identity is refused with
`ServiceAccountCollision`, changing nothing. It sits ahead of everything else,
including the collision probe, because `account.adopt: true` short-circuits
that probe and adopt is the documented remedy for every *other* collision, so a
probe-side rule would be opt-out by design. Admission cannot carry it either:
it resolves against the referenced ControlPlane, which the CRD contract allows
to be absent at admission time.

The comparison goes through `keystoneNameKey`, which folds case **and** accents
because Keystone's identity tables use a `*_general_ci` collation. A narrower
comparison would wave a variant spelling past the refusal while Keystone still
resolved it onto the admin user. The remedy in the message is chosen by reading
the **live** managed User, not `status.account.userID`: the child is
created a full pass before status records its ID, and a lookup failure counts
as owned, so the destructive advice is never what an error falls back to.

**(1) Store gate.** The delivery leg is gated on the ControlPlane's secret
store being ready **in the CR's own namespace**, so an ESO or OpenBao outage
surfaces as `SecretStoreNotReady` promptly instead of at the next hourly
refresh. The gate is scoped to the account block: a catalog-only registration
touches no secret machinery and must not wait for a store it never reads.

**(2) Domain handle.** When the effective domain is the plane's admin domain,
the reconciler reuses the admin Domain import, which the ControlPlane created
and owns, so this CR's prune never sweeps it. Any other domain gets its own
unmanaged import.

**(3) Project.** `create: false` references a pre-existing project through an
unmanaged import the operator never creates or deletes. `create: true` creates
a managed Project behind the same fail-loudly probe as the user, because
K-ORC's managed create would otherwise silently adopt a same-named project. The
collision message points at `create: false`, not at adopt: project takeover is
expressed as referencing.

**(4) User.** A probe decides exists/absent before any managed User is created;
`account.adopt` is the consent that skips it. The managed User then carries a
**generation-scoped** password Secret (`…-password-v<N>`). K-ORC's user actuator
re-applies a password only when the reference *name* changes, so a rotation is
a Secret-name flip and never an in-place edit. The password value is generated
once per generation and preserved across passes.

A registration created with `rotation.mode: Scheduled` emits a
`ScheduledRotationDeferred` Normal event: the mode is accepted by the schema
but its scheduling logic is deferred, and the deferral is not silent.

**(5) Roles.** Per declared role, one unmanaged Role import filtered by name
(Keystone roles are global, so the import carries no domain and reading one
never mutates it) and one managed RoleAssignment binding that role to this
account's user on its project. The imports are per CR, never shared:
registrations are independently owned and deleted, so a shared import would
have no single owner and the first CR deleted would pull it out from under the
others. A role that does not exist in Keystone never resolves, and the wait
message says so.

Role readiness is folded into the block's readiness, so an account is never
reported provisioned before the roles it needs are bound.

**(6) Publish.** Once K-ORC confirms the current password is applied to the
user, the reconciler assembles the source Secret, mirrors it to the per-CR
OpenBao path through a PushSecret, and materializes the consumer Secret through
an ExternalSecret. A content-hash annotation
(`c5c3.io/keystoneservice-push-hash`) forces an immediate re-push and re-sync on
rotation instead of waiting for the refresh interval. The block reports ready
only once the **materialized** password matches the current generation, so a
rotated-away password never reads ready.

The bounded waits in steps 3 through 6 all report
`WaitingForServiceAccounts`, with a message naming the stuck dependency in
dependency order (project, then user, then the password application), so the
condition points at the real blocker instead of at whatever is merely waiting
behind it.

### External-mode failure classification

Against an External ControlPlane, a K-ORC import that cannot reach the external
Keystone reports a non-terminal `TransientError`. Without help, an
authentication failure, a TLS error, and a catalog mismatch would all read as
"registered but not yet Available" forever, with nothing naming the cause.

`keystoneServiceWaitOrClassify` therefore **prefers a classifiable failure over
the generic wait reason** on every bounded wait in both blocks, surfacing
`AuthenticationFailed`, `EndpointUnreachable`, `TLSVerificationFailed`, or
`CatalogEndpointMismatch` with the external auth URL and K-ORC's own message.

## Child Naming and Placement

The K-ORC children and the generation-scoped password Secrets live in the
**ControlPlane's namespace**, never the registration's. K-ORC reads the
clouds.yaml named by a child's `cloudCredentialsRef` from that child's own
namespace, and the admin credential is materialized once, in the plane's
namespace, and stays there: copying it into every allowlisted
namespace would hand each tenant the cloud-admin credential and undo the
escalation the allowlist exists to prevent. So the children go to the
credential, not the credential to the children.

Only the **delivery objects** stay in the registration's own namespace: the
assembled source Secret, the PushSecret, the ExternalSecret, and the consumer
Secret the service reads.

### Ownership

| Placement | Mechanism |
| --- | --- |
| Same namespace as the CR | Controller owner reference, so the garbage collector reaps the child |
| Another namespace | The labels `c5c3.io/keystoneservice-name` and `c5c3.io/keystoneservice-namespace`, since Kubernetes rejects an owner reference across namespaces |

Both halves of the label pair are required: a name is only unique within its
namespace. Every child is stamped the moment it is created, so it is
recognizable on the pass that writes it, not on a later one.

Before adopting a name that already exists outside the CR's namespace, the
apply refuses anything this registration did not create. Without that check the
apply would overwrite a stranger's spec and the teardown would then delete it.

### Name composition

Every child name is composed from a per-CR prefix:

```
<metadata.name>-<8 hex>-registration-<discriminator>
```

The hash is taken over `<namespace>/<name>`, not over the name alone: the
children live in the ControlPlane's namespace, so two same-named CRs in
different namespaces would otherwise project onto the same child names, one
silently reshaping and then deleting the other's Keystone identity. The
readable base stays in front so `kubectl get user` still reads as the
registration it belongs to.

| Discriminator | Child |
| --- | --- |
| `service`, `service-probe` | Catalog Service and its collision probe |
| `endpoint-<interface>` | One Endpoint per declared interface |
| `user`, `user-probe` | Managed User and its collision probe |
| `project`, `project-probe` | Project and its collision probe |
| `domain` | Domain import, when the effective domain is not the plane's admin domain |
| `role-<slug>`, `assign-<slug>` | Role import and RoleAssignment, per role |
| `password-v<N>` | Generation-scoped password Secret |
| `source`, `backup` | Assembled source Secret and its PushSecret |

The **consumer Secret is the one child that does not carry the prefix**.
It is named `<metadata.name>-credentials`, because it is the documented contract
a service reads its credentials from and has to stay predictable from the CR's
name alone. The sweep's name test admits it alongside the prefix, and the
consumer-Secret watch maps it back through that same suffix.

The prefix is also why the CRD's validating webhook bounds `metadata.name`:
the composed child name has to stay within the API server's 253-byte object-name
limit.

## Deletion and Teardown

`reconcileDelete` runs the `c5c3.io/keystoneservice-teardown` finalizer. It
reuses the same `sweepChildren` the per-pass prune uses, passing an empty keep
set so everything goes, and issues the deletes dependents-first: assignments
before roles, endpoints before the service they reference, both before the user
and project they bind. K-ORC enforces its own ordering through finalizers, but
issuing them in dependency order keeps the intent legible and avoids a
guaranteed retry.

**With the ControlPlane present the teardown is patient.** K-ORC's finalizers
take the catalog rows and the Keystone user out of the identity plane, so the
finalizer is held until no owned child is listed any more. There is
**no stall escape**: a child K-ORC cannot delete (a 403 on DELETE, an
unreachable Keystone) leaves the CR visibly `Terminating` instead of silently
orphaning a Keystone identity nobody is tracking any more.

**With the ControlPlane gone the teardown fails open.** K-ORC has no credentials
left to reach Keystone with, so waiting could only hold the CR hostage to a
plane that no longer exists. The Kubernetes children are still deleted, since
they would otherwise leak, but their outcome no longer gates the finalizer. For
a registration the ControlPlane projected, that path only ever meets
finalizer-free children: the ControlPlane teardown strips its children's K-ORC
and ESO finalizers before it releases itself.

The children are looked for in the **resolved** `controlPlaneRef` namespace,
which is where they are whether or not the plane itself is still readable.

For which Keystone resources a deletion destroys, see
[Deletion Semantics](./keystoneservice-crd.md#deletion-semantics) on the CRD
page.

## Metrics Instrumentation

Both block projections run through the package-scope `instrumenter` the
ControlPlane reconciler shares, so they observe the same
`c5c3_operator_reconcile_duration_seconds{sub_reconciler=…}` histogram and
`c5c3_operator_reconcile_errors_total{sub_reconciler=…,condition_type=…}`
counter. The behavioural contract of that helper is documented in the
[ControlPlane Reconciler reference](./controlplane-reconciler.md).

This controller contributes two entries to `subReconcilerConditionTypes`:

| `sub_reconciler` | `condition_type` |
| --- | --- |
| `KeystoneServiceCatalog` | `CatalogReady` |
| `KeystoneServiceAccount` | `AccountReady` |

The names carry the CR kind as a prefix because `Catalog` and `ServiceAccounts`
already label the ControlPlane's own legs (the identity row and the
registration aggregation), and the two must stay distinguishable per metric
series. The condition type strings are shared with the ControlPlane's
`CatalogReady`, which is harmless: they live on different objects.

The drift guard `TestSubReconcilerConditionTypesCoversAllNames` asserts that
every mapped `condition_type` is a member of the ControlPlane's
`subConditionTypes` **or** of `keystoneServiceSubConditionTypes`. A
`sub_reconciler` name reaching the instrumenter without a key here resolves to
the sentinel `condition_type=UNKNOWN`, so the drift is visible in dashboards
instead of silent.

The aggregate `Ready` carries no `sub_reconciler` of its own: it is the
roll-up, re-derived from both sub-conditions on every status persist by
`SetAggregateReady`, which requires every listed type to be present and True.

## Testing

| Layer | Location |
| --- | --- |
| Controller unit tests | `operators/c5c3/internal/controller/keystoneservice_controller_test.go` |
| Catalog projection | `operators/c5c3/internal/controller/keystoneservice_catalog_test.go` |
| Account projection | `operators/c5c3/internal/controller/keystoneservice_account_test.go` |
| CRD schema and webhook | `operators/c5c3/api/v1alpha1/keystoneservice_types_test.go`, `keystoneservice_webhook_test.go` |
| Own-namespace e2e | `tests/e2e/c5c3/keystone-service/` |
| Cross-namespace e2e | `tests/e2e/c5c3/keystone-service-foreign-namespace/` |
| Admission rejection corpus | `tests/e2e/c5c3/invalid-keystoneservice-cr/` |

Unit tests drive a fake client and envtest carries K-ORC as schema without a
controller, so neither sees Keystone, OpenBao, ESO, or cert-manager. The
collision postures of both blocks are asserted end to end only in the
own-namespace suite. See
[ControlPlane E2E Test Suites](../testing/controlplane-e2e-tests.md).
