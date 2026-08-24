---
title: KeystoneService CRD API Reference
quadrant: operator
---

# KeystoneService CRD API Reference

Reference documentation for the KeystoneService Custom Resource Definition.
One CR registers one service against the identity plane of a
[ControlPlane](./controlplane-crd.md) via `spec.controlPlaneRef`, and carries
up to two independent blocks:

- **`spec.catalog`** — the service's catalog entry: a service row (type and
  name) plus at most one endpoint row per interface.
- **`spec.account`** — the service's Keystone account: a user with an
  operator-generated, OpenBao-backed, rotatable password, its project, and the
  roles bound to it. The credentials are delivered as a Secret in the CR's own
  namespace.

At least one block must be set. The CR is mode-independent: the same
declaration works against a Managed and an External (brownfield) ControlPlane,
because the operator projects K-ORC resources instead of talking to the
identity API itself.

A KeystoneService may live in a namespace the ControlPlane does not own, which
is what lets a team register a service the control plane does not manage. That
crosses a privilege boundary, so the ControlPlane has to consent: see
[Namespace consent](#namespace-consent). For the control loop, the collision
probes, and the teardown ordering, see
[KeystoneService Reconciler Architecture](./keystoneservice-reconciler.md).

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `c5c3.io` |
| Version | `v1alpha1` |
| Kind | `KeystoneService` |
| List Kind | `KeystoneServiceList` |
| Scope | Namespaced |

**Scheme registration:** the `init()` function in `keystoneservice_types.go`
registers both Kinds with the shared `SchemeBuilder`, so `AddToScheme` covers
`ControlPlane`, `CredentialRotation`, `SecretAggregate`, and `KeystoneService`
alike.

`kubectl get keystoneservice` prints four columns:

| Column | Source |
| --- | --- |
| `Ready` | `.status.conditions[?(@.type=='Ready')].status` |
| `ControlPlane` | `.spec.controlPlaneRef.name` |
| `Type` | `.spec.catalog.serviceType` |
| `Age` | `.metadata.creationTimestamp` |

## Example

A registration in the ControlPlane's own namespace declaring both blocks. The
user name and the catalog service name are left to default to `metadata.name`;
`controlPlaneRef.namespace` is left unset, so the CR resolves the plane in its
own namespace.

```yaml
apiVersion: c5c3.io/v1alpha1
kind: KeystoneService
metadata:
  name: workflow
  namespace: openstack
spec:
  controlPlaneRef:
    name: cp
  catalog:
    serviceType: workflow
    endpoints:
    - interface: public
      url: "https://workflow-api.example.com:8989/v2"
    - interface: internal
      url: "http://workflow-api.openstack.svc.cluster.local:8989/v2"
  account:
    project:
      name: service-workflow
      create: true
    roles:
    - service
```

This registers a `workflow` catalog entry with two endpoint rows, creates the
project `service-workflow` and the user `workflow` in the control plane's admin
domain, binds the `service` role to that user on that project, and materializes
the account's credentials into the Secret `workflow-credentials` in namespace
`openstack`.

## Spec

### KeystoneServiceSpec

A spec-level CEL rule (`has(self.catalog) || has(self.account)`) rejects a CR
that declares neither block: it describes nothing a reconciler could act on.
The rule lives in the schema, so it binds even when the webhook is unavailable.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `controlPlaneRef` | [`ControlPlaneRefSpec`](#controlplanerefspec) | Yes | — | The ControlPlane whose identity plane this service registers against. |
| `catalog` | [`KeystoneServiceCatalogSpec`](#keystoneservicecatalogspec) | No | — | The catalog entry to register. |
| `account` | [`KeystoneServiceAccountSpec`](#keystoneserviceaccountspec) | No | — | The Keystone account to provision, delivered into this CR's namespace. |

### ControlPlaneRefSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` | Yes | — | Name of the ControlPlane CR (`MinLength=1`). **Immutable** (CEL transition rule). Every child this CR projects authenticates through the referenced plane's admin credential, so re-pointing a live registration would strand the Keystone user, project, and catalog row it already created on the old plane, owned by nothing. Delete and re-create the CR instead. |
| `namespace` | `string` | No | the CR's own namespace | Namespace of the ControlPlane CR (RFC-1123 label, ≤ 63). **Immutable**, enforced by the validating webhook rather than CEL: an empty value means the CR's own namespace, and a CEL rule on a spec field cannot read `metadata.namespace` to resolve it. Setting an explicit value naming the namespace the CR already resolved to is admitted, since it changes nothing. |

The referenced ControlPlane does **not** have to exist at admission time.
GitOps may apply the registration before the plane; a dangling reference
surfaces as `Ready=False/ControlPlaneNotFound`, not as an admission error.

### KeystoneServiceCatalogSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `serviceType` | `string` | Yes | — | OpenStack service type, e.g. `image` or `compute`. Lowercase DNS-1123 label (pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, ≤ 63) because it is embedded verbatim in the child K-ORC CR names. `identity` is rejected: that catalog row is ControlPlane-owned in both modes (created in Managed, imported in External). **Immutable** — see [why the identity fields freeze](#why-the-identity-fields-freeze). |
| `serviceName` | `string` | No | `metadata.name` | Overrides the advertised catalog service name (pattern `^[^,]+$`, ≤ 255, mirroring K-ORC's `OpenStackName`). The fallback is the CR's own name, **not** K-ORC's default: K-ORC would advertise the child CR's name, which carries an operator-generated uniqueness suffix. **Immutable**, compared by effective value — see below. |
| `adopt` | `bool` | No | `false` | Consent to take over a pre-existing catalog service row of this type and name. See [Collision and adoption](#collision-and-adoption). Mutable. |
| `endpoints` | [`[]KeystoneServiceEndpointSpec`](#keystoneserviceendpointspec) | No | — | Endpoint rows for this entry, at most one per interface (`listType=map` keyed on `interface`, so the API server rejects a duplicate). An entry with no endpoints registers the service row alone. |

### KeystoneServiceEndpointSpec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `interface` | `public` \| `internal` \| `admin` | Yes | The catalog interface this endpoint is published under. Keys the `listType=map` list. |
| `url` | `string` | Yes | The URL registered in the catalog. Pattern `^https?://[^\s/]+`, ≤ 1024 bytes, mirroring K-ORC's own `EndpointResourceSpec.URL` cap so a URL admitted here can never be rejected downstream. |

Registering an endpoint row never connects to it, so the URL may point at a
service that is not running yet.

Endpoint groups and per-endpoint regions are not expressible: K-ORC has no
EndpointGroup kind and its `EndpointResourceSpec` carries no region field. Both
are upstream gaps, tracked as follow-ups on #846.

### KeystoneServiceAccountSpec

The CR supplies the two handles this block does not carry itself:
`metadata.name` keys the child resources and the consumer Secret, and the CR's
own namespace is the delivery namespace.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `userName` | `string` | No | `metadata.name` | The OpenStack user managed in Keystone (pattern `^[^,]+$`, ≤ 255, mirroring K-ORC's `OpenStackName`). The defaulting webhook materializes the fallback onto the stored object. **Immutable**, compared by effective value. |
| `domainName` | `string` | No | the ControlPlane's admin domain | The domain the user and project live in (pattern `^[^,]+$`, ≤ 255). The fallback resolves to `spec.korc.adminCredential.domainName` on the referenced plane. **Immutable**, compared as declared: resolving the fallback needs the referenced ControlPlane, which admission cannot read, so setting an explicit value is rejected even where it names that same domain. |
| `adopt` | `bool` | No | `false` | Consent to take over a pre-existing Keystone user of this name, including a password takeover. See [Collision and adoption](#collision-and-adoption). Mutable. |
| `project` | [`ServiceAccountProjectSpec`](#serviceaccountprojectspec) | Yes | — | The project the service user is associated with, referenced or created. |
| `roles` | `[]string` | No | — | Role names bound to the user on the project (≤ 32 entries, each `^[^,]+$` and ≤ 255). Each becomes one **unmanaged** K-ORC Role import (Keystone roles are global, so a role is referenced by name and never created or deleted) plus one **managed** RoleAssignment. A role that does not exist in Keystone never resolves, and the account holds at `AccountReady=False/WaitingForServiceAccounts` naming it. |
| `rotation` | [`ServiceAccountRotationSpec`](#serviceaccountrotationspec) | No | `{mode: Manual}` | How the account's password is rotated. |

### ServiceAccountProjectSpec

Shared with the ControlPlane API. Both leaves are frozen after creation by CEL
rules on `spec.account.project`, because the shared type carries no freeze of
its own.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` | Yes | — | The OpenStack project name (pattern `^[^,]+$`, ≤ 255, mirroring K-ORC's `KeystoneName`). **Immutable**: re-pointing it would leave the role assignments the registration minted behind on the old project. |
| `create` | `bool` | No | `false` | `false` **references** a pre-existing project through an unmanaged K-ORC import the operator never creates or deletes. `true` **creates** and owns a managed K-ORC Project, gated by the same fail-loudly collision probe as the user. **Immutable**: a managed/referenced flip would either orphan a project the registration owns or adopt one it does not. |

Project takeover is expressed as `create: false`, not as `adopt`. A project
that already exists and is wanted is referenced.

### ServiceAccountRotationSpec

Shared with the ControlPlane API.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `mode` | `Manual` \| `Scheduled` | No | `Manual` | `Manual` rotates the password only when a `CredentialRotation` CR requests it. `Scheduled` is accepted by the schema but the scheduling logic is deferred; the reconciler emits a `ScheduledRotationDeferred` Normal event when a registration is created with it, so the deferral is never silent. |

To rotate a password on demand, create a `CredentialRotation` with
`spec.target: serviceAccountPassword` and `spec.keystoneService` naming this CR
in the same namespace. See the
[ControlPlane CRD reference](./controlplane-crd.md) for that CR's own spec.

### Why the identity fields freeze

`serviceType`, the effective `serviceName`, the effective `userName`,
`domainName`, and both project leaves name live Keystone state, so an in-place
edit would silently re-shape or strand it. Each is frozen by a CEL transition
rule, the validating webhook, or both, and the remedy is always the same:
delete and re-create the CR.

Two of them are compared by their **effective** value, which is stricter than
the CEL rule alone can be:

- The **catalog service name**. A transition rule does not evaluate when the
  old object left the field unset, so setting an explicit `serviceName` on a
  registration that relied on the `metadata.name` fallback would slip past
  CEL, and that is a catalog rename.
- The **user name**. The effective comparison is what admits the one benign
  transition: materializing the `metadata.name` fallback onto a CR stored
  before the defaulting webhook existed.

The catalog freeze also protects the collision probe. That probe decides
whether a pre-existing row may be taken over, and it runs only while no managed
Service child exists. An in-place type or name edit would therefore re-shape
the registered row without ever re-probing: an exact match would silently adopt
a row the registration does not own, a near match would duplicate one.

`adopt` stays mutable on both blocks, because flipping it to `true` is the
documented collision remedy. So do the endpoint rows, the roles, and the
rotation policy, none of which re-point an existing identity.

## Collision and adoption

Neither block ever silently takes over Keystone state it did not create.
Before creating a managed child, the reconciler probes for a pre-existing
resource with a short-lived unmanaged import, and a match without consent
parks the block on a collision condition having changed nothing.

| Block | Probe matches on | Condition when it collides | Consent |
| --- | --- | --- | --- |
| `catalog` | service type **and** effective name | `CatalogReady=False/ServiceCollision` | `catalog.adopt: true` |
| `account` (user) | user name in the effective domain | `AccountReady=False/ServiceAccountCollision` | `account.adopt: true` |
| `account` (project, `create: true`) | project name in the effective domain | `AccountReady=False/ServiceAccountCollision` | set `create: false` to reference it |

**Adoption means ownership.** An adopted catalog row or user becomes a managed
K-ORC resource, so it is **deleted from Keystone when the KeystoneService is
deleted**, just like one the operator created. Adopt only what the
registration should own.

Adopting a user additionally means a **password takeover**: the operator
overwrites the account's password with its generated one and delivers that
password in the consumer Secret.

::: warning Adoption does not apply the new password on the first pass
With the currently pinned K-ORC, a newly adopted user keeps its pre-existing
password even though the registration reports `AccountReady=True` and the
consumer Secret carries the operator's generated one. K-ORC writes a password
to Keystone only when the User already records an applied password reference,
which an adopted user does not. Rotate the account once with a
`CredentialRotation` to force the first real write. Tracked in
[#920](https://github.com/C5C3/cobaltcore/issues/920).
:::

Blind creation is not offered as an alternative. K-ORC's service actuator
matches an existing row on name and type, so a managed create silently adopts
an exact match anyway, and deleting the CR would then delete a catalog row the
operator never created. A near match duplicates the row instead, because
Keystone enforces no uniqueness on service names.

### The admin identity is never registrable

A registration whose effective user name and domain resolve to the referenced
ControlPlane's own admin identity
(`spec.korc.adminCredential.userName` / `domainName`) is refused with
`AccountReady=False/ServiceAccountCollision`, ahead of the collision probe and
regardless of `adopt`. Provisioning it would take over the admin account,
rotate its password into a Secret in this CR's namespace, and delete the admin
user from Keystone when the CR is deleted.

The names are compared the way Keystone compares them: its identity tables use
a `*_general_ci` collation that folds case **and** accents, so `Admin`,
`ADMIN`, and `admín` all resolve to the same user as `admin`.

The remedy depends on which side moved onto the other. When the registration
owns nothing yet, delete and re-create it under a name of its own.
`userName` and `domainName` are immutable, so editing them is rejected. When
the registration already owns that user (the plane's admin identity was edited
onto it), the ControlPlane is the side that has to move back: deleting the CR
now would take the admin user out of Keystone with it. The condition message
names whichever applies.

## Namespace consent

A KeystoneService can mint a Keystone user with arbitrary roles, so admitting
one from any namespace would turn namespace access into cloud admin. Three
layers grant consent, in widening order:

1. The ControlPlane's **own namespace**.
2. Its **dedicated service namespaces**, declared through the services'
   namespace blocks.
3. Any other namespace listed in
   `spec.korc.serviceRegistrations.allowedNamespaces` on the ControlPlane.

The first two are implicit: both are already the control plane's, and it
provisions a tenant store in each, which is the path a consumer Secret takes.
A CR from an unlisted namespace reports `Ready=False/NamespaceNotAllowed` and
projects nothing.

The allowlist is an **admission gate, not a revocation tool**. Removing a
namespace freezes reconciliation of the registrations in it while every
Keystone user, catalog row, and delivered Secret already minted stays in place
and keeps authenticating. Teardown happens only through deletion of the
KeystoneService itself, so an allowlist edit can never destroy credentials a
running service depends on.

## Consumer Secret contract

A registration with an account block delivers its credentials as a Secret in
the KeystoneService's **own namespace**, whatever namespace the ControlPlane
lives in.

| Property | Value |
| --- | --- |
| Name | `<metadata.name>-credentials`, also reported in `status.account.secretName` |
| Namespace | the KeystoneService's own |
| Data keys | `clouds.yaml` and `password` |
| Ownership | ESO, with `CreationPolicy: Owner` |
| Backing path | `openstack/keystone/<namespace>/<name>/service-accounts/credentials` in OpenBao |

`clouds.yaml` is a ready-to-use document naming the auth URL, the user, the
project, and both domain names; `password` carries the same password on its
own, for consumers that build their own configuration. The name is the one
child name that carries no internal prefix, so it stays predictable from the
CR's name alone.

Because the Secret is owned by its ExternalSecret and not by the
KeystoneService, it is garbage-collected when that ExternalSecret goes away,
which is what the registration's teardown removes. Do not delete the Secret
directly; ESO recreates it.

On rotation the operator re-pushes the assembled document and forces an ESO
re-sync, so the consumer Secret carries the new password without waiting for
the refresh interval. A consumer must re-read the Secret to pick it up; the
account is not reported ready again until the materialized password matches
the generation Keystone applied.

## Status

### KeystoneServiceStatus

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | One condition per block plus the aggregate `Ready`. See [Conditions](#conditions). |
| `observedGeneration` | `int64` | The `metadata.generation` the controller last reconciled, so a stale status is distinguishable from a current one. |
| `catalog` | [`KeystoneServiceCatalogStatus`](#keystoneservicecatalogstatus) | Observed state of the catalog entry. `nil` while no catalog block is declared. |
| `account` | [`KeystoneServiceAccountStatus`](#keystoneserviceaccountstatus) | Observed state of the service account. `nil` while no account block is declared. |

### KeystoneServiceCatalogStatus

| Field | Type | Description |
| --- | --- | --- |
| `serviceID` | `string` | The OpenStack service id K-ORC resolved or created. Empty until the Service child is Available. |
| `endpoints` | [`[]KeystoneServiceEndpointStatus`](#keystoneserviceendpointstatus) | One entry per declared interface (`listType=map` keyed on `interface`). |

### KeystoneServiceEndpointStatus

| Field | Type | Description |
| --- | --- | --- |
| `interface` | `public` \| `internal` \| `admin` | The interface the endpoint is published under. Keys the list. |
| `id` | `string` | The OpenStack endpoint id K-ORC resolved or created. Empty until the Endpoint child is Available. |

### KeystoneServiceAccountStatus

A CR registers one account, so the status needs no name key, and delivery is
always the CR's own namespace, so it reports no Secret namespace.

| Field | Type | Description |
| --- | --- | --- |
| `secretName` | `string` | Name of the materialized [consumer Secret](#consumer-secret-contract). |
| `userID` | `string` | The OpenStack user id. Empty until the User is Available. |
| `projectID` | `string` | The OpenStack project id. Empty until the Project is Available. |
| `passwordGeneration` | `int64` | Generation of the password currently applied to the user. Increments on every rotation. |
| `lastPasswordRotation` | `*metav1.Time` | Timestamp of the last successful rotation. Preserved across steady-state passes. |

### Conditions

Both sub-condition types are **always present**, whether or not their block is
declared: an undeclared block reports True with a `NotDeclared` reason rather
than being omitted, so the aggregate `Ready` is derived from a fixed set and a
missing type is never what a reader has to diagnose.

Several reasons are shared verbatim with the ControlPlane's own vocabulary. The
two objects are different, and a reader comparing a registration to the plane
it registers against should not have to learn a second vocabulary for the same
idea.

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `CatalogReady` | True | `CatalogNotDeclared` | No catalog block is declared. |
| `CatalogReady` | True | `CatalogRegistered` | The service row and every declared endpoint row are registered and Available. |
| `CatalogReady` | False | `ServiceCollision` | A catalog entry of this type and name already exists and `catalog.adopt` is not set. Nothing was touched. |
| `CatalogReady` | False | `ProbingForCollision` | The collision probe has not resolved yet. |
| `CatalogReady` | False | `WaitingForCatalog` | The Service or an Endpoint child is registered but not yet Available. |
| `CatalogReady` | False | `CatalogFailed` | K-ORC reported a terminal error on the Service or an Endpoint; it has stopped retrying. |
| `CatalogReady` | False | `CatalogError` | A Kubernetes-level failure applying a catalog child (not a K-ORC or OpenStack failure). |
| `AccountReady` | True | `AccountNotDeclared` | No account block is declared. |
| `AccountReady` | True | `AccountProvisioned` | The account exists in Keystone with its roles bound, and its credentials are materialized in the consumer Secret. |
| `AccountReady` | False | `ServiceAccountCollision` | The user, or a `create: true` project, already exists without consent — or the registration resolves to the plane's [admin identity](#the-admin-identity-is-never-registrable). |
| `AccountReady` | False | `ProbingForCollision` | The user or project collision probe has not resolved yet. |
| `AccountReady` | False | `SecretStoreNotReady` | The ControlPlane's secret store is not ready in this CR's namespace; the upstream secret backend is unreachable. |
| `AccountReady` | False | `WaitingForServiceAccounts` | A project, user, Role import, or RoleAssignment is registered but not yet Available; K-ORC has not applied the current password yet; or the OpenBao round-trip has not materialized the Secret yet. The message names the blocking dependency. |
| `AccountReady` | False | `ServiceAccountsFailed` | K-ORC reported a terminal error on the account or one of its roles. |
| `AccountReady` | False | `ServiceAccountError` | A Kubernetes-level failure projecting or delivering the account. |
| both | False | `ControlPlaneNotFound` | `spec.controlPlaneRef` does not resolve. Deferred, not failed: GitOps may apply the registration first. |
| both | False | `NamespaceNotAllowed` | The ControlPlane does not admit registrations from this namespace. Nothing is projected. See [Namespace consent](#namespace-consent). |
| both | False | `WaitingForAdminCredential` | The ControlPlane's `AdminCredentialReady` is not True, so K-ORC cannot reach Keystone yet. |
| `Ready` | True | `AllReady` | Both sub-conditions are present and True. |
| `Ready` | False | `NotAllReady` | At least one sub-condition is not True. |

The three shared reasons are written onto every **declared** block. An
undeclared block keeps its `NotDeclared` True: a gate the registration never
reaches for that block is not a failure of it.

Against an External-mode ControlPlane, a bounded wait is replaced by the
specific cause whenever K-ORC's failure can be classified:
`AuthenticationFailed`, `EndpointUnreachable`, `TLSVerificationFailed`, or
`CatalogEndpointMismatch`. A TLS or credential problem therefore does not read
as "registered but not yet Available" forever.

## Defaulting and Validation Summary

**Defaulting webhook.** One field: `account.userName` is materialized from
`metadata.name` when empty. Nothing else is defaulted. `rotation.mode` carries
a schema default, and the account's domain and the catalog's service name
resolve against values admission cannot see, so both stay reconciler-side.

**Schema layer** (CEL and kubebuilder markers, enforced by the API server even
when the webhook is down): the at-least-one-block rule; the `identity`
serviceType rejection; the immutability transition rules on
`controlPlaneRef.name`, `serviceType`, `serviceName`, `userName`, `domainName`,
`project.name`, and `project.create`; the name patterns and length caps
mirroring K-ORC's `OpenStackName` and `KeystoneName` filters; the endpoint URL
scheme pattern and its 1024-byte cap; the `listType=map` key on `endpoints`;
and the 32-entry cap on `roles`.

**Validating webhook** (`KeystoneServiceWebhook`), which holds no client and
reads nothing:

- The **effective-value** immutability comparisons for `serviceName` and
  `userName`, and the `controlPlaneRef.namespace` freeze: the three rules CEL
  cannot express, because two need a fallback resolved and one needs
  `metadata.namespace`.
- A **`metadata.name` length bound**. Nothing caps `metadata.name` below 253
  bytes, but the child CR names are composed from it, so a name admitted
  without this check would wedge the reconcile projecting a child the API
  server rejects, on a CR whose own admission already succeeded. The overhead
  accounted for is the largest child name the CR will project, which is bigger
  when the account declares roles.
- A **defense-in-depth mirror** of the value rules above, for callers that
  bypass CRD schema admission.

Cross-object rules are absent by design. Whether the referenced ControlPlane
exists, whether it admits this namespace, and whether the Keystone resources
collide are all reconciler-side, because each needs a read admission cannot
rely on. The reconciler fails loudly instead, with a condition that names the
remedy.

The rejection corpus lives in `tests/e2e/c5c3/invalid-keystoneservice-cr/`,
generated from its `_generate.py` and guarded by
`make verify-invalid-cr-fixtures`.

## Projected child names

The K-ORC resources a registration projects live in the **ControlPlane's**
namespace, not the CR's, because that is where the admin credential K-ORC
authenticates them with is materialized. They are named from a per-CR prefix:

```
<metadata.name>-<8 hex>-registration-<discriminator>
```

The hash covers `<namespace>/<name>`, so two same-named registrations in
different namespaces cannot collide on a child name. Discriminators are fixed
per kind (`user`, `project`, `service`, `endpoint-<interface>`,
`role-<slug>`, `assign-<slug>`, and the `-probe` twins of the three probed
kinds), which is what makes `kubectl get user -n <controlplane-namespace>` read
as the registration each user belongs to.

The [consumer Secret](#consumer-secret-contract) is the one child that does
**not** carry this prefix: it is a contract consumers read, so it stays
predictable from the CR's name alone.

This composition is why the validating webhook bounds `metadata.name`: the
longest child name it will produce has to stay within the API server's
253-byte object-name limit. The full child inventory is on the
[reconciler page](./keystoneservice-reconciler.md#name-composition).

## Deletion Semantics

Deleting a KeystoneService runs the `c5c3.io/keystoneservice-teardown`
finalizer, which removes every child the registration projected. What that
destroys in Keystone follows managed-versus-referenced ownership:

| Resource | Fate |
| --- | --- |
| Catalog service row and its endpoints | **Deleted**, including an adopted row |
| Keystone user | **Deleted**, including an adopted user |
| Project with `create: true` | **Deleted** |
| Project with `create: false` | Left in place (an unmanaged import is released, never deleted) |
| Roles | Left in place; Keystone roles are global and are only ever imported |
| Role assignments | **Removed** (managed) |
| Consumer Secret | Garbage-collected with its ExternalSecret |
| OpenBao credential path | **Deleted** with the PushSecret that owns it |

While the referenced ControlPlane still exists the teardown is **patient**: the
finalizer is held until no owned child is listed any more, and there is
no stall escape by design. A child K-ORC cannot delete (a 403 on DELETE, an
unreachable Keystone) leaves the CR visibly `Terminating` instead of silently
orphaning a Keystone identity nobody is tracking.

Once the ControlPlane is **gone** the teardown fails open: K-ORC has no
credentials left to reach Keystone with, so waiting could only hold the CR
hostage to a plane that no longer exists. The Kubernetes children are still
deleted, but their outcome no longer gates the finalizer.

Removing a block from the spec is a teardown of that block alone: the children
it owned are swept on the next pass, and the owning block's condition is held
until the removal completes.

## Chainsaw E2E Tests

Three suites cover the CRD. `tests/e2e/c5c3/keystone-service/` drives the
own-namespace round-trip: both blocks registered, the account authenticating
through its materialized `clouds.yaml`, a `CredentialRotation` rotating the
password, a colliding registration held at `ServiceCollision` /
`ServiceAccountCollision` until adopt takes the rows over, and a deletion that
leaves no residue. `tests/e2e/c5c3/keystone-service-foreign-namespace/` covers
the cross-namespace path: an allowlisted namespace registering and reading its
consumer Secret, an unlisted one holding at `NamespaceNotAllowed`, and
de-listing freezing instead of tearing down.
`tests/e2e/c5c3/invalid-keystoneservice-cr/` is the admission rejection corpus.

See [ControlPlane E2E Test Suites](../testing/controlplane-e2e-tests.md).
