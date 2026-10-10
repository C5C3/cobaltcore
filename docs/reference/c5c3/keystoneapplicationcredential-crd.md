---
title: KeystoneApplicationCredential CRD API Reference
quadrant: operator
---

# KeystoneApplicationCredential CRD API Reference

Reference documentation for the KeystoneApplicationCredential Custom Resource
Definition. A KeystoneApplicationCredential orders one Keystone application
credential for a [KeystoneUser](./keystoneuser-crd.md) on a
[KeystoneProject](./keystoneproject-crd.md), both ordered from the same
namespace a [ControlPlane](./controlplane-crd.md) assigns to a service owner.
The c5c3-operator creates the credential through K-ORC, authenticated as the
ordered user itself, backs it up to OpenBao, and writes its id and secret into
the Secret `<metadata.name>-credentials` beside the order. On a schedule it
creates a successor, switches the Secret to it, and deletes the superseded
credential after a grace period.

The order lives in the assigned namespace, on the cluster that namespace is
assigned on: the management cluster or a registered
[target cluster](../target-clusters.md). Its children live in the
ControlPlane's namespace on the management cluster.

For the control loop see
[Keystone Orders Reconciler Architecture](./keystone-orders-reconciler.md#application-credential-provisioning).
The [Order a Service User](../../guides/order-a-service-user.md#order-an-application-credential)
guide walks the flow on the ControlPlane devstack.

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `c5c3.io` |
| Version | `v1alpha1` |
| Kind | `KeystoneApplicationCredential` |
| List Kind | `KeystoneApplicationCredentialList` |
| Scope | Namespaced |

**Scheme registration:** the `init()` function in
`keystoneapplicationcredential_types.go` registers both Kinds with the shared
`SchemeBuilder`.

`kubectl get keystoneapplicationcredential` prints seven columns:

| Column | Source |
| --- | --- |
| `Ready` | `.status.conditions[?(@.type=='Ready')].status` |
| `ControlPlane` | `.spec.controlPlaneRef.name` |
| `User` | `.spec.userRef.name` |
| `Project` | `.spec.projectRef.name` |
| `Generation` | `.status.credentialGeneration` |
| `NextRotation` | `.status.nextRotation` |
| `Age` | `.metadata.creationTimestamp` |

## Example

A credential for the ordered user `workflow` on the ordered project
`workflow-project` in the assigned namespace `tenant-a`. The rotation
schedule takes its defaults: a new credential every 720 hours, and the
superseded one deleted 24 hours after the switch.

```yaml
apiVersion: c5c3.io/v1alpha1
kind: KeystoneApplicationCredential
metadata:
  name: workflow-appcred
  namespace: tenant-a
spec:
  controlPlaneRef:
    name: cp
    namespace: openstack
  userRef:
    name: workflow
  projectRef:
    name: workflow-project
```

A [KeystoneRoleAssignment](./keystoneroleassignment-crd.md) in the same
namespace must have assigned the user a role on the project before anything is
created.

## Spec

### KeystoneApplicationCredentialSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `controlPlaneRef` | `ControlPlaneRefSpec` | Yes | — | The ControlPlane whose identity plane the credential is created in. Both halves are **immutable**, as for a [KeystoneUser](./keystoneuser-crd.md#controlplanerefspec). |
| `userRef.name` | `string` | Yes | — | The KeystoneUser in the order's namespace the credential belongs to (`MinLength=1`, `MaxLength=63`). **Immutable**. |
| `projectRef.name` | `string` | Yes | — | The KeystoneProject in the order's namespace the credential is scoped to (`MinLength=1`, `MaxLength=63`). **Immutable**. |
| `credentialGeneration` | `int64` | No | `1` | The lowest credential generation the order must hold (`Minimum=1`). Raising it above `status.credentialGeneration` rotates at once. It may only increase. |
| `rotation.interval` | `Duration` | No | `720h` | The time between two scheduled rotations. `0s` turns the schedule off; any other value is at least `1m`. At most `87600h`. |
| `rotation.gracePeriod` | `Duration` | No | `24h` | How long the superseded credential stays in Keystone after the delivered Secret switched. Not negative, and shorter than a non-zero interval. |

Both references name orders of the same ControlPlane, in the order's own
namespace on the order's own cluster. A KeystoneService account or a project
another namespace ordered cannot be referenced. The credential is always
restricted (`unrestricted: false`), so a token from it cannot create or delete
application credentials, and it carries the roles the user holds on the
project.

The API server stores the defaults it applies as written (`720h`, `24h`), and a
Go client that sets a duration stores its Go form (`720h0m0s`). The kind has no
webhook, so the reconciler resolves a zero `credentialGeneration` and an absent
duration to their defaults itself.

### Rotation

Each rotation creates a new credential generation N+1 beside the delivered
generation N:

1. The operator creates the successor in Keystone. The delivered Secret keeps
   generation N, which stays valid, and `CredentialReady` stays `True`.
2. Once K-ORC reports the successor Available, the order switches to it:
   `status.credentialGeneration` reads N+1, `status.previousCredentialID` names
   generation N, and the delivered Secret is rewritten as soon as the backup
   holds the new document.
3. When `status.previousCredentialDeleteAt` has passed, generation N is
   deleted in Keystone.

A rotation starts when one of these holds:

- the schedule is due: the clock has reached `status.nextRotation`;
- `spec.credentialGeneration` is above `status.credentialGeneration`, which
  creates that generation directly;
- the live credential has expired in Keystone;
- the Secret holding the live credential's secret is gone from the cluster.
  Keystone cannot hand a secret back, so a lost one is replaced by a new
  credential.

No rotation starts while a grace period runs, whatever the trigger, so the
order never holds more than two credentials. A rotation already under way is
continued, never restarted. A successor K-ORC reports a terminal error on
(`CredentialFailed`) is continued as well until `spec.credentialGeneration` is
raised above it; the order then deletes it and creates the raised generation.

While the schedule is on, every credential carries a Keystone expiry of its
creation time plus the interval plus the grace period, so a credential the
operator fails to delete stops working on its own. `status.nextRotation` is the
earlier of `status.lastRotation` plus the interval and
`status.credentialExpiresAt` minus the grace period: a lengthened interval never
lets the delivered credential run into its expiry.

A credential created with `interval: 0s` carries no expiry. Turning the
schedule off leaves the live credential's expiry in force, and the order
rotates it when it expires. Raise `spec.credentialGeneration` right after
setting `interval: 0s` to get a credential without an expiry at once.

## Consent

The `spec.namespaceAssignments` entry for the order's namespace and cluster is
the only consent a KeystoneApplicationCredential reads, as for a
[KeystoneUser](./keystoneuser-crd.md#consent). The entry's `allowedRoles` is not
consulted: the credential carries the roles a KeystoneRoleAssignment already
gave the user.

Without an entry both conditions read `False/NamespaceNotAssigned` and the
order is frozen: nothing is created, rotated, delivered, repaired or swept, and
the schedule pauses. The credentials and the delivered Secret it already has
stay. When the entry returns, the next pass rotates at once if
`status.nextRotation` has passed.

The order references three other orders in its namespace, and each gate runs on
every pass:

| Gate | Refusal |
| --- | --- |
| the KeystoneUser `spec.userRef` names | `UserNotFound`, `ControlPlaneMismatch`, `WaitingForUser` |
| the KeystoneProject `spec.projectRef` names | `ProjectNotFound`, `ControlPlaneMismatch`, `WaitingForProject` |
| a KeystoneRoleAssignment of that user and project reporting `status.roleID` | `NoRoleOnProject` |

The credential is created with a token scoped to the project, which needs a
role there. The role gate reads `status.roleID` and not `AssignmentReady`,
because a frozen assignment keeps its role in Keystone. A refusal after the
first credential pauses the schedule and keeps the delivered Secret.

While the order exists, deleting any of the three holds on
`ReferencedByApplicationCredentials`; see [Deletion Semantics](#deletion-semantics).

## Delivered Secret contract

| Property | Value |
| --- | --- |
| Name | `<metadata.name>-credentials`, also reported in `status.secretName` |
| Namespace | the order's own |
| Cluster | the order's own |
| Data keys | `clouds.yaml`, `application_credential_id` and `application_credential_secret`, also reported in `status.secretKeys` |
| Ownership | a controller owner reference to the order; the garbage collector reaps the Secret with it |
| Labels | `c5c3.io/keystoneapplicationcredential-name` and `c5c3.io/keystoneapplicationcredential-namespace` |
| Backing path | `openstack/keystone/<controlplane namespace>/<name>-<hash>-applicationcredential/service-accounts/application-credential` in OpenBao |

`clouds.yaml` is a `v3applicationcredential` document with the auth URL, the
credential id and its secret. A token issued with it is scoped to the project
the credential was created on. `application_credential_id` and
`application_credential_secret` carry the two values on their own, for
consumers that build their own configuration. The document carries no CA
bundle, for the reason the [KeystoneUser](./keystoneuser-crd.md#delivered-secret-contract)
contract gives, and its auth URL is resolved for the order's cluster the same
way. An order on another cluster than a Managed Keystone that publishes no
endpoint reports `DeliveryReady=False/KeystoneNotPublished`.

The Secret always carries the generation `status.credentialGeneration` names.
It switches to a successor only once Keystone holds it, and the superseded
credential stays valid for the grace period after the switch, so the
delivered credential is never invalid. A consumer reads the Secret again
within the grace period to pick the successor up.

The backup comes first, as for a KeystoneUser: the delivered Secret is written
once ESO reports a push of the current document to the backing path. The
operator applies the Secret with Server-Side Apply and owns its three keys. An
edited key is rewritten and a deleted Secret is written again; a key somebody
else adds stays. A [frozen](#consent) order repairs nothing.

A Secret of the same name that the order does not own is never taken over: the
order reports `DeliveryRefused`. A KeystoneUser of the same name in the same
namespace delivers a Secret of the same name, so the two orders cannot share a
name.

## Status

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | `CredentialReady`, `DeliveryReady` and the aggregate `Ready`. See [Conditions](#conditions). |
| `observedGeneration` | `int64` | The `metadata.generation` the controller last reconciled. |
| `credentialID` | `string` | The Keystone id of the credential the delivered Secret carries. |
| `credentialGeneration` | `int64` | The generation of that credential. |
| `credentialExpiresAt` | `*metav1.Time` | When that credential expires in Keystone. Absent for one created with the schedule off. |
| `lastRotation` | `*metav1.Time` | When that credential became the delivered one. |
| `nextRotation` | `*metav1.Time` | When the schedule rotates next. Absent with the schedule off. |
| `previousCredentialID` | `string` | The Keystone id of the superseded credential, during its grace period only. |
| `previousCredentialGeneration` | `int64` | Its generation, during its grace period only. |
| `previousCredentialDeleteAt` | `*metav1.Time` | When it is deleted, during its grace period only. |
| `secretName` | `string` | The [delivered Secret](#delivered-secret-contract), once it carries the current credential. |
| `secretKeys` | `[]string` | `clouds.yaml`, `application_credential_id` and `application_credential_secret`, once the Secret carries the current credential. |

### Conditions

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `CredentialReady` | True | `CredentialMinted` | The delivered credential exists in Keystone. While a successor is created the message names both generations. |
| `CredentialReady` | False | `UserNotFound` | `spec.userRef` names no KeystoneUser in the order's namespace. |
| `CredentialReady` | False | `ProjectNotFound` | `spec.projectRef` names no KeystoneProject in the order's namespace. |
| `CredentialReady` | False | `ControlPlaneMismatch` | A referenced order belongs to another ControlPlane. |
| `CredentialReady` | False | `WaitingForUser` | The KeystoneUser is not provisioned yet (the message names its `UserReady` reason), or the password of its current generation is not available. |
| `CredentialReady` | False | `WaitingForProject` | The KeystoneProject is not provisioned yet or reports no project name. |
| `CredentialReady` | False | `NoRoleOnProject` | No KeystoneRoleAssignment in the namespace has assigned the user a role on the project. |
| `CredentialReady` | False | `WaitingForCABundle` | The Keystone CA bundle the ControlPlane references is not available, so the operator cannot write the document K-ORC authenticates with. |
| `CredentialReady` | False | `WaitingForCredential` | K-ORC has not made the first credential Available yet. |
| `CredentialReady` | False | `CredentialFailed` | K-ORC reported a terminal error on a credential generation, with its message. |
| `CredentialReady` | False | `TransportErrorRetryFailed` | Clearing a latched transport error from a credential failed. |
| `CredentialReady` | False | `CredentialError` | A Kubernetes-level failure writing or reading the credential's children. |
| `DeliveryReady` | True | `Delivered` | The Secret beside the order carries the current credential. |
| `DeliveryReady` | False | `WaitingForCredential` | No credential exists yet, or the Secret does not carry the current one yet. |
| `DeliveryReady` | False | `KeystoneNotPublished` | The order lives on a cluster that cannot reach the in-cluster Keystone Service and the ControlPlane publishes no public endpoint. |
| `DeliveryReady` | False | `BackupNotSynced` | ESO has not pushed the current document to OpenBao yet. |
| `DeliveryReady` | False | `DeliveryRefused` | A Secret of the delivered name exists that the order does not own. |
| `DeliveryReady` | False | `DeliveryError` | A Kubernetes-level failure writing or reading a delivery object, with the error text. |
| both | False | `ControlPlaneNotFound` | `spec.controlPlaneRef` does not resolve, for an order on the management cluster. |
| both | False | `NamespaceNotAssigned` | No `spec.namespaceAssignments` entry assigns the order's namespace on its cluster, or, for an order on a target cluster, `spec.controlPlaneRef` does not resolve. The order is frozen. |
| both | False | `ClusterNameTooLong` | The order lives on a target cluster whose name is longer than the 63 characters a label value carries. |
| both | False | `WaitingForAdminCredential` | The ControlPlane's `AdminCredentialReady` is not True. |
| `Ready` | True | `AllReady` | Both sub-conditions are True. |
| `Ready` | False | `NotAllReady` | At least one sub-condition is not True. |

Against an External-mode ControlPlane a wait on the first credential is
replaced by the classified cause when K-ORC's failure can be classified.

A converged order on the management cluster is reconciled again at
`status.nextRotation`, at `status.previousCredentialDeleteAt`, and when its
ControlPlane, its references or its children change. An order on a target
cluster is reconciled again every 10 minutes at the latest.

## Defaulting and Validation Summary

The kind has **no webhook**, because a target cluster runs none. Every rule is
a CRD rule:

- `userRef.name`, `projectRef.name`: required, `MinLength=1`, `MaxLength=63`,
  and transition rules ("userRef is immutable", "projectRef is immutable").
- `credentialGeneration`: schema default `1`, `Minimum=1`, and the transition
  rule `self >= oldSelf` ("credentialGeneration may only increase").
- `rotation`: schema default `{}`, so the two defaults below apply to an order
  that omits the block.
- `rotation.interval`: schema default `720h`, the rule
  `duration(self) == duration('0s') || duration(self) >= duration('1m')`
  ("rotation.interval is 0s or at least 1m"), and the rule
  `duration(self) <= duration('87600h')` ("rotation.interval is at most
  87600h"), which keeps the mint time plus the interval plus the grace period
  inside the range of a Go duration.
- `rotation.gracePeriod`: schema default `24h` and the rule
  `duration(self) >= duration('0s')` ("rotation.gracePeriod is not negative").
- On the `rotation` block: the grace period is shorter than a non-zero interval
  ("rotation.gracePeriod must be shorter than rotation.interval"). The rule
  checks with `has()` that both fields are present, because the API server
  validates the `{}` default before the field defaults fill it in.
- `controlPlaneRef`: as for a KeystoneUser ("controlPlaneRef.name is
  immutable", "controlPlaneRef.namespace is immutable").
- On the object: `metadata.name` is at most 63 bytes ("metadata.name must be at
  most 63 bytes").

Cross-object rules live in the reconciler: the references, the role on the
project and the consent each need a read admission does not have.

The rejection corpus lives in
`tests/e2e/c5c3/invalid-keystoneapplicationcredential-cr/`, generated from its
`_generate.py` and guarded by `make verify-invalid-cr-fixtures`.

## Projected child names

The children live in the **ControlPlane's** namespace on the management
cluster, named from a per-order prefix:

```
<metadata.name>-<8 hex>-applicationcredential-<discriminator>
```

The hash covers `<cluster>/<namespace>/<name>`, as for every order kind. The
discriminators are:

| Discriminator | Child |
| --- | --- |
| `mint-cloud` | The password `clouds.yaml` of the ordered user, scoped to the project, rebuilt on every pass from the password of the user's `status.passwordGeneration`. K-ORC creates and deletes every credential through it. |
| `secret-v<N>` | The secret of generation N (key `value`), generated once. |
| `credential-v<N>` | The K-ORC ApplicationCredential of generation N. K-ORC names the Keystone credential after it. |
| `source` | The document the backup pushes. |
| `backup` | The PushSecret. |

Each carries three labels: `c5c3.io/keystoneapplicationcredential-name`,
`c5c3.io/keystoneapplicationcredential-namespace` and
`c5c3.io/keystoneapplicationcredential-cluster` (empty for the management
cluster). The [delivered Secret](#delivered-secret-contract) has no prefix and
an owner reference instead.

## Deletion Semantics

The `c5c3.io/keystoneapplicationcredential-teardown` finalizer is installed
once the order's namespace is assigned. Deleting an order that carries it runs
the teardown in two phases:

| Phase | Deletes |
| --- | --- |
| While a K-ORC ApplicationCredential is listed | the PushSecret, which removes the OpenBao path; every `credential-v<N>`, which K-ORC deletes in Keystone; every `secret-v<N>`; the source Secret |
| Once none is listed | the `mint-cloud` document and the delivered Secret |

The document stays through the first phase because K-ORC authenticates the
Keystone deletes with it and guards it with a finalizer. The finalizer is held
until nothing the order owns is listed. With the ControlPlane gone K-ORC has
nothing left to reach Keystone with, so one pass issues every delete and
releases the finalizer.

The order holds three other orders in its namespace while it exists:

| Held order | Condition | Why |
| --- | --- | --- |
| the KeystoneUser `spec.userRef` names | `UserReady=False/ReferencedByApplicationCredentials` | deleting the user deletes its credentials in Keystone, and K-ORC cannot delete a credential whose user is gone |
| the KeystoneProject `spec.projectRef` names | `ProjectReady=False/ReferencedByApplicationCredentials` | the credentials are scoped to the project |
| every KeystoneRoleAssignment of that user and project | `AssignmentReady=False/ReferencedByApplicationCredentials` | the credentials are created and deleted with a token scoped to the project, which needs the role |

Delete the credential order first. The teardown does not rewrite the
document: if the user's `spec.passwordGeneration` is raised while the credential
order is being deleted, K-ORC cannot authenticate the deletes, and the finalizer
holds until the order's K-ORC ApplicationCredentials are removed by hand.

An order whose target cluster was deregistered cannot be read any more, so its
children in the ControlPlane's namespace stay. Remove them by hand:

```bash
kubectl delete applicationcredentials.openstack.k-orc.cloud,pushsecrets.external-secrets.io,secrets \
  -n <controlplane-namespace> \
  -l c5c3.io/keystoneapplicationcredential-cluster=<cluster>,c5c3.io/keystoneapplicationcredential-namespace=<namespace>,c5c3.io/keystoneapplicationcredential-name=<name>
```

## Chainsaw E2E Tests

`tests/e2e/c5c3/keystone-user/` runs on the `e2e-controlplane` job. It orders
a credential beside the refused pieces and reads `NoRoleOnProject`, then, with
the role assigned, reads generation 1, authenticates with it from a Job,
observes the scheduled rotation to generation 2 and the superseded credential
failing after the grace period, turns the schedule off, rotates by hand to
generation 3, freezes the order, and tears it down to an empty OpenBao path
after the assignment held on it.
`tests/e2e/c5c3/invalid-keystoneapplicationcredential-cr/` is the admission
rejection corpus, run on the `e2e-operator (c5c3)` job. The target-cluster path
is proven in `TestIntegration_Multicluster_ControlPlanePlacement`.

See [ControlPlane E2E Test Suites](../testing/controlplane-e2e-tests.md).
