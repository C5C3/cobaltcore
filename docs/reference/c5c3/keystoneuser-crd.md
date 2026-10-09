---
title: KeystoneUser CRD API Reference
quadrant: operator
---

# KeystoneUser CRD API Reference

Reference documentation for the KeystoneUser Custom Resource Definition. A
KeystoneUser orders one Keystone user from a namespace that a
[ControlPlane](./controlplane-crd.md) assigns to a service owner through
[`spec.namespaceAssignments`](./controlplane-crd.md#namespaceassignmentspec).
The c5c3-operator creates the user through K-ORC, backs its password up to
OpenBao, and writes the credentials into the Secret
`<metadata.name>-credentials` beside the order.

The order lives in the assigned namespace, on the cluster that namespace is
assigned on: the management cluster or a registered
[target cluster](../target-clusters.md). Nothing in that namespace can reach
OpenBao. The owner receives a Secret and nothing else.

For the control loop, the watches and the teardown order, see
[KeystoneUser Reconciler Architecture](./keystoneuser-reconciler.md). The
[Order a Service User](../../guides/order-a-service-user.md) guide walks the
flow on the ControlPlane devstack.

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `c5c3.io` |
| Version | `v1alpha1` |
| Kind | `KeystoneUser` |
| List Kind | `KeystoneUserList` |
| Scope | Namespaced |

**Scheme registration:** the `init()` function in `keystoneuser_types.go`
registers both Kinds with the shared `SchemeBuilder`.

`kubectl get keystoneuser` prints four columns:

| Column | Source |
| --- | --- |
| `Ready` | `.status.conditions[?(@.type=='Ready')].status` |
| `ControlPlane` | `.spec.controlPlaneRef.name` |
| `Generation` | `.status.passwordGeneration` |
| `Age` | `.metadata.creationTimestamp` |

## Example

An order in the assigned namespace `tenant-a` against the ControlPlane `cp` in
`openstack`. The user name defaults to `metadata.name` and the password
generation to 1.

```yaml
apiVersion: c5c3.io/v1alpha1
kind: KeystoneUser
metadata:
  name: workflow
  namespace: tenant-a
spec:
  controlPlaneRef:
    name: cp
    namespace: openstack
```

The ControlPlane assigns the namespace with an entry of its own:

```yaml
spec:
  namespaceAssignments:
  - namespace: tenant-a
```

## Spec

### KeystoneUserSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `controlPlaneRef` | [`ControlPlaneRefSpec`](#controlplanerefspec) | Yes | — | The ControlPlane whose identity plane the user is created in. |
| `userName` | `string` | No | `metadata.name` | The Keystone user name (`MinLength=1`, `MaxLength=255`, no comma, mirroring K-ORC's `OpenStackName`). **Immutable**: a rule on the whole object compares the effective name, so spelling out the `metadata.name` default is admitted and any other value is rejected. The names the ControlPlane creates in its admin domain itself are [reserved](#conditions). |
| `passwordGeneration` | `int64` | No | `1` | The generation of the user's password (`Minimum=1`). Raising it rotates the password. It may only increase. |

The reconciler resolves the `userName` default itself. The kind has no
defaulting webhook, so the stored object keeps the field empty.

### ControlPlaneRefSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` | Yes | — | Name of the ControlPlane CR (`MinLength=1`). **Immutable** (CEL transition rule). |
| `namespace` | `string` | No | the order's own namespace | Namespace of the ControlPlane CR (RFC-1123 label, ≤ 63). **Immutable** (CEL transition rule), also when the order was created without it. |

Set the namespace explicitly. An assigned namespace is never the ControlPlane's
own, and an order on a target cluster names a namespace on the management
cluster that its own cluster does not have. Re-pointing a live order would
strand the Keystone user it created on the old plane, so both halves are
frozen. Delete and re-create the order to move it.

The referenced ControlPlane does not have to exist at admission time. A
dangling reference reports `ControlPlaneNotFound` on both conditions for an
order on the management cluster. An order on a target cluster reports
`NamespaceNotAssigned` instead, with the message a ControlPlane without the
assignment writes, so an owner there cannot learn from the reason which
ControlPlanes exist on the management cluster.

### Password rotation

Raising `spec.passwordGeneration` from N to N+1 rotates the password. The
operator generates a new password in a Secret of its own, points the K-ORC User
at it, and K-ORC applies it in Keystone. Until K-ORC reports the new generation
applied, `status.passwordGeneration` stays at N and the delivered Secret keeps
the old password, which still authenticates. Once it is applied,
`status.passwordGeneration` reads N+1, `status.lastPasswordRotation` is set, and
the Secret holding generation N is deleted. The delivered Secret is rewritten
once ESO has pushed the new password to OpenBao. A consumer reads the Secret
again to pick the new password up.

## Consent

The `spec.namespaceAssignments` entry for the order's namespace and cluster is
the only consent a KeystoneUser reads:

| Order on | Entry that admits it |
| --- | --- |
| the management cluster | `namespace: <ns>` without `targetClusterRef` |
| target cluster `edge-1` | `namespace: <ns>` with `targetClusterRef: {name: edge-1}` |

`spec.korc.serviceRegistrations.allowedNamespaces`, the ControlPlane's own
namespace and its dedicated service namespaces admit KeystoneService
registrations and admit no order. An entry on one cluster does not cover the
same namespace name on another. The order requests no role, so the entry's
`allowedRoles` is not consulted.

Without an entry both conditions read `False/NamespaceNotAssigned`, and the
message names `spec.namespaceAssignments` and the cluster. The order is frozen:
nothing is provisioned, delivered, repaired or swept, and the Keystone user,
the password Secrets, the backup and the delivered Secret it already has stay.
Withdrawing an entry is therefore not a revocation; deleting the order is. A
frozen order re-reads the ControlPlane every minute, so restoring the entry
brings it back without an edit to the order.

### Orders on a target cluster

An order for a namespace assigned on a target cluster is created on that
cluster. The operator watches the kind there, writes the order's status and
finalizer there, and writes the delivered Secret beside it. The K-ORC User,
the password Secrets and the backup stay on the management cluster.

The target cluster serves the kind through the `target-cluster-access` chart,
which ships the CRD and grants the operator's account the order and Secret
verbs in each `assignedNamespaces` entry. See
[Assigned namespaces](../target-clusters.md#assigned-namespaces).

The cluster's name is a label value on every child of the order, which
Kubernetes caps at 63 characters. An order on a cluster registered under a
longer name reports `ClusterNameTooLong` on both conditions, and nothing is
created for it.

## Delivered Secret contract

| Property | Value |
| --- | --- |
| Name | `<metadata.name>-credentials`, also reported in `status.secretName` |
| Namespace | the order's own |
| Cluster | the order's own |
| Data keys | `clouds.yaml` and `password`, also reported in `status.secretKeys` |
| Ownership | a controller owner reference to the order; the garbage collector reaps the Secret with it |
| Labels | `c5c3.io/keystoneuser-name` and `c5c3.io/keystoneuser-namespace` |
| Backing path | `openstack/keystone/<controlplane namespace>/<name>-<hash>-user/service-accounts/credentials` in OpenBao |

`clouds.yaml` names the auth URL, the user name, the password and the user's
domain, and no project: the user has no project and no role, so authenticating
with it yields an unscoped token. `password` carries the password on its own,
for consumers that build their own configuration. The document carries no CA
bundle; a consumer on another cluster than Keystone verifies the public
endpoint against its own trust store.

The auth URL is resolved for the cluster the order lives on:

| Order on | Keystone | `auth_url` |
| --- | --- | --- |
| the cluster Keystone runs on | Managed | the in-cluster Service URL, `http://<cp>-keystone.<keystone namespace>.svc:5000/v3` |
| any other cluster | Managed | `spec.services.keystone.publicEndpoint`, or `https://<gateway hostname>/v3` |
| any cluster | External | `spec.services.keystone.external.authURL` |

An order on another cluster than a Managed Keystone that publishes neither a
`publicEndpoint` nor a `gateway` reports `DeliveryReady=False/KeystoneNotPublished`
and receives nothing until the ControlPlane publishes one.

The backup comes first. The operator writes a source Secret and a PushSecret
in the ControlPlane's namespace, and the PushSecret pushes the document to the
path above through that namespace's own secret store. The delivered Secret is
written only once ESO reports a push of the current document. The PushSecret's
`Ready` condition alone is not taken for that: after a rotation it still reads
`True` from the push of the old password.

The operator applies the Secret with Server-Side Apply and owns its two keys.
An edited `password` or `clouds.yaml` is rewritten, and a deleted Secret is
written again, both on the event the edit raises. A key somebody else adds
stays. A [frozen](#consent) order repairs nothing.

A Secret of the same name that the order does not own is never taken over: the
order reports `DeliveryRefused` and leaves it unchanged. A KeystoneService of
the same name in the same namespace delivers a Secret of the same name too, so
the two cannot share a name. The Secret is a native Secret in the owner's
namespace, so its protection at rest is the cluster's encryption configuration.

## Status

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | `UserReady`, `DeliveryReady` and the aggregate `Ready`. See [Conditions](#conditions). |
| `observedGeneration` | `int64` | The `metadata.generation` the controller last reconciled. |
| `userID` | `string` | The Keystone user id. Empty until the User exists in Keystone. |
| `passwordGeneration` | `int64` | The password generation Keystone holds. |
| `lastPasswordRotation` | `*metav1.Time` | When the password was last rotated. |
| `secretName` | `string` | The [delivered Secret](#delivered-secret-contract), once it carries the current password. |
| `secretKeys` | `[]string` | `clouds.yaml` and `password`, once the Secret carries the current password. |

### Conditions

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `UserReady` | True | `UserProvisioned` | The Keystone user exists and K-ORC applied the current password generation. |
| `UserReady` | False | `ServiceAccountCollision` | A Keystone user of this name exists that the order did not create, or the name is one the ControlPlane creates in its admin domain itself: its admin user, or a built-in service account (`glance`, `placement`, `barbican`, `neutron`, `cinder`, `nova`, `neutron-nova`, `hypervisor-operator`), whether or not that service is enabled. Names are compared as Keystone compares them, ignoring case and accents. The order never takes a user over. |
| `UserReady` | False | `ProbingForCollision` | The probe that decides whether the user already exists has not resolved yet. |
| `UserReady` | False | `SecretStoreNotReady` | The secret store in the ControlPlane's namespace is not ready, so the password cannot be backed up. |
| `UserReady` | False | `WaitingForServiceAccounts` | The User is not Available yet, or K-ORC has not applied the current password generation. |
| `UserReady` | False | `ServiceAccountsFailed` | K-ORC reported a terminal error on the User. A latched transport error is cleared first, so K-ORC retries. |
| `UserReady` | False | `TransportErrorRetryFailed` | Clearing a latched transport error from the User failed. |
| `UserReady` | False | `ServiceAccountError` | A Kubernetes-level failure projecting the user. |
| `DeliveryReady` | True | `Delivered` | The Secret beside the order carries the current password. |
| `DeliveryReady` | False | `WaitingForServiceAccounts` | The user is not provisioned yet, the password of the current generation is not available, or the delivered Secret does not carry it yet. |
| `DeliveryReady` | False | `KeystoneNotPublished` | The order lives on a cluster that cannot reach the in-cluster Keystone Service and the ControlPlane publishes no public endpoint. |
| `DeliveryReady` | False | `BackupNotSynced` | ESO has not pushed the current password to OpenBao yet. |
| `DeliveryReady` | False | `DeliveryRefused` | A Secret of the delivered name exists that the order does not own. |
| `DeliveryReady` | False | `DeliveryError` | A Kubernetes-level failure writing or reading a delivery object, with the error text. |
| both | False | `ControlPlaneNotFound` | `spec.controlPlaneRef` does not resolve, for an order on the management cluster. |
| both | False | `NamespaceNotAssigned` | No `spec.namespaceAssignments` entry assigns the order's namespace on its cluster, or, for an order on a target cluster, `spec.controlPlaneRef` does not resolve. The order is frozen. |
| both | False | `ClusterNameTooLong` | The order lives on a target cluster whose name is longer than the 63 characters a label value carries. |
| both | False | `WaitingForAdminCredential` | The ControlPlane's `AdminCredentialReady` is not True, so K-ORC cannot reach Keystone yet. |
| `Ready` | True | `AllReady` | Both sub-conditions are True. |
| `Ready` | False | `NotAllReady` | At least one sub-condition is not True. |

Against an External-mode ControlPlane a wait on the User is replaced by the
classified cause when K-ORC's failure can be classified:
`AuthenticationFailed`, `CredentialDrift`, `EndpointUnreachable`,
`TLSVerificationFailed` or `CatalogEndpointMismatch`.

An order on the management cluster is reconciled again when its ControlPlane
changes. An order on a target cluster, and a refusal that only an edit
elsewhere lifts (`KeystoneNotPublished`, `DeliveryRefused`, the admin
identity), is reconciled again every 10 minutes.

## Defaulting and Validation Summary

The kind has **no webhook**, defaulting or validating, because a target
cluster runs none. Every rule is a CRD rule the API server enforces on any
cluster that serves the kind:

- `userName`: `MinLength=1`, `MaxLength=255`, pattern `^[^,]+$`.
- `passwordGeneration`: schema default `1`, `Minimum=1`, and the transition
  rule `self >= oldSelf` ("passwordGeneration may only increase").
- `controlPlaneRef.name`: required, `MinLength=1`, and a transition rule
  ("controlPlaneRef.name is immutable").
- `controlPlaneRef.namespace`: RFC-1123 label, ≤ 63, and a transition rule
  ("controlPlaneRef.namespace is immutable"). The rule spells the field
  `__namespace__`, the escape Kubernetes CEL requires for a property named
  after a CEL reserved word.
- On the object: `metadata.name` is at most 63 bytes ("metadata.name must be at
  most 63 bytes"), because it is a label value on the order's children, and
  the effective user name is frozen ("userName is immutable").

The 63-byte cap also bounds the child names: the longest is the password
Secret, 35 bytes past the name at a ten-digit generation.

Cross-object rules live in the reconciler: whether the ControlPlane exists,
whether it assigns the namespace, and whether the user collides each need a
read admission does not have.

The rejection corpus lives in `tests/e2e/c5c3/invalid-keystoneuser-cr/`,
generated from its `_generate.py` and guarded by
`make verify-invalid-cr-fixtures`.

## Projected child names

The children of an order live in the **ControlPlane's** namespace on the
management cluster, where K-ORC reads the admin credential. They are named from
a per-order prefix:

```
<metadata.name>-<8 hex>-user-<discriminator>
```

The hash covers `<cluster>/<namespace>/<name>`, with the empty string for the
management cluster, so the same order name in another namespace or on another
cluster never shares a child name. The discriminators are `user` (the managed
K-ORC User), `user-probe` (the collision probe), `password-v<N>` (one Secret
per password generation), `source` (the document the backup pushes) and
`backup` (the PushSecret).

None of them can carry an owner reference to the order, so each carries three
labels instead: `c5c3.io/keystoneuser-name`, `c5c3.io/keystoneuser-namespace`
and `c5c3.io/keystoneuser-cluster` (empty for the management cluster). The
[delivered Secret](#delivered-secret-contract) is the one child without the
prefix and with an owner reference, because it shares the order's namespace
and cluster.

## Deletion Semantics

The `c5c3.io/keystoneuser-teardown` finalizer is installed once the order's
namespace is assigned, before anything is created. An order that never got past
the assignment carries none and is deleted at once. Deleting a KeystoneUser
that carries it runs the teardown:

| Resource | Fate |
| --- | --- |
| Keystone user | **Deleted** with the managed K-ORC User |
| Password Secrets, every generation | **Deleted** |
| OpenBao backup path | **Deleted** with the PushSecret (`deletionPolicy: Delete`) |
| Source Secret | **Deleted** |
| Delivered Secret | **Deleted**; the garbage collector would reap it too |

The assignment is not consulted, so a frozen order tears down the same way.
While the ControlPlane exists the teardown is patient: the finalizer is held
until no child is listed any more, so K-ORC and ESO can clear Keystone and
OpenBao first. Once the ControlPlane is gone the teardown fails open: the
children are deleted and the finalizer is released whatever their outcome.

An order whose target cluster was deregistered cannot be read any more, so its
children in the ControlPlane's namespace stay. Remove them by hand:

```bash
kubectl delete users.openstack.k-orc.cloud,pushsecrets.external-secrets.io,secrets \
  -n <controlplane-namespace> \
  -l c5c3.io/keystoneuser-cluster=<cluster>,c5c3.io/keystoneuser-namespace=<namespace>,c5c3.io/keystoneuser-name=<name>
```

## Chainsaw E2E Tests

`tests/e2e/c5c3/keystone-user/` runs on the `e2e-controlplane` job. It orders a
user from an assigned namespace, reads the Secret, authenticates with it from a
Job in that namespace, repairs an edited and a deleted Secret, rotates the
password, refuses an order from an unassigned namespace, freezes the order by
withdrawing the assignment, and deletes it down to the OpenBao path.
`tests/e2e/c5c3/invalid-keystoneuser-cr/` is the admission rejection corpus,
run on the `e2e-operator (c5c3)` job. The target-cluster path is proven in the
two-API-server envtest `TestIntegration_Multicluster_ControlPlanePlacement`.

See [ControlPlane E2E Test Suites](../testing/controlplane-e2e-tests.md).
